# Exercise 8: Dynamic-N priority over a runtime-changing subscription set

When the channels are known at compile time you write a static `select`. When the
*set* is only known at runtime — dynamic pub/sub subscriptions added and removed on
config reload — you cannot. This exercise builds priority over a dynamic channel
set with `reflect.Select`: a priority-ordered non-blocking peek pass, then a
blocking select over the whole set plus `ctx.Done()`, pruning channels as they
close.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
dynselect/                   module example.com/dynselect
  go.mod
  set.go                     type Set[T]; NewSet; Add; Len; (*Set[T]).Recv
  cmd/
    demo/
      main.go                priority over a set, adds a subscription at runtime
  set_test.go                priority peek, static oracle, closed-prune, blocking wakes
```

Files: `set.go`, `cmd/demo/main.go`, `set_test.go`.
Implement: `Set[T any]` holding an ordered slice of channels (index 0 = highest
priority) with `Add`, `Len`, and `Recv(ctx) (T, int, bool)` built on
`reflect.Select` — a `reflect.SelectDefault` peek pass in priority order, then a
blocking `reflect.Select` over the full set plus `ctx.Done()`; a closed channel
(`recvOK == false`) is pruned.
Test: the highest-priority ready channel wins on the peek; the result matches a
static-select oracle for a fixed set; a closed channel is pruned; the blocking
pass wakes on a late item and on `ctx.Done()`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/dynselect/cmd/demo
cd ~/go-exercises/dynselect
go mod init example.com/dynselect
```

### reflect.Select is the runtime-N select

`reflect.Select(cases []reflect.SelectCase) (chosen int, recv reflect.Value,
recvOK bool)` runs a `select` whose cases are assembled at runtime. A case is a
`reflect.SelectCase{Dir, Chan, Send}`:

- `Dir: reflect.SelectRecv, Chan: reflect.ValueOf(ch)` — a receive case.
- `Dir: reflect.SelectDefault` — the `default` case (leave `Chan` zero).

The returns mirror a normal `select`: `chosen` is the index of the case that
fired, `recv` is the received value (a `reflect.Value`, converted back with
`recv.Interface().(T)`), and `recvOK` is the two-valued receive's second result —
`false` means the channel was **closed**, which is your signal to prune it.

Priority needs two passes, because a single `reflect.Select` over all the cases
picks uniformly at random among the ready ones — the same fairness rule as the
static `select`. So:

1. **Peek pass, in priority order.** Iterate the channels from index 0 upward. For
   each, run a two-case `reflect.Select` of `{recv(ch), default}`. If the recv
   case fires with `recvOK == true`, that is the highest-priority ready channel —
   return it. If it fires with `recvOK == false`, the channel is closed — prune it
   and restart the pass. If the `default` fires, the channel is not ready — move to
   the next. Because the pass visits channels in order and returns on the first
   ready one, the highest-priority ready channel deterministically wins.
2. **Blocking pass.** If the peek found nothing ready, build one case per channel
   plus a final `recv(ctx.Done())` case, and run a blocking `reflect.Select`. It
   wakes on any channel or on cancellation. If `chosen` is the last index, the
   context was cancelled; if `recvOK` is false, that channel closed — prune and
   loop; otherwise return the value.

`reflect.Select` is slower than a static `select` and discards static type safety
(hence the `recv.Interface().(T)` assertion). Reach for it *only* when the set is
genuinely dynamic. For a fixed set, the static peek-then-fall-through of Exercise 1
is faster and safer — which is exactly what the oracle test below asserts the two
approaches agree on.

Create `set.go`:

```go
package dynselect

import (
	"context"
	"reflect"
)

// Set is an ordered, dynamic collection of channels with priority equal to slice
// position: index 0 is highest priority. It is a single-consumer type; call Recv
// from one goroutine (Add may be called from that same goroutine between Recvs).
type Set[T any] struct {
	chans []<-chan T
}

// NewSet builds a Set from channels in priority order (first = highest).
func NewSet[T any](chans ...<-chan T) *Set[T] {
	return &Set[T]{chans: chans}
}

// Add appends a channel at the lowest priority (a new runtime subscription).
func (s *Set[T]) Add(ch <-chan T) { s.chans = append(s.chans, ch) }

// Len reports how many channels remain in the set.
func (s *Set[T]) Len() int { return len(s.chans) }

func (s *Set[T]) remove(i int) {
	s.chans = append(s.chans[:i], s.chans[i+1:]...)
}

// Recv returns the next value and the index of the channel it came from,
// preferring lower indices. Closed channels are pruned. It returns
// (zero, -1, false) when ctx is cancelled.
func (s *Set[T]) Recv(ctx context.Context) (T, int, bool) {
	var zero T
	for {
		select {
		case <-ctx.Done():
			return zero, -1, false
		default:
		}

		// Peek pass: highest-priority ready channel wins.
		pruned := false
		for i := 0; i < len(s.chans); i++ {
			chosen, recv, recvOK := reflect.Select([]reflect.SelectCase{
				{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(s.chans[i])},
				{Dir: reflect.SelectDefault},
			})
			if chosen == 0 {
				if !recvOK { // channel closed: prune and restart the peek
					s.remove(i)
					pruned = true
					break
				}
				return recv.Interface().(T), i, true
			}
			// chosen == 1 (default): channel not ready, try the next.
		}
		if pruned {
			continue
		}

		// Blocking pass: every channel plus ctx.Done().
		if len(s.chans) == 0 {
			<-ctx.Done()
			return zero, -1, false
		}
		cases := make([]reflect.SelectCase, 0, len(s.chans)+1)
		for _, ch := range s.chans {
			cases = append(cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(ch)})
		}
		cases = append(cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(ctx.Done())})

		chosen, recv, recvOK := reflect.Select(cases)
		if chosen == len(cases)-1 {
			return zero, -1, false // ctx.Done()
		}
		if !recvOK { // a channel closed while we blocked: prune and retry
			s.remove(chosen)
			continue
		}
		return recv.Interface().(T), chosen, true
	}
}
```

### The runnable demo

The demo starts with two subscriptions, `a` (highest priority) and `b`, both
ready — `a` wins. Then it adds a third subscription `c` at runtime and drains the
rest, showing the index each value came from.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/dynselect"
)

func main() {
	ctx := context.Background()

	a := make(chan string, 1)
	b := make(chan string, 1)
	set := dynselect.NewSet[string](a, b) // a is higher priority than b

	a <- "alpha"
	b <- "beta"
	v, idx, _ := set.Recv(ctx) // both ready: highest-priority a wins
	fmt.Printf("first: %q from index %d\n", v, idx)

	// A new subscription arrives at runtime.
	c := make(chan string, 1)
	set.Add(c)
	c <- "gamma"

	v2, idx2, _ := set.Recv(ctx) // a is empty now: b at index 1
	fmt.Printf("second: %q from index %d\n", v2, idx2)

	v3, idx3, _ := set.Recv(ctx) // then the new c at index 2
	fmt.Printf("third: %q from index %d\n", v3, idx3)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
first: "alpha" from index 0
second: "beta" from index 1
third: "gamma" from index 2
```

### Tests

`TestPeekPrefersHighestPriority` puts data in two channels and asserts index 0
wins. `TestMatchesStaticOracle` is the equivalence proof: for a fixed two-channel
set, `Recv` returns the same `(value, index, ok)` as a hand-written static
priority `select`, in both the both-ready and empty-high cases — demonstrating
`reflect.Select` earns its cost only from the dynamism, not from different
semantics. `TestClosedChannelPruned` asserts a closed channel is removed.
`TestBlockingWakesOnChannel` and `TestBlockingWakesOnContext` exercise the
blocking pass.

Create `set_test.go`:

```go
package dynselect

import (
	"context"
	"testing"
	"time"
)

func load(vals ...int) <-chan int {
	ch := make(chan int, len(vals))
	for _, v := range vals {
		ch <- v
	}
	return ch
}

// staticPriority is the fixed-set oracle: index 0 (hi) beats index 1 (lo).
func staticPriority(hi, lo <-chan int) (int, int, bool) {
	select {
	case v := <-hi:
		return v, 0, true
	default:
	}
	select {
	case v := <-hi:
		return v, 0, true
	case v := <-lo:
		return v, 1, true
	default:
		return 0, -1, false
	}
}

func TestPeekPrefersHighestPriority(t *testing.T) {
	t.Parallel()

	set := NewSet[int](load(7), load(9))
	v, idx, ok := set.Recv(context.Background())
	if !ok || idx != 0 || v != 7 {
		t.Fatalf("Recv = %d,%d,%v, want 7,0,true", v, idx, ok)
	}
}

func TestMatchesStaticOracle(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		hi, lo []int
	}{
		{"both ready", []int{7}, []int{9}},
		{"high empty", nil, []int{9}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sv, si, sok := staticPriority(load(tc.hi...), load(tc.lo...))

			set := NewSet[int](load(tc.hi...), load(tc.lo...))
			rv, ri, rok := set.Recv(context.Background())

			if sv != rv || si != ri || sok != rok {
				t.Fatalf("reflect %d,%d,%v != oracle %d,%d,%v", rv, ri, rok, sv, si, sok)
			}
		})
	}
}

func TestClosedChannelPruned(t *testing.T) {
	t.Parallel()

	closed := make(chan int)
	close(closed)
	data := make(chan int, 1)
	data <- 5

	set := NewSet[int](closed, data) // index 0 closed, index 1 has data
	v, _, ok := set.Recv(context.Background())
	if !ok || v != 5 {
		t.Fatalf("Recv = %d,%v, want 5,true", v, ok)
	}
	if set.Len() != 1 {
		t.Fatalf("Len = %d, want 1 (closed channel pruned)", set.Len())
	}
}

func TestBlockingWakesOnChannel(t *testing.T) {
	t.Parallel()

	ch := make(chan int) // open, empty: forces the blocking pass
	set := NewSet[int](ch)

	go func() {
		time.Sleep(20 * time.Millisecond)
		ch <- 8
	}()

	v, idx, ok := set.Recv(context.Background())
	if !ok || idx != 0 || v != 8 {
		t.Fatalf("Recv = %d,%d,%v, want 8,0,true", v, idx, ok)
	}
}

func TestBlockingWakesOnContext(t *testing.T) {
	t.Parallel()

	ch := make(chan int) // open, empty
	set := NewSet[int](ch)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	var idx int
	var ok bool
	go func() {
		_, idx, ok = set.Recv(ctx)
		close(done)
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case <-done:
		if ok || idx != -1 {
			t.Fatalf("Recv = idx %d ok %v, want -1,false", idx, ok)
		}
	case <-time.After(time.Second):
		t.Fatal("Recv did not wake on cancel")
	}
}
```

## Review

The dynamic selector is correct when it agrees with the static oracle on a fixed
set (`TestMatchesStaticOracle`) — same priority, same values, same cancellation —
and additionally handles what the static form cannot: channels added at runtime
(`Add`) and closed channels pruned (`TestClosedChannelPruned`). The load-bearing
detail is the two-valued `recvOK`: it is the only way to tell a delivered value
from a closed channel in `reflect.Select`, and treating a closed channel as a
value would spin returning zero values forever. Keep `reflect` scoped to genuine
runtime dynamism; the oracle test exists precisely to remind you that for a fixed
set the static `select` is the better tool. `Recv` is single-consumer by contract:
it mutates the slice while pruning, so a concurrent `Recv` or `Add` from another
goroutine would race — drive each `Set` from one goroutine.

## Resources

- [`reflect.Select`](https://pkg.go.dev/reflect#Select) and [`reflect.SelectCase`](https://pkg.go.dev/reflect#SelectCase) — the runtime-N select and its `SelectDir` values.
- [`reflect.Value.Interface`](https://pkg.go.dev/reflect#Value.Interface) — converting a received `reflect.Value` back to `T`.
- [Go spec: Select statements](https://go.dev/ref/spec#Select_statements) — the uniform-random rule that forces the two-pass priority peek.

---

Back to [07-backpressure-load-shed.md](07-backpressure-load-shed.md) | Next: [09-starvation-slo-detector.md](09-starvation-slo-detector.md)
