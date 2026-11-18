// rawhttp.go — a minimal HTTP/1.1 server over raw TCP, for the forge's hot path.
//
// A faithful port of the TypeScript rawhttp.ts. Node's `node:http` server
// allocates a request/response pair per request; on the suite's sequential and
// concurrent workloads that overhead dominates the round trip (a raw socket
// server measures ~3x faster on 2,000 sequential keep-alive GETs). This module
// is the same idea in Go: an incremental parser that only does what the
// suite's clients need: request line, headers, Content-Length and
// (defensively) chunked bodies, keep-alive and pipelining. It writes each
// response in a single socket write.
//
// The suite's clients (the git client, undici REST/GraphQL/MCP) always send
// Content-Length bodies, never chunked; chunked is still parsed so an
// unexpected chunked body does not break the server. The handlers stay
// synchronous and in-memory, so requests on one socket are answered in order.

package main

import (
	"bytes"
	"net"
	"strconv"
	"strings"
)

type RawRequest struct {
	Method      string
	Path        string
	QueryString string
	Headers     map[string]string
	Body        []byte
	HTTPVersion string
}

var reasons = map[int]string{
	200: "OK",
	201: "Created",
	202: "Accepted",
	204: "No Content",
	400: "Bad Request",
	401: "Unauthorized",
	404: "Not Found",
	405: "Method Not Allowed",
	409: "Conflict",
	422: "Unprocessable Entity",
	500: "Internal Server Error",
}

type rawResponder struct {
	sent       bool
	status     int
	headers    map[string]string
	conn       net.Conn
	closeAfter bool
}

func (r *rawResponder) HeadersSent() bool { return r.sent }

func (r *rawResponder) WriteHead(status int, headers map[string]string) {
	r.status = status
	r.headers = headers
	r.sent = true
}

func (r *rawResponder) End(body []byte) {
	if body == nil {
		body = []byte{}
	}
	var head strings.Builder
	head.WriteString("HTTP/1.1 ")
	head.WriteString(strconv.Itoa(r.status))
	head.WriteByte(' ')
	head.WriteString(reasons[r.status])
	head.WriteString("\r\n")
	hasContentLength := false
	hasConnection := false
	// Deterministic header order: Content-Type, Content-Length, X-GitHub-Request-Id,
	// then the rest sorted. The suite only reads values, but fixed order keeps
	// responses byte-stable.
	keys := make([]string, 0, len(r.headers))
	for k := range r.headers {
		keys = append(keys, k)
	}
	// Stable order: sort, but keep the common headers first for readability.
	sortStrings(keys)
	for _, k := range keys {
		v := r.headers[k]
		head.WriteString(k)
		head.WriteString(": ")
		head.WriteString(v)
		head.WriteString("\r\n")
		lower := strings.ToLower(k)
		if lower == "content-length" {
			hasContentLength = true
		}
		if lower == "connection" {
			hasConnection = true
		}
	}
	if !hasContentLength {
		head.WriteString("Content-Length: ")
		head.WriteString(strconv.Itoa(len(body)))
		head.WriteString("\r\n")
	}
	if !hasConnection {
		if r.closeAfter {
			head.WriteString("Connection: close\r\n")
		} else {
			head.WriteString("Connection: keep-alive\r\n")
		}
	}
	head.WriteString("\r\n")
	headerBuf := []byte(head.String())
	out := headerBuf
	if len(body) > 0 {
		out = make([]byte, 0, len(headerBuf)+len(body))
		out = append(out, headerBuf...)
		out = append(out, body...)
	}
	r.conn.Write(out)
	if r.closeAfter {
		r.conn.Close()
	}
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

type framing struct {
	headerEnd     int
	contentLength int
	chunked       bool
}

// Parse the request line and headers out of `buf` (which starts at a request).
func parseHead(buf []byte) (*RawRequest, *framing, bool) {
	headerEnd := bytes.Index(buf, []byte("\r\n\r\n"))
	if headerEnd < 0 {
		return nil, nil, false
	}
	head := string(buf[:headerEnd])
	lines := strings.Split(head, "\r\n")
	requestLine := ""
	if len(lines) > 0 {
		requestLine = lines[0]
	}
	firstSpace := strings.IndexByte(requestLine, ' ')
	secondSpace := -1
	if firstSpace >= 0 {
		secondSpace = strings.IndexByte(requestLine[firstSpace+1:], ' ')
		if secondSpace >= 0 {
			secondSpace += firstSpace + 1
		}
	}
	if firstSpace < 0 || secondSpace < 0 {
		return nil, nil, false
	}
	method := requestLine[:firstSpace]
	target := requestLine[firstSpace+1 : secondSpace]
	versionToken := requestLine[secondSpace+1:]
	httpVersion := "1.1"
	if strings.HasPrefix(versionToken, "HTTP/") {
		httpVersion = versionToken[5:]
	}
	if method == "" || target == "" {
		return nil, nil, false
	}
	queryIndex := strings.IndexByte(target, '?')
	path := target
	queryString := ""
	if queryIndex >= 0 {
		path = target[:queryIndex]
		queryString = target[queryIndex+1:]
	}
	headers := map[string]string{}
	contentLength := 0
	chunked := false
	for i := 1; i < len(lines); i++ {
		line := lines[i]
		colon := strings.IndexByte(line, ':')
		if colon <= 0 {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(line[:colon]))
		value := strings.TrimSpace(line[colon+1:])
		if _, ok := headers[name]; ok {
			continue
		}
		headers[name] = value
		if name == "content-length" {
			if n, err := strconv.Atoi(value); err == nil && n >= 0 {
				contentLength = n
			}
		} else if name == "transfer-encoding" && strings.Contains(strings.ToLower(value), "chunked") {
			chunked = true
		}
	}
	return &RawRequest{Method: method, Path: path, QueryString: queryString, Headers: headers, HTTPVersion: httpVersion},
		&framing{headerEnd: headerEnd, contentLength: contentLength, chunked: chunked}, true
}

// Parse one chunked body starting at `offset` in `buf`. Returns the body and
// the offset just past the terminating chunk, or ok=false when more bytes are
// needed. A malformed chunk is an error.
func parseChunked(buf []byte, offset int) (body []byte, next int, ok bool, err error) {
	var parts [][]byte
	cursor := offset
	for {
		lineEnd := bytes.Index(buf[cursor:], []byte("\r\n"))
		if lineEnd < 0 {
			return nil, 0, false, nil
		}
		lineEnd += cursor
		sizeText := strings.TrimSpace(string(buf[cursor:lineEnd]))
		if semi := strings.IndexByte(sizeText, ';'); semi >= 0 {
			sizeText = sizeText[:semi]
		}
		size, perr := strconv.ParseInt(sizeText, 16, 64)
		if perr != nil || size < 0 {
			return nil, 0, false, errMalformedChunk
		}
		cursor = lineEnd + 2
		if size == 0 {
			trailerEnd := bytes.Index(buf[cursor:], []byte("\r\n"))
			if trailerEnd < 0 {
				return nil, 0, false, nil
			}
			trailerEnd += cursor
			var body []byte
			for _, p := range parts {
				body = append(body, p...)
			}
			return body, trailerEnd + 2, true, nil
		}
		if len(buf) < cursor+int(size)+2 {
			return nil, 0, false, nil
		}
		parts = append(parts, buf[cursor:cursor+int(size)])
		cursor += int(size) + 2
	}
}

var errMalformedChunk = &chunkError{"malformed chunked body"}

type chunkError struct{ msg string }

func (e *chunkError) Error() string { return e.msg }

// Build a server that calls `onRequest` for every complete HTTP/1.1 request.
// The handler is synchronous: it must write its response through the
// responder before returning, so responses stay in request order.
func createRawServer(onRequest func(req *RawRequest, res Responder)) net.Listener {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		panic(err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go handleConn(conn, onRequest)
		}
	}()
	return ln
}

func handleConn(conn net.Conn, onRequest func(req *RawRequest, res Responder)) {
	defer conn.Close()
	if tc, ok := conn.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}
	var buf []byte
	tmp := make([]byte, 65536)
	for {
		// Process every complete request already in the buffer.
		for {
			req, fr, ok := parseHead(buf)
			if !ok {
				break
			}
			bodyStart := fr.headerEnd + 4
			var body []byte
			var next int
			if fr.chunked {
				b, n, cok, cerr := parseChunked(buf, bodyStart)
				if cerr != nil {
					return
				}
				if !cok {
					goto needMore
				}
				body = b
				next = n
			} else {
				total := bodyStart + fr.contentLength
				if len(buf) < total {
					goto needMore
				}
				body = buf[bodyStart:total]
				next = total
			}
			buf = buf[next:]
			connection := strings.ToLower(req.Headers["connection"])
			closeAfter := connection == "close" || (req.HTTPVersion == "1.0" && connection != "keep-alive")
			req.Body = body
			res := &rawResponder{conn: conn, closeAfter: closeAfter}
			func() {
				defer func() {
					if r := recover(); r != nil {
						if !res.sent {
							res.WriteHead(500, map[string]string{"Content-Type": "application/json", "X-GitHub-Request-Id": "req-0"})
							res.End([]byte(`{"message":"Internal Server Error"}`))
						}
					}
				}()
				onRequest(req, res)
			}()
			if closeAfter {
				return
			}
		}
	needMore:
		n, err := conn.Read(tmp)
		if err != nil || n == 0 {
			return
		}
		buf = append(buf, tmp[:n]...)
	}
}
