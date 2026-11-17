// store.ts — the forge's in-memory store and the pure merge / diff helpers.
//
// One Repo owns its own object map (objects never cross repositories), its refs,
// and its issue / pull-request tables. Everything the server returns is derived
// from here, so REST, GraphQL and MCP all see the same objects. No wall clock,
// no randomness: created/updated stamps are a fixed constant, request ids and
// REST ids come from monotonic counters.

import {
  type GitObject,
  type GitObjectType,
  ZERO_OID,
  buildCommit,
  buildTree,
  collectReachable,
  flattenTree,
  hashObject,
  isObjectId,
  parseCommit,
  parseTree,
} from './gitpack.ts';

export const VIEWER_LOGIN = 'adaiversen';
export const VIEWER_ID = 1000;
export const VIEWER_NAME = 'ada iversen';
export const VIEWER_EMAIL = 'adaiversen@example.invalid';

/** The only timestamp the forge ever stamps: 2025-01-01T00:00:00Z, fixed for determinism. */
export const FIXED_TIMESTAMP = '2025-01-01T00:00:00Z';
export const FIXED_EPOCH = 1735689600;

/** The identity written into every commit the forge authors itself. */
export function gitIdentity(): string {
  return `${VIEWER_NAME} <${VIEWER_EMAIL}>`;
}

export interface Label {
  readonly name: string;
  readonly color: string;
}

export interface IssueComment {
  readonly id: number;
  readonly body: string;
  readonly createdAt: string;
}

export interface Issue {
  readonly id: number;
  readonly number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  labels: string[];
  comments: IssueComment[];
  readonly createdAt: string;
  updatedAt: string;
}

export interface PullRequest {
  readonly id: number;
  readonly number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  readonly headRef: string;
  readonly baseRef: string;
  readonly headSha: string;
  readonly baseSha: string;
  merged: boolean;
  mergedAt: string | null;
  mergeCommitSha: string | null;
  readonly createdAt: string;
  updatedAt: string;
}

export interface Repo {
  readonly id: number;
  readonly owner: string;
  readonly name: string;
  description: string;
  isPrivate: boolean;
  readonly defaultBranch: string;
  readonly objects: Map<string, GitObject>;
  readonly refs: Map<string, string>;
  readonly issues: Issue[];
  readonly pulls: PullRequest[];
  readonly labels: Map<string, Label>;
  nextIssueNumber: number;
  nextPullNumber: number;
  readonly createdAt: string;
  updatedAt: string;
}

export function repoKey(owner: string, name: string): string {
  return `${owner}/${name}`;
}

export class Store {
  readonly repos = new Map<string, Repo>();
  nextRepoId = 1;
  nextRestId = 1;
  nextRequestId = 1;

  getRepo(owner: string, name: string): Repo | undefined {
    return this.repos.get(repoKey(owner, name));
  }

  createRepo(owner: string, name: string, fields: { description?: string; isPrivate?: boolean }): Repo | undefined {
    if (this.repos.has(repoKey(owner, name))) return undefined;
    const repo: Repo = {
      id: this.nextRepoId,
      owner,
      name,
      description: fields.description ?? '',
      isPrivate: fields.isPrivate ?? false,
      defaultBranch: 'main',
      objects: new Map(),
      refs: new Map(),
      issues: [],
      pulls: [],
      labels: new Map(),
      nextIssueNumber: 1,
      nextPullNumber: 1,
      createdAt: FIXED_TIMESTAMP,
      updatedAt: FIXED_TIMESTAMP,
    };
    this.nextRepoId += 1;
    this.repos.set(repoKey(owner, name), repo);
    return repo;
  }

  deleteRepo(owner: string, name: string): boolean {
    return this.repos.delete(repoKey(owner, name));
  }

  // -- objects --------------------------------------------------------------------------

  putObject(repo: Repo, type: GitObjectType, data: Buffer): string {
    const oid = hashObject(type, data);
    if (!repo.objects.has(oid)) repo.objects.set(oid, { type, data });
    return oid;
  }

  getObject(repo: Repo, oid: string): GitObject | undefined {
    return repo.objects.get(oid);
  }

  /** Create a commit object and store it; the message must already end with '\n'. */
  createCommit(
    repo: Repo,
    tree: string,
    parents: string[],
    message: string,
  ): string {
    const data = buildCommit({ tree, parents, author: gitIdentity(), committer: gitIdentity(), timestamp: FIXED_EPOCH, message });
    return this.putObject(repo, 'commit', data);
  }

  /** The commit object for a ref, or undefined when the ref is missing / not a commit. */
  refCommit(repo: Repo, ref: string): { oid: string; tree: string } | undefined {
    const oid = repo.refs.get(ref);
    if (!oid) return undefined;
    const object = repo.objects.get(oid);
    if (!object) return undefined;
    if (object.type === 'tag') return undefined;
    if (object.type !== 'commit') return undefined;
    return { oid, tree: parseCommit(object.data).tree };
  }

  // -- issues and pulls -------------------------------------------------------------------

  createIssue(repo: Repo, fields: { title: string; body?: string; labels?: string[] }): Issue {
    const issue: Issue = {
      id: this.nextRestId,
      number: repo.nextIssueNumber,
      title: fields.title,
      body: fields.body ?? '',
      state: 'open',
      labels: fields.labels ?? [],
      comments: [],
      createdAt: FIXED_TIMESTAMP,
      updatedAt: FIXED_TIMESTAMP,
    };
    this.nextRestId += 1;
    repo.nextIssueNumber += 1;
    repo.issues.push(issue);
    return issue;
  }

  createPull(repo: Repo, fields: {
    title: string;
    body?: string;
    headRef: string;
    baseRef: string;
    headSha: string;
    baseSha: string;
  }): PullRequest {
    const pull: PullRequest = {
      id: this.nextRestId,
      number: repo.nextPullNumber,
      title: fields.title,
      body: fields.body ?? '',
      state: 'open',
      headRef: fields.headRef,
      baseRef: fields.baseRef,
      headSha: fields.headSha,
      baseSha: fields.baseSha,
      merged: false,
      mergedAt: null,
      mergeCommitSha: null,
      createdAt: FIXED_TIMESTAMP,
      updatedAt: FIXED_TIMESTAMP,
    };
    this.nextRestId += 1;
    repo.nextPullNumber += 1;
    repo.pulls.push(pull);
    return pull;
  }

  addComment(issue: Issue, body: string): IssueComment {
    const comment: IssueComment = { id: this.nextRestId, body, createdAt: FIXED_TIMESTAMP };
    this.nextRestId += 1;
    issue.comments.push(comment);
    return comment;
  }
}

// ---------------------------------------------------------------------------------------------
// merge and diff helpers (pure; operate on a repo's object map)
// ---------------------------------------------------------------------------------------------

export function isDescendant(store: Store, repo: Repo, descendant: string, ancestor: string): boolean {
  if (descendant === ancestor) return true;
  const seen = new Set<string>();
  const queue = [descendant];
  while (queue.length) {
    const oid = queue.pop()!;
    if (oid === ancestor) return true;
    if (seen.has(oid)) continue;
    seen.add(oid);
    const object = repo.objects.get(oid);
    if (!object || object.type !== 'commit') continue;
    for (const parent of parseCommit(object.data).parents) queue.push(parent);
  }
  return false;
}

/** All ancestors of `oid` (including itself), as a map oid -> distance from `oid`. */
function ancestorsWithDistance(repo: Repo, oid: string): Map<string, number> {
  const out = new Map<string, number>();
  const queue: [string, number][] = [[oid, 0]];
  while (queue.length) {
    const [current, distance] = queue.shift()!;
    if (out.has(current)) continue;
    out.set(current, distance);
    const object = repo.objects.get(current);
    if (!object || object.type !== 'commit') continue;
    for (const parent of parseCommit(object.data).parents) queue.push([parent, distance + 1]);
  }
  return out;
}

/** The merge base of two commits: the shared ancestor closest to both (smallest distance sum). */
export function mergeBase(repo: Repo, left: string, right: string): string | null {
  const leftAncestors = ancestorsWithDistance(repo, left);
  const rightAncestors = ancestorsWithDistance(repo, right);
  let best: string | null = null;
  let bestScore = Number.POSITIVE_INFINITY;
  for (const [oid, distance] of leftAncestors) {
    const other = rightAncestors.get(oid);
    if (other === undefined) continue;
    const score = distance + other;
    if (score < bestScore) {
      bestScore = score;
      best = oid;
    }
  }
  return best;
}

/** Build a nested tree from a flat path map, storing every intermediate tree object. */
export function buildTreeFromFlat(store: Store, repo: Repo, flat: Map<string, { oid: string; mode: string }>): string {
  interface Node {
    files: Map<string, { oid: string; mode: string }>;
    dirs: Map<string, Node>;
  }
  const root: Node = { files: new Map(), dirs: new Map() };
  for (const [path, entry] of flat) {
    const parts = path.split('/');
    let node = root;
    for (let index = 0; index < parts.length - 1; index += 1) {
      let child = node.dirs.get(parts[index]);
      if (!child) {
        child = { files: new Map(), dirs: new Map() };
        node.dirs.set(parts[index], child);
      }
      node = child;
    }
    node.files.set(parts[parts.length - 1], entry);
  }
  const build = (node: Node): string => {
    const entries: { mode: string; name: string; oid: string }[] = [];
    for (const [name, entry] of node.files) entries.push({ mode: entry.mode, name, oid: entry.oid });
    for (const [name, child] of node.dirs) entries.push({ mode: '40000', name, oid: build(child) });
    return store.putObject(repo, 'tree', buildTree(entries));
  };
  return build(root);
}

/**
 * Three-way squash merge of `headTip` onto `baseTip`. Returns the merged tree (or null
 * when the two sides changed the same file differently) plus the conflicting paths.
 */
export function threeWayMerge(
  store: Store,
  repo: Repo,
  baseTip: string,
  headTip: string,
): { conflict: boolean; tree: string | null; conflicts: string[] } {
  const lookup = (oid: string): GitObject | undefined => repo.objects.get(oid);
  const baseObject = repo.objects.get(baseTip);
  const headObject = repo.objects.get(headTip);
  if (!baseObject || baseObject.type !== 'commit' || !headObject || headObject.type !== 'commit') {
    return { conflict: true, tree: null, conflicts: [] };
  }
  const baseTree = parseCommit(baseObject.data).tree;
  const headTree = parseCommit(headObject.data).tree;
  const base = mergeBase(repo, baseTip, headTip);
  const ancestorTree = base ? parseCommit(repo.objects.get(base)!.data).tree : null;
  const baseFlat = flattenTree(lookup, baseTree);
  const headFlat = flattenTree(lookup, headTree);
  const ancestorFlat = ancestorTree ? flattenTree(lookup, ancestorTree) : new Map<string, { oid: string; mode: string }>();
  const result = new Map<string, { oid: string; mode: string }>();
  const conflicts: string[] = [];
  const paths = new Set<string>([...ancestorFlat.keys(), ...baseFlat.keys(), ...headFlat.keys()]);
  for (const path of paths) {
    const ancestor = ancestorFlat.get(path);
    const baseSide = baseFlat.get(path);
    const headSide = headFlat.get(path);
    const headChanged = headSide?.oid !== ancestor?.oid;
    const baseChanged = baseSide?.oid !== ancestor?.oid;
    if (headChanged && baseChanged && headSide?.oid !== baseSide?.oid) {
      conflicts.push(path);
      continue;
    }
    const winner = headChanged ? headSide : baseSide;
    if (winner) result.set(path, winner);
  }
  if (conflicts.length) return { conflict: true, tree: null, conflicts };
  return { conflict: false, tree: buildTreeFromFlat(store, repo, result), conflicts: [] };
}

export interface FileChange {
  readonly filename: string;
  readonly status: 'added' | 'removed' | 'modified' | 'changed';
  readonly additions: number;
  readonly deletions: number;
  readonly changes: number;
}

/** Longest-common-subsequence line diff; falls back to a multiset count for very large files. */
export function lineDiff(oldText: string, newText: string): { additions: number; deletions: number } {
  // git counts lines: a trailing newline ends the last line, it is not a new empty line.
  const splitLines = (text: string): string[] => {
    if (text === '') return [];
    const parts = text.split('\n');
    if (parts.length > 0 && parts[parts.length - 1] === '') parts.pop();
    return parts;
  };
  const oldLines = splitLines(oldText);
  const newLines = splitLines(newText);
  if (oldLines.length * newLines.length > 1_000_000) {
    const oldCount = new Map<string, number>();
    for (const line of oldLines) oldCount.set(line, (oldCount.get(line) ?? 0) + 1);
    let kept = 0;
    for (const line of newLines) {
      const count = oldCount.get(line) ?? 0;
      if (count > 0) {
        oldCount.set(line, count - 1);
        kept += 1;
      }
    }
    return { additions: newLines.length - kept, deletions: oldLines.length - kept };
  }
  const rows = oldLines.length + 1;
  const cols = newLines.length + 1;
  const table = new Uint32Array(rows * cols);
  for (let i = oldLines.length - 1; i >= 0; i -= 1) {
    for (let j = newLines.length - 1; j >= 0; j -= 1) {
      table[i * cols + j] =
        oldLines[i] === newLines[j]
          ? table[(i + 1) * cols + j + 1] + 1
          : Math.max(table[(i + 1) * cols + j], table[i * cols + j + 1]);
    }
  }
  let i = 0;
  let j = 0;
  let additions = 0;
  let deletions = 0;
  while (i < oldLines.length && j < newLines.length) {
    if (oldLines[i] === newLines[j]) {
      i += 1;
      j += 1;
    } else if (table[(i + 1) * cols + j] >= table[i * cols + j + 1]) {
      deletions += 1;
      i += 1;
    } else {
      additions += 1;
      j += 1;
    }
  }
  deletions += oldLines.length - i;
  additions += newLines.length - j;
  return { additions, deletions };
}

/** File-level diff between two trees: status and line counts per changed path. */
export function diffTrees(store: Store, repo: Repo, baseTree: string, headTree: string): FileChange[] {
  const lookup = (oid: string): GitObject | undefined => repo.objects.get(oid);
  const baseFlat = flattenTree(lookup, baseTree);
  const headFlat = flattenTree(lookup, headTree);
  const changes: FileChange[] = [];
  const paths = new Set<string>([...baseFlat.keys(), ...headFlat.keys()]);
  for (const path of paths) {
    const baseSide = baseFlat.get(path);
    const headSide = headFlat.get(path);
    if (baseSide && !headSide) {
      const oldText = repo.objects.get(baseSide.oid)?.data.toString('utf8') ?? '';
      const counts = lineDiff(oldText, '');
      changes.push({ filename: path, status: 'removed', additions: counts.additions, deletions: counts.deletions, changes: counts.additions + counts.deletions });
    } else if (!baseSide && headSide) {
      const newText = repo.objects.get(headSide.oid)?.data.toString('utf8') ?? '';
      const counts = lineDiff('', newText);
      changes.push({ filename: path, status: 'added', additions: counts.additions, deletions: counts.deletions, changes: counts.additions + counts.deletions });
    } else if (baseSide && headSide && baseSide.oid !== headSide.oid) {
      const oldText = repo.objects.get(baseSide.oid)?.data.toString('utf8') ?? '';
      const newText = repo.objects.get(headSide.oid)?.data.toString('utf8') ?? '';
      const counts = lineDiff(oldText, newText);
      const status = counts.additions > 0 && counts.deletions === 0 ? 'added' : 'modified';
      changes.push({ filename: path, status, additions: counts.additions, deletions: counts.deletions, changes: counts.additions + counts.deletions });
    }
  }
  changes.sort((a, b) => (a.filename < b.filename ? -1 : a.filename > b.filename ? 1 : 0));
  return changes;
}

/** True when a commit's tree contains `path` with exactly `blobOid`. */
export function treeContainsBlob(repo: Repo, treeOid: string, path: string, blobOid: string): boolean {
  const flat = flattenTree((oid) => repo.objects.get(oid), treeOid);
  const entry = flat.get(path);
  return entry !== undefined && entry.oid === blobOid;
}

/** Mergeability of a pull request: MERGEABLE when a clean squash merge exists, else CONFLICTING. */
export function pullMergeability(store: Store, repo: Repo, pull: PullRequest): 'MERGEABLE' | 'CONFLICTING' {
  const headObject = repo.objects.get(pull.headSha);
  const baseObject = repo.objects.get(pull.baseSha);
  if (!headObject || headObject.type !== 'commit' || !baseObject || baseObject.type !== 'commit') return 'CONFLICTING';
  const merged = threeWayMerge(store, repo, pull.baseSha, pull.headSha);
  return merged.conflict ? 'CONFLICTING' : 'MERGEABLE';
}

/**
 * Apply a squash merge to a pull request: the merged tree is built from the pull's
 * frozen head/base, the merge commit's parent is the CURRENT base tip, and the base
 * branch moves to the merge commit. Returns the merge commit oid, or null on conflict.
 */
export function squashMergePull(store: Store, repo: Repo, pull: PullRequest): string | null {
  const baseRef = `refs/heads/${pull.baseRef}`;
  const baseTip = repo.refs.get(baseRef);
  if (!baseTip) return null;
  const merged = threeWayMerge(store, repo, baseTip, pull.headSha);
  if (merged.conflict || !merged.tree) return null;
  const message = `Merge pull request #${pull.number} from ${repo.owner}/${pull.headRef}\n`;
  const mergeCommit = store.createCommit(repo, merged.tree, [baseTip], message);
  repo.refs.set(baseRef, mergeCommit);
  pull.state = 'closed';
  pull.merged = true;
  pull.mergedAt = FIXED_TIMESTAMP;
  pull.mergeCommitSha = mergeCommit;
  pull.updatedAt = FIXED_TIMESTAMP;
  return mergeCommit;
}

/** The object id of the commit that first introduced `path` on the branch, or undefined. */
export function blobOidAtPath(repo: Repo, commitOid: string, path: string): string | undefined {
  const object = repo.objects.get(commitOid);
  if (!object || object.type !== 'commit') return undefined;
  const flat = flattenTree((oid) => repo.objects.get(oid), parseCommit(object.data).tree);
  return flat.get(path)?.oid;
}

/** Apply `entries` (path -> blob oid, or undefined to delete) onto a tree and return the new tree oid. */
export function applyTreeChanges(store: Store, repo: Repo, treeOid: string, changes: Map<string, string | undefined>): string {
  const flat = flattenTree((oid) => repo.objects.get(oid), treeOid);
  for (const [path, oid] of changes) {
    if (oid === undefined) flat.delete(path);
    else flat.set(path, { oid, mode: '100644' });
  }
  return buildTreeFromFlat(store, repo, flat);
}

/** All commit oids reachable from `tip`, newest first (BFS by parent edges). */
export function commitsFromTip(repo: Repo, tip: string): string[] {
  const out: string[] = [];
  const seen = new Set<string>();
  const queue = [tip];
  while (queue.length) {
    const oid = queue.shift()!;
    if (seen.has(oid)) continue;
    seen.add(oid);
    const object = repo.objects.get(oid);
    if (!object || object.type !== 'commit') continue;
    out.push(oid);
    for (const parent of parseCommit(object.data).parents) queue.push(parent);
  }
  return out;
}

export function emptyTreeOid(store: Store, repo: Repo): string {
  return store.putObject(repo, 'tree', buildTree([]));
}

export { collectReachable, ZERO_OID, isObjectId };
