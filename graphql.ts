// GitHub GraphQL v4 at POST /api/graphql: a small spec-shaped engine (parser, schema, introspection, validation,
// executor) with the schema gh and github-mcp-server rely on. Resolvers read the same store as REST and MCP.

import type { IncomingMessage, ServerResponse } from 'node:http';
import {
  ApiError,
  addComment,
  allRepos,
  commentsById,
  createIssue,
  createPull,
  createRepo,
  closePull,
  getRepo,
  getRepoById,
  globalId,
  issuesById,
  mergePull,
  parseGlobalId,
  pullBaseSha,
  pullCommits,
  pullFiles,
  pullHeadSha,
  pullMergeable,
  pullsById,
  reopenPull,
  setIssueState,
  updateIssue,
  viewer,
} from './store.ts';
import type { Comment, Issue, Pull, Repo } from './store.ts';
import { commitInfo } from './gitops.ts';
import { baseUrl } from './rest.ts';
import { HttpError, readBody, sendJson } from './util.ts';

// ---------------------------------------------------------------- lexer and parser

interface Tok {
  t: 'punct' | 'name' | 'int' | 'float' | 'string' | 'eof';
  v: string;
  line: number;
  col: number;
}

class GqlSyntaxError extends Error {
  line: number;
  col: number;

  constructor(message: string, line: number, col: number) {
    super(message);
    this.line = line;
    this.col = col;
  }
}

function lex(src: string): Tok[] {
  const toks: Tok[] = [];
  let i = 0;
  let line = 1;
  let lineStart = 0;
  const n = src.length;
  while (i < n) {
    const ch = src.charCodeAt(i);
    if (ch === 10) {
      line += 1;
      i += 1;
      lineStart = i;
      continue;
    }
    if (ch === 32 || ch === 9 || ch === 13 || ch === 44 || ch === 0xfeff) {
      i += 1;
      continue;
    }
    if (ch === 35) {
      while (i < n && src.charCodeAt(i) !== 10 && src.charCodeAt(i) !== 13) i += 1;
      continue;
    }
    const col = i - lineStart + 1;
    const c = src[i] as string;
    if (c === '.' && src.startsWith('...', i)) {
      toks.push({ t: 'punct', v: '...', line, col });
      i += 3;
    } else if ('!$&():=@[]{}|'.includes(c)) {
      toks.push({ t: 'punct', v: c, line, col });
      i += 1;
    } else if (/[_A-Za-z]/.test(c)) {
      let j = i + 1;
      while (j < n && /[_0-9A-Za-z]/.test(src[j] as string)) j += 1;
      toks.push({ t: 'name', v: src.slice(i, j), line, col });
      i = j;
    } else if (c === '-' || (c >= '0' && c <= '9')) {
      const match = /^-?(?:0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?/.exec(src.slice(i, i + 40));
      if (match === null) throw new GqlSyntaxError(`Parse error on "${c}" at [${line}, ${col}]`, line, col);
      toks.push({ t: match[1] !== undefined || match[2] !== undefined ? 'float' : 'int', v: match[0], line, col });
      i += match[0].length;
    } else if (c === '"') {
      if (src.startsWith('"""', i)) {
        const close = src.indexOf('"""', i + 3);
        if (close < 0) throw new GqlSyntaxError(`Unterminated block string at [${line}, ${col}]`, line, col);
        const raw = src.slice(i + 3, close).replace(/\\"""/g, '"""');
        toks.push({ t: 'string', v: raw.replace(/^\n/, '').replace(/\n\s*$/, ''), line, col });
        for (const ch2 of src.slice(i, close + 3)) if (ch2 === '\n') line += 1;
        i = close + 3;
      } else {
        let j = i + 1;
        let out = '';
        for (;;) {
          if (j >= n || src[j] === '\n') throw new GqlSyntaxError(`Unterminated string at [${line}, ${col}]`, line, col);
          const d = src[j] as string;
          if (d === '"') break;
          if (d === '\\') {
            const e = src[j + 1] as string;
            if (e === 'u') {
              out += String.fromCharCode(parseInt(src.slice(j + 2, j + 6), 16));
              j += 6;
              continue;
            }
            out += ({ n: '\n', t: '\t', r: '\r', b: '\b', f: '\f', '"': '"', '\\': '\\', '/': '/' } as Record<string, string>)[e] ?? e;
            j += 2;
            continue;
          }
          out += d;
          j += 1;
        }
        toks.push({ t: 'string', v: out, line, col });
        i = j + 1;
      }
    } else {
      throw new GqlSyntaxError(`Parse error on "${c}" at [${line}, ${col}]`, line, col);
    }
  }
  toks.push({ t: 'eof', v: '<EOF>', line, col: i - lineStart + 1 });
  return toks;
}

type ValueNode =
  | { k: 'var'; name: string }
  | { k: 'int' | 'float'; v: number }
  | { k: 'string'; v: string }
  | { k: 'bool'; v: boolean }
  | { k: 'null' }
  | { k: 'enum'; v: string }
  | { k: 'list'; items: ValueNode[] }
  | { k: 'obj'; fields: Record<string, ValueNode> };

interface Directive {
  name: string;
  args: Record<string, ValueNode>;
}

interface FieldNode {
  kind: 'field';
  alias: string | null;
  name: string;
  args: Record<string, ValueNode>;
  directives: Directive[];
  selection: Selection[] | null;
  line: number;
  col: number;
}

interface SpreadNode {
  kind: 'spread';
  name: string;
  directives: Directive[];
  line: number;
  col: number;
}

interface InlineNode {
  kind: 'inline';
  typeCond: string | null;
  directives: Directive[];
  selection: Selection[];
}

type Selection = FieldNode | SpreadNode | InlineNode;

interface VarDef {
  name: string;
  type: string;
  def: ValueNode | null;
}

interface Operation {
  type: 'query' | 'mutation' | 'subscription';
  name: string | null;
  vars: VarDef[];
  selection: Selection[];
}

interface Fragment {
  name: string;
  typeCond: string;
  selection: Selection[];
}

interface Document {
  operations: Operation[];
  fragments: Record<string, Fragment>;
}

class Parser {
  toks: Tok[];
  pos = 0;

  constructor(toks: Tok[]) {
    this.toks = toks;
  }

  peek(): Tok {
    return this.toks[this.pos] as Tok;
  }

  next(): Tok {
    const tok = this.toks[this.pos] as Tok;
    if (tok.t !== 'eof') this.pos += 1;
    return tok;
  }

  fail(tok: Tok): never {
    const label = tok.t === 'eof' ? 'end of file' : `"${tok.v}"`;
    throw new GqlSyntaxError(`Parse error on ${label} (${tok.t === 'name' ? 'IDENTIFIER' : tok.t === 'eof' ? 'EOF' : tok.t.toUpperCase()}) at [${tok.line}, ${tok.col}]`, tok.line, tok.col);
  }

  isPunct(v: string): boolean {
    const tok = this.peek();
    return tok.t === 'punct' && tok.v === v;
  }

  expectPunct(v: string): Tok {
    const tok = this.peek();
    if (tok.t !== 'punct' || tok.v !== v) this.fail(tok);
    return this.next();
  }

  expectName(): Tok {
    const tok = this.peek();
    if (tok.t !== 'name') this.fail(tok);
    return this.next();
  }

  document(): Document {
    const doc: Document = { operations: [], fragments: {} };
    while (this.peek().t !== 'eof') {
      if (this.isPunct('{')) {
        doc.operations.push({ type: 'query', name: null, vars: [], selection: this.selectionSet() });
        continue;
      }
      const tok = this.expectName();
      if (tok.v === 'query' || tok.v === 'mutation' || tok.v === 'subscription') {
        let name: string | null = null;
        if (this.peek().t === 'name') name = this.next().v;
        const vars: VarDef[] = [];
        if (this.isPunct('(')) {
          this.next();
          while (!this.isPunct(')')) {
            this.expectPunct('$');
            const varName = this.expectName().v;
            this.expectPunct(':');
            const type = this.typeRef();
            let def: ValueNode | null = null;
            if (this.isPunct('=')) {
              this.next();
              def = this.value(true);
            }
            this.directives();
            vars.push({ name: varName, type, def });
          }
          this.next();
        }
        this.directives();
        doc.operations.push({ type: tok.v, name, vars, selection: this.selectionSet() });
      } else if (tok.v === 'fragment') {
        const name = this.expectName().v;
        const on = this.expectName();
        if (on.v !== 'on') this.fail(on);
        const typeCond = this.expectName().v;
        this.directives();
        doc.fragments[name] = { name, typeCond, selection: this.selectionSet() };
      } else this.fail(tok);
    }
    return doc;
  }

  typeRef(): string {
    let out: string;
    if (this.isPunct('[')) {
      this.next();
      out = `[${this.typeRef()}]`;
      this.expectPunct(']');
    } else out = this.expectName().v;
    if (this.isPunct('!')) {
      this.next();
      out += '!';
    }
    return out;
  }

  directives(): Directive[] {
    const out: Directive[] = [];
    while (this.isPunct('@')) {
      this.next();
      const name = this.expectName().v;
      out.push({ name, args: this.isPunct('(') ? this.args() : {} });
    }
    return out;
  }

  args(): Record<string, ValueNode> {
    const out: Record<string, ValueNode> = {};
    this.expectPunct('(');
    while (!this.isPunct(')')) {
      const name = this.expectName().v;
      this.expectPunct(':');
      out[name] = this.value(false);
    }
    this.next();
    return out;
  }

  selectionSet(): Selection[] {
    this.expectPunct('{');
    const out: Selection[] = [];
    while (!this.isPunct('}')) {
      if (this.peek().t === 'eof') this.fail(this.peek());
      out.push(this.selection());
    }
    this.next();
    if (out.length === 0) this.fail(this.peek());
    return out;
  }

  selection(): Selection {
    if (this.isPunct('...')) {
      const spread = this.next();
      const tok = this.peek();
      if (tok.t === 'name' && tok.v !== 'on') {
        this.next();
        return { kind: 'spread', name: tok.v, directives: this.directives(), line: spread.line, col: spread.col };
      }
      let typeCond: string | null = null;
      if (tok.t === 'name' && tok.v === 'on') {
        this.next();
        typeCond = this.expectName().v;
      }
      const directives = this.directives();
      return { kind: 'inline', typeCond, directives, selection: this.selectionSet() };
    }
    const first = this.expectName();
    let alias: string | null = null;
    let name = first.v;
    if (this.isPunct(':')) {
      this.next();
      alias = name;
      name = this.expectName().v;
    }
    const args = this.isPunct('(') ? this.args() : {};
    const directives = this.directives();
    const selection = this.isPunct('{') ? this.selectionSet() : null;
    return { kind: 'field', alias, name, args, directives, selection, line: first.line, col: first.col };
  }

  value(isConst: boolean): ValueNode {
    const tok = this.next();
    if (tok.t === 'punct') {
      if (tok.v === '$' && !isConst) return { k: 'var', name: this.expectName().v };
      if (tok.v === '[') {
        const items: ValueNode[] = [];
        while (!this.isPunct(']')) items.push(this.value(isConst));
        this.next();
        return { k: 'list', items };
      }
      if (tok.v === '{') {
        const fields: Record<string, ValueNode> = {};
        while (!this.isPunct('}')) {
          const name = this.expectName().v;
          this.expectPunct(':');
          fields[name] = this.value(isConst);
        }
        this.next();
        return { k: 'obj', fields };
      }
      this.fail(tok);
    }
    if (tok.t === 'int') return { k: 'int', v: Number(tok.v) };
    if (tok.t === 'float') return { k: 'float', v: Number(tok.v) };
    if (tok.t === 'string') return { k: 'string', v: tok.v };
    if (tok.t === 'name') {
      if (tok.v === 'true') return { k: 'bool', v: true };
      if (tok.v === 'false') return { k: 'bool', v: false };
      if (tok.v === 'null') return { k: 'null' };
      return { k: 'enum', v: tok.v };
    }
    return this.fail(tok);
  }
}

// ---------------------------------------------------------------- schema

type Json = any; // resolvers work on loosely typed store entities and plain JSON
type Resolver = (src: Json, args: Record<string, Json>) => Json;

interface FieldDef {
  type: string;
  args?: Record<string, string>;
  resolve?: Resolver;
}

interface TypeDef {
  kind: 'OBJECT' | 'INTERFACE' | 'UNION' | 'ENUM' | 'INPUT_OBJECT' | 'SCALAR';
  fields?: Record<string, FieldDef>;
  values?: string[];
  inputFields?: Record<string, string>;
  interfaces?: string[];
  possible?: string[];
  dynamic?: boolean;
}

const f = (type: string, resolve?: Resolver, args?: Record<string, string>): FieldDef => ({ type, resolve, args });
const scalar = (): TypeDef => ({ kind: 'SCALAR' });
const en = (...values: string[]): TypeDef => ({ kind: 'ENUM', values });
const input = (inputFields: Record<string, string>): TypeDef => ({ kind: 'INPUT_OBJECT', inputFields });
const obj = (fields: Record<string, FieldDef>, interfaces: string[] = []): TypeDef => ({ kind: 'OBJECT', fields, interfaces });

const PAGE: Record<string, string> = { first: 'Int', after: 'String', last: 'Int', before: 'String' };

// -- cursors and connections

function encodeCursor(index: number): string {
  return Buffer.from(`cursor:${index}`, 'utf8').toString('base64');
}

function decodeCursor(cursor: unknown): number {
  if (typeof cursor !== 'string') return -1;
  const match = /^cursor:(\d+)$/.exec(Buffer.from(cursor, 'base64').toString('utf8'));
  if (match === null) throw new GqlFailure(`\`${cursor}\` does not appear to be a valid cursor.`);
  return parseInt(match[1] as string, 10);
}

interface Connection {
  nodes: Json[];
  edges: Array<{ cursor: string; node: Json }>;
  pageInfo: { hasNextPage: boolean; hasPreviousPage: boolean; startCursor: string | null; endCursor: string | null };
  totalCount: number;
  [extra: string]: Json;
}

function makeConnection(items: readonly Json[], args: Record<string, Json>): Connection {
  let start = 0;
  let end = items.length;
  if (args.after !== undefined && args.after !== null) start = Math.min(items.length, decodeCursor(args.after) + 1);
  if (args.before !== undefined && args.before !== null) end = Math.max(start, Math.min(items.length, decodeCursor(args.before)));
  if (typeof args.first === 'number') {
    if (args.first < 0) throw new GqlFailure('Argument \'first\' must be a non-negative integer.');
    end = Math.min(end, start + args.first);
  }
  if (typeof args.last === 'number') {
    if (args.last < 0) throw new GqlFailure('Argument \'last\' must be a non-negative integer.');
    start = Math.max(start, end - args.last);
  }
  const nodes = items.slice(start, end);
  return {
    nodes,
    edges: nodes.map((node, i) => ({ cursor: encodeCursor(start + i), node })),
    pageInfo: {
      hasNextPage: end < items.length,
      hasPreviousPage: start > 0,
      startCursor: nodes.length > 0 ? encodeCursor(start) : null,
      endCursor: nodes.length > 0 ? encodeCursor(end - 1) : null,
    },
    totalCount: items.length,
  };
}

function connectionTypes(name: string, node: string, extra: Record<string, FieldDef> = {}): Record<string, TypeDef> {
  return {
    [`${name}Connection`]: obj({ nodes: f(`[${node}]`), edges: f(`[${name}Edge]`), pageInfo: f('PageInfo!'), totalCount: f('Int!'), ...extra }),
    [`${name}Edge`]: obj({ cursor: f('String!'), node: f(node) }),
  };
}

// -- entity helpers

const dateField = (key: string): Resolver => (s) => s[key];

function userOf(login: string): Json {
  return { login, id: login === viewer.login ? viewer.id : 1_000_000, __user: true };
}

function repoUrl(repo: Repo): string {
  return `${baseUrl()}/${repo.owner}/${repo.name}`;
}

function refOf(repo: Repo, branch: string): Json {
  const oid = repo.refs.get(`refs/heads/${branch}`);
  return oid === undefined ? null : { repo, name: branch, oid };
}

function commitOfOid(repo: Repo, oid: string): Json {
  return commitInfo(repo, oid) === null ? null : { repo, oid, __commit: true };
}

function issueState(issue: Issue): string {
  return issue.state.toUpperCase();
}

function pullState(pull: Pull): string {
  return pull.merged ? 'MERGED' : pull.state.toUpperCase();
}

function orderItems<T extends { number: number; seq: number; comments: unknown[] }>(items: T[], orderBy: Json): T[] {
  const field = orderBy?.field ?? 'CREATED_AT';
  const desc = orderBy?.direction === 'DESC';
  const key = (item: T): number => (field === 'UPDATED_AT' ? item.seq : field === 'COMMENTS' ? item.comments.length : item.number);
  const sorted = items.slice().sort((a, b) => key(a) - key(b) || a.number - b.number);
  return desc ? sorted.reverse() : sorted;
}

function repoFromId(id: unknown, what = 'Repository'): Repo {
  const parsed = typeof id === 'string' ? parseGlobalId(id) : null;
  const repo = parsed !== null && parsed.type === 'Repository' ? getRepoById(parsed.id) : undefined;
  if (repo === undefined) throw new GqlFailure(`Could not resolve to a ${what} with the id '${String(id)}'.`);
  return repo;
}

function nodeById(id: unknown): Json {
  const parsed = typeof id === 'string' ? parseGlobalId(id) : null;
  if (parsed === null) return null;
  switch (parsed.type) {
    case 'Repository':
      return getRepoById(parsed.id) ?? null;
    case 'Issue':
      return issuesById.get(parsed.id) ?? null;
    case 'PullRequest':
      return pullsById.get(parsed.id) ?? null;
    case 'IssueComment':
      return commentsById.get(parsed.id) ?? null;
    case 'User':
      return parsed.id === viewer.id ? userOf(viewer.login) : null;
    case 'Label':
      for (const repo of allRepos()) for (const label of repo.labels.values()) if (label.id === parsed.id) return label;
      return null;
    default:
      return null;
  }
}

function resolveTypeName(src: Json): string {
  if (src === null || typeof src !== 'object') return 'User';
  if (src.kind === 'repo') return 'Repository';
  if (src.kind === 'issue') return 'Issue';
  if (src.kind === 'pull') return 'PullRequest';
  if (src.__commit === true) return 'Commit';
  if ('parent' in src && 'body' in src) return 'IssueComment';
  if ('color' in src) return 'Label';
  return 'User';
}

// -- search

function searchItems(queryText: string, type: string): Json[] {
  const tokens = queryText.match(/(?:[^\s"]+:"[^"]*")|(?:"[^"]*")|\S+/g) ?? [];
  const words: string[] = [];
  const quals: Array<[string, string]> = [];
  for (const raw of tokens) {
    const token = raw.replace(/"/g, '');
    const colon = token.indexOf(':');
    if (colon > 0 && /^[a-z-]+$/i.test(token.slice(0, colon))) quals.push([token.slice(0, colon).toLowerCase(), token.slice(colon + 1)]);
    else words.push(token.toLowerCase());
  }
  const qual = (name: string): string[] => quals.filter(([key]) => key === name).map(([, value]) => value);
  const textMatches = (...fields: Array<string | null>): boolean => {
    const hay = fields.map((field) => (field ?? '').toLowerCase()).join('\n');
    return words.every((word) => hay.includes(word));
  };
  if (type === 'REPOSITORY') {
    return allRepos()
      .filter((repo) => textMatches(repo.name, repo.description) && qual('user').concat(qual('owner'), qual('org')).every((owner) => owner.toLowerCase() === repo.owner.toLowerCase()))
      .sort((a, b) => a.id - b.id);
  }
  if (type === 'USER') return textMatches(viewer.login) ? [userOf(viewer.login)] : [];
  if (type !== 'ISSUE' && type !== 'ISSUE_ADVANCED') return [];
  const repoFilters = qual('repo').map((name) => name.toLowerCase());
  const kinds = qual('is').map((value) => value.toLowerCase());
  const wantIssue = kinds.includes('issue') || qual('type').includes('issue');
  const wantPr = kinds.includes('pr') || kinds.includes('pull-request') || qual('type').includes('pr');
  const states = [...kinds, ...qual('state').map((value) => value.toLowerCase())].filter((value) => value === 'open' || value === 'closed' || value === 'merged');
  const labels = qual('label').map((value) => value.toLowerCase());
  const authors = qual('author').map((value) => value.toLowerCase());
  const out: Array<Issue | Pull> = [];
  for (const repo of allRepos()) {
    if (repoFilters.length > 0 && !repoFilters.includes(`${repo.owner}/${repo.name}`.toLowerCase())) continue;
    const pool: Array<Issue | Pull> = [...(wantPr && !wantIssue ? [] : repo.issueList), ...(wantIssue && !wantPr ? [] : repo.pullList)];
    for (const item of pool) {
      if (!textMatches(item.title, item.body)) continue;
      const state = item.kind === 'pull' && item.merged ? 'merged' : item.state;
      if (states.length > 0 && !states.includes(state)) continue;
      if (labels.length > 0 && !labels.every((name) => item.labels.some((label) => label.name.toLowerCase() === name))) continue;
      if (authors.length > 0 && !authors.includes(item.user.toLowerCase())) continue;
      out.push(item);
    }
  }
  return out.sort((a, b) => b.seq - a.seq);
}

// -- mutation helpers

class GqlFailure extends Error {}

function wrapApi<T>(run: () => T): T {
  try {
    return run();
  } catch (err) {
    if (err instanceof ApiError) {
      const detail = Array.isArray(err.errors) && err.errors.length > 0 ? err.errors[0] : null;
      const message = detail !== null && typeof detail === 'object' && typeof (detail as { message?: unknown }).message === 'string' ? (detail as { message: string }).message : null;
      const field = detail !== null && typeof detail === 'object' ? (detail as { field?: unknown }).field : null;
      const code = detail !== null && typeof detail === 'object' ? (detail as { code?: unknown }).code : null;
      if (code === 'missing_field' && typeof field === 'string') throw new GqlFailure(`${field.charAt(0).toUpperCase()}${field.slice(1)} can't be blank`);
      throw new GqlFailure(message ?? err.message);
    }
    throw err;
  }
}

function issueFromId(id: unknown): Issue {
  const node = nodeById(id);
  if (node === null || node.kind !== 'issue') throw new GqlFailure(`Could not resolve to an Issue with the id '${String(id)}'.`);
  return node as Issue;
}

function pullFromId(id: unknown): Pull {
  const node = nodeById(id);
  if (node === null || node.kind !== 'pull') throw new GqlFailure(`Could not resolve to a PullRequest with the id '${String(id)}'.`);
  return node as Pull;
}

function clientId(args: Record<string, Json>): Json {
  return args.input?.clientMutationId ?? null;
}

// -- the types

const SEARCH_TYPES = ['ISSUE', 'ISSUE_ADVANCED', 'REPOSITORY', 'USER', 'DISCUSSION'];

function buildSchema(): Record<string, TypeDef> {
  const schema: Record<string, TypeDef> = {
    String: scalar(),
    Int: scalar(),
    Float: scalar(),
    Boolean: scalar(),
    ID: scalar(),
    DateTime: scalar(),
    URI: scalar(),
    HTML: scalar(),
    GitObjectID: scalar(),
    IssueState: en('OPEN', 'CLOSED'),
    IssueStateReason: en('COMPLETED', 'NOT_PLANNED', 'REOPENED'),
    IssueClosedStateReason: en('COMPLETED', 'NOT_PLANNED', 'DUPLICATE'),
    PullRequestState: en('OPEN', 'CLOSED', 'MERGED'),
    MergeableState: en('MERGEABLE', 'CONFLICTING', 'UNKNOWN'),
    PullRequestMergeMethod: en('MERGE', 'SQUASH', 'REBASE'),
    RepositoryPermission: en('ADMIN', 'MAINTAIN', 'WRITE', 'TRIAGE', 'READ'),
    RepositoryVisibility: en('PRIVATE', 'PUBLIC', 'INTERNAL'),
    SearchType: en(...SEARCH_TYPES),
    OrderDirection: en('ASC', 'DESC'),
    IssueOrderField: en('CREATED_AT', 'UPDATED_AT', 'COMMENTS'),
    PullRequestOrderField: en('CREATED_AT', 'UPDATED_AT'),
    PatchStatus: en('ADDED', 'DELETED', 'RENAMED', 'COPIED', 'MODIFIED', 'CHANGED'),

    IssueOrder: input({ field: 'IssueOrderField!', direction: 'OrderDirection!' }),
    IssueFilters: input({ assignee: 'String', createdBy: 'String', labels: '[String!]', mentioned: 'String', since: 'DateTime', states: '[IssueState!]' }),
    IssueOrPullRequestOrder: input({ field: 'IssueOrderField!', direction: 'OrderDirection!' }),
    PullRequestOrder: input({ field: 'PullRequestOrderField!', direction: 'OrderDirection!' }),
    CreateIssueInput: input({ repositoryId: 'ID!', title: 'String!', body: 'String', assigneeIds: '[ID!]', labelIds: '[ID!]', milestoneId: 'ID', clientMutationId: 'String' }),
    UpdateIssueInput: input({ id: 'ID!', title: 'String', body: 'String', state: 'IssueState', labelIds: '[ID!]', clientMutationId: 'String' }),
    CloseIssueInput: input({ issueId: 'ID!', stateReason: 'IssueClosedStateReason', clientMutationId: 'String' }),
    ReopenIssueInput: input({ issueId: 'ID!', clientMutationId: 'String' }),
    CreatePullRequestInput: input({ repositoryId: 'ID!', baseRefName: 'String!', headRefName: 'String!', title: 'String!', body: 'String', draft: 'Boolean', maintainerCanModify: 'Boolean', clientMutationId: 'String' }),
    MergePullRequestInput: input({ pullRequestId: 'ID!', commitHeadline: 'String', commitBody: 'String', expectedHeadOid: 'GitObjectID', mergeMethod: 'PullRequestMergeMethod', authorEmail: 'String', clientMutationId: 'String' }),
    ClosePullRequestInput: input({ pullRequestId: 'ID!', clientMutationId: 'String' }),
    ReopenPullRequestInput: input({ pullRequestId: 'ID!', clientMutationId: 'String' }),
    AddCommentInput: input({ subjectId: 'ID!', body: 'String!', clientMutationId: 'String' }),
    CreateRepositoryInput: input({ name: 'String!', visibility: 'RepositoryVisibility!', description: 'String', clientMutationId: 'String' }),

    Node: { kind: 'INTERFACE', fields: { id: f('ID!') }, possible: ['Repository', 'Issue', 'PullRequest', 'IssueComment', 'User', 'Label'] },
    Actor: { kind: 'INTERFACE', fields: { login: f('String!'), url: f('URI!'), avatarUrl: f('URI!') }, possible: ['User'] },
    RepositoryOwner: { kind: 'INTERFACE', fields: { id: f('ID!'), login: f('String!'), url: f('URI!') }, possible: ['User'] },
    GitObject: { kind: 'INTERFACE', fields: { oid: f('GitObjectID!') }, possible: ['Commit'] },
    SearchResultItem: { kind: 'UNION', possible: ['Issue', 'PullRequest', 'Repository', 'User'] },

    PageInfo: obj({ hasNextPage: f('Boolean!'), hasPreviousPage: f('Boolean!'), startCursor: f('String'), endCursor: f('String') }),
    RateLimit: obj({ limit: f('Int!', () => 5000), cost: f('Int!', () => 1), remaining: f('Int!', () => 5000), used: f('Int!', () => 0), nodeCount: f('Int!', () => 0), resetAt: f('DateTime!', () => '2025-01-01T01:00:00Z') }),

    User: obj(
      {
        id: f('ID!', (u) => globalId('User', u.id)),
        databaseId: f('Int', (u) => u.id),
        login: f('String!', (u) => u.login),
        name: f('String', (u) => u.login),
        url: f('URI!', (u) => `${baseUrl()}/${u.login}`),
        avatarUrl: f('URI!', (u) => `${baseUrl()}/avatars/u/${u.id}`, { size: 'Int' }),
        repositories: f('RepositoryConnection!', (_u, a) => makeConnection(allRepos().sort((x, y) => x.id - y.id), a), { ...PAGE, orderBy: 'String', privacy: 'RepositoryVisibility' }),
      },
      ['Node', 'Actor', 'RepositoryOwner'],
    ),
    Label: obj(
      {
        id: f('ID!', (l) => globalId('Label', l.id)),
        name: f('String!'),
        color: f('String!'),
        description: f('String'),
      },
      ['Node'],
    ),
    GitActor: obj({ name: f('String'), email: f('String'), date: f('DateTime'), user: f('User') }),
    Commit: obj(
      {
        id: f('ID!', (c) => globalId('Commit', parseInt(c.oid.slice(0, 8), 16))),
        oid: f('GitObjectID!', (c) => c.oid),
        abbreviatedOid: f('String!', (c) => c.oid.slice(0, 7)),
        message: f('String!', (c) => commitInfo(c.repo, c.oid)?.message.replace(/\n$/, '') ?? ''),
        messageHeadline: f('String!', (c) => (commitInfo(c.repo, c.oid)?.message ?? '').split('\n')[0]),
        messageBody: f('String!', (c) => (commitInfo(c.repo, c.oid)?.message ?? '').split('\n').slice(1).join('\n').trim()),
        committedDate: f('DateTime!', (c) => new Date((commitInfo(c.repo, c.oid)?.committer.when ?? 0) * 1000).toISOString()),
        authoredDate: f('DateTime!', (c) => new Date((commitInfo(c.repo, c.oid)?.author.when ?? 0) * 1000).toISOString()),
        url: f('URI!', (c) => `${repoUrl(c.repo)}/commit/${c.oid}`),
        author: f('GitActor', (c) => {
          const info = commitInfo(c.repo, c.oid);
          return info === null ? null : { name: info.author.name, email: info.author.email, date: new Date(info.author.when * 1000).toISOString(), user: info.author.name === viewer.login ? userOf(viewer.login) : null };
        }),
        committer: f('GitActor', (c) => {
          const info = commitInfo(c.repo, c.oid);
          return info === null ? null : { name: info.committer.name, email: info.committer.email, date: new Date(info.committer.when * 1000).toISOString(), user: info.committer.name === viewer.login ? userOf(viewer.login) : null };
        }),
      },
      ['GitObject'],
    ),
    Ref: obj({
      id: f('ID!', (r) => globalId('Ref', r.repo.id * 1000 + r.name.length)),
      name: f('String!'),
      prefix: f('String!', () => 'refs/heads/'),
      target: f('GitObject', (r) => commitOfOid(r.repo, r.oid)),
    }),
    IssueComment: obj(
      {
        id: f('ID!', (c: Comment) => globalId('IssueComment', c.id)),
        databaseId: f('Int', (c: Comment) => c.id),
        body: f('String!'),
        bodyText: f('String!', (c: Comment) => c.body),
        author: f('Actor', (c: Comment) => userOf(c.user)),
        createdAt: f('DateTime!', dateField('createdAt')),
        updatedAt: f('DateTime!', dateField('updatedAt')),
        url: f('URI!', (c: Comment) => `${repoUrl(c.parent.repo)}/issues/${c.parent.number}#issuecomment-${c.id}`),
        issue: f('Issue', (c: Comment) => (c.parent.kind === 'issue' ? c.parent : null)),
      },
      ['Node'],
    ),
    Issue: obj(
      {
        id: f('ID!', (i: Issue) => globalId('Issue', i.id)),
        databaseId: f('Int', (i: Issue) => i.id),
        number: f('Int!'),
        title: f('String!'),
        body: f('String!', (i: Issue) => i.body ?? ''),
        bodyText: f('String!', (i: Issue) => i.body ?? ''),
        state: f('IssueState!', issueState),
        stateReason: f('IssueStateReason', (i: Issue) => (i.stateReason === null ? null : i.stateReason.toUpperCase())),
        closed: f('Boolean!', (i: Issue) => i.state === 'closed'),
        locked: f('Boolean!', () => false),
        url: f('URI!', (i: Issue) => `${repoUrl(i.repo)}/issues/${i.number}`),
        createdAt: f('DateTime!', dateField('createdAt')),
        updatedAt: f('DateTime!', dateField('updatedAt')),
        closedAt: f('DateTime', dateField('closedAt')),
        author: f('Actor', (i: Issue) => userOf(i.user)),
        repository: f('Repository!', (i: Issue) => i.repo),
        labels: f('LabelConnection', (i: Issue, a) => makeConnection(i.labels, a), PAGE),
        comments: f('IssueCommentConnection!', (i: Issue, a) => makeConnection(i.comments, a), PAGE),
        assignees: f('UserConnection!', (_i, a) => makeConnection([], a), PAGE),
      },
      ['Node'],
    ),
    PullRequestChangedFile: obj({
      path: f('String!', (x) => x.filename),
      additions: f('Int!'),
      deletions: f('Int!'),
      changeType: f('PatchStatus!', (x) => (x.status === 'added' ? 'ADDED' : x.status === 'removed' ? 'DELETED' : 'MODIFIED')),
    }),
    PullRequestCommit: obj({
      id: f('ID!', (x) => globalId('PullRequestCommit', parseInt(x.oid.slice(0, 8), 16))),
      commit: f('Commit!', (x) => commitOfOid(x.repo, x.oid)),
      url: f('URI!', (x) => `${repoUrl(x.repo)}/pull/${x.number}/commits/${x.oid}`),
    }),
    PullRequest: obj(
      {
        id: f('ID!', (p: Pull) => globalId('PullRequest', p.id)),
        databaseId: f('Int', (p: Pull) => p.id),
        number: f('Int!'),
        title: f('String!'),
        body: f('String!', (p: Pull) => p.body ?? ''),
        bodyText: f('String!', (p: Pull) => p.body ?? ''),
        state: f('PullRequestState!', pullState),
        closed: f('Boolean!', (p: Pull) => p.state === 'closed'),
        merged: f('Boolean!'),
        mergedAt: f('DateTime', dateField('mergedAt')),
        isDraft: f('Boolean!', (p: Pull) => p.draft),
        mergeable: f('MergeableState!', (p: Pull) => {
          const mergeable = pullMergeable(p);
          return mergeable === true ? 'MERGEABLE' : mergeable === false ? 'CONFLICTING' : 'UNKNOWN';
        }),
        headRefName: f('String!', (p: Pull) => p.headRef),
        baseRefName: f('String!', (p: Pull) => p.baseRef),
        headRefOid: f('GitObjectID!', (p: Pull) => pullHeadSha(p)),
        baseRefOid: f('GitObjectID!', (p: Pull) => pullBaseSha(p)),
        url: f('URI!', (p: Pull) => `${repoUrl(p.repo)}/pull/${p.number}`),
        createdAt: f('DateTime!', dateField('createdAt')),
        updatedAt: f('DateTime!', dateField('updatedAt')),
        closedAt: f('DateTime', dateField('closedAt')),
        author: f('Actor', (p: Pull) => userOf(p.user)),
        repository: f('Repository!', (p: Pull) => p.repo),
        baseRepository: f('Repository', (p: Pull) => p.repo),
        headRepository: f('Repository', (p: Pull) => p.repo),
        mergeCommit: f('Commit', (p: Pull) => (p.mergeCommitSha === null ? null : commitOfOid(p.repo, p.mergeCommitSha))),
        labels: f('LabelConnection', (p: Pull, a) => makeConnection(p.labels, a), PAGE),
        comments: f('IssueCommentConnection!', (p: Pull, a) => makeConnection(p.comments, a), PAGE),
        files: f('PullRequestChangedFileConnection', (p: Pull, a) => makeConnection(pullFiles(p), a), PAGE),
        commits: f('PullRequestCommitConnection!', (p: Pull, a) => makeConnection(pullCommits(p).reverse().map((oid) => ({ oid, repo: p.repo, number: p.number })), a), PAGE),
        additions: f('Int!', (p: Pull) => pullFiles(p).reduce((n, file) => n + file.additions, 0)),
        deletions: f('Int!', (p: Pull) => pullFiles(p).reduce((n, file) => n + file.deletions, 0)),
        changedFiles: f('Int!', (p: Pull) => pullFiles(p).length),
      },
      ['Node'],
    ),
    Repository: obj(
      {
        id: f('ID!', (r: Repo) => globalId('Repository', r.id)),
        databaseId: f('Int', (r: Repo) => r.id),
        name: f('String!'),
        nameWithOwner: f('String!', (r: Repo) => `${r.owner}/${r.name}`),
        owner: f('RepositoryOwner!', (r: Repo) => userOf(r.owner)),
        description: f('String'),
        isPrivate: f('Boolean!', (r: Repo) => r.private),
        isArchived: f('Boolean!', (r: Repo) => r.archived),
        isFork: f('Boolean!', () => false),
        url: f('URI!', repoUrl),
        createdAt: f('DateTime!', dateField('createdAt')),
        updatedAt: f('DateTime!', dateField('updatedAt')),
        pushedAt: f('DateTime', (r: Repo) => (r.pushedAt === 0 ? null : new Date(r.pushedAt * 1000).toISOString())),
        viewerPermission: f('RepositoryPermission', () => 'ADMIN'),
        hasIssuesEnabled: f('Boolean!', (r: Repo) => r.hasIssues),
        hasPullRequestsEnabled: f('Boolean!', () => true),
        stargazerCount: f('Int!', () => 0),
        defaultBranchRef: f('Ref', (r: Repo) => refOf(r, r.defaultBranch)),
        ref: f('Ref', (r: Repo, a) => refOf(r, String(a.qualifiedName ?? '').replace(/^refs\/heads\//, '')), { qualifiedName: 'String!' }),
        issue: f('Issue', (r: Repo, a) => r.issues.get(a.number) ?? null, { number: 'Int!' }),
        issues: f(
          'IssueConnection!',
          (r: Repo, a) => {
            const states: string[] | null = Array.isArray(a.states) ? a.states : Array.isArray(a.filterBy?.states) ? a.filterBy.states : null;
            const labels: string[] = Array.isArray(a.labels) ? a.labels.map((name: string) => name.toLowerCase()) : [];
            const items = r.issueList.filter((issue) => (states === null || states.includes(issueState(issue))) && labels.every((name) => issue.labels.some((label) => label.name.toLowerCase() === name)));
            return makeConnection(orderItems(items, a.orderBy), a);
          },
          { ...PAGE, states: '[IssueState!]', orderBy: 'IssueOrder', labels: '[String!]', filterBy: 'IssueFilters' },
        ),
        pullRequest: f('PullRequest', (r: Repo, a) => r.pulls.get(a.number) ?? null, { number: 'Int!' }),
        pullRequests: f(
          'PullRequestConnection!',
          (r: Repo, a) => {
            const states: string[] | null = Array.isArray(a.states) ? a.states : null;
            const items = r.pullList.filter(
              (pull) => (states === null || states.includes(pullState(pull))) && (a.baseRefName == null || pull.baseRef === a.baseRefName) && (a.headRefName == null || pull.headRef === a.headRefName),
            );
            return makeConnection(orderItems(items, a.orderBy), a);
          },
          { ...PAGE, states: '[PullRequestState!]', baseRefName: 'String', headRefName: 'String', orderBy: 'IssueOrder' },
        ),
        labels: f('LabelConnection', (r: Repo, a) => makeConnection([...r.labels.values()], a), { ...PAGE, query: 'String' }),
      },
      ['Node'],
    ),
    SearchResultItemConnection: obj({
      issueCount: f('Int!'),
      repositoryCount: f('Int!'),
      userCount: f('Int!'),
      discussionCount: f('Int!', () => 0),
      codeCount: f('Int!', () => 0),
      nodes: f('[SearchResultItem]'),
      edges: f('[SearchResultItemEdge]'),
      pageInfo: f('PageInfo!'),
    }),
    SearchResultItemEdge: obj({ cursor: f('String!'), node: f('SearchResultItem') }),
    ...connectionTypes('Issue', 'Issue'),
    ...connectionTypes('PullRequest', 'PullRequest'),
    ...connectionTypes('Label', 'Label'),
    ...connectionTypes('IssueComment', 'IssueComment'),
    ...connectionTypes('PullRequestChangedFile', 'PullRequestChangedFile'),
    ...connectionTypes('PullRequestCommit', 'PullRequestCommit'),
    ...connectionTypes('User', 'User'),
    ...connectionTypes('Repository', 'Repository'),

    CreateIssuePayload: obj({ issue: f('Issue'), clientMutationId: f('String') }),
    UpdateIssuePayload: obj({ issue: f('Issue'), clientMutationId: f('String') }),
    CloseIssuePayload: obj({ issue: f('Issue'), clientMutationId: f('String') }),
    ReopenIssuePayload: obj({ issue: f('Issue'), clientMutationId: f('String') }),
    CreatePullRequestPayload: obj({ pullRequest: f('PullRequest'), clientMutationId: f('String') }),
    MergePullRequestPayload: obj({ pullRequest: f('PullRequest'), clientMutationId: f('String') }),
    ClosePullRequestPayload: obj({ pullRequest: f('PullRequest'), clientMutationId: f('String') }),
    ReopenPullRequestPayload: obj({ pullRequest: f('PullRequest'), clientMutationId: f('String') }),
    CreateRepositoryPayload: obj({ repository: f('Repository'), clientMutationId: f('String') }),
    AddCommentPayload: obj({ commentEdge: f('IssueCommentEdge'), subject: f('Node'), timelineEdge: f('IssueCommentEdge'), clientMutationId: f('String') }),

    Query: obj({
      viewer: f('User!', () => userOf(viewer.login)),
      user: f('User', (_s, a) => (String(a.login).toLowerCase() === viewer.login.toLowerCase() ? userOf(viewer.login) : null), { login: 'String!' }),
      repository: f(
        'Repository',
        (_s, a) => (String(a.owner).toLowerCase() === viewer.login.toLowerCase() ? (getRepo(viewer.login, String(a.name)) ?? null) : null),
        { owner: 'String', name: 'String', followRenames: 'Boolean' },
      ),
      repositoryOwner: f('RepositoryOwner', (_s, a) => (String(a.login).toLowerCase() === viewer.login.toLowerCase() ? userOf(viewer.login) : null), { login: 'String!' }),
      node: f('Node', (_s, a) => nodeById(a.id), { id: 'ID!' }),
      nodes: f('[Node]!', (_s, a) => (Array.isArray(a.ids) ? a.ids.map(nodeById) : []), { ids: '[ID!]!' }),
      search: f(
        'SearchResultItemConnection!',
        (_s, a) => {
          const items = searchItems(String(a.query ?? ''), String(a.type));
          const connection = makeConnection(items, a);
          const type = String(a.type);
          return { ...connection, issueCount: type === 'ISSUE' || type === 'ISSUE_ADVANCED' ? items.length : 0, repositoryCount: type === 'REPOSITORY' ? items.length : 0, userCount: type === 'USER' ? items.length : 0 };
        },
        { ...PAGE, query: 'String!', type: 'SearchType!' },
      ),
      rateLimit: f('RateLimit', () => ({}), { dryRun: 'Boolean' }),
      __schema: f('__Schema!'),
      __type: f('__Type', undefined, { name: 'String!' }),
    }),
    Mutation: obj({
      createIssue: f(
        'CreateIssuePayload',
        (_s, a) => {
          const repo = repoFromId(a.input.repositoryId);
          const labels = Array.isArray(a.input.labelIds) ? a.input.labelIds.map((id: string) => nodeById(id)?.name).filter((name: unknown) => typeof name === 'string') : undefined;
          return { issue: wrapApi(() => createIssue(repo, { title: a.input.title, body: a.input.body, labels })), clientMutationId: clientId(a) };
        },
        { input: 'CreateIssueInput!' },
      ),
      updateIssue: f(
        'UpdateIssuePayload',
        (_s, a) => {
          const issue = issueFromId(a.input.id);
          const patch: Record<string, unknown> = {};
          if (a.input.title !== undefined) patch.title = a.input.title;
          if (a.input.body !== undefined) patch.body = a.input.body;
          if (a.input.state !== undefined) patch.state = String(a.input.state).toLowerCase();
          wrapApi(() => updateIssue(issue, patch));
          return { issue, clientMutationId: clientId(a) };
        },
        { input: 'UpdateIssueInput!' },
      ),
      closeIssue: f(
        'CloseIssuePayload',
        (_s, a) => {
          const issue = issueFromId(a.input.issueId);
          setIssueState(issue, 'closed', a.input.stateReason === 'NOT_PLANNED' ? 'not_planned' : null);
          return { issue, clientMutationId: clientId(a) };
        },
        { input: 'CloseIssueInput!' },
      ),
      reopenIssue: f(
        'ReopenIssuePayload',
        (_s, a) => {
          const issue = issueFromId(a.input.issueId);
          setIssueState(issue, 'open');
          return { issue, clientMutationId: clientId(a) };
        },
        { input: 'ReopenIssueInput!' },
      ),
      createPullRequest: f(
        'CreatePullRequestPayload',
        (_s, a) => {
          const repo = repoFromId(a.input.repositoryId);
          const pullRequest = wrapApi(() => createPull(repo, { title: a.input.title, body: a.input.body, head: a.input.headRefName, base: a.input.baseRefName, draft: a.input.draft }));
          return { pullRequest, clientMutationId: clientId(a) };
        },
        { input: 'CreatePullRequestInput!' },
      ),
      mergePullRequest: f(
        'MergePullRequestPayload',
        (_s, a) => {
          const pull = pullFromId(a.input.pullRequestId);
          const method = a.input.mergeMethod === 'MERGE' ? 'merge' : a.input.mergeMethod === 'REBASE' ? 'rebase' : 'squash';
          wrapApi(() => mergePull(pull, { method, title: a.input.commitHeadline, message: a.input.commitBody, sha: a.input.expectedHeadOid }));
          return { pullRequest: pull, clientMutationId: clientId(a) };
        },
        { input: 'MergePullRequestInput!' },
      ),
      closePullRequest: f(
        'ClosePullRequestPayload',
        (_s, a) => {
          const pull = pullFromId(a.input.pullRequestId);
          closePull(pull);
          return { pullRequest: pull, clientMutationId: clientId(a) };
        },
        { input: 'ClosePullRequestInput!' },
      ),
      reopenPullRequest: f(
        'ReopenPullRequestPayload',
        (_s, a) => {
          const pull = pullFromId(a.input.pullRequestId);
          reopenPull(pull);
          return { pullRequest: pull, clientMutationId: clientId(a) };
        },
        { input: 'ReopenPullRequestInput!' },
      ),
      createRepository: f(
        'CreateRepositoryPayload',
        (_s, a) => ({ repository: wrapApi(() => createRepo({ name: a.input.name, description: a.input.description, private: a.input.visibility !== 'PUBLIC' })), clientMutationId: clientId(a) }),
        { input: 'CreateRepositoryInput!' },
      ),
      addComment: f(
        'AddCommentPayload',
        (_s, a) => {
          const subject = nodeById(a.input.subjectId);
          if (subject === null || (subject.kind !== 'issue' && subject.kind !== 'pull')) throw new GqlFailure(`Could not resolve to a node with the global id of '${String(a.input.subjectId)}'.`);
          const comment = wrapApi(() => addComment(subject as Issue | Pull, a.input.body));
          const edge = { cursor: encodeCursor(subject.comments.length - 1), node: comment };
          return { commentEdge: edge, subject, timelineEdge: edge, clientMutationId: clientId(a) };
        },
        { input: 'AddCommentInput!' },
      ),
    }),
  };
  for (const name of ['__Schema', '__Type', '__Field', '__InputValue', '__EnumValue', '__Directive']) schema[name] = { kind: 'OBJECT', dynamic: true };
  return schema;
}

const schema = buildSchema();

// ---------------------------------------------------------------- type references and introspection

interface Ref {
  nonNull: boolean;
  list: boolean;
  inner: Ref | null;
  name: string;
}

const refCache = new Map<string, Ref>();

function parseRef(text: string): Ref {
  let ref = refCache.get(text);
  if (ref === undefined) {
    if (text.endsWith('!')) ref = { ...parseRef(text.slice(0, -1)), nonNull: true };
    else if (text.startsWith('[')) ref = { nonNull: false, list: true, inner: parseRef(text.slice(1, -1)), name: '' };
    else ref = { nonNull: false, list: false, inner: null, name: text };
    refCache.set(text, ref);
  }
  return ref;
}

function typeRefObject(text: string): Json {
  if (text.endsWith('!')) return { kind: 'NON_NULL', name: null, ofType: typeRefObject(text.slice(0, -1)) };
  if (text.startsWith('[')) return { kind: 'LIST', name: null, ofType: typeRefObject(text.slice(1, -1)) };
  return typeObject(text);
}

const typeObjects = new Map<string, Json>();

function inputValueObject(name: string, type: string): Json {
  return { name, description: null, type: typeRefObject(type), defaultValue: null, isDeprecated: false, deprecationReason: null };
}

function typeObject(name: string): Json {
  let cached = typeObjects.get(name);
  if (cached !== undefined) return cached;
  const def = schema[name] as TypeDef;
  const named = (list: string[] | undefined): Json[] => (list ?? []).map(typeObject);
  cached = {
    __typename: '__Type',
    kind: def.kind,
    name,
    description: null,
    specifiedByURL: null,
    isOneOf: false,
    ofType: null,
    fields: () =>
      def.kind === 'OBJECT' || def.kind === 'INTERFACE'
        ? Object.entries(def.fields ?? {}).map(([fieldName, field]) => ({
            __typename: '__Field',
            name: fieldName,
            description: null,
            args: Object.entries(field.args ?? {}).map(([argName, argType]) => inputValueObject(argName, argType)),
            type: typeRefObject(field.type),
            isDeprecated: false,
            deprecationReason: null,
          }))
        : null,
    interfaces: () => (def.kind === 'OBJECT' ? named(def.interfaces) : null),
    possibleTypes: () => (def.kind === 'INTERFACE' || def.kind === 'UNION' ? named(def.possible) : null),
    enumValues: () => (def.kind === 'ENUM' ? (def.values ?? []).map((value) => ({ __typename: '__EnumValue', name: value, description: null, isDeprecated: false, deprecationReason: null })) : null),
    inputFields: () => (def.kind === 'INPUT_OBJECT' ? Object.entries(def.inputFields ?? {}).map(([fieldName, type]) => inputValueObject(fieldName, type)) : null),
  };
  typeObjects.set(name, cached);
  return cached;
}

function schemaObject(): Json {
  const directive = (name: string, locations: string[]): Json => ({
    __typename: '__Directive',
    name,
    description: null,
    locations,
    args: [inputValueObject('if', 'Boolean!')],
    isRepeatable: false,
  });
  return {
    __typename: '__Schema',
    description: null,
    queryType: typeObject('Query'),
    mutationType: typeObject('Mutation'),
    subscriptionType: null,
    types: Object.keys(schema).filter((name) => !name.startsWith('__')).map(typeObject),
    directives: [directive('include', ['FIELD', 'FRAGMENT_SPREAD', 'INLINE_FRAGMENT']), directive('skip', ['FIELD', 'FRAGMENT_SPREAD', 'INLINE_FRAGMENT'])],
  };
}

// ---------------------------------------------------------------- validation

interface GqlError {
  message: string;
  locations?: Array<{ line: number; column: number }>;
  path?: Array<string | number>;
  extensions?: Record<string, unknown>;
}

function namedType(ref: string): string {
  return ref.replace(/[[\]!]/g, '');
}

function typeApplies(cond: string, concrete: string): boolean {
  if (cond === concrete) return true;
  const def = schema[cond];
  if (def === undefined) return false;
  if (def.kind === 'INTERFACE' || def.kind === 'UNION') return def.possible?.includes(concrete) ?? false;
  return false;
}

function validate(doc: Document, op: Operation): GqlError[] {
  const errors: GqlError[] = [];
  const opLabel = `${op.type}${op.name === null ? '' : ` ${op.name}`}`;
  const defined = new Set(op.vars.map((v) => v.name));
  const usedFragments = new Set<string>();
  const checkValue = (value: ValueNode, at: { line: number; col: number }): void => {
    if (value.k === 'var') {
      if (!defined.has(value.name)) {
        errors.push({ message: `Variable $${value.name} is used by ${op.name ?? 'anonymous operation'} but not declared`, locations: [{ line: at.line, column: at.col }], extensions: { code: 'variableNotDefined', variableName: value.name } });
      }
    } else if (value.k === 'list') for (const item of value.items) checkValue(item, at);
    else if (value.k === 'obj') for (const item of Object.values(value.fields)) checkValue(item, at);
  };
  const walk = (typeName: string, selections: Selection[], path: string[]): void => {
    const def = schema[typeName];
    if (def === undefined || def.dynamic === true) return;
    for (const sel of selections) {
      if (sel.kind === 'inline') {
        const cond = sel.typeCond ?? typeName;
        if (schema[cond] === undefined) {
          errors.push({ message: `No such type ${cond}, so it can't be a fragment condition`, extensions: { code: 'undefinedType', typeName: cond } });
          continue;
        }
        walk(cond, sel.selection, path);
        continue;
      }
      if (sel.kind === 'spread') {
        const fragment = doc.fragments[sel.name];
        if (fragment === undefined) {
          errors.push({ message: `Fragment ${sel.name} was used, but not defined`, locations: [{ line: sel.line, column: sel.col }], path: [...path], extensions: { code: 'undefinedFragment', fragmentName: sel.name } });
          continue;
        }
        if (!usedFragments.has(sel.name)) {
          usedFragments.add(sel.name);
          if (schema[fragment.typeCond] === undefined) errors.push({ message: `No such type ${fragment.typeCond}, so it can't be a fragment condition`, extensions: { code: 'undefinedType', typeName: fragment.typeCond } });
          else walk(fragment.typeCond, fragment.selection, path);
        }
        continue;
      }
      for (const value of Object.values(sel.args)) checkValue(value, sel);
      if (sel.name === '__typename') continue;
      const field = def.fields?.[sel.name];
      if (field === undefined) {
        errors.push({
          message: `Field '${sel.name}' doesn't exist on type '${typeName}'`,
          locations: [{ line: sel.line, column: sel.col }],
          path: [...path, sel.name],
          extensions: { code: 'undefinedField', typeName, fieldName: sel.name },
        });
        continue;
      }
      for (const argName of Object.keys(sel.args)) {
        if (field.args === undefined || !(argName in field.args)) {
          errors.push({
            message: `Field '${sel.name}' doesn't accept argument '${argName}'`,
            locations: [{ line: sel.line, column: sel.col }],
            path: [...path, sel.name, argName],
            extensions: { code: 'argumentNotAccepted', name: sel.name, typeName, argumentName: argName },
          });
        }
      }
      const inner = namedType(field.type);
      const innerDef = schema[inner];
      if (innerDef === undefined) continue;
      const composite = innerDef.kind === 'OBJECT' || innerDef.kind === 'INTERFACE' || innerDef.kind === 'UNION';
      if (composite && sel.selection === null) {
        errors.push({ message: `Field must have selections (field '${sel.name}' returns ${field.type} but has no selections. Did you mean '${sel.name} { ... }'?)`, locations: [{ line: sel.line, column: sel.col }], path: [...path, sel.name], extensions: { code: 'selectionMismatch', nodeName: sel.name, typeName: field.type } });
      } else if (!composite && sel.selection !== null) {
        errors.push({ message: `Selections can't be made on scalars (field '${sel.name}' returns ${inner} but has selections [${sel.selection.map((s) => (s.kind === 'field' ? `"${s.name}"` : '...')).join(', ')}])`, locations: [{ line: sel.line, column: sel.col }], path: [...path, sel.name], extensions: { code: 'selectionMismatch', nodeName: sel.name, typeName: inner } });
      } else if (sel.selection !== null) {
        walk(inner, sel.selection, [...path, sel.name]);
      }
    }
  };
  walk(op.type === 'mutation' ? 'Mutation' : 'Query', op.selection, [opLabel]);
  return errors;
}

// ---------------------------------------------------------------- execution

class Propagate extends Error {}

interface Ctx {
  vars: Record<string, Json>;
  fragments: Record<string, Fragment>;
  errors: GqlError[];
}

function evalValue(node: ValueNode, vars: Record<string, Json>): Json {
  switch (node.k) {
    case 'var':
      return vars[node.name];
    case 'null':
      return null;
    case 'list':
      return node.items.map((item) => evalValue(item, vars));
    case 'obj': {
      const out: Record<string, Json> = {};
      for (const [key, value] of Object.entries(node.fields)) {
        const evaluated = evalValue(value, vars);
        if (evaluated !== undefined) out[key] = evaluated;
      }
      return out;
    }
    default:
      return node.v;
  }
}

function evalArgs(args: Record<string, ValueNode>, vars: Record<string, Json>): Record<string, Json> {
  const out: Record<string, Json> = {};
  for (const [key, value] of Object.entries(args)) {
    const evaluated = evalValue(value, vars);
    if (evaluated !== undefined) out[key] = evaluated;
  }
  return out;
}

function allowed(directives: Directive[], vars: Record<string, Json>): boolean {
  for (const directive of directives) {
    const arg = directive.args.if === undefined ? undefined : evalValue(directive.args.if, vars);
    if (directive.name === 'skip' && arg === true) return false;
    if (directive.name === 'include' && arg === false) return false;
  }
  return true;
}

function collect(typeName: string, selections: Selection[], ctx: Ctx, into: Map<string, FieldNode[]>, seen: Set<string>): void {
  for (const sel of selections) {
    if (!allowed(sel.directives, ctx.vars)) continue;
    if (sel.kind === 'field') {
      const key = sel.alias ?? sel.name;
      const list = into.get(key);
      if (list === undefined) into.set(key, [sel]);
      else list.push(sel);
    } else if (sel.kind === 'inline') {
      if (sel.typeCond === null || typeApplies(sel.typeCond, typeName)) collect(typeName, sel.selection, ctx, into, seen);
    } else if (!seen.has(sel.name)) {
      seen.add(sel.name);
      const fragment = ctx.fragments[sel.name];
      if (fragment !== undefined && typeApplies(fragment.typeCond, typeName)) collect(typeName, fragment.selection, ctx, into, seen);
    }
  }
}

function record(ctx: Ctx, err: unknown, node: FieldNode, path: Array<string | number>): void {
  const message = err instanceof GqlFailure ? err.message : err instanceof Error ? `Internal error: ${err.message}` : 'Internal error';
  ctx.errors.push({ message, locations: [{ line: node.line, column: node.col }], path: [...path] });
}

function execObject(typeName: string, src: Json, selections: Selection[], ctx: Ctx, path: Array<string | number>): Record<string, Json> {
  const def = schema[typeName] as TypeDef;
  const fields = new Map<string, FieldNode[]>();
  collect(typeName, selections, ctx, fields, new Set());
  const out: Record<string, Json> = {};
  for (const [key, nodes] of fields) {
    const node = nodes[0] as FieldNode;
    if (node.name === '__typename') {
      out[key] = typeName;
      continue;
    }
    const field = def.fields?.[node.name];
    if (field === undefined) {
      out[key] = null;
      continue;
    }
    const fieldPath = [...path, key];
    const nonNull = field.type.endsWith('!');
    try {
      const args = evalArgs(node.args, ctx.vars);
      let value: Json;
      if (typeName === 'Query' && node.name === '__schema') value = schemaObject();
      else if (typeName === 'Query' && node.name === '__type') value = typeof args.name === 'string' && schema[args.name] !== undefined && !args.name.startsWith('__') ? typeObject(args.name) : null;
      else value = field.resolve !== undefined ? field.resolve(src, args) : src?.[node.name];
      const merged: Selection[] = nodes.flatMap((n) => n.selection ?? []);
      out[key] = complete(field.type, value, merged, ctx, fieldPath, node);
    } catch (err) {
      if (!(err instanceof Propagate)) record(ctx, err, node, fieldPath);
      if (nonNull) throw new Propagate();
      out[key] = null;
    }
  }
  return out;
}

function complete(typeText: string, value: Json, selections: Selection[], ctx: Ctx, path: Array<string | number>, node: FieldNode): Json {
  const ref = parseRef(typeText);
  if (value === null || value === undefined) {
    if (ref.nonNull) {
      ctx.errors.push({ message: `Cannot return null for non-nullable field ${path.join('.')}`, locations: [{ line: node.line, column: node.col }], path: [...path] });
      throw new Propagate();
    }
    return null;
  }
  if (ref.list) {
    if (!Array.isArray(value)) throw new GqlFailure(`Expected an array for ${path.join('.')}`);
    const itemType = (ref.inner as Ref).nonNull ? `${typeTextOf(ref.inner as Ref)}!` : typeTextOf(ref.inner as Ref);
    return value.map((item, index) => {
      try {
        return complete(itemType, item, selections, ctx, [...path, index], node);
      } catch (err) {
        if (!(err instanceof Propagate) || (ref.inner as Ref).nonNull) throw err;
        return null;
      }
    });
  }
  const def = schema[ref.name];
  if (def === undefined) return value;
  if (def.kind === 'SCALAR' || def.kind === 'ENUM') return value;
  if (def.dynamic === true) return completeDynamic(value, selections, ctx, path);
  let concrete = ref.name;
  if (def.kind === 'INTERFACE' || def.kind === 'UNION') concrete = resolveTypeName(value);
  return execObject(concrete, value, selections, ctx, path);
}

function typeTextOf(ref: Ref): string {
  if (ref.list) return `[${typeTextOf(ref.inner as Ref)}${(ref.inner as Ref).nonNull ? '!' : ''}]`;
  return ref.name;
}

function completeDynamic(value: Json, selections: Selection[], ctx: Ctx, path: Array<string | number>): Json {
  if (value === null || value === undefined) return null;
  if (Array.isArray(value)) return value.map((item, index) => completeDynamic(item, selections, ctx, [...path, index]));
  if (typeof value !== 'object') return value;
  const fields = new Map<string, FieldNode[]>();
  collectDynamic(selections, ctx, fields);
  const out: Record<string, Json> = {};
  for (const [key, nodes] of fields) {
    const node = nodes[0] as FieldNode;
    if (node.name === '__typename') {
      out[key] = value.__typename ?? '__Type';
      continue;
    }
    let item = value[node.name];
    if (typeof item === 'function') item = item(evalArgs(node.args, ctx.vars));
    out[key] = node.selection === null ? (item === undefined ? null : item) : completeDynamic(item, nodes.flatMap((n) => n.selection ?? []), ctx, [...path, key]);
  }
  return out;
}

function collectDynamic(selections: Selection[], ctx: Ctx, into: Map<string, FieldNode[]>): void {
  for (const sel of selections) {
    if (!allowed(sel.directives, ctx.vars)) continue;
    if (sel.kind === 'field') {
      const key = sel.alias ?? sel.name;
      const list = into.get(key);
      if (list === undefined) into.set(key, [sel]);
      else list.push(sel);
    } else if (sel.kind === 'inline') collectDynamic(sel.selection, ctx, into);
    else {
      const fragment = ctx.fragments[sel.name];
      if (fragment !== undefined) collectDynamic(fragment.selection, ctx, into);
    }
  }
}

function coerceVariables(op: Operation, provided: Record<string, Json>): { vars: Record<string, Json>; errors: GqlError[] } {
  const vars: Record<string, Json> = {};
  const errors: GqlError[] = [];
  for (const def of op.vars) {
    if (Object.prototype.hasOwnProperty.call(provided, def.name)) vars[def.name] = provided[def.name];
    else if (def.def !== null) vars[def.name] = evalValue(def.def, {});
    else if (def.type.endsWith('!')) {
      errors.push({ message: `Variable $${def.name} of type ${def.type} was provided invalid value`, extensions: { value: null, problems: [{ path: [], explanation: 'Expected value to not be null' }] } });
      continue;
    }
    if (vars[def.name] === null && def.type.endsWith('!')) {
      errors.push({ message: `Variable $${def.name} of type ${def.type} was provided invalid value`, extensions: { value: null, problems: [{ path: [], explanation: 'Expected value to not be null' }] } });
    }
  }
  return { vars, errors };
}

/** Run one GraphQL request ({query, variables, operationName}) and return its response body. */
export function runGraphql(request: Record<string, Json>): Record<string, Json> {
  const query = request.query;
  if (typeof query !== 'string' || query.trim() === '') return { errors: [{ message: 'A query attribute must be specified and must be a string.' }] };
  let variables: Record<string, Json> = {};
  if (request.variables !== undefined && request.variables !== null && request.variables !== '') {
    let provided = request.variables;
    if (typeof provided === 'string') {
      try {
        provided = JSON.parse(provided);
      } catch {
        return { errors: [{ message: 'Variables must be provided as a JSON object.' }] };
      }
    }
    if (typeof provided !== 'object' || Array.isArray(provided)) return { errors: [{ message: 'Variables must be provided as a JSON object.' }] };
    variables = provided as Record<string, Json>;
  }
  let doc: Document;
  try {
    doc = new Parser(lex(query)).document();
  } catch (err) {
    if (err instanceof GqlSyntaxError) return { errors: [{ message: err.message, locations: [{ line: err.line, column: err.col }] }] };
    throw err;
  }
  if (doc.operations.length === 0) return { errors: [{ message: 'No operations in query document.' }] };
  let op: Operation | undefined;
  const wanted = request.operationName;
  if (typeof wanted === 'string' && wanted !== '') {
    op = doc.operations.find((candidate) => candidate.name === wanted);
    if (op === undefined) return { errors: [{ message: `Unknown operation named "${wanted}".` }] };
  } else if (doc.operations.length === 1) op = doc.operations[0];
  else return { errors: [{ message: 'An operation name is required when the document has more than one operation.' }] };
  const operation = op as Operation;
  const invalid = validate(doc, operation);
  if (invalid.length > 0) return { errors: invalid };
  const coerced = coerceVariables(operation, variables);
  if (coerced.errors.length > 0) return { errors: coerced.errors };
  if (operation.type === 'subscription') return { errors: [{ message: 'Subscriptions are not supported.' }] };
  const ctx: Ctx = { vars: coerced.vars, fragments: doc.fragments, errors: [] };
  let data: Json;
  try {
    data = execObject(operation.type === 'mutation' ? 'Mutation' : 'Query', {}, operation.selection, ctx, []);
  } catch (err) {
    if (!(err instanceof Propagate)) throw err;
    data = null;
  }
  return ctx.errors.length > 0 ? { data, errors: ctx.errors } : { data };
}

/** POST /api/graphql: a single request object, or an array of them answered in order. */
export async function handleGraphql(req: IncomingMessage, res: ServerResponse): Promise<void> {
  const raw = await readBody(req);
  let body: unknown;
  try {
    body = JSON.parse(raw.toString('utf8'));
  } catch {
    throw new HttpError(400, 'Problems parsing JSON');
  }
  if (Array.isArray(body)) {
    sendJson(res, 200, body.map((entry) => (typeof entry === 'object' && entry !== null ? runGraphql(entry as Record<string, Json>) : { errors: [{ message: 'Each batched operation must be a JSON object.' }] })));
    return;
  }
  if (typeof body !== 'object' || body === null) throw new HttpError(400, 'Problems parsing JSON');
  sendJson(res, 200, runGraphql(body as Record<string, Json>));
}
