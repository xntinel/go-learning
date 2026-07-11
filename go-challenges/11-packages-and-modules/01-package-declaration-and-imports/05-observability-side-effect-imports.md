# Exercise 5: Wiring pprof and expvar via Side-Effect Imports

Profiling and metrics wire themselves in through the same blank-import mechanism as
database drivers — and that is exactly where a common production incident comes
from. `import _ "net/http/pprof"` mounts heap and CPU profile routes onto
`http.DefaultServeMux` as a hidden side effect. If your public HTTP server uses the
default mux, you have just published `/debug/pprof/` to the internet. This exercise
reproduces the exposure and the senior correction: serve `/debug` only on a private
admin mux.

This module is self-contained. Nothing here imports another exercise.

## What you'll build

```text
obsadmin/                          module: example.com/obsadmin
  go.mod
  obs/obs.go                       expvar counter + blank _ "net/http/pprof" (the exposure)
  admin/admin.go                   NewAdminMux(): pprof + expvar on a private mux (the fix)
  obs/obs_test.go                  proves default-mux exposure and the private-mux fix
  cmd/demo/main.go                 prints the status of each route on each mux
```

- Files: `obs/obs.go`, `admin/admin.go`, `obs/obs_test.go`, `cmd/demo/main.go`.
- Implement: an `obs` package that publishes an `expvar` counter and (via blank import) registers pprof on the default mux, and an `admin` package that mounts pprof/expvar explicitly on a fresh `http.ServeMux`.
- Test: `GET /debug/pprof/` returns 200 on `http.DefaultServeMux` (blank import registered globally) but 404 on a fresh mux; the admin mux serves it; the expvar counter is visible and increments.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/obsadmin/obs ~/go-exercises/obsadmin/admin ~/go-exercises/obsadmin/cmd/demo
cd ~/go-exercises/obsadmin
go mod init example.com/obsadmin
go mod edit -go=1.26
```

### The exposure: blank import mutates the global mux

`net/http/pprof`'s `init()` calls `http.HandleFunc("/debug/pprof/", …)` and
friends on `http.DefaultServeMux`. So merely importing it — even blank, even from a
package deep in your tree — registers those routes globally. `expvar` behaves the
same way, publishing `/debug/vars` and a couple of default variables from its own
`init()`. Neither shows up at any call site; the effect is entirely in package
initialization. The `obs` package below both publishes an application counter with
`expvar.NewInt` and blank-imports `net/http/pprof` to represent the (very common)
"we added profiling" change. The danger is that if any server anywhere in the
process serves `http.DefaultServeMux` on a public listener, all of `/debug/pprof/`
comes with it.

Create `obs/obs.go`:

```go
package obs

import (
	"expvar"

	// Blank import: pprof registers /debug/pprof/ routes on http.DefaultServeMux
	// from its init(). This is the exposure the admin mux below contains.
	_ "net/http/pprof"
)

// RequestsTotal is an application counter published to expvar's global map.
var RequestsTotal = expvar.NewInt("requests_total")

// IncRequests records one handled request.
func IncRequests() { RequestsTotal.Add(1) }
```

### The fix: an explicit private admin mux

The correction is to never serve `http.DefaultServeMux` publicly, and to mount the
debug routes explicitly on a *fresh* `http.ServeMux` that you bind to an
internal-only listener (a separate port reachable only from inside the cluster, or
behind an auth proxy). `net/http/pprof` exports its handlers as named functions
exactly for this — `pprof.Index`, `pprof.Cmdline`, `pprof.Profile`, `pprof.Symbol`,
`pprof.Trace` — so you can register them where *you* choose. `expvar.Handler`
returns an `http.Handler` for `/debug/vars`. The public server gets its own mux
with only real application routes.

Create `admin/admin.go`:

```go
package admin

import (
	"expvar"
	"net/http"
	"net/http/pprof"
)

// NewAdminMux returns a mux that serves profiling and metrics. Bind it to an
// internal-only listener; never expose it on the public server.
func NewAdminMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	mux.Handle("/debug/vars", expvar.Handler())
	return mux
}
```

### The demo

The demo uses `httptest` recorders (no real ports) to show the three outcomes side
by side. It imports `obs`, which triggers the blank pprof registration on the
default mux.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"

	"example.com/obsadmin/admin"
	"example.com/obsadmin/obs"
)

func status(h http.Handler, path string) int {
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
	return rr.Code
}

func main() {
	obs.IncRequests()
	fmt.Println("default mux /debug/pprof/ ->", status(http.DefaultServeMux, "/debug/pprof/"))
	fmt.Println("fresh   mux /debug/pprof/ ->", status(http.NewServeMux(), "/debug/pprof/"))
	fmt.Println("admin   mux /debug/pprof/ ->", status(admin.NewAdminMux(), "/debug/pprof/"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
default mux /debug/pprof/ -> 200
fresh   mux /debug/pprof/ -> 404
admin   mux /debug/pprof/ -> 200
```

The middle line is the whole point: a mux you create yourself is empty, so the
`/debug` routes are only where you put them — unless you serve the default mux,
which the blank import has silently populated.

### Tests

The test is in-package `obs`, so importing `obs` runs its blank pprof import and
the default mux is populated for the test binary. All requests use `GET`, which
Go 1.22+ method-aware routing expects.

Create `obs/obs_test.go`:

```go
package obs

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"example.com/obsadmin/admin"
)

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
	return rr
}

func TestBlankImportExposesDefaultMux(t *testing.T) {
	if code := get(t, http.DefaultServeMux, "/debug/pprof/").Code; code != http.StatusOK {
		t.Fatalf("default mux /debug/pprof/ = %d, want 200 (blank import should register it)", code)
	}
}

func TestFreshMuxIsNotExposed(t *testing.T) {
	if code := get(t, http.NewServeMux(), "/debug/pprof/").Code; code != http.StatusNotFound {
		t.Fatalf("fresh mux /debug/pprof/ = %d, want 404", code)
	}
}

func TestAdminMuxServesPprof(t *testing.T) {
	if code := get(t, admin.NewAdminMux(), "/debug/pprof/").Code; code != http.StatusOK {
		t.Fatalf("admin mux /debug/pprof/ = %d, want 200", code)
	}
}

func TestExpvarCounterVisibleAndIncrements(t *testing.T) {
	before := RequestsTotal.Value()
	IncRequests()
	if got := RequestsTotal.Value(); got != before+1 {
		t.Fatalf("RequestsTotal = %d, want %d", got, before+1)
	}
	body := get(t, admin.NewAdminMux(), "/debug/vars").Body.String()
	if !strings.Contains(body, "requests_total") {
		t.Fatalf("/debug/vars body missing requests_total: %s", body)
	}
}
```

## Review

The wiring is correct when the blank import registers pprof on the default mux (so
`GET /debug/pprof/` is 200 there), a mux you create is empty (404) until you mount
routes on it, and the admin mux serves both pprof and expvar explicitly. The
security lesson is the 404: it proves that the debug surface lives wherever the mux
lives, so the safe design is to keep it on a private admin listener and never serve
`http.DefaultServeMux` publicly. The subtle failure mode is that this exposure is
invisible in code review — no call site mentions `/debug/pprof/`; a single blank
import three packages away is enough. Grep your binaries for `net/http/pprof` and
confirm nothing serves the default mux on a public port. Run `go test -race`; the
counter is shared state.

## Resources

- [`net/http/pprof`](https://pkg.go.dev/net/http/pprof) — the blank-import behavior and the exported `Index`/`Profile`/… handlers for a custom mux.
- [`expvar`](https://pkg.go.dev/expvar) — `NewInt`, `Handler`, and the default `/debug/vars` registration.
- [`net/http.ServeMux`](https://pkg.go.dev/net/http#ServeMux) — the default vs. a fresh mux, and 1.22+ method-aware patterns.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [04-import-alias-versioned-apis.md](04-import-alias-versioned-apis.md) | Next: [06-break-import-cycle-inversion.md](06-break-import-cycle-inversion.md)
