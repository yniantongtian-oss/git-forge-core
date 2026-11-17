// GitHub REST v3 under /api/v3: repositories, branches, commits, git refs and objects, contents, labels,
// issues, comments and pull requests. Issue JSON is serialized once and cached as a Buffer, so the hot read
// path is a lookup and a write.

import type { IncomingMessage, ServerResponse } from 'node:http';
import { treeOf, parseTag } from './objects.ts';
import {
  ApiError,
  addComment,
  allRepos,
  createIssue,
  createLabel,
  createPull,
  createRef,
  createRepo,
  deleteContents,
  deleteRepo,
  findThread,
  getContents,
  getRepo,
  globalId,
  listIssues,
  listRepos,
  mergePull,
  pullBaseSha,
  pullCommits,
  pullFiles,
  pullHeadSha,
  pullMergeable,
  putContents,
  removeRef,
  updateIssue,
  updatePull,
  updateRef,
  updateRepo,
  viewer,
} from './store.ts';
import type { Comment, ContentsFile, Issue, Label, Pull, Repo } from './store.ts';
import { commitInfo, getObject, listCommits, refsWithPrefix, resolveCommit, diffTrees } from './gitops.ts';
import type { Ident } from './objects.ts';
import { iso } from './clock.ts';
import { HttpError, decode, parseQuery, readBody, sendError, sendJson } from './util.ts';

// ---------------------------------------------------------------- base url

let apiBase = 'http://localhost';
let apiBaseKnown = false;

/** Remember the host the suite reaches the forge at, so URLs inside cached JSON point back at it. */
export function setBase(host: string | undefined): void {
  if (apiBaseKnown || host === undefined || host === '') return;
  apiBase = `http://${host}`;
  apiBaseKnown = true;
}

/** The origin the forge is reached at, for URLs inside GraphQL and MCP answers. */
export function baseUrl(): string {
  return apiBase;
}

// ---------------------------------------------------------------- JSON shapes

export function userJson(login: string): Record<string, unknown> {
  const id = login === viewer.login ? viewer.id : 1_000_000 + login.length;
  return {
    login,
    id,
    node_id: globalId('User', id),
    avatar_url: `${apiBase}/avatars/u/${id}`,
    gravatar_id: '',
    url: `${apiBase}/api/v3/users/${login}`,
    html_url: `${apiBase}/${login}`,
    type: 'User',
    site_admin: false,
  };
}

export function repoJson(repo: Repo): Record<string, unknown> {
  const full = `${repo.owner}/${repo.name}`;
  const url = `${apiBase}/api/v3/repos/${full}`;
  return {
    id: repo.id,
    node_id: globalId('Repository', repo.id),
    name: repo.name,
    full_name: full,
    private: repo.private,
    owner: userJson(repo.owner),
    html_url: `${apiBase}/${full}`,
    description: repo.description,
    fork: false,
    url,
    branches_url: `${url}/branches{/branch}`,
    commits_url: `${url}/commits{/sha}`,
    contents_url: `${url}/contents/{+path}`,
    issues_url: `${url}/issues{/number}`,
    labels_url: `${url}/labels{/name}`,
    pulls_url: `${url}/pulls{/number}`,
    git_url: `git://${apiBase.slice(7)}/${full}.git`,
    clone_url: `${apiBase}/${full}.git`,
    created_at: repo.createdAt,
    updated_at: repo.updatedAt,
    pushed_at: repo.pushedAt === 0 ? null : iso(repo.pushedAt),
    homepage: null,
    size: 0,
    stargazers_count: 0,
    watchers_count: 0,
    language: null,
    has_issues: repo.hasIssues,
    has_projects: true,
    has_downloads: true,
    has_wiki: true,
    has_pages: false,
    forks_count: 0,
    archived: repo.archived,
    disabled: false,
    open_issues_count: repo.issueList.filter((issue) => issue.state === 'open').length + repo.pullList.filter((pull) => pull.state === 'open').length,
    license: null,
    allow_forking: true,
    is_template: false,
    topics: [],
    visibility: repo.private ? 'private' : 'public',
    forks: 0,
    open_issues: repo.issueList.filter((issue) => issue.state === 'open').length,
    watchers: 0,
    default_branch: repo.defaultBranch,
    permissions: { admin: true, maintain: true, push: true, triage: true, pull: true },
  };
}

function labelJson(repo: Repo, label: Label): Record<string, unknown> {
  return {
    id: label.id,
    node_id: globalId('Label', label.id),
    url: `${apiBase}/api/v3/repos/${repo.owner}/${repo.name}/labels/${encodeURIComponent(label.name)}`,
    name: label.name,
    color: label.color,
    default: false,
    description: label.description,
  };
}

function issueObject(issue: Issue): Record<string, unknown> {
  const repo = issue.repo;
  const url = `${apiBase}/api/v3/repos/${repo.owner}/${repo.name}/issues/${issue.number}`;
  return {
    url,
    repository_url: `${apiBase}/api/v3/repos/${repo.owner}/${repo.name}`,
    labels_url: `${url}/labels{/name}`,
    comments_url: `${url}/comments`,
    events_url: `${url}/events`,
    html_url: `${apiBase}/${repo.owner}/${repo.name}/issues/${issue.number}`,
    id: issue.id,
    node_id: globalId('Issue', issue.id),
    number: issue.number,
    title: issue.title,
    user: userJson(issue.user),
    labels: issue.labels.map((label) => labelJson(repo, label)),
    state: issue.state,
    locked: false,
    assignee: null,
    assignees: [],
    milestone: null,
    comments: issue.comments.length,
    created_at: issue.createdAt,
    updated_at: issue.updatedAt,
    closed_at: issue.closedAt,
    author_association: 'OWNER',
    active_lock_reason: null,
    body: issue.body ?? '',
    reactions: { url: `${url}/reactions`, total_count: 0 },
    timeline_url: `${url}/timeline`,
    performed_via_github_app: null,
    state_reason: issue.stateReason,
  };
}

/** The serialized issue, built once and reused until the issue changes. */
export function issueBuffer(issue: Issue): Buffer {
  if (issue.json === null) issue.json = Buffer.from(JSON.stringify(issueObject(issue)), 'utf8');
  return issue.json;
}

export function commentJson(comment: Comment): Record<string, unknown> {
  const parent = comment.parent;
  const repo = parent.repo;
  const base = `${apiBase}/api/v3/repos/${repo.owner}/${repo.name}`;
  return {
    url: `${base}/issues/comments/${comment.id}`,
    html_url: `${apiBase}/${repo.owner}/${repo.name}/issues/${parent.number}#issuecomment-${comment.id}`,
    issue_url: `${base}/issues/${parent.number}`,
    id: comment.id,
    node_id: globalId('IssueComment', comment.id),
    user: userJson(comment.user),
    created_at: comment.createdAt,
    updated_at: comment.updatedAt,
    author_association: 'OWNER',
    body: comment.body,
  };
}

function identJson(ident: Ident): Record<string, unknown> {
  return { name: ident.name, email: ident.email, date: iso(ident.when) };
}

function trimMessage(message: string): string {
  return message.endsWith('\n') ? message.slice(0, -1) : message;
}

export function commitJson(repo: Repo, oid: string, withFiles = false): Record<string, unknown> | null {
  const info = commitInfo(repo, oid);
  if (info === null) return null;
  const base = `${apiBase}/api/v3/repos/${repo.owner}/${repo.name}`;
  const out: Record<string, unknown> = {
    sha: oid,
    node_id: globalId('Commit', parseInt(oid.slice(0, 8), 16)),
    commit: {
      author: identJson(info.author),
      committer: identJson(info.committer),
      message: trimMessage(info.message),
      tree: { sha: info.tree, url: `${base}/git/trees/${info.tree}` },
      url: `${base}/git/commits/${oid}`,
      comment_count: 0,
      verification: { verified: false, reason: 'unsigned', signature: null, payload: null },
    },
    url: `${base}/commits/${oid}`,
    html_url: `${apiBase}/${repo.owner}/${repo.name}/commit/${oid}`,
    comments_url: `${base}/commits/${oid}/comments`,
    author: info.author.name === viewer.login ? userJson(viewer.login) : null,
    committer: info.committer.name === viewer.login ? userJson(viewer.login) : null,
    parents: info.parents.map((parent) => ({ sha: parent, url: `${base}/commits/${parent}`, html_url: `${apiBase}/${repo.owner}/${repo.name}/commit/${parent}` })),
  };
  if (withFiles) {
    const parent = info.parents[0] === undefined ? null : commitInfo(repo, info.parents[0]);
    const files = diffTrees(repo, parent === null ? null : parent.tree, info.tree);
    out.stats = { total: files.reduce((n, f) => n + f.changes, 0), additions: files.reduce((n, f) => n + f.additions, 0), deletions: files.reduce((n, f) => n + f.deletions, 0) };
    out.files = files.map((file) => fileJson(repo, file, oid));
  }
  return out;
}

function fileJson(repo: Repo, file: { filename: string; status: string; additions: number; deletions: number; changes: number; sha: string; patch?: string }, ref: string): Record<string, unknown> {
  const base = `${apiBase}/api/v3/repos/${repo.owner}/${repo.name}`;
  const out: Record<string, unknown> = {
    sha: file.sha,
    filename: file.filename,
    status: file.status,
    additions: file.additions,
    deletions: file.deletions,
    changes: file.changes,
    blob_url: `${apiBase}/${repo.owner}/${repo.name}/blob/${ref}/${file.filename}`,
    raw_url: `${apiBase}/${repo.owner}/${repo.name}/raw/${ref}/${file.filename}`,
    contents_url: `${base}/contents/${file.filename}?ref=${ref}`,
  };
  if (file.patch !== undefined) out.patch = file.patch;
  return out;
}

export function pullJson(pull: Pull, full: boolean): Record<string, unknown> {
  const repo = pull.repo;
  const base = `${apiBase}/api/v3/repos/${repo.owner}/${repo.name}`;
  const headSha = pullHeadSha(pull);
  const baseSha = pullBaseSha(pull);
  const side = (ref: string, sha: string): Record<string, unknown> => ({ label: `${repo.owner}:${ref}`, ref, sha, user: userJson(repo.owner), repo: repoJson(repo) });
  const out: Record<string, unknown> = {
    url: `${base}/pulls/${pull.number}`,
    id: pull.id,
    node_id: globalId('PullRequest', pull.id),
    html_url: `${apiBase}/${repo.owner}/${repo.name}/pull/${pull.number}`,
    diff_url: `${apiBase}/${repo.owner}/${repo.name}/pull/${pull.number}.diff`,
    patch_url: `${apiBase}/${repo.owner}/${repo.name}/pull/${pull.number}.patch`,
    issue_url: `${base}/issues/${pull.number}`,
    number: pull.number,
    state: pull.state,
    locked: false,
    title: pull.title,
    user: userJson(pull.user),
    body: pull.body ?? '',
    created_at: pull.createdAt,
    updated_at: pull.updatedAt,
    closed_at: pull.closedAt,
    merged_at: pull.mergedAt,
    merge_commit_sha: pull.mergeCommitSha,
    assignee: null,
    assignees: [],
    requested_reviewers: [],
    labels: pull.labels.map((label) => labelJson(repo, label)),
    milestone: null,
    draft: pull.draft,
    commits_url: `${base}/pulls/${pull.number}/commits`,
    comments_url: `${base}/issues/${pull.number}/comments`,
    head: side(pull.headRef, headSha),
    base: side(pull.baseRef, baseSha),
    author_association: 'OWNER',
    auto_merge: null,
    active_lock_reason: null,
    merged: pull.merged,
  };
  if (full) {
    const mergeable = pullMergeable(pull);
    const files = pullFiles(pull);
    out.mergeable = mergeable;
    out.rebaseable = mergeable;
    out.mergeable_state = pull.merged ? 'unknown' : mergeable === true ? 'clean' : mergeable === false ? 'dirty' : 'unknown';
    out.merged_by = pull.merged ? userJson(viewer.login) : null;
    out.comments = pull.comments.length;
    out.review_comments = 0;
    out.maintainer_can_modify = false;
    out.commits = pullCommits(pull).length;
    out.additions = files.reduce((n, f) => n + f.additions, 0);
    out.deletions = files.reduce((n, f) => n + f.deletions, 0);
    out.changed_files = files.length;
  }
  return out;
}

// ---------------------------------------------------------------- helpers

function notFound(res: ServerResponse): void {
  sendError(res, 404, 'Not Found');
}

async function jsonBody(req: IncomingMessage): Promise<Record<string, unknown>> {
  const raw = await readBody(req);
  if (raw.length === 0) return {};
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw.toString('utf8'));
  } catch {
    throw new HttpError(400, 'Problems parsing JSON');
  }
  if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) throw new HttpError(400, 'Problems parsing JSON');
  return parsed as Record<string, unknown>;
}

function positive(value: string | undefined, fallback: number, max: number): number {
  const n = value === undefined ? NaN : parseInt(value, 10);
  if (!Number.isFinite(n) || n < 1) return fallback;
  return Math.min(n, max);
}

function pageUrl(req: IncomingMessage, path: string, query: Record<string, string>, page: number): string {
  const params: string[] = [];
  let seen = false;
  for (const key of Object.keys(query)) {
    if (key === 'page') {
      params.push(`page=${page}`);
      seen = true;
    } else params.push(`${encodeURIComponent(key)}=${encodeURIComponent(query[key] as string)}`);
  }
  if (!seen) params.push(`page=${page}`);
  return `http://${req.headers.host ?? apiBase.slice(7)}${path}?${params.join('&')}`;
}

interface Page<T> {
  items: T[];
  link: Record<string, string> | undefined;
}

function paginate<T>(req: IncomingMessage, path: string, query: Record<string, string>, all: readonly T[]): Page<T> {
  const perPage = positive(query.per_page, 30, 100);
  const page = positive(query.page, 1, 1_000_000);
  const last = Math.max(1, Math.ceil(all.length / perPage));
  const items = all.slice((page - 1) * perPage, page * perPage);
  const rels: string[] = [];
  if (page < last) {
    rels.push(`<${pageUrl(req, path, query, page + 1)}>; rel="next"`, `<${pageUrl(req, path, query, last)}>; rel="last"`);
  }
  if (page > 1) rels.push(`<${pageUrl(req, path, query, page - 1)}>; rel="prev"`, `<${pageUrl(req, path, query, 1)}>; rel="first"`);
  return { items, link: rels.length === 0 ? undefined : { link: rels.join(', ') } };
}

function sendPage<T>(res: ServerResponse, page: Page<T>, render: (item: T) => unknown): void {
  sendJson(res, 200, page.items.map(render), page.link);
}

function rawJoin(buffers: Buffer[]): Buffer {
  const parts: Buffer[] = [Buffer.from('[')];
  buffers.forEach((buf, index) => {
    if (index > 0) parts.push(Buffer.from(','));
    parts.push(buf);
  });
  parts.push(Buffer.from(']'));
  return Buffer.concat(parts);
}

function resolveRepo(res: ServerResponse, owner: string, name: string): Repo | null {
  if (owner.includes('%')) owner = decode(owner);
  if (name.includes('%')) name = decode(name);
  if (owner.toLowerCase() !== viewer.login.toLowerCase()) {
    notFound(res);
    return null;
  }
  const repo = getRepo(owner, name);
  if (repo === undefined) {
    notFound(res);
    return null;
  }
  return repo;
}

function number(text: string | undefined): number {
  return text !== undefined && /^\d+$/.test(text) ? parseInt(text, 10) : NaN;
}

// ---------------------------------------------------------------- the router

/** Handle one authenticated request under /api/v3; async only for routes that read a body. */
export function handleRest(req: IncomingMessage, res: ServerResponse, path: string, search: string): void | Promise<void> {
  const method = req.method ?? 'GET';
  const rest = path.length > 7 ? path.slice(8) : '';
  const parts = rest === '' ? [] : rest.split('/');
  if (parts.length > 0 && parts[parts.length - 1] === '' && !(parts[0] === 'repos' && parts[3] === 'contents')) parts.pop();
  const first = parts[0];
  // The hot read: GET /repos/{owner}/{repo}/issues/{number}.
  if (method === 'GET' && first === 'repos' && parts[4] === 'issues' && parts.length === 6) {
    const repo = resolveRepo(res, parts[1] as string, parts[2] as string);
    if (repo === null) return;
    const issue = repo.issues.get(number(parts[5]));
    if (issue !== undefined) {
      sendJson(res, 200, issueBuffer(issue));
      return;
    }
  }
  const query = search === '' ? {} : parseQuery(search);
  const bodyless = method === 'GET' || method === 'HEAD';
  const run = (): void | Promise<void> => dispatch(req, res, method, parts, path, query);
  if (bodyless) {
    try {
      const result = run();
      if (result !== undefined) return result.catch((err: unknown) => fail(res, err));
    } catch (err) {
      fail(res, err);
    }
    return;
  }
  return (async () => {
    try {
      await run();
    } catch (err) {
      fail(res, err);
    }
  })();
}

function fail(res: ServerResponse, err: unknown): void {
  if (res.headersSent) {
    res.destroy();
    return;
  }
  if (err instanceof ApiError) sendError(res, err.status, err.message, err.errors);
  else if (err instanceof HttpError) sendError(res, err.status, err.message);
  else sendError(res, 500, `Server Error: ${err instanceof Error ? err.message : String(err)}`);
}

function dispatch(req: IncomingMessage, res: ServerResponse, method: string, parts: string[], path: string, query: Record<string, string>): void | Promise<void> {
  const first = parts[0];
  if (first === undefined) {
    if (method !== 'GET') return notFound(res);
    sendJson(res, 200, { current_user_url: `${apiBase}/api/v3/user`, repository_url: `${apiBase}/api/v3/repos/{owner}/{repo}`, rate_limit_url: `${apiBase}/api/v3/rate_limit` }, { 'x-oauth-scopes': 'repo, read:org', 'x-accepted-oauth-scopes': '' });
    return;
  }
  if (parts.length === 1 && method === 'GET') {
    if (first === 'meta') {
      sendJson(res, 200, { verifiable_password_authentication: false, installed_version: '3.14.0', packages: [], hooks: [], web: [], api: [], git: [], pages: [], importer: [] });
      return;
    }
    if (first === 'user') {
      sendJson(res, 200, { ...userJson(viewer.login), name: viewer.login, email: null, public_repos: allRepos().length, followers: 0, following: 0, created_at: '2025-01-01T00:00:01Z', updated_at: '2025-01-01T00:00:01Z' });
      return;
    }
    if (first === 'rate_limit') {
      const bucket = { limit: 5000, used: 0, remaining: 5000, reset: 1_735_693_200 };
      sendJson(res, 200, { resources: { core: bucket, graphql: bucket, search: { ...bucket, limit: 30, remaining: 30 } }, rate: bucket });
      return;
    }
  }
  if (first === 'user' && parts[1] === 'repos' && parts.length === 2) {
    if (method === 'GET') {
      let repos = listRepos();
      const sort = query.sort;
      if (sort === 'created' || sort === 'updated' || sort === 'pushed') {
        repos = repos.sort((a, b) => (sort === 'pushed' ? a.pushedAt - b.pushedAt : a.id - b.id));
        if ((query.direction ?? 'desc') === 'desc') repos.reverse();
      } else if (query.direction === 'desc') repos.reverse();
      sendPage(res, paginate(req, path, query, repos), repoJson);
      return;
    }
    if (method === 'POST') {
      return jsonBody(req).then((body) => {
        const repo = createRepo({ name: body.name, description: body.description, private: body.private, autoInit: body.auto_init, defaultBranch: body.default_branch });
        sendJson(res, 201, repoJson(repo), { location: `${apiBase}/api/v3/repos/${repo.owner}/${repo.name}` });
      });
    }
    return notFound(res);
  }
  if (first === 'users' && parts.length === 2 && method === 'GET') {
    if ((parts[1] as string).toLowerCase() !== viewer.login.toLowerCase()) return notFound(res);
    sendJson(res, 200, userJson(viewer.login));
    return;
  }
  if (first === 'repos' && parts.length >= 3) {
    const repo = resolveRepo(res, parts[1] as string, parts[2] as string);
    if (repo === null) return;
    return repoRoutes(req, res, repo, method, parts.slice(3), path, query);
  }
  notFound(res);
}

function repoRoutes(req: IncomingMessage, res: ServerResponse, repo: Repo, method: string, parts: string[], path: string, query: Record<string, string>): void | Promise<void> {
  const resource = parts[0];
  if (resource === undefined) {
    if (method === 'GET') return void sendJson(res, 200, repoJson(repo));
    if (method === 'PATCH') {
      return jsonBody(req).then((body) => {
        updateRepo(repo, body);
        sendJson(res, 200, repoJson(repo));
      });
    }
    if (method === 'DELETE') {
      deleteRepo(repo);
      res.writeHead(204, { 'x-github-request-id': 'FEED:0000:00000000:00000000:00000000' });
      res.end();
      return;
    }
    return notFound(res);
  }
  switch (resource) {
    case 'branches':
      return branchRoutes(res, repo, method, parts.slice(1), req, path, query);
    case 'commits':
      return commitRoutes(res, repo, method, parts.slice(1), req, path, query);
    case 'tags':
      if (method !== 'GET') return notFound(res);
      return void sendPage(res, paginate(req, path, query, refsWithPrefix(repo, 'refs/tags/')), (ref) => {
        const commit = resolveCommit(repo, ref.name);
        return { name: ref.name.slice(10), commit: { sha: commit ?? ref.oid, url: `${apiBase}/api/v3/repos/${repo.owner}/${repo.name}/commits/${commit ?? ref.oid}` }, zipball_url: '', tarball_url: '' };
      });
    case 'git':
      return gitRoutes(req, res, repo, method, parts.slice(1), query);
    case 'contents':
      return contentsRoutes(req, res, repo, method, parts.slice(1), query);
    case 'labels':
      return labelRoutes(req, res, repo, method, parts.slice(1), path, query);
    case 'issues':
      return issueRoutes(req, res, repo, method, parts.slice(1), path, query);
    case 'pulls':
      return pullRoutes(req, res, repo, method, parts.slice(1), path, query);
    default:
      return notFound(res);
  }
}

// ---------------------------------------------------------------- branches and commits

function branchJson(repo: Repo, name: string, oid: string): Record<string, unknown> {
  const base = `${apiBase}/api/v3/repos/${repo.owner}/${repo.name}`;
  return { name, commit: { sha: oid, url: `${base}/commits/${oid}` }, protected: false };
}

function branchRoutes(res: ServerResponse, repo: Repo, method: string, parts: string[], req: IncomingMessage, path: string, query: Record<string, string>): void {
  if (method !== 'GET') return notFound(res);
  if (parts.length === 0) {
    const branches = refsWithPrefix(repo, 'refs/heads/');
    sendPage(res, paginate(req, path, query, branches), (ref) => branchJson(repo, ref.name.slice(11), ref.oid));
    return;
  }
  const name = parts.map(decode).join('/');
  const oid = repo.refs.get(`refs/heads/${name}`);
  if (oid === undefined) return notFound(res);
  sendJson(res, 200, branchJson(repo, name, oid));
}

function commitRoutes(res: ServerResponse, repo: Repo, method: string, parts: string[], req: IncomingMessage, path: string, query: Record<string, string>): void {
  if (method !== 'GET') return notFound(res);
  if (parts.length === 0) {
    if (repo.refs.size === 0) return sendError(res, 409, 'Git Repository is empty.');
    const tip = resolveCommit(repo, query.sha === undefined || query.sha === '' ? 'HEAD' : query.sha);
    if (tip === null) return sendError(res, 404, `No commit found for SHA: ${query.sha ?? ''}`);
    const perPage = positive(query.per_page, 30, 100);
    const page = positive(query.page, 1, 1_000_000);
    const all = listCommits(repo, tip, perPage * page + 1);
    const slice = all.slice((page - 1) * perPage, page * perPage);
    const link = all.length > page * perPage ? { link: `<${pageUrl(req, path, query, page + 1)}>; rel="next"` } : undefined;
    sendJson(res, 200, slice.map((oid) => commitJson(repo, oid)), link);
    return;
  }
  const oid = resolveCommit(repo, parts.map(decode).join('/'));
  if (oid === null) return sendError(res, 404, 'Not Found');
  sendJson(res, 200, commitJson(repo, oid, true));
}

// ---------------------------------------------------------------- git data

function refJson(repo: Repo, name: string, oid: string): Record<string, unknown> {
  const obj = getObject(repo, oid);
  const base = `${apiBase}/api/v3/repos/${repo.owner}/${repo.name}`;
  return { ref: name, node_id: globalId('Ref', parseInt(oid.slice(0, 8), 16)), url: `${base}/git/${name}`, object: { sha: oid, type: obj?.type ?? 'commit', url: `${base}/git/${obj?.type ?? 'commit'}s/${oid}` } };
}

function gitRoutes(req: IncomingMessage, res: ServerResponse, repo: Repo, method: string, parts: string[], query: Record<string, string>): void | Promise<void> {
  const kind = parts[0];
  const base = `${apiBase}/api/v3/repos/${repo.owner}/${repo.name}`;
  if (kind === 'refs' || kind === 'ref') {
    const tail = parts.slice(1).map(decode).join('/');
    if (parts.length === 1 && kind === 'refs') {
      if (method === 'GET') return void sendJson(res, 200, refsWithPrefix(repo, 'refs/').map((ref) => refJson(repo, ref.name, ref.oid)));
      if (method === 'POST') {
        return jsonBody(req).then((body) => {
          createRef(repo, body.ref, body.sha);
          sendJson(res, 201, refJson(repo, body.ref as string, body.sha as string));
        });
      }
      return notFound(res);
    }
    const name = `refs/${tail}`;
    if (method === 'GET') {
      const oid = repo.refs.get(name);
      if (oid !== undefined) return void sendJson(res, 200, refJson(repo, name, oid));
      const matching = refsWithPrefix(repo, `${name}/`);
      if (matching.length > 0 && kind === 'refs') return void sendJson(res, 200, matching.map((ref) => refJson(repo, ref.name, ref.oid)));
      return notFound(res);
    }
    if (method === 'PATCH') {
      return jsonBody(req).then((body) => {
        updateRef(repo, name, body.sha, body.force === true);
        sendJson(res, 200, refJson(repo, name, body.sha as string));
      });
    }
    if (method === 'DELETE') {
      removeRef(repo, name);
      res.writeHead(204, { 'x-github-request-id': 'FEED:0000:00000000:00000000:00000001' });
      res.end();
      return;
    }
    return notFound(res);
  }
  if (kind === 'matching-refs' && method === 'GET') {
    const prefix = `refs/${parts.slice(1).map(decode).join('/')}`;
    return void sendJson(res, 200, refsWithPrefix(repo, prefix).map((ref) => refJson(repo, ref.name, ref.oid)));
  }
  if (method !== 'GET' || parts.length !== 2) return notFound(res);
  const sha = parts[1] as string;
  const obj = getObject(repo, sha);
  if (obj === undefined) return notFound(res);
  if (kind === 'commits' && obj.type === 'commit') {
    const info = commitInfo(repo, sha);
    if (info === null) return notFound(res);
    sendJson(res, 200, {
      sha,
      url: `${base}/git/commits/${sha}`,
      author: identJson(info.author),
      committer: identJson(info.committer),
      tree: { sha: info.tree, url: `${base}/git/trees/${info.tree}` },
      message: trimMessage(info.message),
      parents: info.parents.map((parent) => ({ sha: parent, url: `${base}/git/commits/${parent}` })),
    });
    return;
  }
  if (kind === 'trees' && obj.type === 'tree') {
    const entries: Array<Record<string, unknown>> = [];
    const walk = (treeSha: string, prefix: string): void => {
      const tree = getObject(repo, treeSha);
      if (tree === undefined) return;
      for (const entry of treeOf(tree)) {
        const type = entry.mode === '40000' ? 'tree' : entry.mode === '160000' ? 'commit' : 'blob';
        const item: Record<string, unknown> = { path: `${prefix}${entry.name}`, mode: entry.mode.padStart(6, '0'), type, sha: entry.oid, url: `${base}/git/${type}s/${entry.oid}` };
        if (type === 'blob') item.size = getObject(repo, entry.oid)?.data.length ?? 0;
        entries.push(item);
        if (type === 'tree' && query.recursive !== undefined && query.recursive !== '' && query.recursive !== '0' && query.recursive !== 'false') walk(entry.oid, `${prefix}${entry.name}/`);
      }
    };
    walk(sha, '');
    sendJson(res, 200, { sha, url: `${base}/git/trees/${sha}`, tree: entries, truncated: false });
    return;
  }
  if (kind === 'blobs' && obj.type === 'blob') {
    sendJson(res, 200, { sha, node_id: globalId('Blob', parseInt(sha.slice(0, 8), 16)), size: obj.data.length, url: `${base}/git/blobs/${sha}`, content: obj.data.toString('base64'), encoding: 'base64' });
    return;
  }
  if (kind === 'tags' && obj.type === 'tag') {
    const tag = parseTag(obj.data);
    sendJson(res, 200, { sha, tag: tag.tag, message: tag.message, object: { sha: tag.object, type: tag.type } });
    return;
  }
  notFound(res);
}

// ---------------------------------------------------------------- contents

function contentCommitJson(repo: Repo, oid: string): Record<string, unknown> {
  const info = commitInfo(repo, oid);
  const base = `${apiBase}/api/v3/repos/${repo.owner}/${repo.name}`;
  return {
    sha: oid,
    node_id: globalId('Commit', parseInt(oid.slice(0, 8), 16)),
    url: `${base}/git/commits/${oid}`,
    html_url: `${apiBase}/${repo.owner}/${repo.name}/commit/${oid}`,
    author: info === null ? null : identJson(info.author),
    committer: info === null ? null : identJson(info.committer),
    tree: info === null ? null : { sha: info.tree, url: `${base}/git/trees/${info.tree}` },
    message: info === null ? '' : trimMessage(info.message),
    parents: (info?.parents ?? []).map((parent) => ({ sha: parent, url: `${base}/git/commits/${parent}`, html_url: `${apiBase}/${repo.owner}/${repo.name}/commit/${parent}` })),
  };
}

function fileEntryJson(repo: Repo, entry: { path: string; oid: string; size: number; type: string }, ref: string): Record<string, unknown> {
  const base = `${apiBase}/api/v3/repos/${repo.owner}/${repo.name}`;
  const name = entry.path.slice(entry.path.lastIndexOf('/') + 1);
  return {
    type: entry.type,
    name,
    path: entry.path,
    sha: entry.oid,
    size: entry.size,
    url: `${base}/contents/${entry.path}?ref=${ref}`,
    html_url: `${apiBase}/${repo.owner}/${repo.name}/blob/${ref}/${entry.path}`,
    git_url: `${base}/git/blobs/${entry.oid}`,
    download_url: entry.type === 'file' ? `${apiBase}/${repo.owner}/${repo.name}/raw/${ref}/${entry.path}` : null,
  };
}

function contentsRoutes(req: IncomingMessage, res: ServerResponse, repo: Repo, method: string, parts: string[], query: Record<string, string>): void | Promise<void> {
  const filePath = parts.map(decode).join('/');
  if (method === 'GET') {
    const result = getContents(repo, filePath, query.ref);
    const ref = query.ref ?? repo.defaultBranch;
    if (Array.isArray(result)) {
      sendJson(res, 200, result.map((entry) => fileEntryJson(repo, { ...entry, type: entry.type }, ref)));
      return;
    }
    const file: ContentsFile = result;
    sendJson(res, 200, {
      ...fileEntryJson(repo, { path: file.path, oid: file.oid, size: file.data.length, type: 'file' }, ref),
      content: file.data.toString('base64'),
      encoding: 'base64',
    });
    return;
  }
  if (method === 'PUT') {
    return jsonBody(req).then((body) => {
      if (typeof body.content !== 'string') throw new ApiError(422, 'Invalid request.\n\n"content" wasn\'t supplied.');
      const done = putContents(repo, filePath, { message: body.message, content: Buffer.from(body.content, 'base64'), sha: body.sha, branch: body.branch, author: body.author, committer: body.committer });
      const blob = getObject(repo, done.blob as string);
      sendJson(
        res,
        done.created ? 201 : 200,
        {
          content: { ...fileEntryJson(repo, { path: done.path, oid: done.blob as string, size: blob?.data.length ?? 0, type: 'file' }, done.branch), encoding: 'base64' },
          commit: contentCommitJson(repo, done.commit),
        },
      );
    });
  }
  if (method === 'DELETE') {
    return jsonBody(req).then((body) => {
      const done = deleteContents(repo, filePath, { message: body.message, sha: body.sha, branch: body.branch });
      sendJson(res, 200, { content: null, commit: contentCommitJson(repo, done.commit) });
    });
  }
  notFound(res);
}

// ---------------------------------------------------------------- labels

function labelRoutes(req: IncomingMessage, res: ServerResponse, repo: Repo, method: string, parts: string[], path: string, query: Record<string, string>): void | Promise<void> {
  if (parts.length === 0) {
    if (method === 'GET') return void sendPage(res, paginate(req, path, query, [...repo.labels.values()]), (label) => labelJson(repo, label));
    if (method === 'POST') {
      return jsonBody(req).then((body) => {
        const label = createLabel(repo, { name: body.name, color: body.color, description: body.description });
        sendJson(res, 201, labelJson(repo, label));
      });
    }
    return notFound(res);
  }
  const name = parts.map(decode).join('/');
  const label = repo.labels.get(name.toLowerCase());
  if (label === undefined) return notFound(res);
  if (method === 'GET') return void sendJson(res, 200, labelJson(repo, label));
  if (method === 'DELETE') {
    repo.labels.delete(label.name.toLowerCase());
    for (const issue of repo.issueList) {
      if (issue.labels.includes(label)) {
        issue.labels = issue.labels.filter((candidate) => candidate !== label);
        issue.json = null;
      }
    }
    res.writeHead(204, { 'x-github-request-id': 'FEED:0000:00000000:00000000:00000002' });
    res.end();
    return;
  }
  if (method === 'PATCH') {
    return jsonBody(req).then((body) => {
      if (typeof body.new_name === 'string' && body.new_name !== '' && body.new_name.toLowerCase() !== label.name.toLowerCase()) {
        if (repo.labels.has(body.new_name.toLowerCase())) throw new ApiError(422, 'Validation Failed', [{ resource: 'Label', code: 'already_exists', field: 'name' }]);
        repo.labels.delete(label.name.toLowerCase());
        label.name = body.new_name;
        repo.labels.set(label.name.toLowerCase(), label);
      }
      if (typeof body.color === 'string') label.color = body.color.replace(/^#/, '');
      if (typeof body.description === 'string') label.description = body.description;
      for (const issue of repo.issueList) if (issue.labels.includes(label)) issue.json = null;
      sendJson(res, 200, labelJson(repo, label));
    });
  }
  notFound(res);
}

// ---------------------------------------------------------------- issues

function issueRoutes(req: IncomingMessage, res: ServerResponse, repo: Repo, method: string, parts: string[], path: string, query: Record<string, string>): void | Promise<void> {
  if (parts.length === 0) {
    if (method === 'GET') {
      const issues = listIssues(repo, { state: query.state, labels: query.labels, sort: query.sort, direction: query.direction });
      const page = paginate(req, path, query, issues);
      sendJson(res, 200, rawJoin(page.items.map(issueBuffer)), page.link);
      return;
    }
    if (method === 'POST') {
      return jsonBody(req).then((body) => {
        const issue = createIssue(repo, { title: body.title, body: body.body, labels: body.labels });
        sendJson(res, 201, issueBuffer(issue), { location: `${apiBase}/api/v3/repos/${repo.owner}/${repo.name}/issues/${issue.number}` });
      });
    }
    return notFound(res);
  }
  const n = number(parts[0]);
  if (Number.isNaN(n)) return notFound(res);
  const thread = findThread(repo, n);
  if (thread === undefined) return notFound(res);
  if (parts.length === 1) {
    if (method === 'GET') {
      if (thread.kind === 'issue') return void sendJson(res, 200, issueBuffer(thread));
      sendJson(res, 200, { ...pullAsIssue(thread), pull_request: { url: `${apiBase}/api/v3/repos/${repo.owner}/${repo.name}/pulls/${thread.number}` } });
      return;
    }
    if (method === 'PATCH') {
      return jsonBody(req).then((body) => {
        if (thread.kind === 'issue') {
          updateIssue(thread, body);
          sendJson(res, 200, issueBuffer(thread));
        } else {
          updatePull(thread, body);
          sendJson(res, 200, pullAsIssue(thread));
        }
      });
    }
    return notFound(res);
  }
  if (parts[1] === 'comments' && parts.length === 2) {
    if (method === 'GET') return void sendPage(res, paginate(req, path, query, thread.comments), commentJson);
    if (method === 'POST') {
      return jsonBody(req).then((body) => {
        sendJson(res, 201, commentJson(addComment(thread, body.body)));
      });
    }
  }
  notFound(res);
}

function pullAsIssue(pull: Pull): Record<string, unknown> {
  const repo = pull.repo;
  const url = `${apiBase}/api/v3/repos/${repo.owner}/${repo.name}/issues/${pull.number}`;
  return {
    url,
    repository_url: `${apiBase}/api/v3/repos/${repo.owner}/${repo.name}`,
    html_url: `${apiBase}/${repo.owner}/${repo.name}/pull/${pull.number}`,
    id: pull.id,
    node_id: globalId('PullRequest', pull.id),
    number: pull.number,
    title: pull.title,
    user: userJson(pull.user),
    labels: pull.labels.map((label) => labelJson(repo, label)),
    state: pull.state,
    locked: false,
    comments: pull.comments.length,
    created_at: pull.createdAt,
    updated_at: pull.updatedAt,
    closed_at: pull.closedAt,
    body: pull.body ?? '',
  };
}

// ---------------------------------------------------------------- pull requests

function pullRoutes(req: IncomingMessage, res: ServerResponse, repo: Repo, method: string, parts: string[], path: string, query: Record<string, string>): void | Promise<void> {
  if (parts.length === 0) {
    if (method === 'GET') {
      const state = query.state === 'closed' || query.state === 'all' ? query.state : 'open';
      const headFilter = query.head === undefined ? undefined : query.head.includes(':') ? query.head.slice(query.head.indexOf(':') + 1) : query.head;
      let pulls = repo.pullList.filter((pull) => (state === 'all' || pull.state === state) && (headFilter === undefined || pull.headRef === headFilter) && (query.base === undefined || pull.baseRef === query.base));
      pulls = pulls.slice().sort((a, b) => (query.sort === 'updated' ? a.seq - b.seq : a.number - b.number));
      if ((query.direction ?? 'desc') === 'desc') pulls.reverse();
      sendPage(res, paginate(req, path, query, pulls), (pull) => pullJson(pull, false));
      return;
    }
    if (method === 'POST') {
      return jsonBody(req).then((body) => {
        const pull = createPull(repo, { title: body.title, body: body.body, head: body.head, base: body.base, draft: body.draft });
        sendJson(res, 201, pullJson(pull, true), { location: `${apiBase}/api/v3/repos/${repo.owner}/${repo.name}/pulls/${pull.number}` });
      });
    }
    return notFound(res);
  }
  const n = number(parts[0]);
  const pull = Number.isNaN(n) ? undefined : repo.pulls.get(n);
  if (pull === undefined) return notFound(res);
  if (parts.length === 1) {
    if (method === 'GET') return void sendJson(res, 200, pullJson(pull, true));
    if (method === 'PATCH') {
      return jsonBody(req).then((body) => {
        updatePull(pull, body);
        sendJson(res, 200, pullJson(pull, true));
      });
    }
    return notFound(res);
  }
  const sub = parts[1];
  if (sub === 'files' && method === 'GET') {
    const head = pullHeadSha(pull);
    sendPage(res, paginate(req, path, query, pullFiles(pull)), (file) => fileJson(repo, file, head));
    return;
  }
  if (sub === 'commits' && method === 'GET') {
    sendPage(res, paginate(req, path, query, pullCommits(pull).reverse()), (oid) => commitJson(repo, oid));
    return;
  }
  if (sub === 'merge') {
    if (method === 'GET') {
      if (pull.merged) {
        res.writeHead(204, { 'x-github-request-id': 'FEED:0000:00000000:00000000:00000003' });
        res.end();
        return;
      }
      return notFound(res);
    }
    if (method === 'PUT') {
      return jsonBody(req).then((body) => {
        const merged = mergePull(pull, { method: body.merge_method, title: body.commit_title, message: body.commit_message, sha: body.sha });
        sendJson(res, 200, { sha: merged.sha, merged: true, message: merged.message });
      });
    }
  }
  if (sub === 'comments' || sub === 'reviews') {
    if (method === 'GET') return void sendJson(res, 200, []);
  }
  notFound(res);
}
