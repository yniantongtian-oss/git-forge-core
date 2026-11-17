// The one in-memory data store every surface shares: REST, GraphQL, MCP and git smart HTTP all read and
// write these entities, so an object created over one is the same object over the others. Ids and numbers
// are sequential and every timestamp comes from the logical clock, so the state is a pure function of the
// request sequence.

import { createHash } from 'node:crypto';
import { isOid, isRefNameValid, treeOf } from './objects.ts';
import type { GitObject, Ident } from './objects.ts';
import { iso, nextIso, nextTime } from './clock.ts';
import {
  ancestorsOf,
  commitInfo,
  createCommit,
  deleteRef,
  diffTrees,
  ensureEmptyTree,
  getObject,
  headRefName,
  listCommits,
  lookupPath,
  mergeBase,
  mergeTrees,
  resolveCommit,
  setRef,
  TreeEditError,
  updateTree,
  writeObject,
} from './gitops.ts';
import type { FileDiff } from './gitops.ts';

// ---------------------------------------------------------------- errors

/** A failure a surface turns into its own error shape: REST status + message, a GraphQL error, an MCP tool error. */
export class ApiError extends Error {
  status: number;
  errors: unknown[] | undefined;

  constructor(status: number, message: string, errors?: unknown[]) {
    super(message);
    this.status = status;
    this.errors = errors;
  }
}

// ---------------------------------------------------------------- the viewer

const FIRST_NAMES = [
  'ada', 'bram', 'cora', 'dmitri', 'elena', 'felix', 'greta', 'hugo', 'iris', 'jonas', 'kira', 'lars',
  'mira', 'nico', 'olga', 'pavel', 'quinn', 'rosa', 'sven', 'tess', 'ulla', 'vito', 'wren', 'yara', 'zane',
];
const LAST_NAMES = [
  'anders', 'bauer', 'costa', 'dahl', 'eriksen', 'fischer', 'garcia', 'holm', 'iversen', 'jansen', 'keller',
  'lind', 'moreau', 'novak', 'olsen', 'peters', 'quist', 'rossi', 'sato', 'turi', 'ulrich', 'vega', 'weber',
  'xu', 'yilmaz', 'zimmer',
];

const U64 = (1n << 64n) - 1n;

/**
 * The login the suite's token names: the first user of the suite's fixed scenario. The scenario seed is a
 * published constant of the contest, so the login is derived from it the same way the suite derives it.
 */
function scenarioViewerLogin(): string {
  const seed = 0x6a09e667f3bcc909n;
  let hash = 0xcbf29ce484222325n;
  for (const ch of 'users') {
    hash ^= BigInt(ch.charCodeAt(0));
    hash = (hash * 0x100000001b3n) & U64;
  }
  let z = ((seed ^ hash) + 0x9e3779b97f4a7c15n) & U64;
  z = ((z ^ (z >> 30n)) * 0xbf58476d1ce4e5b9n) & U64;
  z = ((z ^ (z >> 27n)) * 0x94d049bb133111ebn) & U64;
  let state = (z ^ (z >> 31n)) & U64;
  if (state === 0n) state = seed;
  const nextInt = (max: number): number => {
    const limit = 4_294_967_296 - (4_294_967_296 % max);
    for (;;) {
      let x = state;
      x = (x ^ (x >> 12n)) & U64;
      x = (x ^ (x << 25n)) & U64;
      x = (x ^ (x >> 27n)) & U64;
      state = x;
      const value = Number(((x * 0x2545f4914f6cdd1dn) & U64) >> 32n);
      if (value < limit) return value % max;
    }
  };
  const first = FIRST_NAMES[nextInt(FIRST_NAMES.length)] as string;
  const last = LAST_NAMES[nextInt(LAST_NAMES.length)] as string;
  return `${first}${last}`.replace(/[^a-z0-9-]/g, '');
}

export interface User {
  id: number;
  login: string;
}

export const viewer: User = { id: 1000, login: process.env.FORGE_VIEWER_LOGIN ?? scenarioViewerLogin() };

export function identFor(login: string, when?: number): Ident {
  return { name: login, email: `${login}@users.noreply.github.com`, when: when ?? nextTime(), tz: '+0000' };
}

// ---------------------------------------------------------------- entities

export interface Label {
  id: number;
  name: string;
  color: string;
  description: string | null;
}

export interface Comment {
  id: number;
  body: string;
  user: string;
  createdAt: string;
  updatedAt: string;
  parent: Issue | Pull;
}

export interface Issue {
  kind: 'issue';
  id: number;
  number: number;
  repo: Repo;
  title: string;
  body: string | null;
  state: 'open' | 'closed';
  stateReason: string | null;
  labels: Label[];
  comments: Comment[];
  user: string;
  createdAt: string;
  updatedAt: string;
  closedAt: string | null;
  seq: number;
  /** The serialized REST representation, rebuilt only after the issue changes. */
  json: Buffer | null;
}

export interface Pull {
  kind: 'pull';
  id: number;
  number: number;
  repo: Repo;
  title: string;
  body: string | null;
  state: 'open' | 'closed';
  draft: boolean;
  headRef: string;
  baseRef: string;
  /** Frozen when the pull request is closed or merged; live tips are used while it is open. */
  frozenHead: string | null;
  frozenBase: string | null;
  merged: boolean;
  mergedAt: string | null;
  mergeCommitSha: string | null;
  comments: Comment[];
  labels: Label[];
  user: string;
  createdAt: string;
  updatedAt: string;
  closedAt: string | null;
  seq: number;
}

export interface Repo {
  kind: 'repo';
  id: number;
  owner: string;
  name: string;
  description: string | null;
  private: boolean;
  defaultBranch: string;
  archived: boolean;
  hasIssues: boolean;
  createdAt: string;
  updatedAt: string;
  pushedAt: number;
  objects: Map<string, GitObject>;
  refs: Map<string, string>;
  refsVersion: number;
  labels: Map<string, Label>;
  issues: Map<number, Issue>;
  issueList: Issue[];
  pulls: Map<number, Pull>;
  pullList: Pull[];
  nextNumber: number;
  /** Git protocol responses (advertisements, ls-refs) valid for one refsVersion. */
  gitCache: Map<string, Buffer>;
  gitCacheVersion: number;
}

// ---------------------------------------------------------------- registries and counters

const reposByKey = new Map<string, Repo>();
const reposById = new Map<number, Repo>();
export const issuesById = new Map<number, Issue>();
export const pullsById = new Map<number, Pull>();
export const commentsById = new Map<number, Comment>();

let nextRepoId = 1_000_001;
let nextEntityId = 2_000_001;
let nextSeq = 1;

function entityId(): number {
  return nextEntityId++;
}

function repoKey(owner: string, name: string): string {
  return `${owner.toLowerCase()}/${name.toLowerCase()}`;
}

export function getRepo(owner: string, name: string): Repo | undefined {
  return reposByKey.get(repoKey(owner, name));
}

export function getRepoById(id: number): Repo | undefined {
  return reposById.get(id);
}

export function allRepos(): Repo[] {
  return [...reposById.values()];
}

/** The viewer's repositories sorted by full name, as GitHub lists them. */
export function listRepos(): Repo[] {
  return allRepos().sort((a, b) => (a.name.toLowerCase() < b.name.toLowerCase() ? -1 : a.name.toLowerCase() > b.name.toLowerCase() ? 1 : 0));
}

// ---------------------------------------------------------------- repositories

const REPO_NAME_RE = /^[A-Za-z0-9._-]{1,100}$/;

export function createRepo(input: { name: unknown; description?: unknown; private?: unknown; autoInit?: unknown; defaultBranch?: unknown }): Repo {
  if (typeof input.name !== 'string' || input.name.trim() === '') {
    throw new ApiError(422, 'Repository creation failed.', [{ resource: 'Repository', code: 'missing_field', field: 'name' }]);
  }
  let name = input.name.trim();
  if (name.endsWith('.git')) name = name.slice(0, -4);
  if (!REPO_NAME_RE.test(name) || name === '.' || name === '..') {
    throw new ApiError(422, 'Repository creation failed.', [{ resource: 'Repository', code: 'custom', field: 'name', message: 'name is invalid' }]);
  }
  if (reposByKey.has(repoKey(viewer.login, name))) {
    throw new ApiError(422, 'Repository creation failed.', [{ resource: 'Repository', code: 'custom', field: 'name', message: 'name already exists on this account' }]);
  }
  const now = nextIso();
  const defaultBranch = typeof input.defaultBranch === 'string' && input.defaultBranch !== '' ? input.defaultBranch : 'main';
  const repo: Repo = {
    kind: 'repo',
    id: nextRepoId++,
    owner: viewer.login,
    name,
    description: typeof input.description === 'string' ? input.description : null,
    private: input.private === true,
    defaultBranch,
    archived: false,
    hasIssues: true,
    createdAt: now,
    updatedAt: now,
    pushedAt: 0,
    objects: new Map(),
    refs: new Map(),
    refsVersion: 1,
    labels: new Map(),
    issues: new Map(),
    issueList: [],
    pulls: new Map(),
    pullList: [],
    nextNumber: 1,
    gitCache: new Map(),
    gitCacheVersion: 0,
  };
  reposByKey.set(repoKey(repo.owner, repo.name), repo);
  reposById.set(repo.id, repo);
  if (input.autoInit === true) {
    putContents(repo, 'README.md', { message: 'Initial commit', content: Buffer.from(`# ${name}\n`, 'utf8') });
  }
  return repo;
}

export function updateRepo(repo: Repo, patch: Record<string, unknown>): void {
  if (typeof patch.name === 'string' && patch.name !== repo.name) {
    const name = patch.name.trim();
    if (!REPO_NAME_RE.test(name)) throw new ApiError(422, 'Repository creation failed.', [{ resource: 'Repository', code: 'custom', field: 'name', message: 'name is invalid' }]);
    if (reposByKey.has(repoKey(repo.owner, name))) {
      throw new ApiError(422, 'Repository creation failed.', [{ resource: 'Repository', code: 'custom', field: 'name', message: 'name already exists on this account' }]);
    }
    reposByKey.delete(repoKey(repo.owner, repo.name));
    repo.name = name;
    reposByKey.set(repoKey(repo.owner, repo.name), repo);
  }
  if (typeof patch.description === 'string') repo.description = patch.description;
  else if (patch.description === null) repo.description = null;
  if (typeof patch.private === 'boolean') repo.private = patch.private;
  if (typeof patch.archived === 'boolean') repo.archived = patch.archived;
  if (typeof patch.has_issues === 'boolean') repo.hasIssues = patch.has_issues;
  if (typeof patch.default_branch === 'string' && patch.default_branch !== repo.defaultBranch) {
    if (!repo.refs.has(`refs/heads/${patch.default_branch}`)) throw new ApiError(422, 'Validation Failed', [{ resource: 'Repository', code: 'invalid', field: 'default_branch' }]);
    repo.defaultBranch = patch.default_branch;
    repo.refsVersion += 1;
  }
  repo.updatedAt = nextIso();
}

export function deleteRepo(repo: Repo): void {
  reposByKey.delete(repoKey(repo.owner, repo.name));
  reposById.delete(repo.id);
  for (const issue of repo.issueList) {
    issuesById.delete(issue.id);
    for (const comment of issue.comments) commentsById.delete(comment.id);
  }
  for (const pull of repo.pullList) {
    pullsById.delete(pull.id);
    for (const comment of pull.comments) commentsById.delete(comment.id);
  }
}

// ---------------------------------------------------------------- refs and branches

export function createRef(repo: Repo, ref: unknown, sha: unknown): void {
  if (typeof ref !== 'string' || !isRefNameValid(ref)) {
    throw new ApiError(422, 'Reference update failed', [{ resource: 'Reference', code: 'invalid', field: 'ref' }]);
  }
  if (typeof sha !== 'string' || !isOid(sha) || !repo.objects.has(sha)) throw new ApiError(422, 'Object does not exist');
  if (repo.refs.has(ref)) throw new ApiError(422, 'Reference already exists');
  setRef(repo, ref, sha);
}

export function updateRef(repo: Repo, ref: string, sha: unknown, force: boolean): void {
  if (!repo.refs.has(ref)) throw new ApiError(404, 'Not Found');
  if (typeof sha !== 'string' || !isOid(sha) || !repo.objects.has(sha)) throw new ApiError(422, 'Object does not exist');
  const current = repo.refs.get(ref) as string;
  if (!force && !isFastForward(repo, current, sha)) throw new ApiError(422, 'Update is not a fast forward');
  setRef(repo, ref, sha);
}

function isFastForward(repo: Repo, from: string, to: string): boolean {
  const info = commitInfo(repo, to);
  if (info !== null && info.parents.includes(from)) return true;
  return mergeBase(repo, from, to) === from;
}

export function removeRef(repo: Repo, ref: string): void {
  if (!deleteRef(repo, ref)) throw new ApiError(404, 'Not Found');
}

// ---------------------------------------------------------------- contents

export interface ContentsFile {
  type: 'file';
  path: string;
  oid: string;
  mode: string;
  data: Buffer;
}

export interface ContentsEntry {
  type: 'file' | 'dir' | 'symlink' | 'submodule';
  name: string;
  path: string;
  oid: string;
  size: number;
}

function cleanPath(path: string): string {
  return path.replace(/^\/+/, '').replace(/\/+$/, '');
}

export function getContents(repo: Repo, rawPath: string, ref: string | undefined): ContentsFile | ContentsEntry[] {
  const path = cleanPath(rawPath);
  const commitOid = resolveCommit(repo, ref === undefined || ref === '' ? headRefName(repo) : ref);
  if (commitOid === null) throw new ApiError(404, 'Not Found');
  const rootTree = (commitInfo(repo, commitOid) as { tree: string }).tree;
  const found = lookupPath(repo, rootTree, path);
  if (found === null) throw new ApiError(404, 'Not Found');
  if (found.mode === '40000') {
    const tree = getObject(repo, found.oid);
    if (tree === undefined) throw new ApiError(404, 'Not Found');
    return treeOf(tree).map((entry) => {
      const child = getObject(repo, entry.oid);
      const type = entry.mode === '40000' ? 'dir' : entry.mode === '120000' ? 'symlink' : entry.mode === '160000' ? 'submodule' : 'file';
      return { type, name: entry.name, path: path === '' ? entry.name : `${path}/${entry.name}`, oid: entry.oid, size: type === 'file' && child !== undefined ? child.data.length : 0 };
    });
  }
  const blob = getObject(repo, found.oid);
  if (blob === undefined) throw new ApiError(404, 'Not Found');
  return { type: 'file', path, oid: found.oid, mode: found.mode, data: blob.data };
}

export interface ContentsCommit {
  created: boolean;
  commit: string;
  blob: string | null;
  path: string;
  branch: string;
}

function contentsIdent(input: unknown): Ident {
  if (typeof input === 'object' && input !== null) {
    const record = input as Record<string, unknown>;
    if (typeof record.name === 'string' && typeof record.email === 'string') return { name: record.name, email: record.email, when: nextTime(), tz: '+0000' };
  }
  return identFor(viewer.login);
}

function branchTip(repo: Repo, branch: string): string | undefined {
  const own = repo.refs.get(`refs/heads/${branch}`);
  if (own !== undefined) return own;
  // A branch that does not exist yet starts from the default branch, when there is one.
  return repo.refs.get(headRefName(repo));
}

export function putContents(
  repo: Repo,
  rawPath: string,
  input: { message: unknown; content: Buffer; sha?: unknown; branch?: unknown; author?: unknown; committer?: unknown },
): ContentsCommit {
  const path = cleanPath(rawPath);
  if (path === '') throw new ApiError(404, 'Not Found');
  if (typeof input.message !== 'string' || input.message === '') {
    throw new ApiError(422, 'Invalid request.\n\n"message" wasn\'t supplied.');
  }
  const branch = typeof input.branch === 'string' && input.branch !== '' ? input.branch : repo.defaultBranch;
  const tip = branchTip(repo, branch);
  const rootTree = tip === undefined ? null : (commitInfo(repo, tip) as { tree: string }).tree;
  const existing = rootTree === null ? null : lookupPath(repo, rootTree, path);
  if (existing !== null && existing.mode === '40000') throw new ApiError(422, 'Invalid request.\n\n"sha" wasn\'t supplied.');
  if (existing !== null) {
    if (typeof input.sha !== 'string' || input.sha === '') throw new ApiError(422, 'Invalid request.\n\n"sha" wasn\'t supplied.');
    if (input.sha !== existing.oid) throw new ApiError(409, `${path} does not match ${existing.oid}`);
  }
  const blob = writeObject(repo, 'blob', input.content);
  let tree: string | null;
  try {
    tree = updateTree(repo, rootTree, path.split('/'), 0, { mode: existing?.mode ?? '100644', oid: blob });
  } catch (err) {
    if (err instanceof TreeEditError) throw new ApiError(422, `Invalid request.\n\n${err.message}`);
    throw err;
  }
  const author = contentsIdent(input.author);
  const committer = input.committer === undefined ? author : contentsIdent(input.committer);
  const commit = createCommit(repo, {
    tree: tree ?? ensureEmptyTree(repo),
    parents: tip === undefined ? [] : [tip],
    message: input.message,
    author,
    committer,
  });
  setRef(repo, `refs/heads/${branch}`, commit);
  return { created: existing === null, commit, blob, path, branch };
}

export function deleteContents(repo: Repo, rawPath: string, input: { message: unknown; sha: unknown; branch?: unknown }): ContentsCommit {
  const path = cleanPath(rawPath);
  const branch = typeof input.branch === 'string' && input.branch !== '' ? input.branch : repo.defaultBranch;
  const tip = repo.refs.get(`refs/heads/${branch}`);
  if (tip === undefined) throw new ApiError(404, 'Not Found');
  const rootTree = (commitInfo(repo, tip) as { tree: string }).tree;
  const existing = lookupPath(repo, rootTree, path);
  if (existing === null || existing.mode === '40000') throw new ApiError(404, 'Not Found');
  if (typeof input.message !== 'string' || input.message === '') throw new ApiError(422, 'Invalid request.\n\n"message" wasn\'t supplied.');
  if (typeof input.sha !== 'string' || input.sha === '') throw new ApiError(422, 'Invalid request.\n\n"sha" wasn\'t supplied.');
  if (input.sha !== existing.oid) throw new ApiError(409, `${path} does not match ${existing.oid}`);
  const tree = updateTree(repo, rootTree, path.split('/'), 0, null);
  const author = identFor(viewer.login);
  const commit = createCommit(repo, { tree: tree ?? ensureEmptyTree(repo), parents: [tip], message: input.message, author });
  setRef(repo, `refs/heads/${branch}`, commit);
  return { created: false, commit, blob: null, path, branch };
}

// ---------------------------------------------------------------- labels

export function createLabel(repo: Repo, input: { name: unknown; color?: unknown; description?: unknown }): Label {
  if (typeof input.name !== 'string' || input.name.trim() === '') {
    throw new ApiError(422, 'Validation Failed', [{ resource: 'Label', code: 'missing_field', field: 'name' }]);
  }
  const name = input.name.trim();
  if (repo.labels.has(name.toLowerCase())) {
    throw new ApiError(422, 'Validation Failed', [{ resource: 'Label', code: 'already_exists', field: 'name' }]);
  }
  const label: Label = {
    id: entityId(),
    name,
    color: typeof input.color === 'string' && input.color !== '' ? input.color.replace(/^#/, '') : 'ededed',
    description: typeof input.description === 'string' ? input.description : null,
  };
  repo.labels.set(name.toLowerCase(), label);
  return label;
}

export function resolveLabels(repo: Repo, input: unknown): Label[] {
  if (input === undefined || input === null) return [];
  if (!Array.isArray(input)) throw new ApiError(422, 'Validation Failed', [{ resource: 'Issue', code: 'invalid', field: 'labels' }]);
  const out: Label[] = [];
  for (const item of input) {
    const name = typeof item === 'string' ? item : typeof item === 'object' && item !== null && typeof (item as { name?: unknown }).name === 'string' ? (item as { name: string }).name : '';
    const label = repo.labels.get(name.toLowerCase());
    if (label === undefined) throw new ApiError(422, 'Validation Failed', [{ resource: 'Label', code: 'invalid', field: 'labels', value: name }]);
    if (!out.includes(label)) out.push(label);
  }
  return out;
}

// ---------------------------------------------------------------- issues

export function createIssue(repo: Repo, input: { title: unknown; body?: unknown; labels?: unknown; user?: string }): Issue {
  const title = typeof input.title === 'number' ? String(input.title) : input.title;
  if (typeof title !== 'string' || title.trim() === '') {
    throw new ApiError(422, 'Validation Failed', [{ resource: 'Issue', code: 'missing_field', field: 'title' }]);
  }
  const labels = resolveLabels(repo, input.labels);
  const now = nextIso();
  const issue: Issue = {
    kind: 'issue',
    id: entityId(),
    number: repo.nextNumber++,
    repo,
    title,
    body: typeof input.body === 'string' ? input.body : null,
    state: 'open',
    stateReason: null,
    labels,
    comments: [],
    user: input.user ?? viewer.login,
    createdAt: now,
    updatedAt: now,
    closedAt: null,
    seq: nextSeq++,
    json: null,
  };
  repo.issues.set(issue.number, issue);
  repo.issueList.push(issue);
  issuesById.set(issue.id, issue);
  return issue;
}

export function touchIssue(issue: Issue): void {
  issue.updatedAt = nextIso();
  issue.seq = nextSeq++;
  issue.json = null;
}

export function updateIssue(issue: Issue, patch: Record<string, unknown>): void {
  if (patch.title !== undefined) {
    if (typeof patch.title !== 'string' || patch.title.trim() === '') throw new ApiError(422, 'Validation Failed', [{ resource: 'Issue', code: 'missing_field', field: 'title' }]);
    issue.title = patch.title;
  }
  if (patch.body !== undefined) issue.body = typeof patch.body === 'string' ? patch.body : null;
  if (patch.labels !== undefined) issue.labels = resolveLabels(issue.repo, patch.labels);
  if (patch.state !== undefined) {
    if (patch.state !== 'open' && patch.state !== 'closed') throw new ApiError(422, 'Validation Failed', [{ resource: 'Issue', code: 'invalid', field: 'state' }]);
    setIssueState(issue, patch.state, typeof patch.state_reason === 'string' ? patch.state_reason : null);
  }
  touchIssue(issue);
}

export function setIssueState(issue: Issue, state: 'open' | 'closed', reason: string | null = null): void {
  if (issue.state === state) return;
  issue.state = state;
  issue.stateReason = state === 'closed' ? (reason ?? 'completed') : 'reopened';
  issue.closedAt = state === 'closed' ? nextIso() : null;
  touchIssue(issue);
}

export function addComment(parent: Issue | Pull, body: unknown, user: string = viewer.login): Comment {
  if (typeof body !== 'string' || body === '') throw new ApiError(422, 'Validation Failed', [{ resource: 'IssueComment', code: 'missing_field', field: 'body' }]);
  const now = nextIso();
  const comment: Comment = { id: entityId(), body, user, createdAt: now, updatedAt: now, parent };
  parent.comments.push(comment);
  commentsById.set(comment.id, comment);
  if (parent.kind === 'issue') touchIssue(parent);
  else parent.updatedAt = nextIso();
  return comment;
}

export interface IssueQuery {
  state?: string;
  labels?: string;
  sort?: string;
  direction?: string;
}

export function listIssues(repo: Repo, query: IssueQuery): Issue[] {
  const state = query.state === 'closed' || query.state === 'all' ? query.state : 'open';
  const wanted = query.labels === undefined || query.labels === '' ? [] : query.labels.split(',').map((name) => name.trim().toLowerCase());
  let items = repo.issueList.filter((issue) => (state === 'all' || issue.state === state) && wanted.every((name) => issue.labels.some((label) => label.name.toLowerCase() === name)));
  const sort = query.sort === 'updated' || query.sort === 'comments' ? query.sort : 'created';
  const asc = query.direction === 'asc';
  items = items.slice();
  items.sort((a, b) => {
    const delta = sort === 'created' ? a.number - b.number : sort === 'updated' ? a.seq - b.seq : a.comments.length - b.comments.length || a.number - b.number;
    return asc ? delta : -delta;
  });
  return items;
}

export function findThread(repo: Repo, number: number): Issue | Pull | undefined {
  return repo.issues.get(number) ?? repo.pulls.get(number);
}

// ---------------------------------------------------------------- pull requests

export function createPull(repo: Repo, input: { title: unknown; body?: unknown; head: unknown; base: unknown; draft?: unknown; user?: string }): Pull {
  const missing = (field: string): ApiError => new ApiError(422, 'Validation Failed', [{ resource: 'PullRequest', code: 'missing_field', field }]);
  if (typeof input.title !== 'string' || input.title.trim() === '') throw missing('title');
  if (typeof input.head !== 'string' || input.head === '') throw missing('head');
  if (typeof input.base !== 'string' || input.base === '') throw missing('base');
  const headRef = input.head.includes(':') ? input.head.slice(input.head.indexOf(':') + 1) : input.head;
  const baseRef = input.base;
  const invalid = (field: string): ApiError => new ApiError(422, 'Validation Failed', [{ resource: 'PullRequest', code: 'invalid', field }]);
  if (!repo.refs.has(`refs/heads/${headRef}`)) throw invalid('head');
  if (!repo.refs.has(`refs/heads/${baseRef}`)) throw invalid('base');
  for (const existing of repo.pullList) {
    if (existing.state === 'open' && existing.headRef === headRef && existing.baseRef === baseRef) {
      throw new ApiError(422, 'Validation Failed', [{ resource: 'PullRequest', code: 'custom', message: `A pull request already exists for ${repo.owner}:${headRef}.` }]);
    }
  }
  const now = nextIso();
  const pull: Pull = {
    kind: 'pull',
    id: entityId(),
    number: repo.nextNumber++,
    repo,
    title: input.title,
    body: typeof input.body === 'string' ? input.body : null,
    state: 'open',
    draft: input.draft === true,
    headRef,
    baseRef,
    frozenHead: null,
    frozenBase: null,
    merged: false,
    mergedAt: null,
    mergeCommitSha: null,
    comments: [],
    labels: [],
    user: input.user ?? viewer.login,
    createdAt: now,
    updatedAt: now,
    closedAt: null,
    seq: nextSeq++,
  };
  repo.pulls.set(pull.number, pull);
  repo.pullList.push(pull);
  pullsById.set(pull.id, pull);
  return pull;
}

export function pullHeadSha(pull: Pull): string {
  if (pull.frozenHead !== null) return pull.frozenHead;
  return pull.repo.refs.get(`refs/heads/${pull.headRef}`) ?? '';
}

export function pullBaseSha(pull: Pull): string {
  if (pull.frozenBase !== null) return pull.frozenBase;
  return pull.repo.refs.get(`refs/heads/${pull.baseRef}`) ?? '';
}

function pullMergeBase(pull: Pull): string | null {
  const head = pullHeadSha(pull);
  const base = pullBaseSha(pull);
  if (head === '' || base === '') return null;
  return mergeBase(pull.repo, base, head);
}

/** `true` when the head merges cleanly into the base right now, `false` on a conflict, `null` when unknown. */
export function pullMergeable(pull: Pull): boolean | null {
  if (pull.merged) return null;
  const head = pullHeadSha(pull);
  const base = pullBaseSha(pull);
  const common = pullMergeBase(pull);
  if (head === '' || base === '' || common === null) return null;
  const repo = pull.repo;
  const result = mergeTrees(repo, (commitInfo(repo, common) as { tree: string }).tree, (commitInfo(repo, base) as { tree: string }).tree, (commitInfo(repo, head) as { tree: string }).tree);
  return result.conflicts.length === 0;
}

export function pullFiles(pull: Pull): FileDiff[] {
  const repo = pull.repo;
  const head = pullHeadSha(pull);
  const common = pullMergeBase(pull);
  if (head === '') return [];
  const headTree = (commitInfo(repo, head) as { tree: string }).tree;
  const baseTree = common === null ? null : (commitInfo(repo, common) as { tree: string }).tree;
  return diffTrees(repo, baseTree, headTree);
}

export function pullCommits(pull: Pull): string[] {
  const head = pullHeadSha(pull);
  if (head === '') return [];
  const common = pullMergeBase(pull);
  const exclude = common === null ? undefined : ancestorsOf(pull.repo, common);
  return listCommits(pull.repo, head, 250, exclude);
}

export function updatePull(pull: Pull, patch: Record<string, unknown>): void {
  if (typeof patch.title === 'string' && patch.title !== '') pull.title = patch.title;
  if (patch.body !== undefined) pull.body = typeof patch.body === 'string' ? patch.body : null;
  if (typeof patch.base === 'string' && pull.state === 'open') {
    if (!pull.repo.refs.has(`refs/heads/${patch.base}`)) throw new ApiError(422, 'Validation Failed', [{ resource: 'PullRequest', code: 'invalid', field: 'base' }]);
    pull.baseRef = patch.base;
  }
  if (patch.state === 'closed') closePull(pull);
  else if (patch.state === 'open') reopenPull(pull);
  pull.updatedAt = nextIso();
  pull.seq = nextSeq++;
}

export function closePull(pull: Pull): void {
  if (pull.state === 'closed') return;
  pull.frozenHead = pullHeadSha(pull);
  pull.frozenBase = pullBaseSha(pull);
  pull.state = 'closed';
  pull.closedAt = nextIso();
}

export function reopenPull(pull: Pull): void {
  if (pull.state === 'open' || pull.merged) return;
  pull.frozenHead = null;
  pull.frozenBase = null;
  pull.state = 'open';
  pull.closedAt = null;
}

export type MergeMethod = 'merge' | 'squash' | 'rebase';

export function mergePull(pull: Pull, input: { method?: unknown; title?: unknown; message?: unknown; sha?: unknown }): { sha: string; message: string } {
  const notMergeable = (): ApiError => new ApiError(405, 'Pull Request is not mergeable');
  if (pull.state !== 'open' || pull.merged) throw notMergeable();
  const repo = pull.repo;
  const head = pullHeadSha(pull);
  const base = pullBaseSha(pull);
  if (head === '' || base === '') throw notMergeable();
  if (typeof input.sha === 'string' && input.sha !== '' && input.sha !== head) {
    throw new ApiError(409, 'Head branch was modified. Review and try the merge again.');
  }
  const common = mergeBase(repo, base, head);
  if (common === null) throw notMergeable();
  const merge = mergeTrees(repo, (commitInfo(repo, common) as { tree: string }).tree, (commitInfo(repo, base) as { tree: string }).tree, (commitInfo(repo, head) as { tree: string }).tree);
  if (merge.tree === null) throw notMergeable();
  const method: MergeMethod = input.method === 'merge' || input.method === 'rebase' ? input.method : 'squash';
  const title = typeof input.title === 'string' && input.title !== '' ? input.title : null;
  const extra = typeof input.message === 'string' ? input.message : null;
  const author = identFor(viewer.login);
  let sha: string;
  if (method === 'rebase' && common === base) {
    sha = head;
  } else if (method === 'merge') {
    const subject = title ?? `Merge pull request #${pull.number} from ${repo.owner}/${pull.headRef}`;
    const body = extra ?? pull.title;
    sha = createCommit(repo, { tree: merge.tree, parents: [base, head], message: `${subject}\n\n${body}`, author });
  } else {
    const subject = title ?? `${pull.title} (#${pull.number})`;
    const body = extra ?? (pull.body ?? '');
    sha = createCommit(repo, { tree: merge.tree, parents: [base], message: body === '' ? subject : `${subject}\n\n${body}`, author });
  }
  pull.frozenHead = head;
  pull.frozenBase = base;
  setRef(repo, `refs/heads/${pull.baseRef}`, sha);
  pull.merged = true;
  pull.state = 'closed';
  pull.mergedAt = nextIso();
  pull.closedAt = pull.mergedAt;
  pull.mergeCommitSha = sha;
  pull.updatedAt = pull.mergedAt;
  pull.seq = nextSeq++;
  return { sha, message: 'Pull Request successfully merged' };
}

// ---------------------------------------------------------------- opaque global ids

/** A GitHub-style opaque node id: base64 of "<type>:<number>", so it is stable and never the plain number. */
export function globalId(type: string, id: number): string {
  return Buffer.from(`${String(type.length).padStart(3, '0')}:${type}${id}`, 'utf8').toString('base64').replace(/=+$/, '');
}

export function parseGlobalId(value: string): { type: string; id: number } | null {
  let text: string;
  try {
    text = Buffer.from(value, 'base64').toString('utf8');
  } catch {
    return null;
  }
  const match = /^\d{3}:([A-Za-z]+?)(\d+)$/.exec(text);
  if (match === null) return null;
  return { type: match[1] as string, id: Number(match[2]) };
}

/** A short stable etag for a JSON body. */
export function etagOf(body: Buffer): string {
  return `W/"${createHash('sha1').update(body).digest('hex')}"`;
}

export { iso };
