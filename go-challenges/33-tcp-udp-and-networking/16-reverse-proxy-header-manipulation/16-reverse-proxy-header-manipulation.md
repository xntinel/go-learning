# 16. Reverse Proxy with Header Manipulation

`httputil.ReverseProxy` handles the mechanics of proxying, but production use demands a custom `Rewrite` hook that controls path-based routing, correct X-Forwarded header propagation, request-ID tracing, response header sanitization, and structured error responses. The hard parts are understanding the three phases (rewrite, transport, modify-response), knowing which headers are hop-by-hop and therefore removed automatically, and preserving client-supplied trace IDs instead of overwriting them.

## Concepts

### The ReverseProxy Type and the Rewrite Hook

`httputil.ReverseProxy` (package `net/http/httputil`) implements `http.Handler`. It runs three sequential phases per request:

1. Call `Rewrite` (or the older `Director`) on a clone of the incoming request to produce the outbound request sent to the backend.
2. Send the outbound request via `Transport.RoundTrip`.
3. Call `ModifyResponse` on the upstream response before writing it to the client.

If step 2 or 3 returns an error, `ErrorHandler` is called instead of writing the response.

Since Go 1.20, `Rewrite` is preferred over `Director`. `Rewrite` receives a `*httputil.ProxyRequest` with two fields: `In` (the original, read-only incoming `*http.Request`) and `Out` (a writable deep clone). Mutating `Out` is the correct way to rewrite the request; `In` is available for routing decisions that need the original data.

### Path Routing: Longest-Prefix Wins

A routing table maps URL path prefixes to backend base URLs. On each request, the router picks the longest prefix that matches `r.URL.Path`. Sorting the prefixes by descending length at construction time and scanning linearly keeps the lookup O(n) — acceptable for the small route counts typical of an API gateway.

`pr.SetURL(target)` rewrites the outbound request URL to the target's scheme and host, and joins the target's path with the incoming request's path. It also sets `Out.Host` so the `Host` header the backend receives is the backend's hostname, not the proxy's.

### X-Forwarded Headers and Request Tracing

Three headers communicate client-origin information to the backend:

- `X-Forwarded-For`: the client IP, with any prior proxy IPs chained (comma-separated)
- `X-Forwarded-Host`: the `Host` value the client sent to the proxy
- `X-Forwarded-Proto`: `"http"` or `"https"` depending on how the client connected to the proxy

`ProxyRequest.SetXForwarded()` sets all three correctly. It chains existing `X-Forwarded-For` values (`"existing, clientIP"`), reads `In.TLS` to distinguish http from https, and sets `X-Forwarded-Host` from `In.Host`. Call it after `SetURL`.

`X-Request-ID` is a request-tracing header. The proxy generates a random value when the client does not supply one, and preserves the client's value when it does. This lets logs from the proxy and the backend be correlated without the proxy overwriting distributed trace IDs that the client already established.

### Hop-by-Hop Headers

Hop-by-hop headers describe the link between two adjacent nodes and must not be forwarded beyond it. RFC 7230 §6.1 names the base set: `Connection`, `Keep-Alive`, `Proxy-Authenticate`, `Proxy-Authorization`, `TE`, `Trailers`, `Transfer-Encoding`, `Upgrade`. `httputil.ReverseProxy` strips these automatically in both the outbound request and the upstream response. No manual action is needed, but knowing this prevents confusion when a header disappears in flight.

### ModifyResponse and ErrorHandler

`ModifyResponse` runs after the backend responds and before writing to the client. It is the right place to strip headers that backends must not leak (`Server`, `X-Powered-By`) and to add proxy attribution (`X-Proxy-By`) so clients and operators know which layer responded.

`ErrorHandler` runs when `Transport.RoundTrip` or `ModifyResponse` returns an error. The default writes a bare 502 with no body. A custom handler writes a structured body and sets `X-Proxy-By` so clients distinguish a proxy-layer 502 from a backend-layer one.

## Exercises

This is a library verified by `go test`. There is no standalone program in the root package.

### Exercise 1: Routing Table and Core Proxy

Create `proxy.go`:

```go
package proxy

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strings"
)

// Proxy is an HTTP reverse proxy that routes requests to upstream backends
// by longest-matching URL path prefix. Request headers are rewritten per
// RFC 7239; sensitive response headers are stripped before the response
// reaches the client.
type Proxy struct {
	rp *httputil.ReverseProxy
}

// New creates a Proxy from routes, which maps URL path prefixes to backend
// base URLs (e.g. "/api/" -> "http://127.0.0.1:9001"). An error is returned
// only when a backend URL cannot be parsed.
func New(routes map[string]string) (*Proxy, error) {
	parsed := make(map[string]*url.URL, len(routes))
	prefixes := make([]string, 0, len(routes))
	for prefix, raw := range routes {
		u, err := url.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("proxy: invalid URL for prefix %q: %w", prefix, err)
		}
		parsed[prefix] = u
		prefixes = append(prefixes, prefix)
	}
	// Descending by length so the longest prefix is tested first.
	sort.Slice(prefixes, func(i, j int) bool {
		return len(prefixes[i]) > len(prefixes[j])
	})

	route := func(path string) (*url.URL, bool) {
		for _, pfx := range prefixes {
			if strings.HasPrefix(path, pfx) {
				return parsed[pfx], true
			}
		}
		return nil, false
	}

	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			target, ok := route(pr.In.URL.Path)
			if !ok {
				// No route matched: use an unresolvable host so that
				// Transport.RoundTrip returns an error and ErrorHandler fires.
				pr.Out.URL = &url.URL{Scheme: "http", Host: "no-route.invalid"}
				return
			}
			pr.SetURL(target)
			pr.SetXForwarded()
			if pr.Out.Header.Get("X-Request-ID") == "" {
				pr.Out.Header.Set("X-Request-ID", newRequestID())
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			resp.Header.Del("Server")
			resp.Header.Del("X-Powered-By")
			resp.Header.Set("X-Proxy-By", "go-proxy/1.0")
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			rid := r.Header.Get("X-Request-ID")
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("X-Proxy-By", "go-proxy/1.0")
			http.Error(w,
				fmt.Sprintf("502 Bad Gateway (request-id: %s): %v", rid, err),
				http.StatusBadGateway,
			)
		},
	}

	return &Proxy{rp: rp}, nil
}

// ServeHTTP implements http.Handler.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.rp.ServeHTTP(w, r)
}

// newRequestID returns a random 16-hex-character string from crypto/rand.
func newRequestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
```

Key points:

- `Rewrite` receives `*httputil.ProxyRequest`; `pr.Out` starts as a deep clone of `pr.In` including its headers.
- `pr.SetURL(target)` rewrites scheme, host, and path on `pr.Out.URL` and sets `pr.Out.Host`.
- `pr.SetXForwarded()` appends the client IP to `X-Forwarded-For`, sets `X-Forwarded-Host` from `In.Host`, and sets `X-Forwarded-Proto` from `In.TLS`.
- Hop-by-hop headers (`Connection`, `Transfer-Encoding`, etc.) are removed automatically by `httputil.ReverseProxy`.

### Exercise 2: Tests

Create `proxy_test.go`:

```go
package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRouting(t *testing.T) {
	t.Parallel()

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "api")
	}))

	static := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "static")
	}))

	p, err := New(map[string]string{
		"/api/":    api.URL,
		"/static/": static.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(p)
	// t.Cleanup ensures the server is closed AFTER all parallel subtests
	// complete. A plain defer would fire when the parent function returns,
	// which happens before the parallel subtests run.
	t.Cleanup(api.Close)
	t.Cleanup(static.Close)
	t.Cleanup(srv.Close)

	cases := []struct {
		path string
		want string
	}{
		{"/api/users", "api"},
		{"/static/style.css", "static"},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()
			resp, err := http.Get(srv.URL + tc.path)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if string(body) != tc.want {
				t.Errorf("path %s: body = %q, want %q", tc.path, body, tc.want)
			}
		})
	}
}

func TestLongestPrefixWins(t *testing.T) {
	t.Parallel()

	v1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "v1")
	}))
	defer v1.Close()

	v2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "v2")
	}))
	defer v2.Close()

	p, err := New(map[string]string{
		"/api/":    v1.URL,
		"/api/v2/": v2.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(p)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v2/users")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "v2" {
		t.Errorf("body = %q, want v2 (longest prefix must win)", body)
	}
}

func TestXForwardedHeadersSet(t *testing.T) {
	t.Parallel()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Write each forwarded header on its own line for easy assertion.
		fmt.Fprintln(w, r.Header.Get("X-Forwarded-For"))
		fmt.Fprintln(w, r.Header.Get("X-Forwarded-Host"))
		fmt.Fprintln(w, r.Header.Get("X-Forwarded-Proto"))
	}))
	defer backend.Close()

	p, _ := New(map[string]string{"/": backend.URL})
	srv := httptest.NewServer(p)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/ping")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 header lines, got %q", body)
	}
	if lines[0] == "" {
		t.Errorf("X-Forwarded-For is empty in %q", body)
	}
	if lines[1] == "" {
		t.Errorf("X-Forwarded-Host is empty in %q", body)
	}
	if lines[2] != "http" {
		t.Errorf("X-Forwarded-Proto = %q, want http", lines[2])
	}
}

func TestXRequestIDGeneratedWhenAbsent(t *testing.T) {
	t.Parallel()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, r.Header.Get("X-Request-ID"))
	}))
	defer backend.Close()

	p, _ := New(map[string]string{"/": backend.URL})
	srv := httptest.NewServer(p)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/ping")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if len(body) == 0 {
		t.Error("X-Request-ID was not generated when absent from client request")
	}
}

func TestXRequestIDPreservedWhenPresent(t *testing.T) {
	t.Parallel()

	const want = "client-trace-id"

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, r.Header.Get("X-Request-ID"))
	}))
	defer backend.Close()

	p, _ := New(map[string]string{"/": backend.URL})
	srv := httptest.NewServer(p)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/ping", nil)
	req.Header.Set("X-Request-ID", want)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != want {
		t.Errorf("X-Request-ID = %q, want %q", body, want)
	}
}

func TestSensitiveResponseHeadersStripped(t *testing.T) {
	t.Parallel()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "nginx/1.25")
		w.Header().Set("X-Powered-By", "PHP/8.2")
	}))
	defer backend.Close()

	p, _ := New(map[string]string{"/": backend.URL})
	srv := httptest.NewServer(p)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/ping")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if v := resp.Header.Get("Server"); v != "" {
		t.Errorf("Server header not stripped: %q", v)
	}
	if v := resp.Header.Get("X-Powered-By"); v != "" {
		t.Errorf("X-Powered-By header not stripped: %q", v)
	}
}

func TestXProxyByAddedToResponse(t *testing.T) {
	t.Parallel()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer backend.Close()

	p, _ := New(map[string]string{"/": backend.URL})
	srv := httptest.NewServer(p)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/ping")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if got, want := resp.Header.Get("X-Proxy-By"), "go-proxy/1.0"; got != want {
		t.Errorf("X-Proxy-By = %q, want %q", got, want)
	}
}

func TestUpstreamUnavailableReturns502(t *testing.T) {
	t.Parallel()

	// Close the backend before use so its port is unreachable.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	backend.Close()

	p, _ := New(map[string]string{"/": backend.URL})
	srv := httptest.NewServer(p)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/ping")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
}

// Your turn: add TestInvalidBackendURLReturnsError that calls
// New(map[string]string{"/api/": "://bad-url"}) and asserts that the
// returned error is non-nil.

func ExampleNew() {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	p, _ := New(map[string]string{"/api/": backend.URL})
	srv := httptest.NewServer(p)
	defer srv.Close()

	resp, _ := http.Get(srv.URL + "/api/data")
	fmt.Println(resp.StatusCode)
	// Output: 200
}
```

### Exercise 3: Demo Program

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"

	"example.com/proxy"
)

func main() {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "go-api/1.0")
		fmt.Fprintf(w, "api backend  X-Request-ID=%s  X-Forwarded-For=%s",
			r.Header.Get("X-Request-ID"),
			r.Header.Get("X-Forwarded-For"),
		)
	}))
	defer api.Close()

	static := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "nginx/1.25")
		w.Header().Set("X-Powered-By", "PHP/8.2")
		fmt.Fprint(w, "static asset")
	}))
	defer static.Close()

	p, err := proxy.New(map[string]string{
		"/api/":    api.URL,
		"/static/": static.URL,
	})
	if err != nil {
		log.Fatal(err)
	}
	srv := httptest.NewServer(p)
	defer srv.Close()

	for _, path := range []string{"/api/hello", "/static/style.css"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			log.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		fmt.Printf("GET %-22s -> %d  Server=%q  X-Proxy-By=%q\n  body: %s\n\n",
			path, resp.StatusCode,
			resp.Header.Get("Server"),
			resp.Header.Get("X-Proxy-By"),
			body,
		)
	}
}
```

The demo shows:
- `/api/hello` routes to the API backend; the Server header is not forwarded because it is stripped by `ModifyResponse`.
- `/static/style.css` routes to the static backend; both `Server: nginx/1.25` and `X-Powered-By: PHP/8.2` are stripped; `X-Proxy-By: go-proxy/1.0` appears instead.

## Common Mistakes

### Overwriting the Client's X-Request-ID

Wrong:

```go
pr.Out.Header.Set("X-Request-ID", newRequestID())
```

What happens: every request gets a fresh proxy-generated ID, destroying the client's distributed trace context. The client's ID correlates its own logs; overwriting it breaks that correlation.

Fix:

```go
if pr.Out.Header.Get("X-Request-ID") == "" {
	pr.Out.Header.Set("X-Request-ID", newRequestID())
}
```

### Using Director Instead of Rewrite for New Code

Wrong: writing a `Director func(*http.Request)` that manually appends to `X-Forwarded-For` with string concatenation and misses the chaining case (when the client already sent `X-Forwarded-For`).

Fix: switch to `Rewrite` and call `pr.SetXForwarded()`. It handles chaining (`"existing, clientIP"`) and reads `In.TLS` for the proto value. `Director` is still supported but `Rewrite` is the preferred hook since Go 1.20.

### Manually Copying All Request Headers

Wrong: iterating over `pr.In.Header` and copying every entry to `pr.Out.Header` to "make sure nothing is lost". This accidentally forwards hop-by-hop headers (`Connection`, `Transfer-Encoding`) and any `Proxy-Authorization` headers.

Fix: do not copy headers manually. `pr.Out` is already a clone of `pr.In`. `httputil.ReverseProxy` strips hop-by-hop headers on the clone before sending; trust the framework to do its job and only set the headers you explicitly want.

### Expecting SetURL to Strip the Incoming Path Prefix

Wrong: routing `/api/users` to a backend and expecting the backend to receive `/users`. `pr.SetURL(target)` joins the target path with the incoming path, so the backend receives `/api/users`.

Fix: if the backend expects a stripped path, set `pr.Out.URL.Path` manually after `SetURL`:

```go
pr.SetURL(target)
pr.Out.URL.Path = strings.TrimPrefix(pr.Out.URL.Path, "/api")
```

## Verification

From `~/go-exercises/proxy`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

To run the demo:

```bash
go run ./cmd/demo
```

All four gate commands must produce no output or errors. Add `TestInvalidBackendURLReturnsError` before submitting.

## Summary

- `httputil.ReverseProxy` is an `http.Handler`; it proxies requests through three phases: Rewrite, RoundTrip, ModifyResponse.
- `Rewrite` (Go 1.20+) is the preferred hook over `Director`. It receives `*httputil.ProxyRequest` with `In` (read-only) and `Out` (writable clone).
- `pr.SetURL(target)` rewrites the outbound URL and Host; `pr.SetXForwarded()` sets all three X-Forwarded headers correctly, including chaining prior values.
- Longest-prefix routing: sort prefixes by descending length at construction time and scan linearly at request time.
- Generate `X-Request-ID` only when the client did not supply one; preserve client-supplied IDs for distributed tracing.
- Hop-by-hop headers are removed automatically; do not forward them manually.
- `ModifyResponse` strips sensitive response headers and adds proxy attribution.
- `ErrorHandler` returns a structured 502 with `X-Proxy-By` when the upstream is unreachable.

## What's Next

Next: [WebSocket Binary Frames](../17-websocket-binary-frames/17-websocket-binary-frames.md).

## Resources

- [net/http/httputil.ReverseProxy](https://pkg.go.dev/net/http/httputil#ReverseProxy)
- [net/http/httputil.ProxyRequest](https://pkg.go.dev/net/http/httputil#ProxyRequest)
- [Go 1.20 Release Notes — ReverseProxy Rewrite hook](https://go.dev/doc/go1.20#reverseproxy)
- [RFC 7230 §6.1 — Connection Header and Hop-by-Hop Headers](https://datatracker.ietf.org/doc/html/rfc7230#section-6.1)
- [RFC 7239 — Forwarded HTTP Extension](https://datatracker.ietf.org/doc/html/rfc7239)
