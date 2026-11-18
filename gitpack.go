// gitpack.go — the git object model, pkt-line codec and packfile read/write.
//
// A faithful port of the TypeScript gitpack.ts. Only the Go standard library.
// No wall clock, no randomness: every byte this module produces is a pure
// function of its inputs, so two runs over the same store are byte-identical.

package main

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
)

const ZeroOID = "0000000000000000000000000000000000000000"
const PktLineMax = 65520

type GitObjectType string

const (
	ObjCommit GitObjectType = "commit"
	ObjTree   GitObjectType = "tree"
	ObjBlob   GitObjectType = "blob"
	ObjTag    GitObjectType = "tag"
)

type GitObject struct {
	Type GitObjectType
	Data []byte
}

type ObjectLookup func(oid string) *GitObject

var typeCode = map[GitObjectType]int{"commit": 1, "tree": 2, "blob": 3, "tag": 4}
var codeType = map[int]GitObjectType{1: "commit", 2: "tree", 3: "blob", 4: "tag"}

var oidRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

func IsObjectID(value string) bool { return oidRe.MatchString(value) }

// git's object id: SHA-1 over `<type> <size>\0` + the object's bytes.
func HashObject(typ GitObjectType, data []byte) string {
	h := sha1.New()
	h.Write([]byte(fmt.Sprintf("%s %d\x00", typ, len(data))))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// ---------------------------------------------------------------------------------------------
// trees, commits, tags
// ---------------------------------------------------------------------------------------------

type TreeEntry struct {
	Mode string
	Name string
	OID  string
}

// Parse a tree object into its entries, in stored order.
func ParseTree(data []byte) ([]TreeEntry, error) {
	var entries []TreeEntry
	offset := 0
	for offset < len(data) {
		space := bytes.IndexByte(data[offset:], 0x20)
		if space < 0 {
			return nil, errors.New("tree entry has no mode separator")
		}
		space += offset
		nul := bytes.IndexByte(data[space+1:], 0x00)
		if nul < 0 {
			return nil, errors.New("tree entry has no name terminator")
		}
		nul += space + 1
		if nul+21 > len(data) {
			return nil, errors.New("tree entry has a truncated object id")
		}
		entries = append(entries, TreeEntry{
			Mode: string(data[offset:space]),
			Name: string(data[space+1 : nul]),
			OID:  hex.EncodeToString(data[nul+1 : nul+21]),
		})
		offset = nul + 21
	}
	return entries, nil
}

// git's tree-entry sort: byte order of the name, a directory compared as if it ended in '/'.
func CompareTreeNames(a, b TreeEntry) int {
	an := a.Name
	if a.Mode == "40000" {
		an = a.Name + "/"
	}
	bn := b.Name
	if b.Mode == "40000" {
		bn = b.Name + "/"
	}
	return bytes.Compare([]byte(an), []byte(bn))
}

// Build a tree object's bytes from entries; entries are sorted the way git sorts them.
func BuildTree(entries []TreeEntry) []byte {
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.SliceStable(sorted, func(i, j int) bool { return CompareTreeNames(sorted[i], sorted[j]) < 0 })
	var buf bytes.Buffer
	for _, e := range sorted {
		buf.WriteString(e.Mode)
		buf.WriteByte(' ')
		buf.WriteString(e.Name)
		buf.WriteByte(0)
		raw, _ := hex.DecodeString(e.OID)
		buf.Write(raw)
	}
	return buf.Bytes()
}

type CommitView struct {
	Tree      string
	Parents   []string
	Author    string
	Committer string
	Message   string
}

// Parse a commit object.
func ParseCommit(data []byte) (CommitView, error) {
	var out CommitView
	blank := bytes.Index(data, []byte("\n\n"))
	headerEnd := len(data)
	if blank >= 0 {
		headerEnd = blank
	}
	headerText := string(data[:headerEnd])
	if blank >= 0 {
		out.Message = string(data[blank+2:])
	}
	for _, line := range splitLines(headerText) {
		if line == "" {
			continue
		}
		space := indexByte(line, ' ')
		key := line
		value := ""
		if space >= 0 {
			key = line[:space]
			value = line[space+1:]
		}
		switch key {
		case "tree":
			out.Tree = value
		case "parent":
			out.Parents = append(out.Parents, value)
		case "author":
			out.Author = value
		case "committer":
			out.Committer = value
		}
	}
	if !IsObjectID(out.Tree) {
		return out, errors.New("commit has no valid tree header")
	}
	return out, nil
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	out = append(out, s[start:])
	return out
}

func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

// Build a commit object's bytes. `message` is stored verbatim (callers add the trailing newline).
func BuildCommit(tree string, parents []string, author, committer string, timestamp int64, message string) []byte {
	var buf bytes.Buffer
	buf.WriteString("tree " + tree + "\n")
	for _, p := range parents {
		buf.WriteString("parent " + p + "\n")
	}
	buf.WriteString(fmt.Sprintf("author %s %d +0000\n", author, timestamp))
	buf.WriteString(fmt.Sprintf("committer %s %d +0000\n", committer, timestamp))
	buf.WriteString("\n")
	buf.WriteString(message)
	return buf.Bytes()
}

type TagView struct {
	Object  string
	Type    GitObjectType
	Tag     string
	Tagger  string
	Message string
}

// Parse an annotated tag object.
func ParseTag(data []byte) (TagView, error) {
	var out TagView
	blank := bytes.Index(data, []byte("\n\n"))
	headerEnd := len(data)
	if blank >= 0 {
		headerEnd = blank
	}
	headers := map[string]string{}
	for _, line := range splitLines(string(data[:headerEnd])) {
		if line == "" {
			continue
		}
		space := indexByte(line, ' ')
		if space < 0 {
			return out, errors.New("tag header has no value")
		}
		headers[line[:space]] = line[space+1:]
	}
	out.Object = headers["object"]
	typ := headers["type"]
	if !IsObjectID(out.Object) {
		return out, errors.New("tag has no valid object header")
	}
	if typ != "commit" && typ != "tree" && typ != "blob" && typ != "tag" {
		return out, fmt.Errorf("tag has bad type %q", typ)
	}
	out.Type = GitObjectType(typ)
	out.Tag = headers["tag"]
	out.Tagger = headers["tagger"]
	if blank >= 0 {
		out.Message = string(data[blank+2:])
	}
	return out, nil
}

// Build an annotated tag object's bytes.
func BuildTag(object string, typ GitObjectType, tag, tagger string, timestamp int64, message string) []byte {
	return []byte(fmt.Sprintf("object %s\ntype %s\ntag %s\ntagger %s %d +0000\n\n%s", object, typ, tag, tagger, timestamp, message))
}

// Every object reachable from `roots`: commit -> tree + parents, tree -> entries
// (gitlinks skipped), tag -> its target, blob -> nothing. Errors on a missing object.
func CollectReachable(roots []string, lookup ObjectLookup) (map[string]bool, error) {
	seen := map[string]bool{}
	var queue []string
	for _, r := range roots {
		if IsObjectID(r) {
			queue = append(queue, r)
		}
	}
	for len(queue) > 0 {
		oid := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if seen[oid] {
			continue
		}
		obj := lookup(oid)
		if obj == nil {
			return nil, fmt.Errorf("object %s is missing from the store", oid)
		}
		seen[oid] = true
		switch obj.Type {
		case ObjCommit:
			c, err := ParseCommit(obj.Data)
			if err != nil {
				return nil, err
			}
			queue = append(queue, c.Tree)
			queue = append(queue, c.Parents...)
		case ObjTag:
			t, err := ParseTag(obj.Data)
			if err != nil {
				return nil, err
			}
			queue = append(queue, t.Object)
		case ObjTree:
			entries, err := ParseTree(obj.Data)
			if err != nil {
				return nil, err
			}
			for _, e := range entries {
				if e.Mode == "160000" {
					continue
				}
				queue = append(queue, e.OID)
			}
		}
	}
	return seen, nil
}

// A tree flattened to path -> {oid, mode}, recursively.
func FlattenTree(lookup ObjectLookup, treeOID string) (map[string]TreeEntry, error) {
	out := map[string]TreeEntry{}
	var walk func(oid, prefix string) error
	walk = func(oid, prefix string) error {
		obj := lookup(oid)
		if obj == nil || obj.Type != ObjTree {
			return fmt.Errorf("tree %s is missing from the store", oid)
		}
		entries, err := ParseTree(obj.Data)
		if err != nil {
			return err
		}
		for _, e := range entries {
			path := e.Name
			if prefix != "" {
				path = prefix + "/" + e.Name
			}
			if e.Mode == "40000" {
				if err := walk(e.OID, path); err != nil {
					return err
				}
			} else {
				out[path] = e
			}
		}
		return nil
	}
	if err := walk(treeOID, ""); err != nil {
		return nil, err
	}
	return out, nil
}

// ---------------------------------------------------------------------------------------------
// pkt-line
// ---------------------------------------------------------------------------------------------

type PktKind int

const (
	PktData PktKind = iota
	PktFlush
	PktDelim
	PktResponseEnd
)

type Pkt struct {
	Kind    PktKind
	Payload []byte
}

// Encode one data packet: 4 hex length bytes (payload + 4) then the payload.
func PktLine(payload []byte) []byte {
	if len(payload) > PktLineMax-4 {
		panic(fmt.Sprintf("pkt-line payload is %d bytes, over the maximum", len(payload)))
	}
	head := []byte(fmt.Sprintf("%04x", len(payload)+4))
	out := make([]byte, 0, len(payload)+4)
	out = append(out, head...)
	out = append(out, payload...)
	return out
}

func PktLineString(s string) []byte { return PktLine([]byte(s)) }

var PktFlushBytes = []byte("0000")
var PktDelimBytes = []byte("0001")

var pktHeadRe = regexp.MustCompile(`^[0-9a-fA-F]{4}$`)

// Decode pkt-lines from the front of `buf`. Stops at the first flush packet (consumed)
// and returns the packets before it plus the remaining bytes untouched (a pack body).
// Errors on a malformed header. A packet stream that simply ends returns what it has.
func DecodePktLinesUntilFlush(buf []byte) ([]Pkt, []byte, error) {
	var packets []Pkt
	offset := 0
	for offset+4 <= len(buf) {
		head := string(buf[offset : offset+4])
		if !pktHeadRe.MatchString(head) {
			return nil, nil, fmt.Errorf("invalid pkt-line length %q", head)
		}
		length := hexToInt(head)
		if length == 0 {
			offset += 4
			return packets, buf[offset:], nil
		}
		if length == 1 {
			packets = append(packets, Pkt{Kind: PktDelim})
			offset += 4
			continue
		}
		if length == 2 {
			packets = append(packets, Pkt{Kind: PktResponseEnd})
			offset += 4
			continue
		}
		if length == 3 || length > PktLineMax {
			return nil, nil, fmt.Errorf("invalid pkt-line length %d", length)
		}
		if offset+length > len(buf) {
			return nil, nil, errors.New("truncated pkt-line")
		}
		packets = append(packets, Pkt{Kind: PktData, Payload: buf[offset+4 : offset+length]})
		offset += length
	}
	if offset < len(buf) {
		return nil, nil, errors.New("truncated pkt-line")
	}
	return packets, []byte{}, nil
}

// Decode a whole buffer of pkt-lines (no trailing raw bytes allowed).
func DecodePktLinesAll(buf []byte) ([]Pkt, error) {
	var packets []Pkt
	offset := 0
	for offset+4 <= len(buf) {
		head := string(buf[offset : offset+4])
		if !pktHeadRe.MatchString(head) {
			return nil, fmt.Errorf("invalid pkt-line length %q", head)
		}
		length := hexToInt(head)
		if length == 0 {
			packets = append(packets, Pkt{Kind: PktFlush})
			offset += 4
			continue
		}
		if length == 1 {
			packets = append(packets, Pkt{Kind: PktDelim})
			offset += 4
			continue
		}
		if length == 2 {
			packets = append(packets, Pkt{Kind: PktResponseEnd})
			offset += 4
			continue
		}
		if length == 3 || length > PktLineMax {
			return nil, fmt.Errorf("invalid pkt-line length %d", length)
		}
		if offset+length > len(buf) {
			return nil, errors.New("truncated pkt-line")
		}
		packets = append(packets, Pkt{Kind: PktData, Payload: buf[offset+4 : offset+length]})
		offset += length
	}
	if offset < len(buf) {
		return nil, errors.New("truncated pkt-line")
	}
	return packets, nil
}

func hexToInt(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		var v int
		switch {
		case c >= '0' && c <= '9':
			v = int(c - '0')
		case c >= 'a' && c <= 'f':
			v = int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			v = int(c-'A') + 10
		}
		n = n*16 + v
	}
	return n
}

// The text of every data packet, control packets dropped.
func PktLinesToStrings(packets []Pkt) []string {
	var lines []string
	for _, p := range packets {
		if p.Kind == PktData && p.Payload != nil {
			lines = append(lines, string(p.Payload))
		}
	}
	return lines
}

// ---------------------------------------------------------------------------------------------
// packfile
// ---------------------------------------------------------------------------------------------

func writePackObjectHeader(typ GitObjectType, size int) []byte {
	code := typeCode[typ]
	var out []byte
	remaining := size
	first := (code << 4) | (remaining & 0x0f)
	remaining = remaining / 16
	for remaining > 0 {
		out = append(out, byte(first|0x80))
		first = remaining & 0x7f
		remaining = remaining / 128
	}
	out = append(out, byte(first))
	return out
}

// Build a packfile from plain objects (no deltas). Trailer is SHA-1 over everything before it.
func WritePack(objects []*GitObject) []byte {
	var parts [][]byte
	header := make([]byte, 12)
	copy(header[0:4], "PACK")
	putU32BE(header[4:8], 2)
	putU32BE(header[8:12], uint32(len(objects)))
	parts = append(parts, header)
	for _, obj := range objects {
		parts = append(parts, writePackObjectHeader(obj.Type, len(obj.Data)))
		// Level 1: pack bytes are not scored, wall time is; small objects compress
		// almost as well at level 1 and far faster than the default level 6.
		parts = append(parts, deflateLevel1(obj.Data))
	}
	var body bytes.Buffer
	for _, p := range parts {
		body.Write(p)
	}
	sum := sha1.Sum(body.Bytes())
	out := body.Bytes()
	out = append(out, sum[:]...)
	return out
}

func putU32BE(b []byte, v uint32) {
	b[0] = byte(v >> 24)
	b[1] = byte(v >> 16)
	b[2] = byte(v >> 8)
	b[3] = byte(v)
}

func deflateLevel1(data []byte) []byte {
	var buf bytes.Buffer
	// zlib (RFC 1950), NOT raw flate: a git pack object is a zlib stream
	// (2-byte header + deflate data + Adler-32). flate alone produces a raw
	// deflate stream with no header, which zlib.NewReader rejects.
	w, _ := zlib.NewWriterLevel(&buf, zlib.BestSpeed)
	w.Write(data)
	w.Close()
	return buf.Bytes()
}

// SHA-1 of every pack byte before the 20-byte trailer.
func PackChecksum(pack []byte) string {
	sum := sha1.Sum(pack[:len(pack)-20])
	return hex.EncodeToString(sum[:])
}

// True when the pack's trailer matches its bytes.
func VerifyPackChecksum(pack []byte) bool {
	if len(pack) < 32 {
		return false
	}
	return hex.EncodeToString(pack[len(pack)-20:]) == PackChecksum(pack)
}

func readVarintSize(buf []byte, start int) (size int, next int, first int) {
	b := int(buf[start])
	first = b
	size = b & 0x0f
	shift := uint(4)
	offset := start + 1
	for b&0x80 != 0 {
		b = int(buf[offset])
		offset++
		size += (b & 0x7f) << shift
		shift += 7
	}
	return size, offset, first
}

func readOfsDeltaBase(buf []byte, start int) (baseOffset int, next int) {
	b := int(buf[start])
	offset := start + 1
	value := b & 0x7f
	for b&0x80 != 0 {
		b = int(buf[offset])
		offset++
		value = (value+1)*128 + (b & 0x7f)
	}
	return value, offset
}

// countedReader tracks how many bytes were read from the underlying stream.
type countedReader struct {
	r io.Reader
	n int
}

func (c *countedReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// Inflate one zlib stream at `offset`, reporting the offset just past the stream.
//
// The zlib/flate reader buffers ahead: it reads from the underlying reader in
// chunks, so a plain byte counter overcounts. We therefore read through a
// bufio.Reader and subtract what it still holds (br.Buffered()) from the
// bytes the underlying reader supplied (cr.n). The difference is exactly the
// bytes the zlib stream consumed. (A naive countedReader alone overcounts and
// corrupts every object after the first in a pack.)
func inflateAtCounted(buf []byte, offset int) ([]byte, int, error) {
	cr := &countedReader{r: bytes.NewReader(buf[offset:])}
	br := bufio.NewReader(cr)
	zr, err := zlib.NewReader(br)
	if err != nil {
		return nil, 0, err
	}
	data, err := io.ReadAll(zr)
	if err != nil {
		return nil, 0, err
	}
	zr.Close()
	consumed := cr.n - br.Buffered()
	if consumed <= 0 {
		return nil, 0, fmt.Errorf("zlib stream at offset %d consumed no bytes", offset)
	}
	return data, offset + consumed, nil
}

// Apply a git delta (copy/insert instruction stream) to a base object's bytes.
func ApplyDelta(base, delta []byte) ([]byte, error) {
	offset := 0
	readSize := func() int {
		value := 0
		shift := uint(0)
		for {
			b := int(delta[offset])
			offset++
			value += (b & 0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
		}
		return value
	}
	sourceSize := readSize()
	targetSize := readSize()
	if sourceSize != len(base) {
		return nil, fmt.Errorf("delta expects a %d-byte base, got %d", sourceSize, len(base))
	}
	out := make([]byte, targetSize)
	written := 0
	for offset < len(delta) {
		op := int(delta[offset])
		offset++
		if op&0x80 != 0 {
			copyOffset := 0
			copySize := 0
			shift := uint(0)
			for bit := 0; bit < 4; bit++ {
				if op&(1<<bit) != 0 {
					copyOffset += int(delta[offset]) << shift
					offset++
				}
				shift += 8
			}
			shift = 0
			for bit := 4; bit < 7; bit++ {
				if op&(1<<bit) != 0 {
					copySize += int(delta[offset]) << shift
					offset++
				}
				shift += 8
			}
			if copySize == 0 {
				copySize = 0x10000
			}
			copy(out[written:written+copySize], base[copyOffset:copyOffset+copySize])
			written += copySize
		} else if op != 0 {
			literal := op & 0x7f
			copy(out[written:written+literal], delta[offset:offset+literal])
			offset += literal
			written += literal
		} else {
			return nil, errors.New("delta opcode 0 is invalid")
		}
	}
	if written != targetSize {
		return nil, fmt.Errorf("delta produced %d of %d bytes", written, targetSize)
	}
	return out, nil
}

type rawPackEntry struct {
	offset    int
	typeCode  int
	size      int
	data      []byte
	baseOff   int
	baseOID   string
	resolved  *resolvedPackEntry
	resolving bool
}

type resolvedPackEntry struct {
	typ  GitObjectType
	data []byte
	oid  string
}

// Read a packfile (version 2) and resolve every object, including OFS_DELTA and
// REF_DELTA (resolved recursively; `resolveBase` supplies a base a thin pack leaves out).
// Every object's SHA-1 is recomputed; a mismatch is an error.
func ReadPack(pack []byte, resolveBase ObjectLookup) (map[string]*GitObject, error) {
	if len(pack) < 32 {
		return nil, fmt.Errorf("pack is %d bytes, too short", len(pack))
	}
	if string(pack[0:4]) != "PACK" {
		return nil, errors.New("pack does not start with PACK")
	}
	version := u32BE(pack[4:8])
	if version != 2 && version != 3 {
		return nil, fmt.Errorf("unsupported pack version %d", version)
	}
	declaredCount := int(u32BE(pack[8:12]))
	trailerStart := len(pack) - 20
	if !VerifyPackChecksum(pack) {
		return nil, errors.New("pack trailer does not match its bytes")
	}
	var raw []*rawPackEntry
	byOffset := map[int]*rawPackEntry{}
	offset := 12
	for i := 0; i < declaredCount; i++ {
		entryOffset := offset
		size, next, first := readVarintSize(pack, offset)
		offset = next
		typeCode := (first >> 4) & 0x07
		entry := &rawPackEntry{offset: entryOffset, typeCode: typeCode, size: size}
		if typeCode == 6 {
			base, next2 := readOfsDeltaBase(pack, offset)
			offset = next2
			entry.baseOff = entryOffset - base
		} else if typeCode == 7 {
			entry.baseOID = hex.EncodeToString(pack[offset : offset+20])
			offset += 20
		} else if codeType[typeCode] == "" {
			return nil, fmt.Errorf("pack entry %d has unknown type code %d", i, typeCode)
		}
		inflated, next3, err := inflateAtCounted(pack[:trailerStart], offset)
		if err != nil {
			return nil, err
		}
		if len(inflated) != size {
			return nil, fmt.Errorf("pack entry at %d declares %d bytes but inflated to %d", entryOffset, size, len(inflated))
		}
		entry.data = inflated
		offset = next3
		raw = append(raw, entry)
		byOffset[entryOffset] = entry
	}
	if offset != trailerStart {
		return nil, fmt.Errorf("pack has %d stray bytes before its trailer", trailerStart-offset)
	}

	objects := map[string]*GitObject{}
	resolvedByOID := map[string]*rawPackEntry{}
	var resolve func(entry *rawPackEntry) (*resolvedPackEntry, error)
	resolve = func(entry *rawPackEntry) (*resolvedPackEntry, error) {
		if entry.resolved != nil {
			return entry.resolved, nil
		}
		if entry.resolving {
			return nil, fmt.Errorf("delta cycle at pack offset %d", entry.offset)
		}
		entry.resolving = true
		var typ GitObjectType
		var data []byte
		if entry.typeCode == 6 {
			baseEntry := byOffset[entry.baseOff]
			if baseEntry == nil {
				return nil, fmt.Errorf("ofs-delta base at offset %d is not in the pack", entry.baseOff)
			}
			base, err := resolve(baseEntry)
			if err != nil {
				return nil, err
			}
			typ = base.typ
			data, err = ApplyDelta(base.data, entry.data)
			if err != nil {
				return nil, err
			}
		} else if entry.typeCode == 7 {
			wanted := entry.baseOID
			if baseEntry, ok := resolvedByOID[wanted]; ok {
				base, err := resolve(baseEntry)
				if err != nil {
					return nil, err
				}
				typ = base.typ
				data, err = ApplyDelta(base.data, entry.data)
				if err != nil {
					return nil, err
				}
			} else {
				external := resolveBase(wanted)
				if external == nil {
					return nil, fmt.Errorf("ref-delta base %s is neither in the pack nor supplied", wanted)
				}
				typ = external.Type
				var err error
				data, err = ApplyDelta(external.Data, entry.data)
				if err != nil {
					return nil, err
				}
			}
		} else {
			typ = codeType[entry.typeCode]
			data = entry.data
		}
		oid := HashObject(typ, data)
		out := &resolvedPackEntry{typ: typ, data: data, oid: oid}
		entry.resolved = out
		resolvedByOID[oid] = entry
		entry.resolving = false
		return out, nil
	}
	for _, entry := range raw {
		resolved, err := resolve(entry)
		if err != nil {
			return nil, err
		}
		if _, ok := objects[resolved.oid]; !ok {
			objects[resolved.oid] = &GitObject{Type: resolved.typ, Data: resolved.data}
		}
	}
	return objects, nil
}

func u32BE(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}
