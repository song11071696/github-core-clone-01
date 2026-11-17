// app.ts — the HTTP server: routing, authentication and the request lifecycle.
//
// One process, one in-memory Store, four surfaces on the same port:
//   GET  /healthz                       the env's boot probe (no auth)
//   /*/*.git/info/refs, git-upload-pack, git-receive-pack   git smart HTTP
//   /api/v3/...                         REST v3
//   /api/graphql                        GraphQL
//   /mcp                                Model Context Protocol
//
// Every response carries X-GitHub-Request-Id (req-<counter>) and is JSON, even
// the errors: the suite rejects HTML bodies. Authentication is a Bearer token
// compared against MAKEFUN_SHOP_TOKEN; when the variable is unset (local
// development) any non-empty Bearer token is accepted. A request without a
// token is a 401, never a 404.

import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import { Store } from './store.ts';
import { handleGit } from './git.ts';
import { handleRest } from './rest.ts';
import { handleGraphql } from './graphql.ts';
import { handleMcp } from './mcp.ts';
import { newRequestId, sendJson, type Ctx } from './http.ts';

const GIT_PATH = /^\/([^/]+)\/([^/]+)\.git(\/.*)?$/;

function decodeSegment(segment: string): string {
  try {
    return decodeURIComponent(segment);
  } catch {
    return segment;
  }
}

function isAuthorized(req: IncomingMessage, shopToken: string): boolean {
  const header = req.headers.authorization;
  if (!header || !header.startsWith('Bearer ')) return false;
  const token = header.slice('Bearer '.length).trim();
  if (token === '') return false;
  if (shopToken === '') return true;
  return token === shopToken;
}

/** Build the server. It binds nothing until the caller listens; the entry listens on an ephemeral port. */
export function createApp(): Server {
  const store = new Store();
  const shopToken = process.env.MAKEFUN_SHOP_TOKEN ?? '';

  return createServer((req: IncomingMessage, res: ServerResponse) => {
    const chunks: Buffer[] = [];
    req.on('data', (chunk: Buffer) => chunks.push(chunk));
    req.on('end', () => {
      const body = Buffer.concat(chunks);
      const requestId = newRequestId(store);
      try {
        handleRequest(store, shopToken, req, res, body, requestId);
      } catch (error) {
        if (!res.headersSent) {
          sendJson(res, 500, { message: 'Internal Server Error' }, requestId);
        } else {
          res.end();
        }
        process.stderr.write(`[app] ${error instanceof Error ? error.message : String(error)}\n`);
      }
    });
    req.on('error', () => {
      if (!res.headersSent) {
        sendJson(res, 500, { message: 'Internal Server Error' }, newRequestId(store));
      } else {
        res.end();
      }
    });
  });
}

function handleRequest(store: Store, shopToken: string, req: IncomingMessage, res: ServerResponse, body: Buffer, requestId: string): void {
  let url: URL;
  try {
    url = new URL(req.url ?? '/', `http://${req.headers.host ?? '127.0.0.1'}`);
  } catch {
    sendJson(res, 404, { message: 'Not Found' }, requestId);
    return;
  }
  const path = url.pathname;
  const method = req.method ?? 'GET';
  const segments = path
    .split('/')
    .filter((segment) => segment !== '')
    .map(decodeSegment);
  const ctx: Ctx = {
    method,
    path,
    segments,
    query: url.searchParams,
    headers: req.headers,
    body,
    host: req.headers.host ?? '127.0.0.1',
    requestId,
  };

  // The env's boot check calls this twice: 200 {"ok":true}. No authentication.
  if (method === 'GET' && path === '/healthz') {
    sendJson(res, 200, { ok: true }, requestId);
    return;
  }

  const unauthorized = (): void => {
    sendJson(res, 401, { message: 'Requires authentication' }, requestId);
  };

  const gitMatch = GIT_PATH.exec(path);
  if (gitMatch) {
    if (!isAuthorized(req, shopToken)) {
      unauthorized();
      return;
    }
    const owner = decodeSegment(gitMatch[1]);
    const repoName = decodeSegment(gitMatch[2]);
    const subpath = gitMatch[3] ?? '';
    if (handleGit(store, ctx, res, owner, repoName, subpath)) return;
    sendJson(res, 404, { message: 'Not Found' }, requestId);
    return;
  }

  if (segments[0] === 'api' && segments[1] === 'v3') {
    if (!isAuthorized(req, shopToken)) {
      unauthorized();
      return;
    }
    if (handleRest(store, ctx, res)) return;
    sendJson(res, 404, { message: 'Not Found' }, requestId);
    return;
  }

  if (path === '/api/graphql') {
    if (!isAuthorized(req, shopToken)) {
      unauthorized();
      return;
    }
    if (handleGraphql(store, ctx, res)) return;
    sendJson(res, 404, { message: 'Not Found' }, requestId);
    return;
  }

  if (path === '/mcp') {
    if (!isAuthorized(req, shopToken)) {
      unauthorized();
      return;
    }
    if (handleMcp(store, ctx, res)) return;
    sendJson(res, 404, { message: 'Not Found' }, requestId);
    return;
  }

  sendJson(res, 404, { message: 'Not Found' }, requestId);
}
