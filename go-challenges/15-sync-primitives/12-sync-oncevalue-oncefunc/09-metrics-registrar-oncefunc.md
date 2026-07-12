# Exercise 9: Register-Once Observability Hooks: OnceFunc Against Duplicate Registration Panics

Metrics registries panic on duplicate registration — `expvar.Publish` in the
stdlib, Prometheus's `MustRegister` in the wild — and services have multiple
entry points that all want metrics ready; this exercise makes registration
idempotent with `sync.OnceFunc` and proves, with a negative test, why the
wrapper must be the only door in.

## What you'll build

```text
metricsboot/               independent module: example.com/metricsboot
  go.mod                   go mod init example.com/metricsboot
  metricsboot.go           EnsureMetrics via sync.OnceFunc; IncRequests, Requests; EnsureFunc helper
  metricsboot_test.go      50-goroutine idempotency on a panicking stub, real expvar wiring, negative test, Example
  cmd/
    demo/
      main.go              runnable demo: two subsystems call EnsureMetrics, one registration, counter works
```

- Files: `metricsboot.go`, `metricsboot_test.go`, `cmd/demo/main.go`.
- Implement: `registerMetrics` publishing real `expvar` variables (`expvar.NewInt`, `expvar.Publish` — which panic on duplicate names); `EnsureMetrics = sync.OnceFunc(registerMetrics)`; `EnsureFunc(register func()) func()` so tests can wrap a counting, panic-on-second-call stub.
- Test: 50 concurrent plus repeated sequential `ensure()` calls against a stub that panics on its second registration — exactly one registration, no panic; a negative test shows the unwrapped path panicking; the real `EnsureMetrics` wiring verified via `expvar.Get`.
- Verify: `go test -count=1 -race ./...` and `go run ./cmd/demo`.

### Why registries panic, and why that panic finds you

`expvar.Publish` documents it plainly: reuse of a name is a program logic
error, and it panics (via `log.Panic`). Prometheus's `MustRegister` behaves
the same for duplicate collectors. The registry is process-global state, and
two writers claiming the same name is a bug worth crashing on — at *deploy*
time. The trouble is who calls registration. In a real service it is: the
HTTP server setup, the worker setup, an integration test's bootstrap, and —
the classic incident — a second import path that transitively initializes
the same telemetry package. Each caller is individually correct ("make sure
metrics exist before I use them"); together they panic the process on the
second call. Boot-time crash loops from duplicate registration are a staple
of on-call handovers.

`sync.OnceFunc(registerMetrics)` is the exact-fit tool: registration is a
side-effect-only init (no value to return), it must happen at most once per
process, and every subsystem should be able to call "ensure it happened"
freely and concurrently. The once also gives ordering: any goroutine that
returns from `EnsureMetrics()` is guaranteed (happens-before) to see every
variable the registration published, so `IncRequests` can safely touch the
`*expvar.Int` created inside it.

### The pitfall: OnceFunc only dedupes its own calls

The wrapper deduplicates calls **through the returned closure** — nothing
else. If a second code path calls `registerMetrics()` directly, or publishes
the same expvar name itself, the registry still panics; the once never saw
that call. This is the deduplication-scope rule from the concepts file at its
sharpest, because the underlying operation punishes you with a crash rather
than a duplicate side effect. The discipline is architectural: the raw
registration function stays **unexported**, the wrapper is the only exported
entry point, and code review treats a direct `expvar.Publish` of a package
name outside `registerMetrics` as a bug. The negative test below encodes that
rule: it registers through a raw path twice and asserts the panic happens —
proving the wrapper is load-bearing, not decorative.

One more production note: `EnsureMetrics` being callable from anywhere does
not mean callers should be *lazy* about it. Call it early in `main` (or from
the readiness path) so a broken registration crashes at boot, where crash
loops are visible and cheap, not on the first request that happens to touch a
metric.

Create `metricsboot.go`:

```go
// Package metricsboot makes metrics registration idempotent. The underlying
// registry (expvar here; a Prometheus registry in the wild) panics on
// duplicate registration, and a service has many entry points that all want
// metrics ready -- so registration is guarded by sync.OnceFunc and the raw
// register function stays unexported.
package metricsboot

import (
	"expvar"
	"sync"
)

// EnsureFunc returns an idempotent version of register: however many
// callers, from however many goroutines, register runs exactly once.
// Deduplication covers only calls through the returned function.
func EnsureFunc(register func()) func() {
	return sync.OnceFunc(register)
}

var requests *expvar.Int

// EnsureMetrics registers this service's expvar metrics exactly once, no
// matter how many subsystems call it. It is the ONLY entry point to
// registerMetrics; a second path calling expvar.Publish with these names
// would panic the process.
var EnsureMetrics = EnsureFunc(registerMetrics)

// registerMetrics is the raw, panics-on-second-call registration. Keep it
// unexported: every path must go through EnsureMetrics.
func registerMetrics() {
	requests = expvar.NewInt("app_requests_total")
	expvar.Publish("app_build_info", expvar.Func(func() any {
		return map[string]string{"version": "1.4.2"}
	}))
}

// IncRequests bumps the request counter, ensuring registration first. The
// once's happens-before guarantee makes the requests pointer safely visible
// here without further synchronization.
func IncRequests() {
	EnsureMetrics()
	requests.Add(1)
}

// Requests returns the current counter value, ensuring registration first.
func Requests() int64 {
	EnsureMetrics()
	return requests.Value()
}
```

The published variables are served by `expvar.Handler()` (or automatically on
`http.DefaultServeMux` at `/debug/vars` when the importing binary uses it) —
wiring that endpoint is one `mux.Handle("/debug/vars", expvar.Handler())`
line in the server setup, which is one of the subsystems calling
`EnsureMetrics`.

### The demo

The demo plays the two entry points: HTTP setup and worker setup both ensure
metrics, requests are counted from both sides, and the process does not panic
— the registration counter (instrumented via `EnsureFunc` around a counting
stand-in in the tests; here simply observed by the absence of a crash and a
working counter) ran once.

Create `cmd/demo/main.go`:

```go
package main

import (
	"expvar"
	"fmt"

	"example.com/metricsboot"
)

func setupHTTP() {
	metricsboot.EnsureMetrics()
	fmt.Println("http setup: metrics ready")
}

func setupWorker() {
	metricsboot.EnsureMetrics()
	fmt.Println("worker setup: metrics ready")
}

func main() {
	setupHTTP()
	setupWorker() // second registration path: no panic, thanks to the once

	for range 3 {
		metricsboot.IncRequests()
	}
	fmt.Println("requests_total:", metricsboot.Requests())
	fmt.Println("published:", expvar.Get("app_requests_total").String())
	fmt.Println("build_info:", expvar.Get("app_build_info").String())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
http setup: metrics ready
worker setup: metrics ready
requests_total: 3
published: 3
build_info: {"version":"1.4.2"}
```

Without the once, the `setupWorker` line would never print: the second
registration would panic with `Reuse of exported var name: app_requests_total`.

### Tests

`TestEnsureFuncExactlyOnce` is the concurrency contract, run against a stub
that *panics if registered twice* — so a failure here is loud: 50 concurrent
callers plus 10 sequential ones produce exactly one registration.
`TestUnwrappedPathPanics` is the negative proof that the wrapper is the only
safe door: the raw path, called twice with real `expvar.Publish`, panics.
`TestEnsureMetricsPublishes` exercises the real wiring end to end through
`expvar.Get`. The expvar registry is process-global, so the negative test
uses its own unique name and silences `log` while provoking the panic.

Create `metricsboot_test.go`:

```go
package metricsboot

import (
	"expvar"
	"fmt"
	"io"
	"log"
	"sync"
	"sync/atomic"
	"testing"
)

func TestEnsureFuncExactlyOnce(t *testing.T) {
	t.Parallel()

	var registrations atomic.Int64
	ensure := EnsureFunc(func() {
		if registrations.Add(1) > 1 {
			panic("duplicate registration")
		}
	})

	var wg sync.WaitGroup
	const goroutines = 50
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			ensure()
		}()
	}
	wg.Wait()
	for range 10 {
		ensure()
	}

	if got := registrations.Load(); got != 1 {
		t.Fatalf("register ran %d times, want 1", got)
	}
}

func TestUnwrappedPathPanics(t *testing.T) {
	// Not parallel: it redirects the global log output while provoking
	// expvar's log.Panic.
	orig := log.Writer()
	log.SetOutput(io.Discard)
	defer log.SetOutput(orig)

	rawRegister := func() {
		expvar.Publish("metricsboot_test_dup", expvar.Func(func() any { return 1 }))
	}

	rawRegister() // first raw registration succeeds

	defer func() {
		if recover() == nil {
			t.Fatal("second raw registration did not panic; the OnceFunc wrapper would be unnecessary")
		}
	}()
	rawRegister() // second raw registration bypasses any once: panic
}

func TestEnsureMetricsPublishes(t *testing.T) {
	t.Parallel()

	var wg sync.WaitGroup
	wg.Add(20)
	for range 20 {
		go func() {
			defer wg.Done()
			EnsureMetrics()
		}()
	}
	wg.Wait()

	if expvar.Get("app_requests_total") == nil {
		t.Fatal("app_requests_total not published")
	}
	if expvar.Get("app_build_info") == nil {
		t.Fatal("app_build_info not published")
	}

	before := Requests()
	for range 5 {
		IncRequests()
	}
	if got := Requests() - before; got != 5 {
		t.Fatalf("counter moved by %d, want 5", got)
	}
}

func ExampleEnsureFunc() {
	registrations := 0
	ensure := EnsureFunc(func() { registrations++ })
	ensure()
	ensure()
	fmt.Println("registrations:", registrations)
	// Output: registrations: 1
}
```

Run the suite:

```bash
go test -count=1 -race ./...
```

## Review

The module holds when the panicking stub survives 60 calls with one
registration, and — just as important — when the negative test still panics:
if someone "fixes" that panic, they have moved deduplication into the
registry and the architecture note in the package doc is stale. Carry three
rules out of this exercise. Keep the raw registration unexported so the once
is structurally the only entry point — `OnceFunc` dedupes its own calls, not
the operation. Call `EnsureMetrics` eagerly in `main` even though every path
guards itself, so registration failures crash at boot rather than mid-traffic.
And remember the registry is process-global: in tests, unique names and
non-parallel execution around global side effects (like the `log` redirect
here) are what keep the suite honest under `-race` and `-count=1`.

## Resources

- [expvar package](https://pkg.go.dev/expvar) — Publish "will log.Panic if the name is already registered", NewInt, Func, Handler.
- [sync.OnceFunc](https://pkg.go.dev/sync#OnceFunc) — the idempotency wrapper and its exactly-once, happens-before contract.
- [Prometheus Go client: MustRegister](https://pkg.go.dev/github.com/prometheus/client_golang/prometheus#MustRegister) — the same panic-on-duplicate contract this module models with stdlib expvar.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [08-http-client-singleton.md](08-http-client-singleton.md) | Next: [../13-building-a-thread-safe-cache/00-concepts.md](../13-building-a-thread-safe-cache/00-concepts.md)
