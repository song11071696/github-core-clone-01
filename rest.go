// rest.go — the REST v3 API.
//
// A faithful port of the TypeScript rest.ts. Shapes follow the public GitHub
// API closely enough for the conformance checks: repositories, branches,
// commits, contents, labels, issues, comments and pull requests, all backed
// by the same in-memory store (and the same git object map) the smart-HTTP
// endpoints use. A repository requested under any owner other than the token
// owner is a 404, never a 403.
//
// Only the Go standard library. No store lock is taken here: app.go already
// holds the store lock for the whole handler.

package main

import (
	"encoding/base64"
	"sort"
	"strings"
)

// jsParseInt mirrors JavaScript's Number.parseInt(s, 10): leading whitespace,
// an optional sign, then digits; anything else (including no digits at all)
// is NaN, reported as ok=false.
func jsParseInt(s string) (int, bool) {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r' || s[i] == '\f' || s[i] == '\v') {
		i++
	}
	neg := false
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		neg = s[i] == '-'
		i++
	}
	start := i
	n := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		n = n*10 + int(s[i]-'0')
		i++
	}
	if i == start {
		return 0, false
	}
	if neg {
		n = -n
	}
	return n, true
}

// queryParam returns the first value of a query parameter, or def when the
// parameter is absent. This mirrors TS `ctx.query.get(key) ?? def`: a
// parameter that is present but empty yields "" rather than the default.
func queryParam(ctx *Ctx, key, def string) string {
	if vals, ok := ctx.Query[key]; ok && len(vals) > 0 {
		return vals[0]
	}
	return def
}

// decodeBase64Lenient mirrors Buffer.from(s, 'base64'): it accepts missing
// padding and never fails on input a conformance client would send.
func decodeBase64Lenient(s string) []byte {
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b
	}
	if b, err := base64.RawStdEncoding.DecodeString(s); err == nil {
		return b
	}
	if m := len(s) % 4; m != 0 {
		if b, err := base64.StdEncoding.DecodeString(s + strings.Repeat("=", 4-m)); err == nil {
			return b
		}
	}
	return []byte{}
}

func findRepo(store *Store, ctx *Ctx, res Responder, owner, name string) *Repo {
	repo := store.GetRepo(owner, name)
	if repo == nil {
		SendJSON(res, 404, map[string]any{"message": "Not Found"}, ctx.RequestID, nil)
		return nil
	}
	return repo
}

func findIssue(repo *Repo, ctx *Ctx, res Responder, raw string) *Issue {
	number, ok := jsParseInt(raw)
	if !ok {
		SendJSON(res, 404, map[string]any{"message": "Not Found"}, ctx.RequestID, nil)
		return nil
	}
	for _, issue := range repo.Issues {
		if issue.Number == number {
			return issue
		}
	}
	SendJSON(res, 404, map[string]any{"message": "Not Found"}, ctx.RequestID, nil)
	return nil
}

func findPull(repo *Repo, ctx *Ctx, res Responder, raw string) *PullRequest {
	number, ok := jsParseInt(raw)
	if !ok {
		SendJSON(res, 404, map[string]any{"message": "Not Found"}, ctx.RequestID, nil)
		return nil
	}
	for _, pull := range repo.Pulls {
		if pull.Number == number {
			return pull
		}
	}
	SendJSON(res, 404, map[string]any{"message": "Not Found"}, ctx.RequestID, nil)
	return nil
}

func readBody(ctx *Ctx, res Responder) map[string]any {
	parsed := ParseJSONBody(ctx.Body)
	if !parsed.OK {
		SendJSON(res, 400, map[string]any{"message": parsed.Err}, ctx.RequestID, nil)
		return nil
	}
	if !IsRecord(parsed.Value) {
		SendJSON(res, 400, map[string]any{"message": "Request body must be a JSON object"}, ctx.RequestID, nil)
		return nil
	}
	return parsed.Value.(map[string]any)
}

func paginate[T any](items []T, ctx *Ctx) ([]T, string) {
	perPageRaw, ok := jsParseInt(queryParam(ctx, "per_page", "30"))
	perPage := 30
	if ok && perPageRaw > 0 {
		perPage = perPageRaw
		if perPage > 100 {
			perPage = 100
		}
	}
	pageRaw, ok := jsParseInt(queryParam(ctx, "page", "1"))
	page := 1
	if ok && pageRaw > 0 {
		page = pageRaw
	}
	start := (page - 1) * perPage
	if start > len(items) {
		start = len(items)
	}
	end := start + perPage
	if end > len(items) {
		end = len(items)
	}
	slice := items[start:end]
	totalPages := (len(items) + perPage - 1) / perPage
	if totalPages < 1 {
		totalPages = 1
	}
	var link string
	if page < totalPages {
		base := "http://" + ctx.Host + ctx.Path
		next := base + "?per_page=" + itoa(perPage) + "&page=" + itoa(page+1)
		last := base + "?per_page=" + itoa(perPage) + "&page=" + itoa(totalPages)
		link = "<" + next + ">; rel=\"next\", <" + last + ">; rel=\"last\""
	}
	return slice, link
}

func linkHeaders(link string) map[string]string {
	if link == "" {
		return map[string]string{}
	}
	return map[string]string{"Link": link}
}

// subtreeOID returns the tree object id of a nested directory inside a tree,
// or "" when it does not exist.
func subtreeOID(repo *Repo, treeOID, relativePath string) string {
	if relativePath == "" {
		return treeOID
	}
	parts := strings.Split(relativePath, "/")
	current := treeOID
	for _, part := range parts {
		object := repo.Objects[current]
		if object == nil || object.Type != ObjTree {
			return ""
		}
		entries, err := ParseTree(object.Data)
		if err != nil {
			return ""
		}
		found := ""
		for _, entry := range entries {
			if entry.Name == part && entry.Mode == "40000" {
				found = entry.OID
				break
			}
		}
		if found == "" {
			return ""
		}
		current = found
	}
	return current
}

// commitTree returns the tree oid of a commit object, or "" when the object
// is missing or is not a parseable commit.
func commitTree(repo *Repo, oid string) string {
	obj := repo.Objects[oid]
	if obj == nil || obj.Type != ObjCommit {
		return ""
	}
	commit, err := ParseCommit(obj.Data)
	if err != nil {
		return ""
	}
	return commit.Tree
}

func lastSegment(path string) string {
	if idx := strings.LastIndex(path, "/"); idx >= 0 {
		return path[idx+1:]
	}
	return path
}

func handleUserRepos(store *Store, ctx *Ctx, res Responder) bool {
	if ctx.Method == "GET" {
		var all []*Repo
		for _, repo := range store.Repos {
			if repo.Owner == ViewerLogin {
				all = append(all, repo)
			}
		}
		sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
		page, link := paginate(all, ctx)
		out := []any{}
		for _, repo := range page {
			out = append(out, RepoJSON(repo))
		}
		SendJSON(res, 200, out, ctx.RequestID, linkHeaders(link))
		return true
	}
	if ctx.Method == "POST" {
		body := readBody(ctx, res)
		if body == nil {
			return true
		}
		name, _ := AsString(body["name"])
		if name == "" || strings.Contains(name, "/") || name == "." || name == ".." {
			SendJSON(res, 422, map[string]any{"message": "name is required and must be a valid repository name"}, ctx.RequestID, nil)
			return true
		}
		if store.GetRepo(ViewerLogin, name) != nil {
			SendJSON(res, 422, map[string]any{"message": "Repository " + name + " already exists"}, ctx.RequestID, nil)
			return true
		}
		description, _ := AsString(body["description"])
		isPrivate, _ := AsBool(body["private"])
		repo := store.CreateRepo(ViewerLogin, name, description, isPrivate)
		if repo == nil {
			SendJSON(res, 422, map[string]any{"message": "Repository " + name + " already exists"}, ctx.RequestID, nil)
			return true
		}
		SendBuffer(res, 201, "application/json", repoJSONBytes(repo), ctx.RequestID)
		return true
	}
	return false
}

func handleRepo(store *Store, ctx *Ctx, res Responder, owner, name string) bool {
	repo := findRepo(store, ctx, res, owner, name)
	if repo == nil {
		return true
	}
	if ctx.Method == "GET" {
		SendBuffer(res, 200, "application/json", repoJSONBytes(repo), ctx.RequestID)
		return true
	}
	if ctx.Method == "PATCH" {
		body := readBody(ctx, res)
		if body == nil {
			return true
		}
		if description, ok := AsString(body["description"]); ok {
			repo.Description = description
		}
		repo.UpdatedAt = FixedTimestamp
		SendBuffer(res, 200, "application/json", repoJSONBytes(repo), ctx.RequestID)
		return true
	}
	if ctx.Method == "DELETE" {
		store.DeleteRepo(owner, name)
		SendEmpty(res, 204, ctx.RequestID)
		return true
	}
	return false
}

func handleBranches(store *Store, ctx *Ctx, res Responder, owner, name string) bool {
	if ctx.Method != "GET" {
		return false
	}
	repo := findRepo(store, ctx, res, owner, name)
	if repo == nil {
		return true
	}
	var refs []string
	for ref := range repo.Refs {
		if strings.HasPrefix(ref, "refs/heads/") {
			refs = append(refs, ref)
		}
	}
	sort.Strings(refs)
	out := []any{}
	for _, ref := range refs {
		out = append(out, map[string]any{
			"name":      strings.TrimPrefix(ref, "refs/heads/"),
			"commit":    map[string]any{"sha": repo.Refs[ref], "url": ""},
			"protected": false,
		})
	}
	SendJSON(res, 200, out, ctx.RequestID, nil)
	return true
}

func handleBranch(store *Store, ctx *Ctx, res Responder, owner, name, branch string) bool {
	if ctx.Method != "GET" {
		return false
	}
	repo := findRepo(store, ctx, res, owner, name)
	if repo == nil {
		return true
	}
	sha, ok := repo.Refs["refs/heads/"+branch]
	if !ok {
		SendJSON(res, 404, map[string]any{"message": "Not Found"}, ctx.RequestID, nil)
		return true
	}
	SendJSON(res, 200, map[string]any{
		"name":      branch,
		"commit":    map[string]any{"sha": sha, "url": ""},
		"protected": false,
	}, ctx.RequestID, nil)
	return true
}

func handleCommits(store *Store, ctx *Ctx, res Responder, owner, name string) bool {
	if ctx.Method != "GET" {
		return false
	}
	repo := findRepo(store, ctx, res, owner, name)
	if repo == nil {
		return true
	}
	branch := queryParam(ctx, "sha", repo.DefaultBranch)
	tip, ok := repo.Refs["refs/heads/"+branch]
	if !ok {
		SendJSON(res, 404, map[string]any{"message": "Not Found"}, ctx.RequestID, nil)
		return true
	}
	out := []any{}
	for _, oid := range CommitsFromTip(repo, tip) {
		obj := repo.Objects[oid]
		if obj == nil {
			continue
		}
		commit, err := ParseCommit(obj.Data)
		if err != nil {
			continue
		}
		message := strings.TrimSuffix(commit.Message, "\n")
		out = append(out, map[string]any{
			"sha":     oid,
			"node_id": nodeID("Commit", oid),
			"commit": map[string]any{
				"message":   message,
				"author":    map[string]any{"name": ViewerName, "email": ViewerEmail, "date": FixedTimestamp},
				"committer": map[string]any{"name": ViewerName, "email": ViewerEmail, "date": FixedTimestamp},
			},
			"author":    map[string]any{"login": ViewerLogin, "id": ViewerID, "type": "User"},
			"committer": map[string]any{"login": ViewerLogin, "id": ViewerID, "type": "User"},
		})
	}
	SendJSON(res, 200, out, ctx.RequestID, nil)
	return true
}

func handleCreateRef(store *Store, ctx *Ctx, res Responder, owner, name string) bool {
	if ctx.Method != "POST" {
		return false
	}
	repo := findRepo(store, ctx, res, owner, name)
	if repo == nil {
		return true
	}
	body := readBody(ctx, res)
	if body == nil {
		return true
	}
	ref, _ := AsString(body["ref"])
	sha, _ := AsString(body["sha"])
	if ref == "" || !strings.HasPrefix(ref, "refs/") || sha == "" {
		SendJSON(res, 422, map[string]any{"message": "ref and sha are required"}, ctx.RequestID, nil)
		return true
	}
	if _, ok := repo.Refs[ref]; ok {
		SendJSON(res, 422, map[string]any{"message": "Reference already exists"}, ctx.RequestID, nil)
		return true
	}
	if _, ok := repo.Objects[sha]; !ok {
		SendJSON(res, 422, map[string]any{"message": "Object does not exist"}, ctx.RequestID, nil)
		return true
	}
	repo.Refs[ref] = sha
	SendJSON(res, 201, map[string]any{
		"ref":    ref,
		"object": map[string]any{"sha": sha, "type": "commit", "url": ""},
	}, ctx.RequestID, nil)
	return true
}

func handleGetRef(store *Store, ctx *Ctx, res Responder, owner, name, branch string) bool {
	if ctx.Method != "GET" {
		return false
	}
	repo := findRepo(store, ctx, res, owner, name)
	if repo == nil {
		return true
	}
	ref := "refs/heads/" + branch
	sha, ok := repo.Refs[ref]
	if !ok {
		SendJSON(res, 404, map[string]any{"message": "Not Found"}, ctx.RequestID, nil)
		return true
	}
	typ := "commit"
	if object := repo.Objects[sha]; object != nil {
		typ = string(object.Type)
	}
	SendJSON(res, 201, map[string]any{
		"ref":    ref,
		"object": map[string]any{"sha": sha, "type": typ, "url": ""},
	}, ctx.RequestID, nil)
	return true
}

func handleGetContents(store *Store, ctx *Ctx, res Responder, owner, name, path string) bool {
	if ctx.Method != "GET" {
		return false
	}
	repo := findRepo(store, ctx, res, owner, name)
	if repo == nil {
		return true
	}
	branch := queryParam(ctx, "ref", repo.DefaultBranch)
	tip, ok := repo.Refs["refs/heads/"+branch]
	if !ok {
		SendJSON(res, 404, map[string]any{"message": "Not Found"}, ctx.RequestID, nil)
		return true
	}
	treeOID := commitTree(repo, tip)
	if treeOID == "" {
		SendJSON(res, 404, map[string]any{"message": "Not Found"}, ctx.RequestID, nil)
		return true
	}
	lookup := func(oid string) *GitObject { return repo.Objects[oid] }
	flat, err := FlattenTree(lookup, treeOID)
	if err != nil {
		SendJSON(res, 404, map[string]any{"message": "Not Found"}, ctx.RequestID, nil)
		return true
	}
	if file, ok := flat[path]; ok {
		content := repo.Objects[file.OID]
		if content == nil {
			SendJSON(res, 404, map[string]any{"message": "Not Found"}, ctx.RequestID, nil)
			return true
		}
		SendJSON(res, 200, map[string]any{
			"type":     "file",
			"encoding": "base64",
			"size":     len(content.Data),
			"name":     lastSegment(path),
			"path":     path,
			"content":  base64.StdEncoding.EncodeToString(content.Data),
			"sha":      file.OID,
		}, ctx.RequestID, nil)
		return true
	}
	prefix := ""
	if path != "" {
		prefix = path + "/"
	}
	type childEntry struct {
		typ  string
		name string
		path string
		sha  string
	}
	children := map[string]*childEntry{}
	for entryPath, entry := range flat {
		if !strings.HasPrefix(entryPath, prefix) {
			continue
		}
		rest := entryPath[len(prefix):]
		segment := rest
		if idx := strings.Index(rest, "/"); idx >= 0 {
			segment = rest[:idx]
		}
		if strings.Contains(rest, "/") {
			dirPath := prefix + segment
			if _, ok := children[dirPath]; !ok {
				children[dirPath] = &childEntry{typ: "dir", name: segment, path: dirPath, sha: subtreeOID(repo, treeOID, dirPath)}
			}
		} else {
			children[entryPath] = &childEntry{typ: "file", name: segment, path: entryPath, sha: entry.OID}
		}
	}
	if len(children) == 0 {
		SendJSON(res, 404, map[string]any{"message": "Not Found"}, ctx.RequestID, nil)
		return true
	}
	list := make([]*childEntry, 0, len(children))
	for _, child := range children {
		list = append(list, child)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].name < list[j].name })
	out := []any{}
	for _, child := range list {
		out = append(out, map[string]any{"type": child.typ, "name": child.name, "path": child.path, "sha": child.sha})
	}
	SendJSON(res, 200, out, ctx.RequestID, nil)
	return true
}

func handlePutContents(store *Store, ctx *Ctx, res Responder, owner, name, path string) bool {
	if ctx.Method != "PUT" {
		return false
	}
	repo := findRepo(store, ctx, res, owner, name)
	if repo == nil {
		return true
	}
	body := readBody(ctx, res)
	if body == nil {
		return true
	}
	message, _ := AsString(body["message"])
	contentB64, contentOK := AsString(body["content"])
	if !contentOK {
		SendJSON(res, 422, map[string]any{"message": "content is required"}, ctx.RequestID, nil)
		return true
	}
	branch := repo.DefaultBranch
	if b, ok := AsString(body["branch"]); ok {
		branch = b
	}
	branchRef := "refs/heads/" + branch
	baseCommit, hasBase := repo.Refs[branchRef]
	if !hasBase && branch != repo.DefaultBranch {
		baseCommit, hasBase = repo.Refs["refs/heads/"+repo.DefaultBranch]
		if !hasBase {
			SendJSON(res, 404, map[string]any{"message": "Not Found"}, ctx.RequestID, nil)
			return true
		}
	}
	currentBlob := ""
	hasCurrent := false
	if hasBase {
		currentBlob = BlobOIDAtPath(repo, baseCommit, path)
		hasCurrent = currentBlob != ""
	}
	if expectedSha, ok := AsString(body["sha"]); ok && (!hasCurrent || expectedSha != currentBlob) {
		SendJSON(res, 409, map[string]any{"message": "sha does not match the current file"}, ctx.RequestID, nil)
		return true
	}
	content := decodeBase64Lenient(contentB64)
	blobOID := store.PutObject(repo, ObjBlob, content)
	baseTree := EmptyTreeOID(store, repo)
	if hasBase {
		if tree := commitTree(repo, baseCommit); tree != "" {
			baseTree = tree
		}
	}
	newTree := ApplyTreeChanges(store, repo, baseTree, map[string]string{path: blobOID})
	var parents []string
	if hasBase {
		parents = []string{baseCommit}
	}
	commitOID := store.CreateCommit(repo, newTree, parents, message+"\n")
	repo.Refs[branchRef] = commitOID
	status := 201
	if hasCurrent {
		status = 200
	}
	SendJSON(res, status, map[string]any{
		"content": map[string]any{
			"name":     lastSegment(path),
			"path":     path,
			"sha":      blobOID,
			"size":     len(content),
			"type":     "file",
			"encoding": "base64",
			"content":  contentB64,
		},
		"commit": map[string]any{"sha": commitOID, "message": message},
	}, ctx.RequestID, nil)
	return true
}

func handleDeleteContents(store *Store, ctx *Ctx, res Responder, owner, name, path string) bool {
	if ctx.Method != "DELETE" {
		return false
	}
	repo := findRepo(store, ctx, res, owner, name)
	if repo == nil {
		return true
	}
	body := readBody(ctx, res)
	if body == nil {
		return true
	}
	message, _ := AsString(body["message"])
	branch := repo.DefaultBranch
	if b, ok := AsString(body["branch"]); ok {
		branch = b
	}
	branchRef := "refs/heads/" + branch
	tip, ok := repo.Refs[branchRef]
	if !ok {
		SendJSON(res, 404, map[string]any{"message": "Not Found"}, ctx.RequestID, nil)
		return true
	}
	currentBlob := BlobOIDAtPath(repo, tip, path)
	if currentBlob == "" {
		SendJSON(res, 404, map[string]any{"message": "Not Found"}, ctx.RequestID, nil)
		return true
	}
	if expectedSha, ok := AsString(body["sha"]); ok && expectedSha != currentBlob {
		SendJSON(res, 409, map[string]any{"message": "sha does not match the current file"}, ctx.RequestID, nil)
		return true
	}
	baseTree := commitTree(repo, tip)
	if baseTree == "" {
		SendJSON(res, 404, map[string]any{"message": "Not Found"}, ctx.RequestID, nil)
		return true
	}
	newTree := ApplyTreeChanges(store, repo, baseTree, map[string]string{path: ""})
	commitOID := store.CreateCommit(repo, newTree, []string{tip}, message+"\n")
	repo.Refs[branchRef] = commitOID
	SendJSON(res, 200, map[string]any{
		"commit": map[string]any{"sha": commitOID, "message": message},
	}, ctx.RequestID, nil)
	return true
}

func handleLabels(store *Store, ctx *Ctx, res Responder, owner, name string) bool {
	repo := findRepo(store, ctx, res, owner, name)
	if repo == nil {
		return true
	}
	if ctx.Method == "GET" {
		var names []string
		for labelName := range repo.Labels {
			names = append(names, labelName)
		}
		sort.Strings(names)
		out := []any{}
		for _, labelName := range names {
			out = append(out, map[string]any{"name": labelName, "color": repo.Labels[labelName].Color})
		}
		SendJSON(res, 200, out, ctx.RequestID, nil)
		return true
	}
	if ctx.Method == "POST" {
		body := readBody(ctx, res)
		if body == nil {
			return true
		}
		labelName, _ := AsString(body["name"])
		if labelName == "" {
			SendJSON(res, 422, map[string]any{"message": "name is required"}, ctx.RequestID, nil)
			return true
		}
		if _, ok := repo.Labels[labelName]; ok {
			SendJSON(res, 422, map[string]any{"message": "Label already exists"}, ctx.RequestID, nil)
			return true
		}
		color := "ededed"
		if c, ok := AsString(body["color"]); ok {
			color = c
		}
		repo.Labels[labelName] = &Label{Name: labelName, Color: color}
		SendJSON(res, 201, map[string]any{"name": labelName, "color": color}, ctx.RequestID, nil)
		return true
	}
	return false
}

// normalizeLabelNames mirrors the TS helper: absent labels mean [], an array
// of strings or {name} records yields the names, anything else is invalid.
func normalizeLabelNames(body map[string]any) ([]string, bool) {
	value, present := body["labels"]
	if !present {
		return []string{}, true
	}
	arr, ok := value.([]any)
	if !ok {
		return nil, false
	}
	names := []string{}
	for _, item := range arr {
		if s, ok := item.(string); ok {
			names = append(names, s)
			continue
		}
		if rec, ok := item.(map[string]any); ok {
			if s, ok := rec["name"].(string); ok {
				names = append(names, s)
				continue
			}
		}
		return nil, false
	}
	return names, true
}

func handleIssues(store *Store, ctx *Ctx, res Responder, owner, name string) bool {
	repo := findRepo(store, ctx, res, owner, name)
	if repo == nil {
		return true
	}
	if ctx.Method == "GET" {
		state := queryParam(ctx, "state", "open")
		direction := queryParam(ctx, "direction", "desc")
		var issues []*Issue
		for _, issue := range repo.Issues {
			if state == "all" || issue.State == state {
				issues = append(issues, issue)
			}
		}
		sort.Slice(issues, func(i, j int) bool { return issues[i].Number < issues[j].Number })
		if direction == "desc" {
			for i, j := 0, len(issues)-1; i < j; i, j = i+1, j-1 {
				issues[i], issues[j] = issues[j], issues[i]
			}
		}
		page, link := paginate(issues, ctx)
		out := []any{}
		for _, issue := range page {
			out = append(out, IssueJSON(repo, issue))
		}
		SendJSON(res, 200, out, ctx.RequestID, linkHeaders(link))
		return true
	}
	if ctx.Method == "POST" {
		body := readBody(ctx, res)
		if body == nil {
			return true
		}
		title, _ := AsString(body["title"])
		if title == "" {
			SendJSON(res, 422, map[string]any{"message": "title is required"}, ctx.RequestID, nil)
			return true
		}
		labels, ok := normalizeLabelNames(body)
		if !ok {
			SendJSON(res, 422, map[string]any{"message": "labels must be an array of label names"}, ctx.RequestID, nil)
			return true
		}
		for _, label := range labels {
			if _, ok := repo.Labels[label]; !ok {
				SendJSON(res, 422, map[string]any{"message": "Label '" + label + "' does not exist"}, ctx.RequestID, nil)
				return true
			}
		}
		bodyStr, _ := AsString(body["body"])
		issue := store.CreateIssue(repo, title, bodyStr, labels)
		SendBuffer(res, 201, "application/json", issueJSONBytes(repo, issue), ctx.RequestID)
		return true
	}
	return false
}

func handleIssue(store *Store, ctx *Ctx, res Responder, owner, name, rawNumber string) bool {
	repo := findRepo(store, ctx, res, owner, name)
	if repo == nil {
		return true
	}
	issue := findIssue(repo, ctx, res, rawNumber)
	if issue == nil {
		return true
	}
	if ctx.Method == "GET" {
		SendBuffer(res, 200, "application/json", issueJSONBytes(repo, issue), ctx.RequestID)
		return true
	}
	if ctx.Method == "PATCH" {
		body := readBody(ctx, res)
		if body == nil {
			return true
		}
		if title, ok := AsString(body["title"]); ok {
			issue.Title = title
		}
		if issueBody, ok := AsString(body["body"]); ok {
			issue.Body = issueBody
		}
		if state, ok := AsString(body["state"]); ok && (state == "open" || state == "closed") {
			issue.State = state
		}
		issue.UpdatedAt = FixedTimestamp
		SendBuffer(res, 200, "application/json", issueJSONBytes(repo, issue), ctx.RequestID)
		return true
	}
	return false
}

func handleIssueComments(store *Store, ctx *Ctx, res Responder, owner, name, rawNumber string) bool {
	repo := findRepo(store, ctx, res, owner, name)
	if repo == nil {
		return true
	}
	issue := findIssue(repo, ctx, res, rawNumber)
	if issue == nil {
		return true
	}
	if ctx.Method == "GET" {
		out := []any{}
		for _, comment := range issue.Comments {
			out = append(out, commentJSON(repo, comment))
		}
		SendJSON(res, 200, out, ctx.RequestID, nil)
		return true
	}
	if ctx.Method == "POST" {
		body := readBody(ctx, res)
		if body == nil {
			return true
		}
		commentBody, _ := AsString(body["body"])
		if commentBody == "" {
			SendJSON(res, 422, map[string]any{"message": "body is required"}, ctx.RequestID, nil)
			return true
		}
		comment := store.AddComment(issue, commentBody)
		SendJSON(res, 201, commentJSON(repo, comment), ctx.RequestID, nil)
		return true
	}
	return false
}

func handlePulls(store *Store, ctx *Ctx, res Responder, owner, name string) bool {
	repo := findRepo(store, ctx, res, owner, name)
	if repo == nil {
		return true
	}
	if ctx.Method == "GET" {
		state := queryParam(ctx, "state", "open")
		var pulls []*PullRequest
		for _, pull := range repo.Pulls {
			if state == "all" || pull.State == state {
				pulls = append(pulls, pull)
			}
		}
		page, link := paginate(pulls, ctx)
		out := []any{}
		for _, pull := range page {
			out = append(out, PullJSON(repo, pull))
		}
		SendJSON(res, 200, out, ctx.RequestID, linkHeaders(link))
		return true
	}
	if ctx.Method == "POST" {
		body := readBody(ctx, res)
		if body == nil {
			return true
		}
		title, _ := AsString(body["title"])
		head, _ := AsString(body["head"])
		base, _ := AsString(body["base"])
		if title == "" || head == "" || base == "" {
			SendJSON(res, 422, map[string]any{"message": "title, head and base are required"}, ctx.RequestID, nil)
			return true
		}
		headSha, headOK := repo.Refs["refs/heads/"+head]
		baseSha, baseOK := repo.Refs["refs/heads/"+base]
		if !headOK || !baseOK {
			SendJSON(res, 422, map[string]any{"message": "head or base branch does not exist"}, ctx.RequestID, nil)
			return true
		}
		for _, pull := range repo.Pulls {
			if pull.State == "open" && pull.HeadRef == head && pull.BaseRef == base {
				SendJSON(res, 422, map[string]any{"message": "A pull request already exists for these branches"}, ctx.RequestID, nil)
				return true
			}
		}
		bodyStr, _ := AsString(body["body"])
		pull := store.CreatePull(repo, title, bodyStr, head, base, headSha, baseSha)
		SendBuffer(res, 201, "application/json", pullJSONBytes(repo, pull), ctx.RequestID)
		return true
	}
	return false
}

func handlePull(store *Store, ctx *Ctx, res Responder, owner, name, rawNumber string) bool {
	repo := findRepo(store, ctx, res, owner, name)
	if repo == nil {
		return true
	}
	pull := findPull(repo, ctx, res, rawNumber)
	if pull == nil {
		return true
	}
	if ctx.Method == "GET" {
		SendBuffer(res, 200, "application/json", pullJSONBytes(repo, pull), ctx.RequestID)
		return true
	}
	return false
}

func handlePullFiles(store *Store, ctx *Ctx, res Responder, owner, name, rawNumber string) bool {
	if ctx.Method != "GET" {
		return false
	}
	repo := findRepo(store, ctx, res, owner, name)
	if repo == nil {
		return true
	}
	pull := findPull(repo, ctx, res, rawNumber)
	if pull == nil {
		return true
	}
	baseObject := repo.Objects[pull.BaseSha]
	headObject := repo.Objects[pull.HeadSha]
	if baseObject == nil || baseObject.Type != ObjCommit || headObject == nil || headObject.Type != ObjCommit {
		SendJSON(res, 422, map[string]any{"message": "pull request commits are missing"}, ctx.RequestID, nil)
		return true
	}
	baseCommit, err := ParseCommit(baseObject.Data)
	if err != nil {
		SendJSON(res, 422, map[string]any{"message": "pull request commits are missing"}, ctx.RequestID, nil)
		return true
	}
	headCommit, err := ParseCommit(headObject.Data)
	if err != nil {
		SendJSON(res, 422, map[string]any{"message": "pull request commits are missing"}, ctx.RequestID, nil)
		return true
	}
	files := DiffTrees(store, repo, baseCommit.Tree, headCommit.Tree)
	out := []any{}
	for _, file := range files {
		out = append(out, map[string]any{
			"filename":  file.Filename,
			"status":    file.Status,
			"additions": file.Additions,
			"deletions": file.Deletions,
			"changes":   file.Changes,
		})
	}
	SendJSON(res, 200, out, ctx.RequestID, nil)
	return true
}

func handlePullMerge(store *Store, ctx *Ctx, res Responder, owner, name, rawNumber string) bool {
	if ctx.Method != "PUT" {
		return false
	}
	repo := findRepo(store, ctx, res, owner, name)
	if repo == nil {
		return true
	}
	pull := findPull(repo, ctx, res, rawNumber)
	if pull == nil {
		return true
	}
	if pull.State == "closed" {
		SendJSON(res, 405, map[string]any{"message": "Pull request is already closed"}, ctx.RequestID, nil)
		return true
	}
	merged := SquashMergePull(store, repo, pull)
	if merged == "" {
		SendJSON(res, 405, map[string]any{"message": "Merge conflict"}, ctx.RequestID, nil)
		return true
	}
	SendJSON(res, 200, map[string]any{
		"merged":  true,
		"sha":     merged,
		"message": "Merge pull request #" + itoa(pull.Number) + " from " + repo.Owner + "/" + pull.HeadRef,
	}, ctx.RequestID, nil)
	return true
}

// HandleRest handles a request under /api/v3. It returns true when the path
// was a REST endpoint (handled, even on error); false when it is not one.
func HandleRest(store *Store, ctx *Ctx, res Responder) bool {
	if len(ctx.Segments) < 2 {
		return false
	}
	rest := ctx.Segments[2:]
	if len(rest) == 0 {
		if ctx.Method != "GET" {
			return false
		}
		SendJSON(res, 200, map[string]any{
			"current_user_url": "/api/v3/user",
			"repository_url":   "/api/v3/repos/{owner}/{repo}",
		}, ctx.RequestID, map[string]string{"X-OAuth-Scopes": "repo, user"})
		return true
	}
	if len(rest) == 1 && rest[0] == "meta" {
		if ctx.Method != "GET" {
			return false
		}
		SendJSON(res, 200, map[string]any{
			"installed_version":                  "1.0.0",
			"verifiable_password_authentication": false,
			"hooks":                              []any{},
			"git":                                []any{},
			"pages":                              []any{},
		}, ctx.RequestID, nil)
		return true
	}
	if len(rest) == 1 && rest[0] == "user" {
		if ctx.Method != "GET" {
			return false
		}
		SendJSON(res, 200, viewerJSON(), ctx.RequestID, nil)
		return true
	}
	if len(rest) == 2 && rest[0] == "user" && rest[1] == "repos" {
		return handleUserRepos(store, ctx, res)
	}
	if rest[0] == "repos" && len(rest) >= 3 {
		owner := rest[1]
		name := rest[2]
		tail := rest[3:]
		if len(tail) == 0 {
			return handleRepo(store, ctx, res, owner, name)
		}
		if len(tail) == 1 && tail[0] == "branches" {
			return handleBranches(store, ctx, res, owner, name)
		}
		if len(tail) >= 2 && tail[0] == "branches" {
			return handleBranch(store, ctx, res, owner, name, strings.Join(tail[1:], "/"))
		}
		if len(tail) == 1 && tail[0] == "commits" {
			return handleCommits(store, ctx, res, owner, name)
		}
		if len(tail) == 2 && tail[0] == "git" && tail[1] == "refs" {
			return handleCreateRef(store, ctx, res, owner, name)
		}
		if len(tail) >= 3 && tail[0] == "git" && tail[1] == "refs" && tail[2] == "heads" {
			return handleGetRef(store, ctx, res, owner, name, strings.Join(tail[3:], "/"))
		}
		if len(tail) >= 1 && tail[0] == "contents" {
			path := strings.Join(tail[1:], "/")
			if ctx.Method == "GET" {
				return handleGetContents(store, ctx, res, owner, name, path)
			}
			if ctx.Method == "PUT" {
				return handlePutContents(store, ctx, res, owner, name, path)
			}
			if ctx.Method == "DELETE" {
				return handleDeleteContents(store, ctx, res, owner, name, path)
			}
			return false
		}
		if len(tail) == 1 && tail[0] == "labels" {
			return handleLabels(store, ctx, res, owner, name)
		}
		if len(tail) == 1 && tail[0] == "issues" {
			return handleIssues(store, ctx, res, owner, name)
		}
		if len(tail) == 2 && tail[0] == "issues" {
			return handleIssue(store, ctx, res, owner, name, tail[1])
		}
		if len(tail) == 3 && tail[0] == "issues" && tail[2] == "comments" {
			return handleIssueComments(store, ctx, res, owner, name, tail[1])
		}
		if len(tail) == 1 && tail[0] == "pulls" {
			return handlePulls(store, ctx, res, owner, name)
		}
		if len(tail) == 2 && tail[0] == "pulls" {
			return handlePull(store, ctx, res, owner, name, tail[1])
		}
		if len(tail) == 3 && tail[0] == "pulls" && tail[2] == "files" {
			return handlePullFiles(store, ctx, res, owner, name, tail[1])
		}
		if len(tail) == 3 && tail[0] == "pulls" && tail[2] == "merge" {
			return handlePullMerge(store, ctx, res, owner, name, tail[1])
		}
	}
	return false
}
