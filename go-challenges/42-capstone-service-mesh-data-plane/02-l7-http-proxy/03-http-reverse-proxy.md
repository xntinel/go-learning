# Exercise 3: HTTP Reverse Proxy

This is the integrating exercise: an `http.Handler` that matches a route, builds a sanitized outbound request, forwards it through a pooled client, and streams the response back — with per-route timeouts, proxy-identification headers, and a 502-versus-504 distinction. It bundles its own router and header layer so it gates as one standalone module.

This module is fully self-contained: its own `go mod init`, every type it needs defined inline (the router and header code reappear here so nothing is imported from Exercises 1 and 2), its own demo and tests.

## What you'll build

```text
router.go            Router, Route, HeaderRule, Match (bundled from Exercise 1)
headers.go           stripHopByHop, applyHeaderRules (bundled from Exercise 2)
proxy.go             Proxy, New, Options, ServeHTTP, buildOutboundRequest, 502/504
cmd/
  demo/
    main.go          one upstream, an /api/ route and a catch-all, two requests
proxy_test.go        routing, hop-by-hop strip, X-Forwarded-*, header rules, 502, 504, timeout
```

- Files: `router.go`, `headers.go`, `proxy.go`, `cmd/demo/main.go`, `proxy_test.go`.
- Implement: `New(router *Router, opts ...Option) *Proxy` with a pooled, redirect-disabled `http.Client`; `ServeHTTP`; the `WithLogger` and `WithDefaultTimeout` options; outbound-request construction with hop-by-hop stripping, route header rules, and `X-Forwarded-*`; and the 502/504 error path.
- Test: host and path routing, hop-by-hop stripping before forwarding, `X-Forwarded-For` appended, `X-Forwarded-Host` set, route header rules applied, body streamed, 502 on no-route and on connection refused, 504 on timeout, and the logger callback.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/42-capstone-service-mesh-data-plane/02-l7-http-proxy/03-http-reverse-proxy/cmd/demo && cd go-solutions/42-capstone-service-mesh-data-plane/02-l7-http-proxy/03-http-reverse-proxy
go mod edit -go=1.26
```

### The router and header layers, bundled

The proxy needs the same routing and header logic built in the first two exercises. Because each module is standalone, that code is duplicated here verbatim rather than imported. The router file is identical in shape to Exercise 1 (here the package is `httpproxy`), and the header file is Exercise 2's logic with the two helpers kept unexported — `stripHopByHop` and `applyHeaderRules` — since only this package calls them.

Create `router.go`:

```go
package httpproxy

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// ErrNoRoute is returned by Router.Match when no rule matches.
var ErrNoRoute = errors.New("httpproxy: no matching route")

// Route describes one routing rule. An empty Host or PathPrefix matches anything.
type Route struct {
	Host       string        // empty matches any host
	PathPrefix string        // empty matches any path
	Upstream   *url.URL      // where to forward matching requests
	Timeout    time.Duration // per-route deadline; 0 inherits the proxy default
	Headers    []HeaderRule  // applied after hop-by-hop headers are stripped
}

// HeaderRule adds, sets, or removes one header on the outbound request.
type HeaderRule struct {
	Action string // "add", "set", or "remove"
	Name   string
	Value  string // ignored for "remove"
}

// Router holds an ordered list of routes and returns the first match.
// Rules are evaluated in declaration order; the first match wins.
type Router struct {
	routes []Route
}

// NewRouter returns a Router backed by routes.
func NewRouter(routes []Route) *Router {
	return &Router{routes: routes}
}

// Match returns the first route whose Host and PathPrefix constraints are
// satisfied by host and path. Port suffixes on host are stripped before
// comparison.
func (r *Router) Match(host, path string) (Route, error) {
	if i := strings.LastIndexByte(host, ':'); i > 0 {
		host = host[:i]
	}
	for _, route := range r.routes {
		if route.Host != "" && route.Host != host {
			continue
		}
		if route.PathPrefix != "" && !strings.HasPrefix(path, route.PathPrefix) {
			continue
		}
		return route, nil
	}
	return Route{}, fmt.Errorf("%w: host=%q path=%q", ErrNoRoute, host, path)
}
```

Create `headers.go`:

```go
package httpproxy

import (
	"net/http"
	"strings"
)

// hopByHopHeaders is the standard set defined in RFC 9110 section 7.6.1.
var hopByHopHeaders = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailers":            true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

// stripHopByHop removes hop-by-hop headers from h. It first processes the
// Connection header to remove sender-nominated headers (RFC 9110 section
// 7.6.1), then removes the standard hop-by-hop set.
func stripHopByHop(h http.Header) {
	for _, connVal := range h["Connection"] {
		for _, name := range strings.Split(connVal, ",") {
			if name = strings.TrimSpace(name); name != "" {
				h.Del(name)
			}
		}
	}
	for name := range hopByHopHeaders {
		h.Del(name)
	}
}

// applyHeaderRules runs each rule in order against h.
func applyHeaderRules(h http.Header, rules []HeaderRule) {
	for _, r := range rules {
		switch r.Action {
		case "add":
			h.Add(r.Name, r.Value)
		case "set":
			h.Set(r.Name, r.Value)
		case "remove":
			h.Del(r.Name)
		}
	}
}
```

### The handler: route, build, forward, stream

`ServeHTTP` is the whole request lifecycle in one method, and the order of its steps is the design. First it matches the route; a miss is a 502 with a JSON body, because from the client's view an unroutable request is a bad-gateway condition. Then it derives a timeout context from `r.Context()` using the route's timeout (or the proxy default when the route's is zero), so the upstream call is bounded by the route deadline *and* cancelled if the client disconnects — `defer cancel()` ensures the timer never leaks.

`buildOutboundRequest` is where header correctness lives, and the order is mandatory. It copies the upstream's scheme and host but keeps the client's path and query; it propagates `ContentLength`; it clones every inbound header and then, in this exact sequence, calls `stripHopByHop` to drop the hop-by-hop set, `applyHeaderRules` to run the route's operator policy, and `setProxyHeaders` to chain `X-Forwarded-For` / `-Host` / `-Proto`. Stripping must come before the rules and the forwarding headers, or a hop-by-hop header could survive into the outbound request. Finally it sets `outReq.Host = r.Host`, so a virtual-hosting upstream sees the name the client asked for instead of the upstream's own address.

After `client.Do`, an error is classified: if `ctx.Err()` is non-nil the failure is a timeout and the status is 504, otherwise the upstream was unreachable or invalid and the status is 502. On success the response headers are stripped of hop-by-hop fields, copied to the client, the status is written, and `io.Copy` streams the body without buffering. The `defer resp.Body.Close()` immediately after a successful `Do` is what returns the connection to the pool on every exit path.

The `http.Client` is configured once in `New`: `CheckRedirect` returns `http.ErrUseLastResponse` so a 3xx is relayed rather than followed, and the `http.Transport` sets pool sizes and timeouts. Functional options (`WithLogger`, `WithDefaultTimeout`) keep the constructor extensible without a widening parameter list.

Create `proxy.go`:

```go
package httpproxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

const defaultTimeout = 30 * time.Second

// Proxy is an http.Handler that routes and forwards HTTP/1.1 requests.
// Use New to construct one.
type Proxy struct {
	router    *Router
	client    *http.Client
	logger    func(method, path, upstream string, status int, latency time.Duration)
	defaultTO time.Duration
}

// Option is a functional option for New.
type Option func(*Proxy)

// WithLogger sets the per-request log callback.
// Arguments: HTTP method, request path, upstream host, response status, latency.
func WithLogger(fn func(method, path, upstream string, status int, latency time.Duration)) Option {
	return func(p *Proxy) { p.logger = fn }
}

// WithDefaultTimeout sets the fallback per-request timeout used when a route's
// Timeout field is zero.
func WithDefaultTimeout(d time.Duration) Option {
	return func(p *Proxy) { p.defaultTO = d }
}

// New returns a Proxy backed by router. The internal http.Client uses a pooled
// Transport: no redirect following, 90s idle-connection TTL, 5s dial timeout.
func New(router *Router, opts ...Option) *Proxy {
	p := &Proxy{
		router:    router,
		defaultTO: defaultTimeout,
		client: &http.Client{
			// Do not follow redirects: relay 3xx back to the client unchanged.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Transport: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
				MaxIdleConns:          100,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
			},
		},
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// ServeHTTP implements http.Handler.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	route, err := p.router.Match(r.Host, r.URL.Path)
	if err != nil {
		writeError(w, http.StatusBadGateway, "no upstream route", err)
		return
	}

	timeout := route.Timeout
	if timeout <= 0 {
		timeout = p.defaultTO
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	outReq, err := buildOutboundRequest(ctx, r, route)
	if err != nil {
		writeError(w, http.StatusBadGateway, "build request", err)
		return
	}

	resp, err := p.client.Do(outReq)
	if err != nil {
		if ctx.Err() != nil {
			writeError(w, http.StatusGatewayTimeout, "upstream timeout", ctx.Err())
		} else {
			writeError(w, http.StatusBadGateway, "upstream unreachable", err)
		}
		return
	}
	defer resp.Body.Close()

	// Strip hop-by-hop from the response and copy the rest to the client.
	stripHopByHop(resp.Header)
	for name, values := range resp.Header {
		for _, v := range values {
			w.Header().Add(name, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	// Stream the body without buffering. The error is intentionally discarded:
	// the status code is already committed, and a write error usually means the
	// client disconnected.
	_, _ = io.Copy(w, resp.Body)

	if p.logger != nil {
		p.logger(r.Method, r.URL.Path, route.Upstream.Host, resp.StatusCode, time.Since(start))
	}
}

// buildOutboundRequest constructs the upstream request from the inbound one.
func buildOutboundRequest(ctx context.Context, r *http.Request, route Route) (*http.Request, error) {
	// Upstream scheme+host, original path+query.
	target := *route.Upstream
	target.Path = r.URL.Path
	target.RawQuery = r.URL.RawQuery

	outReq, err := http.NewRequestWithContext(ctx, r.Method, target.String(), r.Body)
	if err != nil {
		return nil, fmt.Errorf("httpproxy: new request: %w", err)
	}
	// Propagate Content-Length so the upstream receives the correct value.
	outReq.ContentLength = r.ContentLength

	// Clone all request headers, then sanitize.
	for name, values := range r.Header {
		for _, v := range values {
			outReq.Header.Add(name, v)
		}
	}
	stripHopByHop(outReq.Header)
	applyHeaderRules(outReq.Header, route.Headers)
	setProxyHeaders(outReq.Header, r)

	// Preserve the original Host so the upstream can do virtual hosting.
	outReq.Host = r.Host

	return outReq, nil
}

// setProxyHeaders appends/sets the standard proxy identification headers.
func setProxyHeaders(h http.Header, r *http.Request) {
	clientIP, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		clientIP = r.RemoteAddr
	}
	if prior := h.Get("X-Forwarded-For"); prior != "" {
		h.Set("X-Forwarded-For", prior+", "+clientIP)
	} else {
		h.Set("X-Forwarded-For", clientIP)
	}
	if h.Get("X-Forwarded-Host") == "" {
		h.Set("X-Forwarded-Host", r.Host)
	}
	if h.Get("X-Forwarded-Proto") == "" {
		proto := "http"
		if r.TLS != nil {
			proto = "https"
		}
		h.Set("X-Forwarded-Proto", proto)
	}
}

// errBody is the JSON shape returned for 5xx proxy errors.
type errBody struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

func writeError(w http.ResponseWriter, code int, label string, cause error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	b := errBody{
		Error:   strings.ToLower(http.StatusText(code)),
		Message: label + ": " + cause.Error(),
	}
	_, _ = fmt.Fprintf(w, `{"error":%q,"message":%q}`, b.Error, b.Message)
}
```

`setProxyHeaders` is the one piece worth re-reading: it appends to `X-Forwarded-For` when a prior value exists and only sets it outright when absent, which is what preserves the original client across a proxy chain. `X-Forwarded-Host` and `-Proto` are set only when not already present, so a downstream proxy does not clobber an upstream's value.

### The runnable demo

The demo starts one in-process upstream that echoes the path and the proxy-set headers it received, routes `/api/` through a rule that sets `X-Service: demo` and falls everything else through a catch-all, and issues two requests. The logger is wired to record method, path, and status only — not ports or latency — so the output is deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"time"

	"example.com/http-reverse-proxy"
)

func main() {
	// A single upstream that echoes the path and the proxy-set headers it saw.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "path=%s xff=%s proto=%s service=%s",
			r.URL.Path,
			r.Header.Get("X-Forwarded-For"),
			r.Header.Get("X-Forwarded-Proto"),
			r.Header.Get("X-Service"),
		)
	}))
	defer upstream.Close()

	u, err := url.Parse(upstream.URL)
	if err != nil {
		fmt.Println("parse:", err)
		return
	}

	router := httpproxy.NewRouter([]httpproxy.Route{
		{
			PathPrefix: "/api/",
			Upstream:   u,
			Timeout:    5 * time.Second,
			Headers:    []httpproxy.HeaderRule{{Action: "set", Name: "X-Service", Value: "demo"}},
		},
		{Upstream: u}, // catch-all
	})

	// A deterministic logger: method, path, and status only (no ports/latency).
	var logLines []string
	proxy := httpproxy.New(router,
		httpproxy.WithLogger(func(method, path, _ string, status int, _ time.Duration) {
			logLines = append(logLines, fmt.Sprintf("%s %s status=%d", method, path, status))
		}),
	)

	proxySrv := httptest.NewServer(proxy)
	defer proxySrv.Close()

	for _, path := range []string{"/api/users", "/health"} {
		resp, err := http.Get(proxySrv.URL + path)
		if err != nil {
			fmt.Printf("GET %s: %v\n", path, err)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		fmt.Printf("GET %-12s -> %d: %s\n", path, resp.StatusCode, body)
	}

	sort.Strings(logLines)
	for _, line := range logLines {
		fmt.Println("log:", line)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
GET /api/users   -> 200: path=/api/users xff=127.0.0.1 proto=http service=demo
GET /health      -> 200: path=/health xff=127.0.0.1 proto=http service=
log: GET /api/users status=200
log: GET /health status=200
```

The `/api/users` request matches the first route and gains `X-Service: demo`; both requests carry the proxy-set `X-Forwarded-For: 127.0.0.1` and `X-Forwarded-Proto: http`. The `/health` request falls through to the catch-all, which has no header rule, so `service=` is empty. The two sorted log lines confirm the logger callback fired for each request.

### Tests

The tests are the real verification — there is no output to eyeball beyond the demo. They cover the router and header units, then the integration surface: host and path routing to distinct backends, hop-by-hop stripping before forwarding, `X-Forwarded-For` appended to a prior value, `X-Forwarded-Host` set to the client's `Host`, route header rules applied, the body streamed intact, 502 on an unroutable host and on a refused connection, 504 when the upstream exceeds the route timeout, and the logger callback. An `ExampleNew` doubles as runnable documentation.

Create `proxy_test.go`:

```go
package httpproxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// echoServer returns a test upstream that mirrors select request headers back
// as "X-Echo-<name>" response headers and echoes the body.
func echoServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, name := range []string{
			"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto",
			"X-Service", "X-Custom-Hop",
		} {
			if v := r.Header.Get(name); v != "" {
				w.Header().Set("X-Echo-"+name, v)
			}
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, r.Body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", raw, err)
	}
	return u
}

func newProxySrv(t *testing.T, router *Router, opts ...Option) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(New(router, opts...))
	t.Cleanup(srv.Close)
	return srv
}

// --- Router unit tests ---

func TestRouterMatchByHost(t *testing.T) {
	t.Parallel()
	u := mustParseURL(t, "http://backend:8080")
	r := NewRouter([]Route{{Host: "api.example.com", Upstream: u}})

	got, err := r.Match("api.example.com", "/any")
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if got.Upstream != u {
		t.Fatal("upstream mismatch")
	}
}

func TestRouterMatchStripsPort(t *testing.T) {
	t.Parallel()
	u := mustParseURL(t, "http://backend:8080")
	r := NewRouter([]Route{{Host: "api.example.com", Upstream: u}})

	if _, err := r.Match("api.example.com:443", "/"); err != nil {
		t.Fatalf("Match with port: %v", err)
	}
}

func TestRouterFirstRuleWins(t *testing.T) {
	t.Parallel()
	u1 := mustParseURL(t, "http://first:80")
	u2 := mustParseURL(t, "http://second:80")
	r := NewRouter([]Route{
		{PathPrefix: "/api/", Upstream: u1},
		{PathPrefix: "/api/v1/", Upstream: u2},
	})

	got, err := r.Match("", "/api/v1/users")
	if err != nil {
		t.Fatal(err)
	}
	if got.Upstream != u1 {
		t.Errorf("first rule should win, got %v", got.Upstream)
	}
}

func TestRouterNoMatchReturnsErrNoRoute(t *testing.T) {
	t.Parallel()
	r := NewRouter([]Route{{Host: "specific.example.com", Upstream: mustParseURL(t, "http://b:80")}})
	_, err := r.Match("other.example.com", "/")
	if !errors.Is(err, ErrNoRoute) {
		t.Fatalf("err = %v, want ErrNoRoute", err)
	}
}

// --- Header unit tests ---

func TestStripHopByHopStandardSet(t *testing.T) {
	t.Parallel()
	h := http.Header{}
	for name := range hopByHopHeaders {
		h.Set(name, "value")
	}
	h.Set("X-Custom", "keep")
	stripHopByHop(h)

	for name := range hopByHopHeaders {
		if h.Get(name) != "" {
			t.Errorf("%s should be stripped", name)
		}
	}
	if h.Get("X-Custom") != "keep" {
		t.Error("X-Custom should be preserved")
	}
}

func TestStripHopByHopConnectionNominated(t *testing.T) {
	t.Parallel()
	h := http.Header{}
	h.Set("Connection", "close, X-Nominated")
	h.Set("X-Nominated", "should-go")
	h.Set("X-Keep", "keep")
	stripHopByHop(h)

	if h.Get("X-Nominated") != "" {
		t.Error("X-Nominated should be stripped (nominated by Connection header)")
	}
	if h.Get("X-Keep") != "keep" {
		t.Error("X-Keep should be preserved")
	}
}

func TestApplyHeaderRulesSetAndRemove(t *testing.T) {
	t.Parallel()
	h := http.Header{}
	h.Set("X-Token", "old")
	h.Set("X-Secret", "sensitive")
	applyHeaderRules(h, []HeaderRule{
		{Action: "set", Name: "X-Token", Value: "new"},
		{Action: "remove", Name: "X-Secret"},
	})
	if h.Get("X-Token") != "new" {
		t.Errorf("X-Token = %q, want new", h.Get("X-Token"))
	}
	if h.Get("X-Secret") != "" {
		t.Error("X-Secret should be removed")
	}
}

// --- Integration tests ---

func TestProxyForwardsToUpstream(t *testing.T) {
	t.Parallel()
	upstream := echoServer(t)
	proxy := newProxySrv(t, NewRouter([]Route{{Upstream: mustParseURL(t, upstream.URL)}}))

	resp, err := http.Get(proxy.URL + "/ping")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestProxyHostRouting(t *testing.T) {
	t.Parallel()
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Backend", "api")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(apiSrv.Close)

	webSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Backend", "web")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(webSrv.Close)

	router := NewRouter([]Route{
		{Host: "api.example.com", Upstream: mustParseURL(t, apiSrv.URL)},
		{Host: "web.example.com", Upstream: mustParseURL(t, webSrv.URL)},
	})
	proxy := newProxySrv(t, router)

	for _, tc := range []struct{ host, backend string }{
		{"api.example.com", "api"},
		{"web.example.com", "web"},
	} {
		req, _ := http.NewRequest(http.MethodGet, proxy.URL+"/", nil)
		req.Host = tc.host
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("host=%s: Do: %v", tc.host, err)
		}
		resp.Body.Close()
		if got := resp.Header.Get("X-Backend"); got != tc.backend {
			t.Errorf("host=%s: X-Backend = %q, want %q", tc.host, got, tc.backend)
		}
	}
}

func TestProxyPathPrefixRouting(t *testing.T) {
	t.Parallel()
	v1Srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Backend", "v1")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(v1Srv.Close)

	v2Srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Backend", "v2")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(v2Srv.Close)

	router := NewRouter([]Route{
		{PathPrefix: "/api/v2/", Upstream: mustParseURL(t, v2Srv.URL)},
		{PathPrefix: "/api/v1/", Upstream: mustParseURL(t, v1Srv.URL)},
	})
	proxy := newProxySrv(t, router)

	for _, tc := range []struct{ path, want string }{
		{"/api/v1/users", "v1"},
		{"/api/v2/orders", "v2"},
	} {
		resp, err := http.Get(proxy.URL + tc.path)
		if err != nil {
			t.Fatalf("path=%s: Get: %v", tc.path, err)
		}
		resp.Body.Close()
		if got := resp.Header.Get("X-Backend"); got != tc.want {
			t.Errorf("path=%s: X-Backend = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestProxyStripsHopByHopBeforeForwarding(t *testing.T) {
	t.Parallel()
	var gotHdr http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHdr = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	proxy := newProxySrv(t, NewRouter([]Route{{Upstream: mustParseURL(t, upstream.URL)}}))

	req, _ := http.NewRequest(http.MethodGet, proxy.URL+"/", nil)
	req.Header.Set("Connection", "close, X-Custom-Hop")
	req.Header.Set("X-Custom-Hop", "should-be-stripped")
	req.Header.Set("Transfer-Encoding", "identity")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()

	for _, name := range []string{"Connection", "Transfer-Encoding", "X-Custom-Hop", "Keep-Alive"} {
		if gotHdr.Get(name) != "" {
			t.Errorf("upstream saw hop-by-hop header %q; should have been stripped", name)
		}
	}
}

func TestProxySetsXForwardedFor(t *testing.T) {
	t.Parallel()
	upstream := echoServer(t)
	proxy := newProxySrv(t, NewRouter([]Route{{Upstream: mustParseURL(t, upstream.URL)}}))

	resp, err := http.Get(proxy.URL + "/")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	resp.Body.Close()

	if resp.Header.Get("X-Echo-X-Forwarded-For") == "" {
		t.Error("upstream did not receive X-Forwarded-For")
	}
}

func TestProxyAppendsXForwardedFor(t *testing.T) {
	t.Parallel()
	var gotXFF string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotXFF = r.Header.Get("X-Forwarded-For")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	proxy := newProxySrv(t, NewRouter([]Route{{Upstream: mustParseURL(t, upstream.URL)}}))

	req, _ := http.NewRequest(http.MethodGet, proxy.URL+"/", nil)
	req.Header.Set("X-Forwarded-For", "10.0.0.1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()

	if !strings.HasPrefix(gotXFF, "10.0.0.1, ") {
		t.Errorf("X-Forwarded-For = %q, want prefix \"10.0.0.1, \"", gotXFF)
	}
}

func TestProxyXForwardedHostIsSet(t *testing.T) {
	t.Parallel()
	var gotXFH string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotXFH = r.Header.Get("X-Forwarded-Host")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	proxy := newProxySrv(t, NewRouter([]Route{{Upstream: mustParseURL(t, upstream.URL)}}))

	req, _ := http.NewRequest(http.MethodGet, proxy.URL+"/", nil)
	req.Host = "my.service.local"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()

	if gotXFH != "my.service.local" {
		t.Errorf("X-Forwarded-Host = %q, want my.service.local", gotXFH)
	}
}

func TestProxyAppliesHeaderRules(t *testing.T) {
	t.Parallel()
	var gotHdr http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHdr = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	router := NewRouter([]Route{{
		Upstream: mustParseURL(t, upstream.URL),
		Headers: []HeaderRule{
			{Action: "set", Name: "X-Service", Value: "payments"},
			{Action: "remove", Name: "X-Internal-Token"},
		},
	}})
	proxy := newProxySrv(t, router)

	req, _ := http.NewRequest(http.MethodGet, proxy.URL+"/", nil)
	req.Header.Set("X-Internal-Token", "secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()

	if gotHdr.Get("X-Service") != "payments" {
		t.Errorf("X-Service = %q, want payments", gotHdr.Get("X-Service"))
	}
	if gotHdr.Get("X-Internal-Token") != "" {
		t.Error("X-Internal-Token should have been removed by header rule")
	}
}

func TestProxyStreamsBody(t *testing.T) {
	t.Parallel()
	const want = "hello from upstream"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, want)
	}))
	t.Cleanup(upstream.Close)

	proxy := newProxySrv(t, NewRouter([]Route{{Upstream: mustParseURL(t, upstream.URL)}}))

	resp, err := http.Get(proxy.URL + "/")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if string(got) != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

func TestProxy502WhenNoRouteMatches(t *testing.T) {
	t.Parallel()
	router := NewRouter([]Route{
		{Host: "specific.example.com", Upstream: mustParseURL(t, "http://127.0.0.1:1")},
	})
	proxy := newProxySrv(t, router)

	req, _ := http.NewRequest(http.MethodGet, proxy.URL+"/", nil)
	req.Host = "other.example.com" // deliberately does not match any route
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode JSON body: %v", err)
	}
	if body["error"] == "" {
		t.Error("JSON error field is empty")
	}
}

func TestProxy502WhenUpstreamRefuses(t *testing.T) {
	t.Parallel()
	// Port 1 is reserved; connections are refused immediately.
	router := NewRouter([]Route{{Upstream: mustParseURL(t, "http://127.0.0.1:1")}})
	proxy := newProxySrv(t, router)

	resp, err := http.Get(proxy.URL + "/")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
}

func TestProxy504WhenUpstreamExceedsTimeout(t *testing.T) {
	t.Parallel()
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // block until the proxy cancels the context
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(slow.Close)

	router := NewRouter([]Route{
		{Upstream: mustParseURL(t, slow.URL), Timeout: 50 * time.Millisecond},
	})
	proxy := newProxySrv(t, router)

	resp, err := http.Get(proxy.URL + "/")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Errorf("status = %d, want 504", resp.StatusCode)
	}
}

func TestProxyWithLogger(t *testing.T) {
	t.Parallel()
	upstream := echoServer(t)
	var logged bool
	proxy := newProxySrv(t,
		NewRouter([]Route{{Upstream: mustParseURL(t, upstream.URL)}}),
		WithLogger(func(_, _ string, _ string, _ int, _ time.Duration) {
			logged = true
		}),
	)

	resp, err := http.Get(proxy.URL + "/")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	resp.Body.Close()
	if !logged {
		t.Error("logger was not called")
	}
}

// ExampleNew demonstrates a minimal proxy: one catch-all route forwarding
// every request to a local test upstream.
func ExampleNew() {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "upstream: %s", r.URL.Path)
	}))
	defer upstream.Close()

	u, _ := url.Parse(upstream.URL)
	proxy := New(NewRouter([]Route{{Upstream: u}}))
	proxySrv := httptest.NewServer(proxy)
	defer proxySrv.Close()

	resp, _ := http.Get(proxySrv.URL + "/hello")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	fmt.Println(string(body))
	// Output:
	// upstream: /hello
}
```

## Review

Correctness here is mostly about ordering and classification. The header order in `buildOutboundRequest` — clone, strip, apply rules, set forwarding headers — is load-bearing: `TestProxyStripsHopByHopBeforeForwarding` sends `Connection: close, X-Custom-Hop` plus `Transfer-Encoding` and asserts the upstream sees none of them, which only holds if the strip runs after the clone and before forwarding. `TestProxyAppendsXForwardedFor` guards the append-don't-overwrite rule by sending a prior `10.0.0.1` and expecting the `"10.0.0.1, "` prefix, and `TestProxyXForwardedHostIsSet` confirms the client's `Host` reaches the upstream as `X-Forwarded-Host`. The status classification is pinned by three tests: `TestProxy502WhenNoRouteMatches` and `TestProxy502WhenUpstreamRefuses` for 502, and `TestProxy504WhenUpstreamExceedsTimeout` for 504 — the last one only passes if the handler inspects `ctx.Err()` rather than treating every `Do` error as a timeout. The whole suite holding under `go test -race` is the bar that proves the streaming copy and the pooled client interleave without a data race.

## Resources

- [`net/http/httputil.ReverseProxy`](https://pkg.go.dev/net/http/httputil#ReverseProxy) — the standard library's reverse proxy; read it to compare its director/rewrite model against the manual handler built here.
- [`net/http.Transport`](https://pkg.go.dev/net/http#Transport) — the connection-pool, idle-timeout, and dial-timeout fields configured in `New`.
- [MDN: X-Forwarded-For](https://developer.mozilla.org/en-US/docs/Web/HTTP/Reference/Headers/X-Forwarded-For) — the chaining semantics and the security caveats of trusting this header at the edge.
- [Envoy HTTP connection manager](https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/http/http_connection_management) — a production reference for L7 HTTP proxying in a service-mesh data plane.

---

Back to [02-header-rewriting.md](02-header-rewriting.md) | Next: [mTLS Termination](../03-mtls-termination/00-concepts.md)
