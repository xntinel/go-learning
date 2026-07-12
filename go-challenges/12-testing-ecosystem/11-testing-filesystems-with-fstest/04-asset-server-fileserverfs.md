# Exercise 4: Static asset HTTP handler over fs.FS, tested with httptest

A static-asset handler is the cleanest demonstration of the `fs.FS` boundary:
`http.FileServerFS` takes an `fs.FS`, so the same handler serves an `embed.FS`
in production and a `fstest.MapFS` in tests — and you drive it through
`httptest` with no listen socket and no files on disk. This exercise builds that
handler and tests Content-Type sniffing, 404s, directory listing, and
`If-Modified-Since` conditional handling keyed off `MapFile.ModTime`.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
assetserver/                 independent module: example.com/assetserver
  go.mod                     go 1.26
  handler.go                 Handler(fs.FS) http.Handler using FileServerFS + StripPrefix
  cmd/
    demo/
      main.go                serve a MapFS through httptest and print responses
  handler_test.go            200+Content-Type, 404, listing, 304 conditional tests
```

- Files: `handler.go`, `cmd/demo/main.go`, `handler_test.go`.
- Implement: `Handler(assets fs.FS) http.Handler` that serves files under
  `/static/` via `http.FileServerFS` wrapped in `http.StripPrefix`.
- Test: `httptest.NewRecorder` assertions for 200 + body + Content-Type on an
  existing asset, 404 on a missing one, a directory listing, and 304 when
  `If-Modified-Since` matches a `MapFile.ModTime` set in the fixture.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/11-testing-filesystems-with-fstest/04-asset-server-fileserverfs/cmd/demo
cd go-solutions/12-testing-ecosystem/11-testing-filesystems-with-fstest/04-asset-server-fileserverfs
```

### What net/http gives you for free once you speak fs.FS

`http.FileServerFS(fsys)` returns a handler that serves the contents of `fsys`.
Because the URL path (`/static/app.css`) carries the `/static/` mount prefix but
the FS keys do not, you wrap it in `http.StripPrefix("/static/", ...)` so the
handler looks up `app.css`. That is the entire handler. Everything else — the
things you would otherwise write and test by hand — comes from `net/http`:

- *Content-Type* is set from the file extension via the `mime` package
  (`.css` -> `text/css; charset=utf-8`), falling back to content sniffing
  (`http.DetectContentType`) for extensionless files.
- *404* is returned for a missing file, because the FS `Open` returns a
  `*fs.PathError` wrapping `fs.ErrNotExist` and `FileServer` maps that to
  `http.StatusNotFound`.
- *Directory listing* is generated for a directory request.
- *Conditional requests* — `If-Modified-Since` and `If-None-Match` — are handled
  by `http.ServeContent` underneath, which compares the request's
  `If-Modified-Since` time against the file's `ModTime` and returns
  `304 Not Modified` (no body) when the client's copy is current.

That last point is where `MapFile.ModTime` earns its place. In production the
`ModTime` comes from the real file's mtime; in a test you *set* it to a fixed
instant in the fixture, then send an `If-Modified-Since` header equal to it and
assert a `304`. No real file, no real clock — the time dimension is an explicit
value. (One requirement: the served files must implement `io.Seeker`, because
`ServeContent` seeks to support range requests. `MapFS` files do.)

Create `handler.go`:

```go
package assetserver

import (
	"io/fs"
	"net/http"
)

// Handler serves the files in assets under the /static/ URL prefix. In
// production assets is an embed.FS; in tests it is a fstest.MapFS. The handler
// code is identical either way.
func Handler(assets fs.FS) http.Handler {
	return http.StripPrefix("/static/", http.FileServerFS(assets))
}
```

### The runnable demo

The demo mounts a `MapFS` and drives the handler with `httptest` in-process,
printing the status and Content-Type for an existing asset and the status for a
missing one — no socket, no disk.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http/httptest"
	"testing/fstest"

	"example.com/assetserver"
)

func main() {
	assets := fstest.MapFS{
		"app.css":    {Data: []byte("body{margin:0}")},
		"index.html": {Data: []byte("<h1>home</h1>")},
	}
	h := assetserver.Handler(assets)

	for _, path := range []string{"/static/app.css", "/static/missing.css"} {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		fmt.Printf("%s -> %d %s\n", path, rec.Code, rec.Header().Get("Content-Type"))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
/static/app.css -> 200 text/css; charset=utf-8
/static/missing.css -> 404 text/plain; charset=utf-8
```

### Tests

Each test builds a `MapFS`, wraps it in the handler, and drives it with
`httptest.NewRecorder`. `TestServesAsset` asserts 200, the body, and the
`text/css` Content-Type. `TestMissingIs404` asserts 404. `TestDirectoryListing`
requests the root and asserts the generated listing contains the filenames.
`TestConditionalNotModified` sets a fixed `ModTime` in the fixture, sends
`If-Modified-Since` equal to it, and asserts a `304` with an empty body — the
conditional-GET path made deterministic by a chosen `ModTime`.

Create `handler_test.go`:

```go
package assetserver

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

var fixedTime = time.Date(2024, 1, 2, 15, 4, 5, 0, time.UTC)

func testFS() fstest.MapFS {
	return fstest.MapFS{
		"app.css":    {Data: []byte("body{margin:0}"), ModTime: fixedTime},
		"index.html": {Data: []byte("<h1>home</h1>"), ModTime: fixedTime},
	}
}

func do(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestServesAsset(t *testing.T) {
	t.Parallel()

	rec := do(Handler(testFS()), httptest.NewRequest("GET", "/static/app.css", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "body{margin:0}" {
		t.Fatalf("body = %q, want the css", got)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/css") {
		t.Fatalf("Content-Type = %q, want text/css...", ct)
	}
}

func TestMissingIs404(t *testing.T) {
	t.Parallel()

	rec := do(Handler(testFS()), httptest.NewRequest("GET", "/static/nope.css", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestDirectoryListing(t *testing.T) {
	t.Parallel()

	rec := do(Handler(testFS()), httptest.NewRequest("GET", "/static/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "app.css") {
		t.Fatalf("listing missing app.css: %q", body)
	}
}

func TestConditionalNotModified(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest("GET", "/static/app.css", nil)
	req.Header.Set("If-Modified-Since", fixedTime.UTC().Format(http.TimeFormat))

	rec := do(Handler(testFS()), req)
	if rec.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", rec.Code)
	}
	if body := rec.Body.String(); body != "" {
		t.Fatalf("304 body = %q, want empty", body)
	}
}
```

## Review

The handler is one line — `StripPrefix` over `FileServerFS` — and that is the
lesson: once your code depends on `fs.FS`, `net/http` supplies Content-Type
sniffing, 404 mapping, directory listing, and conditional-request handling, and
you test all of it through `httptest` against a `MapFS` with no socket and no
temp files. The `304` test is the one that would be painful the old way: it
needs a file with a *known* mtime, which `MapFile.ModTime` provides exactly.
Assert Content-Type with `HasPrefix` rather than a full equality so a platform
mime table that appends parameters differently does not make the test brittle.

## Resources

- [`http.FileServerFS`](https://pkg.go.dev/net/http#FileServerFS) — serve any `fs.FS` over HTTP (Go 1.22+).
- [`http.ServeContent`](https://pkg.go.dev/net/http#ServeContent) — the conditional-request and range engine underneath the file server.
- [`net/http/httptest`](https://pkg.go.dev/net/http/httptest) — `NewRequest`/`NewRecorder` for socketless handler tests.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-migration-runner-walkdir.md](03-migration-runner-walkdir.md) | Next: [05-fs-sub-tenant-scoping.md](05-fs-sub-tenant-scoping.md)
