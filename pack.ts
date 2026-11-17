// Packfile reader and writer: all four object types, OFS_DELTA and REF_DELTA (thin packs resolve against
// the repository's own objects), trailer verification, and pack assembly from cached pre-deflated entries.

import { createHash } from 'node:crypto';
import { inflateSync } from 'node:zlib';
import { CODE_TYPES, makeObject, packEntryOf } from './objects.ts';
import type { GitObject, ObjType } from './objects.ts';

export class PackError extends Error {}

/** Apply a git delta (copy/insert instructions) to its base. */
export function applyDelta(base: Buffer, delta: Buffer): Buffer {
  let pos = 0;
  const readSize = (): number => {
    let result = 0;
    let shift = 0;
    let byte: number;
    do {
      if (pos >= delta.length) throw new PackError('truncated delta header');
      byte = delta[pos++] as number;
      result += (byte & 0x7f) * 2 ** shift;
      shift += 7;
    } while ((byte & 0x80) !== 0);
    return result;
  };
  const baseSize = readSize();
  if (baseSize !== base.length) throw new PackError('delta base size mismatch');
  const resultSize = readSize();
  const out = Buffer.allocUnsafe(resultSize);
  let written = 0;
  while (pos < delta.length) {
    const op = delta[pos++] as number;
    if ((op & 0x80) !== 0) {
      let offset = 0;
      let size = 0;
      if ((op & 0x01) !== 0) offset = delta[pos++] as number;
      if ((op & 0x02) !== 0) offset += (delta[pos++] as number) * 0x100;
      if ((op & 0x04) !== 0) offset += (delta[pos++] as number) * 0x10000;
      if ((op & 0x08) !== 0) offset += (delta[pos++] as number) * 0x1000000;
      if ((op & 0x10) !== 0) size = delta[pos++] as number;
      if ((op & 0x20) !== 0) size += (delta[pos++] as number) * 0x100;
      if ((op & 0x40) !== 0) size += (delta[pos++] as number) * 0x10000;
      if (size === 0) size = 0x10000;
      if (offset + size > base.length || written + size > resultSize) throw new PackError('delta copy out of range');
      base.copy(out, written, offset, offset + size);
      written += size;
    } else if (op !== 0) {
      if (pos + op > delta.length || written + op > resultSize) throw new PackError('delta insert out of range');
      delta.copy(out, written, pos, pos + op);
      pos += op;
      written += op;
    } else {
      throw new PackError('delta opcode 0');
    }
  }
  if (written !== resultSize) throw new PackError('delta result size mismatch');
  return out;
}

interface PendingRefDelta {
  baseOid: string;
  delta: Buffer;
  start: number;
}

/**
 * Parse a whole pack. `external` supplies bases for thin-pack REF_DELTAs. Whole (non-delta) entries keep a copy
 * of their original bytes as the object's pack entry, so serving the object later costs no deflate.
 */
export function parsePack(buf: Buffer, external: (oid: string) => GitObject | undefined): GitObject[] {
  if (buf.length < 32 || buf.toString('latin1', 0, 4) !== 'PACK') throw new PackError('bad pack signature');
  const version = buf.readUInt32BE(4);
  if (version !== 2 && version !== 3) throw new PackError(`unsupported pack version ${version}`);
  const count = buf.readUInt32BE(8);
  const end = buf.length - 20;
  const trailer = createHash('sha1').update(buf.subarray(0, end)).digest();
  if (!trailer.equals(buf.subarray(end))) throw new PackError('pack checksum mismatch');

  const byOffset = new Map<number, { type: ObjType; data: Buffer }>();
  const inPack = new Map<string, GitObject>();
  const objects: GitObject[] = [];
  const pending: PendingRefDelta[] = [];
  const remember = (start: number, obj: GitObject): void => {
    byOffset.set(start, { type: obj.type, data: obj.data });
    inPack.set(obj.oid, obj);
    objects.push(obj);
  };

  let pos = 12;
  for (let index = 0; index < count; index += 1) {
    const start = pos;
    if (pos >= end) throw new PackError('pack ends before its declared object count');
    let byte = buf[pos++] as number;
    const typeCode = (byte >> 4) & 7;
    let size = byte & 15;
    let shift = 4;
    while ((byte & 0x80) !== 0) {
      if (pos >= end) throw new PackError('truncated entry header');
      byte = buf[pos++] as number;
      size += (byte & 0x7f) * 2 ** shift;
      shift += 7;
    }
    let baseOffset = -1;
    let baseOid = '';
    if (typeCode === 6) {
      if (pos >= end) throw new PackError('truncated ofs-delta header');
      let b = buf[pos++] as number;
      let distance = b & 0x7f;
      while ((b & 0x80) !== 0) {
        if (pos >= end) throw new PackError('truncated ofs-delta offset');
        b = buf[pos++] as number;
        distance = (distance + 1) * 128 + (b & 0x7f);
      }
      baseOffset = start - distance;
    } else if (typeCode === 7) {
      if (pos + 20 > end) throw new PackError('truncated ref-delta header');
      baseOid = buf.toString('hex', pos, pos + 20);
      pos += 20;
    } else if (typeCode < 1 || typeCode > 4) {
      throw new PackError(`invalid object type ${typeCode}`);
    }
    let inflated: { buffer: Buffer; engine: { bytesWritten: number } };
    try {
      inflated = inflateSync(buf.subarray(pos, end), { info: true, maxOutputLength: Math.max(size, 1) }) as unknown as { buffer: Buffer; engine: { bytesWritten: number } };
    } catch (err) {
      throw new PackError(`bad zlib stream at ${start}: ${err instanceof Error ? err.message : String(err)}`);
    }
    pos += inflated.engine.bytesWritten;
    const data = inflated.buffer;
    if (data.length !== size) throw new PackError(`entry at ${start} inflates to ${data.length} bytes, header says ${size}`);
    if (typeCode <= 4) {
      remember(start, makeObject(CODE_TYPES[typeCode] as ObjType, data, Buffer.from(buf.subarray(start, pos))));
    } else if (typeCode === 6) {
      const base = byOffset.get(baseOffset);
      if (base === undefined) throw new PackError(`ofs-delta at ${start} names an unknown base offset`);
      remember(start, makeObject(base.type, applyDelta(base.data, data)));
    } else {
      const base = inPack.get(baseOid) ?? external(baseOid);
      if (base === undefined) pending.push({ baseOid, delta: data, start });
      else remember(start, makeObject(base.type, applyDelta(base.data, data)));
    }
  }
  if (pos !== end) throw new PackError('trailing bytes between the last entry and the checksum');

  let queue = pending;
  while (queue.length > 0) {
    const next: PendingRefDelta[] = [];
    for (const item of queue) {
      const base = inPack.get(item.baseOid) ?? external(item.baseOid);
      if (base === undefined) next.push(item);
      else remember(item.start, makeObject(base.type, applyDelta(base.data, item.delta)));
    }
    if (next.length === queue.length) throw new PackError('a ref-delta base is missing from the pack and the repository');
    queue = next;
  }
  return objects;
}

/** Assemble a pack from objects, reusing each object's cached pack entry. */
export function writePack(objects: readonly GitObject[]): Buffer {
  const entries: Buffer[] = new Array(objects.length);
  let total = 32;
  for (let i = 0; i < objects.length; i += 1) {
    const entry = packEntryOf(objects[i] as GitObject);
    entries[i] = entry;
    total += entry.length;
  }
  const out = Buffer.allocUnsafe(total);
  out.write('PACK', 0, 'latin1');
  out.writeUInt32BE(2, 4);
  out.writeUInt32BE(objects.length, 8);
  let pos = 12;
  for (const entry of entries) {
    entry.copy(out, pos);
    pos += entry.length;
  }
  createHash('sha1').update(out.subarray(0, pos)).digest().copy(out, pos);
  return out;
}
