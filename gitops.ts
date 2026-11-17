// Repository-level git operations on the in-memory object store: refs, revision lookup, ancestry,
// merge bases, tree editing, three-way tree merges and file diffs. REST, GraphQL and MCP all go through here.

import { buildCommit, buildTree, commitOf, isOid, makeObject, treeOf, EMPTY_TREE_OID, parseTag } from './objects.ts';
import type { GitObject, Ident, ObjType, TreeEntry, CommitInfo } from './objects.ts';
import { nextTime } from './clock.ts';
import type { Repo } from './store.ts';

// ---------------------------------------------------------------- objects and refs

export function getObject(repo: Repo, oid: string): GitObject | undefined {
  return repo.objects.get(oid);
}

export function addObject(repo: Repo, obj: GitObject): void {
  if (!repo.objects.has(obj.oid)) repo.objects.set(obj.oid, obj);
}

export function writeObject(repo: Repo, type: ObjType, data: Buffer): string {
  const existing = makeObject(type, data);
  addObject(repo, existing);
  return existing.oid;
}

export function ensureEmptyTree(repo: Repo): string {
  if (!repo.objects.has(EMPTY_TREE_OID)) addObject(repo, makeObject('tree', Buffer.alloc(0)));
  return EMPTY_TREE_OID;
}

export function getRef(repo: Repo, name: string): string | undefined {
  return repo.refs.get(name);
}

export function setRef(repo: Repo, name: string, oid: string): void {
  repo.refs.set(name, oid);
  repo.refsVersion += 1;
  repo.pushedAt = nextTime();
}

export function deleteRef(repo: Repo, name: string): boolean {
  const had = repo.refs.delete(name);
  if (had) {
    repo.refsVersion += 1;
    repo.pushedAt = nextTime();
  }
  return had;
}

/** Refs whose name starts with `prefix`, sorted by name. */
export function refsWithPrefix(repo: Repo, prefix: string): Array<{ name: string; oid: string }> {
  const out: Array<{ name: string; oid: string }> = [];
  for (const [name, oid] of repo.refs) if (name.startsWith(prefix)) out.push({ name, oid });
  out.sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
  return out;
}

export function headRefName(repo: Repo): string {
  return `refs/heads/${repo.defaultBranch}`;
}

/** The commit a (possibly tag) object finally points at, or null when something is missing. */
export function peelToCommit(repo: Repo, oid: string): string | null {
  let current = oid;
  for (let i = 0; i < 16; i += 1) {
    const obj = repo.objects.get(current);
    if (obj === undefined) return null;
    if (obj.type === 'commit') return current;
    if (obj.type !== 'tag') return null;
    current = parseTag(obj.data).object;
  }
  return null;
}

/** Resolve a branch, tag, full ref name or object id (full or abbreviated) to a commit id. */
export function resolveCommit(repo: Repo, name: string): string | null {
  if (name === '') return null;
  const candidates = name === 'HEAD' ? [headRefName(repo)] : [name, `refs/heads/${name}`, `refs/tags/${name}`, `refs/${name}`];
  for (const candidate of candidates) {
    const oid = repo.refs.get(candidate);
    if (oid !== undefined) return peelToCommit(repo, oid);
  }
  if (isOid(name)) return peelToCommit(repo, name);
  if (/^[0-9a-f]{7,39}$/.test(name)) {
    for (const oid of repo.objects.keys()) if (oid.startsWith(name)) return peelToCommit(repo, oid);
  }
  return null;
}

export function commitInfo(repo: Repo, oid: string): CommitInfo | null {
  const obj = repo.objects.get(oid);
  if (obj === undefined || obj.type !== 'commit') return null;
  return commitOf(obj);
}

// ---------------------------------------------------------------- ancestry

export function isAncestor(repo: Repo, ancestor: string, descendant: string): boolean {
  if (ancestor === descendant) return true;
  const seen = new Set<string>([descendant]);
  const queue = [descendant];
  for (let head = 0; head < queue.length; head += 1) {
    const info = commitInfo(repo, queue[head] as string);
    if (info === null) continue;
    for (const parent of info.parents) {
      if (parent === ancestor) return true;
      if (!seen.has(parent)) {
        seen.add(parent);
        queue.push(parent);
      }
    }
  }
  return false;
}

export function ancestorsOf(repo: Repo, tip: string): Set<string> {
  const seen = new Set<string>([tip]);
  const queue = [tip];
  for (let head = 0; head < queue.length; head += 1) {
    const info = commitInfo(repo, queue[head] as string);
    if (info === null) continue;
    for (const parent of info.parents) {
      if (!seen.has(parent)) {
        seen.add(parent);
        queue.push(parent);
      }
    }
  }
  return seen;
}

/** The nearest common ancestor of two commits, or null for unrelated histories. */
export function mergeBase(repo: Repo, a: string, b: string): string | null {
  if (a === b) return a;
  const ofA = ancestorsOf(repo, a);
  const seen = new Set<string>([b]);
  const queue = [b];
  for (let head = 0; head < queue.length; head += 1) {
    const oid = queue[head] as string;
    if (ofA.has(oid)) return oid;
    const info = commitInfo(repo, oid);
    if (info === null) continue;
    for (const parent of info.parents) {
      if (!seen.has(parent)) {
        seen.add(parent);
        queue.push(parent);
      }
    }
  }
  return null;
}

/** Up to `limit` commits reachable from `tip`, newest committer date first. */
export function listCommits(repo: Repo, tip: string, limit: number, exclude?: Set<string>): string[] {
  const seen = new Set<string>([tip]);
  const frontier: Array<{ oid: string; when: number; seq: number }> = [];
  let seq = 0;
  const push = (oid: string): void => {
    const info = commitInfo(repo, oid);
    frontier.push({ oid, when: info === null ? 0 : info.committer.when, seq: seq++ });
  };
  push(tip);
  const out: string[] = [];
  while (frontier.length > 0 && out.length < limit) {
    let best = 0;
    for (let i = 1; i < frontier.length; i += 1) {
      const a = frontier[i] as { when: number; seq: number };
      const b = frontier[best] as { when: number; seq: number };
      if (a.when > b.when || (a.when === b.when && a.seq < b.seq)) best = i;
    }
    const top = frontier.splice(best, 1)[0] as { oid: string };
    if (exclude !== undefined && exclude.has(top.oid)) continue;
    out.push(top.oid);
    const info = commitInfo(repo, top.oid);
    if (info === null) continue;
    for (const parent of info.parents) {
      if (!seen.has(parent)) {
        seen.add(parent);
        push(parent);
      }
    }
  }
  return out;
}

// ---------------------------------------------------------------- trees

export interface FlatEntry {
  mode: string;
  oid: string;
}

export function lookupPath(repo: Repo, treeOid: string, path: string): { mode: string; oid: string } | null {
  if (path === '') return { mode: '40000', oid: treeOid };
  const segments = path.split('/');
  let current = treeOid;
  for (let i = 0; i < segments.length; i += 1) {
    const obj = repo.objects.get(current);
    if (obj === undefined || obj.type !== 'tree') return null;
    const entry = treeOf(obj).find((candidate) => candidate.name === segments[i]);
    if (entry === undefined) return null;
    if (i === segments.length - 1) return { mode: entry.mode, oid: entry.oid };
    if (entry.mode !== '40000') return null;
    current = entry.oid;
  }
  return null;
}

export function flattenTree(repo: Repo, treeOid: string, prefix = '', out: Map<string, FlatEntry> = new Map()): Map<string, FlatEntry> {
  const obj = repo.objects.get(treeOid);
  if (obj === undefined || obj.type !== 'tree') return out;
  for (const entry of treeOf(obj)) {
    if (entry.mode === '40000') flattenTree(repo, entry.oid, `${prefix}${entry.name}/`, out);
    else out.set(`${prefix}${entry.name}`, { mode: entry.mode, oid: entry.oid });
  }
  return out;
}

interface TrieNode {
  files: Map<string, FlatEntry>;
  dirs: Map<string, TrieNode>;
}

export function buildTreeFromFlat(repo: Repo, flat: ReadonlyMap<string, FlatEntry>): string {
  const root: TrieNode = { files: new Map(), dirs: new Map() };
  for (const [path, entry] of flat) {
    const segments = path.split('/');
    let node = root;
    for (let i = 0; i < segments.length - 1; i += 1) {
      const name = segments[i] as string;
      let child = node.dirs.get(name);
      if (child === undefined) {
        child = { files: new Map(), dirs: new Map() };
        node.dirs.set(name, child);
      }
      node = child;
    }
    node.files.set(segments[segments.length - 1] as string, entry);
  }
  const write = (node: TrieNode): string => {
    const entries: TreeEntry[] = [];
    for (const [name, entry] of node.files) entries.push({ mode: entry.mode, name, oid: entry.oid });
    for (const [name, child] of node.dirs) entries.push({ mode: '40000', name, oid: write(child) });
    return writeObject(repo, 'tree', buildTree(entries));
  };
  return write(root);
}

export class TreeEditError extends Error {}

/** Set (or, with a null leaf, remove) one path in a tree and return the new tree id, or null when it ends up empty. */
export function updateTree(repo: Repo, treeOid: string | null, segments: readonly string[], index: number, leaf: FlatEntry | null): string | null {
  const existing = treeOid === null ? null : repo.objects.get(treeOid);
  const entries: TreeEntry[] = existing === null || existing === undefined ? [] : treeOf(existing).slice();
  const name = segments[index] as string;
  const at = entries.findIndex((entry) => entry.name === name);
  if (index === segments.length - 1) {
    if (at >= 0 && entries[at]?.mode === '40000') throw new TreeEditError(`${segments.join('/')} is a directory`);
    if (leaf === null) {
      if (at >= 0) entries.splice(at, 1);
    } else if (at >= 0) entries[at] = { mode: leaf.mode, name, oid: leaf.oid };
    else entries.push({ mode: leaf.mode, name, oid: leaf.oid });
  } else {
    if (at >= 0 && entries[at]?.mode !== '40000') throw new TreeEditError(`${segments.slice(0, index + 1).join('/')} is a file`);
    const child = at >= 0 ? (entries[at] as TreeEntry).oid : null;
    const updated = updateTree(repo, child, segments, index + 1, leaf);
    if (updated === null) {
      if (at >= 0) entries.splice(at, 1);
    } else if (at >= 0) entries[at] = { mode: '40000', name, oid: updated };
    else entries.push({ mode: '40000', name, oid: updated });
  }
  if (entries.length === 0) return null;
  return writeObject(repo, 'tree', buildTree(entries));
}

export interface MergeResult {
  tree: string | null;
  conflicts: string[];
}

/** Path-level three-way merge of three trees; a path changed differently on both sides is a conflict. */
export function mergeTrees(repo: Repo, baseTree: string, oursTree: string, theirsTree: string): MergeResult {
  const base = flattenTree(repo, baseTree);
  const ours = flattenTree(repo, oursTree);
  const theirs = flattenTree(repo, theirsTree);
  const paths = new Set<string>([...base.keys(), ...ours.keys(), ...theirs.keys()]);
  const merged = new Map<string, FlatEntry>();
  const conflicts: string[] = [];
  const same = (a: FlatEntry | undefined, b: FlatEntry | undefined): boolean => (a === undefined || b === undefined ? a === b : a.oid === b.oid && a.mode === b.mode);
  for (const path of [...paths].sort()) {
    const b = base.get(path);
    const o = ours.get(path);
    const t = theirs.get(path);
    let pick: FlatEntry | undefined;
    if (same(o, t)) pick = o;
    else if (same(b, o)) pick = t;
    else if (same(b, t)) pick = o;
    else {
      conflicts.push(path);
      continue;
    }
    if (pick !== undefined) merged.set(path, pick);
  }
  if (conflicts.length > 0) return { tree: null, conflicts };
  return { tree: merged.size === 0 ? ensureEmptyTree(repo) : buildTreeFromFlat(repo, merged), conflicts };
}

// ---------------------------------------------------------------- commits

export function createCommit(repo: Repo, input: { tree: string; parents: readonly string[]; message: string; author: Ident; committer?: Ident }): string {
  const data = buildCommit({ tree: input.tree, parents: input.parents, author: input.author, committer: input.committer ?? input.author, message: input.message });
  return writeObject(repo, 'commit', data);
}

// ---------------------------------------------------------------- diffs

export interface FileDiff {
  filename: string;
  status: 'added' | 'removed' | 'modified';
  additions: number;
  deletions: number;
  changes: number;
  sha: string;
  patch?: string;
}

function splitLines(text: string): string[] {
  if (text === '') return [];
  const lines = text.split('\n');
  if (lines[lines.length - 1] === '') lines.pop();
  return lines;
}

type DiffOp = { op: ' ' | '-' | '+'; line: string };

/** A line diff (LCS), or null when the inputs are too large to diff cheaply. */
export function diffLines(a: readonly string[], b: readonly string[]): DiffOp[] | null {
  let start = 0;
  while (start < a.length && start < b.length && a[start] === b[start]) start += 1;
  let endA = a.length;
  let endB = b.length;
  while (endA > start && endB > start && a[endA - 1] === b[endB - 1]) {
    endA -= 1;
    endB -= 1;
  }
  const n = endA - start;
  const m = endB - start;
  if (n * m > 4_000_000) return null;
  const ops: DiffOp[] = [];
  for (let i = 0; i < start; i += 1) ops.push({ op: ' ', line: a[i] as string });
  const width = m + 1;
  const table = new Uint32Array((n + 1) * width);
  for (let i = n - 1; i >= 0; i -= 1) {
    for (let j = m - 1; j >= 0; j -= 1) {
      table[i * width + j] =
        a[start + i] === b[start + j] ? (table[(i + 1) * width + j + 1] as number) + 1 : Math.max(table[(i + 1) * width + j] as number, table[i * width + j + 1] as number);
    }
  }
  let i = 0;
  let j = 0;
  while (i < n && j < m) {
    if (a[start + i] === b[start + j]) {
      ops.push({ op: ' ', line: a[start + i] as string });
      i += 1;
      j += 1;
    } else if ((table[(i + 1) * width + j] as number) >= (table[i * width + j + 1] as number)) {
      ops.push({ op: '-', line: a[start + i] as string });
      i += 1;
    } else {
      ops.push({ op: '+', line: b[start + j] as string });
      j += 1;
    }
  }
  while (i < n) ops.push({ op: '-', line: a[start + i++] as string });
  while (j < m) ops.push({ op: '+', line: b[start + j++] as string });
  for (let k = endA; k < a.length; k += 1) ops.push({ op: ' ', line: a[k] as string });
  return ops;
}

function isBinary(data: Buffer): boolean {
  return data.indexOf(0) >= 0;
}

function describeFile(repo: Repo, filename: string, before: FlatEntry | undefined, after: FlatEntry | undefined): FileDiff {
  const status = before === undefined ? 'added' : after === undefined ? 'removed' : 'modified';
  const oldData = before === undefined ? Buffer.alloc(0) : (repo.objects.get(before.oid)?.data ?? Buffer.alloc(0));
  const newData = after === undefined ? Buffer.alloc(0) : (repo.objects.get(after.oid)?.data ?? Buffer.alloc(0));
  const diff: FileDiff = { filename, status, additions: 0, deletions: 0, changes: 0, sha: (after ?? before)?.oid ?? '' };
  if (isBinary(oldData) || isBinary(newData)) return diff;
  const a = splitLines(oldData.toString('utf8'));
  const b = splitLines(newData.toString('utf8'));
  const ops = diffLines(a, b);
  if (ops === null) {
    diff.additions = b.length;
    diff.deletions = a.length;
  } else {
    let patch = `@@ -${a.length === 0 ? 0 : 1},${a.length} +${b.length === 0 ? 0 : 1},${b.length} @@\n`;
    for (const op of ops) {
      if (op.op === '+') diff.additions += 1;
      else if (op.op === '-') diff.deletions += 1;
      patch += `${op.op}${op.line}\n`;
    }
    if (ops.length > 0) diff.patch = patch.slice(0, -1);
  }
  diff.changes = diff.additions + diff.deletions;
  return diff;
}

/** The files that differ between two trees, in path order. */
export function diffTrees(repo: Repo, baseTree: string | null, headTree: string): FileDiff[] {
  const before = baseTree === null ? new Map<string, FlatEntry>() : flattenTree(repo, baseTree);
  const after = flattenTree(repo, headTree);
  const paths = [...new Set<string>([...before.keys(), ...after.keys()])].sort();
  const out: FileDiff[] = [];
  for (const path of paths) {
    const b = before.get(path);
    const a = after.get(path);
    if (b !== undefined && a !== undefined && b.oid === a.oid && b.mode === a.mode) continue;
    out.push(describeFile(repo, path, b, a));
  }
  return out;
}
