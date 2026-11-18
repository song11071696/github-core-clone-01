// git.go — the git smart-HTTP protocol: info/refs advertisements (v0 and v2),
// ls-refs, fetch (v2 and v0) and receive-pack.
//
// A faithful port of the TypeScript git.ts. Only the Go standard library.
//
// The object-selection rules for fetch follow the kit's conformance checks:
//   * a want's reachable closure is walked by parent edges, counting the distance
//     from the nearest want; a `deepen` limit keeps only commits closer than it;
//   * a non-shallow have contributes its whole closure (never re-sent);
//   * a shallow have contributes only itself, and the walk continues past it;
//   * new shallow boundaries are the included commits exactly depth-1 deep that
//     still have a parent; unshallow boundaries are previous boundaries that are
//     wanted or included and are not new boundaries;
//   * the pack is always a plain (undelted) pack, version 2, with a correct
//     SHA-1 trailer; an empty pack is still sent (32 bytes) so the client always
//     sees at least one sideband packet.

package main

import (
	"bytes"
	"sort"
	"strconv"
	"strings"
)

const gitAgent = "github-core-clone-01/1.0"
const sidebandChunk = 32768

type fetchArgs struct {
	wants       []string
	haves       []string
	prevShallow []string
	depth       *int
}

type fetchSelection struct {
	objects   map[string]*GitObject
	acks      []string
	shallow   []string
	unshallow []string
}

// Split decoded packets into sections at flush packets, returning the text lines of each.
func sectionsOf(packets []Pkt) [][]string {
	sections := [][]string{{}}
	for _, packet := range packets {
		if packet.Kind == PktFlush {
			sections = append(sections, []string{})
		} else if packet.Kind == PktData && packet.Payload != nil {
			last := len(sections) - 1
			sections[last] = append(sections[last], string(packet.Payload))
		}
	}
	return sections
}

// The annotated-tag target a ref points at, when the ref points at a tag object.
func peeledTarget(repo *Repo, oid string) string {
	object := repo.Objects[oid]
	if object == nil || object.Type != ObjTag {
		return ""
	}
	tag, err := ParseTag(object.Data)
	if err != nil {
		return ""
	}
	return tag.Object
}

// Refs in git's sorted order.
func sortedRefs(repo *Repo) []string {
	refs := make([]string, 0, len(repo.Refs))
	for ref := range repo.Refs {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	return refs
}

func v0Advertisement(repo *Repo, service string) []byte {
	var caps string
	if service == "git-upload-pack" {
		caps = "symref=HEAD:refs/heads/" + repo.DefaultBranch + " side-band-64k thin-pack ofs-delta shallow multi_ack agent=" + gitAgent
	} else {
		caps = "symref=HEAD:refs/heads/" + repo.DefaultBranch + " report-status side-band-64k delete-refs ofs-delta agent=" + gitAgent
	}
	var parts [][]byte
	parts = append(parts, PktLineString("# service="+service+"\n"))
	tip, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]
	if ok {
		parts = append(parts, PktLineString(tip+" HEAD\x00"+caps+"\n"))
	} else {
		parts = append(parts, PktLineString(ZeroOID+" capabilities^{}\x00"+caps+"\n"))
	}
	for _, ref := range sortedRefs(repo) {
		oid := repo.Refs[ref]
		parts = append(parts, PktLineString(oid+" "+ref+"\n"))
		if peeled := peeledTarget(repo, oid); peeled != "" {
			parts = append(parts, PktLineString(peeled+" "+ref+"^{}\n"))
		}
	}
	parts = append(parts, PktFlushBytes)
	return bytes.Join(parts, nil)
}

func v2Advertisement() []byte {
	return bytes.Join([][]byte{
		PktLineString("# service=git-upload-pack\n"),
		PktLineString("version 2\n"),
		PktLineString("agent=" + gitAgent + "\n"),
		PktLineString("ls-refs\n"),
		PktLineString("fetch=shallow filter\n"),
		PktLineString("object-format=sha1\n"),
		PktFlushBytes,
	}, nil)
}

func handleInfoRefs(store *Store, ctx *Ctx, res Responder, owner, repoName string) bool {
	service := ctx.QueryGet("service")
	if service != "git-upload-pack" && service != "git-receive-pack" {
		SendJSON(res, 404, map[string]any{"message": "Not Found"}, ctx.RequestID, nil)
		return true
	}
	repo := store.GetRepo(owner, repoName)
	if repo == nil {
		SendJSON(res, 404, map[string]any{"message": "Not Found"}, ctx.RequestID, nil)
		return true
	}
	wantsV2 := service == "git-upload-pack" && ctx.Headers["git-protocol"] == "version=2"
	var body []byte
	if wantsV2 {
		body = v2Advertisement()
	} else {
		body = v0Advertisement(repo, service)
	}
	SendBuffer(res, 200, "application/x-"+service+"-advertisement", body, ctx.RequestID)
	return true
}

func lsRefs(repo *Repo, args []string) []byte {
	clean := make([]string, len(args))
	for i, arg := range args {
		clean[i] = strings.TrimSuffix(arg, "\n")
	}
	wantsPeel := false
	wantsSymrefs := false
	var prefixes []string
	for _, arg := range clean {
		if arg == "peel" {
			wantsPeel = true
		}
		if arg == "symrefs" {
			wantsSymrefs = true
		}
		if strings.HasPrefix(arg, "ref-prefix ") {
			prefixes = append(prefixes, arg[len("ref-prefix "):])
		}
	}
	var parts [][]byte
	emit := func(oid, ref string) {
		line := oid + " " + ref
		if wantsPeel {
			if peeled := peeledTarget(repo, oid); peeled != "" {
				line += " peeled:" + peeled
			}
		}
		if wantsSymrefs && ref == "HEAD" {
			line += " symref-target:refs/heads/" + repo.DefaultBranch
		}
		parts = append(parts, PktLineString(line+"\n"))
	}
	matches := func(ref string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, prefix := range prefixes {
			if strings.HasPrefix(ref, prefix) {
				return true
			}
		}
		return false
	}
	if len(prefixes) == 0 {
		if tip, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]; ok {
			emit(tip, "HEAD")
		}
	}
	for _, ref := range sortedRefs(repo) {
		if !matches(ref) {
			continue
		}
		emit(repo.Refs[ref], ref)
	}
	parts = append(parts, PktFlushBytes)
	return bytes.Join(parts, nil)
}

// parseIntPrefix mirrors JavaScript's parseInt(text, 10) on an already-trimmed
// string: an optional sign followed by decimal digits, or NaN when there are none.
func parseIntPrefix(text string) (int, bool) {
	i := 0
	neg := false
	if i < len(text) && (text[i] == '+' || text[i] == '-') {
		neg = text[i] == '-'
		i++
	}
	start := i
	for i < len(text) && text[i] >= '0' && text[i] <= '9' {
		i++
	}
	if i == start {
		return 0, false
	}
	n, err := strconv.Atoi(text[:i])
	if err != nil {
		return 0, false
	}
	if neg {
		n = -n
	}
	return n, true
}

func parseFetchArgs(lines []string, prefix string) fetchArgs {
	_ = prefix
	args := fetchArgs{}
	for _, line := range lines {
		text := strings.TrimSuffix(line, "\n")
		switch {
		case strings.HasPrefix(text, "want "):
			args.wants = append(args.wants, strings.Split(text[5:], " ")[0])
		case strings.HasPrefix(text, "have "):
			args.haves = append(args.haves, strings.Split(text[5:], " ")[0])
		case strings.HasPrefix(text, "shallow "):
			args.prevShallow = append(args.prevShallow, strings.TrimSpace(text[8:]))
		case strings.HasPrefix(text, "deepen "):
			if value, ok := parseIntPrefix(strings.TrimSpace(text[7:])); ok && value > 0 {
				v := value
				args.depth = &v
			}
		}
	}
	return args
}

func containsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

func selectFetch(store *Store, repo *Repo, args fetchArgs) fetchSelection {
	heldShallow := map[string]bool{}
	for _, oid := range args.prevShallow {
		if IsObjectID(oid) {
			if _, ok := repo.Objects[oid]; ok {
				heldShallow[oid] = true
			}
		}
	}
	heldNonShallow := map[string]bool{}
	for _, have := range args.haves {
		if heldShallow[have] {
			continue
		}
		if _, ok := repo.Objects[have]; !ok {
			continue
		}
		for oid := range collectReachableSafe(repo, have) {
			heldNonShallow[oid] = true
		}
	}

	type queueItem struct {
		oid   string
		depth int
	}
	distance := map[string]int{}
	var queue []queueItem
	for _, want := range args.wants {
		if !IsObjectID(want) {
			continue
		}
		if _, ok := distance[want]; ok {
			continue
		}
		distance[want] = 0
		queue = append(queue, queueItem{want, 0})
	}
	included := map[string]bool{}
	var includedOrder []string
	includedTags := map[string]bool{}
	for len(queue) > 0 {
		item := queue[0]
		queue = queue[1:]
		oid, depth := item.oid, item.depth
		object := repo.Objects[oid]
		if object == nil {
			continue
		}
		if object.Type == ObjTag {
			includedTags[oid] = true
			tag, err := ParseTag(object.Data)
			if err != nil {
				continue
			}
			target := tag.Object
			if _, ok := distance[target]; !ok {
				distance[target] = depth
				queue = append(queue, queueItem{target, depth})
			}
			continue
		}
		if object.Type != ObjCommit {
			includedTags[oid] = true
			continue
		}
		if heldNonShallow[oid] {
			continue
		}
		commit, err := ParseCommit(object.Data)
		if err != nil {
			continue
		}
		if heldShallow[oid] {
			for _, parent := range commit.Parents {
				if _, ok := distance[parent]; !ok {
					distance[parent] = depth + 1
					queue = append(queue, queueItem{parent, depth + 1})
				}
			}
			continue
		}
		if args.depth != nil && depth >= *args.depth {
			continue
		}
		if !included[oid] {
			included[oid] = true
			includedOrder = append(includedOrder, oid)
		}
		for _, parent := range commit.Parents {
			if _, ok := distance[parent]; !ok {
				distance[parent] = depth + 1
				queue = append(queue, queueItem{parent, depth + 1})
			}
		}
	}

	roots := make([]string, 0, len(included)+len(includedTags))
	roots = append(roots, includedOrder...)
	for oid := range includedTags {
		roots = append(roots, oid)
	}
	closure := collectFetchObjects(repo, roots)
	heldClosure := map[string]bool{}
	for oid := range heldNonShallow {
		heldClosure[oid] = true
	}
	// A shallow-held commit is held with only itself and its own tree closure:
	// its parents are precisely what the client does NOT have yet.
	for oid := range heldShallow {
		for reached := range collectFetchObjects(repo, []string{oid}) {
			heldClosure[reached] = true
		}
	}
	objects := map[string]*GitObject{}
	for oid := range closure {
		if heldClosure[oid] {
			continue
		}
		if object, ok := repo.Objects[oid]; ok {
			objects[oid] = object
		}
	}

	var acks []string
	if len(args.haves) > 0 {
		for _, have := range args.haves {
			if _, ok := repo.Objects[have]; ok {
				acks = append(acks, have)
			}
		}
	}
	var shallow []string
	if args.depth != nil {
		for _, oid := range includedOrder {
			if distance[oid] != *args.depth-1 {
				continue
			}
			object := repo.Objects[oid]
			if object == nil || object.Type != ObjCommit {
				continue
			}
			commit, err := ParseCommit(object.Data)
			if err != nil {
				continue
			}
			if len(commit.Parents) > 0 {
				shallow = append(shallow, oid)
			}
		}
	}
	var unshallow []string
	for _, oid := range args.prevShallow {
		if !heldShallow[oid] {
			continue
		}
		if containsString(shallow, oid) {
			continue
		}
		if containsString(args.wants, oid) || included[oid] {
			unshallow = append(unshallow, oid)
		}
	}
	return fetchSelection{objects: objects, acks: acks, shallow: shallow, unshallow: unshallow}
}

// The objects a fetch delivers for its included commits and tags: a commit
// contributes itself and its whole tree closure, but never its parents (that
// is what a `deepen` limit cuts off); a tag contributes its target the same
// way. The held closure is subtracted afterwards, so a held object is never
// re-sent even when a tag points at it.
func collectFetchObjects(repo *Repo, roots []string) map[string]bool {
	out := map[string]bool{}
	queue := append([]string{}, roots...)
	for len(queue) > 0 {
		oid := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if out[oid] {
			continue
		}
		object := repo.Objects[oid]
		if object == nil {
			continue
		}
		out[oid] = true
		switch object.Type {
		case ObjCommit:
			commit, err := ParseCommit(object.Data)
			if err != nil {
				continue
			}
			queue = append(queue, commit.Tree)
		case ObjTag:
			tag, err := ParseTag(object.Data)
			if err != nil {
				continue
			}
			queue = append(queue, tag.Object)
		case ObjTree:
			for _, entry := range parseTreeSafe(object.Data) {
				if entry.Mode != "160000" {
					queue = append(queue, entry.OID)
				}
			}
		}
	}
	return out
}

// collectReachable that tolerates a missing root (returns what it can reach).
func collectReachableSafe(repo *Repo, root string) map[string]bool {
	out := map[string]bool{}
	queue := []string{root}
	for len(queue) > 0 {
		oid := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if out[oid] {
			continue
		}
		object := repo.Objects[oid]
		if object == nil {
			continue
		}
		out[oid] = true
		switch object.Type {
		case ObjCommit:
			commit, err := ParseCommit(object.Data)
			if err != nil {
				continue
			}
			queue = append(queue, commit.Tree)
			queue = append(queue, commit.Parents...)
		case ObjTag:
			tag, err := ParseTag(object.Data)
			if err != nil {
				continue
			}
			queue = append(queue, tag.Object)
		case ObjTree:
			for _, entry := range parseTreeSafe(object.Data) {
				if entry.Mode != "160000" {
					queue = append(queue, entry.OID)
				}
			}
		}
	}
	return out
}

// A tolerant tree parse: a malformed entry stops the walk instead of erroring,
// mirroring the TypeScript parseTreeSafe.
func parseTreeSafe(data []byte) []TreeEntry {
	var entries []TreeEntry
	offset := 0
	for offset < len(data) {
		space := bytes.IndexByte(data[offset:], 0x20)
		if space < 0 {
			break
		}
		space += offset
		nul := bytes.IndexByte(data[space+1:], 0x00)
		if nul < 0 {
			break
		}
		nul += space + 1
		if nul+21 > len(data) {
			break
		}
		entries = append(entries, TreeEntry{
			Mode: string(data[offset:space]),
			Name: string(data[space+1 : nul]),
			OID:  oidHex(data[nul+1 : nul+21]),
		})
		offset = nul + 21
	}
	return entries
}

func oidHex(raw []byte) string {
	const hexDigits = "0123456789abcdef"
	out := make([]byte, 0, len(raw)*2)
	for _, b := range raw {
		out = append(out, hexDigits[b>>4], hexDigits[b&0x0f])
	}
	return string(out)
}

// Split a pack into sideband band-1 packets (band byte 0x01 + at most 32 KiB of data).
func sidebandPackets(pack []byte) [][]byte {
	var packets [][]byte
	for offset := 0; offset < len(pack); offset += sidebandChunk {
		end := offset + sidebandChunk
		if end > len(pack) {
			end = len(pack)
		}
		chunk := pack[offset:end]
		payload := make([]byte, 0, len(chunk)+1)
		payload = append(payload, 0x01)
		payload = append(payload, chunk...)
		packets = append(packets, PktLine(payload))
	}
	return packets
}

// The pack for an object set, from the repo's cache when the same set was
// packed before. Objects are content-addressed and immutable, so a pack for
// an oid set never goes stale; the key is the sorted oid list. The cache is
// capped at 128 entries (FIFO) so a long trial cannot grow it without bound.
//
// Singleflight: concurrent fetches of the same set share one build. The first
// goroutine builds while the rest wait on the in-flight channel, then all read
// the cached pack. Without this, 50 concurrent clones each pay a full
// pure-Go deflate of the same pack.
func cachedPack(repo *Repo, objects map[string]*GitObject) []byte {
	oids := make([]string, 0, len(objects))
	for oid := range objects {
		oids = append(oids, oid)
	}
	sort.Strings(oids)
	key := strings.Join(oids, ",")
	repo.PackMu.Lock()
	if hit, ok := repo.PackCache[key]; ok {
		repo.PackMu.Unlock()
		return hit
	}
	if ch, building := repo.PackInflight[key]; building {
		repo.PackMu.Unlock()
		<-ch
		repo.PackMu.Lock()
		hit := repo.PackCache[key]
		repo.PackMu.Unlock()
		return hit
	}
	done := make(chan struct{})
	repo.PackInflight[key] = done
	repo.PackMu.Unlock()

	ordered := make([]*GitObject, 0, len(oids))
	for _, oid := range oids {
		ordered = append(ordered, objects[oid])
	}
	pack := WritePack(ordered)
	repo.PackMu.Lock()
	repo.PackCache[key] = pack
	repo.PackCacheOrder = append(repo.PackCacheOrder, key)
	for len(repo.PackCacheOrder) > 128 {
		oldest := repo.PackCacheOrder[0]
		delete(repo.PackCache, oldest)
		repo.PackCacheOrder = repo.PackCacheOrder[1:]
	}
	delete(repo.PackInflight, key)
	close(done)
	repo.PackMu.Unlock()
	return pack
}

func handleFetchV2(store *Store, ctx *Ctx, res Responder, repo *Repo, args fetchArgs) bool {
	selection := selectFetch(store, repo, args)
	var parts [][]byte
	// Each section opens with its name line; delim packets separate the sections.
	parts = append(parts, PktLineString("acknowledgments\n"))
	if len(selection.acks) > 0 {
		for _, ack := range selection.acks {
			parts = append(parts, PktLineString("ACK "+ack+"\n"))
		}
	} else {
		parts = append(parts, PktLineString("NAK\n"))
	}
	parts = append(parts, PktLineString("ready\n"))
	parts = append(parts, PktDelimBytes)
	if len(selection.shallow) > 0 || len(selection.unshallow) > 0 {
		parts = append(parts, PktLineString("shallow-info\n"))
		for _, oid := range selection.shallow {
			parts = append(parts, PktLineString("shallow "+oid+"\n"))
		}
		for _, oid := range selection.unshallow {
			parts = append(parts, PktLineString("unshallow "+oid+"\n"))
		}
		parts = append(parts, PktDelimBytes)
	}
	parts = append(parts, PktLineString("packfile\n"))
	pack := cachedPack(repo, selection.objects)
	parts = append(parts, sidebandPackets(pack)...)
	parts = append(parts, PktFlushBytes)
	SendBuffer(res, 200, "application/x-git-upload-pack-result", bytes.Join(parts, nil), ctx.RequestID)
	return true
}

func handleFetchV0(store *Store, ctx *Ctx, res Responder, repo *Repo, args fetchArgs) bool {
	selection := selectFetch(store, repo, args)
	var parts [][]byte
	if len(selection.acks) > 0 {
		for _, ack := range selection.acks {
			parts = append(parts, PktLineString("ACK "+ack+"\n"))
		}
	} else {
		parts = append(parts, PktLineString("NAK\n"))
	}
	for _, oid := range selection.shallow {
		parts = append(parts, PktLineString("shallow "+oid+"\n"))
	}
	for _, oid := range selection.unshallow {
		parts = append(parts, PktLineString("unshallow "+oid+"\n"))
	}
	pack := cachedPack(repo, selection.objects)
	parts = append(parts, sidebandPackets(pack)...)
	parts = append(parts, PktFlushBytes)
	SendBuffer(res, 200, "application/x-git-upload-pack-result", bytes.Join(parts, nil), ctx.RequestID)
	return true
}

func handleUploadPack(store *Store, ctx *Ctx, res Responder, repo *Repo) bool {
	packets, err := DecodePktLinesAll(ctx.Body)
	if err != nil {
		SendJSON(res, 400, map[string]any{"message": "Malformed pkt-line in request body"}, ctx.RequestID, nil)
		return true
	}
	lines := PktLinesToStrings(packets)
	first := ""
	if len(lines) > 0 {
		first = strings.TrimSuffix(lines[0], "\n")
	}
	if first == "command=ls-refs" {
		args := lines[1:]
		SendBuffer(res, 200, "application/x-git-upload-pack-result", lsRefs(repo, args), ctx.RequestID)
		return true
	}
	if first == "command=fetch" {
		// The arguments live after the delim packet (the trailing flush only ends them).
		delimIndex := -1
		for i, packet := range packets {
			if packet.Kind == PktDelim {
				delimIndex = i
				break
			}
		}
		var argLines []string
		if delimIndex >= 0 {
			argLines = PktLinesToStrings(packets[delimIndex+1:])
		} else {
			argLines = lines[1:]
		}
		args := parseFetchArgs(argLines, "v2")
		return handleFetchV2(store, ctx, res, repo, args)
	}
	if strings.HasPrefix(first, "want ") {
		sections := sectionsOf(packets)
		var firstSection, secondSection []string
		if len(sections) > 0 {
			firstSection = sections[0]
		}
		if len(sections) > 1 {
			secondSection = sections[1]
		}
		combined := append(append([]string{}, firstSection...), secondSection...)
		args := parseFetchArgs(combined, "v0")
		return handleFetchV0(store, ctx, res, repo, args)
	}
	SendJSON(res, 400, map[string]any{"message": "Unknown git-upload-pack command"}, ctx.RequestID, nil)
	return true
}

func sanitizeOneLine(text string) string {
	var b strings.Builder
	prevWasSep := false
	for _, r := range text {
		if r == '\r' || r == '\n' {
			if !prevWasSep {
				b.WriteByte(' ')
			}
			prevWasSep = true
			continue
		}
		prevWasSep = false
		b.WriteRune(r)
	}
	runes := []rune(b.String())
	if len(runes) > 200 {
		runes = runes[:200]
	}
	return string(runes)
}

type refUpdateCommand struct {
	old  string
	next string
	ref  string
}

func handleReceivePack(store *Store, ctx *Ctx, res Responder, repo *Repo) bool {
	first, rest, err := DecodePktLinesUntilFlush(ctx.Body)
	if err != nil {
		SendJSON(res, 400, map[string]any{"message": "Malformed pkt-line in request body"}, ctx.RequestID, nil)
		return true
	}
	lines := PktLinesToStrings(first)
	var commands []refUpdateCommand
	for _, rawLine := range lines {
		text := rawLine
		if nul := strings.Index(text, "\x00"); nul >= 0 {
			text = text[:nul]
		}
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		parts := strings.Split(text, " ")
		if len(parts) != 3 {
			SendJSON(res, 400, map[string]any{"message": "Malformed ref update command"}, ctx.RequestID, nil)
			return true
		}
		commands = append(commands, refUpdateCommand{old: parts[0], next: parts[1], ref: parts[2]})
	}
	if len(commands) == 0 {
		SendJSON(res, 400, map[string]any{"message": "No ref update commands"}, ctx.RequestID, nil)
		return true
	}

	unpackError := ""
	if len(rest) > 0 {
		imported, err := ReadPack(rest, func(oid string) *GitObject { return repo.Objects[oid] })
		if err != nil {
			unpackError = sanitizeOneLine(err.Error())
		} else {
			for oid, object := range imported {
				if _, ok := repo.Objects[oid]; !ok {
					repo.Objects[oid] = object
				}
			}
		}
	} else {
		hasNonDelete := false
		for _, command := range commands {
			if command.next != ZeroOID {
				hasNonDelete = true
				break
			}
		}
		if hasNonDelete {
			unpackError = "pack is missing"
		}
	}

	preRefs := make(map[string]string, len(repo.Refs))
	for ref, oid := range repo.Refs {
		preRefs[ref] = oid
	}
	type cmdResult struct {
		ref    string
		ok     bool
		reason string
	}
	results := make([]cmdResult, 0, len(commands))
	for _, command := range commands {
		if unpackError != "" {
			results = append(results, cmdResult{ref: command.ref, ok: false, reason: "unpack failed"})
			continue
		}
		if !IsObjectID(command.old) || !IsObjectID(command.next) {
			results = append(results, cmdResult{ref: command.ref, ok: false, reason: "invalid object id"})
			continue
		}
		current, exists := preRefs[command.ref]
		if command.old == ZeroOID {
			if exists {
				results = append(results, cmdResult{ref: command.ref, ok: false, reason: "already exists"})
			} else if command.next == ZeroOID {
				results = append(results, cmdResult{ref: command.ref, ok: false, reason: "invalid ref update"})
			} else if _, ok := repo.Objects[command.next]; !ok {
				results = append(results, cmdResult{ref: command.ref, ok: false, reason: "missing object"})
			} else {
				results = append(results, cmdResult{ref: command.ref, ok: true})
			}
		} else if !exists {
			results = append(results, cmdResult{ref: command.ref, ok: false, reason: "reference does not exist"})
		} else if current != command.old {
			results = append(results, cmdResult{ref: command.ref, ok: false, reason: "non-fast-forward"})
		} else if command.next == ZeroOID {
			results = append(results, cmdResult{ref: command.ref, ok: true})
		} else if command.next == command.old {
			results = append(results, cmdResult{ref: command.ref, ok: true})
		} else if _, ok := repo.Objects[command.next]; !ok {
			results = append(results, cmdResult{ref: command.ref, ok: false, reason: "missing object"})
		} else if !IsDescendant(repo, command.next, command.old) {
			results = append(results, cmdResult{ref: command.ref, ok: false, reason: "non-fast-forward"})
		} else {
			results = append(results, cmdResult{ref: command.ref, ok: true})
		}
	}
	for i, command := range commands {
		if !results[i].ok {
			continue
		}
		if command.next == ZeroOID {
			delete(repo.Refs, command.ref)
		} else {
			repo.Refs[command.ref] = command.next
		}
	}

	reportLines := []string{}
	if unpackError != "" {
		reportLines = append(reportLines, "unpack "+unpackError+"\n")
	} else {
		reportLines = append(reportLines, "unpack ok\n")
	}
	for _, result := range results {
		if result.ok {
			reportLines = append(reportLines, "ok "+result.ref+"\n")
		} else {
			reportLines = append(reportLines, "ng "+result.ref+" "+sanitizeOneLine(result.reason)+"\n")
		}
	}
	var report []byte
	for _, line := range reportLines {
		report = append(report, PktLineString(line)...)
	}
	var parts [][]byte
	parts = append(parts, sidebandPackets(report)...)
	parts = append(parts, PktFlushBytes)
	SendBuffer(res, 200, "application/x-git-receive-pack-result", bytes.Join(parts, nil), ctx.RequestID)
	return true
}

// Handle a request under `/{owner}/{repo}.git/...`. Returns true when the path
// was a git endpoint (handled, even on error); false when it is not one.
func HandleGit(store *Store, ctx *Ctx, res Responder, owner, repoName, subpath string) bool {
	if ctx.Method == "GET" && subpath == "/info/refs" {
		return handleInfoRefs(store, ctx, res, owner, repoName)
	}
	if ctx.Method == "POST" && subpath == "/git-upload-pack" {
		repo := store.GetRepo(owner, repoName)
		if repo == nil {
			SendJSON(res, 404, map[string]any{"message": "Not Found"}, ctx.RequestID, nil)
			return true
		}
		return handleUploadPack(store, ctx, res, repo)
	}
	if ctx.Method == "POST" && subpath == "/git-receive-pack" {
		repo := store.GetRepo(owner, repoName)
		if repo == nil {
			SendJSON(res, 404, map[string]any{"message": "Not Found"}, ctx.RequestID, nil)
			return true
		}
		return handleReceivePack(store, ctx, res, repo)
	}
	return false
}
