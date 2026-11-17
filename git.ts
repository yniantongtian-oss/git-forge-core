// Git smart HTTP: ref advertisements, upload-pack (protocol v2 with a v0 fallback, shallow and deepen) and
// receive-pack v0 with report-status, side-band-64k and delete-refs. Fast-forward is enforced per ref.
//
// Performance: objects carry their pre-deflated pack entry, advertisements and ls-refs answers are cached per
// refs version, and whole fetch responses for fresh clones are cached by tip (the shallow depth-1 clone of a
// tip is the same bytes for every client).

import { gunzipSync } from 'node:zlib';
import type { IncomingMessage, ServerResponse } from 'node:http';
import { ZERO_OID, isOid, isRefNameValid, parseTag, parseTree, commitOf, treeOf } from './objects.ts';
import type { GitObject } from './objects.ts';
import { PackError, parsePack, writePack } from './pack.ts';
import { addObject, commitInfo, headRefName, isAncestor, peelToCommit, setRef, deleteRef, refsWithPrefix } from './gitops.ts';
import type { Repo } from './store.ts';
import { readBody, sendBuffer } from './util.ts';

const AGENT = 'forge-core/1.0';
const SIDEBAND_CHUNK = 65515;

// ---------------------------------------------------------------- pkt-lines

const FLUSH = Buffer.from('0000');
const DELIM = Buffer.from('0001');

export class GitProtocolError extends Error {}

function pkt(data: string | Buffer): Buffer {
  const body = typeof data === 'string' ? Buffer.from(data, 'utf8') : data;
  const out = Buffer.allocUnsafe(body.length + 4);
  out.write((body.length + 4).toString(16).padStart(4, '0'), 0, 'latin1');
  body.copy(out, 4);
  return out;
}

type Packet = { kind: 'data'; data: Buffer } | { kind: 'flush' } | { kind: 'delim' } | { kind: 'end' };

/** Parse pkt-lines from `start`; with `stopAtFlush` parsing ends after the first flush and `end` is the next offset. */
function parsePackets(buf: Buffer, start: number, stopAtFlush: boolean): { packets: Packet[]; end: number } {
  const packets: Packet[] = [];
  let pos = start;
  while (pos < buf.length) {
    if (pos + 4 > buf.length) throw new GitProtocolError('truncated pkt-line length');
    const head = buf.toString('latin1', pos, pos + 4);
    if (!/^[0-9a-fA-F]{4}$/.test(head)) throw new GitProtocolError(`bad pkt-line length ${JSON.stringify(head)}`);
    const length = parseInt(head, 16);
    if (length === 0) {
      packets.push({ kind: 'flush' });
      pos += 4;
      if (stopAtFlush) return { packets, end: pos };
      continue;
    }
    if (length === 1) {
      packets.push({ kind: 'delim' });
      pos += 4;
      continue;
    }
    if (length === 2) {
      packets.push({ kind: 'end' });
      pos += 4;
      continue;
    }
    if (length < 4 || pos + length > buf.length) throw new GitProtocolError('bad pkt-line length');
    packets.push({ kind: 'data', data: buf.subarray(pos + 4, pos + length) });
    pos += length;
  }
  return { packets, end: pos };
}

function text(packet: Packet): string {
  return packet.kind === 'data' ? packet.data.toString('utf8').replace(/\n$/, '') : '';
}

/** Wrap bytes as side-band-64k band 1 packets. */
function sideband(data: Buffer, band: number): Buffer[] {
  const out: Buffer[] = [];
  for (let offset = 0; offset < data.length; offset += SIDEBAND_CHUNK) {
    const chunk = data.subarray(offset, Math.min(data.length, offset + SIDEBAND_CHUNK));
    const framed = Buffer.allocUnsafe(chunk.length + 5);
    framed.write((chunk.length + 5).toString(16).padStart(4, '0'), 0, 'latin1');
    framed[4] = band;
    chunk.copy(framed, 5);
    out.push(framed);
  }
  return out;
}

// ---------------------------------------------------------------- per-repo caches

function cached(repo: Repo, key: string, build: () => Buffer): Buffer {
  if (repo.gitCacheVersion !== repo.refsVersion) {
    repo.gitCache.clear();
    repo.gitCacheVersion = repo.refsVersion;
  }
  let value = repo.gitCache.get(key);
  if (value === undefined) {
    value = build();
    repo.gitCache.set(key, value);
  }
  return value;
}

/** Whole fetch responses for fresh clones, keyed by what they contain; content-addressed, so valid across repos. */
const cloneCache = new Map<string, Buffer>();
const CLONE_CACHE_LIMIT = 256;

function rememberClone(key: string, value: Buffer): void {
  if (value.length > 16 * 1024 * 1024) return;
  if (cloneCache.size >= CLONE_CACHE_LIMIT) {
    const oldest = cloneCache.keys().next();
    if (!oldest.done) cloneCache.delete(oldest.value);
  }
  cloneCache.set(key, value);
}

// ---------------------------------------------------------------- advertisements

function peeledOf(repo: Repo, oid: string): string | null {
  const obj = repo.objects.get(oid);
  if (obj === undefined || obj.type !== 'tag') return null;
  let current: GitObject | undefined = obj;
  for (let i = 0; i < 16 && current !== undefined && current.type === 'tag'; i += 1) {
    const target: string = parseTag(current.data).object;
    current = repo.objects.get(target);
    if (current !== undefined && current.type !== 'tag') return current.oid;
  }
  return null;
}

const UPLOAD_CAPS = 'side-band-64k thin-pack ofs-delta shallow deepen-since deepen-not deepen-relative no-progress include-tag allow-tip-sha1-in-want allow-reachable-sha1-in-want object-format=sha1';
const RECEIVE_CAPS = 'report-status delete-refs side-band-64k quiet atomic ofs-delta object-format=sha1';

function advertisementV2(): Buffer {
  return Buffer.concat([
    pkt('version 2\n'),
    pkt(`agent=${AGENT}\n`),
    pkt('ls-refs=unborn\n'),
    pkt('fetch=shallow\n'),
    pkt('server-option\n'),
    pkt('object-format=sha1\n'),
    FLUSH,
  ]);
}

const V2_ADVERT = advertisementV2();

function advertisementV0(repo: Repo, service: 'git-upload-pack' | 'git-receive-pack'): Buffer {
  const parts: Buffer[] = [pkt(`# service=${service}\n`), FLUSH];
  const headTarget = headRefName(repo);
  const headOid = repo.refs.get(headTarget);
  const refs = [...refsWithPrefix(repo, 'refs/')];
  let caps = service === 'git-upload-pack' ? UPLOAD_CAPS : RECEIVE_CAPS;
  if (service === 'git-upload-pack') caps = `${headOid === undefined ? '' : `symref=HEAD:${headTarget} `}${caps}`;
  caps += ` agent=${AGENT}`;
  const lines: Array<{ oid: string; name: string }> = [];
  if (service === 'git-upload-pack' && headOid !== undefined) lines.push({ oid: headOid, name: 'HEAD' });
  for (const ref of refs) lines.push(ref);
  if (lines.length === 0) {
    parts.push(pkt(`${ZERO_OID} capabilities^{}\0${caps}\n`));
  } else {
    lines.forEach((line, index) => {
      parts.push(pkt(index === 0 ? `${line.oid} ${line.name}\0${caps}\n` : `${line.oid} ${line.name}\n`));
      if (service === 'git-upload-pack' && line.name !== 'HEAD') {
        const peeled = peeledOf(repo, line.oid);
        if (peeled !== null) parts.push(pkt(`${peeled} ${line.name}^{}\n`));
      }
    });
  }
  parts.push(FLUSH);
  return Buffer.concat(parts);
}

// ---------------------------------------------------------------- ls-refs

function lsRefsResponse(repo: Repo, args: string[]): Buffer {
  const prefixes: string[] = [];
  let peel = false;
  let symrefs = false;
  let unborn = false;
  for (const arg of args) {
    if (arg === 'peel') peel = true;
    else if (arg === 'symrefs') symrefs = true;
    else if (arg === 'unborn') unborn = true;
    else if (arg.startsWith('ref-prefix ')) prefixes.push(arg.slice(11));
  }
  const key = `ls:${peel ? 1 : 0}${symrefs ? 1 : 0}${unborn ? 1 : 0}:${prefixes.join(',')}`;
  return cached(repo, key, () => {
    const matches = (name: string): boolean => prefixes.length === 0 || prefixes.some((prefix) => name.startsWith(prefix));
    const parts: Buffer[] = [];
    const headTarget = headRefName(repo);
    const headOid = repo.refs.get(headTarget);
    if (matches('HEAD')) {
      if (headOid !== undefined) parts.push(pkt(`${headOid} HEAD${symrefs ? ` symref-target:${headTarget}` : ''}\n`));
      else if (unborn) parts.push(pkt(`unborn HEAD symref-target:${headTarget}\n`));
    }
    for (const ref of refsWithPrefix(repo, 'refs/')) {
      if (!matches(ref.name)) continue;
      let line = `${ref.oid} ${ref.name}`;
      if (peel) {
        const peeled = peeledOf(repo, ref.oid);
        if (peeled !== null) line += ` peeled:${peeled}`;
      }
      parts.push(pkt(`${line}\n`));
    }
    parts.push(FLUSH);
    return Buffer.concat(parts);
  });
}

// ---------------------------------------------------------------- fetch planning

interface FetchArgs {
  wants: string[];
  haves: string[];
  shallow: string[];
  depth: number | null;
  since: number | null;
  done: boolean;
}

interface FetchPlan {
  objects: GitObject[];
  shallow: string[];
  unshallow: string[];
}

function markTree(repo: Repo, oid: string, excluded: Set<string>): void {
  if (excluded.has(oid)) return;
  const obj = repo.objects.get(oid);
  if (obj === undefined) return;
  excluded.add(oid);
  if (obj.type !== 'tree') return;
  for (const entry of treeOf(obj)) {
    if (entry.mode === '160000') continue;
    if (entry.mode === '40000') markTree(repo, entry.oid, excluded);
    else excluded.add(entry.oid);
  }
}

/** What a fetch must send: `wants` minus everything the client provably has, cut off at the requested depth. */
function planFetch(repo: Repo, args: FetchArgs): FetchPlan {
  const clientShallow = new Set(args.shallow);
  const depth = args.depth;
  const tags: GitObject[] = [];
  const wantCommits: string[] = [];
  const loose: string[] = [];
  for (const want of args.wants) {
    let oid = want;
    for (let hop = 0; hop < 16; hop += 1) {
      const obj = repo.objects.get(oid);
      if (obj === undefined) throw new GitProtocolError(`upload-pack: not our ref ${want}`);
      if (obj.type === 'tag') {
        if (!tags.includes(obj)) tags.push(obj);
        oid = parseTag(obj.data).object;
        continue;
      }
      if (obj.type === 'commit') {
        if (!wantCommits.includes(oid)) wantCommits.push(oid);
      } else loose.push(oid);
      break;
    }
  }
  const haveCommits: string[] = [];
  for (const have of args.haves) {
    if (!repo.objects.has(have)) continue;
    const commit = peelToCommit(repo, have);
    if (commit !== null && !haveCommits.includes(commit)) haveCommits.push(commit);
  }
  // Commits the client has: its haves and their history, stopping at its own shallow boundary.
  const known = new Set<string>(haveCommits);
  const walk = [...haveCommits];
  for (let head = 0; head < walk.length; head += 1) {
    const oid = walk[head] as string;
    if (clientShallow.has(oid)) continue;
    const info = commitInfo(repo, oid);
    if (info === null) continue;
    for (const parent of info.parents) {
      if (!known.has(parent)) {
        known.add(parent);
        walk.push(parent);
      }
    }
  }
  const level = new Map<string, number>();
  const queue: string[] = [];
  for (const want of wantCommits) {
    level.set(want, 1);
    queue.push(want);
  }
  const sendCommits: string[] = [];
  const edges: string[] = [];
  const shallow: string[] = [];
  const unshallow: string[] = [];
  for (let head = 0; head < queue.length; head += 1) {
    const oid = queue[head] as string;
    const at = level.get(oid) as number;
    const info = commitInfo(repo, oid);
    if (info === null) continue;
    const deepening = depth !== null;
    const reopens = deepening && clientShallow.has(oid) && at < (depth as number);
    if (reopens) unshallow.push(oid);
    let traverse: boolean;
    if (known.has(oid)) {
      edges.push(oid);
      traverse = reopens;
    } else {
      sendCommits.push(oid);
      traverse = true;
      if (deepening && at >= (depth as number)) {
        traverse = false;
        if (info.parents.length > 0) shallow.push(oid);
      } else if (args.since !== null && info.parents.length > 0) {
        const olderParents = info.parents.filter((parent) => (commitInfo(repo, parent)?.committer.when ?? 0) < (args.since as number));
        if (olderParents.length === info.parents.length) {
          traverse = false;
          shallow.push(oid);
        }
      }
    }
    if (traverse) {
      for (const parent of info.parents) {
        if (!level.has(parent)) {
          level.set(parent, at + 1);
          queue.push(parent);
        }
      }
    }
  }
  // Objects the client already holds: the trees of its edge commits.
  const excluded = new Set<string>();
  for (const oid of [...haveCommits, ...edges]) {
    const info = commitInfo(repo, oid);
    if (info !== null) markTree(repo, info.tree, excluded);
  }
  const objects: GitObject[] = [...tags];
  const seen = new Set<string>(tags.map((tag) => tag.oid));
  for (const oid of sendCommits) {
    const obj = repo.objects.get(oid) as GitObject;
    seen.add(oid);
    objects.push(obj);
  }
  const addTree = (oid: string): void => {
    if (seen.has(oid) || excluded.has(oid)) return;
    const obj = repo.objects.get(oid);
    if (obj === undefined) return;
    seen.add(oid);
    objects.push(obj);
    if (obj.type !== 'tree') return;
    for (const entry of treeOf(obj)) {
      if (entry.mode === '160000') continue;
      addTree(entry.oid);
    }
  };
  for (const oid of sendCommits) addTree((commitOf(repo.objects.get(oid) as GitObject)).tree);
  for (const oid of loose) addTree(oid);
  return { objects, shallow, unshallow };
}

function parseFetchArgs(args: string[]): FetchArgs & { wantRefs: string[] } {
  const out: FetchArgs & { wantRefs: string[] } = { wants: [], haves: [], shallow: [], depth: null, since: null, done: false, wantRefs: [] };
  for (const arg of args) {
    if (arg.startsWith('want ')) out.wants.push(arg.slice(5).trim());
    else if (arg.startsWith('want-ref ')) out.wantRefs.push(arg.slice(9).trim());
    else if (arg.startsWith('have ')) out.haves.push(arg.slice(5).trim());
    else if (arg.startsWith('shallow ')) out.shallow.push(arg.slice(8).trim());
    else if (arg.startsWith('deepen ')) out.depth = Math.max(1, parseInt(arg.slice(7), 10) || 1);
    else if (arg.startsWith('deepen-since ')) out.since = parseInt(arg.slice(13), 10);
    else if (arg === 'done') out.done = true;
  }
  return out;
}

function fetchResponseV2(repo: Repo, args: string[]): Buffer {
  const parsed = parseFetchArgs(args);
  const wanted: Array<{ oid: string; ref: string }> = [];
  for (const ref of parsed.wantRefs) {
    const oid = repo.refs.get(ref);
    if (oid === undefined) return pkt(`ERR upload-pack: unknown ref ${ref}\n`);
    wanted.push({ oid, ref });
    parsed.wants.push(oid);
  }
  for (const want of parsed.wants) if (!isOid(want)) return pkt('ERR upload-pack: protocol error: expected want <oid>\n');
  const cacheable = parsed.haves.length === 0 && parsed.shallow.length === 0 && parsed.since === null && parsed.done && wanted.length === 0;
  const cacheKey = cacheable ? `v2:${[...new Set(parsed.wants)].sort().join(',')}:${parsed.depth ?? 0}` : '';
  if (cacheable) {
    const hit = cloneCache.get(cacheKey);
    if (hit !== undefined) return hit;
  }
  const parts: Buffer[] = [];
  if (!parsed.done) {
    const common = parsed.haves.filter((have) => repo.objects.has(have));
    parts.push(pkt('acknowledgments\n'));
    if (common.length === 0) {
      parts.push(pkt('NAK\n'), FLUSH);
      return Buffer.concat(parts);
    }
    for (const oid of common) parts.push(pkt(`ACK ${oid}\n`));
    parts.push(pkt('ready\n'), DELIM);
  }
  let plan: FetchPlan;
  try {
    plan = planFetch(repo, parsed);
  } catch (err) {
    if (err instanceof GitProtocolError) return pkt(`ERR ${err.message}\n`);
    throw err;
  }
  if (parsed.depth !== null || parsed.since !== null) {
    parts.push(pkt('shallow-info\n'));
    for (const oid of plan.shallow) parts.push(pkt(`shallow ${oid}\n`));
    for (const oid of plan.unshallow) parts.push(pkt(`unshallow ${oid}\n`));
    parts.push(DELIM);
  }
  if (wanted.length > 0) {
    parts.push(pkt('wanted-refs\n'));
    for (const item of wanted) parts.push(pkt(`${item.oid} ${item.ref}\n`));
    parts.push(DELIM);
  }
  parts.push(pkt('packfile\n'), ...sideband(writePack(plan.objects), 1), FLUSH);
  const response = Buffer.concat(parts);
  if (cacheable) rememberClone(cacheKey, response);
  return response;
}

function fetchResponseV0(repo: Repo, packets: Packet[]): Buffer {
  const wants: string[] = [];
  const haves: string[] = [];
  const shallow: string[] = [];
  let depth: number | null = null;
  let since: number | null = null;
  let done = false;
  let flushes = 0;
  for (const packet of packets) {
    if (packet.kind === 'flush') {
      flushes += 1;
      continue;
    }
    const line = text(packet);
    if (line.startsWith('want ')) wants.push(line.slice(5, 45));
    else if (line.startsWith('have ')) haves.push(line.slice(5, 45));
    else if (line.startsWith('shallow ')) shallow.push(line.slice(8).trim());
    else if (line.startsWith('deepen ')) depth = Math.max(1, parseInt(line.slice(7), 10) || 1);
    else if (line.startsWith('deepen-since ')) since = parseInt(line.slice(13), 10);
    else if (line === 'done') done = true;
  }
  void flushes;
  if (wants.length === 0) return FLUSH;
  for (const want of wants) if (!isOid(want)) return pkt('ERR upload-pack: protocol error: expected want <oid>\n');
  let plan: FetchPlan;
  try {
    plan = planFetch(repo, { wants, haves, shallow, depth, since, done });
  } catch (err) {
    if (err instanceof GitProtocolError) return pkt(`ERR ${err.message}\n`);
    throw err;
  }
  const parts: Buffer[] = [];
  if (depth !== null || since !== null) {
    for (const oid of plan.shallow) parts.push(pkt(`shallow ${oid}\n`));
    for (const oid of plan.unshallow) parts.push(pkt(`unshallow ${oid}\n`));
    parts.push(FLUSH);
  }
  const common = haves.find((have) => repo.objects.has(have));
  parts.push(pkt(common === undefined ? 'NAK\n' : `ACK ${common}\n`));
  if (!done) return Buffer.concat(parts);
  parts.push(...sideband(writePack(plan.objects), 1), FLUSH);
  return Buffer.concat(parts);
}

// ---------------------------------------------------------------- receive-pack

interface Command {
  oldOid: string;
  newOid: string;
  ref: string;
}

/** Every object a new object points at must exist; the first missing link is described, or null. */
function missingLink(repo: Repo, objects: readonly GitObject[]): string | null {
  for (const obj of objects) {
    if (obj.type === 'commit') {
      const info = commitOf(obj);
      if (!repo.objects.has(info.tree)) return `missing tree ${info.tree}`;
      for (const parent of info.parents) if (!repo.objects.has(parent)) return `missing parent ${parent}`;
    } else if (obj.type === 'tree') {
      for (const entry of parseTree(obj.data)) {
        if (entry.mode !== '160000' && !repo.objects.has(entry.oid)) return `missing ${entry.oid}`;
      }
    } else if (obj.type === 'tag') {
      if (!repo.objects.has(parseTag(obj.data).object)) return 'missing tag target';
    }
  }
  return null;
}

function isFastForward(repo: Repo, from: string, to: string): boolean {
  const info = commitInfo(repo, to);
  if (info !== null && info.parents.includes(from)) return true;
  return isAncestor(repo, from, to);
}

function receivePack(repo: Repo, body: Buffer): Buffer {
  const { packets, end } = parsePackets(body, 0, true);
  const commands: Command[] = [];
  let caps = new Set<string>();
  for (const packet of packets) {
    if (packet.kind !== 'data') continue;
    let line = packet.data.toString('utf8').replace(/\n$/, '');
    const nul = line.indexOf('\0');
    if (nul >= 0) {
      caps = new Set(line.slice(nul + 1).split(' ').filter(Boolean));
      line = line.slice(0, nul);
    }
    if (line.startsWith('shallow ') || line.startsWith('push-cert')) continue;
    const match = /^([0-9a-f]{40}) ([0-9a-f]{40}) (.+)$/.exec(line);
    if (match === null) throw new GitProtocolError(`bad command ${JSON.stringify(line.slice(0, 80))}`);
    commands.push({ oldOid: match[1] as string, newOid: match[2] as string, ref: match[3] as string });
  }
  const wantsReport = caps.has('report-status') || caps.has('report-status-v2');
  const useSideband = caps.has('side-band-64k');
  const results: string[] = [];
  let unpack = 'ok';
  const packData = body.subarray(end);
  let added: GitObject[] = [];
  if (commands.some((command) => command.newOid !== ZERO_OID)) {
    try {
      if (packData.length > 0) {
        added = parsePack(packData, (oid) => repo.objects.get(oid));
        for (const obj of added) addObject(repo, obj);
      }
    } catch (err) {
      unpack = err instanceof PackError ? err.message : `unpack error: ${err instanceof Error ? err.message : String(err)}`;
    }
  }
  const linkProblem = unpack === 'ok' ? missingLink(repo, added) : null;
  const atomic = caps.has('atomic');
  const verdicts: Array<string | null> = commands.map((command) => {
    if (unpack !== 'ok') return 'unpacker error';
    if (!isRefNameValid(command.ref)) return 'funny refname';
    const current = repo.refs.get(command.ref) ?? ZERO_OID;
    if (command.newOid === ZERO_OID) {
      if (current === ZERO_OID) return 'failed to delete';
      if (command.ref === headRefName(repo)) return 'deletion of the current branch prohibited';
      if (command.oldOid !== current && command.oldOid !== ZERO_OID) return 'failed to update ref';
      return null;
    }
    if (command.oldOid !== current) return 'failed to update ref';
    const target = repo.objects.get(command.newOid);
    if (target === undefined || linkProblem !== null) return 'missing necessary objects';
    if (command.ref.startsWith('refs/heads/') && target.type !== 'commit') return 'not a commit';
    if (current !== ZERO_OID) {
      const existing = repo.objects.get(current);
      if (existing === undefined || existing.type !== 'commit' || target.type !== 'commit' || !isFastForward(repo, current, command.newOid)) return 'non-fast-forward';
    }
    return null;
  });
  if (atomic && verdicts.some((verdict) => verdict !== null)) {
    for (let i = 0; i < verdicts.length; i += 1) if (verdicts[i] === null) verdicts[i] = 'atomic push failed';
  }
  const hadBranches = refsWithPrefix(repo, 'refs/heads/').length > 0;
  commands.forEach((command, index) => {
    if (verdicts[index] !== null) return;
    if (command.newOid === ZERO_OID) deleteRef(repo, command.ref);
    else {
      setRef(repo, command.ref, command.newOid);
      if (!hadBranches && command.ref.startsWith('refs/heads/') && !repo.refs.has(headRefName(repo))) {
        repo.defaultBranch = command.ref.slice('refs/heads/'.length);
      }
    }
  });
  if (!wantsReport) return useSideband ? FLUSH : Buffer.alloc(0);
  results.push(`unpack ${unpack}\n`);
  commands.forEach((command, index) => {
    const verdict = verdicts[index];
    results.push(verdict === null ? `ok ${command.ref}\n` : `ng ${command.ref} ${verdict}\n`);
  });
  const report = Buffer.concat([...results.map((line) => pkt(line)), FLUSH]);
  return useSideband ? Buffer.concat([...sideband(report, 1), FLUSH]) : report;
}

// ---------------------------------------------------------------- the HTTP surface

function wantsV2(req: IncomingMessage): boolean {
  const header = req.headers['git-protocol'];
  return typeof header === 'string' && header.split(':').includes('version=2');
}

async function requestBody(req: IncomingMessage): Promise<Buffer> {
  const body = await readBody(req);
  const encoding = req.headers['content-encoding'];
  if (encoding === 'gzip' || encoding === 'x-gzip') return gunzipSync(body);
  return body;
}

const NO_CACHE = { 'cache-control': 'no-cache, max-age=0, must-revalidate', pragma: 'no-cache', expires: 'Fri, 01 Jan 1980 00:00:00 GMT' };

function plain(res: ServerResponse, status: number, message: string): void {
  sendBuffer(res, status, 'text/plain; charset=utf-8', Buffer.from(`${message}\n`, 'utf8'), NO_CACHE);
}

/** Handle one request under /{owner}/{repo}.git/ (the repository is already resolved). */
export async function handleGit(req: IncomingMessage, res: ServerResponse, repo: Repo, rest: string, search: string): Promise<void> {
  const method = req.method ?? 'GET';
  if (rest === '/info/refs' && method === 'GET') {
    const match = /(?:^|&)service=(git-upload-pack|git-receive-pack)(?:&|$)/.exec(search);
    if (match === null) {
      plain(res, 403, 'Dumb HTTP transport is not supported; use the smart protocol (?service=git-upload-pack)');
      return;
    }
    const service = match[1] as 'git-upload-pack' | 'git-receive-pack';
    if (service === 'git-upload-pack' && wantsV2(req)) {
      sendBuffer(res, 200, 'application/x-git-upload-pack-advertisement', V2_ADVERT, NO_CACHE);
      return;
    }
    const body = cached(repo, `adv:${service}`, () => advertisementV0(repo, service));
    sendBuffer(res, 200, `application/x-${service}-advertisement`, body, NO_CACHE);
    return;
  }
  if (rest === '/git-upload-pack' && method === 'POST') {
    let body: Buffer;
    try {
      body = await requestBody(req);
    } catch {
      plain(res, 400, 'Bad request');
      return;
    }
    let parsed: { packets: Packet[] };
    try {
      parsed = parsePackets(body, 0, false);
    } catch (err) {
      plain(res, 400, err instanceof Error ? err.message : 'Bad request');
      return;
    }
    let output: Buffer;
    if (wantsV2(req)) {
      const header: string[] = [];
      const args: string[] = [];
      let section = 0;
      for (const packet of parsed.packets) {
        if (packet.kind === 'delim') section = 1;
        else if (packet.kind === 'flush') break;
        else if (packet.kind === 'data') (section === 0 ? header : args).push(text(packet));
      }
      const command = header.find((line) => line.startsWith('command='));
      if (command === 'command=ls-refs') output = lsRefsResponse(repo, args);
      else if (command === 'command=fetch') output = fetchResponseV2(repo, args);
      else output = pkt(`ERR unknown command ${command ?? '(none)'}\n`);
    } else {
      output = fetchResponseV0(repo, parsed.packets);
    }
    sendBuffer(res, 200, 'application/x-git-upload-pack-result', output, NO_CACHE);
    return;
  }
  if (rest === '/git-receive-pack' && method === 'POST') {
    let body: Buffer;
    try {
      body = await requestBody(req);
    } catch {
      plain(res, 400, 'Bad request');
      return;
    }
    let output: Buffer;
    try {
      output = receivePack(repo, body);
    } catch (err) {
      if (err instanceof GitProtocolError) {
        plain(res, 400, err.message);
        return;
      }
      throw err;
    }
    sendBuffer(res, 200, 'application/x-git-receive-pack-result', output, NO_CACHE);
    return;
  }
  plain(res, 404, 'Not Found');
}
