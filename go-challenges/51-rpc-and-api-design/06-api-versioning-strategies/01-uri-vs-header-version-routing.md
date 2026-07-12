# Exercise 1: URI-Path vs Media-Type Version Routing

The two most common client-facing versioning axes are URI-path (`/v1/orders`)
and media-type content negotiation (`Accept: application/vnd.acme.v2+json`). This
exercise serves the same resource under both at once, so the trade-offs are
visible in code rather than in the abstract.

This module is fully self-contained. It has its own `go mod init`, all the code
it needs inline, and its own demo and tests. Nothing here imports another
exercise.

## What you'll build

```text
versionrouting/                independent module: example.com/versionrouting
  go.mod                       go 1.24
  versionrouting.go            resolveVersion, negotiateVersion middleware, NewServer
  cmd/
    demo/
      main.go                  drives both axes over an httptest.Server
  versionrouting_test.go       table-driven recorder tests + Example
```

- Files: `versionrouting.go`, `cmd/demo/main.go`, `versionrouting_test.go`.
- Implement: a `resolveVersion` that parses `Accept` and falls back to a default, a `negotiateVersion` middleware that carries the resolved version in context, and a `NewServer` that exposes `/v1/orders`, `/v2/orders`, and a negotiated `/orders`.
- Test: `/v1` and `/v2` hit distinct handlers; vendor `Accept` values route to the matching version; blank, wildcard, and malformed `Accept` fall back rather than 500; the negotiated route echoes the version and sends `Vary: Accept`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/51-rpc-and-api-design/06-api-versioning-strategies/01-uri-vs-header-version-routing/cmd/demo
cd go-solutions/51-rpc-and-api-design/06-api-versioning-strategies/01-uri-vs-header-version-routing
```

### Why both axes, and where the version lives

URI-path versioning is trivial to route in Go 1.22+: a `ServeMux` pattern like
`"GET /v1/orders"` matches method and path together, so `/v1/orders` and
`/v2/orders` are simply two registered handlers. The version is right there in the
URL, which makes it curl-friendly, log-visible, and — because it is part of the
cache key — safe with any shared cache. The downside is conceptual: the same order
now has two URLs, and clients must rewrite URLs to move between versions.

Media-type versioning keeps a single URL, `/orders`, and versions the
*representation*. The client signals which shape it wants through the `Accept`
header, using a vendor-tree media type: `application/vnd.acme.v2+json`. The server
parses that header, extracts the version token, and renders accordingly. The URL
never changes, but the version is now invisible to a browser and to most logs, and
the response body varies by a request header — which is why the negotiated handler
must send `Vary: Accept` so a shared cache keys on it.

### Parsing Accept without trusting it

The header is attacker-influenced and client-buggy, so the resolver must never
500 on a bad value. `resolveVersion` splits the `Accept` header on commas (a
client may send several media ranges), and for each part calls
`mime.ParseMediaType`, which validates the syntax and lowercases the type. A value
`mime.ParseMediaType` rejects (for example `@@@garbage`, which has no slash) yields
an error, and the resolver simply skips that part. A well-formed but non-vendor
type such as `*/*` or `application/json` parses fine but carries no version, so
`versionFromMediaType` reports `false` and the resolver keeps looking. If nothing
resolves — blank header, wildcard, malformed, or a vendor version this service
does not serve, like `application/vnd.acme.v9+json` — it returns `DefaultVersion`.
Falling back rather than erroring is the whole point: an unparseable `Accept`
should degrade to a sane default, not take down the request.

Note one subtlety `mime.ParseMediaType` forces on you: it lowercases the subtype,
so `application/vnd.acme.vX+json` becomes `...vx+json`. The known-versions set is
lowercase (`v1`, `v2`), so an uppercase or unknown token lands in the fallback
path exactly as a garbage value would.

Create `versionrouting.go`:

```go
package versionrouting

import (
	"context"
	"io"
	"mime"
	"net/http"
	"strings"
)

// DefaultVersion is served when the client expresses no parseable preference.
const DefaultVersion = "v1"

type ctxKey int

const versionKey ctxKey = 0

// vendorPrefix and vendorSuffix bracket the version token inside a vendor-tree
// media type such as application/vnd.acme.v2+json.
const (
	vendorPrefix = "application/vnd.acme."
	vendorSuffix = "+json"
)

// knownVersions is the set of representations this service can produce.
var knownVersions = map[string]bool{"v1": true, "v2": true}

// versionFromMediaType extracts the vendor-tree version token from an already
// parsed media type. It reports false when the media type is not in the vendor
// tree or names a version this service does not serve.
func versionFromMediaType(mt string) (string, bool) {
	if !strings.HasPrefix(mt, vendorPrefix) || !strings.HasSuffix(mt, vendorSuffix) {
		return "", false
	}
	v := mt[len(vendorPrefix) : len(mt)-len(vendorSuffix)]
	if !knownVersions[v] {
		return "", false
	}
	return v, true
}

// resolveVersion parses an Accept header value and returns the negotiated
// version. A blank header, a wildcard, an unknown vendor version, or a value
// that mime.ParseMediaType rejects all fall back to DefaultVersion rather than
// failing the request.
func resolveVersion(accept string) string {
	if strings.TrimSpace(accept) == "" {
		return DefaultVersion
	}
	for _, part := range strings.Split(accept, ",") {
		mt, _, err := mime.ParseMediaType(strings.TrimSpace(part))
		if err != nil {
			continue
		}
		if v, ok := versionFromMediaType(mt); ok {
			return v
		}
	}
	return DefaultVersion
}

// negotiateVersion resolves the version from the Accept header and stores it in
// the request context for the downstream handler.
func negotiateVersion(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v := resolveVersion(r.Header.Get("Accept"))
		ctx := context.WithValue(r.Context(), versionKey, v)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// writeOrder renders the order resource in the requested representation and
// echoes the negotiated version so a client can confirm what it received.
func writeOrder(w http.ResponseWriter, version string) {
	w.Header().Set("Content-Type", vendorPrefix+version+vendorSuffix)
	w.Header().Set("X-API-Version", version)
	switch version {
	case "v2":
		io.WriteString(w, `{"id":"ord-42","total_cents":1999,"currency":"USD"}`)
	default:
		io.WriteString(w, `{"id":"ord-42","total":19.99}`)
	}
}

// ordersHandler serves a fixed version, used by the URI-path routes.
func ordersHandler(version string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeOrder(w, version)
	}
}

// negotiatedOrders serves whichever version the middleware resolved from the
// Accept header. Because the body varies by Accept, it must advertise Vary so
// shared caches key on the header.
func negotiatedOrders(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Vary", "Accept")
	v, _ := r.Context().Value(versionKey).(string)
	if v == "" {
		v = DefaultVersion
	}
	writeOrder(w, v)
}

// NewServer wires both versioning axes: URI-path routes (/v1/orders,
// /v2/orders) and a single media-type-negotiated route (/orders).
func NewServer() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /v1/orders", ordersHandler("v1"))
	mux.Handle("GET /v2/orders", ordersHandler("v2"))
	mux.Handle("GET /orders", negotiateVersion(http.HandlerFunc(negotiatedOrders)))
	return mux
}
```

### The runnable demo

The demo stands up the server on an `httptest.Server` and issues real HTTP
requests across both axes, printing the version each response reports through its
`X-API-Version` header.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"

	"example.com/versionrouting"
)

func main() {
	srv := httptest.NewServer(versionrouting.NewServer())
	defer srv.Close()

	do := func(label, path, accept string) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			fmt.Println(err)
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		fmt.Printf("%-28s version=%s | %s\n", label, resp.Header.Get("X-API-Version"), body)
	}

	do("GET /v1/orders", "/v1/orders", "")
	do("GET /v2/orders", "/v2/orders", "")
	do("GET /orders (Accept v2)", "/orders", "application/vnd.acme.v2+json")
	do("GET /orders (Accept v1)", "/orders", "application/vnd.acme.v1+json")
	do("GET /orders (no Accept)", "/orders", "")
	do("GET /orders (Accept */*)", "/orders", "*/*")
	do("GET /orders (unknown ver)", "/orders", "application/vnd.acme.vX+json")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
GET /v1/orders               version=v1 | {"id":"ord-42","total":19.99}
GET /v2/orders               version=v2 | {"id":"ord-42","total_cents":1999,"currency":"USD"}
GET /orders (Accept v2)      version=v2 | {"id":"ord-42","total_cents":1999,"currency":"USD"}
GET /orders (Accept v1)      version=v1 | {"id":"ord-42","total":19.99}
GET /orders (no Accept)      version=v1 | {"id":"ord-42","total":19.99}
GET /orders (Accept */*)     version=v1 | {"id":"ord-42","total":19.99}
GET /orders (unknown ver)    version=v1 | {"id":"ord-42","total":19.99}
```

### Tests

The tests are pure recorder tests — no network, no ports. `TestResolveVersion`
tables the header-parsing logic directly, including the fallback cases that must
not error. `TestURIPathRouting` confirms the two path routes hit distinct
handlers, and `TestMediaTypeRouting` confirms the negotiated route dispatches on
`Accept` and always sets `Vary: Accept`. The `Example` pins the negotiated
`Content-Type` echo.

Create `versionrouting_test.go`:

```go
package versionrouting

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestResolveVersion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		accept string
		want   string
	}{
		{"blank falls back", "", DefaultVersion},
		{"wildcard falls back", "*/*", DefaultVersion},
		{"plain json falls back", "application/json", DefaultVersion},
		{"vendor v1", "application/vnd.acme.v1+json", "v1"},
		{"vendor v2", "application/vnd.acme.v2+json", "v2"},
		{"unknown vendor version falls back", "application/vnd.acme.v9+json", DefaultVersion},
		{"malformed media type falls back", "@@@garbage", DefaultVersion},
		{"first parseable vendor wins", "text/html, application/vnd.acme.v2+json", "v2"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := resolveVersion(tc.accept); got != tc.want {
				t.Fatalf("resolveVersion(%q) = %q; want %q", tc.accept, got, tc.want)
			}
		})
	}
}

func TestURIPathRouting(t *testing.T) {
	t.Parallel()
	srv := NewServer()
	tests := []struct {
		path string
		want string
	}{
		{"/v1/orders", "v1"},
		{"/v2/orders", "v2"},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)
			if got := rec.Header().Get("X-API-Version"); got != tc.want {
				t.Fatalf("path %s: X-API-Version = %q; want %q", tc.path, got, tc.want)
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("path %s: status = %d; want 200", tc.path, rec.Code)
			}
		})
	}
}

func TestMediaTypeRouting(t *testing.T) {
	t.Parallel()
	srv := NewServer()
	tests := []struct {
		name   string
		accept string
		want   string
	}{
		{"accept v2", "application/vnd.acme.v2+json", "v2"},
		{"accept v1", "application/vnd.acme.v1+json", "v1"},
		{"blank falls back", "", DefaultVersion},
		{"unknown vendor version falls back", "application/vnd.acme.vX+json", DefaultVersion},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodGet, "/orders", nil)
			if tc.accept != "" {
				req.Header.Set("Accept", tc.accept)
			}
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)
			if got := rec.Header().Get("X-API-Version"); got != tc.want {
				t.Fatalf("accept %q: X-API-Version = %q; want %q", tc.accept, got, tc.want)
			}
			if got := rec.Header().Get("Vary"); got != "Accept" {
				t.Fatalf("accept %q: Vary = %q; want %q", tc.accept, got, "Accept")
			}
		})
	}
}

func ExampleNewServer() {
	srv := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/orders", nil)
	req.Header.Set("Accept", "application/vnd.acme.v2+json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	fmt.Println(rec.Header().Get("X-API-Version"))
	fmt.Println(rec.Header().Get("Content-Type"))
	// Output:
	// v2
	// application/vnd.acme.v2+json
}
```

## Review

The routing is correct when `resolveVersion` is a total function: every input,
including malformed and unknown ones, returns a version this service can serve,
and only a real vendor match diverts from `DefaultVersion`. The most common bug is
to let a `mime.ParseMediaType` error escape as a 500 instead of falling back — the
table's `@@@garbage` row (no slash, so `ParseMediaType` errors) exists precisely to
catch that, while the `application/vnd.acme.vX+json` row covers the other fallback
path: a syntactically valid media type whose version token is one this service does
not serve, so `knownVersions` rejects it. The second is forgetting `Vary: Accept` on the negotiated route, which
the recorder test asserts on every case; without it a CDN will cache one version's
body and serve it to the other version's clients. Run `go test -race` to confirm
the context-carried version has no data race across concurrent requests.

## Resources

- [`net/http.ServeMux`](https://pkg.go.dev/net/http#ServeMux) — Go 1.22 method-and-path patterns such as `"GET /v1/orders"`.
- [`mime.ParseMediaType`](https://pkg.go.dev/mime#ParseMediaType) — parsing and validating a media type from an `Accept` value.
- [MDN: `Vary`](https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Vary) — why content negotiation by header requires `Vary`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-deprecation-sunset-lifecycle.md](02-deprecation-sunset-lifecycle.md)
