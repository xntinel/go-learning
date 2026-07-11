# Exercise 4: Stateful Operators — Scan and Reduce

Map, filter, and flat-map are stateless: each output depends only on the current input. Real pipelines also need operators that remember — a running total, a per-user event count, a fold over the whole stream. This exercise builds three stateful operators on Go generics: `Scan` (emit the running aggregate after every element), `Reduce` (emit one final folded value when the stream ends), and `ScanByKey` (keep an independent accumulator per key).

This module is fully self-contained. It defines its own generic operators, the keyed wrapper type, its own demo, and its own tests. Nothing here imports any other exercise.

## What you'll build

```text
scan.go                ScanFn, Scan, Reduce, Keyed, ScanByKey (single-goroutine state)
cmd/
  demo/
    main.go            running sum, full-stream reduce, per-key running count
scan_test.go           running-aggregate sequence, final-only reduce, per-key isolation, ctx abort
```

- Files: `scan.go`, `cmd/demo/main.go`, `scan_test.go`.
- Implement: `Scan` (emit accumulator after each fold), `Reduce` (emit accumulator once at end of input), and `ScanByKey` (per-key accumulator in a map owned by one goroutine), all generic over input and state types.
- Test: `scan_test.go` checks the running-aggregate sequence, that Reduce emits exactly one value, that two keys keep independent state in arrival order, and that a cancelled context stops the operator without emitting.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p stateful-scan/cmd/demo && cd stateful-scan
go mod init example.com/stateful-scan
go mod edit -go=1.26
```

### Why a single owning goroutine makes the state lock-free

A stateful operator holds an accumulator that every input mutates, so the obvious worry is data races. The design sidesteps locks entirely by giving exactly one goroutine ownership of the state. `Scan` reads each input, folds it into the local `state` variable, and emits the new value — all on one goroutine — so nothing else can touch `state` and no mutex is needed. This is the same discipline the channel operators use for their `Metrics` counters, except those counters are read concurrently by outside goroutines (hence `atomic.Int64`), whereas the scan accumulator is never read from outside, so even an atomic is unnecessary. Confining mutable state to a single goroutine and communicating results over a channel is the core of Go's "share memory by communicating" model, and it is exactly how a production stream engine keeps keyed state consistent.

The difference between Scan and Reduce is only when they emit. Scan emits after every fold, so a running sum of `1, 2, 3, 4` produces `1, 3, 6, 10` — useful for live dashboards and incremental aggregates. Reduce emits nothing until the input closes, then sends the single final accumulator (`10`) and stops — useful when only the total matters. Reduce's output channel is buffered to one slot so the final send never blocks even if the consumer is momentarily away, and the send itself is guarded by a `select` on `ctx.Done()` so a cancellation during the final emit does not park the goroutine. If the context is cancelled before the input closes, Reduce returns without emitting at all, which is the correct semantics: a partial fold of an aborted stream is not a meaningful total.

`ScanByKey` generalises Scan to many independent accumulators. It keeps a `map[string]State` and, for each input, looks up (or initialises) that key's state, folds, stores it back, and emits a `Keyed[State]` carrying the key and its new running value. Because the map lives inside the single operator goroutine it needs no lock, and because records for one key are processed in arrival order, per-key ordering is preserved even though different keys interleave on the wire. This is precisely the model behind Flink's `keyBy(...).aggregate(...)`: partition by key, keep per-key state, fold in order. The one caveat to teach is unbounded growth — the map keeps an entry per distinct key forever, so on an unbounded key space a real engine adds state TTL or windowing, which later lessons cover.

Create `scan.go`:

```go
// Package stream provides stateful streaming operators that fold a stream
// into running or final aggregates while keeping all mutable state inside
// a single owning goroutine, so no locks are required.
package stream

import "context"

// ScanFn folds the next input element into the running state and returns
// the new state. It is called from a single goroutine, so the state needs
// no lock; a hidden side effect outside state would still break replay
// determinism, so keep it pure with respect to anything but state.
type ScanFn[In, State any] func(state State, in In) State

// Scan emits the running aggregate: for every input element it folds the
// element into the accumulator and emits the new accumulator value, in
// order. The output carries one State per input element.
func Scan[In, State any](ctx context.Context, in <-chan In, init State, fn ScanFn[In, State]) <-chan State {
	out := make(chan State)
	go func() {
		defer close(out)
		state := init
		for {
			select {
			case v, ok := <-in:
				if !ok {
					return
				}
				state = fn(state, v)
				select {
				case out <- state:
				case <-ctx.Done():
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

// Reduce folds the entire stream into a single value, emitted once when the
// input is exhausted. If the context is cancelled before the input closes,
// nothing is emitted: a partial fold of an aborted stream is not a result.
func Reduce[In, State any](ctx context.Context, in <-chan In, init State, fn ScanFn[In, State]) <-chan State {
	out := make(chan State, 1)
	go func() {
		defer close(out)
		state := init
		for {
			select {
			case v, ok := <-in:
				if !ok {
					select {
					case out <- state:
					case <-ctx.Done():
					}
					return
				}
				state = fn(state, v)
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

// Keyed pairs a key with a value, used to emit per-key running aggregates.
type Keyed[V any] struct {
	Key   string
	Value V
}

// ScanByKey keeps an independent accumulator per key. For each input it
// folds into that key's state and emits the key's running aggregate. The
// per-key map is owned by this one goroutine, so it needs no lock, and
// per-key ordering is preserved because elements of one key are folded in
// arrival order.
func ScanByKey[In, State any](ctx context.Context, in <-chan In, keyOf func(In) string, init func() State, fn ScanFn[In, State]) <-chan Keyed[State] {
	out := make(chan Keyed[State])
	go func() {
		defer close(out)
		states := make(map[string]State)
		for {
			select {
			case v, ok := <-in:
				if !ok {
					return
				}
				k := keyOf(v)
				s, seen := states[k]
				if !seen {
					s = init()
				}
				s = fn(s, v)
				states[k] = s
				select {
				case out <- Keyed[State]{Key: k, Value: s}:
				case <-ctx.Done():
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}
```

The two-level `select` in each operator is the same shutdown discipline the channel operators use: the outer `select` lets cancellation interrupt a wait on an empty input, and the inner one lets it interrupt a wait on a full output. A stateful operator gets no special exemption from clean cancellation just because it carries an accumulator.

### The runnable demo

The demo runs all three operators on the same source data so their semantics sit side by side: Scan prints the running sum of five integers, Reduce prints only their total, and ScanByKey prints the running per-user event count for a small stream of keyed events.

Create `cmd/demo/main.go`:

```go
// Command demo contrasts Scan (running aggregate), Reduce (final only), and
// ScanByKey (per-key running aggregate) on the same inputs.
package main

import (
	"context"
	"fmt"

	stream "example.com/stateful-scan"
)

func main() {
	ctx := context.Background()

	// Scan: running sum.
	nums := make(chan int, 5)
	for _, n := range []int{1, 2, 3, 4, 5} {
		nums <- n
	}
	close(nums)
	for s := range stream.Scan(ctx, nums, 0, func(acc, n int) int { return acc + n }) {
		fmt.Printf("running sum: %d\n", s)
	}

	// Reduce: total only.
	nums2 := make(chan int, 5)
	for _, n := range []int{1, 2, 3, 4, 5} {
		nums2 <- n
	}
	close(nums2)
	for total := range stream.Reduce(ctx, nums2, 0, func(acc, n int) int { return acc + n }) {
		fmt.Printf("total: %d\n", total)
	}

	// ScanByKey: per-user running event count.
	type event struct {
		user string
	}
	events := make(chan event, 5)
	for _, u := range []string{"alice", "bob", "alice", "alice", "bob"} {
		events <- event{user: u}
	}
	close(events)
	byKey := stream.ScanByKey(ctx, events,
		func(e event) string { return e.user },
		func() int { return 0 },
		func(acc int, _ event) int { return acc + 1 },
	)
	for kv := range byKey {
		fmt.Printf("%s count: %d\n", kv.Key, kv.Value)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
running sum: 1
running sum: 3
running sum: 6
running sum: 10
running sum: 15
total: 15
alice count: 1
bob count: 1
alice count: 2
alice count: 3
bob count: 2
```

### Tests

`TestScanRunningAggregate` asserts the exact running-sum sequence, which is the property that distinguishes Scan from Reduce. `TestReduceEmitsFinalOnly` drains Reduce and asserts exactly one value, equal to the total. `TestScanByKeyIndependentState` interleaves two keys and asserts each key's running count is independent and in arrival order. `TestScanContextCancel` cancels before the input closes and asserts Reduce emits nothing.

Create `scan_test.go`:

```go
package stream

import (
	"context"
	"testing"
)

func intChan(vals ...int) <-chan int {
	ch := make(chan int, len(vals))
	for _, v := range vals {
		ch <- v
	}
	close(ch)
	return ch
}

// TestScanRunningAggregate verifies Scan emits the running fold after each element.
func TestScanRunningAggregate(t *testing.T) {
	t.Parallel()

	var got []int
	for s := range Scan(context.Background(), intChan(1, 2, 3, 4), 0, func(a, n int) int { return a + n }) {
		got = append(got, s)
	}
	want := []int{1, 3, 6, 10}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %d, want %d", i, got[i], want[i])
		}
	}
}

// TestReduceEmitsFinalOnly verifies Reduce emits exactly one value: the total.
func TestReduceEmitsFinalOnly(t *testing.T) {
	t.Parallel()

	var got []int
	for v := range Reduce(context.Background(), intChan(1, 2, 3, 4, 5), 0, func(a, n int) int { return a + n }) {
		got = append(got, v)
	}
	if len(got) != 1 {
		t.Fatalf("Reduce emitted %d values, want 1: %v", len(got), got)
	}
	if got[0] != 15 {
		t.Errorf("total = %d, want 15", got[0])
	}
}

// TestScanByKeyIndependentState verifies per-key accumulators are isolated
// and folded in arrival order.
func TestScanByKeyIndependentState(t *testing.T) {
	t.Parallel()

	type event struct{ user string }
	in := make(chan event, 5)
	for _, u := range []string{"a", "b", "a", "a", "b"} {
		in <- event{user: u}
	}
	close(in)

	out := ScanByKey(context.Background(), in,
		func(e event) string { return e.user },
		func() int { return 0 },
		func(acc int, _ event) int { return acc + 1 },
	)

	var seq []Keyed[int]
	for kv := range out {
		seq = append(seq, kv)
	}

	want := []Keyed[int]{
		{"a", 1}, {"b", 1}, {"a", 2}, {"a", 3}, {"b", 2},
	}
	if len(seq) != len(want) {
		t.Fatalf("got %v, want %v", seq, want)
	}
	for i := range want {
		if seq[i] != want[i] {
			t.Errorf("seq[%d] = %+v, want %+v", i, seq[i], want[i])
		}
	}
}

// TestScanContextCancel verifies a cancelled context stops Reduce before it
// emits, since a partial fold of an aborted stream is not a result.
func TestScanContextCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before any input is read

	in := make(chan int) // never closed, never written
	out := Reduce(ctx, in, 0, func(a, n int) int { return a + n })

	var got []int
	for v := range out {
		got = append(got, v)
	}
	if len(got) != 0 {
		t.Errorf("Reduce emitted %v on a cancelled context, want nothing", got)
	}
}
```

## Review

The operators are correct when the accumulator is touched by exactly one goroutine — the bug to avoid is exposing the running state to outside readers, which would reintroduce the data race the single-owner design exists to prevent. Scan and Reduce differ only in emit timing; the easy mistake is having Reduce emit inside the loop (turning it into Scan) or having Scan emit only at the end (turning it into Reduce). For `ScanByKey`, the per-key isolation test is what proves two keys do not share an accumulator, and the arrival-order assertion is what proves the single goroutine preserves per-key ordering. Run with `-race`: even though the state is single-owner, the channels are crossed by the test goroutine, and `-race` confirms there is no accidental shared access.

## Resources

- [Go generics tutorial](https://go.dev/doc/tutorial/generics) — the type-parameter syntax used by the generic operators.
- [Effective Go: Share by communicating](https://go.dev/doc/effective_go#sharing) — the single-owner-goroutine model the stateful operators rely on.
- [Apache Flink: keyed state](https://nightlies.apache.org/flink/flink-docs-stable/docs/dev/datastream/fault-tolerance/state/#keyed-state) — the production analogue of ScanByKey's per-key accumulators.

---

Prev: [03-pipeline-fusion.md](03-pipeline-fusion.md) | Next: [05-keyby-partition.md](05-keyby-partition.md)
