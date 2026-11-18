// The entry <-> env lifecycle: bind an ephemeral port, register it with the env, poll until the env says the
// trial is over, then close the server and exit 0.
//
//   1. MAKEFUN_ENV_URL names the env's origin. Without it there is nothing to talk to: exit 1.
//   2. listen(0): the kernel picks a free port (never 8080, which the suite owns).
//   3. POST $MAKEFUN_ENV_URL/register {"port": N}; the first register wins (a 409 is not an error).
//   4. GET $MAKEFUN_ENV_URL/done until {"done": true}, polling with a short backoff.
//   5. Close the server and exit 0.
//
// The forge is a static Go binary (cmd/forge) that runs this whole lifecycle itself. main.ts is the thin launcher:
// it starts the build for this platform (forge-linux, or forge-win.exe on Windows) that sits next to it, forwards
// SIGINT/SIGTERM, and exits with the binary's exit code.

import { spawn } from 'node:child_process';
import type { ChildProcess } from 'node:child_process';
import { chmodSync, copyFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { basename, dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const ENV_URL = (process.env.MAKEFUN_ENV_URL ?? '').replace(/\/+$/, '');
if (ENV_URL === '') {
  process.stderr.write('[forge] MAKEFUN_ENV_URL is not set: there is no env to register with\n');
  process.exit(1);
}

const HERE = dirname(fileURLToPath(import.meta.url));
const BINARY = join(HERE, process.platform === 'win32' ? 'forge-win.exe' : 'forge-linux');

const env: NodeJS.ProcessEnv = { ...process.env };
env.GOGC ??= '800';
env.GOMEMLIMIT ??= '700MiB';

let child: ChildProcess | null = null;

function launch(path: string, retried: boolean): void {
  const proc = spawn(path, [], { stdio: 'inherit', env });
  child = proc;
  let started = false;
  proc.once('spawn', () => {
    started = true;
  });
  proc.once('error', (err: NodeJS.ErrnoException) => {
    if (!started && !retried && (err.code === 'EACCES' || err.code === 'EPERM')) {
      // A checkout or mount without the executable bit: run a private executable copy instead.
      const copy = join(tmpdir(), `${process.pid}-${basename(path)}`);
      copyFileSync(path, copy);
      chmodSync(copy, 0o755);
      launch(copy, true);
      return;
    }
    process.stderr.write(`[forge] cannot start ${path}: ${err.message}\n`);
    process.exit(1);
  });
  proc.once('exit', (code) => {
    if (started) process.exit(code ?? 1);
  });
}

for (const signal of ['SIGINT', 'SIGTERM'] as const) {
  process.on(signal, () => {
    if (child !== null && child.exitCode === null && child.signalCode === null) child.kill(signal);
    else process.exit(0);
  });
}

launch(BINARY, false);
