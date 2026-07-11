# Exercise 12: Streaming Per-Tenant Usage Rollup Emitted on Close

**Level: Intermediate**

A metering pipeline consumes a stream of usage events, one per API call tagged
with a tenant id and a unit count, and must produce per-tenant totals for a
billing period. The naive approach collects every event into a slice and sums at
the end, holding the whole period in memory for no reason; the streaming approach
reduces into a keyed accumulator while it ranges and flushes the aggregate exactly
once, when the producer closes the stream. This exercise builds that
reduction-on-close consumer: no batching, no timer, just accumulate-while-ranging
then emit a deterministic, tenant-sorted snapshot.

This module is self-contained: its own module, a `rollup` package, a demo, and
tests. Nothing here imports another exercise.

## What you'll build

```text
rollup/                      independent module: example.com/rollup
  go.mod                     go 1.26
  rollup.go                  type Event, TenantTotal; Rollup(in) []TenantTotal
  cmd/demo/main.go           runnable demo: interleaved events -> sorted totals
  rollup_test.go             interleaved-sum-and-sort, empty-nil, single-tenant, negative/zero, 1000-event contract, fan-in drain
```

- Files: `rollup.go`, `cmd/demo/main.go`, `rollup_test.go`.
- Implement: `Rollup(in <-chan Event) []TenantTotal` that ranges `in` to completion, summing `Units` per `Tenant`, and returns the totals sorted by `Tenant` ascending once `in` is closed and drained.
- Test: interleaved multi-tenant events sum correctly and come back sorted; empty-closed input returns nil; a single tenant collapses to one row; negative and zero units are summed faithfully; a thousand-event contract pins exact totals independent of interleaving.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/rollup/cmd/demo
cd ~/go-exercises/rollup
go mod init example.com/rollup
```

### Reduce while ranging, flush once on close

The shape here is the purest form of a ranging consumer: a fold. You range the
input channel, and for each event you update a running accumulator; when the
channel closes and drains, the loop ends and you emit the accumulated result. No
per-event output, no downstream channel, no timer. The design rests on three facts.

1. **The termination signal is close, and only close.** `Rollup` has no context,
   no deadline, no count bound; it runs until the producer announces "no more
   sends" by closing `in`. That is correct precisely because the job is to total
   an *entire* billing period — it must see every event, so ending on anything but
   close would under-count. The contract is in the type: `in <-chan Event` is
   receive-only, so the consumer cannot close it. The producer owns the close.

2. **Close drains the buffer first, and the accumulator is a keyed map.** A
   buffered burst followed by close still yields every buffered event before the
   loop ends, so the tail is never truncated. `map[string]int64` folds each event
   in O(1): `totals[ev.Tenant] += ev.Units` both inserts a new tenant and
   accumulates an existing one, since the zero value is `0`. Zero and negative
   units need no special case — a `-1` correction subtracts, a `0` adds nothing,
   both land in the same sum as any positive count.

3. **Map iteration order is randomized, so the emit step must sort.** You cannot
   return the accumulator by ranging it into a slice and stopping — two runs would
   emit tenants in different orders. You materialize `[]TenantTotal`, then
   `slices.SortFunc` by tenant ascending so the snapshot is identical every run.
   That is what makes the thousand-event contract test possible: the totals are a
   commutative sum (interleaving is irrelevant) and the order is imposed by the
   sort, not by the stream.

The empty case returns `nil`, not an empty non-nil slice: an empty-closed stream
means no tenant had any usage, and the idiomatic Go zero result for "no rows" is a
nil slice. `len(totals) == 0` is the guard.

Create `rollup.go`:

```go
package rollup

import (
	"cmp"
	"slices"
)

// Event is one usage record: a tenant id and the units consumed by a single API
// call. Units may be zero or negative (a correction/credit); those are summed
// faithfully, not dropped.
type Event struct {
	Tenant string
	Units  int64
}

// TenantTotal is one row of the emitted snapshot: a tenant and its summed units.
type TenantTotal struct {
	Tenant string
	Total  int64
}

// Rollup ranges in to completion, summing Units per Tenant into a keyed
// accumulator, and once in is closed and drained emits the totals sorted by
// Tenant ascending. It reduces while ranging and flushes the aggregate exactly
// once, on close. Empty-closed input yields a nil slice.
func Rollup(in <-chan Event) []TenantTotal {
	totals := make(map[string]int64)
	for ev := range in {
		totals[ev.Tenant] += ev.Units
	}
	if len(totals) == 0 {
		return nil
	}
	out := make([]TenantTotal, 0, len(totals))
	for tenant, total := range totals {
		out = append(out, TenantTotal{Tenant: tenant, Total: total})
	}
	slices.SortFunc(out, func(a, b TenantTotal) int {
		return cmp.Compare(a.Tenant, b.Tenant)
	})
	return out
}
```

### The runnable demo

The demo runs a producer goroutine that streams a fixed, interleaved batch of
events across tenants — including a `-1` correction and a `0` no-op — then closes
the channel. The consumer reduces on close and prints the tenant-sorted snapshot.
The output is deterministic for two independent reasons: summation is
order-independent, and `Rollup` sorts before returning, so neither the goroutine
scheduling nor the map's internal order can change what prints.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/rollup"
)

func main() {
	// A producer goroutine streams interleaved usage events, then closes the
	// channel to signal "no more sends". The consumer reduces on close and emits
	// a tenant-sorted snapshot. The output is deterministic: the totals are a sum
	// (order-independent) and Rollup sorts by tenant before returning.
	events := []rollup.Event{
		{Tenant: "acme", Units: 5},
		{Tenant: "globex", Units: 2},
		{Tenant: "acme", Units: 3},
		{Tenant: "initech", Units: 10},
		{Tenant: "globex", Units: 8},
		{Tenant: "acme", Units: -1}, // a correction: summed, not dropped
		{Tenant: "initech", Units: 0},
	}

	in := make(chan rollup.Event)
	go func() {
		defer close(in)
		for _, ev := range events {
			in <- ev
		}
	}()

	for _, row := range rollup.Rollup(in) {
		fmt.Printf("%s\t%d\n", row.Tenant, row.Total)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
acme	7
globex	10
initech	10
```

### Tests

`TestMultipleTenantsInterleavedSumAndSort` feeds interleaved events for three
tenants and asserts both the exact totals and that the result is sorted by tenant.
`TestEmptyClosedReturnsNil` closes an empty channel and asserts a nil slice.
`TestSingleTenantCollapsesToOneRow` confirms a stream from one tenant produces
exactly one row with the summed total. `TestNegativeAndZeroUnitsSummedFaithfully`
mixes positive, zero, and negative units and asserts they are added, not dropped,
including a tenant whose total is negative. `TestThousandEventContractPinsTotalsIndependentOfInterleaving`
builds a 1000-event multiset with known per-tenant totals, shuffles arrival order
with a fixed seed, and pins the exact totals and ordering — proving the result is
a function of the multiset, not the interleaving. `TestConcurrentProducersFanInFullDrain`
fans several producer goroutines into one channel closed exactly once after all
finish, confirming the reduce-on-close consumer drains the whole stream with no
lost events under `-race`.

Create `rollup_test.go`:

```go
package rollup

import (
	"cmp"
	"math/rand/v2"
	"slices"
	"sync"
	"testing"
)

// stream launches a producer goroutine that sends every event and closes the
// channel on completion, returning the receive-only end the consumer ranges.
func stream(events []Event) <-chan Event {
	in := make(chan Event)
	go func() {
		defer close(in)
		for _, ev := range events {
			in <- ev
		}
	}()
	return in
}

func TestMultipleTenantsInterleavedSumAndSort(t *testing.T) {
	t.Parallel()
	got := Rollup(stream([]Event{
		{Tenant: "globex", Units: 2},
		{Tenant: "acme", Units: 5},
		{Tenant: "initech", Units: 10},
		{Tenant: "acme", Units: 3},
		{Tenant: "globex", Units: 8},
	}))
	want := []TenantTotal{
		{Tenant: "acme", Total: 8},
		{Tenant: "globex", Total: 10},
		{Tenant: "initech", Total: 10},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Rollup = %v, want %v", got, want)
	}
	if !slices.IsSortedFunc(got, func(a, b TenantTotal) int {
		return cmp.Compare(a.Tenant, b.Tenant)
	}) {
		t.Fatalf("result not sorted by tenant: %v", got)
	}
}

func TestEmptyClosedReturnsNil(t *testing.T) {
	t.Parallel()
	in := make(chan Event)
	close(in)
	got := Rollup(in)
	if got != nil {
		t.Fatalf("Rollup(empty) = %v, want nil", got)
	}
	if len(got) != 0 {
		t.Fatalf("len = %d, want 0", len(got))
	}
}

func TestSingleTenantCollapsesToOneRow(t *testing.T) {
	t.Parallel()
	got := Rollup(stream([]Event{
		{Tenant: "acme", Units: 1},
		{Tenant: "acme", Units: 4},
		{Tenant: "acme", Units: 100},
	}))
	want := []TenantTotal{{Tenant: "acme", Total: 105}}
	if !slices.Equal(got, want) {
		t.Fatalf("Rollup = %v, want %v", got, want)
	}
}

func TestNegativeAndZeroUnitsSummedFaithfully(t *testing.T) {
	t.Parallel()
	got := Rollup(stream([]Event{
		{Tenant: "acme", Units: 10},
		{Tenant: "acme", Units: -4},
		{Tenant: "acme", Units: 0},
		{Tenant: "globex", Units: -7},
		{Tenant: "globex", Units: 0},
	}))
	want := []TenantTotal{
		{Tenant: "acme", Total: 6},
		{Tenant: "globex", Total: -7},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Rollup = %v, want %v", got, want)
	}
}

// TestThousandEventContractPinsTotalsIndependentOfInterleaving builds a fixed
// multiset of 1000 events with known per-tenant totals, shuffles the arrival
// order with a fixed seed, and asserts the exact totals and ordering. The result
// must not depend on how the events interleave over the channel.
func TestThousandEventContractPinsTotalsIndependentOfInterleaving(t *testing.T) {
	t.Parallel()
	tenants := []string{"alpha", "bravo", "charlie", "delta"}
	events := make([]Event, 0, 1000)
	want := make([]TenantTotal, len(tenants))
	for ti, name := range tenants {
		// tenant i contributes 250 events each of Units == i+1.
		unit := int64(ti + 1)
		for range 250 {
			events = append(events, Event{Tenant: name, Units: unit})
		}
		want[ti] = TenantTotal{Tenant: name, Total: unit * 250}
	}
	r := rand.New(rand.NewPCG(42, 1))
	r.Shuffle(len(events), func(i, j int) { events[i], events[j] = events[j], events[i] })

	got := Rollup(stream(events))
	if !slices.Equal(got, want) {
		t.Fatalf("Rollup = %v, want %v", got, want)
	}
}

// TestConcurrentProducersFanInFullDrain runs several producers feeding one
// merged channel closed exactly once after all producers finish, confirming the
// reduce-on-close consumer drains the whole fan-in with no lost events under
// -race.
func TestConcurrentProducersFanInFullDrain(t *testing.T) {
	t.Parallel()
	const producers, perProducer = 8, 125
	in := make(chan Event)
	var wg sync.WaitGroup
	for p := range producers {
		wg.Go(func() {
			tenant := "acme"
			if p%2 == 1 {
				tenant = "globex"
			}
			for range perProducer {
				in <- Event{Tenant: tenant, Units: 1}
			}
		})
	}
	go func() {
		wg.Wait()
		close(in)
	}()

	got := Rollup(in)
	// 4 even producers -> acme, 4 odd -> globex, each 125 units.
	want := []TenantTotal{
		{Tenant: "acme", Total: 500},
		{Tenant: "globex", Total: 500},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Rollup = %v, want %v", got, want)
	}
}
```

## Review

Correct here means the emitted snapshot is the exact per-tenant sum of every event
the producer sent, in tenant-ascending order, produced exactly once when the stream
closes. The map accumulator guarantees the totals — `+=` folds each event in O(1),
and because addition is commutative the arrival order cannot change any total,
which is what `TestThousandEventContractPinsTotalsIndependentOfInterleaving` pins
by shuffling arrival and still demanding the same result. `range in` ending only on
close guarantees completeness: every buffered event drains before the loop returns,
so no tail is lost, which `TestConcurrentProducersFanInFullDrain` proves by fanning
eight producers into one channel closed once and asserting the full count survives
under `-race`. The explicit `slices.SortFunc` turns a randomized map into a
deterministic emit; without it the totals would be right but the row order would
flap run to run. The production bug this prevents is the under-count from ending the
range early (silently under-billing) or the false diff from emitting a
non-deterministic order and then comparing snapshots that were actually identical.

## Resources

- [pkg.go.dev: slices.SortFunc](https://pkg.go.dev/slices#SortFunc) -- imposes the deterministic tenant order on the randomized map emit.
- [pkg.go.dev: cmp.Compare](https://pkg.go.dev/cmp#Compare) -- the three-way comparison the sort key returns.
- [Go Blog: Go maps in action](https://go.dev/blog/maps) -- why map iteration order is randomized and must not be relied on for output.
- [Effective Go: Channels](https://go.dev/doc/effective_go#channels) -- the close-and-drain semantics this reduce-on-close consumer depends on.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [11-streaming-export-terminal-writer-drain-on-error.md](11-streaming-export-terminal-writer-drain-on-error.md) | Next: [13-per-tenant-isolating-webhook-dispatcher.md](13-per-tenant-isolating-webhook-dispatcher.md)
