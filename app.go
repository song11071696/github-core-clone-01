// app.go — the HTTP server: routing, authentication and the request lifecycle.
//
// A faithful port of the TypeScript app.ts. One process, one in-memory store,
// four surfaces on the same port: git smart HTTP, REST v3, GraphQL, MCP.
//
// The transport is a raw TCP HTTP/1.1 server (rawhttp.go), not net/http.
// Every response carries X-GitHub-Request-Id (req-<counter>) and is JSON, even
// the errors. Authentication is a Bearer token compared against
// MAKEFUN_SHOP_TOKEN; when the variable is unset (local development) any
// non-empty Bearer token is accepted. A request without a token is a 401,
// never a 404.
//
// Locking: the store is guarded by a RWMutex. Read-only requests (GETs and
// git fetch) take a shared read lock so the 50-concurrent-clone workload
// overlaps; writes take the exclusive lock. Each request is atomic, matching
// the Node single-threaded semantics.

package main

import (
	"net/url"
	"os"
	"regexp"
	"strings"
)

var gitPathRe = regexp.MustCompile(`^/([^/]+)/([^/]+)\.git(/.*)?$`)

func decodeSegment(segment string) string {
	if decoded, err := url.PathUnescape(segment); err == nil {
		return decoded
	}
	return segment
}

func isAuthorized(headers map[string]string, shopToken string) bool {
	header, ok := headers["authorization"]
	if !ok || !strings.HasPrefix(header, "Bearer ") {
		return false
	}
	token := strings.TrimSpace(header[len("Bearer "):])
	if token == "" {
		return false
	}
	if shopToken == "" {
		return true
	}
	return token == shopToken
}

// Build the request handler. The store is created here and shared by all requests.
func newHandler() func(req *RawRequest, res Responder) {
	store := NewStore()
	shopToken := os.Getenv("MAKEFUN_SHOP_TOKEN")

	return func(req *RawRequest, res Responder) {
		requestID := NewRequestID(store)
		segments := []string{}
		for _, seg := range strings.Split(req.Path, "/") {
			if seg != "" {
				segments = append(segments, decodeSegment(seg))
			}
		}
		query, _ := url.ParseQuery(req.QueryString)
		host := req.Headers["host"]
		if host == "" {
			host = "127.0.0.1"
		}
		ctx := &Ctx{
			Method:    req.Method,
			Path:      req.Path,
			Segments:  segments,
			Query:     query,
			Headers:   req.Headers,
			Body:      req.Body,
			Host:      host,
			RequestID: requestID,
		}
		// Read-only requests take the shared lock; everything else takes the
		// exclusive lock. The whole handler (including the response write) runs
		// under the lock so each request is atomic.
		readOnly := ctx.Method == "GET" ||
			(ctx.Method == "POST" && strings.HasSuffix(ctx.Path, "/git-upload-pack"))
		if readOnly {
			store.RLock()
			defer store.RUnlock()
		} else {
			store.Lock()
			defer store.Unlock()
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					if !res.HeadersSent() {
						SendJSON(res, 500, map[string]any{"message": "Internal Server Error"}, requestID, nil)
					}
				}
			}()
			handleRequest(store, shopToken, ctx, res)
		}()
	}
}

func handleRequest(store *Store, shopToken string, ctx *Ctx, res Responder) {
	path := ctx.Path
	method := ctx.Method
	requestID := ctx.RequestID

	// The env's boot check calls this twice: 200 {"ok":true}. No authentication.
	if method == "GET" && path == "/healthz" {
		SendJSON(res, 200, map[string]any{"ok": true}, requestID, nil)
		return
	}

	unauthorized := func() {
		SendJSON(res, 401, map[string]any{"message": "Requires authentication"}, requestID, nil)
	}

	gitMatch := gitPathRe.FindStringSubmatch(path)
	if gitMatch != nil {
		if !isAuthorized(ctx.Headers, shopToken) {
			unauthorized()
			return
		}
		owner := decodeSegment(gitMatch[1])
		repoName := decodeSegment(gitMatch[2])
		subpath := gitMatch[3]
		if HandleGit(store, ctx, res, owner, repoName, subpath) {
			return
		}
		SendJSON(res, 404, map[string]any{"message": "Not Found"}, requestID, nil)
		return
	}

	if len(ctx.Segments) >= 2 && ctx.Segments[0] == "api" && ctx.Segments[1] == "v3" {
		if !isAuthorized(ctx.Headers, shopToken) {
			unauthorized()
			return
		}
		if HandleRest(store, ctx, res) {
			return
		}
		SendJSON(res, 404, map[string]any{"message": "Not Found"}, requestID, nil)
		return
	}

	if path == "/api/graphql" {
		if !isAuthorized(ctx.Headers, shopToken) {
			unauthorized()
			return
		}
		if HandleGraphQL(store, ctx, res) {
			return
		}
		SendJSON(res, 404, map[string]any{"message": "Not Found"}, requestID, nil)
		return
	}

	if path == "/mcp" {
		if !isAuthorized(ctx.Headers, shopToken) {
			unauthorized()
			return
		}
		if HandleMCP(store, ctx, res) {
			return
		}
		SendJSON(res, 404, map[string]any{"message": "Not Found"}, requestID, nil)
		return
	}

	SendJSON(res, 404, map[string]any{"message": "Not Found"}, requestID, nil)
}
