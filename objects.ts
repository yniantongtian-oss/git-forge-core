// Git object primitives: hashing, trees, commits, tags and the cached pack entry every stored object
// carries. Everything is in memory and built on node:crypto and node:zlib only.

import { createHash } from 'node:crypto';
import { deflateSync } from 'node:zlib';

export const ZERO_OID = '0000000000000000000000000000000000000000';
export const EMPTY_TREE_OID = '4b825dc642cb6eb9a060e54bf8d69288fbee4904';

export type ObjType = 'commit' | 'tree' | 'blob' | 'tag';

export const TYPE_CODES: Readonly<Record<ObjType, number>> = { commit: 1, tree: 2, blob: 3, tag: 4 };
export const CODE_TYPES: ReadonlyArray<ObjType | undefined> = [undefined, 'commit', 'tree', 'blob', 'tag'];

export interface TreeEntry {
  mode: string;
  name: string;
  oid: string;
}

export interface Ident {
  name: string;
  email: string;
  when: number;
  tz: string;
}

export interface CommitInfo {
  tree: string;
  parents: string[];
  author: Ident;
  committer: Ident;
  /** The message exactly as stored in the object. */
  message: string;
}

export interface TagInfo {
  object: string;
  type: string;
  tag: string;
  tagger: string;
  message: string;
}

export interface GitObject {
  oid: string;
  type: ObjType;
  data: Buffer;
  /** The pack entry (type/size header plus zlib stream), built once and reused by every pack that carries it. */
  entry: Buffer | null;
  commit?: CommitInfo;
  tree?: TreeEntry[];
}

export function hashObject(type: ObjType, data: Buffer): string {
  return createHash('sha1').update(`${type} ${data.length}\0`).update(data).digest('hex');
}

export function makeObject(type: ObjType, data: Buffer, entry: Buffer | null = null): GitObject {
  return { oid: hashObject(type, data), type, data, entry };
}

export function isOid(value: unknown): value is string {
  return typeof value === 'string' && value.length === 40 && /^[0-9a-f]{40}$/.test(value);
}

/** The pack entry header: type in bits 6-4 of the first byte, then the size as a little-endian varint. */
export function entryHeader(typeCode: number, size: number): Buffer {
  const bytes: number[] = [];
  let first = (typeCode << 4) | (size & 15);
  let rest = Math.floor(size / 16);
  while (rest > 0) {
    bytes.push(first | 0x80);
    first = rest & 0x7f;
    rest = Math.floor(rest / 128);
  }
  bytes.push(first);
  return Buffer.from(bytes);
}

/** The pack entry for an object, deflated once and cached on the object. */
export function packEntryOf(obj: GitObject): Buffer {
  if (obj.entry !== null) return obj.entry;
  const body = deflateSync(obj.data, { level: 1 });
  obj.entry = Buffer.concat([entryHeader(TYPE_CODES[obj.type], obj.data.length), body]);
  return obj.entry;
}

// ---------------------------------------------------------------- trees

export function parseTree(data: Buffer): TreeEntry[] {
  const entries: TreeEntry[] = [];
  let pos = 0;
  while (pos < data.length) {
    const space = data.indexOf(0x20, pos);
    const nul = data.indexOf(0x00, space + 1);
    if (space < 0 || nul < 0 || nul + 21 > data.length) throw new Error('malformed tree object');
    entries.push({
      mode: data.toString('latin1', pos, space),
      name: data.toString('utf8', space + 1, nul),
      oid: data.toString('hex', nul + 1, nul + 21),
    });
    pos = nul + 21;
  }
  return entries;
}

function treeKey(entry: TreeEntry): Buffer {
  return Buffer.from(entry.mode === '40000' ? `${entry.name}/` : entry.name, 'utf8');
}

export function buildTree(entries: readonly TreeEntry[]): Buffer {
  const keyed = entries.map((entry) => ({ entry, key: treeKey(entry) }));
  keyed.sort((a, b) => Buffer.compare(a.key, b.key));
  const parts: Buffer[] = [];
  for (const { entry } of keyed) {
    parts.push(Buffer.from(`${entry.mode} ${entry.name}\0`, 'utf8'), Buffer.from(entry.oid, 'hex'));
  }
  return Buffer.concat(parts);
}

// ---------------------------------------------------------------- commits

const IDENT_RE = /^(.*) <(.*)> (-?\d+) ([+-]\d{4})$/;

export function parseIdent(line: string): Ident {
  const match = IDENT_RE.exec(line);
  if (match === null) return { name: line, email: '', when: 0, tz: '+0000' };
  return { name: match[1] ?? '', email: match[2] ?? '', when: Number(match[3]), tz: match[4] ?? '+0000' };
}

export function formatIdent(ident: Ident): string {
  return `${ident.name} <${ident.email}> ${ident.when} ${ident.tz}`;
}

export function parseCommit(data: Buffer): CommitInfo {
  const text = data.toString('utf8');
  const split = text.indexOf('\n\n');
  const head = split < 0 ? text : text.slice(0, split);
  const message = split < 0 ? '' : text.slice(split + 2);
  let tree = '';
  const parents: string[] = [];
  let author: Ident = { name: '', email: '', when: 0, tz: '+0000' };
  let committer: Ident = author;
  for (const line of head.split('\n')) {
    if (line.startsWith('tree ')) tree = line.slice(5);
    else if (line.startsWith('parent ')) parents.push(line.slice(7));
    else if (line.startsWith('author ')) author = parseIdent(line.slice(7));
    else if (line.startsWith('committer ')) committer = parseIdent(line.slice(10));
  }
  return { tree, parents, author, committer, message };
}

export function buildCommit(input: { tree: string; parents: readonly string[]; author: Ident; committer: Ident; message: string }): Buffer {
  let text = `tree ${input.tree}\n`;
  for (const parent of input.parents) text += `parent ${parent}\n`;
  text += `author ${formatIdent(input.author)}\ncommitter ${formatIdent(input.committer)}\n\n${input.message}`;
  return Buffer.from(text, 'utf8');
}

export function parseTag(data: Buffer): TagInfo {
  const text = data.toString('utf8');
  const split = text.indexOf('\n\n');
  const head = split < 0 ? text : text.slice(0, split);
  const info: TagInfo = { object: '', type: '', tag: '', tagger: '', message: split < 0 ? '' : text.slice(split + 2) };
  for (const line of head.split('\n')) {
    if (line.startsWith('object ')) info.object = line.slice(7);
    else if (line.startsWith('type ')) info.type = line.slice(5);
    else if (line.startsWith('tag ')) info.tag = line.slice(4);
    else if (line.startsWith('tagger ')) info.tagger = line.slice(7);
  }
  return info;
}

/** The parsed commit of a stored commit object, cached on the object. */
export function commitOf(obj: GitObject): CommitInfo {
  if (obj.commit === undefined) obj.commit = parseCommit(obj.data);
  return obj.commit;
}

/** The parsed entries of a stored tree object, cached on the object. */
export function treeOf(obj: GitObject): TreeEntry[] {
  if (obj.tree === undefined) obj.tree = parseTree(obj.data);
  return obj.tree;
}

export function isRefNameValid(name: string): boolean {
  if (!name.startsWith('refs/') || name.length > 250) return false;
  if (name.includes('..') || name.includes('//') || name.endsWith('/') || name.endsWith('.') || name.endsWith('.lock')) return false;
  if (name.includes('@{') || name.includes('\\')) return false;
  // eslint-disable-next-line no-control-regex
  return !/[\x00-\x20\x7f~^:?*[]/.test(name);
}
