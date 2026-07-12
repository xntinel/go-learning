# Exercise 13: A Per-Tenant Webhook Dispatcher With Isolated, Dynamically Managed Lifecycles

**Level: Advanced**

A webhook fan-out service delivers events for many tenants from one process. Run
every tenant through a shared delivery loop and a single stuck endpoint stalls
delivery for everyone; the fix is one delivery goroutine per tenant, so a slow or
hung tenant only backs up its own queue. But tenants are onboarded and offboarded
at runtime, so the registry owns a *dynamic* set of goroutine lifecycles: start a
worker lazily on first `Send`, stop exactly one on demand, and close them all
under a deadline. This exercise builds that registry and the three lifecycle traps
it must survive: use-after-close, two goroutines racing to start the same tenant,
and a stuck tenant that must be detached at shutdown without being leaked.

This module is self-contained: its own module, a `dispatch` package, a demo, and
tests. Nothing here imports another exercise.

## What you'll build

```text
dispatch/                    independent module: example.com/dispatch
  go.mod                     go 1.26
  dispatch.go                Registry: lazy per-tenant workers, StopTenant, deadline-bounded Close
  cmd/demo/main.go           runnable demo: fan out to 3 tenants, stop one, close
  dispatch_test.go           isolation, exactly-one-per-tenant, StopTenant, backpressure, close-baseline, straggler
```

- Files: `dispatch.go`, `cmd/demo/main.go`, `dispatch_test.go`.
- Implement: `New(deliver Delivery, inbox int) *Registry`; `Send(tenant string, msg []byte) error` (lazy race-safe start, non-blocking enqueue, `ErrBackpressure`/`ErrClosed`); `StopTenant(tenant string) error` (signal + wait one worker, `ErrNoTenant`); `Close(ctx context.Context) error` (stop all concurrently, deadline-bounded, idempotent).
- Test: per-tenant isolation, exactly one goroutine per tenant under concurrent `Send`, `StopTenant` removes only its tenant, `Send` after `Close` returns `ErrClosed`, live count returns to baseline after `Close`, and a deadline-bounded `Close` detaches a stuck tenant that later exits cleanly.
- Verify: `go test -count=1 -race ./...`

Set up the module. `goleak` is a test dependency, so tidy after adding the import:

```bash
mkdir -p go-solutions/13-goroutines-and-channels/11-goroutine-lifecycle-management/13-per-tenant-dispatcher-registry-isolation/cmd/demo
cd go-solutions/13-goroutines-and-channels/11-goroutine-lifecycle-management/13-per-tenant-dispatcher-registry-isolation
go mod edit -go=1.26
go get go.uber.org/goleak
go mod tidy
```

### One owned lifecycle per tenant, created under a mutex

The registry holds `workers map[string]*tenantWorker` guarded by `mu`. Each
`tenantWorker` is a goroutine's *owned resource*: its own buffered `inbox`, a
`cancel` func that signals it to leave, and a `done` channel closed when it has.
That per-tenant ownership is what buys isolation — a `Delivery` that blocks parks
exactly one worker on one inbox, and every other tenant's goroutine keeps
selecting on its own inbox untouched.

Three invariants make the dynamic set safe:

1. **Exactly one worker per tenant, even under concurrent first-Send.** The
   check-then-create in `Send` (`w, ok := workers[tenant]`; if absent, start one)
   is a classic read-modify-write race: two goroutines both miss, both start a
   worker, and one silently orphans the other — a leak, and split delivery order.
   Doing the whole check-and-create while holding `mu` serialises the racing
   first-Sends into a single start. A monotonic `started` atomic, incremented
   inside the creation path, lets a test *prove* only one worker was launched no
   matter how many goroutines raced.
2. **Signal and wait are two steps, per worker.** Stopping a tenant cancels its
   context (signal) and then blocks on `<-done` (wait). The worker decrements the
   `live` counter and then closes `done` — in that defer order — so once
   `StopTenant` returns, `live` already reflects the departure. Cancelling without
   waiting would let `StopTenant` return while the goroutine is still mid-delivery.
3. **No use-after-close.** A `closed` flag under `mu` turns every post-Close
   `Send` into `ErrClosed` instead of enqueuing onto a worker that is being torn
   down. Because the worker never closes its own `inbox`, a `Send` that races Close
   and wins can at worst enqueue a message that is never delivered — never a
   send-on-closed-channel panic.

`Close` is the deadline-bounded teardown. It flips `closed`, snapshots and clears
the map under `mu`, then fans out with `wg.Go`: each goroutine cancels one worker
and races `<-w.done` against `<-ctx.Done()`. If every worker drains before the
deadline, `Close` returns nil. If one is stuck inside a `Delivery` that ignores
cancellation, that goroutine's wait loses to `ctx.Done()`, `wg.Wait` returns near
the deadline, and `Close` reports `ctx.Err()`. The stuck worker is *detached*, not
leaked: its context is already cancelled, so the moment its `Delivery` returns it
observes `ctx.Done()` and exits, driving `live` back to zero.

Create `dispatch.go`:

```go
package dispatch

import (
	"context"
	"errors"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
)

// Delivery performs one webhook delivery for a tenant. It receives the worker's
// context, which is cancelled when the tenant is stopped or the registry closes;
// a cooperative Delivery observes ctx.Done to abort in-flight work.
type Delivery func(ctx context.Context, tenant string, msg []byte)

var (
	ErrClosed       = errors.New("dispatch: closed")
	ErrNoTenant     = errors.New("dispatch: no such tenant")
	ErrBackpressure = errors.New("dispatch: tenant inbox full")
)

// tenantWorker is the per-tenant delivery goroutine's owned state: its inbox,
// the cancel func that signals it to leave, and a done channel closed when it has.
type tenantWorker struct {
	inbox  chan []byte
	cancel context.CancelFunc
	done   chan struct{}
}

// Registry fans webhook messages out to one delivery goroutine per tenant, so a
// single slow or stuck tenant cannot stall delivery for the others. Tenants start
// lazily on first Send and can be stopped individually or all at once.
type Registry struct {
	mu      sync.Mutex
	workers map[string]*tenantWorker
	deliver Delivery
	inbox   int
	closed  bool

	started atomic.Int64 // total workers ever launched (monotonic)
	live    atomic.Int64 // worker goroutines currently running
}

// New returns a Registry that runs deliver in a dedicated goroutine per tenant,
// each with an inbox buffered to inbox messages.
func New(deliver Delivery, inbox int) *Registry {
	return &Registry{
		workers: make(map[string]*tenantWorker),
		deliver: deliver,
		inbox:   inbox,
	}
}

// Send enqueues msg for tenant, lazily and race-safely starting that tenant's
// worker on first use. Enqueue is non-blocking: a full inbox returns
// ErrBackpressure rather than stalling the caller. After Close it returns
// ErrClosed.
func (r *Registry) Send(tenant string, msg []byte) error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return ErrClosed
	}
	w, ok := r.workers[tenant]
	if !ok {
		w = r.startWorkerLocked(tenant)
	}
	r.mu.Unlock()

	select {
	case w.inbox <- msg:
		return nil
	default:
		return ErrBackpressure
	}
}

// startWorkerLocked creates and launches exactly one worker for tenant. The
// caller holds r.mu, which is what serialises concurrent first-Sends to the same
// tenant into a single start.
func (r *Registry) startWorkerLocked(tenant string) *tenantWorker {
	ctx, cancel := context.WithCancel(context.Background())
	w := &tenantWorker{
		inbox:  make(chan []byte, r.inbox),
		cancel: cancel,
		done:   make(chan struct{}),
	}
	r.workers[tenant] = w
	r.started.Add(1)
	r.live.Add(1)
	go r.run(ctx, tenant, w)
	return w
}

func (r *Registry) run(ctx context.Context, tenant string, w *tenantWorker) {
	defer close(w.done)  // runs last: done closes only after live is decremented
	defer r.live.Add(-1) // runs first
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-w.inbox:
			r.deliver(ctx, tenant, msg)
		}
	}
}

// StopTenant signals one tenant's worker to leave and waits for it to exit, then
// removes it from the registry. Unknown tenants return ErrNoTenant.
func (r *Registry) StopTenant(tenant string) error {
	r.mu.Lock()
	w, ok := r.workers[tenant]
	if !ok {
		r.mu.Unlock()
		return ErrNoTenant
	}
	delete(r.workers, tenant)
	r.mu.Unlock()

	w.cancel() // signal
	<-w.done   // wait
	return nil
}

// Close stops every tenant concurrently and waits, bounded by ctx. If a stuck
// Delivery does not return before ctx's deadline, Close stops waiting on that
// straggler (detaching it, not leaking it: it still exits once its Delivery
// unblocks) and returns ctx.Err. Close is idempotent.
func (r *Registry) Close(ctx context.Context) error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	workers := slices.Collect(maps.Values(r.workers))
	clear(r.workers)
	r.mu.Unlock()

	var straggler atomic.Bool
	var wg sync.WaitGroup
	for _, w := range workers {
		wg.Go(func() {
			w.cancel() // signal
			select {
			case <-w.done: // waited: it left in time
			case <-ctx.Done(): // deadline: detach, do not wait further
				straggler.Store(true)
			}
		})
	}
	wg.Wait()

	if straggler.Load() {
		return ctx.Err()
	}
	return nil
}

// Live reports the number of tenant worker goroutines currently running.
func (r *Registry) Live() int64 { return r.live.Load() }

// Started reports the total number of workers ever launched (monotonic).
func (r *Registry) Started() int64 { return r.started.Load() }
```

### The runnable demo

The demo fans nine events out to three tenants, using a `WaitGroup` decremented by
each delivery as a deterministic barrier: `wg.Wait` returns only once every message
has been delivered, so the live count and per-tenant totals it prints are stable.
It then stops one tenant, shows `StopTenant` on an unknown tenant, closes the
registry, and shows a post-close `Send` rejected. Output order is fixed by
iterating a slice, never a map.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"example.com/dispatch"
)

func main() {
	var mu sync.Mutex
	counts := map[string]int{}
	var wg sync.WaitGroup

	// Each delivery decrements wg, so wg.Wait gives a deterministic barrier:
	// it returns only once every enqueued message has been delivered.
	deliver := func(ctx context.Context, tenant string, msg []byte) {
		mu.Lock()
		counts[tenant]++
		mu.Unlock()
		wg.Done()
	}

	r := dispatch.New(deliver, 8)

	tenants := []string{"acme", "globex", "initech"}
	for _, t := range tenants {
		for range 3 {
			wg.Add(1)
			if err := r.Send(t, []byte("event")); err != nil {
				log.Fatalf("Send(%s): %v", t, err)
			}
		}
	}
	wg.Wait()

	for _, t := range tenants {
		fmt.Printf("delivered %s=%d\n", t, counts[t])
	}
	fmt.Println("live workers:", r.Live())

	if err := r.StopTenant("globex"); err != nil {
		log.Fatalf("StopTenant: %v", err)
	}
	fmt.Println("after StopTenant(globex) live:", r.Live())
	fmt.Println("StopTenant(unknown):", r.StopTenant("unknown"))

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	fmt.Println("close error:", r.Close(ctx))
	fmt.Println("send after close:", r.Send("acme", []byte("x")))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
delivered acme=3
delivered globex=3
delivered initech=3
live workers: 3
after StopTenant(globex) live: 2
StopTenant(unknown): dispatch: no such tenant
close error: <nil>
send after close: dispatch: closed
```

### Tests

`TestMain` registers `goleak.VerifyTestMain`, which fails the run if any worker
goroutine survives the suite — the ultimate proof that every lifecycle was closed.
`TestPerTenantIsolation` parks a "slow" tenant in a `Delivery` that ignores its
context and asserts three other tenants still deliver within the timeout, pinning
the one-goroutine-per-tenant isolation. `TestExactlyOneWorkerPerTenant` fires 100
concurrent `Send` calls at the same tenant and asserts `Started() == 1` and
`Live() == 1` — the mutex-serialised start. `TestStopTenantRemovesOnlyThatTenant`
stops one of two tenants, asserts `Live()` drops by exactly one and the survivor
keeps delivering, and that `StopTenant` on an unknown tenant returns `ErrNoTenant`.
`TestBackpressureWhenInboxFull` parks the worker inside a delivery, fills the
two-slot inbox, and asserts the overflowing `Send` returns `ErrBackpressure`.
`TestSendAfterCloseReturnsErrClosed` proves post-close `Send` is `ErrClosed` and
`Close` is idempotent. `TestCloseReturnsToBaseline` drains four tenants, closes,
and asserts `Live() == 0`. `TestCloseDeadlineDetachesStraggler` blocks one tenant,
closes under a 50ms deadline, asserts `Close` returns an error near that deadline,
then releases the delivery and confirms the detached worker exits (`Live()` reaches
zero) — detached, not leaked.

Create `dispatch_test.go`:

```go
package dispatch

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func closeReg(t *testing.T, r *Registry) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := r.Close(ctx); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// A tenant whose Delivery blocks must not stall delivery for the others.
func TestPerTenantIsolation(t *testing.T) {
	t.Parallel()

	block := make(chan struct{})
	fast := make(chan string, 16)
	deliver := func(ctx context.Context, tenant string, msg []byte) {
		if tenant == "slow" {
			<-block // truly stuck: ignores ctx
			return
		}
		fast <- tenant
	}
	r := New(deliver, 4)
	defer func() {
		close(block)
		closeReg(t, r)
	}()

	if err := r.Send("slow", []byte("x")); err != nil {
		t.Fatalf("Send(slow): %v", err)
	}

	others := []string{"a", "b", "c"}
	for _, tn := range others {
		if err := r.Send(tn, []byte("y")); err != nil {
			t.Fatalf("Send(%s): %v", tn, err)
		}
	}

	got := map[string]bool{}
	timeout := time.After(2 * time.Second)
	for range others {
		select {
		case tn := <-fast:
			got[tn] = true
		case <-timeout:
			t.Fatal("fast tenants were blocked by the slow tenant")
		}
	}
	for _, tn := range others {
		if !got[tn] {
			t.Fatalf("tenant %s was not delivered", tn)
		}
	}
}

// Many goroutines racing to first-Send the same tenant must start exactly one
// worker.
func TestExactlyOneWorkerPerTenant(t *testing.T) {
	t.Parallel()

	r := New(func(context.Context, string, []byte) {}, 32)
	defer closeReg(t, r)

	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() { _ = r.Send("acme", []byte("x")) })
	}
	wg.Wait()

	if n := r.Started(); n != 1 {
		t.Fatalf("Started() = %d, want 1", n)
	}
	if n := r.Live(); n != 1 {
		t.Fatalf("Live() = %d, want 1", n)
	}
}

// StopTenant removes only that tenant; others keep delivering; unknown -> ErrNoTenant.
func TestStopTenantRemovesOnlyThatTenant(t *testing.T) {
	t.Parallel()

	done := make(chan string, 64)
	deliver := func(ctx context.Context, tenant string, msg []byte) {
		done <- tenant
	}
	r := New(deliver, 8)
	defer closeReg(t, r)

	if err := r.Send("a", []byte("1")); err != nil {
		t.Fatalf("Send(a): %v", err)
	}
	if err := r.Send("b", []byte("1")); err != nil {
		t.Fatalf("Send(b): %v", err)
	}
	<-done
	<-done
	if n := r.Live(); n != 2 {
		t.Fatalf("Live() = %d, want 2", n)
	}

	if err := r.StopTenant("a"); err != nil {
		t.Fatalf("StopTenant(a): %v", err)
	}
	if n := r.Live(); n != 1 {
		t.Fatalf("Live() after StopTenant = %d, want 1", n)
	}

	if err := r.Send("b", []byte("2")); err != nil {
		t.Fatalf("Send(b): %v", err)
	}
	select {
	case tn := <-done:
		if tn != "b" {
			t.Fatalf("delivered from %q, want b", tn)
		}
	case <-time.After(time.Second):
		t.Fatal("tenant b stopped delivering after StopTenant(a)")
	}

	if err := r.StopTenant("zzz"); !errors.Is(err, ErrNoTenant) {
		t.Fatalf("StopTenant(unknown) = %v, want ErrNoTenant", err)
	}
}

// A full inbox returns ErrBackpressure instead of blocking the caller.
func TestBackpressureWhenInboxFull(t *testing.T) {
	t.Parallel()

	entered := make(chan struct{}, 1)
	block := make(chan struct{})
	deliver := func(ctx context.Context, tenant string, msg []byte) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-block
	}
	r := New(deliver, 2)
	defer func() {
		close(block)
		closeReg(t, r)
	}()

	// First message is pulled off the inbox by the worker, which then blocks in
	// deliver; wait for that so the buffer state below is deterministic.
	if err := r.Send("t", []byte("1")); err != nil {
		t.Fatalf("Send 1: %v", err)
	}
	<-entered

	// Inbox now empty with the worker parked; fill its two slots, then overflow.
	if err := r.Send("t", []byte("2")); err != nil {
		t.Fatalf("Send 2: %v", err)
	}
	if err := r.Send("t", []byte("3")); err != nil {
		t.Fatalf("Send 3: %v", err)
	}
	if err := r.Send("t", []byte("4")); !errors.Is(err, ErrBackpressure) {
		t.Fatalf("Send 4 = %v, want ErrBackpressure", err)
	}
}

// Send after Close returns ErrClosed; Close is idempotent.
func TestSendAfterCloseReturnsErrClosed(t *testing.T) {
	t.Parallel()

	r := New(func(context.Context, string, []byte) {}, 4)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := r.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := r.Send("a", []byte("x")); !errors.Is(err, ErrClosed) {
		t.Fatalf("Send after Close = %v, want ErrClosed", err)
	}
	if err := r.Close(ctx); err != nil { // idempotent
		t.Fatalf("second Close: %v", err)
	}
}

// Closing several idle tenants drives the live goroutine count back to zero.
func TestCloseReturnsToBaseline(t *testing.T) {
	t.Parallel()

	done := make(chan string, 64)
	r := New(func(ctx context.Context, tenant string, msg []byte) { done <- tenant }, 8)

	tenants := []string{"a", "b", "c", "d"}
	for _, tn := range tenants {
		if err := r.Send(tn, []byte("x")); err != nil {
			t.Fatalf("Send(%s): %v", tn, err)
		}
	}
	for range tenants {
		<-done
	}
	if n := r.Live(); n != int64(len(tenants)) {
		t.Fatalf("Live() = %d, want %d", n, len(tenants))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := r.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if n := r.Live(); n != 0 {
		t.Fatalf("Live() after Close = %d, want 0", n)
	}
}

// A deadline-bounded Close with one stuck tenant returns near its deadline, and
// the detached straggler exits cleanly once unblocked (detached, not leaked).
func TestCloseDeadlineDetachesStraggler(t *testing.T) {
	t.Parallel()

	entered := make(chan struct{}, 1)
	block := make(chan struct{})
	deliver := func(ctx context.Context, tenant string, msg []byte) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-block // ignores ctx: truly stuck until the test releases it
	}
	r := New(deliver, 4)

	if err := r.Send("stuck", []byte("x")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	<-entered // the worker is now inside a blocked Delivery

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := r.Close(ctx)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Close returned nil, want a deadline error")
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("Close blocked on the straggler for %v, want ~deadline", elapsed)
	}

	// Detached, not leaked: release the Delivery and the straggler exits.
	close(block)
	deadline := time.Now().Add(2 * time.Second)
	for r.Live() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("detached straggler never exited")
		}
		time.Sleep(time.Millisecond)
	}
}
```

## Review

Correct here means every tenant is an independently owned lifecycle: one goroutine
started exactly once, stoppable in isolation, and guaranteed to exit. The
isolation guarantee comes from giving each tenant its own inbox and goroutine, so a
blocked `Delivery` parks only its own worker — `TestPerTenantIsolation` proves it
by keeping three tenants flowing past a stuck one. The exactly-one guarantee comes
from performing the map check-and-create entirely under `mu`, collapsing racing
first-Sends into a single start that `TestExactlyOneWorkerPerTenant` pins with
`Started() == 1` under 100 concurrent callers. The no-leak guarantee comes from the
signal-then-wait discipline (`cancel()` then `<-done`, with `live` decremented
before `done` closes) and from `Close` racing each wait against `ctx.Done()` so one
stuck tenant cannot hang shutdown; the detached worker still exits because its
context was already cancelled, which `TestCloseDeadlineDetachesStraggler` verifies
and `goleak.VerifyTestMain` enforces globally. The production bug this pattern
prevents is the one that only shows up at scale: a fan-out service where a single
hung customer endpoint silently blocks the shared delivery loop for every other
customer, or where onboarding churn slowly leaks a goroutine per departed tenant
until the process is OOM-killed. Run `go test -count=1 -race ./...`; `-race` is
what exposes any unsynchronised access to the workers map or the counters.

## Resources

- [`context.WithCancel`](https://pkg.go.dev/context#WithCancel) -- the per-worker cancellation signal that unblocks a cooperative Delivery and drives the run loop's exit.
- [`sync.WaitGroup.Go`](https://pkg.go.dev/sync#WaitGroup.Go) -- the Go 1.25 launch-and-track helper used to stop every tenant concurrently in Close.
- [`go.uber.org/goleak`](https://pkg.go.dev/go.uber.org/goleak) -- fails the test run if any tenant worker outlives the suite, proving every lifecycle was closed.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) -- the cancellation-and-fan-out signalling patterns this registry is built from.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [12-run-group-coordinated-component-shutdown.md](12-run-group-coordinated-component-shutdown.md) | Next: [../12-channel-patterns-semaphore-barrier/00-concepts.md](../12-channel-patterns-semaphore-barrier/00-concepts.md)
