// Model Context Protocol over streamable HTTP at POST /mcp: JSON-RPC 2.0, the 2026-07-28 revision
// (server/discover, no handshake) and the handshake revisions (initialize + notifications/initialized).
// The tools act on the same store as REST, so what an agent creates here reads back there unchanged.

import type { IncomingMessage, ServerResponse } from 'node:http';
import {
  ApiError,
  addComment,
  createIssue,
  createPull,
  createRef,
  createRepo,
  deleteContents,
  getContents,
  getRepo,
  findThread,
  listIssues,
  mergePull,
  pullFiles,
  putContents,
  updateIssue,
  viewer,
} from './store.ts';
import type { Repo } from './store.ts';
import { listCommits, refsWithPrefix, resolveCommit } from './gitops.ts';
import { commitJson, issueBuffer, pullJson, repoJson, userJson } from './rest.ts';
import { readBody } from './util.ts';

const SERVER_INFO = { name: 'forge-core', title: 'forge-core git forge', version: '1.0.0' };
const SUPPORTED_VERSIONS = ['2026-07-28', '2025-11-25', '2025-06-18', '2025-03-26', '2024-11-05'];
const INSTRUCTIONS = 'A GitHub-compatible forge: create repositories, issues and pull requests, then merge them. Everything is shared with the REST and GraphQL APIs.';

type Json = any;

interface Tool {
  name: string;
  description: string;
  inputSchema: Json;
  run: (args: Record<string, Json>) => string;
}

class ToolFailure extends Error {}

function str(description: string): Json {
  return { type: 'string', description };
}

function schemaOf(properties: Record<string, Json>, required: string[]): Json {
  return { type: 'object', properties, required, additionalProperties: false };
}

const OWNER = str('Repository owner (user login)');
const REPO = str('Repository name');

function repoOf(args: Record<string, Json>): Repo {
  const owner = String(args.owner);
  const repo = owner.toLowerCase() === viewer.login.toLowerCase() ? getRepo(owner, String(args.repo)) : undefined;
  if (repo === undefined) throw new ToolFailure(`Not Found: repository ${String(args.owner)}/${String(args.repo)} does not exist`);
  return repo;
}

function issueOf(args: Record<string, Json>, key = 'issue_number'): { repo: Repo; number: number } {
  const number = Number(args[key]);
  if (!Number.isInteger(number) || number < 1) throw new ToolFailure(`${key} must be a positive integer`);
  return { repo: repoOf(args), number };
}

const out = (value: unknown): string => JSON.stringify(value);

const TOOLS: Tool[] = [
  {
    name: 'create_repo',
    description: 'Create a new repository owned by the authenticated user.',
    inputSchema: schemaOf({ name: str('Repository name'), description: str('Short description'), private: { type: 'boolean', description: 'Whether the repository is private' }, autoInit: { type: 'boolean', description: 'Create an initial commit with a README' } }, ['name']),
    run: (args) => out(repoJson(createRepo({ name: args.name, description: args.description, private: args.private, autoInit: args.autoInit }))),
  },
  {
    name: 'get_repo',
    description: 'Get the details of a repository.',
    inputSchema: schemaOf({ owner: OWNER, repo: REPO }, ['owner', 'repo']),
    run: (args) => out(repoJson(repoOf(args))),
  },
  {
    name: 'create_issue',
    description: 'Create a new issue in a repository.',
    inputSchema: schemaOf({ owner: OWNER, repo: REPO, title: str('Issue title'), body: str('Issue body'), labels: { type: 'array', items: { type: 'string' }, description: 'Names of existing labels' } }, ['owner', 'repo', 'title']),
    run: (args) => issueBuffer(createIssue(repoOf(args), { title: args.title, body: args.body, labels: args.labels })).toString('utf8'),
  },
  {
    name: 'list_issues',
    description: 'List the issues of a repository, optionally filtered by state.',
    inputSchema: schemaOf({ owner: OWNER, repo: REPO, state: { type: 'string', enum: ['open', 'closed', 'all'], description: 'Issue state filter (default open)' }, labels: str('Comma separated label names'), perPage: { type: 'integer', description: 'Results per page (max 100)' } }, ['owner', 'repo']),
    run: (args) => {
      const per = Math.min(100, Math.max(1, Number(args.perPage ?? args.per_page) || 30));
      const issues = listIssues(repoOf(args), { state: typeof args.state === 'string' ? args.state : undefined, labels: typeof args.labels === 'string' ? args.labels : undefined }).slice(0, per);
      return `[${issues.map((issue) => issueBuffer(issue).toString('utf8')).join(',')}]`;
    },
  },
  {
    name: 'create_pull_request',
    description: 'Open a pull request from a head branch into a base branch.',
    inputSchema: schemaOf({ owner: OWNER, repo: REPO, title: str('Pull request title'), head: str('Head branch'), base: str('Base branch'), body: str('Pull request body'), draft: { type: 'boolean', description: 'Open as a draft' } }, ['owner', 'repo', 'title', 'head', 'base']),
    run: (args) => out(pullJson(createPull(repoOf(args), { title: args.title, body: args.body, head: args.head, base: args.base, draft: args.draft }), true)),
  },
  {
    name: 'merge_pull_request',
    description: 'Merge a pull request (squash by default).',
    inputSchema: schemaOf({ owner: OWNER, repo: REPO, pull_number: { type: 'integer', description: 'Pull request number' }, merge_method: { type: 'string', enum: ['merge', 'squash', 'rebase'], description: 'Merge method (default squash)' }, commit_title: str('Merge commit title'), commit_message: str('Merge commit message') }, ['owner', 'repo', 'pull_number']),
    run: (args) => {
      const { repo, number } = issueOf(args, 'pull_number');
      const pull = repo.pulls.get(number);
      if (pull === undefined) throw new ToolFailure(`Not Found: pull request #${number}`);
      const merged = mergePull(pull, { method: args.merge_method, title: args.commit_title, message: args.commit_message });
      return out({ sha: merged.sha, merged: true, message: merged.message });
    },
  },
  {
    name: 'get_issue',
    description: 'Get one issue by number.',
    inputSchema: schemaOf({ owner: OWNER, repo: REPO, issue_number: { type: 'integer', description: 'Issue number' } }, ['owner', 'repo', 'issue_number']),
    run: (args) => {
      const { repo, number } = issueOf(args);
      const issue = repo.issues.get(number);
      if (issue === undefined) throw new ToolFailure(`Not Found: issue #${number}`);
      return issueBuffer(issue).toString('utf8');
    },
  },
  {
    name: 'update_issue',
    description: 'Update the title, body or state of an issue.',
    inputSchema: schemaOf({ owner: OWNER, repo: REPO, issue_number: { type: 'integer', description: 'Issue number' }, title: str('New title'), body: str('New body'), state: { type: 'string', enum: ['open', 'closed'], description: 'New state' } }, ['owner', 'repo', 'issue_number']),
    run: (args) => {
      const { repo, number } = issueOf(args);
      const issue = repo.issues.get(number);
      if (issue === undefined) throw new ToolFailure(`Not Found: issue #${number}`);
      const patch: Record<string, unknown> = {};
      for (const key of ['title', 'body', 'state']) if (args[key] !== undefined) patch[key] = args[key];
      updateIssue(issue, patch);
      return issueBuffer(issue).toString('utf8');
    },
  },
  {
    name: 'add_issue_comment',
    description: 'Add a comment to an issue or pull request.',
    inputSchema: schemaOf({ owner: OWNER, repo: REPO, issue_number: { type: 'integer', description: 'Issue or pull request number' }, body: str('Comment text') }, ['owner', 'repo', 'issue_number', 'body']),
    run: (args) => {
      const { repo, number } = issueOf(args);
      const thread = findThread(repo, number);
      if (thread === undefined) throw new ToolFailure(`Not Found: #${number}`);
      const comment = addComment(thread, args.body);
      return out({ id: comment.id, body: comment.body, user: comment.user, created_at: comment.createdAt });
    },
  },
  {
    name: 'list_pull_requests',
    description: 'List the pull requests of a repository.',
    inputSchema: schemaOf({ owner: OWNER, repo: REPO, state: { type: 'string', enum: ['open', 'closed', 'all'], description: 'State filter (default open)' } }, ['owner', 'repo']),
    run: (args) => {
      const state = args.state === 'closed' || args.state === 'all' ? args.state : 'open';
      return out(repoOf(args).pullList.filter((pull) => state === 'all' || pull.state === state).map((pull) => pullJson(pull, false)));
    },
  },
  {
    name: 'get_pull_request',
    description: 'Get one pull request by number.',
    inputSchema: schemaOf({ owner: OWNER, repo: REPO, pull_number: { type: 'integer', description: 'Pull request number' } }, ['owner', 'repo', 'pull_number']),
    run: (args) => {
      const { repo, number } = issueOf(args, 'pull_number');
      const pull = repo.pulls.get(number);
      if (pull === undefined) throw new ToolFailure(`Not Found: pull request #${number}`);
      return out(pullJson(pull, true));
    },
  },
  {
    name: 'get_pull_request_files',
    description: 'List the files changed by a pull request.',
    inputSchema: schemaOf({ owner: OWNER, repo: REPO, pull_number: { type: 'integer', description: 'Pull request number' } }, ['owner', 'repo', 'pull_number']),
    run: (args) => {
      const { repo, number } = issueOf(args, 'pull_number');
      const pull = repo.pulls.get(number);
      if (pull === undefined) throw new ToolFailure(`Not Found: pull request #${number}`);
      return out(pullFiles(pull).map((file) => ({ filename: file.filename, status: file.status, additions: file.additions, deletions: file.deletions, changes: file.changes, sha: file.sha })));
    },
  },
  {
    name: 'list_branches',
    description: 'List the branches of a repository.',
    inputSchema: schemaOf({ owner: OWNER, repo: REPO }, ['owner', 'repo']),
    run: (args) => out(refsWithPrefix(repoOf(args), 'refs/heads/').map((ref) => ({ name: ref.name.slice(11), commit: { sha: ref.oid } }))),
  },
  {
    name: 'create_branch',
    description: 'Create a branch from an existing branch (the default branch unless from_branch is given).',
    inputSchema: schemaOf({ owner: OWNER, repo: REPO, branch: str('New branch name'), from_branch: str('Source branch') }, ['owner', 'repo', 'branch']),
    run: (args) => {
      const repo = repoOf(args);
      const source = resolveCommit(repo, typeof args.from_branch === 'string' ? args.from_branch : repo.defaultBranch);
      if (source === null) throw new ToolFailure('Not Found: the source branch does not exist');
      createRef(repo, `refs/heads/${String(args.branch)}`, source);
      return out({ ref: `refs/heads/${String(args.branch)}`, object: { sha: source } });
    },
  },
  {
    name: 'get_file_contents',
    description: 'Get the contents of a file or directory.',
    inputSchema: schemaOf({ owner: OWNER, repo: REPO, path: str('Path within the repository'), ref: str('Branch, tag or commit') }, ['owner', 'repo', 'path']),
    run: (args) => {
      const result = getContents(repoOf(args), String(args.path), typeof args.ref === 'string' ? args.ref : undefined);
      if (Array.isArray(result)) return out(result.map((entry) => ({ type: entry.type, name: entry.name, path: entry.path, sha: entry.oid, size: entry.size })));
      return out({ type: 'file', path: result.path, sha: result.oid, size: result.data.length, encoding: 'base64', content: result.data.toString('base64'), text: result.data.toString('utf8') });
    },
  },
  {
    name: 'create_or_update_file',
    description: 'Create or update a file with a commit.',
    inputSchema: schemaOf({ owner: OWNER, repo: REPO, path: str('File path'), content: str('File content (plain text)'), message: str('Commit message'), branch: str('Target branch'), sha: str('Blob sha of the file being replaced') }, ['owner', 'repo', 'path', 'content', 'message']),
    run: (args) => {
      const done = putContents(repoOf(args), String(args.path), { message: args.message, content: Buffer.from(String(args.content), 'utf8'), sha: args.sha, branch: args.branch });
      return out({ created: done.created, commit: { sha: done.commit }, content: { path: done.path, sha: done.blob } });
    },
  },
  {
    name: 'delete_file',
    description: 'Delete a file with a commit.',
    inputSchema: schemaOf({ owner: OWNER, repo: REPO, path: str('File path'), message: str('Commit message'), sha: str('Blob sha of the file'), branch: str('Target branch') }, ['owner', 'repo', 'path', 'message', 'sha']),
    run: (args) => {
      const done = deleteContents(repoOf(args), String(args.path), { message: args.message, sha: args.sha, branch: args.branch });
      return out({ commit: { sha: done.commit } });
    },
  },
  {
    name: 'list_commits',
    description: 'List commits reachable from a branch, tag or commit.',
    inputSchema: schemaOf({ owner: OWNER, repo: REPO, sha: str('Branch, tag or commit (default branch when omitted)') }, ['owner', 'repo']),
    run: (args) => {
      const repo = repoOf(args);
      const tip = resolveCommit(repo, typeof args.sha === 'string' && args.sha !== '' ? args.sha : 'HEAD');
      if (tip === null) throw new ToolFailure('Not Found: no such commit');
      return out(listCommits(repo, tip, 30).map((oid) => commitJson(repo, oid)));
    },
  },
  {
    name: 'get_me',
    description: 'Get the authenticated user.',
    inputSchema: schemaOf({}, []),
    run: () => out(userJson(viewer.login)),
  },
];

const TOOL_BY_NAME = new Map(TOOLS.map((tool) => [tool.name, tool]));

/** The list as it is served: built once, so two tools/list answers are byte-identical. */
const TOOL_LIST = TOOLS.map((tool) => ({ name: tool.name, title: tool.name.replace(/_/g, ' '), description: tool.description, inputSchema: tool.inputSchema }));

// ---------------------------------------------------------------- JSON-RPC

interface RpcError {
  code: number;
  message: string;
  data?: unknown;
}

class RpcFailure extends Error {
  code: number;

  constructor(code: number, message: string) {
    super(message);
    this.code = code;
  }
}

function typeProblem(schema: Json, args: Record<string, Json>): string | null {
  for (const key of schema.required as string[]) {
    if (args[key] === undefined || args[key] === null) return `Missing required argument: ${key}`;
  }
  for (const [key, prop] of Object.entries(schema.properties as Record<string, Json>)) {
    const value = args[key];
    if (value === undefined || value === null) continue;
    if (prop.type === 'string' && typeof value !== 'string') return `Argument ${key} must be a string`;
    if (prop.type === 'integer' && !(typeof value === 'number' && Number.isInteger(value)) && !(typeof value === 'string' && /^\d+$/.test(value))) return `Argument ${key} must be an integer`;
    if (prop.type === 'boolean' && typeof value !== 'boolean') return `Argument ${key} must be a boolean`;
    if (prop.type === 'array' && !Array.isArray(value)) return `Argument ${key} must be an array`;
    if (prop.enum !== undefined && !(prop.enum as unknown[]).includes(value)) return `Argument ${key} must be one of ${(prop.enum as string[]).join(', ')}`;
  }
  return null;
}

function toolResult(text: string, isError: boolean): Json {
  return isError ? { content: [{ type: 'text', text }], isError: true } : { content: [{ type: 'text', text }] };
}

function callTool(params: Json): Json {
  const name = typeof params?.name === 'string' ? params.name : '';
  const tool = TOOL_BY_NAME.get(name);
  if (tool === undefined) throw new RpcFailure(-32602, `Unknown tool: ${name}`);
  const raw = params.arguments;
  if (raw !== undefined && raw !== null && (typeof raw !== 'object' || Array.isArray(raw))) throw new RpcFailure(-32602, 'arguments must be an object');
  const args = (raw ?? {}) as Record<string, Json>;
  const problem = typeProblem(tool.inputSchema, args);
  if (problem !== null) return toolResult(problem, true);
  try {
    return toolResult(tool.run(args), false);
  } catch (err) {
    if (err instanceof ToolFailure) return toolResult(err.message, true);
    if (err instanceof ApiError) {
      const detail = err.errors === undefined ? '' : ` ${JSON.stringify(err.errors)}`;
      return toolResult(`${err.status} ${err.message}${detail}`, true);
    }
    return toolResult(`Internal error: ${err instanceof Error ? err.message : String(err)}`, true);
  }
}

let sessionCounter = 0;

function dispatch(method: string, params: Json): { result: Json; headers?: Record<string, string> } {
  switch (method) {
    case 'server/discover':
      return {
        result: {
          resultType: 'complete',
          supportedVersions: SUPPORTED_VERSIONS,
          capabilities: { tools: { listChanged: false } },
          serverInfo: SERVER_INFO,
          instructions: INSTRUCTIONS,
          _meta: { 'io.modelcontextprotocol/serverInfo': SERVER_INFO },
        },
      };
    case 'initialize': {
      const asked = typeof params?.protocolVersion === 'string' ? params.protocolVersion : '';
      const chosen = SUPPORTED_VERSIONS.includes(asked) && asked !== '2026-07-28' ? asked : '2025-11-25';
      sessionCounter += 1;
      return {
        result: { protocolVersion: chosen, capabilities: { tools: { listChanged: false } }, serverInfo: SERVER_INFO, instructions: INSTRUCTIONS },
        headers: { 'mcp-session-id': `forge-session-${sessionCounter}` },
      };
    }
    case 'ping':
      return { result: {} };
    case 'tools/list':
      return { result: { tools: TOOL_LIST } };
    case 'tools/call':
      return { result: callTool(params) };
    case 'resources/list':
      return { result: { resources: [] } };
    case 'resources/templates/list':
      return { result: { resourceTemplates: [] } };
    case 'prompts/list':
      return { result: { prompts: [] } };
    case 'logging/setLevel':
      return { result: {} };
    default:
      throw new RpcFailure(-32601, `Method not found: ${method}`);
  }
}

function answer(message: Json): { body: Json | null; headers?: Record<string, string> } {
  if (typeof message !== 'object' || message === null || Array.isArray(message) || typeof message.method !== 'string' || (message.jsonrpc !== undefined && message.jsonrpc !== '2.0')) {
    return { body: { jsonrpc: '2.0', id: message?.id ?? null, error: { code: -32600, message: 'Invalid Request' } satisfies RpcError } };
  }
  const isNotification = message.id === undefined;
  try {
    const { result, headers } = dispatch(message.method, message.params);
    return isNotification ? { body: null } : { body: { jsonrpc: '2.0', id: message.id, result }, headers };
  } catch (err) {
    if (isNotification) return { body: null };
    if (err instanceof RpcFailure) return { body: { jsonrpc: '2.0', id: message.id, error: { code: err.code, message: err.message } satisfies RpcError } };
    return { body: { jsonrpc: '2.0', id: message.id, error: { code: -32603, message: `Internal error: ${err instanceof Error ? err.message : String(err)}` } satisfies RpcError } };
  }
}

function send(req: IncomingMessage, res: ServerResponse, status: number, body: Json, headers: Record<string, string> = {}): void {
  const accept = String(req.headers.accept ?? '');
  const sse = accept.includes('text/event-stream') && !accept.includes('application/json');
  const text = sse ? `event: message\ndata: ${JSON.stringify(body)}\n\n` : JSON.stringify(body);
  const data = Buffer.from(text, 'utf8');
  res.writeHead(status, { 'content-type': sse ? 'text/event-stream' : 'application/json; charset=utf-8', 'content-length': data.length, 'cache-control': 'no-cache', ...headers });
  res.end(data);
}

/** POST /mcp (authentication has already been checked). */
export async function handleMcp(req: IncomingMessage, res: ServerResponse): Promise<void> {
  const raw = await readBody(req);
  let message: Json;
  try {
    message = JSON.parse(raw.toString('utf8'));
  } catch {
    send(req, res, 400, { jsonrpc: '2.0', id: null, error: { code: -32700, message: 'Parse error' } });
    return;
  }
  if (Array.isArray(message)) {
    if (message.length === 0) {
      send(req, res, 400, { jsonrpc: '2.0', id: null, error: { code: -32600, message: 'Invalid Request' } });
      return;
    }
    const bodies = message.map((entry) => answer(entry).body).filter((body) => body !== null);
    if (bodies.length === 0) {
      res.writeHead(202);
      res.end();
      return;
    }
    send(req, res, 200, bodies);
    return;
  }
  if (typeof message !== 'object' || message === null) {
    send(req, res, 400, { jsonrpc: '2.0', id: null, error: { code: -32600, message: 'Invalid Request' } });
    return;
  }
  const { body, headers } = answer(message);
  if (body === null) {
    res.writeHead(202);
    res.end();
    return;
  }
  send(req, res, 200, body, headers);
}

/** The JSON-RPC body of an unauthenticated MCP request. */
export const MCP_UNAUTHORIZED = { jsonrpc: '2.0', id: null, error: { code: -32001, message: 'Unauthorized: a bearer token is required' } };
