// main.go — the entrypoint for the github-core-clone-01 forge (Go binary).
//
// Lifecycle (the harness's contract):
//   1. MAKEFUN_ENV_URL names the env's origin. Without it there is nothing to
//      talk to, so the program exits 1 at once.
//   2. Listen on an ephemeral port (listen(0)): the kernel picks a free one.
//      Never bind 8080 — the env owns it.
//   3. POST $MAKEFUN_ENV_URL/register {"port": N}. The env records the address
//      the request came from plus N as the base URL. The first register wins;
//      a 409 means another entry already registered, and this process keeps
//      waiting like any other.
//   4. GET $MAKEFUN_ENV_URL/done until the body is {"done": true}. The env
//      answers at once ({"done": false} while the suite runs), so this is a
//      poll, not a long-held request. A failed or slow poll is retried with a
//      short exponential backoff; the process never exits before the env says
//      done.
//   5. Close the server and exit 0.
//
// The server itself lives in app.go: one process, one in-memory store, four
// surfaces on the same port (git smart HTTP, REST v3, GraphQL, MCP).
//
// This binary is spawned by main.ts (the Node entrypoint the harness runs:
// `node /bot/main.ts`), which execs the statically linked binary committed
// beside it and waits for it to exit.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

const requestTimeout = 30 * time.Second
const retryInitial = 250 * time.Millisecond
const retryMax = 2 * time.Second

func retryDelay(attempt int) time.Duration {
	d := retryInitial
	for i := 0; i < attempt && i < 8; i++ {
		d *= 2
	}
	if d > retryMax {
		return retryMax
	}
	return d
}

func register(envURL string, port int, client *http.Client) error {
	body, _ := json.Marshal(map[string]int{"port": port})
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequest("POST", envURL+"/register", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("content-type", "application/json")
		resp, err := client.Do(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 || resp.StatusCode == 409 {
				return nil
			}
			if resp.StatusCode == 400 || resp.StatusCode == 405 {
				return fmt.Errorf("register answered HTTP %d", resp.StatusCode)
			}
		}
		time.Sleep(retryDelay(attempt))
	}
}

func waitForDone(envURL string, client *http.Client) {
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequest("GET", envURL+"/done", nil)
		if err == nil {
			resp, err := client.Do(req)
			if err == nil {
				var parsed struct {
					Done bool `json:"done"`
				}
				if resp.StatusCode == 200 {
					b, _ := io.ReadAll(resp.Body)
					json.Unmarshal(b, &parsed)
					if parsed.Done {
						resp.Body.Close()
						return
					}
				}
				resp.Body.Close()
			}
		}
		time.Sleep(retryDelay(attempt))
	}
}

func main() {
	envURL := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")
	if envURL == "" {
		fmt.Fprintln(os.Stderr, "[entry] MAKEFUN_ENV_URL is not set: there is no env to register with")
		os.Exit(1)
	}
	handler := newHandler()
	ln := createRawServer(handler)
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		fmt.Fprintln(os.Stderr, "[entry] the server did not bind an ephemeral TCP port")
		os.Exit(1)
	}
	fmt.Printf("[entry] listening on port %d\n", addr.Port)

	client := &http.Client{Timeout: requestTimeout}
	if err := register(envURL, addr.Port, client); err != nil {
		fmt.Fprintf(os.Stderr, "[entry] %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stdout, "[entry] registered; waiting for the env to say done")

	waitForDone(envURL, client)
	ln.Close()
	fmt.Fprintln(os.Stdout, "[entry] done")
}
