# Exercise 1: The Non-Blocking Toolkit — TryRecv, TrySend, Drain, PollUntil

Every non-blocking channel operation in a Go backend is one of four shapes:
try-receive, try-send, drain-what-is-buffered, and poll-until-ready-or-cancelled.
This exercise builds all four as a small generic package, and every other module in
this lesson is a real-world application of one of them.

This module is fully self-contained: it has its own `go mod init`, defines every
type it needs inline, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
trydefault/                 independent module: example.com/trydefault
  go.mod                    go 1.26
  trydefault.go             TryRecv[T], TrySend[T], Drain[T], PollUntil
  cmd/
    demo/
      main.go               drains an event buffer, try-sends, polls until ready
  trydefault_test.go        contract tests: empty/buffered/full/closed, poll outcomes
```

- Files: `trydefault.go`, `cmd/demo/main.go`, `trydefault_test.go`.
- Implement: `TryRecv[T any](ch <-chan T) (T, bool)`, `TrySend[T any](ch chan<- T, v T) bool`, `Drain[T any](ch <-chan T) []T`, and `PollUntil(ctx, every, pred) bool`.
- Test: empty channel returns `(_, false)`; buffered value returns `(v, true)` then `false`; full buffer rejects a send without blocking; `Drain` returns all buffered values and is idempotent; `PollUntil` returns on first-true, after N ticks, and on cancel; a closed channel returns `false`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/14-select-and-context/02-select-with-default/01-trydefault-nonblocking-primitives/cmd/demo
cd go-solutions/14-select-and-context/02-select-with-default/01-trydefault-nonblocking-primitives
go mod edit -go=1.26
```

### Why these four primitives

`TryRecv` and `TrySend` are the atoms: a `select` with one communication case and a
`default`. `TryRecv` uses the two-value receive so it can tell a delivered value
from a closed channel — both make the receive case *ready*, so a plain
`select { case v := <-ch: ...; default: }` would treat a closed channel as "value
received" and hand you the zero value as if it were real data. The `ok` flag is how
you avoid that: `ok == false` means closed-and-drained, and this package reports it
as `(zero, false)`, the same shape as "nothing ready". That is the documented,
deliberate ambiguity of a non-blocking receive — a closed empty channel and a
not-ready channel are indistinguishable to a caller who only asked "is there a
value right now?".

`Drain` is a `TryRecv` loop: it collects every value currently buffered and stops
the instant none is ready. It never blocks waiting for the next send, which is
exactly what a shutdown path wants — empty what is queued, then move on. It is
idempotent on an already-empty channel: a second `Drain` returns an empty slice.

`PollUntil` is the non-busy-loop way to wait for a condition. It checks the
predicate once up front (so an already-true condition returns before any ticker is
created), then drives a `select` over `time.Ticker.C` and `ctx.Done()`. The ticker
bounds the poll rate; `ctx.Done()` provides prompt cancellation — cancelling the
context wakes the loop immediately rather than waiting out a `time.Sleep`. The
`defer t.Stop()` releases the runtime timer; forgetting it leaks a timer per call.
The generic `[T any]` parameter keeps the channel helpers usable for any element
type.

Create `trydefault.go`:

```go
package trydefault

import (
	"context"
	"time"
)

// TryRecv attempts a non-blocking receive on ch. It returns the value and true
// if one is available, otherwise the zero value and false. A closed, drained
// channel also reports (zero, false): to a non-blocking probe, "closed" and
// "nothing ready" are indistinguishable.
func TryRecv[T any](ch <-chan T) (T, bool) {
	var zero T
	select {
	case v, ok := <-ch:
		if !ok {
			return zero, false
		}
		return v, true
	default:
		return zero, false
	}
}

// TrySend attempts a non-blocking send of v on ch. It returns true if the value
// was delivered (a receiver was waiting or the buffer had room), false otherwise.
// It never blocks.
func TrySend[T any](ch chan<- T, v T) bool {
	select {
	case ch <- v:
		return true
	default:
		return false
	}
}

// Drain reads every value currently buffered on ch and returns them in order. It
// stops the moment no value is ready; it never blocks waiting for a future send.
// On an empty channel it returns an empty slice.
func Drain[T any](ch <-chan T) []T {
	var out []T
	for {
		v, ok := TryRecv(ch)
		if !ok {
			return out
		}
		out = append(out, v)
	}
}

// PollUntil checks pred immediately, then every `every` until pred returns true
// (returns true) or ctx is done (returns false). Cancellation wakes the loop at
// once rather than waiting out the interval.
func PollUntil(ctx context.Context, every time.Duration, pred func() bool) bool {
	if pred() {
		return true
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
			if pred() {
				return true
			}
		}
	}
}
```

### The runnable demo

The demo shows all four primitives against a real buffer: it fills an event buffer
and drains it in one sweep, try-sends into a size-1 metrics channel until it is
full, and polls a predicate that flips true on its third call.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/trydefault"
)

func main() {
	events := make(chan string, 5)
	for _, v := range []string{"login", "click", "purchase", "logout"} {
		events <- v
	}
	for _, v := range trydefault.Drain(events) {
		fmt.Println("event:", v)
	}

	metrics := make(chan int, 1)
	fmt.Println("try-send 1:", trydefault.TrySend(metrics, 1))
	fmt.Println("try-send 2:", trydefault.TrySend(metrics, 2))

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	calls := 0
	ready := trydefault.PollUntil(ctx, 5*time.Millisecond, func() bool {
		calls++
		return calls >= 3
	})
	fmt.Println("polled until ready:", ready, "calls:", calls)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
event: login
event: click
event: purchase
event: logout
try-send 1: true
try-send 2: false
polled until ready: true calls: 3
```

### Tests

The tests pin the contracts that matter. `TestTryRecvEmptyChannel` and
`TestTryRecvBufferedValue` cover the two states of a receive. `TestTrySendFullChannel`
pins the send side. `TestDrainCollectsAllBufferedValues` proves `Drain` returns
every value and is idempotent. The three `PollUntil` tests cover its three
outcomes. `TestTryRecvOnClosedChannel` pins the closed-and-empty contract: it must
report `false`, not spin.

Create `trydefault_test.go`:

```go
package trydefault

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestTryRecvEmptyChannel(t *testing.T) {
	t.Parallel()

	ch := make(chan int)
	if v, ok := TryRecv(ch); ok {
		t.Fatalf("TryRecv on empty channel: ok = true, got %d", v)
	}
}

func TestTryRecvBufferedValue(t *testing.T) {
	t.Parallel()

	ch := make(chan int, 1)
	ch <- 99

	v, ok := TryRecv(ch)
	if !ok {
		t.Fatal("TryRecv: ok = false, want true")
	}
	if v != 99 {
		t.Fatalf("TryRecv: v = %d, want 99", v)
	}

	if _, ok := TryRecv(ch); ok {
		t.Fatal("TryRecv on drained channel: ok = true, want false")
	}
}

func TestTryRecvOnClosedChannel(t *testing.T) {
	t.Parallel()

	ch := make(chan int)
	close(ch)

	// Closed and empty is indistinguishable from "no value ready": the receive
	// case is ready and yields (zero, false), which TryRecv reports as false.
	if v, ok := TryRecv(ch); ok {
		t.Fatalf("TryRecv on closed channel: ok = true, got %d", v)
	}
}

func TestTrySendFullChannel(t *testing.T) {
	t.Parallel()

	ch := make(chan int, 1)
	if !TrySend(ch, 1) {
		t.Fatal("TrySend: first send failed")
	}
	if TrySend(ch, 2) {
		t.Fatal("TrySend: second send succeeded on a full channel")
	}
	if got := <-ch; got != 1 {
		t.Fatalf("channel received %d, want 1", got)
	}
}

func TestTrySendUnbufferedAlwaysFails(t *testing.T) {
	t.Parallel()

	// An unbuffered send needs a receiver parked at that instant; with none,
	// TrySend can never proceed and always takes default.
	ch := make(chan int)
	if TrySend(ch, 1) {
		t.Fatal("TrySend into unbuffered channel with no receiver succeeded")
	}
}

func TestDrainCollectsAllBufferedValues(t *testing.T) {
	t.Parallel()

	ch := make(chan string, 5)
	for _, v := range []string{"a", "b", "c", "d", "e"} {
		ch <- v
	}

	got := Drain(ch)
	want := []string{"a", "b", "c", "d", "e"}
	if len(got) != len(want) {
		t.Fatalf("Drain returned %d values, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Drain[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	if rest := Drain(ch); len(rest) != 0 {
		t.Fatalf("Drain after Drain returned %d values, want 0", len(rest))
	}
}

func TestPollUntilReturnsTrueWhenPredTrue(t *testing.T) {
	t.Parallel()

	calls := 0
	ok := PollUntil(context.Background(), time.Millisecond, func() bool {
		calls++
		return true
	})
	if !ok {
		t.Fatal("PollUntil: returned false, want true")
	}
	if calls != 1 {
		t.Fatalf("PollUntil: pred called %d times, want 1", calls)
	}
}

func TestPollUntilReturnsTrueAfterSomeTicks(t *testing.T) {
	t.Parallel()

	calls := 0
	ok := PollUntil(context.Background(), 2*time.Millisecond, func() bool {
		calls++
		return calls >= 3
	})
	if !ok {
		t.Fatal("PollUntil: returned false, want true")
	}
	if calls < 3 {
		t.Fatalf("PollUntil: pred called %d times, want at least 3", calls)
	}
}

func TestPollUntilReturnsFalseOnCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	if PollUntil(ctx, 5*time.Millisecond, func() bool { return false }) {
		t.Fatal("PollUntil: returned true, want false")
	}
}

func ExampleTryRecv() {
	ch := make(chan int, 1)
	ch <- 5
	v, ok := TryRecv(ch)
	fmt.Println(v, ok)
	// Output: 5 true
}

func ExampleTrySend() {
	ch := make(chan int, 1)
	fmt.Println(TrySend(ch, 42))
	fmt.Println(TrySend(ch, 43)) // false, buffer full
	// Output:
	// true
	// false
}
```

## Review

The package is correct when each primitive touches the channel exactly once and
never blocks. `TryRecv` returns `false` in two distinct situations — empty-and-open
and closed-and-drained — and the closed test proves the second does not spin. The
most common structural mistake is a plain `select { case v := <-ch: ...; default: }`
that omits the `ok`: it treats a closed channel as a delivered zero value, so a
drain loop over a closed channel would append zeros forever. The second is a
`Drain` that blocks on the next receive instead of using the `default`, which turns
a shutdown flush into a hang. Run `go test -race` to confirm the poll tests do not
depend on scheduler timing beyond the generous bounds asserted, and note that
`TestTrySendUnbufferedAlwaysFails` documents the buffer-sizing rule the later
modules rely on.

## Resources

- [Go Specification: Select statements](https://go.dev/ref/spec#Select_statements) — the exact rule for when `default` is taken.
- [Go by Example: Non-Blocking Channel Operations](https://gobyexample.com/non-blocking-channel-operations) — the try-recv/try-send/default idiom.
- [`time.Ticker`](https://pkg.go.dev/time#Ticker) — `NewTicker`, `Ticker.C`, and `Ticker.Stop` used by `PollUntil`.
- [`context`](https://pkg.go.dev/context) — `Done` and cancellation semantics.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-drop-when-full-telemetry-emitter.md](02-drop-when-full-telemetry-emitter.md)
