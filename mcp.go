// mcp.go — the Model Context Protocol endpoint (streamable HTTP, single JSON replies).
//
// A faithful port of the TypeScript mcp.ts. Only the Go standard library.
//
// Speaks both eras the kit's client knows: the modern 2026-07-28 `server/discover`
// handshake and the legacy `initialize` + `notifications/initialized` handshake.
// Six tools are exposed over the same in-memory store the REST, GraphQL and git
// endpoints use, so a repository created through MCP is the same object REST reads.
// Tool results carry both a text content block (the JSON string) and
// structuredContent. Unknown tools answer -32601, missing parameters answer
// with isError, never with an HTTP 500.

package main

import (
	"math"
	"strconv"
	"strings"
)

const MCPServerName = "github-core-clone-01"
const MCPServerVersion = "1.0.0"

var mcpSupportedVersions = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}
var mcpLegacyVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

type mcpToolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func mcpSchema(properties map[string]struct {
	Type        string
	Description string
}, required []string) map[string]any {
	props := map[string]any{}
	for name, def := range properties {
		props[name] = map[string]any{"type": def.Type, "description": def.Description}
	}
	return map[string]any{
		"type":                 "object",
		"properties":           props,
		"required":             required,
		"additionalProperties": true,
	}
}

// The tool list is a frozen constant: two tools/list calls serialize identically.
var mcpTools = []mcpToolDefinition{
	{
		Name:        "create_repo",
		Description: "Create a new repository owned by the authenticated user.",
		InputSchema: mcpSchema(map[string]struct {
			Type        string
			Description string
		}{
			"name":        {Type: "string", Description: "The repository name."},
			"description": {Type: "string", Description: "A short description of the repository."},
			"private":     {Type: "boolean", Description: "Whether the repository is private."},
			"owner":       {Type: "string", Description: "The owner login; defaults to the authenticated user."},
		}, []string{"name"}),
	},
	{
		Name:        "get_repo",
		Description: "Get a repository by owner and name.",
		InputSchema: mcpSchema(map[string]struct {
			Type        string
			Description string
		}{
			"owner": {Type: "string", Description: "The owner login."},
			"repo":  {Type: "string", Description: "The repository name."},
		}, []string{"owner", "repo"}),
	},
	{
		Name:        "create_issue",
		Description: "Create an issue in a repository.",
		InputSchema: mcpSchema(map[string]struct {
			Type        string
			Description string
		}{
			"owner": {Type: "string", Description: "The owner login."},
			"repo":  {Type: "string", Description: "The repository name."},
			"title": {Type: "string", Description: "The issue title."},
			"body":  {Type: "string", Description: "The issue body."},
		}, []string{"owner", "repo", "title"}),
	},
	{
		Name:        "list_issues",
		Description: "List issues in a repository, optionally filtered by state.",
		InputSchema: mcpSchema(map[string]struct {
			Type        string
			Description string
		}{
			"owner": {Type: "string", Description: "The owner login."},
			"repo":  {Type: "string", Description: "The repository name."},
			"state": {Type: "string", Description: "open, closed or all."},
		}, []string{"owner", "repo"}),
	},
	{
		Name:        "create_pull_request",
		Description: "Open a pull request from a head branch onto a base branch.",
		InputSchema: mcpSchema(map[string]struct {
			Type        string
			Description string
		}{
			"owner": {Type: "string", Description: "The owner login."},
			"repo":  {Type: "string", Description: "The repository name."},
			"title": {Type: "string", Description: "The pull request title."},
			"head":  {Type: "string", Description: "The head branch name."},
			"base":  {Type: "string", Description: "The base branch name."},
			"body":  {Type: "string", Description: "The pull request body."},
		}, []string{"owner", "repo", "title", "head", "base"}),
	},
	{
		Name:        "merge_pull_request",
		Description: "Squash-merge a pull request into its base branch.",
		InputSchema: mcpSchema(map[string]struct {
			Type        string
			Description string
		}{
			"owner":       {Type: "string", Description: "The owner login."},
			"repo":        {Type: "string", Description: "The repository name."},
			"pull_number": {Type: "number", Description: "The pull request number."},
		}, []string{"owner", "repo", "pull_number"}),
	},
}

type mcpToolContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type mcpToolResult struct {
	Content           []mcpToolContentBlock `json:"content"`
	StructuredContent map[string]any        `json:"structuredContent"`
	IsError           bool                  `json:"isError,omitempty"`
}

func mcpToolOk(data map[string]any) mcpToolResult {
	return mcpToolResult{
		Content:           []mcpToolContentBlock{{Type: "text", Text: string(MarshalJSON(data))}},
		StructuredContent: data,
	}
}

func mcpToolError(message string) mcpToolResult {
	data := map[string]any{"error": message}
	return mcpToolResult{
		Content:           []mcpToolContentBlock{{Type: "text", Text: string(MarshalJSON(data))}},
		StructuredContent: data,
		IsError:           true,
	}
}

// Accept the standard property names plus the common aliases the kit may send.
func mcpArgString(args map[string]any, names ...string) string {
	for _, name := range names {
		if value, ok := args[name].(string); ok && value != "" {
			return value
		}
	}
	return ""
}

func mcpArgNumber(args map[string]any, names ...string) (float64, bool) {
	for _, name := range names {
		value, ok := args[name]
		if !ok {
			continue
		}
		if f, ok := value.(float64); ok && !math.IsNaN(f) && !math.IsInf(f, 0) {
			return f, true
		}
		if s, ok := value.(string); ok && s != "" {
			// JavaScript's Number() trims surrounding whitespace.
			if f, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) {
				return f, true
			}
		}
	}
	return 0, false
}

func mcpArgBoolean(args map[string]any, names ...string) bool {
	for _, name := range names {
		if value, ok := args[name].(bool); ok {
			return value
		}
	}
	return false
}

func mcpResolveRepo(store *Store, args map[string]any) *Repo {
	owner := mcpArgString(args, "owner", "owner_login")
	if owner == "" {
		owner = ViewerLogin
	}
	repoName := mcpArgString(args, "repo", "repository")
	fullName := mcpArgString(args, "full_name")
	if repoName == "" && fullName != "" && strings.Contains(fullName, "/") {
		parts := strings.Split(fullName, "/")
		repoName = parts[1]
	}
	if repoName == "" {
		return nil
	}
	return store.GetRepo(owner, repoName)
}

func mcpCallTool(store *Store, name string, args map[string]any) mcpToolResult {
	switch name {
	case "create_repo":
		repoName := mcpArgString(args, "name")
		if repoName == "" {
			return mcpToolError("Missing required parameter: name")
		}
		owner := mcpArgString(args, "owner", "owner_login")
		if owner == "" {
			owner = ViewerLogin
		}
		if store.GetRepo(owner, repoName) != nil {
			return mcpToolError("Repository " + repoName + " already exists")
		}
		repo := store.CreateRepo(owner, repoName, mcpArgString(args, "description"), mcpArgBoolean(args, "private"))
		return mcpToolOk(RepoJSON(repo))
	case "get_repo":
		repo := mcpResolveRepo(store, args)
		if repo == nil {
			return mcpToolError("Repository not found")
		}
		return mcpToolOk(RepoJSON(repo))
	case "create_issue":
		repo := mcpResolveRepo(store, args)
		if repo == nil {
			return mcpToolError("Repository not found")
		}
		title := mcpArgString(args, "title")
		if title == "" {
			return mcpToolError("Missing required parameter: title")
		}
		issue := store.CreateIssue(repo, title, mcpArgString(args, "body"), nil)
		return mcpToolOk(IssueJSON(repo, issue))
	case "list_issues":
		repo := mcpResolveRepo(store, args)
		if repo == nil {
			return mcpToolError("Repository not found")
		}
		state := mcpArgString(args, "state")
		if state == "" {
			state = "open"
		}
		issues := []any{}
		for _, issue := range repo.Issues {
			if state == "all" || issue.State == state {
				issues = append(issues, IssueJSON(repo, issue))
			}
		}
		return mcpToolOk(map[string]any{"issues": issues})
	case "create_pull_request":
		repo := mcpResolveRepo(store, args)
		if repo == nil {
			return mcpToolError("Repository not found")
		}
		title := mcpArgString(args, "title")
		head := mcpArgString(args, "head", "head_branch")
		base := mcpArgString(args, "base", "base_branch")
		if title == "" {
			return mcpToolError("Missing required parameter: title")
		}
		if head == "" {
			return mcpToolError("Missing required parameter: head")
		}
		if base == "" {
			return mcpToolError("Missing required parameter: base")
		}
		headSha, headOk := repo.Refs["refs/heads/"+head]
		baseSha, baseOk := repo.Refs["refs/heads/"+base]
		if !headOk || !baseOk {
			return mcpToolError("head or base branch does not exist")
		}
		for _, pull := range repo.Pulls {
			if pull.State == "open" && pull.HeadRef == head && pull.BaseRef == base {
				return mcpToolError("A pull request already exists for these branches")
			}
		}
		pull := store.CreatePull(repo, title, mcpArgString(args, "body"), head, base, headSha, baseSha)
		return mcpToolOk(PullJSON(repo, pull))
	case "merge_pull_request":
		repo := mcpResolveRepo(store, args)
		if repo == nil {
			return mcpToolError("Repository not found")
		}
		number, ok := mcpArgNumber(args, "pull_number", "number")
		if !ok {
			return mcpToolError("Missing required parameter: pull_number")
		}
		var pull *PullRequest
		for _, item := range repo.Pulls {
			if float64(item.Number) == number {
				pull = item
				break
			}
		}
		if pull == nil {
			return mcpToolError("Pull request not found")
		}
		if pull.State == "closed" {
			return mcpToolError("Pull request is already closed")
		}
		merged := SquashMergePull(store, repo, pull)
		if merged == "" {
			return mcpToolError("Merge conflict")
		}
		return mcpToolOk(map[string]any{
			"merged":  true,
			"sha":     merged,
			"message": "Merge pull request #" + itoa(pull.Number) + " from " + repo.Owner + "/" + pull.HeadRef,
		})
	default:
		return mcpToolError("Unknown tool: " + name)
	}
}

func mcpRPCResult(id any, result any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}

func mcpRPCError(id any, code int, message string, data ...any) map[string]any {
	err := map[string]any{"code": code, "message": message}
	if len(data) > 0 && data[0] != nil {
		err["data"] = data[0]
	}
	return map[string]any{"jsonrpc": "2.0", "id": id, "error": err}
}

func mcpServerInfo() map[string]string {
	return map[string]string{"name": MCPServerName, "version": MCPServerVersion}
}

func mcpHandleOne(store *Store, request any) map[string]any {
	req, ok := request.(map[string]any)
	if !ok {
		return mcpRPCError(nil, -32600, "Invalid Request")
	}
	method, ok := req["method"].(string)
	if !ok {
		return mcpRPCError(nil, -32600, "Invalid Request")
	}
	idValue, hasID := req["id"]
	var id any
	if hasID {
		id = idValue
	}
	isNotification := !hasID
	var params map[string]any
	if p, ok := req["params"].(map[string]any); ok {
		params = p
	} else {
		params = map[string]any{}
	}

	if strings.HasPrefix(method, "notifications/") {
		return nil
	}
	if isNotification {
		return nil
	}

	switch method {
	case "server/discover":
		return mcpRPCResult(id, map[string]any{
			"supportedVersions": mcpSupportedVersions,
			"capabilities":      map[string]any{"tools": map[string]any{}},
			"serverInfo":        mcpServerInfo(),
			"_meta":             map[string]any{"io.modelcontextprotocol/serverInfo": mcpServerInfo()},
		})
	case "initialize":
		var requested any
		if v, ok := params["protocolVersion"]; ok {
			requested = v
		}
		negotiated := mcpLegacyVersions[0]
		if s, ok := requested.(string); ok && containsString(mcpLegacyVersions, s) {
			negotiated = s
		}
		return mcpRPCResult(id, map[string]any{
			"protocolVersion": negotiated,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      mcpServerInfo(),
		})
	case "ping":
		return mcpRPCResult(id, map[string]any{})
	case "tools/list":
		return mcpRPCResult(id, map[string]any{"tools": mcpTools})
	case "tools/call":
		name, _ := params["name"].(string)
		var tool *mcpToolDefinition
		for i := range mcpTools {
			if mcpTools[i].Name == name {
				tool = &mcpTools[i]
				break
			}
		}
		if tool == nil {
			return mcpRPCError(id, -32601, "Method not found: "+name)
		}
		args, _ := params["arguments"].(map[string]any)
		if args == nil {
			args = map[string]any{}
		}
		result := mcpCallTool(store, name, args)
		return mcpRPCResult(id, result)
	default:
		return mcpRPCError(id, -32601, "Method not found: "+method)
	}
}

// Handle POST /mcp. Returns true when the path was the MCP endpoint.
func HandleMCP(store *Store, ctx *Ctx, res Responder) bool {
	if ctx.Method != "POST" {
		return false
	}
	parsed := ParseJSONBody(ctx.Body)
	if !parsed.OK {
		SendJSON(res, 400, mcpRPCError(nil, -32700, "Parse error"), ctx.RequestID, map[string]string{"Mcp-Session-Id": "session-1"})
		return true
	}
	var requests []any
	if arr, ok := parsed.Value.([]any); ok {
		requests = arr
	} else {
		requests = []any{parsed.Value}
	}
	var responses []map[string]any
	for _, request := range requests {
		if response := mcpHandleOne(store, request); response != nil {
			responses = append(responses, response)
		}
	}
	if len(responses) == 0 {
		res.WriteHead(202, map[string]string{"Mcp-Session-Id": "session-1", "X-GitHub-Request-Id": ctx.RequestID})
		res.End(nil)
		return true
	}
	var body any
	if _, isArray := parsed.Value.([]any); isArray {
		body = responses
	} else {
		body = responses[0]
	}
	SendJSON(res, 200, body, ctx.RequestID, map[string]string{"Mcp-Session-Id": "session-1"})
	return true
}
