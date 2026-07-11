# Exercise 2: End-to-end test with httptest.NewServer and URL decoding

A recorder tests handler logic; it does not test that the code survives a real
HTTP round-trip and the router. This module starts the same greeting/health API on
an ephemeral port with `httptest.NewServer`, issues real requests through
`srv.URL`, and pins a contract a recorder can miss: `GreetHandler` sees a
URL-decoded path, so `/greet/alice%20smith` yields `Hello, alice smith`.

## What you'll build

```text
greetserver/                    independent module: example.com/newserver-e2e-handler
  go.mod                        go 1.26
  api.go                        Greeting; GreetHandler, HealthHandler, NewMux
  cmd/
    demo/
      main.go                   starts NewServer, GETs /greet/ada, prints the body
  api_test.go                   e2e tests via srv.Client(); URL-decoding contract test
```

- Files: `api.go`, `cmd/demo/main.go`, `api_test.go`.
- Implement: the same `GreetHandler`/`HealthHandler`/`NewMux` API, exercised through a real server.
- Test: start `httptest.NewServer(NewMux())`, register `srv.Close` with `t.Cleanup`, call `srv.Client().Get(srv.URL + path)`, read and close the body, assert the round-tripped greeting including a percent-encoded space.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/greetserver/cmd/demo
cd ~/go-exercises/greetserver
go mod init example.com/newserver-e2e-handler
```

### Recorder routing vs real server routing

With a recorder you call a handler directly, so you bypass the router entirely and
you supply `r.URL.Path` yourself. A real server is different in two ways that this
module makes concrete. First, the `http.ServeMux` actually dispatches: a request
to `/greet/bob` is matched against the registered `/greet/` prefix pattern and
routed to `GreetHandler`, and a request to `/healthz` to `HealthHandler`. Second,
the path is parsed from a real request line and *URL-decoded* before your handler
sees it: a client that requests `/greet/alice%20smith` causes `r.URL.Path` to be
`/greet/alice smith`, with the `%20` decoded to a space. So
`strings.TrimPrefix(r.URL.Path, "/greet/")` returns `alice smith`, and the
greeting round-trips as `Hello, alice smith`. This is the decoding contract the
final test pins — a behavior you only observe by going through the wire, not by
hand-feeding a recorder.

`httptest.NewServer` binds `127.0.0.1:0`, so the kernel assigns a free ephemeral
port; `srv.URL` is the resulting `http://127.0.0.1:PORT`. Because it is a real
listener with a serving goroutine, you must close it. Register the close with
`t.Cleanup(srv.Close)` rather than `defer srv.Close()`: `t.Cleanup` runs after the
test and all its subtests finish, composes correctly if a case is later moved
under a `t.Run`, and keeps the module copy-safe. Use `srv.Client()` — an
`*http.Client` preconfigured for this server — to make the calls, and always close
each response body.

Create `api.go`:

```go
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Greeting is the JSON payload returned by GreetHandler.
type Greeting struct {
	Message string `json:"message"`
}

// GreetHandler writes a JSON greeting for the (URL-decoded) name in the path.
func GreetHandler(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/greet/")
	if name == "" || name == r.URL.Path {
		http.Error(w, "missing name", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(Greeting{Message: fmt.Sprintf("Hello, %s", name)})
}

// HealthHandler reports service liveness as JSON.
func HealthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// NewMux wires the routes onto a fresh ServeMux.
func NewMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", HealthHandler)
	mux.HandleFunc("/greet/", GreetHandler)
	return mux
}
```

### The demo

The demo starts a real server, makes a real GET, and prints the round-tripped
body — the closest thing to running the service and curling it.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"io"
	"log"
	"net/http/httptest"
	"strings"

	api "example.com/newserver-e2e-handler"
)

func main() {
	srv := httptest.NewServer(api.NewMux())
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/greet/ada")
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("status: %d\n", resp.StatusCode)
	fmt.Printf("body: %s\n", strings.TrimSpace(string(body)))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
status: 200
body: {"message":"Hello, ada"}
```

### Tests

The end-to-end test starts the mux, closes it via `t.Cleanup`, and drives it with
`srv.Client()`. The table covers a plaintext name, the health route, and — the
migrated contract — a percent-encoded name that must decode to a space. Each case
reads the full body and closes it; the greeting cases decode the JSON and compare
the message.

Create `api_test.go`:

```go
package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestServerEndToEnd(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(NewMux())
	t.Cleanup(srv.Close)
	client := srv.Client()

	cases := []struct {
		name        string
		path        string
		wantCode    int
		wantMessage string // empty means do not decode a Greeting
	}{
		{name: "plain name", path: "/greet/bob", wantCode: http.StatusOK, wantMessage: "Hello, bob"},
		{name: "url-encoded space", path: "/greet/alice%20smith", wantCode: http.StatusOK, wantMessage: "Hello, alice smith"},
		{name: "health", path: "/healthz", wantCode: http.StatusOK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			resp, err := client.Get(srv.URL + tc.path)
			if err != nil {
				t.Fatalf("GET %s: %v", tc.path, err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tc.wantCode {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.wantCode)
			}
			if tc.wantMessage != "" {
				var g Greeting
				if err := json.NewDecoder(resp.Body).Decode(&g); err != nil {
					t.Fatalf("decode: %v", err)
				}
				if g.Message != tc.wantMessage {
					t.Fatalf("message = %q, want %q", g.Message, tc.wantMessage)
				}
			} else {
				// Drain so the connection can be reused.
				_, _ = io.Copy(io.Discard, resp.Body)
			}
		})
	}
}

func TestGreetHandlerWithURLEncodedName(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(NewMux())
	t.Cleanup(srv.Close)

	resp, err := srv.Client().Get(srv.URL + "/greet/alice%20smith")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var g Greeting
	if err := json.Unmarshal(body, &g); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	if g.Message != "Hello, alice smith" {
		t.Fatalf("message = %q, want %q", g.Message, "Hello, alice smith")
	}
}
```

As a "your turn" addition, add a case that requests a path the mux does not route
(e.g. `/nope`) and assert the status the default `ServeMux` returns.

## Review

The end-to-end test earns its cost by exercising the two things a recorder skips:
the router actually dispatches, and the request path is URL-decoded before the
handler runs. The `alice%20smith` case is the sharp end — it can only pass if the
server decodes the path, which pins `GreetHandler`'s "decode the name" contract at
the wire level. The hermeticity discipline is the rest of the lesson: an ephemeral
port so parallel runs never collide, `t.Cleanup(srv.Close)` so the serving
goroutine never leaks, and a closed (or drained) body on every response so
connections return to the pool. Run `go test -race`; the parallel subtests share
one server and client, and `-race` confirms that sharing is safe.

## Resources

- [net/http/httptest `NewServer`](https://pkg.go.dev/net/http/httptest#NewServer) — the ephemeral-port server and `Server.Client`.
- [net/http `ServeMux`](https://pkg.go.dev/net/http#ServeMux) — pattern matching and routing.
- [net/url `URL.Path`](https://pkg.go.dev/net/url#URL) — decoded path vs `RawPath`.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-recorder-unit-handler.md](01-recorder-unit-handler.md) | Next: [03-result-response-inspection.md](03-result-response-inspection.md)
