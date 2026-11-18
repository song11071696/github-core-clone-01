// graphql.go — a small hand-written GraphQL executor for the conformance checks.
//
// A faithful port of the TypeScript graphql.ts. It parses the query documents
// the kit sends (queries and mutations, variables, aliases, inline fragments,
// lists and input objects), executes them against the same in-memory store the
// REST and git endpoints use, and answers with `{data, errors}`. Unknown
// fields fail with the GraphQL-style message `Cannot query field "x" on
// type "Y".` Global ids are opaque base64 strings with a type prefix; they
// are stable across reads.
//
// Only the Go standard library. No store lock is taken here: app.go already
// holds the store lock for the whole handler.

package main

import (
	"encoding/base64"
	"errors"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------------------------
// lexer + parser
// ---------------------------------------------------------------------------------------------

type tokenKind int

const (
	tokPunct tokenKind = iota
	tokName
	tokString
	tokNumber
)

type token struct {
	kind tokenKind
	str  string  // punct, name and string values
	num  float64 // number values
}

var gqlNumberRe = regexp.MustCompile(`^-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?`)
var gqlNameRe = regexp.MustCompile(`^[_A-Za-z][_0-9A-Za-z]*`)

func isHexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func tokenize(input string) []token {
	runes := []rune(input)
	var tokens []token
	index := 0
	for index < len(runes) {
		char := runes[index]
		if char == ' ' || char == '\t' || char == '\n' || char == '\r' || char == ',' {
			index += 1
			continue
		}
		if char == '#' {
			for index < len(runes) && runes[index] != '\n' {
				index += 1
			}
			continue
		}
		if index+2 < len(runes) && runes[index] == '.' && runes[index+1] == '.' && runes[index+2] == '.' {
			tokens = append(tokens, token{kind: tokPunct, str: "..."})
			index += 3
			continue
		}
		if strings.ContainsRune("{}():$[]!=@", char) {
			tokens = append(tokens, token{kind: tokPunct, str: string(char)})
			index += 1
			continue
		}
		if char == '"' {
			var value strings.Builder
			index += 1
			for index < len(runes) && runes[index] != '"' {
				if runes[index] == '\\' && index+1 < len(runes) {
					escape := runes[index+1]
					switch escape {
					case 'n':
						value.WriteByte('\n')
					case 't':
						value.WriteByte('\t')
					case 'r':
						value.WriteByte('\r')
					case 'u':
						if index+5 < len(runes) {
							hexText := string(runes[index+2 : index+6])
							// JS parseInt: longest leading hex-digit prefix;
							// empty prefix is NaN, and fromCharCode(NaN) is "\0".
							j := 0
							for j < len(hexText) && isHexDigit(hexText[j]) {
								j++
							}
							if j == 0 {
								value.WriteRune(0)
							} else {
								v, _ := strconv.ParseInt(hexText[:j], 16, 32)
								value.WriteRune(rune(v))
							}
							index += 4
						} else {
							value.WriteRune(escape)
						}
					default:
						value.WriteRune(escape)
					}
					index += 2
				} else {
					value.WriteRune(runes[index])
					index += 1
				}
			}
			index += 1
			tokens = append(tokens, token{kind: tokString, str: value.String()})
			continue
		}
		if (char >= '0' && char <= '9') || char == '-' {
			if match := gqlNumberRe.FindString(string(runes[index:])); match != "" {
				// ParseFloat returns ±Inf (with an error) for huge exponents,
				// matching JS Number(); keep the value either way.
				v, _ := strconv.ParseFloat(match, 64)
				tokens = append(tokens, token{kind: tokNumber, num: v})
				index += len(match)
				continue
			}
		}
		if char == '_' || (char >= 'A' && char <= 'Z') || (char >= 'a' && char <= 'z') {
			if match := gqlNameRe.FindString(string(runes[index:])); match != "" {
				tokens = append(tokens, token{kind: tokName, str: match})
				index += len(match)
				continue
			}
		}
		panic(errors.New("unexpected character " + string(MarshalJSON(string(char))) + " in GraphQL document"))
	}
	return tokens
}

// fieldNode is one selection: a field, an alias, or an inline fragment
// (name "" and fragmentType set).
type fieldNode struct {
	name         string
	alias        string // "" means no alias
	args         map[string]any
	children     []*fieldNode
	fragmentType string // "" means not a fragment
}

func (f *fieldNode) responseName() string {
	if f.alias != "" {
		return f.alias
	}
	return f.name
}

type gqlOperation struct {
	kind       string // "query" | "mutation"
	defaults   map[string]any
	selections []*fieldNode
}

type gqlParser struct {
	pos    int
	tokens []token
}

func (p *gqlParser) peek() *token {
	if p.pos < len(p.tokens) {
		return &p.tokens[p.pos]
	}
	return nil
}

func (p *gqlParser) next() token {
	tok := p.peek()
	if tok == nil {
		panic(errors.New("unexpected end of GraphQL document"))
	}
	p.pos += 1
	return *tok
}

func (p *gqlParser) expectPunct(value string) {
	tok := p.next()
	if tok.kind != tokPunct || tok.str != value {
		panic(errors.New("expected " + string(MarshalJSON(value)) + " in GraphQL document"))
	}
}

func (p *gqlParser) isPunct(value string) bool {
	tok := p.peek()
	return tok != nil && tok.kind == tokPunct && tok.str == value
}

func (p *gqlParser) parseDocument() *gqlOperation {
	kind := "query"
	first := p.peek()
	if first != nil && first.kind == tokName && (first.str == "query" || first.str == "mutation") {
		kind = first.str
		p.next()
		maybeName := p.peek()
		if maybeName != nil && maybeName.kind == tokName {
			p.next()
		}
	}
	defaults := map[string]any{}
	if p.isPunct("(") {
		p.next()
		for !p.isPunct(")") {
			p.expectPunct("$")
			name := p.next()
			if name.kind != tokName {
				panic(errors.New("expected a variable name"))
			}
			p.expectPunct(":")
			p.skipType()
			if p.isPunct("=") {
				p.next()
				defaults[name.str] = p.parseValue()
			}
		}
		p.expectPunct(")")
	}
	selections := p.parseSelectionSet()
	return &gqlOperation{kind: kind, defaults: defaults, selections: selections}
}

// skipType skips a type reference, including list brackets.
func (p *gqlParser) skipType() {
	if p.isPunct("[") {
		p.next()
		p.skipType()
		p.expectPunct("]")
	} else {
		tok := p.next()
		if tok.kind != tokName {
			panic(errors.New("expected a type name"))
		}
	}
	if p.isPunct("!") {
		p.next()
	}
}

func (p *gqlParser) parseSelectionSet() []*fieldNode {
	p.expectPunct("{")
	var selections []*fieldNode
	for !p.isPunct("}") {
		selections = append(selections, p.parseSelection())
	}
	p.expectPunct("}")
	return selections
}

func (p *gqlParser) parseSelection() *fieldNode {
	if p.isPunct("...") {
		p.next()
		maybeOn := p.peek()
		if maybeOn != nil && maybeOn.kind == tokName && maybeOn.str == "on" {
			p.next()
			typeName := p.next()
			if typeName.kind != tokName {
				panic(errors.New("expected a type name after on"))
			}
			return &fieldNode{args: map[string]any{}, children: p.parseSelectionSet(), fragmentType: typeName.str}
		}
		panic(errors.New("named fragments are not supported"))
	}
	first := p.next()
	if first.kind != tokName {
		panic(errors.New("expected a field name"))
	}
	alias := ""
	name := first.str
	if p.isPunct(":") {
		p.next()
		real := p.next()
		if real.kind != tokName {
			panic(errors.New("expected a field name after an alias"))
		}
		alias = name
		name = real.str
	}
	args := map[string]any{}
	if p.isPunct("(") {
		p.next()
		for !p.isPunct(")") {
			argName := p.next()
			if argName.kind != tokName {
				panic(errors.New("expected an argument name"))
			}
			p.expectPunct(":")
			args[argName.str] = p.parseValue()
		}
		p.expectPunct(")")
	}
	var children []*fieldNode
	if tok := p.peek(); tok != nil && tok.kind == tokPunct && tok.str == "{" {
		children = p.parseSelectionSet()
	}
	return &fieldNode{name: name, alias: alias, args: args, children: children}
}

func (p *gqlParser) parseValue() any {
	tok := p.next()
	if tok.kind == tokPunct && tok.str == "$" {
		name := p.next()
		if name.kind != tokName {
			panic(errors.New("expected a variable name"))
		}
		return map[string]any{"__variable": name.str}
	}
	if tok.kind == tokString || tok.kind == tokNumber {
		if tok.kind == tokString {
			return tok.str
		}
		return tok.num
	}
	if tok.kind == tokPunct && tok.str == "[" {
		values := []any{}
		for {
			peek := p.peek()
			if peek != nil && peek.kind == tokPunct && peek.str == "]" {
				break
			}
			values = append(values, p.parseValue())
		}
		p.next()
		return values
	}
	if tok.kind == tokPunct && tok.str == "{" {
		object := map[string]any{}
		for {
			peek := p.peek()
			if peek != nil && peek.kind == tokPunct && peek.str == "}" {
				break
			}
			key := p.next()
			if key.kind != tokName {
				panic(errors.New("expected an object key"))
			}
			p.expectPunct(":")
			object[key.str] = p.parseValue()
		}
		p.next()
		return object
	}
	if tok.kind == tokName {
		if tok.str == "true" {
			return true
		}
		if tok.str == "false" {
			return false
		}
		if tok.str == "null" {
			return nil
		}
		return tok.str
	}
	panic(errors.New("unexpected value in GraphQL document"))
}

// substituteVariables replaces `{__variable: name}` markers with the supplied
// variable values.
func substituteVariables(value any, variables map[string]any) any {
	if arr, ok := value.([]any); ok {
		out := make([]any, 0, len(arr))
		for _, item := range arr {
			out = append(out, substituteVariables(item, variables))
		}
		return out
	}
	if m, ok := value.(map[string]any); ok && m != nil {
		if name, ok := m["__variable"].(string); ok {
			return variables[name]
		}
		out := map[string]any{}
		for key, item := range m {
			out[key] = substituteVariables(item, variables)
		}
		return out
	}
	return value
}

// ---------------------------------------------------------------------------------------------
// global ids
// ---------------------------------------------------------------------------------------------

// nodeID (shapes.go) is the global id: base64 of "Kind:value".

type decodedGlobalID struct {
	kind  string
	value string
	ok    bool
}

func decodeGlobalID(id string) decodedGlobalID {
	raw, err := base64.StdEncoding.DecodeString(id)
	if err != nil {
		return decodedGlobalID{}
	}
	text := string(raw)
	colon := strings.IndexByte(text, ':')
	if colon < 0 {
		return decodedGlobalID{}
	}
	return decodedGlobalID{kind: text[:colon], value: text[colon+1:], ok: true}
}

// ---------------------------------------------------------------------------------------------
// execution
// ---------------------------------------------------------------------------------------------

// gqlObject is a resolved GraphQL object: a map with a string __typename plus
// typed payloads under "repo", "issue", "pull" or "comment". Plain records
// (pageInfo, __Type field entries) are also map[string]any but carry no
// __typename, or a non-string one; isGqlObject tells them apart.
type gqlContext struct {
	store *Store
}

func gqlStringValue(value any) (string, bool) {
	s, ok := value.(string)
	return s, ok
}

func gqlNumberValue(value any) (float64, bool) {
	f, ok := value.(float64)
	if !ok || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

func gqlStringArrayValue(value any) ([]string, bool) {
	arr, ok := value.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		s, ok := item.(string)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

func cursorForNumber(number int) string {
	return base64.StdEncoding.EncodeToString([]byte("cursor:" + itoa(number)))
}

func numberFromCursor(cursor string) (int, bool) {
	if cursor == "" {
		return 0, false
	}
	raw, err := base64.StdEncoding.DecodeString(cursor)
	if err != nil {
		return 0, false
	}
	text := string(raw)
	if !strings.HasPrefix(text, "cursor:") {
		return 0, false
	}
	return jsParseInt(text[len("cursor:"):])
}

type gqlPage[T any] struct {
	nodes       []T
	hasNextPage bool
	endCursor   any // string, or nil
}

func paginateByCursor[T any](items []T, first int, hasFirst bool, after string, keyOf func(T) int) gqlPage[T] {
	afterNumber, hasAfter := numberFromCursor(after)
	start := 0
	if hasAfter {
		index := -1
		for i, item := range items {
			if keyOf(item) == afterNumber {
				index = i
				break
			}
		}
		if index >= 0 {
			start = index + 1
		}
	}
	if start > len(items) {
		start = len(items)
	}
	count := len(items) - start
	if hasFirst {
		count = first
	}
	if count < 0 {
		count = 0
	}
	end := start + count
	if end > len(items) {
		end = len(items)
	}
	nodes := items[start:end]
	var endCursor any
	if len(nodes) > 0 {
		endCursor = cursorForNumber(keyOf(nodes[len(nodes)-1]))
	}
	return gqlPage[T]{nodes: nodes, hasNextPage: start+len(nodes) < len(items), endCursor: endCursor}
}

func issueConnection(repo *Repo, issues []*Issue, first int, hasFirst bool, after string) map[string]any {
	page := paginateByCursor(issues, first, hasFirst, after, func(issue *Issue) int { return issue.Number })
	nodes := []any{}
	edges := []any{}
	for _, issue := range page.nodes {
		obj := issueObject(repo, issue)
		nodes = append(nodes, obj)
		edges = append(edges, map[string]any{"__typename": "IssueEdge", "node": obj, "cursor": cursorForNumber(issue.Number)})
	}
	var startCursor any
	if len(page.nodes) > 0 {
		startCursor = cursorForNumber(page.nodes[0].Number)
	}
	return map[string]any{
		"__typename": "IssueConnection",
		"nodes":      nodes,
		"edges":      edges,
		"pageInfo": map[string]any{
			"__typename": "PageInfo", "hasNextPage": page.hasNextPage, "endCursor": page.endCursor,
			"hasPreviousPage": false, "startCursor": startCursor,
		},
		"totalCount": len(issues),
	}
}

func pullConnection(repo *Repo, pulls []*PullRequest, first int, hasFirst bool, after string) map[string]any {
	page := paginateByCursor(pulls, first, hasFirst, after, func(pull *PullRequest) int { return pull.Number })
	nodes := []any{}
	edges := []any{}
	for _, pull := range page.nodes {
		obj := pullObject(repo, pull)
		nodes = append(nodes, obj)
		edges = append(edges, map[string]any{"__typename": "PullRequestEdge", "node": obj, "cursor": cursorForNumber(pull.Number)})
	}
	var startCursor any
	if len(page.nodes) > 0 {
		startCursor = cursorForNumber(page.nodes[0].Number)
	}
	return map[string]any{
		"__typename": "PullRequestConnection",
		"nodes":      nodes,
		"edges":      edges,
		"pageInfo": map[string]any{
			"__typename": "PageInfo", "hasNextPage": page.hasNextPage, "endCursor": page.endCursor,
			"hasPreviousPage": false, "startCursor": startCursor,
		},
		"totalCount": len(pulls),
	}
}

func repositoryObject(repo *Repo) map[string]any {
	return map[string]any{"__typename": "Repository", "repo": repo}
}

func issueObject(repo *Repo, issue *Issue) map[string]any {
	return map[string]any{"__typename": "Issue", "repo": repo, "issue": issue}
}

func pullObject(repo *Repo, pull *PullRequest) map[string]any {
	return map[string]any{"__typename": "PullRequest", "repo": repo, "pull": pull}
}

func commentObject(repo *Repo, comment *IssueComment) map[string]any {
	return map[string]any{"__typename": "IssueComment", "repo": repo, "comment": comment}
}

func findRepoByGlobalID(store *Store, id string) *Repo {
	decoded := decodeGlobalID(id)
	if !decoded.ok || decoded.kind != "Repository" {
		return nil
	}
	repoID, ok := jsParseInt(decoded.value)
	if !ok {
		return nil
	}
	for _, repo := range store.Repos {
		if repo.ID == repoID {
			return repo
		}
	}
	return nil
}

func findIssueByGlobalID(store *Store, id string) (*Repo, *Issue) {
	decoded := decodeGlobalID(id)
	if !decoded.ok || decoded.kind != "Issue" {
		return nil, nil
	}
	parts := strings.Split(decoded.value, ":")
	if len(parts) < 2 {
		return nil, nil
	}
	repoID, ok1 := jsParseInt(parts[0])
	number, ok2 := jsParseInt(parts[1])
	if !ok1 || !ok2 {
		return nil, nil
	}
	for _, repo := range store.Repos {
		if repo.ID != repoID {
			continue
		}
		for _, issue := range repo.Issues {
			if issue.Number == number {
				return repo, issue
			}
		}
	}
	return nil, nil
}

func findPullByGlobalID(store *Store, id string) (*Repo, *PullRequest) {
	decoded := decodeGlobalID(id)
	if !decoded.ok || decoded.kind != "PullRequest" {
		return nil, nil
	}
	parts := strings.Split(decoded.value, ":")
	if len(parts) < 2 {
		return nil, nil
	}
	repoID, ok1 := jsParseInt(parts[0])
	number, ok2 := jsParseInt(parts[1])
	if !ok1 || !ok2 {
		return nil, nil
	}
	for _, repo := range store.Repos {
		if repo.ID != repoID {
			continue
		}
		for _, pull := range repo.Pulls {
			if pull.Number == number {
				return repo, pull
			}
		}
	}
	return nil, nil
}

func unknownField(field *fieldNode, typeName string) error {
	return errors.New(`Cannot query field "` + field.name + `" on type "` + typeName + `".`)
}

// gqlIDString renders an id value the way JS String() would: numbers as
// decimal, strings as-is, missing as the fallback.
func gqlIDString(value any, fallback int) string {
	switch v := value.(type) {
	case nil:
		return itoa(fallback)
	case int:
		return itoa(v)
	case float64:
		return itoa(int(v))
	case string:
		return v
	default:
		return itoa(fallback)
	}
}

func viewerUserObject(login string) map[string]any {
	return map[string]any{"__typename": "User", "login": login, "id": ViewerID, "name": ViewerName, "email": ViewerEmail}
}

func changedFileObject(file FileChange) map[string]any {
	return map[string]any{
		"__typename": "ChangedFile", "path": file.Filename, "additions": file.Additions,
		"deletions": file.Deletions, "changes": file.Changes, "status": strings.ToUpper(file.Status),
	}
}

func emptyFileConnection() map[string]any {
	return map[string]any{
		"__typename": "FileConnection", "nodes": []any{}, "edges": []any{},
		"pageInfo": map[string]any{
			"__typename": "PageInfo", "hasNextPage": false, "endCursor": nil,
			"hasPreviousPage": false, "startCursor": nil,
		},
		"totalCount": 0,
	}
}

func resolveField(typeName string, obj map[string]any, field *fieldNode, ctx *gqlContext) any {
	store := ctx.store
	if field.name == "__typename" {
		return obj["__typename"]
	}
	switch typeName {
	case "Query":
		if field.name == "viewer" {
			return map[string]any{"__typename": "Viewer", "login": ViewerLogin, "id": ViewerID, "name": ViewerName, "email": ViewerEmail}
		}
		if field.name == "repository" {
			owner, _ := gqlStringValue(field.args["owner"])
			name, _ := gqlStringValue(field.args["name"])
			if owner == "" || name == "" {
				return nil
			}
			repo := store.GetRepo(owner, name)
			if repo == nil {
				return nil
			}
			return repositoryObject(repo)
		}
		if field.name == "node" {
			id, _ := gqlStringValue(field.args["id"])
			if id == "" {
				return nil
			}
			decoded := decodeGlobalID(id)
			if !decoded.ok {
				return nil
			}
			switch decoded.kind {
			case "Repository":
				repo := findRepoByGlobalID(store, id)
				if repo == nil {
					return nil
				}
				return repositoryObject(repo)
			case "Issue":
				repo, issue := findIssueByGlobalID(store, id)
				if repo == nil || issue == nil {
					return nil
				}
				return issueObject(repo, issue)
			case "PullRequest":
				repo, pull := findPullByGlobalID(store, id)
				if repo == nil || pull == nil {
					return nil
				}
				return pullObject(repo, pull)
			case "IssueComment":
				parts := strings.Split(decoded.value, ":")
				if len(parts) < 2 {
					return nil
				}
				repoID, ok1 := jsParseInt(parts[0])
				commentID, ok2 := jsParseInt(parts[1])
				if !ok1 || !ok2 {
					return nil
				}
				for _, repo := range store.Repos {
					if repo.ID != repoID {
						continue
					}
					for _, issue := range repo.Issues {
						for _, comment := range issue.Comments {
							if comment.ID == commentID {
								return commentObject(repo, comment)
							}
						}
					}
				}
				return nil
			}
			return nil
		}
		if field.name == "__type" {
			name, _ := gqlStringValue(field.args["name"])
			return typeIntrospection(name)
		}
		if field.name == "__schema" {
			return map[string]any{
				"__typename":   "__Schema",
				"queryType":    map[string]any{"name": "Query", "kind": "OBJECT"},
				"mutationType": map[string]any{"name": "Mutation", "kind": "OBJECT"},
			}
		}
		panic(unknownField(field, typeName))
	case "Mutation":
		if field.name == "createIssue" {
			input, _ := field.args["input"].(map[string]any)
			repoID, _ := gqlStringValue(input["repositoryId"])
			repo := findRepoByGlobalID(store, repoID)
			if repo == nil {
				panic(errors.New("Repository not found"))
			}
			title, _ := gqlStringValue(input["title"])
			if title == "" {
				panic(errors.New("title is required"))
			}
			body, _ := gqlStringValue(input["body"])
			issue := store.CreateIssue(repo, title, body, nil)
			return map[string]any{"__typename": "CreateIssuePayload", "issue": issueObject(repo, issue)}
		}
		if field.name == "closeIssue" {
			input, _ := field.args["input"].(map[string]any)
			issueID, _ := gqlStringValue(input["issueId"])
			repo, issue := findIssueByGlobalID(store, issueID)
			if repo == nil || issue == nil {
				panic(errors.New("Issue not found"))
			}
			issue.State = "closed"
			issue.UpdatedAt = FixedTimestamp
			return map[string]any{"__typename": "CloseIssuePayload", "issue": issueObject(repo, issue)}
		}
		if field.name == "createPullRequest" {
			input, _ := field.args["input"].(map[string]any)
			repoID, _ := gqlStringValue(input["repositoryId"])
			repo := findRepoByGlobalID(store, repoID)
			if repo == nil {
				panic(errors.New("Repository not found"))
			}
			headRef, _ := gqlStringValue(input["headRefName"])
			baseRef, _ := gqlStringValue(input["baseRefName"])
			title, _ := gqlStringValue(input["title"])
			if headRef == "" || baseRef == "" || title == "" {
				panic(errors.New("headRefName, baseRefName and title are required"))
			}
			headSha := repo.Refs["refs/heads/"+headRef]
			baseSha := repo.Refs["refs/heads/"+baseRef]
			if headSha == "" || baseSha == "" {
				panic(errors.New("head or base branch does not exist"))
			}
			for _, pull := range repo.Pulls {
				if pull.State == "open" && pull.HeadRef == headRef && pull.BaseRef == baseRef {
					return map[string]any{"__typename": "CreatePullRequestPayload", "pullRequest": pullObject(repo, pull)}
				}
			}
			body, _ := gqlStringValue(input["body"])
			pull := store.CreatePull(repo, title, body, headRef, baseRef, headSha, baseSha)
			return map[string]any{"__typename": "CreatePullRequestPayload", "pullRequest": pullObject(repo, pull)}
		}
		if field.name == "mergePullRequest" {
			input, _ := field.args["input"].(map[string]any)
			pullID, _ := gqlStringValue(input["pullRequestId"])
			repo, pull := findPullByGlobalID(store, pullID)
			if repo == nil || pull == nil {
				panic(errors.New("Pull request not found"))
			}
			if pull.State == "closed" {
				panic(errors.New("Pull request is already closed"))
			}
			merged := SquashMergePull(store, repo, pull)
			if merged == "" {
				panic(errors.New("Merge conflict"))
			}
			return map[string]any{"__typename": "MergePullRequestPayload", "pullRequest": pullObject(repo, pull)}
		}
		if field.name == "addComment" {
			input, _ := field.args["input"].(map[string]any)
			subjectID, _ := gqlStringValue(input["subjectId"])
			repo, issue := findIssueByGlobalID(store, subjectID)
			if repo == nil || issue == nil {
				panic(errors.New("Issue not found"))
			}
			body, _ := gqlStringValue(input["body"])
			if body == "" {
				panic(errors.New("body is required"))
			}
			comment := store.AddComment(issue, body)
			return map[string]any{
				"__typename": "AddCommentPayload",
				"commentEdge": map[string]any{
					"__typename": "IssueCommentEdge", "node": commentObject(repo, comment),
					"cursor": cursorForNumber(comment.ID),
				},
			}
		}
		panic(unknownField(field, typeName))
	case "Viewer":
		if field.name == "login" {
			return obj["login"]
		}
		if field.name == "id" {
			return nodeID("User", itoa(ViewerID))
		}
		if field.name == "name" {
			return obj["name"]
		}
		if field.name == "email" {
			return obj["email"]
		}
		panic(unknownField(field, typeName))
	case "Repository":
		repo := obj["repo"].(*Repo)
		if field.name == "id" {
			return nodeID("Repository", itoa(repo.ID))
		}
		if field.name == "databaseId" {
			return repo.ID
		}
		if field.name == "name" {
			return repo.Name
		}
		if field.name == "nameWithOwner" {
			return repo.Owner + "/" + repo.Name
		}
		if field.name == "description" {
			return repo.Description
		}
		if field.name == "url" {
			return "https://github.com/" + repo.Owner + "/" + repo.Name
		}
		if field.name == "createdAt" {
			return repo.CreatedAt
		}
		if field.name == "updatedAt" {
			return repo.UpdatedAt
		}
		if field.name == "isPrivate" {
			return repo.IsPrivate
		}
		if field.name == "owner" {
			return viewerUserObject(repo.Owner)
		}
		if field.name == "defaultBranchRef" {
			tip := repo.Refs["refs/heads/"+repo.DefaultBranch]
			if tip == "" {
				return nil
			}
			return map[string]any{"__typename": "Ref", "name": repo.DefaultBranch}
		}
		if field.name == "viewerPermission" {
			return "ADMIN"
		}
		if field.name == "hasIssuesEnabled" {
			return true
		}
		if field.name == "issue" {
			number, ok := gqlNumberValue(field.args["number"])
			if !ok {
				return nil
			}
			for _, issue := range repo.Issues {
				if issue.Number == int(number) {
					return issueObject(repo, issue)
				}
			}
			return nil
		}
		if field.name == "issues" {
			issues := make([]*Issue, len(repo.Issues))
			copy(issues, repo.Issues)
			if states, ok := gqlStringArrayValue(field.args["states"]); ok && len(states) > 0 {
				wanted := map[string]bool{}
				for _, state := range states {
					wanted[strings.ToLower(state)] = true
				}
				filtered := issues[:0]
				for _, issue := range issues {
					if wanted[issue.State] {
						filtered = append(filtered, issue)
					}
				}
				issues = filtered
			}
			direction := 1
			if orderBy, ok := field.args["orderBy"].(map[string]any); ok {
				if dir, _ := gqlStringValue(orderBy["direction"]); dir == "DESC" {
					direction = -1
				}
			}
			sort.Slice(issues, func(i, j int) bool {
				return (issues[i].Number-issues[j].Number)*direction < 0
			})
			first, hasFirst := gqlNumberValue(field.args["first"])
			after, _ := gqlStringValue(field.args["after"])
			firstInt := 0
			if hasFirst {
				firstInt = int(first)
			}
			return issueConnection(repo, issues, firstInt, hasFirst, after)
		}
		if field.name == "pullRequest" {
			number, ok := gqlNumberValue(field.args["number"])
			if !ok {
				return nil
			}
			for _, pull := range repo.Pulls {
				if pull.Number == int(number) {
					return pullObject(repo, pull)
				}
			}
			return nil
		}
		if field.name == "pullRequests" {
			pulls := make([]*PullRequest, len(repo.Pulls))
			copy(pulls, repo.Pulls)
			if states, ok := gqlStringArrayValue(field.args["states"]); ok && len(states) > 0 {
				wanted := map[string]bool{}
				for _, state := range states {
					wanted[strings.ToUpper(state)] = true
				}
				filtered := pulls[:0]
				for _, pull := range pulls {
					if wanted[pullGraphqlState(pull)] {
						filtered = append(filtered, pull)
					}
				}
				pulls = filtered
			}
			sort.Slice(pulls, func(i, j int) bool { return pulls[i].Number < pulls[j].Number })
			first, hasFirst := gqlNumberValue(field.args["first"])
			after, _ := gqlStringValue(field.args["after"])
			firstInt := 0
			if hasFirst {
				firstInt = int(first)
			}
			return pullConnection(repo, pulls, firstInt, hasFirst, after)
		}
		panic(unknownField(field, typeName))
	case "Issue":
		repo := obj["repo"].(*Repo)
		issue := obj["issue"].(*Issue)
		if field.name == "id" {
			return nodeID("Issue", itoa(repo.ID)+":"+itoa(issue.Number))
		}
		if field.name == "databaseId" {
			return issue.ID
		}
		if field.name == "number" {
			return issue.Number
		}
		if field.name == "title" {
			return issue.Title
		}
		if field.name == "body" {
			return issue.Body
		}
		if field.name == "state" {
			return strings.ToUpper(issue.State)
		}
		if field.name == "url" {
			return "https://github.com/" + repo.Owner + "/" + repo.Name + "/issues/" + itoa(issue.Number)
		}
		if field.name == "createdAt" {
			return issue.CreatedAt
		}
		if field.name == "updatedAt" {
			return issue.UpdatedAt
		}
		if field.name == "author" {
			return viewerUserObject(ViewerLogin)
		}
		if field.name == "labels" {
			nodes := []any{}
			for _, name := range issue.Labels {
				nodes = append(nodes, map[string]any{"__typename": "Label", "name": name})
			}
			return map[string]any{"__typename": "LabelConnection", "nodes": nodes}
		}
		if field.name == "comments" {
			first, hasFirst := gqlNumberValue(field.args["first"])
			after, _ := gqlStringValue(field.args["after"])
			firstInt := 0
			if hasFirst {
				firstInt = int(first)
			}
			page := paginateByCursor(issue.Comments, firstInt, hasFirst, after, func(comment *IssueComment) int { return comment.ID })
			nodes := []any{}
			edges := []any{}
			for _, comment := range page.nodes {
				commentObj := commentObject(repo, comment)
				nodes = append(nodes, commentObj)
				edges = append(edges, map[string]any{"__typename": "IssueCommentEdge", "node": commentObj, "cursor": cursorForNumber(comment.ID)})
			}
			var startCursor any
			if len(page.nodes) > 0 {
				startCursor = cursorForNumber(page.nodes[0].ID)
			}
			return map[string]any{
				"__typename": "IssueCommentConnection",
				"nodes":      nodes,
				"edges":      edges,
				"pageInfo": map[string]any{
					"__typename": "PageInfo", "hasNextPage": page.hasNextPage, "endCursor": page.endCursor,
					"hasPreviousPage": false, "startCursor": startCursor,
				},
				"totalCount": len(issue.Comments),
			}
		}
		if field.name == "repository" {
			return repositoryObject(repo)
		}
		panic(unknownField(field, typeName))
	case "PullRequest":
		repo := obj["repo"].(*Repo)
		pull := obj["pull"].(*PullRequest)
		if field.name == "id" {
			return nodeID("PullRequest", itoa(repo.ID)+":"+itoa(pull.Number))
		}
		if field.name == "databaseId" {
			return pull.ID
		}
		if field.name == "number" {
			return pull.Number
		}
		if field.name == "title" {
			return pull.Title
		}
		if field.name == "body" {
			return pull.Body
		}
		if field.name == "state" {
			return pullGraphqlState(pull)
		}
		if field.name == "merged" {
			return pull.Merged
		}
		if field.name == "mergedAt" {
			// TS stores null before a merge; the Go store stores "".
			if pull.MergedAt == "" {
				return nil
			}
			return pull.MergedAt
		}
		if field.name == "headRefName" {
			return pull.HeadRef
		}
		if field.name == "baseRefName" {
			return pull.BaseRef
		}
		if field.name == "mergeable" {
			return PullMergeability(store, repo, pull)
		}
		if field.name == "url" {
			return "https://github.com/" + repo.Owner + "/" + repo.Name + "/pull/" + itoa(pull.Number)
		}
		if field.name == "createdAt" {
			return pull.CreatedAt
		}
		if field.name == "updatedAt" {
			return pull.UpdatedAt
		}
		if field.name == "author" {
			return viewerUserObject(ViewerLogin)
		}
		if field.name == "files" {
			baseObject := repo.Objects[pull.BaseSha]
			headObject := repo.Objects[pull.HeadSha]
			if baseObject == nil || baseObject.Type != ObjCommit || headObject == nil || headObject.Type != ObjCommit {
				return emptyFileConnection()
			}
			baseCommit, err := ParseCommit(baseObject.Data)
			if err != nil {
				panic(err)
			}
			headCommit, err := ParseCommit(headObject.Data)
			if err != nil {
				panic(err)
			}
			files := DiffTrees(store, repo, baseCommit.Tree, headCommit.Tree)
			first, hasFirst := gqlNumberValue(field.args["first"])
			nodes := files
			if hasFirst {
				n := int(first)
				if n < 0 {
					n = 0
				}
				if n < len(nodes) {
					nodes = nodes[:n]
				}
			}
			nodeObjs := []any{}
			edgeObjs := []any{}
			for i, file := range nodes {
				nodeObjs = append(nodeObjs, changedFileObject(file))
				edgeObjs = append(edgeObjs, map[string]any{
					"__typename": "FileEdge", "node": changedFileObject(file), "cursor": cursorForNumber(i + 1),
				})
			}
			var endCursor any
			if len(nodes) > 0 {
				endCursor = cursorForNumber(len(nodes))
			}
			var startCursor any
			if len(nodes) > 0 {
				startCursor = cursorForNumber(1)
			}
			return map[string]any{
				"__typename": "FileConnection",
				"nodes":      nodeObjs,
				"edges":      edgeObjs,
				"pageInfo": map[string]any{
					"__typename": "PageInfo", "hasNextPage": hasFirst && len(files) > int(first),
					"endCursor": endCursor, "hasPreviousPage": false, "startCursor": startCursor,
				},
				"totalCount": len(files),
			}
		}
		if field.name == "repository" {
			return repositoryObject(repo)
		}
		panic(unknownField(field, typeName))
	case "IssueComment":
		repo := obj["repo"].(*Repo)
		comment := obj["comment"].(*IssueComment)
		if field.name == "id" {
			return nodeID("IssueComment", itoa(repo.ID)+":"+itoa(comment.ID))
		}
		if field.name == "databaseId" {
			return comment.ID
		}
		if field.name == "body" {
			return comment.Body
		}
		if field.name == "createdAt" {
			return comment.CreatedAt
		}
		if field.name == "author" {
			return viewerUserObject(ViewerLogin)
		}
		panic(unknownField(field, typeName))
	case "User":
		if field.name == "login" {
			return obj["login"]
		}
		if field.name == "id" {
			return nodeID("User", gqlIDString(obj["id"], ViewerID))
		}
		if field.name == "name" {
			return obj["name"]
		}
		if field.name == "email" {
			return obj["email"]
		}
		panic(unknownField(field, typeName))
	case "Ref":
		if field.name == "name" {
			return obj["name"]
		}
		panic(unknownField(field, typeName))
	case "Label":
		if field.name == "name" {
			return obj["name"]
		}
		panic(unknownField(field, typeName))
	case "ChangedFile":
		if field.name == "path" {
			return obj["path"]
		}
		if field.name == "additions" {
			return obj["additions"]
		}
		if field.name == "deletions" {
			return obj["deletions"]
		}
		if field.name == "changes" {
			return obj["changes"]
		}
		if field.name == "status" {
			return obj["status"]
		}
		panic(unknownField(field, typeName))
	case "IssueConnection", "PullRequestConnection", "IssueCommentConnection", "FileConnection", "LabelConnection":
		if field.name == "nodes" {
			return obj["nodes"]
		}
		if field.name == "edges" {
			return obj["edges"]
		}
		if field.name == "pageInfo" {
			return obj["pageInfo"]
		}
		if field.name == "totalCount" {
			return obj["totalCount"]
		}
		panic(unknownField(field, typeName))
	case "IssueEdge", "PullRequestEdge", "IssueCommentEdge", "FileEdge":
		if field.name == "node" {
			return obj["node"]
		}
		if field.name == "cursor" {
			return obj["cursor"]
		}
		panic(unknownField(field, typeName))
	case "PageInfo":
		if field.name == "hasNextPage" {
			return obj["hasNextPage"]
		}
		if field.name == "endCursor" {
			return obj["endCursor"]
		}
		if field.name == "hasPreviousPage" {
			return obj["hasPreviousPage"]
		}
		if field.name == "startCursor" {
			return obj["startCursor"]
		}
		panic(unknownField(field, typeName))
	case "CreateIssuePayload", "CloseIssuePayload":
		if field.name == "issue" {
			return obj["issue"]
		}
		panic(unknownField(field, typeName))
	case "CreatePullRequestPayload", "MergePullRequestPayload":
		if field.name == "pullRequest" {
			return obj["pullRequest"]
		}
		panic(unknownField(field, typeName))
	case "AddCommentPayload":
		if field.name == "commentEdge" {
			return obj["commentEdge"]
		}
		panic(unknownField(field, typeName))
	case "__Type":
		if field.name == "name" {
			return obj["name"]
		}
		if field.name == "kind" {
			return obj["kind"]
		}
		if field.name == "description" {
			return obj["description"]
		}
		if field.name == "fields" {
			return obj["fields"]
		}
		if field.name == "enumValues" {
			return obj["enumValues"]
		}
		panic(unknownField(field, typeName))
	case "__Schema":
		if field.name == "queryType" {
			return obj["queryType"]
		}
		if field.name == "mutationType" {
			return obj["mutationType"]
		}
		panic(unknownField(field, typeName))
	default:
		panic(unknownField(field, typeName))
	}
}

func pullGraphqlState(pull *PullRequest) string {
	if pull.Merged {
		return "MERGED"
	}
	if pull.State == "closed" {
		return "CLOSED"
	}
	return "OPEN"
}

// typeIntrospection is static introspection data for the types the checks query.
func typeIntrospection(name string) map[string]any {
	if name == "" {
		return nil
	}
	fieldList := func(names []string) []any {
		out := []any{}
		for _, fieldName := range names {
			out = append(out, map[string]any{
				"name": fieldName, "description": "", "args": []any{},
				"type": map[string]any{"name": "String", "kind": "SCALAR"},
			})
		}
		return out
	}
	switch name {
	case "Issue":
		return map[string]any{
			"__typename": "__Type", "name": "Issue", "kind": "OBJECT", "description": "",
			"fields":     fieldList([]string{"id", "databaseId", "number", "title", "body", "state", "url", "createdAt", "updatedAt", "author", "labels", "comments", "repository"}),
			"enumValues": nil,
		}
	case "PullRequest":
		return map[string]any{
			"__typename": "__Type", "name": "PullRequest", "kind": "OBJECT", "description": "",
			"fields":     fieldList([]string{"id", "databaseId", "number", "title", "body", "state", "merged", "mergedAt", "headRefName", "baseRefName", "mergeable", "files", "url", "createdAt", "updatedAt", "author", "repository"}),
			"enumValues": nil,
		}
	case "Repository":
		return map[string]any{
			"__typename": "__Type", "name": "Repository", "kind": "OBJECT", "description": "",
			"fields":     fieldList([]string{"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequest", "pullRequests", "description", "url", "createdAt", "updatedAt", "isPrivate"}),
			"enumValues": nil,
		}
	case "SearchType":
		return map[string]any{
			"__typename": "__Type", "name": "SearchType", "kind": "ENUM", "description": "",
			"fields": nil,
			"enumValues": []any{
				map[string]any{"name": "ISSUE", "description": ""},
				map[string]any{"name": "REPOSITORY", "description": ""},
				map[string]any{"name": "USER", "description": ""},
			},
		}
	default:
		return nil
	}
}

func isGqlObject(value any) bool {
	m, ok := value.(map[string]any)
	if !ok || m == nil {
		return false
	}
	_, ok = m["__typename"].(string)
	return ok
}

func materialize(value any, field *fieldNode, ctx *gqlContext) any {
	if value == nil {
		return nil
	}
	if arr, ok := value.([]any); ok {
		out := make([]any, 0, len(arr))
		for _, item := range arr {
			out = append(out, materialize(item, field, ctx))
		}
		return out
	}
	if isGqlObject(value) {
		obj := value.(map[string]any)
		return resolveSelections(obj["__typename"].(string), obj, field.children, ctx)
	}
	if m, ok := value.(map[string]any); ok {
		if len(field.children) == 0 {
			return value
		}
		out := map[string]any{}
		for _, child := range field.children {
			if child.fragmentType != "" {
				continue
			}
			out[child.responseName()] = materialize(m[child.name], child, ctx)
		}
		return out
	}
	return value
}

func resolveSelections(typeName string, obj map[string]any, selections []*fieldNode, ctx *gqlContext) map[string]any {
	out := map[string]any{}
	for _, selection := range selections {
		if selection.fragmentType != "" {
			if selection.fragmentType != typeName {
				continue
			}
			for k, v := range resolveSelections(typeName, obj, selection.children, ctx) {
				out[k] = v
			}
			continue
		}
		value := resolveField(typeName, obj, selection, ctx)
		out[selection.responseName()] = materialize(value, selection, ctx)
	}
	return out
}

func executeOperation(store *Store, operation *gqlOperation) map[string]any {
	ctx := &gqlContext{store: store}
	rootType := "Query"
	if operation.kind == "mutation" {
		rootType = "Mutation"
	}
	root := map[string]any{"__typename": rootType}
	return resolveSelections(rootType, root, operation.selections, ctx)
}

// substituteSelectionArgs replaces `{__variable: name}` markers in every
// field's arguments, at any depth.
func substituteSelectionArgs(selections []*fieldNode, variables map[string]any) []*fieldNode {
	out := make([]*fieldNode, 0, len(selections))
	for _, selection := range selections {
		cp := *selection
		cp.args = substituteVariables(selection.args, variables).(map[string]any)
		cp.children = substituteSelectionArgs(selection.children, variables)
		out = append(out, &cp)
	}
	return out
}

func executeDocument(store *Store, query string, variables map[string]any) (result map[string]any) {
	defer func() {
		if r := recover(); r != nil {
			message := "GraphQL execution failed"
			if err, ok := r.(error); ok {
				message = err.Error()
			}
			result = map[string]any{"data": nil, "errors": []any{map[string]any{"message": message}}}
		}
	}()
	operation := (&gqlParser{tokens: tokenize(query)}).parseDocument()
	mergedVariables := map[string]any{}
	for k, v := range operation.defaults {
		mergedVariables[k] = v
	}
	for k, v := range variables {
		mergedVariables[k] = v
	}
	selections := substituteSelectionArgs(operation.selections, mergedVariables)
	op := *operation
	op.selections = selections
	data := executeOperation(store, &op)
	return map[string]any{"data": data}
}

// HandleGraphQL handles POST /api/graphql. A JSON array is a batch: each entry
// is executed in order and the results are returned in order.
func HandleGraphQL(store *Store, ctx *Ctx, res Responder) bool {
	if ctx.Method != "POST" {
		return false
	}
	parsed := ParseJSONBody(ctx.Body)
	if !parsed.OK {
		SendJSON(res, 400, map[string]any{"message": parsed.Err}, ctx.RequestID, nil)
		return true
	}
	isArray := false
	var requests []any
	if arr, ok := parsed.Value.([]any); ok {
		requests = arr
		isArray = true
	} else {
		requests = []any{parsed.Value}
	}
	results := make([]any, 0, len(requests))
	for _, request := range requests {
		rec, ok := request.(map[string]any)
		if !ok {
			results = append(results, map[string]any{"data": nil, "errors": []any{map[string]any{"message": "GraphQL request must include a query string"}}})
			continue
		}
		query, ok := rec["query"].(string)
		if !ok {
			results = append(results, map[string]any{"data": nil, "errors": []any{map[string]any{"message": "GraphQL request must include a query string"}}})
			continue
		}
		variables := map[string]any{}
		if v, ok := rec["variables"].(map[string]any); ok {
			variables = v
		}
		results = append(results, executeDocument(store, query, variables))
	}
	if isArray {
		SendJSON(res, 200, results, ctx.RequestID, nil)
	} else {
		SendJSON(res, 200, results[0], ctx.RequestID, nil)
	}
	return true
}
