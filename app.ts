// The HTTP surface: one server that routes git smart HTTP, the REST v3 API, GraphQL and MCP to their modules,
// checks the bearer token, stamps X-GitHub-Request-Id on JSON replies, and turns every failure into a JSON body.

import { createServer } from 'node:http';
import type { IncomingMessage, Server, ServerResponse } from 'node:http';
import { getRepo, viewer } from './store.ts';
import { handleGit } from './git.ts';
import { handleRest, setBase } from './rest.ts';
import { handleGraphql } from './graphql.ts';
import { MCP_UNAUTHORIZED, handleMcp } from './mcp.ts';
import { HttpError, decode, sendBuffer, sendError, sendJson } from './util.ts';

const TOKEN = process.env.MAKEFUN_SHOP_TOKEN ?? '';
const BEARER = `Bearer ${TOKEN}`;

/** `Authorization: Bearer <token>` (or the older `token <token>` scheme) naming MAKEFUN_SHOP_TOKEN. */
function authorized(req: IncomingMessage): boolean {
  const header = req.headers.authorization;
  if (header === undefined || header === '') return false;
  if (TOKEN === '') return header.length > 7;
  if (header === BEARER) return true;
  const scheme = header.slice(0, header.indexOf(' ')).toLowerCase();
  return (scheme === 'bearer' || scheme === 'token') && header.slice(scheme.length + 1) === TOKEN;
}

function unauthorized(res: ServerResponse): void {
  sendError(res, 401, 'Requires authentication', undefined, { 'www-authenticate': 'Bearer realm="forge"' });
}

const GIT_PATH = /^\/([^/]+)\/([^/]+?)\.git(\/.*)?$/;

function onFailure(res: ServerResponse, err: unknown): void {
  if (res.headersSent) {
    res.destroy();
    return;
  }
  if (err instanceof HttpError) sendError(res, err.status, err.message);
  else sendError(res, 500, `Server Error: ${err instanceof Error ? err.message : String(err)}`);
}

function route(req: IncomingMessage, res: ServerResponse): void | Promise<void> {
  const url = req.url ?? '/';
  const q = url.indexOf('?');
  const path = q < 0 ? url : url.slice(0, q);
  const search = q < 0 ? '' : url.slice(q + 1);
  const method = req.method ?? 'GET';

  if (path.startsWith('/api/v3')) {
    if (!authorized(req)) return unauthorized(res);
    setBase(req.headers.host);
    return handleRest(req, res, path, search);
  }
  if (path === '/healthz') {
    sendJson(res, 200, { ok: true });
    return;
  }
  if (path === '/api/graphql') {
    if (!authorized(req)) return unauthorized(res);
    setBase(req.headers.host);
    if (method !== 'POST') return sendError(res, 404, 'Not Found');
    return handleGraphql(req, res);
  }
  if (path === '/mcp') {
    if (!authorized(req)) {
      sendJson(res, 401, MCP_UNAUTHORIZED, { 'www-authenticate': 'Bearer realm="forge"' });
      return;
    }
    setBase(req.headers.host);
    if (method === 'POST') return handleMcp(req, res);
    if (method === 'DELETE') {
      res.writeHead(204);
      res.end();
      return;
    }
    sendJson(res, 405, { jsonrpc: '2.0', id: null, error: { code: -32601, message: 'Method not allowed' } }, { allow: 'POST, DELETE' });
    return;
  }
  const git = GIT_PATH.exec(path);
  if (git !== null) {
    const owner = decode(git[1] as string);
    const repo = owner.toLowerCase() === viewer.login.toLowerCase() ? getRepo(owner, decode(git[2] as string)) : undefined;
    if (repo === undefined) {
      sendBuffer(res, 404, 'text/plain; charset=utf-8', Buffer.from('Repository not found\n'), { 'cache-control': 'no-cache' });
      return;
    }
    setBase(req.headers.host);
    return handleGit(req, res, repo, git[3] ?? '', search);
  }
  sendError(res, 404, 'Not Found');
}

/** Build the server. It binds nothing until the caller listens. */
export function createApp(): Server {
  return createServer((req: IncomingMessage, res: ServerResponse) => {
    try {
      const pending = route(req, res);
      if (pending !== undefined) pending.catch((err: unknown) => onFailure(res, err));
    } catch (err) {
      onFailure(res, err);
    }
  });
}
