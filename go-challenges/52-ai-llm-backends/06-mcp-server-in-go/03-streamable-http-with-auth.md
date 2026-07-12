# Exercise 3: Serving MCP over Streamable HTTP with bearer-token auth

Stdio is for a local subprocess with one trusted host. The moment a remote agent
runtime needs your tools, you move to the Streamable HTTP transport — and that
transport is a public network surface. This exercise mounts an MCP server behind
`NewStreamableHTTPHandler`, wraps it as an ordinary `http.Handler` with bearer-token
authentication, request logging, and an unauthenticated health endpoint on a mux,
and connects a real MCP client over HTTP in-process with `httptest`. This is the
realistic production shape of an internal tool server exposed to an LLM.

This module is fully self-contained: its own `go mod init`, its own tool server,
the HTTP wiring, a bind-to-port demo, and `httptest`-based tests. It imports no
other exercise.

## What you'll build

```text
mcphttp/                    independent module: example.com/mcphttp
  go.mod                    go 1.26; requires the official MCP SDK
  server.go                 NewServer (a flag_status tool); NewHandler (auth + logging + health mux); Connect (bearer client)
  cmd/
    demo/
      main.go               binds :8080 and serves the authenticated handler
  server_test.go            httptest: 401 without token, 200 health, end-to-end CallTool with a valid token
```

- Files: `server.go`, `cmd/demo/main.go`, `server_test.go`.
- Implement: an MCP server with one tool, exposed through `mcp.NewStreamableHTTPHandler` in stateless mode, wrapped by bearer-auth and logging middleware on a `net/http.ServeMux` with a `/healthz` endpoint, plus a client helper that injects the bearer token via a custom `http.RoundTripper`.
- Test: `httptest.NewServer` hosts the handler on loopback; assert a request without a token gets 401 and never reaches a tool, a valid token connects a real `mcp` client and `CallTool` succeeds end-to-end, and `/healthz` needs no token.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/52-ai-llm-backends/06-mcp-server-in-go/03-streamable-http-with-auth/cmd/demo
cd go-solutions/52-ai-llm-backends/06-mcp-server-in-go/03-streamable-http-with-auth
go mod edit -go=1.26
go get github.com/modelcontextprotocol/go-sdk@latest
```

### The transport is an http.Handler, so treat it like one

`mcp.NewStreamableHTTPHandler(getServer, opts)` returns an `http.Handler`. The
`getServer` callback runs per request and returns the `*mcp.Server` to use — here
a single shared server for every request, which is the common case for a stateless
service. Because the result is a plain handler, everything you already do for an
HTTP API applies unchanged: you mount it on a `ServeMux`, wrap it in middleware,
and put a health check beside it. Nothing about MCP exempts this endpoint from
authentication — skipping it publishes your internal tools to anyone who can reach
the port.

We enforce auth in middleware that runs before the MCP handler. A request without
a valid `Authorization: Bearer <token>` header gets a `401` and never reaches a
tool. The comparison uses `crypto/subtle.ConstantTimeCompare` so token checking
does not leak length or content through timing. The `/healthz` endpoint is mounted
outside the auth wrapper, because a load balancer must probe liveness without
credentials.

### Stateless versus session mode

`StreamableHTTPOptions{Stateless: true}` tells the handler not to track a session
id per client: each request uses a fresh, default-initialized session. The
trade-off is explicit. Stateless mode scales horizontally — any process behind a
load balancer can serve any request, because there is no session affinity to
preserve — but it gives up server-initiated messages (the server cannot push a
notification to a client between its requests) and per-session state. The default
(stateful) mode keeps a session id and supports server-to-client messages, at the
cost of pinning a client to the process that holds its session. For an internal
tool server that only answers tool calls and needs to scale out, stateless is the
right default; choose stateful when you need progress notifications or resource
subscriptions.

### The client injects the token via a RoundTripper

The client transport, `mcp.StreamableClientTransport{Endpoint: url}`, makes plain
HTTP requests. To authenticate, we give it an `*http.Client` whose `Transport` is
a small `http.RoundTripper` that sets the `Authorization` header on every outbound
request. This is the idiomatic way to attach credentials to any Go HTTP client and
keeps the auth concern out of the MCP call sites.

Create `server.go`:

```go
package mcphttp

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ErrUnknownFlag is returned by the flag_status tool for an unknown flag name.
var ErrUnknownFlag = errors.New("unknown flag")

// FlagInput is the argument surface of the flag_status tool.
type FlagInput struct {
	Name string `json:"name" jsonschema:"the feature-flag name to look up"`
}

// FlagOutput is the structured result of flag_status.
type FlagOutput struct {
	Name    string `json:"name" jsonschema:"the flag name that was looked up"`
	Enabled bool   `json:"enabled" jsonschema:"whether the flag is enabled"`
}

// NewServer builds an MCP server exposing a single flag_status tool backed by an
// in-memory flag table.
func NewServer() *mcp.Server {
	flags := map[string]bool{"new-checkout": true, "beta-search": false}

	s := mcp.NewServer(&mcp.Implementation{Name: "flags", Version: "v1.0.0"}, nil)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "flag_status",
		Description: "Report whether a named feature flag is enabled.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in FlagInput) (*mcp.CallToolResult, FlagOutput, error) {
		enabled, ok := flags[in.Name]
		if !ok {
			return nil, FlagOutput{}, fmt.Errorf("flag_status: %w: %s", ErrUnknownFlag, in.Name)
		}
		return nil, FlagOutput{Name: in.Name, Enabled: enabled}, nil
	})
	return s
}

// requireBearer rejects any request without the exact bearer token before it can
// reach the wrapped handler. The comparison is constant-time.
func requireBearer(token string, next http.Handler) http.Handler {
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// logRequests records method, path, and latency for every request.
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start))
	})
}

// NewHandler wires the MCP server behind auth on /mcp and an unauthenticated
// health check on /healthz, with logging around everything. The MCP transport is
// stateless so the service can scale horizontally.
func NewHandler(token string) http.Handler {
	server := NewServer()
	mcpHandler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Stateless: true},
	)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})
	mux.Handle("/mcp", requireBearer(token, mcpHandler))
	return logRequests(mux)
}

// bearerTransport attaches the bearer token to every outbound request.
type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (b *bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(r)
}

// Connect dials the remote MCP endpoint with the given bearer token and returns a
// live client session.
func Connect(ctx context.Context, endpoint, token string) (*mcp.ClientSession, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "v1.0.0"}, nil)
	transport := &mcp.StreamableClientTransport{
		Endpoint:   endpoint,
		HTTPClient: &http.Client{Transport: &bearerTransport{token: token, base: http.DefaultTransport}},
	}
	return client.Connect(ctx, transport, nil)
}
```

### The runnable demo

The demo binds a real port and serves the authenticated handler; it blocks like
any HTTP server. It prints one startup line to stdout before serving.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"
	"net/http"

	"example.com/mcphttp"
)

func main() {
	const token = "dev-token"
	const addr = ":8080"

	fmt.Printf("mcp server listening on %s (POST /mcp, GET /healthz)\n", addr)
	if err := http.ListenAndServe(addr, mcphttp.NewHandler(token)); err != nil {
		log.Fatal(err)
	}
}
```

Run it (in another terminal, `curl -s localhost:8080/healthz` returns `ok`; a POST
to `/mcp` without the token returns `401`):

```bash
go run ./cmd/demo
```

Expected output:

```
mcp server listening on :8080 (POST /mcp, GET /healthz)
```

### Tests

`httptest.NewServer` hosts the handler on a loopback address in the same process —
local, not external network, so no build tag is needed. The first test posts to
`/mcp` with no token and asserts `401`, proving the middleware rejects the request
before any tool runs. The second connects a real MCP client through
`StreamableClientTransport` with a valid token and calls the tool end-to-end. A
third checks that `/healthz` answers without credentials.

Create `server_test.go`:

```go
package mcphttp

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const testToken = "test-token"

func TestUnauthenticated(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(NewHandler(testToken))
	t.Cleanup(srv.Close)

	body := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	resp, err := http.Post(srv.URL+"/mcp", "application/json", body)
	if err != nil {
		t.Fatalf("POST /mcp: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestHealthzNoAuth(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(NewHandler(testToken))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestAuthenticatedCallTool(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(NewHandler(testToken))
	t.Cleanup(srv.Close)

	cs, err := Connect(t.Context(), srv.URL+"/mcp", testToken)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "flag_status",
		Arguments: map[string]any{"name": "new-checkout"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected tool error: %v", res.Content)
	}
	var out FlagOutput
	b, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if out.Name != "new-checkout" || !out.Enabled {
		t.Fatalf("got %+v, want new-checkout enabled", out)
	}
}

func Example() {
	srv := httptest.NewServer(NewHandler(testToken))
	defer srv.Close()

	cs, err := Connect(context.Background(), srv.URL+"/mcp", testToken)
	if err != nil {
		log.Fatal(err)
	}
	defer cs.Close()

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "flag_status",
		Arguments: map[string]any{"name": "beta-search"},
	})
	if err != nil {
		log.Fatal(err)
	}
	var out FlagOutput
	b, _ := json.Marshal(res.StructuredContent)
	_ = json.Unmarshal(b, &out)
	fmt.Printf("%s enabled=%v\n", out.Name, out.Enabled)
	// Output: beta-search enabled=false
}
```

## Review

The security property is the whole point: `TestUnauthenticated` proves a request
without the bearer token is rejected with `401` before the MCP handler runs, so no
tool executes. `TestAuthenticatedCallTool` proves the full remote path works — a
real client, over HTTP, through the auth middleware, into the tool and back with
structured output. `TestHealthzNoAuth` proves the liveness probe is deliberately
open.

The mistakes to avoid are the ones that turn an internal server into an open one.
Do not mount the streamable handler without middleware; the handler itself performs
no authentication. Compare tokens in constant time rather than with `==`, and put
the health endpoint outside the auth wrapper, not inside it. On the client side,
attach credentials through the transport's `HTTPClient` round-tripper rather than
trying to thread headers through each call. Finally, weigh stateless against
stateful deliberately: stateless scales out but forfeits server-initiated
messages, so if you later need progress notifications you must revisit the choice.

## Resources

- [`mcp` package reference](https://pkg.go.dev/github.com/modelcontextprotocol/go-sdk/mcp) — `NewStreamableHTTPHandler`, `StreamableHTTPOptions`, `StreamableClientTransport`.
- [MCP specification — Transports](https://modelcontextprotocol.io/specification) — the Streamable HTTP transport, sessions, and SSE versus JSON responses.
- [`net/http/httptest`](https://pkg.go.dev/net/http/httptest) — in-process HTTP servers for testing handlers on loopback.
- [`crypto/subtle`](https://pkg.go.dev/crypto/subtle) — `ConstantTimeCompare` for credential checks that do not leak through timing.

---

Back to [02-resources-and-prompts.md](02-resources-and-prompts.md) | Next: [../07-prompt-templating-token-budgeting/00-concepts.md](../07-prompt-templating-token-budgeting/00-concepts.md)
