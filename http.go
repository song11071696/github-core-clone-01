// http.go — the request context every handler shares, plus the response helpers.
//
// A faithful port of the TypeScript http.ts. No wall clock, no randomness:
// request ids are a per-process counter, so two runs over the same store
// produce identical bytes.
//
// JSON encoding uses encoding/json with HTML escaping disabled, matching
// JSON.stringify's output (Go's default escapes <, > and &; the TS side does
// not). Response objects are encoded from structs so field order is fixed.

package main

import (
	"bytes"
	"encoding/json"
	"net/url"
	"strings"
)

// The minimal response sink the handlers write to: the raw TCP server's
// responder. Only the three members the handlers use.
type Responder interface {
	HeadersSent() bool
	WriteHead(status int, headers map[string]string)
	End(body []byte)
}

type Ctx struct {
	Method string
	Path   string
	// Decoded path segments (no leading empty segment, no trailing empty segment).
	Segments []string
	Query    url.Values
	// Lowercase header names to their (first) value; the raw server sends one value per name.
	Headers map[string]string
	// Raw request body (empty when the client sent none).
	Body []byte
	// The Host header, for building absolute URLs in Link headers.
	Host string
	// The request id stamped on every response (req-<counter>).
	RequestID string
}

func (c *Ctx) QueryGet(key string) string { return c.Query.Get(key) }

func NewRequestID(store *Store) string {
	return "req-" + itoa64(store.NextRequestID())
}

func itoa64(n int64) string { return itoa(int(n)) }

// Marshal a value to JSON the way JSON.stringify would (no HTML escaping).
func MarshalJSON(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
	// Encoder adds a trailing newline; strip it.
	b := buf.Bytes()
	if len(b) > 0 && b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
	}
	return b
}

func SendJSON(res Responder, status int, body any, requestID string, extra map[string]string) {
	text := MarshalJSON(body)
	headers := map[string]string{
		"Content-Type":        "application/json",
		"Content-Length":      itoa(len(text)),
		"X-GitHub-Request-Id": requestID,
	}
	for k, v := range extra {
		headers[k] = v
	}
	res.WriteHead(status, headers)
	res.End(text)
}

func SendBuffer(res Responder, status int, contentType string, body []byte, requestID string) {
	res.WriteHead(status, map[string]string{
		"Content-Type":        contentType,
		"Content-Length":      itoa(len(body)),
		"X-GitHub-Request-Id": requestID,
	})
	res.End(body)
}

func SendEmpty(res Responder, status int, requestID string) {
	res.WriteHead(status, map[string]string{"X-GitHub-Request-Id": requestID})
	res.End(nil)
}

type jsonParseResult struct {
	OK    bool
	Value any
	Err   string
}

// Parse a JSON body. Empty bodies are `{}` for convenience; malformed JSON is an error.
func ParseJSONBody(body []byte) jsonParseResult {
	text := strings.TrimSpace(string(body))
	if text == "" {
		return jsonParseResult{OK: true, Value: map[string]any{}}
	}
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return jsonParseResult{OK: false, Err: "Malformed JSON in request body"}
	}
	return jsonParseResult{OK: true, Value: v}
}

func IsRecord(v any) bool {
	_, ok := v.(map[string]any)
	return ok
}

func AsString(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

func AsBool(v any) (bool, bool) {
	b, ok := v.(bool)
	return b, ok
}

func AsNumber(v any) (float64, bool) {
	f, ok := v.(float64)
	return f, ok
}

func AsStringArray(v any) ([]string, bool) {
	arr, ok := v.([]any)
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

// JSON string escaping, matching JSON.stringify for the characters that matter.
func JSONEscape(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			if c < 0x20 {
				const hex = "0123456789abcdef"
				b.WriteString(`\u00`)
				b.WriteByte(hex[c>>4])
				b.WriteByte(hex[c&0xf])
			} else {
				b.WriteByte(c)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}
