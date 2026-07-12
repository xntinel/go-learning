# Exercise 2: Drain Flag Behind a Readiness Probe

Graceful shutdown starts with a single boolean: "are we draining?". The `/readyz`
handler reads it to start failing readiness so the load balancer stops sending new
traffic, and workers read it to stop pulling new jobs. This exercise builds that
drain gate with `atomic.Bool`, and uses `CompareAndSwap` to give it one-shot
semantics: exactly one caller learns that *it* was the one that started the drain.

This module is fully self-contained.

## What you'll build

```text
draingate/                 independent module: example.com/draingate
  go.mod
  gate.go                  type Gate; Request (CAS, one-shot), Requested, ReadyHandler
  cmd/
    demo/
      main.go              flips the gate and shows the readiness response change
  gate_test.go             concurrent one-winner test, httptest readiness test, Example
```

- Files: `gate.go`, `cmd/demo/main.go`, `gate_test.go`.
- Implement: a `Gate` with `atomic.Bool`; `Request` returns whether THIS caller initiated the drain, `Requested` reports the state, `ReadyHandler` returns 503 while draining.
- Test: 100 goroutines call `Request` concurrently and exactly one sees `true`; an `httptest` check of the readiness flip.
- Verify: `go test -count=1 -race ./...`

### Why CompareAndSwap for a one-shot flag

The plain version of a drain flag is `Store(true)` / `Load()`, and for the readiness
handler that is all you need — any number of callers can request the drain and the
handler just reads the current state. But real shutdown code wants to run a
one-shot side effect exactly once: log "draining, stopping intake", close the
accept loop, start the shutdown timer. If every caller of `Request` ran that, a
racing SIGTERM handler and health check could fire it twice.

`CompareAndSwap(false, true)` solves this without a mutex or a `sync.Once`. It
atomically flips the flag from `false` to `true` *and* tells the caller whether it
was the one that did the flip: it returns `true` only for the single goroutine that
observed the `false`-to-`true` transition; everyone arriving afterward sees the
value is already `true`, the CAS fails, and they get `false`. So `Request` doubles
as both "signal drain" and "am I the initiator?". The initiator can run the
one-shot side effect; the rest simply return.

`Requested` is a plain `Load`: cheap, wait-free, safe to call on every readiness
probe and in every worker's loop condition. The whole point of atomics here is that
a readiness handler hit dozens of times a second never contends on a lock.

Create `gate.go`:

```go
package draingate

import (
	"io"
	"net/http"
	"sync/atomic"
)

// Gate is a one-shot drain signal for graceful shutdown. Requested is read on
// every readiness probe and in worker loop conditions; Request flips it once.
type Gate struct {
	draining atomic.Bool
}

// Request signals that the process should start draining. It returns true only
// for the single caller that transitioned the gate from live to draining; every
// later caller returns false. Use the true result to run one-shot shutdown work.
func (g *Gate) Request() bool {
	return g.draining.CompareAndSwap(false, true)
}

// Requested reports whether a drain has been requested. It is a wait-free load,
// cheap to call on every readiness probe.
func (g *Gate) Requested() bool {
	return g.draining.Load()
}

// ReadyHandler serves readiness: 200 while live, 503 once draining so the load
// balancer stops routing new traffic.
func (g *Gate) ReadyHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if g.Requested() {
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, "draining")
			return
		}
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "ready")
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http/httptest"

	"example.com/draingate"
)

func main() {
	g := &draingate.Gate{}

	fmt.Println("requested before:", g.Requested())
	fmt.Println("probe before:", probe(g))

	first := g.Request()
	second := g.Request()
	fmt.Println("first Request initiated:", first)
	fmt.Println("second Request initiated:", second)

	fmt.Println("requested after:", g.Requested())
	fmt.Println("probe after:", probe(g))
}

func probe(g *draingate.Gate) int {
	rec := httptest.NewRecorder()
	g.ReadyHandler()(rec, httptest.NewRequest("GET", "/readyz", nil))
	return rec.Code
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
requested before: false
probe before: 200
first Request initiated: true
second Request initiated: false
requested after: true
probe after: 503
```

### Tests

`TestOneWinner` launches 100 goroutines all calling `Request`; exactly one must see
`true`, and afterward `Requested` must be `true`. `TestReadiness` drives the handler
before and after the flip.

Create `gate_test.go`:

```go
package draingate

import (
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

func TestOneWinner(t *testing.T) {
	t.Parallel()

	g := &Gate{}
	const goroutines = 100
	var winners atomic.Int64
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			if g.Request() {
				winners.Add(1)
			}
		}()
	}
	wg.Wait()

	if got := winners.Load(); got != 1 {
		t.Fatalf("initiators = %d, want exactly 1", got)
	}
	if !g.Requested() {
		t.Fatal("Requested() = false after concurrent Request calls")
	}
}

func TestReadiness(t *testing.T) {
	t.Parallel()

	g := &Gate{}
	code := func() int {
		rec := httptest.NewRecorder()
		g.ReadyHandler()(rec, httptest.NewRequest("GET", "/readyz", nil))
		return rec.Code
	}

	if got := code(); got != 200 {
		t.Fatalf("ready before drain = %d, want 200", got)
	}
	g.Request()
	if got := code(); got != 503 {
		t.Fatalf("ready after drain = %d, want 503", got)
	}
}

func TestIdempotentRequest(t *testing.T) {
	t.Parallel()

	g := &Gate{}
	if !g.Request() {
		t.Fatal("first Request should initiate (true)")
	}
	if g.Request() {
		t.Fatal("second Request should not initiate (false)")
	}
}
```

## Review

The gate is correct when `Request` returns `true` for exactly one caller across any
number of concurrent calls and `Requested` is `true` forever after the first — that
is what `CompareAndSwap(false, true)` buys you over `Store`. The trap to avoid is
running a one-shot side effect on the result of `Store`/`Load`, which fires for
every caller; gate the side effect on the `true` from `Request` instead. `Requested`
stays a plain `Load` precisely so the hot readiness path never takes a lock.

## Resources

- [`atomic.Bool.CompareAndSwap`](https://pkg.go.dev/sync/atomic#Bool.CompareAndSwap) — the one-shot transition primitive.
- [Kubernetes readiness probes](https://kubernetes.io/docs/tasks/configure-pod-container/configure-liveness-readiness-startup-probes/) — how `/readyz` gates traffic during drain.
- [`net/http/httptest`](https://pkg.go.dev/net/http/httptest) — driving the handler in tests.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-request-metrics-counters.md](01-request-metrics-counters.md) | Next: [03-cas-state-machine.md](03-cas-state-machine.md)
