# Exercise 13: Per-Tenant Isolating Webhook Fan-Out Dispatcher

**Level: Advanced**

A webhook delivery service consumes one mixed intake channel carrying deliveries
for every tenant, and must guarantee tenant isolation: one tenant whose endpoint is
slow or wedged cannot delay or block delivery for anyone else. The naive design --
one channel, one draining loop -- head-of-line-blocks the whole service the moment a
single endpoint hangs. This exercise builds a dispatcher that ranges the intake
stream and routes each delivery to a lazily-created per-tenant channel, each drained
by its own goroutine ranging that channel, so a slow tenant backs up only its own
queue; on intake close it closes every per-tenant channel exactly once, waits for
all drains, and leaks no goroutine.

This module is self-contained: its own module, a `tenantfanout` package, a demo, and
tests. Nothing here imports another exercise.

## What you'll build

```text
tenantfanout/                independent module: example.com/tenantfanout
  go.mod                     go 1.26 (requires go.uber.org/goleak for the leak check)
  tenantfanout.go            type Delivery; Dispatcher with Run and Counts
  cmd/demo/main.go           runnable demo: three tenants routed from one intake stream
  tenantfanout_test.go       per-tenant order + counts, isolation proof, exactly-once close, empty intake
```

- Files: `tenantfanout.go`, `cmd/demo/main.go`, `tenantfanout_test.go`.
- Implement: `NewDispatcher(deliver func(Delivery), perTenantBuffer int) *Dispatcher`, `(*Dispatcher).Run(in <-chan Delivery)` which ranges `in`, fans each delivery to its tenant's own ranged channel, and returns only after `in` is closed and every per-tenant queue is fully drained, and `(*Dispatcher).Counts() map[string]int`.
- Test: per-tenant order is preserved and `Counts()` totals equal the intake partition; a wedged tenant's deliveries never block another tenant (deterministic via channels, not sleeps); no send-on-closed / close-of-closed panic across many tenants under `-race`; and zero leaked goroutines after `Run` returns.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/tenantfanout/cmd/demo
cd ~/go-exercises/tenantfanout
go mod init example.com/tenantfanout
go get go.uber.org/goleak
go mod tidy
```

### One intake, many owners: where isolation actually comes from

The dangerous version of this service has one channel and one `for range` that calls
`deliver` inline. When one tenant's HTTP endpoint hangs, that `deliver` blocks, the
range loop stops pulling, and every subsequent delivery -- for every other tenant --
waits behind it. That is head-of-line blocking, and it is how a single misbehaving
customer takes down delivery for all of them.

The fix is to separate routing from delivery. A single Run goroutine ranges the
intake stream and does nothing slow: it only looks up (or creates) the target
tenant's channel and hands the delivery off. The actual `deliver` call happens in a
*different* goroutine, one per tenant, each ranging its own channel. A wedged
endpoint now blocks only its own drain goroutine; its channel fills, but every other
tenant's drain keeps running. The ownership rule from the concept file holds cleanly:
Run is the sole sender on every per-tenant channel, so Run -- and only Run -- closes
them, exactly once each, after intake ends.

There is one honest limit, and naming it is the point of the exercise. A buffered
per-tenant channel decouples Run from a slow drain only up to its buffer. The moment
a wedged tenant's backlog exceeds `perTenantBuffer`, Run blocks on the send to that
full channel and isolation is lost. So `perTenantBuffer` is the isolation budget:
the amount of in-flight backlog one tenant may accumulate before backpressure reaches
the fan-out loop. In production you size it, then shed or spill beyond it; here the
test sizes it to hold the wedged tenant's whole backlog, so the fan-out loop never
stalls and the isolation property is exact.

The lifecycle is a three-step protocol Run runs in order:

1. Range `in`. For each delivery, find the tenant's channel or, on first sight,
   `make` it and start its drain via `wg.Go`. Then send the delivery to that channel.
2. When `in` closes, the range ends. Close every per-tenant channel exactly once --
   the broadcast that ends each drain's own `range`.
3. `wg.Wait()` for all drains to finish delivering their buffered backlog, then
   return. Because every drain ends when its channel closes, no goroutine leaks.

Create `tenantfanout.go`:

```go
// Package tenantfanout routes a single mixed intake stream of webhook deliveries
// into one lazily-created, independently-drained channel per tenant, so a slow or
// wedged tenant endpoint backs up only its own queue and never head-of-line-blocks
// any other tenant.
package tenantfanout

import (
	"maps"
	"sync"
)

// Delivery is one webhook to send: which tenant it belongs to and its payload.
type Delivery struct {
	Tenant  string
	Payload string
}

// Dispatcher fans a mixed intake stream out to per-tenant channels. Each tenant
// gets its own buffered channel and its own draining goroutine that ranges that
// channel and calls deliver in arrival order. perTenantBuffer is the isolation
// budget: up to that many in-flight deliveries for one tenant are absorbed without
// stalling the fan-out loop, so a slow tenant is isolated as long as its backlog
// fits the buffer.
type Dispatcher struct {
	deliver         func(Delivery)
	perTenantBuffer int

	// queues is touched only by the single Run goroutine (create, send, close), so
	// it needs no lock.
	queues map[string]chan Delivery
	wg     sync.WaitGroup

	// counts is written by every drain goroutine, so it is mutex-guarded.
	mu     sync.Mutex
	counts map[string]int
}

// NewDispatcher builds a Dispatcher that calls deliver for each routed delivery and
// gives every tenant a channel buffered to perTenantBuffer.
func NewDispatcher(deliver func(Delivery), perTenantBuffer int) *Dispatcher {
	return &Dispatcher{
		deliver:         deliver,
		perTenantBuffer: perTenantBuffer,
		queues:          make(map[string]chan Delivery),
		counts:          make(map[string]int),
	}
}

// Run ranges the intake channel, routing each delivery to its tenant's own channel
// (creating that channel and its drain goroutine on first sight of the tenant).
// When in is closed and drained, Run closes every per-tenant channel exactly once,
// waits for all drains to finish delivering their backlog, and only then returns.
// It leaks no goroutine: every drain ends when its channel closes.
func (d *Dispatcher) Run(in <-chan Delivery) {
	for del := range in {
		q, ok := d.queues[del.Tenant]
		if !ok {
			q = make(chan Delivery, d.perTenantBuffer)
			d.queues[del.Tenant] = q
			d.wg.Go(func() { d.drain(q) })
		}
		q <- del
	}
	// Run is the sole sender on every per-tenant channel, so Run is the one that
	// closes them, exactly once each, after intake ends.
	for _, q := range d.queues {
		close(q)
	}
	d.wg.Wait()
}

// drain ranges one tenant's channel until Run closes it, delivering each item in
// arrival order and counting it. Because each tenant has exactly one drain, order
// within a tenant is preserved and the count is a simple per-tenant tally.
func (d *Dispatcher) drain(q <-chan Delivery) {
	for del := range q {
		d.deliver(del)
		d.mu.Lock()
		d.counts[del.Tenant]++
		d.mu.Unlock()
	}
}

// Counts returns a snapshot of delivered counts per tenant. After Run returns it is
// the final tally and its totals equal the intake partition.
func (d *Dispatcher) Counts() map[string]int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return maps.Clone(d.counts)
}
```

### The runnable demo

The demo routes six deliveries across three tenants from one intake stream and prints
the per-tenant tally in sorted key order, so the output is deterministic regardless
of goroutine scheduling or map iteration order.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"maps"
	"slices"

	"example.com/tenantfanout"
)

func main() {
	// A mixed intake stream: three tenants interleaved on one channel.
	deliveries := []tenantfanout.Delivery{
		{Tenant: "billing", Payload: "invoice-1"},
		{Tenant: "email", Payload: "welcome"},
		{Tenant: "sms", Payload: "otp"},
		{Tenant: "billing", Payload: "invoice-2"},
		{Tenant: "email", Payload: "receipt"},
		{Tenant: "billing", Payload: "invoice-3"},
	}

	// deliver just records; the dispatcher guarantees per-tenant order and isolation.
	d := tenantfanout.NewDispatcher(func(tenantfanout.Delivery) {}, 8)

	in := make(chan tenantfanout.Delivery)
	go func() {
		for _, del := range deliveries {
			in <- del
		}
		close(in)
	}()

	d.Run(in) // returns only after every per-tenant queue is fully drained

	counts := d.Counts()
	fmt.Printf("routing %d deliveries across %d tenants\n", len(deliveries), len(counts))
	total := 0
	for _, tenant := range slices.Sorted(maps.Keys(counts)) {
		fmt.Printf("tenant %-8s %d delivered\n", tenant, counts[tenant])
		total += counts[tenant]
	}
	fmt.Printf("total delivered: %d\n", total)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
routing 6 deliveries across 3 tenants
tenant billing  3 delivered
tenant email    2 delivered
tenant sms      1 delivered
total delivered: 6
```

### Tests

`TestPerTenantOrderAndCounts` sends five payloads for each of three tenants and
asserts each tenant's `deliver` sequence matches intake order and `Counts()` totals
equal the partition. `TestIsolationSlowTenantDoesNotBlockOthers` is the core proof,
deterministic via channels: tenant A's `deliver` parks on a gate while the test
receives a completion signal for every one of tenant B's deliveries -- all of them
observed *before* the gate is released -- proving B was never head-of-line-blocked by
A; the buffer is sized to hold A's whole backlog so the fan-out loop never stalls.
`TestExactlyOnceCloseManyTenants` drives fifty tenants and, under `-race`, pins down
that each per-tenant channel is closed exactly once with no send-on-closed panic.
`TestEmptyIntake` covers the degenerate no-delivery case. `TestMain` wraps the run in
`goleak.VerifyTestMain`, failing if any drain goroutine outlives the tests.

Create `tenantfanout_test.go`:

```go
package tenantfanout

import (
	"fmt"
	"sync"
	"testing"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	// Fails the run if any goroutine outlives the tests: proves every drain returns.
	goleak.VerifyTestMain(m)
}

// feed sends every delivery on a fresh channel and closes it, from a goroutine, so
// Run drives the intake exactly as production would.
func feed(deliveries []Delivery) <-chan Delivery {
	in := make(chan Delivery)
	go func() {
		defer close(in)
		for _, d := range deliveries {
			in <- d
		}
	}()
	return in
}

// TestPerTenantOrderAndCounts pins down that each tenant's payloads arrive at
// deliver in intake order and that Counts totals equal the intake partition.
func TestPerTenantOrderAndCounts(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	got := map[string][]string{}
	d := NewDispatcher(func(del Delivery) {
		mu.Lock()
		got[del.Tenant] = append(got[del.Tenant], del.Payload)
		mu.Unlock()
	}, 4)

	var in []Delivery
	want := map[string][]string{}
	for _, tenant := range []string{"a", "b", "c"} {
		for i := range 5 {
			p := fmt.Sprintf("%s-%d", tenant, i)
			in = append(in, Delivery{Tenant: tenant, Payload: p})
			want[tenant] = append(want[tenant], p)
		}
	}

	d.Run(feed(in))

	for tenant, seq := range want {
		if fmt.Sprint(got[tenant]) != fmt.Sprint(seq) {
			t.Fatalf("tenant %s order = %v, want %v", tenant, got[tenant], seq)
		}
	}
	counts := d.Counts()
	for tenant, seq := range want {
		if counts[tenant] != len(seq) {
			t.Fatalf("Counts()[%s] = %d, want %d", tenant, counts[tenant], len(seq))
		}
	}
}

// TestIsolationSlowTenantDoesNotBlockOthers is the core proof, deterministic via
// channels and not sleeps. Tenant A's deliver blocks on a gate; the test observes
// every B delivery complete BEFORE it releases the gate, proving B was never
// head-of-line-blocked behind A. The per-tenant buffer is sized to hold all of A's
// backlog so the fan-out loop never stalls on A's full queue.
func TestIsolationSlowTenantDoesNotBlockOthers(t *testing.T) {
	t.Parallel()

	const nA, nB = 6, 8
	gate := make(chan struct{})    // A's deliver parks here until released
	bDone := make(chan string, nB) // one signal per completed B delivery

	deliver := func(del Delivery) {
		switch del.Tenant {
		case "A":
			<-gate // wedged endpoint: blocks until teardown
		case "B":
			bDone <- del.Payload
		}
	}
	// Buffer >= nA so Run can park all of A's backlog without blocking on the send.
	d := NewDispatcher(deliver, nA)

	var in []Delivery
	for i := range nA {
		in = append(in, Delivery{Tenant: "A", Payload: fmt.Sprintf("a-%d", i)})
	}
	for i := range nB {
		in = append(in, Delivery{Tenant: "B", Payload: fmt.Sprintf("b-%d", i)})
	}

	done := make(chan struct{})
	go func() {
		d.Run(feed(in)) // blocks until A is released, so run it off the test goroutine
		close(done)
	}()

	// Every B must finish while A is still gated. Receiving all nB signals here,
	// before close(gate) below, is the isolation proof.
	for range nB {
		<-bDone
	}

	select {
	case <-done:
		t.Fatal("Run returned while tenant A was still gated: A was not isolated")
	default:
	}

	close(gate) // release the wedged tenant at teardown
	<-done      // now every drain, including A, finishes and Run returns

	counts := d.Counts()
	if counts["A"] != nA {
		t.Fatalf("Counts()[A] = %d, want %d", counts["A"], nA)
	}
	if counts["B"] != nB {
		t.Fatalf("Counts()[B] = %d, want %d", counts["B"], nB)
	}
}

// TestExactlyOnceCloseManyTenants drives many tenants with many deliveries each and
// asserts no send-on-closed / close-of-closed panic and correct totals. Run under
// -race this pins down that Run closes each per-tenant channel exactly once.
func TestExactlyOnceCloseManyTenants(t *testing.T) {
	t.Parallel()

	const tenants, per = 50, 20
	d := NewDispatcher(func(Delivery) {}, 2)

	var in []Delivery
	for round := range per {
		for id := range tenants {
			in = append(in, Delivery{
				Tenant:  fmt.Sprintf("t%02d", id),
				Payload: fmt.Sprintf("p-%d", round),
			})
		}
	}

	d.Run(feed(in))

	counts := d.Counts()
	if len(counts) != tenants {
		t.Fatalf("len(Counts()) = %d, want %d", len(counts), tenants)
	}
	total := 0
	for tenant, c := range counts {
		if c != per {
			t.Fatalf("Counts()[%s] = %d, want %d", tenant, c, per)
		}
		total += c
	}
	if total != tenants*per {
		t.Fatalf("total delivered = %d, want %d", total, tenants*per)
	}
}

// TestEmptyIntake pins down the degenerate case: no deliveries means no per-tenant
// channels, no drains, and an immediate clean return with empty Counts.
func TestEmptyIntake(t *testing.T) {
	t.Parallel()

	d := NewDispatcher(func(Delivery) {}, 4)
	d.Run(feed(nil))
	if len(d.Counts()) != 0 {
		t.Fatalf("Counts() = %v, want empty", d.Counts())
	}
}
```

## Review

Correct here means three things at once: per-tenant FIFO (each tenant has exactly one
drain ranging one channel, so arrival order is preserved), isolation (a wedged tenant
blocks only its own drain, never the fan-out loop, as long as its backlog fits
`perTenantBuffer`), and clean teardown (every per-tenant channel closed exactly once
by its sole sender, all drains awaited, no leak). The single-owner rule is what makes
the exactly-once close safe -- Run is the only sender, so Run is the only closer, and
`wg.Wait` after the close loop guarantees every buffered delivery is flushed before
`Run` returns. The isolation test proves the property without any sleep: it drains
every one of tenant B's completion signals while tenant A's `deliver` is still parked
on the gate, so B provably finished without waiting for A, and `goleak` confirms that
releasing the gate at teardown lets A's drain return too, leaving nothing behind. The
production bug this prevents is the outage where one customer's dead webhook endpoint
freezes the shared delivery loop and stalls every other customer's notifications --
the classic head-of-line failure that per-tenant queueing exists to eliminate.

## Resources

- [Go Blog: Go Concurrency Patterns: Pipelines and cancellation](https://go.dev/blog/pipelines) -- fan-out/fan-in with per-stage channel ownership and single-close discipline.
- [pkg.go.dev: sync.WaitGroup.Go](https://pkg.go.dev/sync#WaitGroup.Go) -- the Go 1.25+ helper that starts a goroutine and tracks it in one call, used to launch each drain.
- [pkg.go.dev: go.uber.org/goleak](https://pkg.go.dev/go.uber.org/goleak) -- the leak detector that fails the test run if any drain goroutine outlives it.
- [Go Wiki: Go Memory Model](https://go.dev/ref/mem) -- why the mutex around counts and the channel handoffs establish the happens-before edges this design relies on.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [12-streaming-usage-rollup-by-key.md](12-streaming-usage-rollup-by-key.md) | Next: [14-contiguous-watermark-offset-commit-consumer.md](14-contiguous-watermark-offset-commit-consumer.md)
