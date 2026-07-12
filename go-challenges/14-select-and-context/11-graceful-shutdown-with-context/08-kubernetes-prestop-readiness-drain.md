# Exercise 8: The Kubernetes Readiness-Drain Sequence Before Shutdown

On Kubernetes, SIGTERM and endpoint deregistration race: a pod can still receive
requests for a moment after SIGTERM because removing it from Service endpoints has
to propagate through the API server and every node's kube-proxy. Draining the
instant SIGTERM lands drops those requests. The fix is a deliberate sequence: fail
readiness first, wait for propagation, then drain — while keeping liveness healthy.
This module builds that `Coordinator`.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. It imports no other exercise.

## What you'll build

```text
readydrain/                module example.com/readydrain
  go.mod                   go 1.26
  coordinator.go           Coordinator; /readyz, /livez, Drain sequence, ordering log
  cmd/
    demo/
      main.go              drive the sequence; show readiness flip and a live request
  coordinator_test.go      readiness-fails-first, liveness-stays-up, request-in-window-succeeds
```

Files: `coordinator.go`, `cmd/demo/main.go`, `coordinator_test.go`.
Implement: `Coordinator` serving `/readyz` (503 once draining), `/livez` (always 200), and `Drain(ctx, httpTimeout)` that flips readiness, sleeps the propagation delay, then calls `Server.Shutdown`, recording call order.
Test: `/readyz` fails immediately while `/livez` stays 200; a request in the propagation window still succeeds; `Shutdown` is called only after the delay; the sequence fits a budget.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/14-select-and-context/11-graceful-shutdown-with-context/08-kubernetes-prestop-readiness-drain/cmd/demo
cd go-solutions/14-select-and-context/11-graceful-shutdown-with-context/08-kubernetes-prestop-readiness-drain
```

## Why fail readiness before draining

The termination sequence has an ordering that is not intuitive until you have
watched a rolling deploy throw 502s. When a pod is deleted, the kubelet sends
SIGTERM *and* the endpoints controller starts removing the pod from Service
endpoints — concurrently. That removal is eventually-consistent: it must reach the
API server, the endpoints controller must update the EndpointSlice, and every
node's kube-proxy (or the ingress/load balancer) must observe the change. Until
that propagation completes, traffic is still routed to the pod. If the process
reacts to SIGTERM by immediately calling `Server.Shutdown`, the server stops
accepting exactly while the mesh is still sending, and clients see
connection-refused or 502.

`Coordinator.Drain` fixes the ordering. Step one: flip an atomic `ready` flag so
`/readyz` returns 503. The kubelet's readiness probe sees the failure and the
endpoints controller begins removing the pod — this is the *signal* that starts
deregistration cleanly. Step two: sleep a propagation delay long enough for that
removal to reach every kube-proxy, during which the server keeps serving requests
already in flight and any stragglers still being routed. Step three, only now,
call `Server.Shutdown` to drain in-flight requests. The delay is the whole point:
it converts the SIGTERM-vs-endpoints race into a sequence.

Two constraints ride along. Liveness (`/livez`) must stay healthy the entire time
— if it fails, the kubelet concludes the container is dead and kills it early,
truncating the drain. So readiness fails (stop new traffic) while liveness stays
green (keep the drain alive). And the whole sequence — delay plus drain — must fit
inside `terminationGracePeriodSeconds`, or SIGKILL truncates it anyway. The delay
is injected so tests run in milliseconds; in production it is a few seconds.

Create `coordinator.go`:

```go
package readydrain

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Coordinator models the Kubernetes termination sequence: fail readiness, wait
// for endpoint deregistration to propagate, then drain in-flight requests, all
// while liveness stays healthy.
type Coordinator struct {
	ready            atomic.Bool
	propagationDelay time.Duration
	server           *http.Server

	mu    sync.Mutex
	order []string
}

// New returns a Coordinator that reports ready until Drain is called. The
// propagation delay is injected so tests can use milliseconds.
func New(propagationDelay time.Duration) *Coordinator {
	c := &Coordinator{propagationDelay: propagationDelay}
	c.ready.Store(true)

	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if c.ready.Load() {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	mux.HandleFunc("/livez", func(w http.ResponseWriter, r *http.Request) {
		// Liveness stays healthy throughout drain so the kubelet does not kill
		// the pod early.
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/work", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	c.server = &http.Server{Handler: mux}
	return c
}

func (c *Coordinator) record(step string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.order = append(c.order, step)
}

// Order returns the recorded shutdown steps in order.
func (c *Coordinator) Order() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.order...)
}

// Ready reports the current readiness state.
func (c *Coordinator) Ready() bool { return c.ready.Load() }

// Serve runs the health/work server on ln.
func (c *Coordinator) Serve(ln net.Listener) error {
	err := c.server.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Drain runs the termination sequence: fail readiness, wait the propagation
// delay (so endpoint deregistration reaches every kube-proxy), then drain
// in-flight requests within httpTimeout.
func (c *Coordinator) Drain(ctx context.Context, httpTimeout time.Duration) error {
	// Step 1: stop advertising readiness so new traffic stops being routed here.
	c.ready.Store(false)
	c.record("readiness-failed")

	// Step 2: wait for endpoint deregistration to propagate. The server keeps
	// serving during this window.
	select {
	case <-time.After(c.propagationDelay):
	case <-ctx.Done():
	}

	// Step 3: drain in-flight requests.
	c.record("shutdown-started")
	sctx, cancel := context.WithTimeout(context.Background(), httpTimeout)
	defer cancel()
	return c.server.Shutdown(sctx)
}
```

## The runnable demo

The demo serves the health endpoints, fires `Drain` with a 100ms propagation
delay, and during the window shows readiness already failed while a `/work`
request still succeeds — the requests-still-routed reality the delay protects.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"example.com/readydrain"
)

func status(url string) int {
	resp, err := http.Get(url)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func main() {
	c := readydrain.New(100 * time.Millisecond)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Println("listen:", err)
		return
	}
	base := "http://" + ln.Addr().String()
	go c.Serve(ln) //nolint:errcheck
	time.Sleep(20 * time.Millisecond)

	fmt.Println("readyz before drain:", status(base+"/readyz"))

	done := make(chan error, 1)
	go func() { done <- c.Drain(context.Background(), time.Second) }()

	time.Sleep(20 * time.Millisecond) // inside the propagation window
	fmt.Println("readyz during drain:", status(base+"/readyz"))
	fmt.Println("livez during drain:", status(base+"/livez"))
	fmt.Println("work during propagation window:", status(base+"/work"))

	<-done
	fmt.Println("drain order:", c.Order())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
readyz before drain: 200
readyz during drain: 503
livez during drain: 200
work during propagation window: 200
drain order: [readiness-failed shutdown-started]
```

## Tests

`TestReadinessFailsBeforeShutdown` starts `Drain` and, inside the propagation
window, asserts `/readyz` is 503, `/livez` is 200, a `/work` request still
succeeds, and `Shutdown` has not yet been called (order is just
`[readiness-failed]`); after `Drain` returns, the order is
`[readiness-failed shutdown-started]`. `TestSequenceWithinBudget` asserts the full
sequence finishes within the delay plus drain budget plus slack.

Create `coordinator_test.go`:

```go
package readydrain

import (
	"context"
	"io"
	"net"
	"net/http"
	"slices"
	"testing"
	"time"
)

func serve(t *testing.T, c *Coordinator) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		if err := c.Serve(ln); err != nil {
			t.Errorf("Serve: %v", err)
		}
	}()
	return "http://" + ln.Addr().String()
}

func status(t *testing.T, url string) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func TestReadinessFailsBeforeShutdown(t *testing.T) {
	t.Parallel()

	c := New(120 * time.Millisecond)
	base := serve(t, c)
	time.Sleep(20 * time.Millisecond)

	if got := status(t, base+"/readyz"); got != http.StatusOK {
		t.Fatalf("readyz before drain = %d, want 200", got)
	}

	done := make(chan error, 1)
	go func() { done <- c.Drain(context.Background(), time.Second) }()

	time.Sleep(30 * time.Millisecond) // inside the 120ms propagation window

	if got := status(t, base+"/readyz"); got != http.StatusServiceUnavailable {
		t.Fatalf("readyz during drain = %d, want 503", got)
	}
	if got := status(t, base+"/livez"); got != http.StatusOK {
		t.Fatalf("livez during drain = %d, want 200 (liveness must stay up)", got)
	}
	if got := status(t, base+"/work"); got != http.StatusOK {
		t.Fatalf("work in propagation window = %d, want 200 (still serving)", got)
	}
	if order := c.Order(); !slices.Equal(order, []string{"readiness-failed"}) {
		t.Fatalf("order in window = %v, want [readiness-failed] (shutdown not yet)", order)
	}

	if err := <-done; err != nil {
		t.Fatalf("Drain: %v; want nil", err)
	}
	if order := c.Order(); !slices.Equal(order, []string{"readiness-failed", "shutdown-started"}) {
		t.Fatalf("final order = %v, want [readiness-failed shutdown-started]", order)
	}
}

func TestSequenceWithinBudget(t *testing.T) {
	t.Parallel()

	const delay = 40 * time.Millisecond
	const httpTimeout = 100 * time.Millisecond
	c := New(delay)
	_ = serve(t, c)
	time.Sleep(10 * time.Millisecond)

	start := time.Now()
	if err := c.Drain(context.Background(), httpTimeout); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	elapsed := time.Since(start)

	if budget := delay + httpTimeout + 100*time.Millisecond; elapsed > budget {
		t.Fatalf("sequence took %v, want under %v", elapsed, budget)
	}
}

func ExampleCoordinator_Ready() {
	c := New(10 * time.Millisecond)
	println(c.Ready())
	// Output:
}
```

## Review

The coordinator is correct when the sequence is fail-readiness, wait, then drain —
never drain-first. `TestReadinessFailsBeforeShutdown` pins all four properties in
one window: readiness is already 503, liveness is still 200, a routed request
still succeeds, and `Shutdown` has not run yet. That window is exactly the time a
drain-first service would be dropping requests. The mistakes to avoid: skipping the
readiness flip and propagation delay (connection-refused/502 on every rolling
deploy), and failing liveness during drain (the kubelet kills the pod early and
truncates the drain). Keep the propagation delay injected so it is real seconds in
production but milliseconds in tests. Run `go test -race`; the readiness flag and
the order log are read concurrently with `Drain`.

## Resources

- [Kubernetes: Pod termination lifecycle](https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/#pod-termination) — SIGTERM, grace period, and endpoint deregistration.
- [Kubernetes: Configure liveness, readiness and startup probes](https://kubernetes.io/docs/tasks/configure-pod-container/configure-liveness-readiness-startup-probes/) — why readiness fails while liveness stays healthy.
- [sync/atomic.Bool](https://pkg.go.dev/sync/atomic#Bool) — the readiness flag read by the probe handler.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [07-inflight-request-tracking-drain.md](07-inflight-request-tracking-drain.md) | Next: [09-idempotent-double-signal-shutdown.md](09-idempotent-double-signal-shutdown.md)
