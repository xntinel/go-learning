# Exercise 1: HTTP Handler Backed By A Private internal/render Helper

The most common real use of `internal` at depth is a package that owns a helper no
one else should touch. Here an `http.Handler` is the public API; its
response-shaping helper lives in `pkg/handler/internal/render`, so the presentation
logic stays private to the handler package while the handler itself is what callers
import.

This module is fully self-contained: its own module, its own demo, its own tests.
Nothing here imports any other exercise.

## What you'll build

```text
pkginternal/                        module example.com/pkginternal
  go.mod
  pkg/handler/handler.go            type Handler; New, ServeHTTP (public API)
  pkg/handler/internal/render/
    render.go                       type Page; Write (private to pkg/handler subtree)
  pkg/handler/handler_test.go       white-box test: ServeHTTP via httptest + render.Write
  cmd/demo/main.go                  runnable demo hitting ServeHTTP through httptest
```

- Files: `pkg/handler/internal/render/render.go`, `pkg/handler/handler.go`, `pkg/handler/handler_test.go`, `cmd/demo/main.go`.
- Implement: a `render.Write(w, Page)` helper that validates and writes HTML, and a `handler.Handler` whose `ServeHTTP` renders a page through it.
- Test: drive `ServeHTTP` with `httptest.NewRecorder`/`NewRequest`, assert body and `Content-Type`; call `render.Write` directly (legal from the same package) to assert an empty title is rejected.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/pkginternal/pkg/handler/internal/render ~/go-exercises/pkginternal/cmd/demo
cd ~/go-exercises/pkginternal
go mod init example.com/pkginternal
```

### Why the render helper belongs under internal

The handler's job is to accept a request and produce a response; the HTML shaping
is an implementation detail of that job. If the shaping lived in a plain sibling
directory like `pkg/handler/render`, any other package — in this module or a
downstream one — could import it and start depending on its exact function
signature and output format. The day you want to switch to a template engine, or
change the page structure, you would be making a breaking change for importers you
never intended to have.

Placing it at `pkg/handler/internal/render` sets the allow-list to exactly
`pkg/handler` and its subtree. The parent of that `internal` directory is
`pkg/handler`, so only `pkg/handler` (and anything nested under it) may import
`render`. A sibling like `pkg/other` cannot, and neither can any external module.
The helper is free to change shape forever because its only legal caller is the one
package that owns it.

Note the composition with unexported identifiers: `render` is a package boundary,
but inside it we still export `Write` and `Page` — because from `render`'s point of
view its only legal importer is `handler`, and `handler` needs to call `Write`.
The `internal` directory already restricts WHO can import; exporting within it just
decides what that one legal importer sees.

Create `pkg/handler/internal/render/render.go`. `Write` validates the page, sets
the content type, and streams the HTML; an empty title is a caller error returned
as a sentinel wrapped with `%w`:

```go
package render

import (
	"errors"
	"fmt"
	"net/http"
)

// ErrEmptyTitle is returned by Write when a Page has no title. It is a sentinel
// so callers can match it with errors.Is.
var ErrEmptyTitle = errors.New("render: empty title")

// Page is the minimal data a rendered HTML response needs.
type Page struct {
	Title string
	Body  string
}

// Write validates p and streams an HTML document to w. It returns ErrEmptyTitle
// (wrapped) if the title is empty, and otherwise the first write error, if any.
func Write(w http.ResponseWriter, p Page) error {
	if p.Title == "" {
		return fmt.Errorf("write page: %w", ErrEmptyTitle)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, err := fmt.Fprintf(w, "<!doctype html><title>%s</title><body>%s</body>", p.Title, p.Body)
	return err
}
```

Create `pkg/handler/handler.go`. The handler imports the internal helper by its
full path; this compiles precisely because `handler` sits at the parent of the
`internal` directory:

```go
package handler

import (
	"net/http"

	"example.com/pkginternal/pkg/handler/internal/render"
)

// Handler renders a titled HTML page that greets the request path.
type Handler struct {
	title string
}

// New returns a Handler that stamps every page with the given title.
func New(title string) *Handler {
	return &Handler{title: title}
}

// ServeHTTP renders the page for the request path. A bad page (empty title)
// becomes a 500; a good one is written with a text/html content type.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := "world"
	if len(r.URL.Path) > 1 {
		name = r.URL.Path[1:]
	}
	if err := render.Write(w, render.Page{Title: h.title, Body: "Hello, " + name}); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}
```

### The runnable demo

The demo wires the handler behind an in-process `httptest.Server` and issues a real
HTTP request, so you can see the full path from request to rendered body without
binding a public port. `httptest.NewServer` picks a free loopback port and gives
back a client-ready URL.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"

	"example.com/pkginternal/pkg/handler"
)

func main() {
	srv := httptest.NewServer(handler.New("myapp"))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/gopher")
	if err != nil {
		fmt.Println("request failed:", err)
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	fmt.Println("Content-Type:", resp.Header.Get("Content-Type"))
	fmt.Println("Body:", string(body))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
Content-Type: text/html; charset=utf-8
Body: <!doctype html><title>myapp</title><body>Hello, gopher</body>
```

### Tests

The test file is `package handler` — a white-box test — which is exactly why it may
import `render`: a same-package test sits at `pkg/handler`, inside the allow-list of
`pkg/handler/internal/render`. `TestServeHTTP` drives the public API through
`httptest`; `TestRenderRejectsEmptyTitle` reaches straight into the helper to prove
the validation branch, matching the sentinel with `errors.Is` rather than comparing
strings.

Create `pkg/handler/handler_test.go`:

```go
package handler

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"example.com/pkginternal/pkg/handler/internal/render"
)

func TestServeHTTP(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		path     string
		wantBody string
	}{
		{"named path", "/gopher", "Hello, gopher"},
		{"nested path", "/team/ops", "Hello, team/ops"},
		{"root path", "/", "Hello, world"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)

			New("myapp").ServeHTTP(rec, req)

			res := rec.Result()
			if got := res.Header.Get("Content-Type"); got != "text/html; charset=utf-8" {
				t.Errorf("Content-Type = %q, want text/html; charset=utf-8", got)
			}
			if body := rec.Body.String(); !strings.Contains(body, "<title>myapp</title>") {
				t.Errorf("body = %q, want it to contain <title>myapp</title>", body)
			}
			if body := rec.Body.String(); !strings.Contains(body, tc.wantBody) {
				t.Errorf("body = %q, want it to contain %q", body, tc.wantBody)
			}
		})
	}
}

func TestRenderRejectsEmptyTitle(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	err := render.Write(rec, render.Page{Title: "", Body: "x"})
	if !errors.Is(err, render.ErrEmptyTitle) {
		t.Fatalf("Write with empty title: err = %v, want ErrEmptyTitle", err)
	}
}
```

## Review

The handler is correct when its only dependency on presentation flows through the
`internal/render` package: the public surface is `New` and `ServeHTTP`, and every
byte of HTML is produced by `render.Write`, which validates before it writes. The
boundary proof is structural — the test can import `render` because it is
`package handler`, and a hypothetical `pkg/other` could not, which is the point of
the next exercise.

The traps here are the two placement mistakes. Do not move `render` up to
`pkg/handler/render` "so it can be reused": that publishes the helper to the whole
module and any downstream importer, and you lose the freedom to change its output.
Do not push the handler itself under `internal` either — it is the entry point
callers must import. Keep the `%w`-wrapped sentinel so `TestRenderRejectsEmptyTitle`
asserts the failure with `errors.Is`, not a brittle string match, and run
`go test -race` to confirm the recorder path is clean.

## Resources

- [Go Modules Reference: Internal packages](https://go.dev/ref/mod#internal-packages) — the exact importability rule.
- [`net/http/httptest`](https://pkg.go.dev/net/http/httptest) — `NewRecorder`, `NewRequest`, and `NewServer` for driving handlers.
- [`net/http.Handler`](https://pkg.go.dev/net/http#Handler) — the `ServeHTTP` contract.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-prove-internal-rule-in-ci.md](02-prove-internal-rule-in-ci.md)
