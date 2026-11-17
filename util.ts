// Small HTTP helpers shared by every surface: body reading, canned JSON replies, request ids.

import type { IncomingMessage, ServerResponse } from 'node:http';

export const JSON_TYPE = 'application/json; charset=utf-8';

const MAX_BODY_BYTES = 512 * 1024 * 1024;

export class HttpError extends Error {
  status: number;

  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

/** Read a whole request body. */
export function readBody(req: IncomingMessage): Promise<Buffer> {
  return new Promise<Buffer>((resolve, reject) => {
    const chunks: Buffer[] = [];
    let size = 0;
    req.on('data', (chunk: Buffer) => {
      size += chunk.length;
      if (size > MAX_BODY_BYTES) {
        reject(new HttpError(413, 'Request body too large'));
        req.destroy();
        return;
      }
      chunks.push(chunk);
    });
    req.on('end', () => resolve(chunks.length === 1 ? (chunks[0] as Buffer) : Buffer.concat(chunks)));
    req.on('error', reject);
    req.on('aborted', () => reject(new HttpError(400, 'Request aborted')));
  });
}

let requestCounter = 0;

/** A GitHub-shaped request id that is a pure function of how many replies came before it. */
export function nextRequestId(): string {
  requestCounter += 1;
  const hex = requestCounter.toString(16).toUpperCase().padStart(8, '0');
  return `FEED:${hex.slice(0, 4)}:${hex.slice(4)}:0000A1:00000001`;
}

/** Answer with a JSON body (an object to serialize, or an already serialized buffer). */
export function sendJson(res: ServerResponse, status: number, body: unknown, headers?: Record<string, string>): void {
  const data = Buffer.isBuffer(body) ? body : Buffer.from(JSON.stringify(body), 'utf8');
  const head: Record<string, string | number> = {
    'content-type': JSON_TYPE,
    'content-length': data.length,
    'x-github-request-id': nextRequestId(),
  };
  if (headers !== undefined) for (const key of Object.keys(headers)) head[key] = headers[key] as string;
  res.writeHead(status, head);
  res.end(data);
}

/** A GitHub-style error body: {message, documentation_url, errors?}. */
export function sendError(res: ServerResponse, status: number, message: string, errors?: unknown[], headers?: Record<string, string>): void {
  const body: Record<string, unknown> = { message };
  if (errors !== undefined) body.errors = errors;
  body.documentation_url = 'https://docs.github.com/rest';
  sendJson(res, status, body, headers);
}

/** Answer with raw bytes. */
export function sendBuffer(res: ServerResponse, status: number, type: string, data: Buffer, headers?: Record<string, string>): void {
  const head: Record<string, string | number> = { 'content-type': type, 'content-length': data.length };
  if (headers !== undefined) for (const key of Object.keys(headers)) head[key] = headers[key] as string;
  res.writeHead(status, head);
  res.end(data);
}

/** Parse a query string into a plain map (the last value of a repeated key wins). */
export function parseQuery(search: string): Record<string, string> {
  const out: Record<string, string> = {};
  if (search === '') return out;
  for (const part of search.split('&')) {
    if (part === '') continue;
    const eq = part.indexOf('=');
    const key = decode(eq < 0 ? part : part.slice(0, eq));
    out[key] = eq < 0 ? '' : decode(part.slice(eq + 1));
  }
  return out;
}

export function decode(text: string): string {
  try {
    return decodeURIComponent(text.replace(/\+/g, ' '));
  } catch {
    return text;
  }
}
