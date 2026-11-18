// shapes.go — the shared REST response shapes (repo, issue, comment, pull).
//
// These are used by both rest.go and mcp.go so MCP and REST return the
// identical object shapes. A faithful port of the repoJson / issueJson /
// commentJson / pullJson / viewerJson functions in rest.ts.
//
// The map[string]any builders (RepoJSON / IssueJSON / PullJSON) are the
// shared source of truth. The *Bytes builders below are hand-rolled JSON for
// the perf hot paths (single GET/POST issue, repo, pull): encoding/json on a
// map pays reflection + key sorting per value, while the hand-rolled version
// writes the same fields in the same order with no allocation beyond the
// output buffer. Values are identical; only the encoding path differs.

package main

import (
	"encoding/base64"
	"strconv"
	"strings"
)

// Minimal JSON object writer: appends `"key":value` pairs in insertion order.
// Writes straight into one shared builder; strings are escaped inline so no
// per-string allocation happens (JSONEscape allocates a fresh builder per
// string, which made the first hand-rolled version slower than encoding/json).
type jsonObj struct {
	sb    *strings.Builder
	first bool
}

func newJSONObj(sb *strings.Builder) *jsonObj {
	o := &jsonObj{sb: sb, first: true}
	sb.WriteByte('{')
	return o
}

// escapeInto appends s as a JSON string literal to sb.
func escapeInto(sb *strings.Builder, s string) {
	sb.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			sb.WriteString(`\"`)
		case '\\':
			sb.WriteString(`\\`)
		case '\n':
			sb.WriteString(`\n`)
		case '\r':
			sb.WriteString(`\r`)
		case '\t':
			sb.WriteString(`\t`)
		case '\b':
			sb.WriteString(`\b`)
		case '\f':
			sb.WriteString(`\f`)
		default:
			if c < 0x20 {
				const hex = "0123456789abcdef"
				sb.WriteString(`\u00`)
				sb.WriteByte(hex[c>>4])
				sb.WriteByte(hex[c&0xf])
			} else {
				sb.WriteByte(c)
			}
		}
	}
	sb.WriteByte('"')
}

func (o *jsonObj) str(key, value string) {
	o.sep()
	escapeInto(o.sb, key)
	o.sb.WriteByte(':')
	escapeInto(o.sb, value)
}

func (o *jsonObj) num(key string, value int) {
	o.sep()
	escapeInto(o.sb, key)
	o.sb.WriteByte(':')
	o.sb.WriteString(strconv.Itoa(value))
}

func (o *jsonObj) bool(key string, value bool) {
	o.sep()
	escapeInto(o.sb, key)
	o.sb.WriteByte(':')
	if value {
		o.sb.WriteString("true")
	} else {
		o.sb.WriteString("false")
	}
}

func (o *jsonObj) null(key string) {
	o.sep()
	escapeInto(o.sb, key)
	o.sb.WriteString(":null")
}

// raw inserts a pre-encoded JSON value (object/array) for key.
func (o *jsonObj) raw(key string, value []byte) {
	o.sep()
	escapeInto(o.sb, key)
	o.sb.WriteByte(':')
	o.sb.Write(value)
}

func (o *jsonObj) sep() {
	if !o.first {
		o.sb.WriteByte(',')
	}
	o.first = false
}

func (o *jsonObj) end() {
	o.sb.WriteByte('}')
}

// The JSON for an issue's labels array, in order.
func labelsJSONInto(sb *strings.Builder, repo *Repo, issue *Issue) {
	sb.WriteByte('[')
	for i, name := range issue.Labels {
		if i > 0 {
			sb.WriteByte(',')
		}
		color := "ededed"
		if l, ok := repo.Labels[name]; ok {
			color = l.Color
		}
		o := newJSONObj(sb)
		o.str("name", name)
		o.str("color", color)
		o.end()
	}
	sb.WriteByte(']')
}

func userJSONInto(sb *strings.Builder) {
	o := newJSONObj(sb)
	o.str("login", ViewerLogin)
	o.num("id", ViewerID)
	o.str("type", "User")
	o.end()
}

func repoJSONBytes(repo *Repo) []byte {
	var sb strings.Builder
	sb.Grow(512)
	o := newJSONObj(&sb)
	o.num("id", repo.ID)
	o.str("node_id", nodeID("Repository", itoa(repo.ID)))
	o.str("name", repo.Name)
	o.str("full_name", repo.Owner+"/"+repo.Name)
	// owner object, written inline
	o.sep()
	escapeInto(&sb, "owner")
	sb.WriteByte(':')
	owner := newJSONObj(&sb)
	owner.str("login", repo.Owner)
	owner.num("id", ViewerID)
	owner.str("node_id", nodeID("User", itoa(ViewerID)))
	owner.str("type", "User")
	owner.end()
	o.bool("private", repo.IsPrivate)
	o.str("description", repo.Description)
	o.str("default_branch", repo.DefaultBranch)
	o.str("created_at", repo.CreatedAt)
	o.str("updated_at", repo.UpdatedAt)
	o.str("html_url", "https://github.com/"+repo.Owner+"/"+repo.Name)
	o.str("clone_url", "https://github.com/"+repo.Owner+"/"+repo.Name+".git")
	o.bool("has_issues", true)
	o.bool("has_projects", false)
	o.bool("has_wiki", false)
	o.end()
	return []byte(sb.String())
}

func issueJSONBytes(repo *Repo, issue *Issue) []byte {
	var sb strings.Builder
	sb.Grow(512)
	o := newJSONObj(&sb)
	o.num("id", issue.ID)
	o.str("node_id", nodeID("Issue", itoa(repo.ID)+":"+itoa(issue.Number)))
	o.num("number", issue.Number)
	o.str("title", issue.Title)
	o.str("body", issue.Body)
	o.str("state", issue.State)
	o.sep()
	escapeInto(&sb, "labels")
	sb.WriteByte(':')
	labelsJSONInto(&sb, repo, issue)
	o.str("created_at", issue.CreatedAt)
	o.str("updated_at", issue.UpdatedAt)
	o.sep()
	escapeInto(&sb, "user")
	sb.WriteByte(':')
	userJSONInto(&sb)
	o.num("comments", len(issue.Comments))
	o.end()
	return []byte(sb.String())
}

func pullJSONBytes(repo *Repo, pull *PullRequest) []byte {
	var sb strings.Builder
	sb.Grow(1024)
	headRepo := repoJSONBytes(repo)
	o := newJSONObj(&sb)
	o.num("id", pull.ID)
	o.str("node_id", nodeID("PullRequest", itoa(repo.ID)+":"+itoa(pull.Number)))
	o.num("number", pull.Number)
	o.str("title", pull.Title)
	o.str("body", pull.Body)
	o.str("state", pull.State)
	o.bool("merged", pull.Merged)
	if pull.MergedAt != "" {
		o.str("merged_at", pull.MergedAt)
	} else {
		o.null("merged_at")
	}
	if pull.MergeCommitSha != "" {
		o.str("merge_commit_sha", pull.MergeCommitSha)
	} else {
		o.null("merge_commit_sha")
	}
	// head object, written inline
	o.sep()
	escapeInto(&sb, "head")
	sb.WriteByte(':')
	head := newJSONObj(&sb)
	head.str("label", repo.Owner+":"+pull.HeadRef)
	head.str("ref", pull.HeadRef)
	head.str("sha", pull.HeadSha)
	head.raw("repo", headRepo)
	head.end()
	// base object, written inline
	o.sep()
	escapeInto(&sb, "base")
	sb.WriteByte(':')
	base := newJSONObj(&sb)
	base.str("label", repo.Owner+":"+pull.BaseRef)
	base.str("ref", pull.BaseRef)
	base.str("sha", pull.BaseSha)
	base.raw("repo", headRepo)
	base.end()
	o.str("created_at", pull.CreatedAt)
	o.str("updated_at", pull.UpdatedAt)
	o.sep()
	escapeInto(&sb, "user")
	sb.WriteByte(':')
	userJSONInto(&sb)
	o.end()
	return []byte(sb.String())
}

func nodeID(kind, value string) string {
	return base64.StdEncoding.EncodeToString([]byte(kind + ":" + value))
}

func viewerJSON() map[string]any {
	return map[string]any{
		"login":     ViewerLogin,
		"id":        ViewerID,
		"node_id":   nodeID("User", itoa(ViewerID)),
		"avatar_url": "",
		"type":      "User",
		"site_admin": false,
		"name":      ViewerName,
		"email":     ViewerEmail,
		"html_url":  "https://github.com/" + ViewerLogin,
	}
}

// Shared with the MCP tools so MCP and REST return the identical repository shape.
func RepoJSON(repo *Repo) map[string]any {
	return map[string]any{
		"id":             repo.ID,
		"node_id":        nodeID("Repository", itoa(repo.ID)),
		"name":           repo.Name,
		"full_name":      repo.Owner + "/" + repo.Name,
		"owner":          map[string]any{"login": repo.Owner, "id": ViewerID, "node_id": nodeID("User", itoa(ViewerID)), "type": "User"},
		"private":        repo.IsPrivate,
		"description":    repo.Description,
		"default_branch": repo.DefaultBranch,
		"created_at":     repo.CreatedAt,
		"updated_at":     repo.UpdatedAt,
		"html_url":       "https://github.com/" + repo.Owner + "/" + repo.Name,
		"clone_url":      "https://github.com/" + repo.Owner + "/" + repo.Name + ".git",
		"has_issues":     true,
		"has_projects":   false,
		"has_wiki":       false,
	}
}

// Shared with the MCP tools so MCP and REST return the identical issue shape.
func IssueJSON(repo *Repo, issue *Issue) map[string]any {
	labels := []any{}
	for _, name := range issue.Labels {
		color := "ededed"
		if l, ok := repo.Labels[name]; ok {
			color = l.Color
		}
		labels = append(labels, map[string]any{"name": name, "color": color})
	}
	return map[string]any{
		"id":         issue.ID,
		"node_id":    nodeID("Issue", itoa(repo.ID)+":"+itoa(issue.Number)),
		"number":     issue.Number,
		"title":      issue.Title,
		"body":       issue.Body,
		"state":      issue.State,
		"labels":     labels,
		"created_at": issue.CreatedAt,
		"updated_at": issue.UpdatedAt,
		"user":       map[string]any{"login": ViewerLogin, "id": ViewerID, "type": "User"},
		"comments":   len(issue.Comments),
	}
}

func commentJSON(repo *Repo, comment *IssueComment) map[string]any {
	return map[string]any{
		"id":         comment.ID,
		"node_id":    nodeID("IssueComment", itoa(repo.ID)+":"+itoa(comment.ID)),
		"body":       comment.Body,
		"created_at": comment.CreatedAt,
		"user":       map[string]any{"login": ViewerLogin, "id": ViewerID, "type": "User"},
	}
}

// Shared with the MCP tools so MCP and REST return the identical pull-request shape.
func PullJSON(repo *Repo, pull *PullRequest) map[string]any {
	headRepo := RepoJSON(repo)
	// mergedAt / merge_commit_sha are null (not "") before a merge.
	var mergedAt any
	if pull.MergedAt != "" {
		mergedAt = pull.MergedAt
	}
	var mergeCommitSha any
	if pull.MergeCommitSha != "" {
		mergeCommitSha = pull.MergeCommitSha
	}
	return map[string]any{
		"id":               pull.ID,
		"node_id":          nodeID("PullRequest", itoa(repo.ID)+":"+itoa(pull.Number)),
		"number":           pull.Number,
		"title":            pull.Title,
		"body":             pull.Body,
		"state":            pull.State,
		"merged":           pull.Merged,
		"merged_at":        mergedAt,
		"merge_commit_sha": mergeCommitSha,
		"head":             map[string]any{"label": repo.Owner + ":" + pull.HeadRef, "ref": pull.HeadRef, "sha": pull.HeadSha, "repo": headRepo},
		"base":             map[string]any{"label": repo.Owner + ":" + pull.BaseRef, "ref": pull.BaseRef, "sha": pull.BaseSha, "repo": headRepo},
		"created_at":       pull.CreatedAt,
		"updated_at":       pull.UpdatedAt,
		"user":             map[string]any{"login": ViewerLogin, "id": ViewerID, "type": "User"},
	}
}
