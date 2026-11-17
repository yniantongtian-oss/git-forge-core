// The entry <-> env lifecycle: bind an ephemeral port, register it with the env, poll until the env says the
// trial is over, then close the server and exit 0.
//
//   1. MAKEFUN_ENV_URL names the env's origin. Without it there is nothing to talk to: exit 1.
//   2. listen(0): the kernel picks a free port (never 8080, which the suite owns).
//   3. POST $MAKEFUN_ENV_URL/register {"port": N}; the first register wins (a 409 is not an error).
//   4. GET $MAKEFUN_ENV_URL/done until {"done": true}, polling with a short backoff.
//   5. Close the server and exit 0.

import type { Socket } from 'node:net';
import { createApp } from './app.ts';

const ENV_URL = (process.env.MAKEFUN_ENV_URL ?? '').replace(/\/+$/, '');
if (ENV_URL === '') {
  process.stderr.write('[forge] MAKEFUN_ENV_URL is not set: there is no env to register with\n');
  process.exit(1);
}

const REQUEST_TIMEOUT_MS = 30_000;
const RETRY_INITIAL_MS = 250;
const RETRY_MAX_MS = 2_000;

const sleep = (ms: number): Promise<void> => new Promise((resolve) => setTimeout(resolve, ms));

function retryDelay(attempt: number): number {
  return Math.min(RETRY_MAX_MS, RETRY_INITIAL_MS * 2 ** Math.min(attempt, 8));
}

// A bug in one request must never take the forge down mid-trial.
process.on('uncaughtException', (err) => {
  process.stderr.write(`[forge] uncaught exception: ${err.stack ?? String(err)}\n`);
});
process.on('unhandledRejection', (err) => {
  process.stderr.write(`[forge] unhandled rejection: ${err instanceof Error ? (err.stack ?? err.message) : String(err)}\n`);
});

async function register(port: number): Promise<void> {
  for (let attempt = 0; ; attempt += 1) {
    try {
      const res = await fetch(`${ENV_URL}/register`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ port }),
        signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS),
      });
      if (res.status === 200 || res.status === 409) return;
      if (res.status === 400 || res.status === 405) throw new Error(`register answered HTTP ${res.status}: ${await res.text()}`);
    } catch (err) {
      if (err instanceof Error && err.message.startsWith('register answered HTTP ')) throw err;
    }
    await sleep(retryDelay(attempt));
  }
}

async function waitForDone(): Promise<void> {
  for (let attempt = 0; ; attempt += 1) {
    try {
      const res = await fetch(`${ENV_URL}/done`, { signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS) });
      if (res.ok) {
        const body = (await res.json()) as { done?: unknown };
        if (body.done === true) return;
      }
    } catch {
      // The env is not answering yet, or the poll timed out: it is not done, so poll again.
    }
    await sleep(retryDelay(attempt));
  }
}

async function main(): Promise<void> {
  const server = createApp();
  server.keepAliveTimeout = 60_000;
  server.headersTimeout = 65_000;
  server.on('connection', (socket: Socket) => {
    socket.setNoDelay(true);
  });
  await new Promise<void>((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, () => resolve());
  });
  const address = server.address();
  if (address === null || typeof address === 'string') throw new Error('the server did not bind an ephemeral TCP port');
  process.stdout.write(`[forge] listening on port ${address.port}\n`);

  await register(address.port);
  process.stdout.write('[forge] registered; waiting for the env to say done\n');

  await waitForDone();
  server.close();
  server.closeAllConnections();
  process.stdout.write('[forge] done\n');
}

try {
  await main();
  process.exit(0);
} catch (err) {
  process.stderr.write(`[forge] ${err instanceof Error ? err.message : String(err)}\n`);
  process.exit(1);
}
