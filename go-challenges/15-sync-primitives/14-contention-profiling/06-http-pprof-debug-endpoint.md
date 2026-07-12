# Exercise 6: Expose pprof on an authenticated admin mux, never on the public listener

Capturing a contention profile from a live service requires an HTTP endpoint — and
the convenient way to get one, a blank import of `net/http/pprof`, is one of the
best-known security traps in Go. This module builds the production-correct
alternative: a public mux with no debug surface at all, and a separate admin mux
where the pprof handlers are registered explicitly behind a bearer token and a
debug switch that raises the mutex-profile fraction only while diagnosing.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
pprof-admin/                  independent module: example.com/pprof-admin
  go.mod                      go 1.23+
  server.go                   NewPublicMux, NewAdminMux, requireToken, type Debug,
                              ErrNoToken
  cmd/
    demo/
      main.go                 runnable demo: 404 on public, 401 without token,
                              200 profile bytes with token
  server_test.go              table-driven auth test, DefaultServeMux-trap test,
                              Debug enable/disable round-trip, Example
```

- Files: `server.go`, `cmd/demo/main.go`, `server_test.go`.
- Implement: `NewPublicMux()` (business routes only), `NewAdminMux(token, *Debug)` registering `pprof.Index` and `pprof.Handler("mutex")`/`pprof.Handler("block")` behind a constant-time bearer check, and a `Debug` switch that saves and restores `SetMutexProfileFraction`.
- Test: public mux returns 404 for `/debug/pprof/`; admin mux returns 401 without the token and 200 with a non-empty profile body with it; `http.DefaultServeMux` is shown to be polluted by the import; the `Debug` round-trip restores the previous fraction.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/15-sync-primitives/14-contention-profiling/06-http-pprof-debug-endpoint/cmd/demo
cd go-solutions/15-sync-primitives/14-contention-profiling/06-http-pprof-debug-endpoint
```

### Why the blank import is a trap

`import _ "net/http/pprof"` looks harmless — it exports nothing you call. But
importing a package runs its `init` functions, and this package's `init` registers
`/debug/pprof/*` handlers on `http.DefaultServeMux`. If your public server is
`http.ListenAndServe(addr, nil)` — `nil` means "use `DefaultServeMux`" — you have
just published your process internals to the internet: heap contents via the heap
profile, source paths and dependency names via symbolized stacks, and, worse, a
denial-of-service vector, because `/debug/pprof/profile?seconds=N` and
`/debug/pprof/trace?seconds=N` block a handler goroutine and burn CPU for a
*caller-controlled* duration. Anyone who can reach the port can ask your service
to profile itself in a loop.

Note the subtlety this module's tests make concrete: the pollution happens on
*import*, not on use. This module imports `net/http/pprof` deliberately (it needs
`pprof.Index` and `pprof.Handler`), so `http.DefaultServeMux` in this process is
already carrying the debug routes — the test proves it with a direct request. The
defense is therefore not "avoid the import somewhere"; any dependency in your
build might import it. The defense is: **never serve `http.DefaultServeMux` on a
public listener**. Build the public server on your own `http.NewServeMux()`, which
contains exactly the routes you registered and nothing else.

### The admin mux: explicit registration behind auth

The admin side registers the handlers manually. `pprof.Index` serves the
`/debug/pprof/` index page and also dispatches `/debug/pprof/<name>` lookups;
`pprof.Handler(name)` returns an `http.Handler` for one named profile, which is
the precise way to expose exactly `mutex` and `block` and nothing you did not
choose. Every route is wrapped in `requireToken`, a middleware that compares the
`Authorization: Bearer <token>` header using `crypto/subtle.ConstantTimeCompare`
so the comparison cannot leak token bytes through timing. A missing or wrong
token gets a 401 and a `WWW-Authenticate` header; the constant-time compare
returns 0 for length mismatches too, so there is a single rejection path.

Constructing the admin mux with an empty token is a configuration bug, not a
runtime condition to limp through — `NewAdminMux` refuses with the sentinel
`ErrNoToken`, wrapped with `%w` so callers assert it with `errors.Is`. An admin
surface that silently comes up unauthenticated is exactly the failure this module
exists to prevent.

### The Debug switch: profiling overhead only while diagnosing

Even authenticated, you do not want `SetMutexProfileFraction(1)` running all day —
every contended lock pays recording overhead. The `Debug` type wraps the
process-global setting in the save/restore discipline from earlier modules:
`Enable` captures the previous fraction from the return value and raises it to a
production-safe 100 (1-in-100 contention events sampled); `Disable` puts the
previous value back. Both are idempotent under a mutex so a double `POST` from a
retrying client cannot clobber the saved value. The admin mux exposes them as
`POST /debug/profiling/enable` and `POST /debug/profiling/disable` — the SRE
workflow is: enable, capture the profile through the mutex/block endpoints,
disable.

Create `server.go`:

```go
package pprofadmin

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"net/http/pprof"
	"runtime"
	"sync"
)

// ErrNoToken reports that the admin mux was configured without a bearer token.
// An unauthenticated debug surface is a misconfiguration, never a fallback.
var ErrNoToken = errors.New("pprofadmin: admin token must not be empty")

// productionFraction samples 1 in 100 contention events: enough signal to see
// the shape of a problem, cheap enough to leave on while diagnosing.
const productionFraction = 100

// Debug guards the process-global mutex-profile fraction with the save/restore
// discipline: Enable captures the previous fraction, Disable puts it back.
// Both are idempotent so a retried request cannot clobber the saved value.
type Debug struct {
	mu   sync.Mutex
	on   bool
	prev int
}

// Enable raises the mutex-profile fraction, remembering the previous value.
func (d *Debug) Enable() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.on {
		return
	}
	d.prev = runtime.SetMutexProfileFraction(productionFraction)
	d.on = true
}

// Disable restores the fraction that Enable saved.
func (d *Debug) Disable() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.on {
		return
	}
	runtime.SetMutexProfileFraction(d.prev)
	d.on = false
}

// NewPublicMux is the internet-facing surface: business routes only, no debug
// registrations, and crucially not http.DefaultServeMux.
func NewPublicMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("GET /orders/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, "{\"order\":%q,\"status\":\"shipped\"}\n", r.PathValue("id"))
	})
	return mux
}

// NewAdminMux registers the pprof handlers explicitly, each behind the bearer
// token, plus the profiling on/off switch. It refuses an empty token.
func NewAdminMux(token string, dbg *Debug) (*http.ServeMux, error) {
	if token == "" {
		return nil, fmt.Errorf("new admin mux: %w", ErrNoToken)
	}
	mux := http.NewServeMux()
	mux.Handle("/debug/pprof/", requireToken(token, http.HandlerFunc(pprof.Index)))
	mux.Handle("/debug/pprof/mutex", requireToken(token, pprof.Handler("mutex")))
	mux.Handle("/debug/pprof/block", requireToken(token, pprof.Handler("block")))
	mux.Handle("POST /debug/profiling/enable", requireToken(token,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			dbg.Enable()
			fmt.Fprintln(w, "profiling enabled")
		})))
	mux.Handle("POST /debug/profiling/disable", requireToken(token,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			dbg.Disable()
			fmt.Fprintln(w, "profiling disabled")
		})))
	return mux, nil
}

// requireToken rejects any request whose Authorization header is not exactly
// "Bearer <token>", using a constant-time comparison.
func requireToken(token string, next http.Handler) http.Handler {
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="pprof-admin"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
```

### The runnable demo

The demo stands up both muxes on `httptest` servers (an ephemeral real listener
each) and walks the three cases that matter: the public listener knows nothing
about `/debug/pprof/`, the admin listener rejects a tokenless request, and with
the token it serves a non-empty binary mutex profile.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"

	pprofadmin "example.com/pprof-admin"
)

func get(url, token string) (int, int) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		panic(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		panic(err)
	}
	return resp.StatusCode, len(body)
}

func main() {
	const token = "s3cr3t-admin-token"
	var dbg pprofadmin.Debug

	public := httptest.NewServer(pprofadmin.NewPublicMux())
	defer public.Close()

	adminMux, err := pprofadmin.NewAdminMux(token, &dbg)
	if err != nil {
		panic(err)
	}
	admin := httptest.NewServer(adminMux)
	defer admin.Close()

	status, _ := get(public.URL+"/debug/pprof/", "")
	fmt.Printf("public  /debug/pprof/      -> %d\n", status)

	status, _ = get(public.URL+"/healthz", "")
	fmt.Printf("public  /healthz           -> %d\n", status)

	status, _ = get(admin.URL+"/debug/pprof/mutex", "")
	fmt.Printf("admin   no token           -> %d\n", status)

	status, n := get(admin.URL+"/debug/pprof/mutex", token)
	fmt.Printf("admin   with token         -> %d (profile bytes > 0: %v)\n", status, n > 0)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
public  /debug/pprof/      -> 404
public  /healthz           -> 200
admin   no token           -> 401
admin   with token         -> 200 (profile bytes > 0: true)
```

### Tests

`TestAdminAuth` is the table: no token, wrong token, right token, against both the
index and the mutex profile route, asserting status and (for the authorized
profile fetch) a non-empty body — the profile handler emits a valid binary
protobuf even before any samples exist, so non-empty is the honest, non-flaky
assertion. `TestPublicMuxHasNoPprof` pins the isolation. `TestDefaultServeMuxTrap`
proves the import side effect: this package never registered anything on
`http.DefaultServeMux`, yet the route answers 200 there, because the
`net/http/pprof` init did it. `TestDebugRoundTrip` reads the live fraction with
`SetMutexProfileFraction(-1)` (negative reads without setting) and asserts
`Enable`/`Disable` restore it exactly; it is deliberately not parallel because the
fraction is process-global.

Create `server_test.go`:

```go
package pprofadmin

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
)

const testToken = "test-token"

func TestPublicMuxHasNoPprof(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(NewPublicMux())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/debug/pprof/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("public mux /debug/pprof/ = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}

	resp, err = http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("public mux /healthz = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestAdminAuth(t *testing.T) {
	t.Parallel()
	var dbg Debug
	mux, err := NewAdminMux(testToken, &dbg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	tests := []struct {
		name       string
		path       string
		auth       string
		wantStatus int
		wantBody   bool // assert non-empty body
	}{
		{name: "index without token", path: "/debug/pprof/", auth: "", wantStatus: http.StatusUnauthorized},
		{name: "mutex without token", path: "/debug/pprof/mutex", auth: "", wantStatus: http.StatusUnauthorized},
		{name: "mutex wrong token", path: "/debug/pprof/mutex", auth: "Bearer nope", wantStatus: http.StatusUnauthorized},
		{name: "index with token", path: "/debug/pprof/", auth: "Bearer " + testToken, wantStatus: http.StatusOK},
		{name: "mutex with token", path: "/debug/pprof/mutex", auth: "Bearer " + testToken, wantStatus: http.StatusOK, wantBody: true},
		{name: "block with token", path: "/debug/pprof/block", auth: "Bearer " + testToken, wantStatus: http.StatusOK, wantBody: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			req, err := http.NewRequest(http.MethodGet, srv.URL+tt.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			if tt.auth != "" {
				req.Header.Set("Authorization", tt.auth)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("GET %s = %d, want %d", tt.path, resp.StatusCode, tt.wantStatus)
			}
			if tt.wantBody {
				body, err := io.ReadAll(resp.Body)
				if err != nil {
					t.Fatal(err)
				}
				if len(body) == 0 {
					t.Fatalf("GET %s returned an empty profile body", tt.path)
				}
			}
		})
	}
}

func TestDefaultServeMuxTrap(t *testing.T) {
	t.Parallel()
	// This package never registered anything on http.DefaultServeMux, yet the
	// route answers: the net/http/pprof import's init did it. This is why the
	// default mux must never back a public listener.
	req := httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil)
	rec := httptest.NewRecorder()
	http.DefaultServeMux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("DefaultServeMux /debug/pprof/ = %d, want %d (import side effect)",
			rec.Code, http.StatusOK)
	}
}

func TestEmptyTokenRejected(t *testing.T) {
	t.Parallel()
	var dbg Debug
	_, err := NewAdminMux("", &dbg)
	if !errors.Is(err, ErrNoToken) {
		t.Fatalf("NewAdminMux(\"\") error = %v, want errors.Is(..., ErrNoToken)", err)
	}
}

func TestDebugRoundTrip(t *testing.T) {
	// Process-global setting: not parallel. A negative argument reads the
	// current fraction without changing it.
	before := runtime.SetMutexProfileFraction(-1)

	var dbg Debug
	dbg.Enable()
	if got := runtime.SetMutexProfileFraction(-1); got != productionFraction {
		t.Fatalf("after Enable fraction = %d, want %d", got, productionFraction)
	}
	dbg.Enable() // idempotent: must not overwrite the saved previous value
	dbg.Disable()
	if got := runtime.SetMutexProfileFraction(-1); got != before {
		t.Fatalf("after Disable fraction = %d, want %d (previous value restored)", got, before)
	}
	dbg.Disable() // idempotent no-op
}

func ExampleNewAdminMux() {
	var dbg Debug
	mux, _ := NewAdminMux("example-token", &dbg)

	req := httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	fmt.Println("without token:", rec.Code)

	req = httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil)
	req.Header.Set("Authorization", "Bearer example-token")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	fmt.Println("with token:", rec.Code)

	// Output:
	// without token: 401
	// with token: 200
}
```

## Review

The design is correct when the two surfaces cannot be confused: the public mux
serves only the routes you registered on it (a 404 for `/debug/pprof/` proves it),
and the admin mux serves nothing without the bearer token. The trap test is the
one to internalize — `http.DefaultServeMux` answering 200 for a route this package
never registered is the concrete demonstration that importing `net/http/pprof`
mutates global state, and that any transitive dependency could do the same to you.
The mistakes to avoid: passing `nil` (meaning `DefaultServeMux`) to a public
`ListenAndServe`; registering the full pprof index when you only need two
profiles (expose the minimum — `pprof.Handler` per profile is precise);
comparing tokens with `==` instead of `subtle.ConstantTimeCompare`; and leaving
the fraction raised after the incident is over — `Debug.Disable` restoring the
saved value is the same save/restore discipline the test modules use with
`t.Cleanup`. Confirm with `go test -race ./...` and by running the demo, then
point `go tool pprof` at `<admin>/debug/pprof/mutex` with the token in an
`Authorization` header to complete the real workflow.

## Resources

- [net/http/pprof](https://pkg.go.dev/net/http/pprof) — the init side effect, `Index`, and `Handler(name)` for explicit registration.
- [Diagnostics (official Go documentation)](https://go.dev/doc/diagnostics) — where profiling fits among Go's diagnostic tools.
- [crypto/subtle.ConstantTimeCompare](https://pkg.go.dev/crypto/subtle#ConstantTimeCompare) — timing-safe secret comparison.
- [runtime.SetMutexProfileFraction](https://pkg.go.dev/runtime#SetMutexProfileFraction) — the fraction semantics, including the negative-argument read.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [05-mutex-and-block-profile-capture.md](05-mutex-and-block-profile-capture.md) | Next: [07-lock-wait-slo-gauge.md](07-lock-wait-slo-gauge.md)
