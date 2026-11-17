// rest.ts — the REST v3 API.
//
// Shapes follow the public GitHub API closely enough for the conformance
// checks: repositories, branches, commits, contents, labels, issues, comments
// and pull requests, all backed by the same in-memory store (and the same git
// object map) the smart-HTTP endpoints use. A repository requested under any
// owner other than the token owner is a 404, never a 403.

import type { ServerResponse } from 'node:http';
import {
  parseCommit,
  flattenTree,
  type GitObject,
} from './gitpack.ts';
import {
  FIXED_TIMESTAMP,
  VIEWER_EMAIL,
  VIEWER_ID,
  VIEWER_LOGIN,
  VIEWER_NAME,
  applyTreeChanges,
  blobOidAtPath,
  commitsFromTip,
  diffTrees,
  emptyTreeOid,
  squashMergePull,
  type Issue,
  type IssueComment,
  type PullRequest,
  type Repo,
  type Store,
} from './store.ts';
import {
  asBoolean,
  asString,
  asStringArray,
  isRecord,
  parseJsonBody,
  sendEmpty,
  sendJson,
  type Ctx,
} from './http.ts';

function nodeId(kind: string, value: string): string {
  return Buffer.from(`${kind}:${value}`, 'utf8').toString('base64');
}

function viewerJson(): Record<string, unknown> {
  return {
    login: VIEWER_LOGIN,
    id: VIEWER_ID,
    node_id: nodeId('User', String(VIEWER_ID)),
    avatar_url: '',
    type: 'User',
    site_admin: false,
    name: VIEWER_NAME,
    email: VIEWER_EMAIL,
    html_url: `https://github.com/${VIEWER_LOGIN}`,
  };
}

/** Shared with the MCP tools so MCP and REST return the identical repository shape. */
export function repoJson(repo: Repo): Record<string, unknown> {
  return {
    id: repo.id,
    node_id: nodeId('Repository', String(repo.id)),
    name: repo.name,
    full_name: `${repo.owner}/${repo.name}`,
    owner: { login: repo.owner, id: VIEWER_ID, node_id: nodeId('User', String(VIEWER_ID)), type: 'User' },
    private: repo.isPrivate,
    description: repo.description,
    default_branch: repo.defaultBranch,
    created_at: repo.createdAt,
    updated_at: repo.updatedAt,
    html_url: `https://github.com/${repo.owner}/${repo.name}`,
    clone_url: `https://github.com/${repo.owner}/${repo.name}.git`,
    has_issues: true,
    has_projects: false,
    has_wiki: false,
  };
}

/** Shared with the MCP tools so MCP and REST return the identical issue shape. */
export function issueJson(repo: Repo, issue: Issue): Record<string, unknown> {
  return {
    id: issue.id,
    node_id: nodeId('Issue', `${repo.id}:${issue.number}`),
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state,
    labels: issue.labels.map((name) => {
      const label = repo.labels.get(name);
      return { name, color: label?.color ?? 'ededed' };
    }),
    created_at: issue.createdAt,
    updated_at: issue.updatedAt,
    user: { login: VIEWER_LOGIN, id: VIEWER_ID, type: 'User' },
    comments: issue.comments.length,
  };
}

function commentJson(repo: Repo, comment: IssueComment): Record<string, unknown> {
  return {
    id: comment.id,
    node_id: nodeId('IssueComment', `${repo.id}:${comment.id}`),
    body: comment.body,
    created_at: comment.createdAt,
    user: { login: VIEWER_LOGIN, id: VIEWER_ID, type: 'User' },
  };
}

/** Shared with the MCP tools so MCP and REST return the identical pull-request shape. */
export function pullJson(repo: Repo, pull: PullRequest): Record<string, unknown> {
  const headRepo = repoJson(repo);
  return {
    id: pull.id,
    node_id: nodeId('PullRequest', `${repo.id}:${pull.number}`),
    number: pull.number,
    title: pull.title,
    body: pull.body,
    state: pull.state,
    merged: pull.merged,
    merged_at: pull.mergedAt,
    merge_commit_sha: pull.mergeCommitSha,
    head: { label: `${repo.owner}:${pull.headRef}`, ref: pull.headRef, sha: pull.headSha, repo: headRepo },
    base: { label: `${repo.owner}:${pull.baseRef}`, ref: pull.baseRef, sha: pull.baseSha, repo: headRepo },
    created_at: pull.createdAt,
    updated_at: pull.updatedAt,
    user: { login: VIEWER_LOGIN, id: VIEWER_ID, type: 'User' },
  };
}

function findRepo(store: Store, ctx: Ctx, res: ServerResponse, owner: string, name: string): Repo | null {
  const repo = store.getRepo(owner, name);
  if (!repo) {
    sendJson(res, 404, { message: 'Not Found' }, ctx.requestId);
    return null;
  }
  return repo;
}

function findIssue(repo: Repo, ctx: Ctx, res: ServerResponse, raw: string): Issue | null {
  const number = Number.parseInt(raw, 10);
  const issue = repo.issues.find((item) => item.number === number);
  if (!issue) {
    sendJson(res, 404, { message: 'Not Found' }, ctx.requestId);
    return null;
  }
  return issue;
}

function findPull(repo: Repo, ctx: Ctx, res: ServerResponse, raw: string): PullRequest | null {
  const number = Number.parseInt(raw, 10);
  const pull = repo.pulls.find((item) => item.number === number);
  if (!pull) {
    sendJson(res, 404, { message: 'Not Found' }, ctx.requestId);
    return null;
  }
  return pull;
}

function readBody(ctx: Ctx, res: ServerResponse): Record<string, unknown> | null {
  const parsed = parseJsonBody(ctx.body);
  if (!parsed.ok) {
    sendJson(res, 400, { message: parsed.error }, ctx.requestId);
    return null;
  }
  if (!isRecord(parsed.value)) {
    sendJson(res, 400, { message: 'Request body must be a JSON object' }, ctx.requestId);
    return null;
  }
  return parsed.value;
}

function paginate<T>(items: T[], ctx: Ctx): { page: T[]; link: string | null } {
  const perPageRaw = Number.parseInt(ctx.query.get('per_page') ?? '30', 10);
  const perPage = Number.isFinite(perPageRaw) && perPageRaw > 0 ? Math.min(perPageRaw, 100) : 30;
  const pageRaw = Number.parseInt(ctx.query.get('page') ?? '1', 10);
  const page = Number.isFinite(pageRaw) && pageRaw > 0 ? pageRaw : 1;
  const start = (page - 1) * perPage;
  const slice = items.slice(start, start + perPage);
  const totalPages = Math.max(1, Math.ceil(items.length / perPage));
  let link: string | null = null;
  if (page < totalPages) {
    const base = `http://${ctx.host}${ctx.path}`;
    const next = `${base}?per_page=${perPage}&page=${page + 1}`;
    const last = `${base}?per_page=${perPage}&page=${totalPages}`;
    link = `<${next}>; rel="next", <${last}>; rel="last"`;
  }
  return { page: slice, link };
}

function linkHeaders(link: string | null): Record<string, string> {
  return link ? { Link: link } : {};
}

/** The tree object id of a nested directory inside a tree, or undefined. */
function subtreeOid(repo: Repo, treeOid: string, relativePath: string): string | undefined {
  if (relativePath === '') return treeOid;
  const parts = relativePath.split('/');
  let current = treeOid;
  for (const part of parts) {
    const object = repo.objects.get(current);
    if (!object || object.type !== 'tree') return undefined;
    const entries = parseTreeEntries(object.data);
    const entry = entries.find((item) => item.name === part && item.mode === '40000');
    if (!entry) return undefined;
    current = entry.oid;
  }
  return current;
}

function parseTreeEntries(data: Buffer): { mode: string; name: string; oid: string }[] {
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

function handleUserRepos(store: Store, ctx: Ctx, res: ServerResponse): boolean {
  if (ctx.method === 'GET') {
    const all = [...store.repos.values()]
      .filter((repo) => repo.owner === VIEWER_LOGIN)
      .sort((a, b) => a.id - b.id);
    const { page, link } = paginate(all, ctx);
    sendJson(res, 200, page.map(repoJson), ctx.requestId, linkHeaders(link));
    return true;
  }
  if (ctx.method === 'POST') {
    const body = readBody(ctx, res);
    if (!body) return true;
    const name = asString(body.name);
    if (!name || name.includes('/') || name === '.' || name === '..') {
      sendJson(res, 422, { message: 'name is required and must be a valid repository name' }, ctx.requestId);
      return true;
    }
    if (store.getRepo(VIEWER_LOGIN, name)) {
      sendJson(res, 422, { message: `Repository ${name} already exists` }, ctx.requestId);
      return true;
    }
    const repo = store.createRepo(VIEWER_LOGIN, name, {
      description: asString(body.description),
      isPrivate: asBoolean(body.private),
    })!;
    sendJson(res, 201, repoJson(repo), ctx.requestId);
    return true;
  }
  return false;
}

function handleRepo(store: Store, ctx: Ctx, res: ServerResponse, owner: string, name: string): boolean {
  const repo = findRepo(store, ctx, res, owner, name);
  if (!repo) return true;
  if (ctx.method === 'GET') {
    sendJson(res, 200, repoJson(repo), ctx.requestId);
    return true;
  }
  if (ctx.method === 'PATCH') {
    const body = readBody(ctx, res);
    if (!body) return true;
    const description = asString(body.description);
    if (description !== undefined) repo.description = description;
    repo.updatedAt = FIXED_TIMESTAMP;
    sendJson(res, 200, repoJson(repo), ctx.requestId);
    return true;
  }
  if (ctx.method === 'DELETE') {
    store.deleteRepo(owner, name);
    sendEmpty(res, 204, ctx.requestId);
    return true;
  }
  return false;
}

function handleBranches(store: Store, ctx: Ctx, res: ServerResponse, owner: string, name: string): boolean {
  if (ctx.method !== 'GET') return false;
  const repo = findRepo(store, ctx, res, owner, name);
  if (!repo) return true;
  const branches = [...repo.refs.keys()]
    .filter((ref) => ref.startsWith('refs/heads/'))
    .sort()
    .map((ref) => ({
      name: ref.slice('refs/heads/'.length),
      commit: { sha: repo.refs.get(ref), url: '' },
      protected: false,
    }));
  sendJson(res, 200, branches, ctx.requestId);
  return true;
}

function handleBranch(store: Store, ctx: Ctx, res: ServerResponse, owner: string, name: string, branch: string): boolean {
  if (ctx.method !== 'GET') return false;
  const repo = findRepo(store, ctx, res, owner, name);
  if (!repo) return true;
  const sha = repo.refs.get(`refs/heads/${branch}`);
  if (!sha) {
    sendJson(res, 404, { message: 'Not Found' }, ctx.requestId);
    return true;
  }
  sendJson(res, 200, { name: branch, commit: { sha, url: '' }, protected: false }, ctx.requestId);
  return true;
}

function handleCommits(store: Store, ctx: Ctx, res: ServerResponse, owner: string, name: string): boolean {
  if (ctx.method !== 'GET') return false;
  const repo = findRepo(store, ctx, res, owner, name);
  if (!repo) return true;
  const branch = ctx.query.get('sha') ?? repo.defaultBranch;
  const tip = repo.refs.get(`refs/heads/${branch}`);
  if (!tip) {
    sendJson(res, 404, { message: 'Not Found' }, ctx.requestId);
    return true;
  }
  const commits = commitsFromTip(repo, tip).map((oid) => {
    const commit = parseCommit(repo.objects.get(oid)!.data);
    const message = commit.message.endsWith('\n') ? commit.message.slice(0, -1) : commit.message;
    return {
      sha: oid,
      node_id: nodeId('Commit', oid),
      commit: {
        message,
        author: { name: VIEWER_NAME, email: VIEWER_EMAIL, date: FIXED_TIMESTAMP },
        committer: { name: VIEWER_NAME, email: VIEWER_EMAIL, date: FIXED_TIMESTAMP },
      },
      author: { login: VIEWER_LOGIN, id: VIEWER_ID, type: 'User' },
      committer: { login: VIEWER_LOGIN, id: VIEWER_ID, type: 'User' },
    };
  });
  sendJson(res, 200, commits, ctx.requestId);
  return true;
}

function handleCreateRef(store: Store, ctx: Ctx, res: ServerResponse, owner: string, name: string): boolean {
  if (ctx.method !== 'POST') return false;
  const repo = findRepo(store, ctx, res, owner, name);
  if (!repo) return true;
  const body = readBody(ctx, res);
  if (!body) return true;
  const ref = asString(body.ref);
  const sha = asString(body.sha);
  if (!ref || !ref.startsWith('refs/') || !sha) {
    sendJson(res, 422, { message: 'ref and sha are required' }, ctx.requestId);
    return true;
  }
  if (repo.refs.has(ref)) {
    sendJson(res, 422, { message: 'Reference already exists' }, ctx.requestId);
    return true;
  }
  if (!repo.objects.has(sha)) {
    sendJson(res, 422, { message: 'Object does not exist' }, ctx.requestId);
    return true;
  }
  repo.refs.set(ref, sha);
  sendJson(res, 201, { ref, object: { sha, type: 'commit', url: '' } }, ctx.requestId);
  return true;
}

function handleGetRef(store: Store, ctx: Ctx, res: ServerResponse, owner: string, name: string, branch: string): boolean {
  if (ctx.method !== 'GET') return false;
  const repo = findRepo(store, ctx, res, owner, name);
  if (!repo) return true;
  const ref = `refs/heads/${branch}`;
  const sha = repo.refs.get(ref);
  if (!sha) {
    sendJson(res, 404, { message: 'Not Found' }, ctx.requestId);
    return true;
  }
  const object = repo.objects.get(sha);
  sendJson(res, 201, { ref, object: { sha, type: object?.type ?? 'commit', url: '' } }, ctx.requestId);
  return true;
}

function handleGetContents(store: Store, ctx: Ctx, res: ServerResponse, owner: string, name: string, path: string): boolean {
  if (ctx.method !== 'GET') return false;
  const repo = findRepo(store, ctx, res, owner, name);
  if (!repo) return true;
  const branch = ctx.query.get('ref') ?? repo.defaultBranch;
  const tip = repo.refs.get(`refs/heads/${branch}`);
  if (!tip) {
    sendJson(res, 404, { message: 'Not Found' }, ctx.requestId);
    return true;
  }
  const treeOid = parseCommit(repo.objects.get(tip)!.data).tree;
  const flat = flattenTree((oid: string): GitObject | undefined => repo.objects.get(oid), treeOid);
  const file = flat.get(path);
  if (file) {
    const content = repo.objects.get(file.oid)!.data;
    sendJson(res, 200, {
      type: 'file',
      encoding: 'base64',
      size: content.length,
      name: path.split('/').pop() ?? path,
      path,
      content: content.toString('base64'),
      sha: file.oid,
    }, ctx.requestId);
    return true;
  }
  const prefix = path === '' ? '' : `${path}/`;
  const children = new Map<string, { type: 'file' | 'dir'; name: string; path: string; sha: string }>();
  for (const [entryPath, entry] of flat) {
    if (!entryPath.startsWith(prefix)) continue;
    const rest = entryPath.slice(prefix.length);
    const segment = rest.split('/')[0];
    if (rest.includes('/')) {
      const dirPath = prefix + segment;
      if (!children.has(dirPath)) {
        const dirOid = subtreeOid(repo, treeOid, dirPath);
        children.set(dirPath, { type: 'dir', name: segment, path: dirPath, sha: dirOid ?? '' });
      }
    } else {
      children.set(entryPath, { type: 'file', name: segment, path: entryPath, sha: entry.oid });
    }
  }
  if (children.size === 0) {
    sendJson(res, 404, { message: 'Not Found' }, ctx.requestId);
    return true;
  }
  const list = [...children.values()].sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
  sendJson(res, 200, list, ctx.requestId);
  return true;
}

function handlePutContents(store: Store, ctx: Ctx, res: ServerResponse, owner: string, name: string, path: string): boolean {
  if (ctx.method !== 'PUT') return false;
  const repo = findRepo(store, ctx, res, owner, name);
  if (!repo) return true;
  const body = readBody(ctx, res);
  if (!body) return true;
  const message = asString(body.message) ?? '';
  const contentB64 = asString(body.content);
  if (contentB64 === undefined) {
    sendJson(res, 422, { message: 'content is required' }, ctx.requestId);
    return true;
  }
  const branch = asString(body.branch) ?? repo.defaultBranch;
  const branchRef = `refs/heads/${branch}`;
  let baseCommit = repo.refs.get(branchRef);
  if (!baseCommit && branch !== repo.defaultBranch) {
    baseCommit = repo.refs.get(`refs/heads/${repo.defaultBranch}`);
    if (!baseCommit) {
      sendJson(res, 404, { message: 'Not Found' }, ctx.requestId);
      return true;
    }
  }
  const currentBlob = baseCommit ? blobOidAtPath(repo, baseCommit, path) : undefined;
  const expectedSha = asString(body.sha);
  if (expectedSha !== undefined && expectedSha !== currentBlob) {
    sendJson(res, 409, { message: 'sha does not match the current file' }, ctx.requestId);
    return true;
  }
  const content = Buffer.from(contentB64, 'base64');
  const blobOid = store.putObject(repo, 'blob', content);
  const baseTree = baseCommit ? parseCommit(repo.objects.get(baseCommit)!.data).tree : emptyTreeOid(store, repo);
  const newTree = applyTreeChanges(store, repo, baseTree, new Map([[path, blobOid]]));
  const parents = baseCommit ? [baseCommit] : [];
  const commitOid = store.createCommit(repo, newTree, parents, `${message}\n`);
  repo.refs.set(branchRef, commitOid);
  const status = currentBlob ? 200 : 201;
  sendJson(res, status, {
    content: {
      name: path.split('/').pop() ?? path,
      path,
      sha: blobOid,
      size: content.length,
      type: 'file',
      encoding: 'base64',
      content: contentB64,
    },
    commit: { sha: commitOid, message },
  }, ctx.requestId);
  return true;
}

function handleDeleteContents(store: Store, ctx: Ctx, res: ServerResponse, owner: string, name: string, path: string): boolean {
  if (ctx.method !== 'DELETE') return false;
  const repo = findRepo(store, ctx, res, owner, name);
  if (!repo) return true;
  const body = readBody(ctx, res);
  if (!body) return true;
  const message = asString(body.message) ?? '';
  const branch = asString(body.branch) ?? repo.defaultBranch;
  const branchRef = `refs/heads/${branch}`;
  const tip = repo.refs.get(branchRef);
  if (!tip) {
    sendJson(res, 404, { message: 'Not Found' }, ctx.requestId);
    return true;
  }
  const currentBlob = blobOidAtPath(repo, tip, path);
  if (!currentBlob) {
    sendJson(res, 404, { message: 'Not Found' }, ctx.requestId);
    return true;
  }
  const expectedSha = asString(body.sha);
  if (expectedSha !== undefined && expectedSha !== currentBlob) {
    sendJson(res, 409, { message: 'sha does not match the current file' }, ctx.requestId);
    return true;
  }
  const baseTree = parseCommit(repo.objects.get(tip)!.data).tree;
  const newTree = applyTreeChanges(store, repo, baseTree, new Map([[path, undefined]]));
  const commitOid = store.createCommit(repo, newTree, [tip], `${message}\n`);
  repo.refs.set(branchRef, commitOid);
  sendJson(res, 200, { commit: { sha: commitOid, message } }, ctx.requestId);
  return true;
}

function handleLabels(store: Store, ctx: Ctx, res: ServerResponse, owner: string, name: string): boolean {
  const repo = findRepo(store, ctx, res, owner, name);
  if (!repo) return true;
  if (ctx.method === 'GET') {
    sendJson(res, 200, [...repo.labels.values()].map((label) => ({ name: label.name, color: label.color })), ctx.requestId);
    return true;
  }
  if (ctx.method === 'POST') {
    const body = readBody(ctx, res);
    if (!body) return true;
    const labelName = asString(body.name);
    if (!labelName) {
      sendJson(res, 422, { message: 'name is required' }, ctx.requestId);
      return true;
    }
    if (repo.labels.has(labelName)) {
      sendJson(res, 422, { message: 'Label already exists' }, ctx.requestId);
      return true;
    }
    const color = asString(body.color) ?? 'ededed';
    repo.labels.set(labelName, { name: labelName, color });
    sendJson(res, 201, { name: labelName, color }, ctx.requestId);
    return true;
  }
  return false;
}

function normalizeLabelNames(value: unknown): string[] | undefined {
  if (value === undefined) return [];
  if (Array.isArray(value)) {
    const names: string[] = [];
    for (const item of value) {
      if (typeof item === 'string') names.push(item);
      else if (isRecord(item) && typeof item.name === 'string') names.push(item.name);
      else return undefined;
    }
    return names;
  }
  return undefined;
}

function handleIssues(store: Store, ctx: Ctx, res: ServerResponse, owner: string, name: string): boolean {
  const repo = findRepo(store, ctx, res, owner, name);
  if (!repo) return true;
  if (ctx.method === 'GET') {
    const state = ctx.query.get('state') ?? 'open';
    const sort = ctx.query.get('sort') ?? 'created';
    const direction = ctx.query.get('direction') ?? 'desc';
    let issues = repo.issues.filter((issue) => state === 'all' || issue.state === state);
    issues = [...issues].sort((a, b) => (sort === 'created' ? a.number - b.number : a.number - b.number));
    if (direction === 'desc') issues.reverse();
    const { page, link } = paginate(issues, ctx);
    sendJson(res, 200, page.map((issue) => issueJson(repo, issue)), ctx.requestId, linkHeaders(link));
    return true;
  }
  if (ctx.method === 'POST') {
    const body = readBody(ctx, res);
    if (!body) return true;
    const title = asString(body.title);
    if (!title) {
      sendJson(res, 422, { message: 'title is required' }, ctx.requestId);
      return true;
    }
    const labels = normalizeLabelNames(body.labels);
    if (labels === undefined) {
      sendJson(res, 422, { message: 'labels must be an array of label names' }, ctx.requestId);
      return true;
    }
    for (const label of labels) {
      if (!repo.labels.has(label)) {
        sendJson(res, 422, { message: `Label '${label}' does not exist` }, ctx.requestId);
        return true;
      }
    }
    const issue = store.createIssue(repo, { title, body: asString(body.body), labels });
    sendJson(res, 201, issueJson(repo, issue), ctx.requestId);
    return true;
  }
  return false;
}

function handleIssue(store: Store, ctx: Ctx, res: ServerResponse, owner: string, name: string, rawNumber: string): boolean {
  const repo = findRepo(store, ctx, res, owner, name);
  if (!repo) return true;
  const issue = findIssue(repo, ctx, res, rawNumber);
  if (!issue) return true;
  if (ctx.method === 'GET') {
    sendJson(res, 200, issueJson(repo, issue), ctx.requestId);
    return true;
  }
  if (ctx.method === 'PATCH') {
    const body = readBody(ctx, res);
    if (!body) return true;
    const title = asString(body.title);
    const state = asString(body.state);
    const issueBody = asString(body.body);
    if (title !== undefined) issue.title = title;
    if (issueBody !== undefined) issue.body = issueBody;
    if (state === 'open' || state === 'closed') issue.state = state;
    issue.updatedAt = FIXED_TIMESTAMP;
    sendJson(res, 200, issueJson(repo, issue), ctx.requestId);
    return true;
  }
  return false;
}

function handleIssueComments(store: Store, ctx: Ctx, res: ServerResponse, owner: string, name: string, rawNumber: string): boolean {
  const repo = findRepo(store, ctx, res, owner, name);
  if (!repo) return true;
  const issue = findIssue(repo, ctx, res, rawNumber);
  if (!issue) return true;
  if (ctx.method === 'GET') {
    sendJson(res, 200, issue.comments.map((comment) => commentJson(repo, comment)), ctx.requestId);
    return true;
  }
  if (ctx.method === 'POST') {
    const body = readBody(ctx, res);
    if (!body) return true;
    const commentBody = asString(body.body);
    if (!commentBody) {
      sendJson(res, 422, { message: 'body is required' }, ctx.requestId);
      return true;
    }
    const comment = store.addComment(issue, commentBody);
    sendJson(res, 201, commentJson(repo, comment), ctx.requestId);
    return true;
  }
  return false;
}

function handlePulls(store: Store, ctx: Ctx, res: ServerResponse, owner: string, name: string): boolean {
  const repo = findRepo(store, ctx, res, owner, name);
  if (!repo) return true;
  if (ctx.method === 'GET') {
    const state = ctx.query.get('state') ?? 'open';
    const pulls = repo.pulls.filter((pull) => state === 'all' || pull.state === state);
    const { page, link } = paginate(pulls, ctx);
    sendJson(res, 200, page.map((pull) => pullJson(repo, pull)), ctx.requestId, linkHeaders(link));
    return true;
  }
  if (ctx.method === 'POST') {
    const body = readBody(ctx, res);
    if (!body) return true;
    const title = asString(body.title);
    const head = asString(body.head);
    const base = asString(body.base);
    if (!title || !head || !base) {
      sendJson(res, 422, { message: 'title, head and base are required' }, ctx.requestId);
      return true;
    }
    const headSha = repo.refs.get(`refs/heads/${head}`);
    const baseSha = repo.refs.get(`refs/heads/${base}`);
    if (!headSha || !baseSha) {
      sendJson(res, 422, { message: 'head or base branch does not exist' }, ctx.requestId);
      return true;
    }
    const duplicate = repo.pulls.find((pull) => pull.state === 'open' && pull.headRef === head && pull.baseRef === base);
    if (duplicate) {
      sendJson(res, 422, { message: 'A pull request already exists for these branches' }, ctx.requestId);
      return true;
    }
    const pull = store.createPull(repo, { title, body: asString(body.body), headRef: head, baseRef: base, headSha, baseSha });
    sendJson(res, 201, pullJson(repo, pull), ctx.requestId);
    return true;
  }
  return false;
}

function handlePull(store: Store, ctx: Ctx, res: ServerResponse, owner: string, name: string, rawNumber: string): boolean {
  const repo = findRepo(store, ctx, res, owner, name);
  if (!repo) return true;
  const pull = findPull(repo, ctx, res, rawNumber);
  if (!pull) return true;
  if (ctx.method === 'GET') {
    sendJson(res, 200, pullJson(repo, pull), ctx.requestId);
    return true;
  }
  return false;
}

function handlePullFiles(store: Store, ctx: Ctx, res: ServerResponse, owner: string, name: string, rawNumber: string): boolean {
  if (ctx.method !== 'GET') return false;
  const repo = findRepo(store, ctx, res, owner, name);
  if (!repo) return true;
  const pull = findPull(repo, ctx, res, rawNumber);
  if (!pull) return true;
  const baseObject = repo.objects.get(pull.baseSha);
  const headObject = repo.objects.get(pull.headSha);
  if (!baseObject || baseObject.type !== 'commit' || !headObject || headObject.type !== 'commit') {
    sendJson(res, 422, { message: 'pull request commits are missing' }, ctx.requestId);
    return true;
  }
  const files = diffTrees(store, repo, parseCommit(baseObject.data).tree, parseCommit(headObject.data).tree);
  sendJson(res, 200, files.map((file) => ({
    filename: file.filename,
    status: file.status,
    additions: file.additions,
    deletions: file.deletions,
    changes: file.changes,
  })), ctx.requestId);
  return true;
}

function handlePullMerge(store: Store, ctx: Ctx, res: ServerResponse, owner: string, name: string, rawNumber: string): boolean {
  if (ctx.method !== 'PUT') return false;
  const repo = findRepo(store, ctx, res, owner, name);
  if (!repo) return true;
  const pull = findPull(repo, ctx, res, rawNumber);
  if (!pull) return true;
  if (pull.state === 'closed') {
    sendJson(res, 405, { message: 'Pull request is already closed' }, ctx.requestId);
    return true;
  }
  const merged = squashMergePull(store, repo, pull);
  if (!merged) {
    sendJson(res, 405, { message: 'Merge conflict' }, ctx.requestId);
    return true;
  }
  sendJson(res, 200, {
    merged: true,
    sha: merged,
    message: `Merge pull request #${pull.number} from ${repo.owner}/${pull.headRef}`,
  }, ctx.requestId);
  return true;
}

/**
 * Handle a request under /api/v3. Returns true when the path was a REST
 * endpoint (handled, even on error); false when it is not one.
 */
export function handleRest(store: Store, ctx: Ctx, res: ServerResponse): boolean {
  const rest = ctx.segments.slice(2);
  if (rest.length === 0) {
    if (ctx.method !== 'GET') return false;
    sendJson(res, 200, {
      current_user_url: '/api/v3/user',
      repository_url: '/api/v3/repos/{owner}/{repo}',
    }, ctx.requestId, { 'X-OAuth-Scopes': 'repo, user' });
    return true;
  }
  if (rest.length === 1 && rest[0] === 'meta') {
    if (ctx.method !== 'GET') return false;
    sendJson(res, 200, {
      installed_version: '1.0.0',
      verifiable_password_authentication: false,
      hooks: [],
      git: [],
      pages: [],
    }, ctx.requestId);
    return true;
  }
  if (rest.length === 1 && rest[0] === 'user') {
    if (ctx.method !== 'GET') return false;
    sendJson(res, 200, viewerJson(), ctx.requestId);
    return true;
  }
  if (rest.length === 2 && rest[0] === 'user' && rest[1] === 'repos') {
    return handleUserRepos(store, ctx, res);
  }
  if (rest[0] === 'repos' && rest.length >= 3) {
    const owner = rest[1];
    const name = rest[2];
    const tail = rest.slice(3);
    if (tail.length === 0) return handleRepo(store, ctx, res, owner, name);
    if (tail.length === 1 && tail[0] === 'branches') return handleBranches(store, ctx, res, owner, name);
    if (tail.length >= 2 && tail[0] === 'branches') return handleBranch(store, ctx, res, owner, name, tail.slice(1).join('/'));
    if (tail.length === 1 && tail[0] === 'commits') return handleCommits(store, ctx, res, owner, name);
    if (tail.length === 2 && tail[0] === 'git' && tail[1] === 'refs') return handleCreateRef(store, ctx, res, owner, name);
    if (tail.length >= 3 && tail[0] === 'git' && tail[1] === 'refs' && tail[2] === 'heads') {
      return handleGetRef(store, ctx, res, owner, name, tail.slice(3).join('/'));
    }
    if (tail.length >= 1 && tail[0] === 'contents') {
      const path = tail.slice(1).join('/');
      if (ctx.method === 'GET') return handleGetContents(store, ctx, res, owner, name, path);
      if (ctx.method === 'PUT') return handlePutContents(store, ctx, res, owner, name, path);
      if (ctx.method === 'DELETE') return handleDeleteContents(store, ctx, res, owner, name, path);
      return false;
    }
    if (tail.length === 1 && tail[0] === 'labels') return handleLabels(store, ctx, res, owner, name);
    if (tail.length === 1 && tail[0] === 'issues') return handleIssues(store, ctx, res, owner, name);
    if (tail.length === 2 && tail[0] === 'issues') return handleIssue(store, ctx, res, owner, name, tail[1]);
    if (tail.length === 3 && tail[0] === 'issues' && tail[2] === 'comments') {
      return handleIssueComments(store, ctx, res, owner, name, tail[1]);
    }
    if (tail.length === 1 && tail[0] === 'pulls') return handlePulls(store, ctx, res, owner, name);
    if (tail.length === 2 && tail[0] === 'pulls') return handlePull(store, ctx, res, owner, name, tail[1]);
    if (tail.length === 3 && tail[0] === 'pulls' && tail[2] === 'files') {
      return handlePullFiles(store, ctx, res, owner, name, tail[1]);
    }
    if (tail.length === 3 && tail[0] === 'pulls' && tail[2] === 'merge') {
      return handlePullMerge(store, ctx, res, owner, name, tail[1]);
    }
  }
  return false;
}
