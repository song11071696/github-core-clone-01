// git.ts — the git smart-HTTP protocol: info/refs advertisements (v0 and v2),
// ls-refs, fetch (v2 and v0) and receive-pack.
//
// The object-selection rules for fetch follow the kit's conformance checks:
//   * a want's reachable closure is walked by parent edges, counting the distance
//     from the nearest want; a `deepen` limit keeps only commits closer than it;
//   * a non-shallow have contributes its whole closure (never re-sent);
//   * a shallow have contributes only itself, and the walk continues past it;
//   * new shallow boundaries are the included commits exactly depth-1 deep that
//     still have a parent; unshallow boundaries are previous boundaries that are
//     wanted or included and are not new boundaries;
//   * the pack is always a plain (undelted) pack, version 2, with a correct
//     SHA-1 trailer; an empty pack is still sent (32 bytes) so the client always
//     sees at least one sideband packet.

import {
  ZERO_OID,
  decodePktLinesAll,
  decodePktLinesUntilFlush,
  isObjectId,
  parseCommit,
  parseTag,
  pktLine,
  pktLinesToStrings,
  PKT_DELIM,
  PKT_FLUSH,
  readPack,
  writePack,
  type GitObject,
  type Pkt,
} from './gitpack.ts';
import { isDescendant, type Repo, type Store } from './store.ts';
import { sendBuffer, sendJson, type Ctx } from './http.ts';
import type { ServerResponse } from 'node:http';

const AGENT = 'github-core-clone-01/1.0';
const SIDEBAND_CHUNK = 32768;

interface FetchArgs {
  wants: string[];
  haves: string[];
  prevShallow: string[];
  depth: number | undefined;
}

interface FetchSelection {
  objects: Map<string, GitObject>;
  acks: string[];
  shallow: string[];
  unshallow: string[];
}

/** Split decoded packets into sections at flush packets, returning the text lines of each. */
function sectionsOf(packets: readonly Pkt[]): string[][] {
  const sections: string[][] = [[]];
  for (const packet of packets) {
    if (packet.kind === 'flush') sections.push([]);
    else if (packet.kind === 'data' && packet.payload) sections[sections.length - 1].push(packet.payload.toString('utf8'));
  }
  return sections;
}

/** The annotated-tag target a ref points at, when the ref points at a tag object. */
function peeledTarget(repo: Repo, oid: string): string | undefined {
  const object = repo.objects.get(oid);
  if (!object || object.type !== 'tag') return undefined;
  return parseTag(object.data).object;
}

/** Refs in git's sorted order. */
function sortedRefs(repo: Repo): string[] {
  return [...repo.refs.keys()].sort();
}

function v0Advertisement(repo: Repo, service: 'git-upload-pack' | 'git-receive-pack'): Buffer {
  const caps =
    service === 'git-upload-pack'
      ? `symref=HEAD:refs/heads/${repo.defaultBranch} side-band-64k thin-pack ofs-delta shallow multi_ack agent=${AGENT}`
      : `symref=HEAD:refs/heads/${repo.defaultBranch} report-status side-band-64k delete-refs ofs-delta agent=${AGENT}`;
  const tip = repo.refs.get(`refs/heads/${repo.defaultBranch}`);
  const parts: Buffer[] = [pktLine(`# service=${service}\n`)];
  if (tip) parts.push(pktLine(`${tip} HEAD\u0000${caps}\n`));
  else parts.push(pktLine(`${ZERO_OID} capabilities^{}\u0000${caps}\n`));
  for (const ref of sortedRefs(repo)) {
    const oid = repo.refs.get(ref)!;
    parts.push(pktLine(`${oid} ${ref}\n`));
    const peeled = peeledTarget(repo, oid);
    if (peeled) parts.push(pktLine(`${peeled} ${ref}^{}\n`));
  }
  parts.push(PKT_FLUSH);
  return Buffer.concat(parts);
}

function v2Advertisement(): Buffer {
  return Buffer.concat([
    pktLine('# service=git-upload-pack\n'),
    pktLine('version 2\n'),
    pktLine(`agent=${AGENT}\n`),
    pktLine('ls-refs\n'),
    pktLine('fetch=shallow filter\n'),
    pktLine('object-format=sha1\n'),
    PKT_FLUSH,
  ]);
}

function handleInfoRefs(store: Store, ctx: Ctx, res: ServerResponse, owner: string, repoName: string): boolean {
  const service = ctx.query.get('service');
  if (service !== 'git-upload-pack' && service !== 'git-receive-pack') {
    sendJson(res, 404, { message: 'Not Found' }, ctx.requestId);
    return true;
  }
  const repo = store.getRepo(owner, repoName);
  if (!repo) {
    sendJson(res, 404, { message: 'Not Found' }, ctx.requestId);
    return true;
  }
  const wantsV2 = service === 'git-upload-pack' && ctx.headers['git-protocol'] === 'version=2';
  const body = wantsV2 ? v2Advertisement() : v0Advertisement(repo, service);
  sendBuffer(res, 200, `application/x-${service}-advertisement`, body, ctx.requestId);
  return true;
}

function lsRefs(repo: Repo, args: string[]): Buffer {
  const clean = args.map((arg) => arg.replace(/\n$/, ''));
  const wantsPeel = clean.includes('peel');
  const wantsSymrefs = clean.includes('symrefs');
  const prefixes: string[] = [];
  for (const arg of clean) if (arg.startsWith('ref-prefix ')) prefixes.push(arg.slice('ref-prefix '.length));
  const parts: Buffer[] = [];
  const emit = (oid: string, ref: string): void => {
    let line = `${oid} ${ref}`;
    if (wantsPeel) {
      const peeled = peeledTarget(repo, oid);
      if (peeled) line += ` peeled:${peeled}`;
    }
    if (wantsSymrefs && ref === 'HEAD') line += ` symref-target:refs/heads/${repo.defaultBranch}`;
    parts.push(pktLine(`${line}\n`));
  };
  const matches = (ref: string): boolean => prefixes.length === 0 || prefixes.some((prefix) => ref.startsWith(prefix));
  if (prefixes.length === 0) {
    const tip = repo.refs.get(`refs/heads/${repo.defaultBranch}`);
    if (tip) emit(tip, 'HEAD');
  }
  for (const ref of sortedRefs(repo)) {
    if (!matches(ref)) continue;
    emit(repo.refs.get(ref)!, ref);
  }
  parts.push(PKT_FLUSH);
  return Buffer.concat(parts);
}

function parseFetchArgs(lines: string[], prefix: string): FetchArgs {
  const args: FetchArgs = { wants: [], haves: [], prevShallow: [], depth: undefined };
  for (const line of lines) {
    const text = line.replace(/\n$/, '');
    if (text.startsWith('want ')) args.wants.push(text.slice(5).split(' ')[0]);
    else if (text.startsWith('have ')) args.haves.push(text.slice(5).split(' ')[0]);
    else if (text.startsWith('shallow ')) args.prevShallow.push(text.slice(8).trim());
    else if (text.startsWith('deepen ')) {
      const value = Number.parseInt(text.slice(7).trim(), 10);
      if (Number.isFinite(value) && value > 0) args.depth = value;
    }
  }
  void prefix;
  return args;
}

function selectFetch(store: Store, repo: Repo, args: FetchArgs): FetchSelection {
  const lookup = (oid: string): GitObject | undefined => repo.objects.get(oid);
  const heldShallow = new Set(args.prevShallow.filter((oid) => isObjectId(oid) && repo.objects.has(oid)));
  const heldNonShallow = new Set<string>();
  for (const have of args.haves) {
    if (heldShallow.has(have) || !repo.objects.has(have)) continue;
    for (const oid of collectReachableSafe(repo, have)) heldNonShallow.add(oid);
  }

  const distance = new Map<string, number>();
  const queue: [string, number][] = [];
  for (const want of args.wants) {
    if (!isObjectId(want) || distance.has(want)) continue;
    distance.set(want, 0);
    queue.push([want, 0]);
  }
  const included = new Set<string>();
  const includedTags = new Set<string>();
  while (queue.length) {
    const [oid, depth] = queue.shift()!;
    const object = repo.objects.get(oid);
    if (!object) continue;
    if (object.type === 'tag') {
      includedTags.add(oid);
      const target = parseTag(object.data).object;
      if (!distance.has(target)) {
        distance.set(target, depth);
        queue.push([target, depth]);
      }
      continue;
    }
    if (object.type !== 'commit') {
      includedTags.add(oid);
      continue;
    }
    if (heldNonShallow.has(oid)) continue;
    if (heldShallow.has(oid)) {
      for (const parent of parseCommit(object.data).parents) {
        if (!distance.has(parent)) {
          distance.set(parent, depth + 1);
          queue.push([parent, depth + 1]);
        }
      }
      continue;
    }
    if (args.depth !== undefined && depth >= args.depth) continue;
    included.add(oid);
    for (const parent of parseCommit(object.data).parents) {
      if (!distance.has(parent)) {
        distance.set(parent, depth + 1);
        queue.push([parent, depth + 1]);
      }
    }
  }

  const closure = collectFetchObjects(repo, [...included, ...includedTags]);
  const heldClosure = new Set<string>(heldNonShallow);
  // A shallow-held commit is held with only itself and its own tree closure:
  // its parents are precisely what the client does NOT have yet.
  for (const oid of heldShallow) for (const reached of collectFetchObjects(repo, [oid])) heldClosure.add(reached);
  const objects = new Map<string, GitObject>();
  for (const oid of closure) {
    if (heldClosure.has(oid)) continue;
    const object = repo.objects.get(oid);
    if (object) objects.set(oid, object);
  }

  const acks: string[] = [];
  if (args.haves.length) {
    for (const have of args.haves) if (repo.objects.has(have)) acks.push(have);
  }
  const shallow: string[] = [];
  if (args.depth !== undefined) {
    for (const oid of included) {
      if (distance.get(oid) !== args.depth - 1) continue;
      const object = repo.objects.get(oid);
      if (object && object.type === 'commit' && parseCommit(object.data).parents.length > 0) shallow.push(oid);
    }
  }
  const unshallow: string[] = [];
  for (const oid of args.prevShallow) {
    if (!heldShallow.has(oid)) continue;
    if (shallow.includes(oid)) continue;
    if (args.wants.includes(oid) || included.has(oid)) unshallow.push(oid);
  }
  return { objects, acks, shallow, unshallow };
}

/**
 * The objects a fetch delivers for its included commits and tags: a commit
 * contributes itself and its whole tree closure, but never its parents (that
 * is what a `deepen` limit cuts off); a tag contributes its target the same
 * way. The held closure is subtracted afterwards, so a held object is never
 * re-sent even when a tag points at it.
 */
function collectFetchObjects(repo: Repo, roots: Iterable<string>): Set<string> {
  const out = new Set<string>();
  const queue = [...roots];
  while (queue.length) {
    const oid = queue.pop()!;
    if (out.has(oid)) continue;
    const object = repo.objects.get(oid);
    if (!object) continue;
    out.add(oid);
    if (object.type === 'commit') {
      queue.push(parseCommit(object.data).tree);
    } else if (object.type === 'tag') {
      queue.push(parseTag(object.data).object);
    } else if (object.type === 'tree') {
      for (const entry of parseTreeSafe(object.data)) {
        if (entry.mode !== '160000') queue.push(entry.oid);
      }
    }
  }
  return out;
}

/** collectReachable that tolerates a missing root (returns what it can reach). */
function collectReachableSafe(repo: Repo, root: string): Set<string> {
  const out = new Set<string>();
  const queue = [root];
  while (queue.length) {
    const oid = queue.pop()!;
    if (out.has(oid)) continue;
    const object = repo.objects.get(oid);
    if (!object) continue;
    out.add(oid);
    if (object.type === 'commit') {
      const commit = parseCommit(object.data);
      queue.push(commit.tree);
      for (const parent of commit.parents) queue.push(parent);
    } else if (object.type === 'tag') {
      queue.push(parseTag(object.data).object);
    } else if (object.type === 'tree') {
      for (const entry of parseTreeSafe(object.data)) {
        if (entry.mode !== '160000') queue.push(entry.oid);
      }
    }
  }
  return out;
}

function parseTreeSafe(data: Buffer): { mode: string; name: string; oid: string }[] {
  const entries: { mode: string; name: string; oid: string }[] = [];
  let offset = 0;
  while (offset < data.length) {
    const space = data.indexOf(0x20, offset);
    const nul = data.indexOf(0x00, space + 1);
    if (space < 0 || nul < 0 || nul + 21 > data.length) break;
    entries.push({
      mode: data.subarray(offset, space).toString('ascii'),
      name: data.subarray(space + 1, nul).toString('utf8'),
      oid: data.subarray(nul + 1, nul + 21).toString('hex'),
    });
    offset = nul + 21;
  }
  return entries;
}

/** Split a pack into sideband band-1 packets (band byte 0x01 + at most 32 KiB of data). */
function sidebandPackets(pack: Buffer): Buffer[] {
  const packets: Buffer[] = [];
  for (let offset = 0; offset < pack.length; offset += SIDEBAND_CHUNK) {
    const chunk = pack.subarray(offset, Math.min(pack.length, offset + SIDEBAND_CHUNK));
    packets.push(pktLine(Buffer.concat([Buffer.from([0x01]), chunk])));
  }
  return packets;
}

function sortedPackObjects(objects: Map<string, GitObject>): GitObject[] {
  return [...objects.keys()].sort().map((oid) => objects.get(oid)!);
}

function handleFetchV2(store: Store, ctx: Ctx, res: ServerResponse, repo: Repo, args: FetchArgs): boolean {
  const selection = selectFetch(store, repo, args);
  const parts: Buffer[] = [];
  // Each section opens with its name line; delim packets separate the sections.
  parts.push(pktLine('acknowledgments\n'));
  if (selection.acks.length) {
    for (const ack of selection.acks) parts.push(pktLine(`ACK ${ack}\n`));
  } else {
    parts.push(pktLine('NAK\n'));
  }
  parts.push(pktLine('ready\n'));
  parts.push(PKT_DELIM);
  if (selection.shallow.length || selection.unshallow.length) {
    parts.push(pktLine('shallow-info\n'));
    for (const oid of selection.shallow) parts.push(pktLine(`shallow ${oid}\n`));
    for (const oid of selection.unshallow) parts.push(pktLine(`unshallow ${oid}\n`));
    parts.push(PKT_DELIM);
  }
  parts.push(pktLine('packfile\n'));
  const pack = writePack(sortedPackObjects(selection.objects));
  for (const packet of sidebandPackets(pack)) parts.push(packet);
  parts.push(PKT_FLUSH);
  sendBuffer(res, 200, 'application/x-git-upload-pack-result', Buffer.concat(parts), ctx.requestId);
  return true;
}

function handleFetchV0(store: Store, ctx: Ctx, res: ServerResponse, repo: Repo, args: FetchArgs): boolean {
  const selection = selectFetch(store, repo, args);
  const parts: Buffer[] = [];
  if (selection.acks.length) {
    for (const ack of selection.acks) parts.push(pktLine(`ACK ${ack}\n`));
  } else {
    parts.push(pktLine('NAK\n'));
  }
  for (const oid of selection.shallow) parts.push(pktLine(`shallow ${oid}\n`));
  for (const oid of selection.unshallow) parts.push(pktLine(`unshallow ${oid}\n`));
  const pack = writePack(sortedPackObjects(selection.objects));
  for (const packet of sidebandPackets(pack)) parts.push(packet);
  parts.push(PKT_FLUSH);
  sendBuffer(res, 200, 'application/x-git-upload-pack-result', Buffer.concat(parts), ctx.requestId);
  return true;
}

function handleUploadPack(store: Store, ctx: Ctx, res: ServerResponse, repo: Repo): boolean {
  let packets: Pkt[];
  try {
    packets = decodePktLinesAll(ctx.body);
  } catch {
    sendJson(res, 400, { message: 'Malformed pkt-line in request body' }, ctx.requestId);
    return true;
  }
  const lines = pktLinesToStrings(packets);
  const first = (lines[0] ?? '').replace(/\n$/, '');
  if (first === 'command=ls-refs') {
    const args = lines.slice(1);
    sendBuffer(res, 200, 'application/x-git-upload-pack-result', lsRefs(repo, args), ctx.requestId);
    return true;
  }
  if (first === 'command=fetch') {
    // The arguments live after the delim packet (the trailing flush only ends them).
    const delimIndex = packets.findIndex((packet) => packet.kind === 'delim');
    const argLines = delimIndex >= 0 ? pktLinesToStrings(packets.slice(delimIndex + 1)) : lines.slice(1);
    const args = parseFetchArgs(argLines, 'v2');
    return handleFetchV2(store, ctx, res, repo, args);
  }
  if (first.startsWith('want ')) {
    const sections = sectionsOf(packets);
    const firstSection = sections[0] ?? [];
    const secondSection = sections[1] ?? [];
    const args = parseFetchArgs([...firstSection, ...secondSection], 'v0');
    return handleFetchV0(store, ctx, res, repo, args);
  }
  sendJson(res, 400, { message: 'Unknown git-upload-pack command' }, ctx.requestId);
  return true;
}

function sanitizeOneLine(text: string): string {
  return text.replace(/[\r\n]+/g, ' ').slice(0, 200);
}

function handleReceivePack(store: Store, ctx: Ctx, res: ServerResponse, repo: Repo): boolean {
  let first: Pkt[];
  let rest: Buffer;
  try {
    const decoded = decodePktLinesUntilFlush(ctx.body);
    first = decoded.packets;
    rest = decoded.rest;
  } catch {
    sendJson(res, 400, { message: 'Malformed pkt-line in request body' }, ctx.requestId);
    return true;
  }
  const lines = pktLinesToStrings(first);
  interface Command {
    old: string;
    next: string;
    ref: string;
  }
  const commands: Command[] = [];
  for (const rawLine of lines) {
    const nul = rawLine.indexOf('\u0000');
    const text = (nul >= 0 ? rawLine.slice(0, nul) : rawLine).trim();
    if (text === '') continue;
    const parts = text.split(' ');
    if (parts.length !== 3) {
      sendJson(res, 400, { message: 'Malformed ref update command' }, ctx.requestId);
      return true;
    }
    commands.push({ old: parts[0], next: parts[1], ref: parts[2] });
  }
  if (commands.length === 0) {
    sendJson(res, 400, { message: 'No ref update commands' }, ctx.requestId);
    return true;
  }

  let unpackError: string | null = null;
  if (rest.length > 0) {
    try {
      const imported = readPack(rest, { resolveBase: (oid) => repo.objects.get(oid) });
      for (const [oid, object] of imported) {
        if (!repo.objects.has(oid)) repo.objects.set(oid, object);
      }
    } catch (error) {
      unpackError = sanitizeOneLine(error instanceof Error ? error.message : 'pack parse failed');
    }
  } else if (commands.some((command) => command.next !== ZERO_OID)) {
    unpackError = 'pack is missing';
  }

  const preRefs = new Map(repo.refs);
  const results: { ref: string; ok: boolean; reason: string }[] = [];
  for (const command of commands) {
    if (unpackError) {
      results.push({ ref: command.ref, ok: false, reason: 'unpack failed' });
      continue;
    }
    if (!isObjectId(command.old) || !isObjectId(command.next)) {
      results.push({ ref: command.ref, ok: false, reason: 'invalid object id' });
      continue;
    }
    const current = preRefs.get(command.ref);
    if (command.old === ZERO_OID) {
      if (current !== undefined) results.push({ ref: command.ref, ok: false, reason: 'already exists' });
      else if (command.next === ZERO_OID) results.push({ ref: command.ref, ok: false, reason: 'invalid ref update' });
      else if (!repo.objects.has(command.next)) results.push({ ref: command.ref, ok: false, reason: 'missing object' });
      else results.push({ ref: command.ref, ok: true, reason: '' });
    } else if (current === undefined) {
      results.push({ ref: command.ref, ok: false, reason: 'reference does not exist' });
    } else if (current !== command.old) {
      results.push({ ref: command.ref, ok: false, reason: 'non-fast-forward' });
    } else if (command.next === ZERO_OID) {
      results.push({ ref: command.ref, ok: true, reason: '' });
    } else if (command.next === command.old) {
      results.push({ ref: command.ref, ok: true, reason: '' });
    } else if (!repo.objects.has(command.next)) {
      results.push({ ref: command.ref, ok: false, reason: 'missing object' });
    } else if (!isDescendant(store, repo, command.next, command.old)) {
      results.push({ ref: command.ref, ok: false, reason: 'non-fast-forward' });
    } else {
      results.push({ ref: command.ref, ok: true, reason: '' });
    }
  }
  for (let index = 0; index < commands.length; index += 1) {
    if (!results[index].ok) continue;
    const command = commands[index];
    if (command.next === ZERO_OID) repo.refs.delete(command.ref);
    else repo.refs.set(command.ref, command.next);
  }

  const reportLines: string[] = [`unpack ${unpackError ? unpackError : 'ok'}\n`];
  for (const result of results) {
    reportLines.push(result.ok ? `ok ${result.ref}\n` : `ng ${result.ref} ${sanitizeOneLine(result.reason)}\n`);
  }
  const report = Buffer.concat(reportLines.map((line) => pktLine(line)));
  const parts: Buffer[] = [];
  for (let offset = 0; offset < report.length; offset += SIDEBAND_CHUNK) {
    const chunk = report.subarray(offset, Math.min(report.length, offset + SIDEBAND_CHUNK));
    parts.push(pktLine(Buffer.concat([Buffer.from([0x01]), chunk])));
  }
  parts.push(PKT_FLUSH);
  sendBuffer(res, 200, 'application/x-git-receive-pack-result', Buffer.concat(parts), ctx.requestId);
  return true;
}

/**
 * Handle a request under `/{owner}/{repo}.git/...`. Returns true when the path
 * was a git endpoint (handled, even on error); false when it is not one.
 */
export function handleGit(store: Store, ctx: Ctx, res: ServerResponse, owner: string, repoName: string, subpath: string): boolean {
  if (ctx.method === 'GET' && subpath === '/info/refs') {
    return handleInfoRefs(store, ctx, res, owner, repoName);
  }
  if (ctx.method === 'POST' && subpath === '/git-upload-pack') {
    const repo = store.getRepo(owner, repoName);
    if (!repo) {
      sendJson(res, 404, { message: 'Not Found' }, ctx.requestId);
      return true;
    }
    return handleUploadPack(store, ctx, res, repo);
  }
  if (ctx.method === 'POST' && subpath === '/git-receive-pack') {
    const repo = store.getRepo(owner, repoName);
    if (!repo) {
      sendJson(res, 404, { message: 'Not Found' }, ctx.requestId);
      return true;
    }
    return handleReceivePack(store, ctx, res, repo);
  }
  return false;
}
