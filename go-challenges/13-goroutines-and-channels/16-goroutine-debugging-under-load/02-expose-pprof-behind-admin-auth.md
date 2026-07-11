# Exercise 2: Wire net/http/pprof onto a private admin mux gated by a bearer token

`net/http/pprof` is the live window into a running service, but importing it the
usual way silently publishes that window on `http.DefaultServeMux`. This exercise
builds the production-correct surface: the pprof handlers registered *explicitly*
on a private admin mux behind a bearer-token check, plus a public mux that has no
debug routes at all.

This module is fully self-contained. Nothing here imports another exercise.

## What you'll build

```text
adminpprof/                    independent module: example.com/adminpprof
  go.mod
  admin.go                     NewAdminMux(token) behind bearer auth; NewPublicMux()
  cmd/demo/main.go             httptest server: 401 without token, 200 with token
  admin_test.go                auth + public-mux-has-no-pprof + default-mux-leak tests
```

- Files: `admin.go`, `cmd/demo/main.go`, `admin_test.go`.
- Implement: `NewAdminMux(token)` registering `pprof.Index/Cmdline/Profile/Symbol/Trace` and `pprof.Handler("goroutine"/"heap"/"block"/"mutex")` on a fresh `http.ServeMux`, wrapped by a constant-time bearer-token middleware; `NewPublicMux()` with only `/healthz`.
- Test: unauthenticated `GET /debug/pprof/` returns 401; authenticated `GET /debug/pprof/goroutine?debug=2` returns 200 with a body containing `goroutine `; the public mux returns 404 for `/debug/pprof/`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/adminpprof/cmd/demo
cd ~/go-exercises/adminpprof
go mod init example.com/adminpprof
```

### The side effect you are defending against

`net/http/pprof` has an `init` that calls `http.HandleFunc("/debug/pprof/", ...)`
and friends on `http.DefaultServeMux`. So the moment the package is linked into your
binary — even via `import _ "net/http/pprof"` — those routes exist on the default
mux. If any listener serves `DefaultServeMux` (the zero value `http.ListenAndServe(addr,
nil)` does exactly that), the profiles and `cmdline` are reachable by anyone who can
hit the port. A CPU profile request pins a core for its duration, and a goroutine
dump can leak internal structure, so this is both a DoS and an info-disclosure
surface.

The defense has two halves. First, register the handlers on a mux *you* control,
not the default one. The package exports the handlers precisely so you can:
`pprof.Index` serves the index and the per-profile pages, `pprof.Cmdline`,
`pprof.Profile` (CPU), `pprof.Symbol`, and `pprof.Trace` are the special endpoints,
and `pprof.Handler(name)` returns an `http.Handler` for any named profile
(`goroutine`, `heap`, `block`, `mutex`, `allocs`, `threadcreate`). Second, gate that
mux behind authentication and bind it to an admin-only listener — never serve
`DefaultServeMux` on the public port.

The auth middleware compares the bearer token with `crypto/subtle.ConstantTimeCompare`
so the check does not leak the token length or contents through timing. On failure
it sets `WWW-Authenticate` and returns `401`.

Create `admin.go`:

```go
package adminpprof

import (
	"crypto/subtle"
	"net/http"
	httppprof "net/http/pprof"
	"strings"
)

const bearerPrefix = "Bearer "

// NewAdminMux returns a handler that serves the net/http/pprof endpoints behind
// a bearer-token check. It uses a private ServeMux and never touches
// http.DefaultServeMux, so nothing here is reachable from a public listener.
func NewAdminMux(token string) http.Handler {
	mux := http.NewServeMux()

	// The special endpoints have dedicated handler functions.
	mux.HandleFunc("/debug/pprof/", httppprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", httppprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", httppprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", httppprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", httppprof.Trace)

	// Named profiles are served through pprof.Handler.
	for _, name := range []string{"goroutine", "heap", "block", "mutex"} {
		mux.Handle("/debug/pprof/"+name, httppprof.Handler(name))
	}

	return requireBearer(token, mux)
}

// NewPublicMux is the service's public surface. It intentionally has no
// /debug/pprof routes.
func NewPublicMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	return mux
}

// requireBearer rejects any request whose Authorization header is not exactly
// "Bearer <token>", using a constant-time comparison.
func requireBearer(token string, next http.Handler) http.Handler {
	want := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("Authorization")
		if !strings.HasPrefix(got, bearerPrefix) ||
			subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(got, bearerPrefix)), want) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
```

### The runnable demo

The demo stands the admin mux up on an `httptest` server and makes two requests: one
without a token (expect `401`) and one with the correct bearer token (expect `200`).
It prints the status codes.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"

	"example.com/adminpprof"
)

func main() {
	srv := httptest.NewServer(adminpprof.NewAdminMux("s3cr3t"))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/debug/pprof/goroutine?debug=1")
	if err != nil {
		panic(err)
	}
	resp.Body.Close()
	fmt.Println("no token:", resp.StatusCode)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/debug/pprof/goroutine?debug=1", nil)
	req.Header.Set("Authorization", "Bearer s3cr3t")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		panic(err)
	}
	resp2.Body.Close()
	fmt.Println("with token:", resp2.StatusCode)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
no token: 401
with token: 200
```

### Tests

The tests assert all three contracts: the admin surface rejects unauthenticated
requests, serves a real profile when authenticated, and the public mux has no debug
route. `TestImportRegistersOnDefaultMux` makes the whole point concrete — merely
linking `net/http/pprof` registers `/debug/pprof/` on `http.DefaultServeMux`, which
is why you must keep that mux off any public listener.

Create `admin_test.go`:

```go
package adminpprof

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testToken = "test-token"

func TestUnauthenticatedIsRejected(t *testing.T) {
	t.Parallel()
	h := NewAdminMux(testToken)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/debug/pprof/goroutine?debug=1", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: status = %d; want 401", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got != "Bearer" {
		t.Errorf("WWW-Authenticate = %q; want Bearer", got)
	}
}

func TestWrongTokenIsRejected(t *testing.T) {
	t.Parallel()
	h := NewAdminMux(testToken)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/debug/pprof/goroutine?debug=1", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: status = %d; want 401", rec.Code)
	}
}

func TestAuthenticatedGoroutineProfile(t *testing.T) {
	t.Parallel()
	h := NewAdminMux(testToken)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/debug/pprof/goroutine?debug=2", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("with token: status = %d; want 200", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "goroutine ") {
		t.Errorf("profile body missing 'goroutine ' header")
	}
}

func TestPublicMuxHasNoPprof(t *testing.T) {
	t.Parallel()
	h := NewPublicMux()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("public /debug/pprof/ status = %d; want 404", rec.Code)
	}
}

// TestImportRegistersOnDefaultMux documents the side effect: importing
// net/http/pprof registers routes on http.DefaultServeMux whether you wanted it
// or not. This is why DefaultServeMux must never back a public listener.
func TestImportRegistersOnDefaultMux(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil)
	http.DefaultServeMux.ServeHTTP(rec, req)

	if rec.Code == http.StatusNotFound {
		t.Fatal("expected net/http/pprof to have registered /debug/pprof/ on DefaultServeMux")
	}
}

func ExampleNewAdminMux() {
	h := NewAdminMux("tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil))
	fmt.Println(rec.Code)
	// Output: 401
}
```

## Review

The surface is correct when the *same* set of pprof endpoints is reachable with a
valid token and unreachable without one, and the public mux never exposes any of
them. The constant-time comparison matters: a naive `got == want` string compare
leaks token information through timing, and for a debug surface that gates access to
full stack dumps that is worth avoiding. `TestImportRegistersOnDefaultMux` is not
decoration — it proves the default mux carries the routes as a link-time side
effect, so the discipline "register on a private mux, keep DefaultServeMux off the
public listener" is the actual fix, not just registering handlers by hand. Note the
demo and tests import `net/http/httptest`, an ordinary package usable outside test
files. Run `go test -race` to confirm the middleware is safe under concurrent
requests.

## Resources

- [`net/http/pprof`](https://pkg.go.dev/net/http/pprof) — `Index`, `Cmdline`, `Profile`, `Symbol`, `Trace`, `Handler`, and the note that importing installs handlers on `DefaultServeMux`.
- [`crypto/subtle.ConstantTimeCompare`](https://pkg.go.dev/crypto/subtle#ConstantTimeCompare) — timing-safe token comparison.
- [`net/http.ServeMux`](https://pkg.go.dev/net/http#ServeMux) — building a private mux and the routing rules for the `/debug/pprof/` prefix.

---

Prev: [01-dump-goroutine-stacks-under-load.md](01-dump-goroutine-stacks-under-load.md) | Back to [00-concepts.md](00-concepts.md) | Next: [03-goroutine-leak-guard-in-tests.md](03-goroutine-leak-guard-in-tests.md)
