// mcp.ts — the Model Context Protocol endpoint (streamable HTTP, single JSON replies).
//
// Speaks both eras the kit's client knows: the modern 2026-07-28 `server/discover`
// handshake and the legacy `initialize` + `notifications/initialized` handshake.
// Six tools are exposed over the same in-memory store the REST, GraphQL and git
// endpoints use, so a repository created through MCP is the same object REST reads.
// Tool results carry both a text content block (the JSON string) and
// structuredContent. Unknown tools answer -32601, missing parameters answer
// with isError, never with an HTTP 500.

import type { ServerResponse } from 'node:http';
import { VIEWER_LOGIN, squashMergePull, type Repo, type Store } from './store.ts';
import { issueJson, pullJson, repoJson } from './rest.ts';
import { isRecord, parseJsonBody, sendJson, type Ctx } from './http.ts';

export const MCP_SERVER_NAME = 'github-core-clone-01';
export const MCP_SERVER_VERSION = '1.0.0';

const SUPPORTED_VERSIONS = ['2026-07-28', '2025-11-25', '2025-06-18', '2025-03-26', '2024-11-05'];
const LEGACY_VERSIONS = ['2025-11-25', '2025-06-18', '2025-03-26', '2024-11-05'];

interface ToolDefinition {
  readonly name: string;
  readonly description: string;
  readonly inputSchema: Record<string, unknown>;
}

function schema(properties: Record<string, { type: string; description: string }>, required: string[]): Record<string, unknown> {
  return {
    type: 'object',
    properties: Object.fromEntries(
      Object.entries(properties).map(([name, def]) => [name, { type: def.type, description: def.description }]),
    ),
    required,
    additionalProperties: true,
  };
}

/** The tool list is a frozen constant: two tools/list calls serialize identically. */
export const TOOLS: readonly ToolDefinition[] = Object.freeze([
  {
    name: 'create_repo',
    description: 'Create a new repository owned by the authenticated user.',
    inputSchema: schema(
      {
        name: { type: 'string', description: 'The repository name.' },
        description: { type: 'string', description: 'A short description of the repository.' },
        private: { type: 'boolean', description: 'Whether the repository is private.' },
        owner: { type: 'string', description: 'The owner login; defaults to the authenticated user.' },
      },
      ['name'],
    ),
  },
  {
    name: 'get_repo',
    description: 'Get a repository by owner and name.',
    inputSchema: schema(
      {
        owner: { type: 'string', description: 'The owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
      },
      ['owner', 'repo'],
    ),
  },
  {
    name: 'create_issue',
    description: 'Create an issue in a repository.',
    inputSchema: schema(
      {
        owner: { type: 'string', description: 'The owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
        title: { type: 'string', description: 'The issue title.' },
        body: { type: 'string', description: 'The issue body.' },
      },
      ['owner', 'repo', 'title'],
    ),
  },
  {
    name: 'list_issues',
    description: 'List issues in a repository, optionally filtered by state.',
    inputSchema: schema(
      {
        owner: { type: 'string', description: 'The owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
        state: { type: 'string', description: 'open, closed or all.' },
      },
      ['owner', 'repo'],
    ),
  },
  {
    name: 'create_pull_request',
    description: 'Open a pull request from a head branch onto a base branch.',
    inputSchema: schema(
      {
        owner: { type: 'string', description: 'The owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
        title: { type: 'string', description: 'The pull request title.' },
        head: { type: 'string', description: 'The head branch name.' },
        base: { type: 'string', description: 'The base branch name.' },
        body: { type: 'string', description: 'The pull request body.' },
      },
      ['owner', 'repo', 'title', 'head', 'base'],
    ),
  },
  {
    name: 'merge_pull_request',
    description: 'Squash-merge a pull request into its base branch.',
    inputSchema: schema(
      {
        owner: { type: 'string', description: 'The owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
        pull_number: { type: 'number', description: 'The pull request number.' },
      },
      ['owner', 'repo', 'pull_number'],
    ),
  },
]);

interface ToolResult {
  content: { type: 'text'; text: string }[];
  structuredContent: Record<string, unknown>;
  isError?: boolean;
}

function toolOk(data: Record<string, unknown>): ToolResult {
  return { content: [{ type: 'text', text: JSON.stringify(data) }], structuredContent: data };
}

function toolError(message: string): ToolResult {
  const data = { error: message };
  return { content: [{ type: 'text', text: JSON.stringify(data) }], structuredContent: data, isError: true };
}

/** Accept the standard property names plus the common aliases the kit may send. */
function argString(args: Record<string, unknown>, ...names: string[]): string | undefined {
  for (const name of names) {
    const value = args[name];
    if (typeof value === 'string' && value !== '') return value;
  }
  return undefined;
}

function argNumber(args: Record<string, unknown>, ...names: string[]): number | undefined {
  for (const name of names) {
    const value = args[name];
    if (typeof value === 'number' && Number.isFinite(value)) return value;
    if (typeof value === 'string' && value !== '' && Number.isFinite(Number(value))) return Number(value);
  }
  return undefined;
}

function argBoolean(args: Record<string, unknown>, ...names: string[]): boolean | undefined {
  for (const name of names) {
    const value = args[name];
    if (typeof value === 'boolean') return value;
  }
  return undefined;
}

function resolveRepo(store: Store, args: Record<string, unknown>): Repo | undefined {
  const owner = argString(args, 'owner', 'owner_login') ?? VIEWER_LOGIN;
  let repoName = argString(args, 'repo', 'repository');
  const fullName = argString(args, 'full_name');
  if (!repoName && fullName && fullName.includes('/')) {
    const parts = fullName.split('/');
    repoName = parts[1];
  }
  if (!repoName) return undefined;
  return store.getRepo(owner, repoName);
}

function callTool(store: Store, name: string, args: Record<string, unknown>): ToolResult {
  switch (name) {
    case 'create_repo': {
      const repoName = argString(args, 'name');
      if (!repoName) return toolError('Missing required parameter: name');
      const owner = argString(args, 'owner', 'owner_login') ?? VIEWER_LOGIN;
      if (store.getRepo(owner, repoName)) return toolError(`Repository ${repoName} already exists`);
      const repo = store.createRepo(owner, repoName, {
        description: argString(args, 'description'),
        isPrivate: argBoolean(args, 'private'),
      })!;
      return toolOk(repoJson(repo) as Record<string, unknown>);
    }
    case 'get_repo': {
      const repo = resolveRepo(store, args);
      if (!repo) return toolError('Repository not found');
      return toolOk(repoJson(repo) as Record<string, unknown>);
    }
    case 'create_issue': {
      const repo = resolveRepo(store, args);
      if (!repo) return toolError('Repository not found');
      const title = argString(args, 'title');
      if (!title) return toolError('Missing required parameter: title');
      const issue = store.createIssue(repo, { title, body: argString(args, 'body') });
      return toolOk(issueJson(repo, issue) as Record<string, unknown>);
    }
    case 'list_issues': {
      const repo = resolveRepo(store, args);
      if (!repo) return toolError('Repository not found');
      const state = argString(args, 'state') ?? 'open';
      const issues = repo.issues.filter((issue) => state === 'all' || issue.state === state);
      return toolOk({ issues: issues.map((issue) => issueJson(repo, issue)) });
    }
    case 'create_pull_request': {
      const repo = resolveRepo(store, args);
      if (!repo) return toolError('Repository not found');
      const title = argString(args, 'title');
      const head = argString(args, 'head', 'head_branch');
      const base = argString(args, 'base', 'base_branch');
      if (!title) return toolError('Missing required parameter: title');
      if (!head) return toolError('Missing required parameter: head');
      if (!base) return toolError('Missing required parameter: base');
      const headSha = repo.refs.get(`refs/heads/${head}`);
      const baseSha = repo.refs.get(`refs/heads/${base}`);
      if (!headSha || !baseSha) return toolError('head or base branch does not exist');
      const duplicate = repo.pulls.find((pull) => pull.state === 'open' && pull.headRef === head && pull.baseRef === base);
      if (duplicate) return toolError('A pull request already exists for these branches');
      const pull = store.createPull(repo, { title, body: argString(args, 'body'), headRef: head, baseRef: base, headSha, baseSha });
      return toolOk(pullJson(repo, pull) as Record<string, unknown>);
    }
    case 'merge_pull_request': {
      const repo = resolveRepo(store, args);
      if (!repo) return toolError('Repository not found');
      const number = argNumber(args, 'pull_number', 'number');
      if (number === undefined) return toolError('Missing required parameter: pull_number');
      const pull = repo.pulls.find((item) => item.number === number);
      if (!pull) return toolError('Pull request not found');
      if (pull.state === 'closed') return toolError('Pull request is already closed');
      const merged = squashMergePull(store, repo, pull);
      if (!merged) return toolError('Merge conflict');
      return toolOk({ merged: true, sha: merged, message: `Merge pull request #${pull.number} from ${repo.owner}/${pull.headRef}` });
    }
    default:
      return toolError(`Unknown tool: ${name}`);
  }
}

function rpcResult(id: unknown, result: unknown): Record<string, unknown> {
  return { jsonrpc: '2.0', id, result };
}

function rpcError(id: unknown, code: number, message: string, data?: unknown): Record<string, unknown> {
  const error: Record<string, unknown> = { code, message };
  if (data !== undefined) error.data = data;
  return { jsonrpc: '2.0', id, error };
}

function serverInfo(): Record<string, string> {
  return { name: MCP_SERVER_NAME, version: MCP_SERVER_VERSION };
}

function handleOne(store: Store, request: unknown): Record<string, unknown> | null {
  if (!isRecord(request) || typeof request.method !== 'string') {
    return rpcError(null, -32600, 'Invalid Request');
  }
  const id = 'id' in request ? request.id : undefined;
  const isNotification = id === undefined;
  const method = request.method;
  const params = isRecord(request.params) ? request.params : {};

  if (method.startsWith('notifications/')) {
    return null;
  }
  if (isNotification) {
    return null;
  }

  if (method === 'server/discover') {
    return rpcResult(id, {
      supportedVersions: SUPPORTED_VERSIONS,
      capabilities: { tools: {} },
      serverInfo: serverInfo(),
      _meta: { 'io.modelcontextprotocol/serverInfo': serverInfo() },
    });
  }
  if (method === 'initialize') {
    const requested = isRecord(params) ? params.protocolVersion : undefined;
    const negotiated =
      typeof requested === 'string' && LEGACY_VERSIONS.includes(requested) ? requested : LEGACY_VERSIONS[0];
    return rpcResult(id, {
      protocolVersion: negotiated,
      capabilities: { tools: {} },
      serverInfo: serverInfo(),
    });
  }
  if (method === 'ping') {
    return rpcResult(id, {});
  }
  if (method === 'tools/list') {
    return rpcResult(id, { tools: TOOLS });
  }
  if (method === 'tools/call') {
    const name = typeof params.name === 'string' ? params.name : '';
    const tool = TOOLS.find((item) => item.name === name);
    if (!tool) return rpcError(id, -32601, `Method not found: ${name}`);
    const args = isRecord(params.arguments) ? (params.arguments as Record<string, unknown>) : {};
    const result = callTool(store, name, args);
    return rpcResult(id, result);
  }
  return rpcError(id, -32601, `Method not found: ${method}`);
}

/** Handle POST /mcp. Returns true when the path was the MCP endpoint. */
export function handleMcp(store: Store, ctx: Ctx, res: ServerResponse): boolean {
  if (ctx.method !== 'POST') return false;
  const parsed = parseJsonBody(ctx.body);
  if (!parsed.ok) {
    sendJson(res, 400, rpcError(null, -32700, 'Parse error'), ctx.requestId, { 'Mcp-Session-Id': 'session-1' });
    return true;
  }
  const requests = Array.isArray(parsed.value) ? parsed.value : [parsed.value];
  const responses: Record<string, unknown>[] = [];
  for (const request of requests) {
    const response = handleOne(store, request);
    if (response !== null) responses.push(response);
  }
  if (responses.length === 0) {
    res.writeHead(202, { 'Mcp-Session-Id': 'session-1', 'X-GitHub-Request-Id': ctx.requestId });
    res.end();
    return true;
  }
  sendJson(res, 200, Array.isArray(parsed.value) ? responses : responses[0], ctx.requestId, {
    'Mcp-Session-Id': 'session-1',
  });
  return true;
}
