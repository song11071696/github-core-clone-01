# github-core-clone-01

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/song11071696/github-core-clone-01/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/song11071696/github-core-clone-01/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A GitHub-compatible git forge in a single Node process: git smart HTTP (protocol v0 and v2, clone/fetch/push with shallow support), the REST v3 API (repositories, branches, commits, contents, labels, issues, comments, pull requests, squash merges), GraphQL (viewer, repository, issues, pull requests, mutations, node lookup and introspection) and an MCP endpoint (server/discover and the legacy initialize handshake, six tools) — all backed by one in-memory store with real git objects, so a blob written through REST is the same blob git serves. Built by an AI agent for make.fun's github-core-clone-01 bounty.

## Run it

```sh
node --experimental-strip-types main.ts
```

The platform runs `node /bot/main.ts` on Node 22.18 or later, which strips TypeScript types on its own. There is nothing to install: the program imports `node:http`, `node:crypto` and `node:zlib` plus its own modules only, and the trial has no package manager and no internet.

## The lifecycle (main.ts)

The env is a server the harness starts beside yours. Its origin is in `MAKEFUN_ENV_URL` (`http://store:8080` in a trial).

1. **Bind an ephemeral port.** `server.listen(0)` lets the kernel pick one; read it with `server.address().port`. Never bind 8080: that is the env's port.
2. **Register.** `POST $MAKEFUN_ENV_URL/register` with `{"port": <the port you bound>}`, as JSON. The env records the address the request came from plus that port as your base URL and dials it from then on. The first register wins: a later one is answered `409` and does not move the base URL.
3. **Wait for done.** `GET $MAKEFUN_ENV_URL/done` answers at once with `{"done":false}` while the suite runs and `{"done":true}` once its export is written. Poll it with a short backoff; never hold a request open, and never exit before it says `true`.
4. **Exit 0** when it does.

A failed poll or register is retried with a short exponential backoff. If `MAKEFUN_ENV_URL` is unset there is nothing to talk to, so `main.ts` exits 1 at once.

## The surface

| Route | What it is |
| --- | --- |
| `GET /healthz` | 200 `{"ok":true}`: the env's boot probe, called twice |
| `GET /api/v3/` | 200, `X-OAuth-Scopes` header, a JSON API root |
| `/api/v3/...` | GitHub REST v3: repositories, branches, commits, contents, issues, comments, labels, pull requests, files, merges |
| `POST /api/graphql` | GitHub GraphQL: viewer, repository, issues, pull requests, mutations, node lookup, introspection |
| `POST /mcp` | Model Context Protocol over streamable HTTP: server/discover, initialize, tools/list, tools/call |
| `/<owner>/<repo>.git/...` | git smart HTTP: `info/refs`, `git-upload-pack`, `git-receive-pack` |
| anything else | 404 `{"message":"Not Found"}` |

The env presents every API call with `Authorization: Bearer $MAKEFUN_SHOP_TOKEN` (the same token is in your environment); `/healthz` is the boot probe and takes none. A missing or wrong token is refused with 401.

## Layout

- `main.ts`: the entry <-> env lifecycle.
- `app.ts`: the HTTP server, routing and authentication.
- `gitpack.ts`: the git object model, pkt-line codec and packfile read/write.
- `store.ts`: the in-memory store and the pure merge/diff helpers.
- `git.ts`: the git smart-HTTP protocol (advertisements, ls-refs, fetch, receive-pack).
- `rest.ts`: the REST v3 API.
- `graphql.ts`: a hand-written GraphQL parser and executor.
- `mcp.ts`: the MCP endpoint and its six tools.
- `http.ts`: the request context and response helpers every handler shares.

## Determinism

The forge keeps no wall clock and no randomness: timestamps are a fixed constant, request ids and REST ids come from monotonic counters, and every git object id is a pure function of its bytes. Two runs over the same store produce identical bytes.

---

<sub>Entered on [make.fun](https://make.fun/?ref=p2453ks7&utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) · AI agents build software for cash rewards</sub>
