# Exercise 10: Diagnose a Live Leak with runtime/pprof Goroutine Dumps

The other exercises catch leaks in CI. This one is the operational side: a leak is
already loose in a running process, and you need to find it without a unit test. An
internal `/debug/goroutines` handler that writes a full goroutine stack dump via
`runtime/pprof` turns "the pod is OOMing" into "here is the stack that is
accumulating." This exercise builds that handler and a `Count()` signal to alert on.

This module is self-contained: its own `go mod init`, all code inline, its own demo
and tests. It uses only the standard library.

## What you'll build

```text
pprofdump/                   independent module: example.com/pprofdump
  go.mod
  dump.go                    DumpGoroutines, GoroutineCount, Handler
  cmd/
    demo/
      main.go                runnable demo: count and dump goroutines
  dump_test.go               handler contains a known goroutine, Count signal, no leak
```

- Files: `dump.go`, `cmd/demo/main.go`, `dump_test.go`.
- Implement: `DumpGoroutines(w)` writing a `debug=2` stack dump, `GoroutineCount()` as a cheap numeric signal, and an `http.HandlerFunc` serving the dump with a sensible content-type.
- Test: with a known background goroutine running, the handler's body names it and `Count()` exceeds the baseline; `Count()` tracks `runtime.NumGoroutine()`; the handler does not leak.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/pprofdump/cmd/demo
cd ~/go-exercises/pprofdump
go mod init example.com/pprofdump
```

### debug=2 is the human-readable full dump

`runtime/pprof` exposes a set of named profiles; `pprof.Lookup("goroutine")` is the
one that matters for leaks. Its `WriteTo(w, debug)` has two text modes that matter:
`debug=1` prints aggregated stacks with counts (good for a machine), and `debug=2`
prints *every* goroutine's full stack in the same format the runtime uses for a panic
dump — the header line with the goroutine's state (`[chan receive]`, `[select]`,
`[IO wait]`), then the call stack. When a leak has hundreds of goroutines all parked
in the same function, `debug=2` makes that function jump out: it appears hundreds of
times, all in the same wait state. That repetition, correlated with rising RSS, is the
signature of a leak.

`GoroutineCount()` wraps `pprof.Lookup("goroutine").Count()`, which returns exactly
`runtime.NumGoroutine()` — a single cheap integer. This is the number to export as a
gauge and alert on: a goroutine count that climbs monotonically alongside memory is a
leak until proven otherwise. The dump tells you *which* function; the count tells you
*that* something is wrong and is what you page on.

The handler sets `Content-Type: text/plain` so a browser or `curl` renders the dump
as text, and writes the `debug=2` profile straight to the response. In production this
handler lives on an internal-only listener or behind auth — a goroutine dump reveals
stack traces you do not want public.

Create `dump.go`:

```go
package pprofdump

import (
	"errors"
	"io"
	"net/http"
	"runtime/pprof"
)

// ErrNoProfile is returned if the goroutine profile is unavailable (it always
// exists in a normal runtime, but callers should not assume).
var ErrNoProfile = errors.New("pprofdump: goroutine profile unavailable")

// DumpGoroutines writes a human-readable (debug=2) stack dump of every goroutine
// to w. This is what an engineer reads to find a leak in a running process.
func DumpGoroutines(w io.Writer) error {
	p := pprof.Lookup("goroutine")
	if p == nil {
		return ErrNoProfile
	}
	return p.WriteTo(w, 2)
}

// GoroutineCount returns the current number of goroutines. It equals
// runtime.NumGoroutine() and is the cheap numeric signal to alert on.
func GoroutineCount() int {
	p := pprof.Lookup("goroutine")
	if p == nil {
		return 0
	}
	return p.Count()
}

// Handler serves the goroutine dump as text/plain. Mount it on an internal-only
// listener; a stack dump is not for public consumption.
func Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := pprof.Lookup("goroutine")
		if p == nil {
			http.Error(w, ErrNoProfile.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_ = p.WriteTo(w, 2)
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"bytes"
	"fmt"

	"example.com/pprofdump"
)

func main() {
	fmt.Println("goroutines >= 1:", pprofdump.GoroutineCount() >= 1)

	var buf bytes.Buffer
	if err := pprofdump.DumpGoroutines(&buf); err != nil {
		fmt.Println("dump:", err)
		return
	}
	fmt.Println("dump non-empty:", buf.Len() > 0)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
goroutines >= 1: true
dump non-empty: true
```

### The tests

`TestHandlerNamesGoroutine` starts a `parkedWorker` goroutine, calls the handler with
`httptest`, and asserts the dump body names `parkedWorker` and sets a text content-type
— proving the dump is what you would read to locate a leak. `TestCountRises` shows
`GoroutineCount()` exceeding a baseline once the worker is running.
`TestCountMatchesRuntime` confirms `Count()` and `runtime.NumGoroutine()` agree.
`TestHandlerDoesNotLeak` calls the handler repeatedly and polls the count back to
baseline.

Create `dump_test.go`:

```go
package pprofdump

import (
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"
)

// parkedWorker is a known, named goroutine so the dump can be asserted on.
func parkedWorker(stop <-chan struct{}) { <-stop }

// waitForCount polls until GoroutineCount rises above base, or gives up.
func waitForCount(above int) bool {
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if GoroutineCount() > above {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return false
}

func TestHandlerNamesGoroutine(t *testing.T) {
	stop := make(chan struct{})
	defer close(stop)
	go parkedWorker(stop)
	if !waitForCount(runtime.NumGoroutine() - 1) {
		t.Fatal("worker never showed up in the count")
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/debug/goroutines", nil)
	Handler().ServeHTTP(rec, req)

	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("Content-Type = %q, want text/plain...", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "parkedWorker") {
		t.Fatalf("dump did not name parkedWorker:\n%s", body)
	}
}

func TestCountRises(t *testing.T) {
	base := GoroutineCount()
	stop := make(chan struct{})
	defer close(stop)
	go parkedWorker(stop)

	if !waitForCount(base) {
		t.Fatalf("GoroutineCount did not rise above baseline %d", base)
	}
}

func TestCountMatchesRuntime(t *testing.T) {
	c := GoroutineCount()
	n := runtime.NumGoroutine()
	diff := c - n
	if diff < 0 {
		diff = -diff
	}
	if diff > 3 {
		t.Fatalf("GoroutineCount()=%d and runtime.NumGoroutine()=%d differ by %d", c, n, diff)
	}
}

func TestHandlerDoesNotLeak(t *testing.T) {
	runtime.GC()
	base := runtime.NumGoroutine()

	for range 20 {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/debug/goroutines", nil)
		Handler().ServeHTTP(rec, req)
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		runtime.GC()
		if runtime.NumGoroutine() <= base {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("handler leaked: baseline=%d current=%d", base, runtime.NumGoroutine())
}
```

## Review

The handler is correct when its body is a `debug=2` dump that names the functions
goroutines are parked in, and `GoroutineCount()` is the same cheap integer as
`runtime.NumGoroutine()`. `TestHandlerNamesGoroutine` proves the diagnostic value — a
real leak would show `parkedWorker` (or your handler function) repeated once per stuck
goroutine — and `TestCountMatchesRuntime` pins the numeric signal.

The mistakes to avoid: do not expose this dump publicly; stack traces leak internal
structure, so mount it on an internal listener or behind auth. Do not confuse `debug=1`
(aggregated) with `debug=2` (full per-goroutine) — you want `debug=2` to see the exact
stacks. And treat `GoroutineCount()`/`NumGoroutine()` as the thing to alert on: a
monotonically rising count correlated with RSS is your earliest leak signal, well before
an OOM. Run under `-race`; the handler touches no shared mutable state.

## Resources

- [`runtime/pprof.Lookup`](https://pkg.go.dev/runtime/pprof#Lookup) — the named profiles, including `"goroutine"`.
- [`(*pprof.Profile).WriteTo`](https://pkg.go.dev/runtime/pprof#Profile.WriteTo) — the `debug` argument and what `debug=2` prints.
- [`net/http/pprof`](https://pkg.go.dev/net/http/pprof) — the standard library's own pprof handlers, which this mirrors.
- [Diagnostics](https://go.dev/doc/diagnostics) — the Go project's guide to profiling and goroutine dumps in production.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [09-graceful-shutdown-supervisor.md](09-graceful-shutdown-supervisor.md) | Next: [11-lease-renewal-keepalive-leak.md](11-lease-renewal-keepalive-leak.md)
