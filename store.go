// store.go — the forge's in-memory store and the pure merge / diff helpers.
//
// A faithful port of the TypeScript store.ts. One Repo owns its own object map
// (objects never cross repositories), its refs, and its issue / pull-request
// tables. No wall clock, no randomness: created/updated stamps are a fixed
// constant, request ids and REST ids come from monotonic counters.

package main

import (
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

const ViewerLogin = "adaiversen"
const ViewerID = 1000
const ViewerName = "ada iversen"
const ViewerEmail = "adaiversen@example.invalid"

// The only timestamp the forge ever stamps: 2025-01-01T00:00:00Z, fixed for determinism.
const FixedTimestamp = "2025-01-01T00:00:00Z"
const FixedEpoch int64 = 1735689600

// The identity written into every commit the forge authors itself.
func GitIdentity() string { return ViewerName + " <" + ViewerEmail + ">" }

type Label struct {
	Name  string
	Color string
}

type IssueComment struct {
	ID        int
	Body      string
	CreatedAt string
}

type Issue struct {
	ID        int
	Number    int
	Title     string
	Body      string
	State     string // "open" | "closed"
	Labels    []string
	Comments  []*IssueComment
	CreatedAt string
	UpdatedAt string
}

type PullRequest struct {
	ID             int
	Number         int
	Title          string
	Body           string
	State          string // "open" | "closed"
	HeadRef        string
	BaseRef        string
	HeadSha        string
	BaseSha        string
	Merged         bool
	MergedAt       string
	MergeCommitSha string
	CreatedAt      string
	UpdatedAt      string
}

type Repo struct {
	ID               int
	Owner            string
	Name             string
	Description      string
	IsPrivate        bool
	DefaultBranch    string
	Objects          map[string]*GitObject
	Refs             map[string]string
	Issues           []*Issue
	Pulls            []*PullRequest
	Labels           map[string]*Label
	// Pack bytes keyed by the sorted object-oid set they were built from. Objects are
	// content-addressed and immutable, so a pack for an oid set never goes stale;
	// the fetch path reuses it for repeated identical fetches (the clone workload
	// fetches the same tip 50 times). Capped at 128 entries, FIFO.
	// PackMu guards PackCache/PackCacheOrder: fetches run under the store's
	// shared read lock, so the cache needs its own mutex.
	// PackInflight (guarded by PackMu) is the singleflight table for pack
	// builds: the clone workload fires 50 concurrent fetches of the same
	// object set, and without it every goroutine builds the same pack
	// (pure-Go deflate) instead of one building and the rest waiting.
	PackMu       sync.Mutex
	PackCache    map[string][]byte
	PackCacheOrder []string
	PackInflight map[string]chan struct{}
	NextIssueNumber int
	NextPullNumber  int
	CreatedAt       string
	UpdatedAt       string
}

func RepoKey(owner, name string) string { return owner + "/" + name }

type Store struct {
	mu            sync.RWMutex
	Repos         map[string]*Repo
	NextRepoID    int
	NextRestID    int
	nextRequestID atomic.Int64
}

func NewStore() *Store {
	s := &Store{Repos: map[string]*Repo{}, NextRepoID: 1, NextRestID: 1}
	s.nextRequestID.Store(1)
	return s
}

func (s *Store) GetRepo(owner, name string) *Repo {
	return s.Repos[RepoKey(owner, name)]
}

func (s *Store) CreateRepo(owner, name string, description string, isPrivate bool) *Repo {
	if _, ok := s.Repos[RepoKey(owner, name)]; ok {
		return nil
	}
	repo := &Repo{
		ID:              s.NextRepoID,
		Owner:           owner,
		Name:            name,
		Description:     description,
		IsPrivate:       isPrivate,
		DefaultBranch:   "main",
		Objects:         map[string]*GitObject{},
		Refs:            map[string]string{},
		Issues:          []*Issue{},
		Pulls:           []*PullRequest{},
		Labels:          map[string]*Label{},
		PackCache:       map[string][]byte{},
		PackInflight:    map[string]chan struct{}{},
		NextIssueNumber: 1,
		NextPullNumber:  1,
		CreatedAt:       FixedTimestamp,
		UpdatedAt:       FixedTimestamp,
	}
	s.NextRepoID++
	s.Repos[RepoKey(owner, name)] = repo
	return repo
}

func (s *Store) DeleteRepo(owner, name string) bool {
	key := RepoKey(owner, name)
	if _, ok := s.Repos[key]; !ok {
		return false
	}
	delete(s.Repos, key)
	return true
}

// -- objects --------------------------------------------------------------------------

func (s *Store) PutObject(repo *Repo, typ GitObjectType, data []byte) string {
	oid := HashObject(typ, data)
	if _, ok := repo.Objects[oid]; !ok {
		repo.Objects[oid] = &GitObject{Type: typ, Data: data}
	}
	return oid
}

func (s *Store) GetObject(repo *Repo, oid string) *GitObject {
	return repo.Objects[oid]
}

// Create a commit object and store it; the message must already end with '\n'.
func (s *Store) CreateCommit(repo *Repo, tree string, parents []string, message string) string {
	data := BuildCommit(tree, parents, GitIdentity(), GitIdentity(), FixedEpoch, message)
	return s.PutObject(repo, ObjCommit, data)
}

type RefCommit struct {
	OID  string
	Tree string
}

// The commit object for a ref, or nil when the ref is missing / not a commit.
func (s *Store) RefCommit(repo *Repo, ref string) *RefCommit {
	oid, ok := repo.Refs[ref]
	if !ok {
		return nil
	}
	obj := repo.Objects[oid]
	if obj == nil || obj.Type == ObjTag || obj.Type != ObjCommit {
		return nil
	}
	c, err := ParseCommit(obj.Data)
	if err != nil {
		return nil
	}
	return &RefCommit{OID: oid, Tree: c.Tree}
}

// -- issues and pulls -------------------------------------------------------------------

func (s *Store) CreateIssue(repo *Repo, title, body string, labels []string) *Issue {
	issue := &Issue{
		ID:        s.NextRestID,
		Number:    repo.NextIssueNumber,
		Title:     title,
		Body:      body,
		State:     "open",
		Labels:    labels,
		Comments:  []*IssueComment{},
		CreatedAt: FixedTimestamp,
		UpdatedAt: FixedTimestamp,
	}
	s.NextRestID++
	repo.NextIssueNumber++
	repo.Issues = append(repo.Issues, issue)
	return issue
}

func (s *Store) CreatePull(repo *Repo, title, body, headRef, baseRef, headSha, baseSha string) *PullRequest {
	pull := &PullRequest{
		ID:        s.NextRestID,
		Number:    repo.NextPullNumber,
		Title:     title,
		Body:      body,
		State:     "open",
		HeadRef:   headRef,
		BaseRef:   baseRef,
		HeadSha:   headSha,
		BaseSha:   baseSha,
		Merged:    false,
		CreatedAt: FixedTimestamp,
		UpdatedAt: FixedTimestamp,
	}
	s.NextRestID++
	repo.NextPullNumber++
	repo.Pulls = append(repo.Pulls, pull)
	return pull
}

func (s *Store) AddComment(issue *Issue, body string) *IssueComment {
	comment := &IssueComment{ID: s.NextRestID, Body: body, CreatedAt: FixedTimestamp}
	s.NextRestID++
	issue.Comments = append(issue.Comments, comment)
	return comment
}

// Locking: handlers take the store lock around each request (RLock for reads,
// Lock for writes) so concurrent requests are atomic, matching the Node
// single-threaded semantics. The lock is taken in app.go, not here.

func (s *Store) NextRequestID() int64 { return s.nextRequestID.Add(1) - 1 }

func (s *Store) RLock()   { s.mu.RLock() }
func (s *Store) RUnlock() { s.mu.RUnlock() }
func (s *Store) Lock()    { s.mu.Lock() }
func (s *Store) Unlock()  { s.mu.Unlock() }

// ---------------------------------------------------------------------------------------------
// merge and diff helpers (pure; operate on a repo's object map)
// ---------------------------------------------------------------------------------------------

func IsDescendant(repo *Repo, descendant, ancestor string) bool {
	if descendant == ancestor {
		return true
	}
	seen := map[string]bool{}
	queue := []string{descendant}
	for len(queue) > 0 {
		oid := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if oid == ancestor {
			return true
		}
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj := repo.Objects[oid]
		if obj == nil || obj.Type != ObjCommit {
			continue
		}
		c, err := ParseCommit(obj.Data)
		if err != nil {
			continue
		}
		queue = append(queue, c.Parents...)
	}
	return false
}

// All ancestors of `oid` (including itself), as a map oid -> distance from `oid`.
func ancestorsWithDistance(repo *Repo, oid string) map[string]int {
	out := map[string]int{}
	type item struct {
		oid string
		d   int
	}
	queue := []item{{oid, 0}}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if _, ok := out[cur.oid]; ok {
			continue
		}
		out[cur.oid] = cur.d
		obj := repo.Objects[cur.oid]
		if obj == nil || obj.Type != ObjCommit {
			continue
		}
		c, err := ParseCommit(obj.Data)
		if err != nil {
			continue
		}
		for _, p := range c.Parents {
			queue = append(queue, item{p, cur.d + 1})
		}
	}
	return out
}

// The merge base of two commits: the shared ancestor closest to both (smallest distance sum).
func MergeBase(repo *Repo, left, right string) string {
	leftAnc := ancestorsWithDistance(repo, left)
	rightAnc := ancestorsWithDistance(repo, right)
	best := ""
	bestScore := int(^uint(0) >> 1)
	for oid, d := range leftAnc {
		other, ok := rightAnc[oid]
		if !ok {
			continue
		}
		if score := d + other; score < bestScore {
			bestScore = score
			best = oid
		}
	}
	return best
}

type treeNode struct {
	files map[string]TreeEntry
	dirs  map[string]*treeNode
}

// Build a nested tree from a flat path map, storing every intermediate tree object.
func BuildTreeFromFlat(store *Store, repo *Repo, flat map[string]TreeEntry) string {
	root := &treeNode{files: map[string]TreeEntry{}, dirs: map[string]*treeNode{}}
	// Deterministic iteration: sort paths so tree construction order is stable.
	paths := make([]string, 0, len(flat))
	for p := range flat {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, path := range paths {
		entry := flat[path]
		parts := strings.Split(path, "/")
		node := root
		for i := 0; i < len(parts)-1; i++ {
			child, ok := node.dirs[parts[i]]
			if !ok {
				child = &treeNode{files: map[string]TreeEntry{}, dirs: map[string]*treeNode{}}
				node.dirs[parts[i]] = child
			}
			node = child
		}
		node.files[parts[len(parts)-1]] = entry
	}
	var build func(node *treeNode) string
	build = func(node *treeNode) string {
		var entries []TreeEntry
		for name, e := range node.files {
			entries = append(entries, TreeEntry{Mode: e.Mode, Name: name, OID: e.OID})
		}
		for name, child := range node.dirs {
			entries = append(entries, TreeEntry{Mode: "40000", Name: name, OID: build(child)})
		}
		return store.PutObject(repo, ObjTree, BuildTree(entries))
	}
	return build(root)
}

type MergeResult struct {
	Conflict  bool
	Tree      string
	Conflicts []string
}

// Three-way squash merge of `headTip` onto `baseTip`. Returns the merged tree (or ""
// when the two sides changed the same file differently) plus the conflicting paths.
func ThreeWayMerge(store *Store, repo *Repo, baseTip, headTip string) MergeResult {
	lookup := func(oid string) *GitObject { return repo.Objects[oid] }
	baseObj := repo.Objects[baseTip]
	headObj := repo.Objects[headTip]
	if baseObj == nil || baseObj.Type != ObjCommit || headObj == nil || headObj.Type != ObjCommit {
		return MergeResult{Conflict: true}
	}
	baseCommit, _ := ParseCommit(baseObj.Data)
	headCommit, _ := ParseCommit(headObj.Data)
	base := MergeBase(repo, baseTip, headTip)
	var ancestorTree string
	if base != "" {
		if bc, ok := repo.Objects[base]; ok {
			if c, err := ParseCommit(bc.Data); err == nil {
				ancestorTree = c.Tree
			}
		}
	}
	baseFlat, _ := FlattenTree(lookup, baseCommit.Tree)
	headFlat, _ := FlattenTree(lookup, headCommit.Tree)
	ancestorFlat := map[string]TreeEntry{}
	if ancestorTree != "" {
		ancestorFlat, _ = FlattenTree(lookup, ancestorTree)
	}
	result := map[string]TreeEntry{}
	var conflicts []string
	pathSet := map[string]bool{}
	for p := range ancestorFlat {
		pathSet[p] = true
	}
	for p := range baseFlat {
		pathSet[p] = true
	}
	for p := range headFlat {
		pathSet[p] = true
	}
	for path := range pathSet {
		ancestor, hasAncestor := ancestorFlat[path]
		baseSide, hasBase := baseFlat[path]
		headSide, hasHead := headFlat[path]
		// Mirror the TS exactly: headChanged = headSide?.oid !== ancestor?.oid
		// (undefined !== oid is true; undefined !== undefined is false).
		headChanged := oidChanged(headSide, hasHead, ancestor, hasAncestor)
		baseChanged := oidChanged(baseSide, hasBase, ancestor, hasAncestor)
		if headChanged && baseChanged && oidDiffers(headSide, hasHead, baseSide, hasBase) {
			conflicts = append(conflicts, path)
			continue
		}
		var winner *TreeEntry
		if headChanged {
			if hasHead {
				e := headSide
				winner = &e
			}
		} else {
			if hasBase {
				e := baseSide
				winner = &e
			}
		}
		if winner != nil {
			result[path] = *winner
		}
	}
	if len(conflicts) > 0 {
		return MergeResult{Conflict: true, Conflicts: conflicts}
	}
	return MergeResult{Tree: BuildTreeFromFlat(store, repo, result)}
}

// oidChanged mirrors `side?.oid !== ancestor?.oid` (missing side = undefined).
func oidChanged(side TreeEntry, hasSide bool, ancestor TreeEntry, hasAncestor bool) bool {
	if !hasSide && !hasAncestor {
		return false
	}
	if !hasSide || !hasAncestor {
		return true
	}
	return side.OID != ancestor.OID
}

func oidDiffers(a TreeEntry, hasA bool, b TreeEntry, hasB bool) bool {
	if !hasA && !hasB {
		return false
	}
	if !hasA || !hasB {
		return true
	}
	return a.OID != b.OID
}

type FileChange struct {
	Filename  string
	Status    string // "added" | "removed" | "modified" | "changed"
	Additions int
	Deletions int
	Changes   int
}

type lineCounts struct {
	additions int
	deletions int
}

// Longest-common-subsequence line diff; falls back to a multiset count for very large files.
func LineDiff(oldText, newText string) lineCounts {
	splitLines := func(text string) []string {
		if text == "" {
			return nil
		}
		parts := strings.Split(text, "\n")
		if len(parts) > 0 && parts[len(parts)-1] == "" {
			parts = parts[:len(parts)-1]
		}
		return parts
	}
	oldLines := splitLines(oldText)
	newLines := splitLines(newText)
	if len(oldLines)*len(newLines) > 1000000 {
		oldCount := map[string]int{}
		for _, line := range oldLines {
			oldCount[line]++
		}
		kept := 0
		for _, line := range newLines {
			if c := oldCount[line]; c > 0 {
				oldCount[line] = c - 1
				kept++
			}
		}
		return lineCounts{additions: len(newLines) - kept, deletions: len(oldLines) - kept}
	}
	rows := len(oldLines) + 1
	cols := len(newLines) + 1
	table := make([]uint32, rows*cols)
	for i := len(oldLines) - 1; i >= 0; i-- {
		for j := len(newLines) - 1; j >= 0; j-- {
			if oldLines[i] == newLines[j] {
				table[i*cols+j] = table[(i+1)*cols+j+1] + 1
			} else {
				a := table[(i+1)*cols+j]
				b := table[i*cols+j+1]
				if a > b {
					table[i*cols+j] = a
				} else {
					table[i*cols+j] = b
				}
			}
		}
	}
	i, j := 0, 0
	additions, deletions := 0, 0
	for i < len(oldLines) && j < len(newLines) {
		if oldLines[i] == newLines[j] {
			i++
			j++
		} else if table[(i+1)*cols+j] >= table[i*cols+j+1] {
			deletions++
			i++
		} else {
			additions++
			j++
		}
	}
	deletions += len(oldLines) - i
	additions += len(newLines) - j
	return lineCounts{additions: additions, deletions: deletions}
}

// File-level diff between two trees: status and line counts per changed path.
func DiffTrees(store *Store, repo *Repo, baseTree, headTree string) []FileChange {
	lookup := func(oid string) *GitObject { return repo.Objects[oid] }
	baseFlat, _ := FlattenTree(lookup, baseTree)
	headFlat, _ := FlattenTree(lookup, headTree)
	var changes []FileChange
	pathSet := map[string]bool{}
	for p := range baseFlat {
		pathSet[p] = true
	}
	for p := range headFlat {
		pathSet[p] = true
	}
	for path := range pathSet {
		baseSide, hasBase := baseFlat[path]
		headSide, hasHead := headFlat[path]
		if hasBase && !hasHead {
			oldText := ""
			if o := repo.Objects[baseSide.OID]; o != nil {
				oldText = string(o.Data)
			}
			c := LineDiff(oldText, "")
			changes = append(changes, FileChange{Filename: path, Status: "removed", Additions: c.additions, Deletions: c.deletions, Changes: c.additions + c.deletions})
		} else if !hasBase && hasHead {
			newText := ""
			if o := repo.Objects[headSide.OID]; o != nil {
				newText = string(o.Data)
			}
			c := LineDiff("", newText)
			changes = append(changes, FileChange{Filename: path, Status: "added", Additions: c.additions, Deletions: c.deletions, Changes: c.additions + c.deletions})
		} else if hasBase && hasHead && baseSide.OID != headSide.OID {
			oldText := ""
			if o := repo.Objects[baseSide.OID]; o != nil {
				oldText = string(o.Data)
			}
			newText := ""
			if o := repo.Objects[headSide.OID]; o != nil {
				newText = string(o.Data)
			}
			c := LineDiff(oldText, newText)
			status := "modified"
			if c.additions > 0 && c.deletions == 0 {
				status = "added"
			}
			changes = append(changes, FileChange{Filename: path, Status: status, Additions: c.additions, Deletions: c.deletions, Changes: c.additions + c.deletions})
		}
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Filename < changes[j].Filename })
	return changes
}

// True when a commit's tree contains `path` with exactly `blobOID`.
func TreeContainsBlob(repo *Repo, treeOID, path, blobOID string) bool {
	flat, err := FlattenTree(func(oid string) *GitObject { return repo.Objects[oid] }, treeOID)
	if err != nil {
		return false
	}
	e, ok := flat[path]
	return ok && e.OID == blobOID
}

// Mergeability of a pull request: MERGEABLE when a clean squash merge exists, else CONFLICTING.
func PullMergeability(store *Store, repo *Repo, pull *PullRequest) string {
	headObj := repo.Objects[pull.HeadSha]
	baseObj := repo.Objects[pull.BaseSha]
	if headObj == nil || headObj.Type != ObjCommit || baseObj == nil || baseObj.Type != ObjCommit {
		return "CONFLICTING"
	}
	merged := ThreeWayMerge(store, repo, pull.BaseSha, pull.HeadSha)
	if merged.Conflict {
		return "CONFLICTING"
	}
	return "MERGEABLE"
}

// Apply a squash merge to a pull request: the merged tree is built from the pull's
// frozen head/base, the merge commit's parent is the CURRENT base tip, and the base
// branch moves to the merge commit. Returns the merge commit oid, or "" on conflict.
func SquashMergePull(store *Store, repo *Repo, pull *PullRequest) string {
	baseRef := "refs/heads/" + pull.BaseRef
	baseTip, ok := repo.Refs[baseRef]
	if !ok {
		return ""
	}
	merged := ThreeWayMerge(store, repo, baseTip, pull.HeadSha)
	if merged.Conflict || merged.Tree == "" {
		return ""
	}
	message := "Merge pull request #" + itoa(pull.Number) + " from " + repo.Owner + "/" + pull.HeadRef + "\n"
	mergeCommit := store.CreateCommit(repo, merged.Tree, []string{baseTip}, message)
	repo.Refs[baseRef] = mergeCommit
	pull.State = "closed"
	pull.Merged = true
	pull.MergedAt = FixedTimestamp
	pull.MergeCommitSha = mergeCommit
	pull.UpdatedAt = FixedTimestamp
	return mergeCommit
}

// The object id of the commit that first introduced `path` on the branch, or "".
func BlobOIDAtPath(repo *Repo, commitOID, path string) string {
	obj := repo.Objects[commitOID]
	if obj == nil || obj.Type != ObjCommit {
		return ""
	}
	c, err := ParseCommit(obj.Data)
	if err != nil {
		return ""
	}
	flat, err := FlattenTree(func(oid string) *GitObject { return repo.Objects[oid] }, c.Tree)
	if err != nil {
		return ""
	}
	if e, ok := flat[path]; ok {
		return e.OID
	}
	return ""
}

// Apply `changes` (path -> blob oid, or "" to delete) onto a tree and return the new tree oid.
func ApplyTreeChanges(store *Store, repo *Repo, treeOID string, changes map[string]string) string {
	flat, _ := FlattenTree(func(oid string) *GitObject { return repo.Objects[oid] }, treeOID)
	for path, oid := range changes {
		if oid == "" {
			delete(flat, path)
		} else {
			flat[path] = TreeEntry{OID: oid, Mode: "100644"}
		}
	}
	return BuildTreeFromFlat(store, repo, flat)
}

// All commit oids reachable from `tip`, newest first (BFS by parent edges).
func CommitsFromTip(repo *Repo, tip string) []string {
	var out []string
	seen := map[string]bool{}
	queue := []string{tip}
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj := repo.Objects[oid]
		if obj == nil || obj.Type != ObjCommit {
			continue
		}
		out = append(out, oid)
		c, err := ParseCommit(obj.Data)
		if err != nil {
			continue
		}
		queue = append(queue, c.Parents...)
	}
	return out
}

func EmptyTreeOID(store *Store, repo *Repo) string {
	return store.PutObject(repo, ObjTree, BuildTree(nil))
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
