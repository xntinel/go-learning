# Exercise 4: Serve the GOPROXY protocol read-only from a local module store

An air-gapped build farm needs an internal proxy: a read-only server that answers
the GOPROXY protocol from a filesystem module store, with the correct content
types and — critically — the correct 404 so a real client can fail over. This
exercise builds that server and drives it with `httptest`.

## What you'll build

```text
miniproxy/                 independent module: example.com/miniproxy
  go.mod                   go 1.26 (requires golang.org/x/mod for semver sort)
  server.go                type Server; @v/list, .info, .mod, .zip, @latest
  cmd/
    demo/
      main.go              builds a store, serves it, GETs each endpoint
  server_test.go           httptest table: status, Content-Type, body, 404
  example_test.go          Example serving @v/list with // Output
```

- Files: `server.go`, `cmd/demo/main.go`, `server_test.go`, `example_test.go`.
- Implement: a `Server` over a `root/<module>/<version>.<ext>` store answering `@v/list` (sorted text), `@v/$version.info` (JSON), `@v/$version.mod` (text/plain), `@v/$version.zip` (application/zip), and `@latest`, returning 404 for anything absent.
- Test: `httptest.Server` table cases asserting status, `Content-Type`, and body; an unknown version returns 404 so a comma-chain client falls through.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
go get golang.org/x/mod/semver
```

### The protocol is just static GETs with the right headers and status codes

Because the GOPROXY protocol has no query parameters, a proxy is essentially a
static file server that knows five URL shapes. The store here is keyed by the
on-wire module path — `root/<module>/<version>.<ext>` — so a request for
`/example.com/widgets/@v/v1.2.0.info` maps directly to a file on disk. The routing
cannot use a `ServeMux` wildcard because a `{module...}` catch-all must be the
final path segment and the protocol puts a literal `@v/...` after the module path;
so the handler splits the request path on `/@v/` and `/@latest` itself.

Two details make this a real proxy rather than a toy. First, the content types
must be exact: `.info` is `application/json`, `.mod` is `text/plain`, `.zip` is
`application/zip`. `http.ServeContent` respects a `Content-Type` we set before
calling it, so we set it explicitly per extension rather than letting it sniff.
Second, and more important for a chain, a missing version must return 404 (or
410). That is precisely the signal a comma-separated `GOPROXY` chain needs to fall
through to the next proxy; a proxy that answered 500 for an unknown module would
wrongly hard-stop every client's chain. `@v/list` returns the versions sorted with
`semver.Compare` so `v1.2.0` correctly precedes `v1.10.0` — a lexical sort would
get that backwards. `@latest` serves the newest version's `.info`.

Create `server.go`:

```go
// Package miniproxy serves the read-only GOPROXY protocol subset over a
// filesystem module store: @v/list, @v/$version.info, @v/$version.mod,
// @v/$version.zip, and @latest.
package miniproxy

import (
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/mod/semver"
)

// Server answers GOPROXY protocol requests from a directory tree. The store is
// keyed by the on-wire (case-encoded) module path: root/<module>/<version>.<ext>.
type Server struct {
	root string
}

// NewServer returns a Server backed by the module store rooted at dir.
func NewServer(dir string) *Server {
	return &Server{root: dir}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/")

	if module, ok := strings.CutSuffix(path, "/@latest"); ok {
		s.serveLatest(w, r, module)
		return
	}
	if i := strings.Index(path, "/@v/"); i >= 0 {
		module := path[:i]
		rest := path[i+len("/@v/"):]
		if rest == "list" {
			s.serveList(w, r, module)
			return
		}
		s.serveFile(w, r, module, rest)
		return
	}
	http.NotFound(w, r)
}

// versions returns the sorted list of versions in a module directory, newest
// last, by scanning for .info files.
func (s *Server) versions(module string) ([]string, error) {
	dir := filepath.Join(s.root, filepath.FromSlash(module))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var vs []string
	for _, e := range entries {
		if v, ok := strings.CutSuffix(e.Name(), ".info"); ok {
			vs = append(vs, v)
		}
	}
	slices.SortFunc(vs, semver.Compare)
	return vs, nil
}

func (s *Server) serveList(w http.ResponseWriter, r *http.Request, module string) {
	vs, err := s.versions(module)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=UTF-8")
	for _, v := range vs {
		if _, err := w.Write([]byte(v + "\n")); err != nil {
			return
		}
	}
}

func (s *Server) serveLatest(w http.ResponseWriter, r *http.Request, module string) {
	vs, err := s.versions(module)
	if err != nil || len(vs) == 0 {
		http.NotFound(w, r)
		return
	}
	latest := vs[len(vs)-1]
	s.serveFile(w, r, module, latest+".info")
}

func (s *Server) serveFile(w http.ResponseWriter, r *http.Request, module, name string) {
	switch {
	case strings.HasSuffix(name, ".info"):
		w.Header().Set("Content-Type", "application/json")
	case strings.HasSuffix(name, ".mod"):
		w.Header().Set("Content-Type", "text/plain; charset=UTF-8")
	case strings.HasSuffix(name, ".zip"):
		w.Header().Set("Content-Type", "application/zip")
	default:
		http.NotFound(w, r)
		return
	}
	full := filepath.Join(s.root, filepath.FromSlash(module), name)
	f, err := os.Open(full)
	if err != nil {
		// A missing version returns 404 so a comma-chain client falls through.
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	http.ServeContent(w, r, name, fi.ModTime(), f)
}
```

### The runnable demo

The demo builds a store with two versions deliberately out of order on disk
(`v1.2.0` and `v1.10.0`), serves it with `httptest`, and shows the sorted list,
the `@latest` selection, the zip's content type, and the 404 for an unknown
version.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"

	"example.com/miniproxy"
)

func main() {
	root, err := os.MkdirTemp("", "modstore")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(root)

	mod := "example.com/widgets"
	dir := filepath.Join(root, filepath.FromSlash(mod))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		panic(err)
	}
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			panic(err)
		}
	}
	// Two published versions, deliberately out of order on disk.
	write("v1.2.0.info", `{"Version":"v1.2.0","Time":"2024-03-01T00:00:00Z"}`)
	write("v1.2.0.mod", "module example.com/widgets\n\ngo 1.26\n")
	write("v1.2.0.zip", "PK-fake-zip-bytes-v1.2.0")
	write("v1.10.0.info", `{"Version":"v1.10.0","Time":"2024-09-01T00:00:00Z"}`)
	write("v1.10.0.mod", "module example.com/widgets\n\ngo 1.26\n")
	write("v1.10.0.zip", "PK-fake-zip-bytes-v1.10.0")

	srv := httptest.NewServer(miniproxy.NewServer(root))
	defer srv.Close()

	get := func(p string) (int, string, string) {
		resp, err := http.Get(srv.URL + p)
		if err != nil {
			panic(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header.Get("Content-Type"), string(b)
	}

	code, ct, body := get("/example.com/widgets/@v/list")
	fmt.Printf("list -> %d %s\n%s", code, ct, body)

	code, ct, body = get("/example.com/widgets/@latest")
	fmt.Printf("latest -> %d %s\n%s\n", code, ct, body)

	code, ct, _ = get("/example.com/widgets/@v/v1.10.0.zip")
	fmt.Printf("zip -> %d %s\n", code, ct)

	code, ct, _ = get("/example.com/widgets/@v/v9.9.9.info")
	fmt.Printf("unknown -> %d %s\n", code, ct)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
list -> 200 text/plain; charset=UTF-8
v1.2.0
v1.10.0
latest -> 200 application/json
{"Version":"v1.10.0","Time":"2024-09-01T00:00:00Z"}
zip -> 200 application/zip
unknown -> 404 text/plain; charset=utf-8
```

### Tests

The table exercises every endpoint against a fixture store built in `t.TempDir()`,
asserting status, `Content-Type`, and body. The load-bearing rows are the sorted
`@v/list` (proving the semver sort), the exact content types, and the two 404s —
an unknown version and an unknown module — that a real client depends on for
failover.

Create `server_test.go`:

```go
package miniproxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newStore builds a module store fixture and returns its root directory.
func newStore(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, filepath.FromSlash("example.com/widgets"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"v1.2.0.info":  `{"Version":"v1.2.0","Time":"2024-03-01T00:00:00Z"}`,
		"v1.2.0.mod":   "module example.com/widgets\n\ngo 1.26\n",
		"v1.2.0.zip":   "PK-fake-zip-bytes-v1.2.0",
		"v1.10.0.info": `{"Version":"v1.10.0","Time":"2024-09-01T00:00:00Z"}`,
		"v1.10.0.mod":  "module example.com/widgets\n\ngo 1.26\n",
		"v1.10.0.zip":  "PK-fake-zip-bytes-v1.10.0",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func doGet(t *testing.T, srv *httptest.Server, path string) (*http.Response, string) {
	t.Helper()
	resp, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(body)
}

func TestServer(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(NewServer(newStore(t)))
	t.Cleanup(srv.Close)

	tests := []struct {
		name        string
		path        string
		wantStatus  int
		wantCT      string
		wantBody    string // exact, when non-empty
		wantContain string // substring, when non-empty
	}{
		{
			name:       "list returns sorted versions",
			path:       "/example.com/widgets/@v/list",
			wantStatus: http.StatusOK,
			wantCT:     "text/plain; charset=UTF-8",
			wantBody:   "v1.2.0\nv1.10.0\n",
		},
		{
			name:        "info returns application/json",
			path:        "/example.com/widgets/@v/v1.2.0.info",
			wantStatus:  http.StatusOK,
			wantCT:      "application/json",
			wantContain: `"Version":"v1.2.0"`,
		},
		{
			name:       "mod returns text/plain",
			path:       "/example.com/widgets/@v/v1.2.0.mod",
			wantStatus: http.StatusOK,
			wantCT:     "text/plain; charset=UTF-8",
			wantBody:   "module example.com/widgets\n\ngo 1.26\n",
		},
		{
			name:       "zip returns application/zip with the right bytes",
			path:       "/example.com/widgets/@v/v1.10.0.zip",
			wantStatus: http.StatusOK,
			wantCT:     "application/zip",
			wantBody:   "PK-fake-zip-bytes-v1.10.0",
		},
		{
			name:        "latest returns newest version",
			path:        "/example.com/widgets/@latest",
			wantStatus:  http.StatusOK,
			wantCT:      "application/json",
			wantContain: `"Version":"v1.10.0"`,
		},
		{
			name:       "unknown version is 404 so a comma-chain client falls through",
			path:       "/example.com/widgets/@v/v9.9.9.info",
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "unknown module is 404",
			path:       "/example.com/nope/@v/list",
			wantStatus: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, body := doGet(t, srv, tt.path)
			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d; want %d", resp.StatusCode, tt.wantStatus)
			}
			if tt.wantCT != "" && resp.Header.Get("Content-Type") != tt.wantCT {
				t.Errorf("Content-Type = %q; want %q", resp.Header.Get("Content-Type"), tt.wantCT)
			}
			if tt.wantBody != "" && body != tt.wantBody {
				t.Errorf("body = %q; want %q", body, tt.wantBody)
			}
			if tt.wantContain != "" && !strings.Contains(body, tt.wantContain) {
				t.Errorf("body %q does not contain %q", body, tt.wantContain)
			}
		})
	}
}
```

Create `example_test.go`:

```go
package miniproxy_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"

	"example.com/miniproxy"
)

func Example() {
	root, _ := os.MkdirTemp("", "modstore")
	defer os.RemoveAll(root)
	dir := filepath.Join(root, filepath.FromSlash("example.com/widgets"))
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "v1.0.0.info"), []byte(`{"Version":"v1.0.0","Time":"2024-01-01T00:00:00Z"}`), 0o644)

	srv := httptest.NewServer(miniproxy.NewServer(root))
	defer srv.Close()

	resp, _ := http.Get(srv.URL + "/example.com/widgets/@v/list")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	fmt.Print(string(body))
	// Output: v1.0.0
}
```

## Review

The server is correct when each endpoint returns the exact content type and a
missing artifact returns 404, not 500 — the 404 is the entire contract that lets a
comma chain fail over, so the two 404 rows are the most important tests here. The
semver-aware `@v/list` sort is the other trap: a lexical `slices.Sort` would order
`v1.10.0` before `v1.2.0`, which is wrong; `semver.Compare` fixes it. Keep the
store read-only — the server never writes — so it models a mirror that a build
farm can trust. Confirm the whole surface with `go test -race`; the `httptest`
server exercises the real HTTP stack, not a mock.

## Resources

- [Go Modules Reference: GOPROXY protocol](https://go.dev/ref/mod#goproxy-protocol) — the endpoints, content types, and the 404/410 requirement.
- [`net/http/httptest`](https://pkg.go.dev/net/http/httptest) — `NewServer` for driving the handler over a real socket.
- [`http.ServeContent`](https://pkg.go.dev/net/http#ServeContent) — serving a file body while honoring a preset `Content-Type`.
- [`golang.org/x/mod/semver`](https://pkg.go.dev/golang.org/x/mod/semver) — `Compare` for correct version ordering.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [03-proxy-path-codec.md](03-proxy-path-codec.md) | Next: [05-goprivate-pattern-matcher.md](05-goprivate-pattern-matcher.md)
