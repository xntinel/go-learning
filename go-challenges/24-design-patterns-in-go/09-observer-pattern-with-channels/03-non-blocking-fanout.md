# Exercise 3: Non-Blocking Fan-Out with Drop-on-Full

The event bus drops events for a full subscriber silently. That is the right default, but in production a silent drop is a blind spot: you cannot tune buffer sizes or alert on backpressure if you cannot count what you lost. This exercise builds the drop policy as a first-class, measurable thing — a generic `Fanout[T]` that delivers each value to every subscriber with a non-blocking `select`, skips a subscriber whose buffer is full, and counts every drop so a slow consumer is visible rather than invisible.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
fanout.go            Fanout[T], Subscribe, Send (returns drop count), Dropped, Close
cmd/
  demo/
    main.go          one fast and one slow subscriber; watch drops accumulate
fanout_test.go       drop accounting + a -race concurrent send/subscribe sweep
```

- Files: `fanout.go`, `cmd/demo/main.go`, `fanout_test.go`.
- Implement: `Fanout[T]` with `Subscribe(buf int) <-chan T`, `Send(T) int`, `Dropped() int64`, and `Close()`.
- Test: `fanout_test.go` proves a fast subscriber receives everything, a full subscriber's overflow is dropped and counted exactly, and concurrent send/subscribe is race-free.
- Verify: `go test -race ./...`

### Availability over delivery, made countable

The fan-out's contract is "the producer never blocks." Each `Send` walks the subscriber list and, for each channel, runs:

```
select {
case ch <- v:
default:
	dropped++
}
```

If the buffer has room, the value lands; if it is full, the `default` arm fires and that subscriber misses this value. No subscriber, however slow, can ever stall `Send`, because the slowest possible path through the loop is one `default` per subscriber. This is availability chosen over delivery, and it is the correct choice for telemetry, metrics, live dashboards, and any stream where a fresh value supersedes a stale one. It is the wrong choice for a stream where every value must arrive — there you need a bounded blocking send, or an unbounded queue, and you accept the backpressure or the memory cost that comes with guaranteed delivery.

What turns a silent drop into an operable signal is counting it. `Send` returns the number of subscribers that dropped this particular value — a caller can react immediately ("everyone is behind, shed load") — and the `Fanout` also keeps a cumulative `Dropped()` total for monitoring. The total is an `atomic.Int64` so `Dropped()` can be read without taking the lock, while the increment happens inside `Send` under the lock. Reading a metric should never contend with the hot path, and an atomic counter is the standard way to expose one cheaply.

The same "send under the lock that closes channels" discipline from the previous exercises applies: `Send` holds the lock across its non-blocking fan-out, and `Close` takes the same lock to close every channel, so a value is never sent to a channel `Close` just closed. A subscriber is identified only by its channel here because this fan-out, like a telemetry tap, is subscribe-and-forget: subscriptions live for the lifetime of the fan-out and are all torn down together by `Close`.

Create `fanout.go`:

```go
package fanout

import (
	"sync"
	"sync/atomic"
)

// Fanout delivers each sent value to every subscriber without ever blocking the
// sender. A subscriber whose buffer is full misses the value, which is counted
// as a drop. It is safe for concurrent Send, Subscribe, and Close.
type Fanout[T any] struct {
	mu      sync.Mutex
	subs    []chan T
	closed  bool
	dropped atomic.Int64
}

// New returns an empty Fanout.
func New[T any]() *Fanout[T] {
	return &Fanout[T]{}
}

// Subscribe registers a subscriber with the given buffer (clamped to >= 0) and
// returns its receive-only channel. On a closed Fanout it returns an
// already-closed channel.
func (f *Fanout[T]) Subscribe(buf int) <-chan T {
	if buf < 0 {
		buf = 0
	}
	ch := make(chan T, buf)

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		close(ch)
		return ch
	}
	f.subs = append(f.subs, ch)
	return ch
}

// Send delivers v to every subscriber with a non-blocking send and returns how
// many subscribers dropped it because their buffer was full. The fan-out runs
// under the lock so a channel cannot be closed mid-send.
func (f *Fanout[T]) Send(v T) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0
	}
	dropped := 0
	for _, ch := range f.subs {
		select {
		case ch <- v:
		default:
			dropped++
		}
	}
	if dropped > 0 {
		f.dropped.Add(int64(dropped))
	}
	return dropped
}

// Dropped returns the cumulative number of dropped deliveries across all Sends.
// It is safe to call without holding the lock.
func (f *Fanout[T]) Dropped() int64 {
	return f.dropped.Load()
}

// Close marks the Fanout closed and closes every subscriber channel exactly
// once. It is idempotent.
func (f *Fanout[T]) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	f.closed = true
	for _, ch := range f.subs {
		close(ch)
	}
	f.subs = nil
}
```

`Send` returns the per-call drop count for immediate reaction and folds it into the cumulative `atomic.Int64` for monitoring. The increment is guarded by `if dropped > 0` only to skip a pointless atomic write on the common no-drop path. Because `Fanout` holds an `atomic.Int64` (which must not be copied) it is used only through the `*Fanout[T]` pointer that `New` returns; `go vet`'s copylocks check enforces this.

### The runnable demo

The demo is deterministic. It registers a fast subscriber with a roomy buffer and a slow one with a buffer of exactly 1 that is never drained, sends four values, and prints the per-send drop count and the running total. The fast subscriber receives all four; the slow one accepts the first value into its single buffer slot and drops the rest.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/non-blocking-fanout"
)

func main() {
	f := fanout.New[string]()
	defer f.Close()

	fast := f.Subscribe(8) // roomy buffer, drained below
	slow := f.Subscribe(1) // buffer of 1, never drained

	events := []string{"e1", "e2", "e3", "e4"}
	for _, e := range events {
		dropped := f.Send(e)
		fmt.Printf("sent %s, dropped by %d subscriber(s)\n", e, dropped)
	}
	fmt.Printf("total dropped: %d\n", f.Dropped())

	got := 0
drain:
	for {
		select {
		case <-fast:
			got++
		default:
			break drain
		}
	}
	fmt.Printf("fast subscriber received %d/%d events\n", got, len(events))

	select {
	case v := <-slow:
		fmt.Printf("slow subscriber holds: %s\n", v)
	default:
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
sent e1, dropped by 0 subscriber(s)
sent e2, dropped by 1 subscriber(s)
sent e3, dropped by 1 subscriber(s)
sent e4, dropped by 1 subscriber(s)
total dropped: 3
fast subscriber received 4/4 events
slow subscriber holds: e1
```

### Tests

`TestSend_DropsWhenBufferFull` sets up exactly the demo's asymmetry and asserts the arithmetic: the slow subscriber drops `n-1` values, the fast one receives all `n`, and both the per-send return and the cumulative `Dropped()` agree. `TestSubscribe_AfterClose` proves a post-close subscribe yields a closed channel and that `Send` on a closed fan-out is a no-op. `TestConcurrentSendAndSubscribe` runs many senders against many late-arriving subscribers under `-race`.

Create `fanout_test.go`:

```go
package fanout

import (
	"sync"
	"testing"
)

func TestSend_DropsWhenBufferFull(t *testing.T) {
	t.Parallel()

	f := New[int]()
	slow := f.Subscribe(1)   // never drained -> overflow drops
	fast := f.Subscribe(100) // big enough to hold everything

	const n = 10
	total := 0
	for i := range n {
		total += f.Send(i)
	}

	if total != n-1 {
		t.Fatalf("summed per-send drops = %d, want %d", total, n-1)
	}
	if f.Dropped() != int64(n-1) {
		t.Fatalf("Dropped() = %d, want %d", f.Dropped(), n-1)
	}

	got := 0
	for {
		select {
		case <-fast:
			got++
			continue
		default:
		}
		break
	}
	if got != n {
		t.Fatalf("fast received %d, want %d", got, n)
	}
	if len(slow) != 1 {
		t.Fatalf("slow holds %d, want 1", len(slow))
	}
}

func TestSubscribe_AfterClose(t *testing.T) {
	t.Parallel()

	f := New[int]()
	f.Close()
	f.Close() // idempotent.

	ch := f.Subscribe(1)
	if _, ok := <-ch; ok {
		t.Fatal("expected closed channel from Subscribe after Close")
	}
	if d := f.Send(1); d != 0 {
		t.Fatalf("Send after Close dropped %d, want 0 (no-op)", d)
	}
}

func TestConcurrentSendAndSubscribe(t *testing.T) {
	t.Parallel()

	f := New[int]()
	defer f.Close()

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 500 {
				f.Send(j)
			}
		}()
	}
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				ch := f.Subscribe(2)
				select {
				case <-ch:
				default:
				}
			}
		}()
	}
	wg.Wait()
	_ = f.Dropped()
}
```

## Review

The fan-out is correct when its accounting is exact and its sender never blocks. Exactness is the testable claim: with a slow subscriber of buffer 1 that is never drained and a fast subscriber large enough to hold everything, sending `n` values must drop exactly `n-1` (the slow one accepts the first and refuses the rest) and deliver all `n` to the fast one, with the summed per-send return equal to `Dropped()`. Non-blocking is structural: every `Send` path is a `select` with a `default`, so the loop's worst case is one `default` per subscriber and no slow consumer can stall it. `TestConcurrentSendAndSubscribe` under `-race` confirms the shared slice, the closed flag, and the atomic counter are all accessed safely.

Common mistakes for this feature. The first is dropping silently with no counter, which leaves backpressure invisible — you cannot size buffers or alert on a slow consumer you cannot measure, so `Send` returns the count and `Dropped()` accumulates it. The second is reading the cumulative counter under the lock, which makes a cheap monitoring read contend with the hot `Send` path; an `atomic.Int64` lets `Dropped()` read lock-free. The third is sending outside the lock that `Close` uses, which reintroduces the "send on closed channel" panic the moment a `Close` races a `Send`; holding the lock across the non-blocking fan-out closes that window.

## Resources

- [`sync/atomic`](https://pkg.go.dev/sync/atomic) — `atomic.Int64`, the lock-free counter that exposes the drop total without contending with `Send`.
- [Go Concurrency Patterns: Pipelines and cancellation](https://go.dev/blog/pipelines) — backpressure, buffering, and shedding load in channel pipelines.
- [Effective Go: Channels](https://go.dev/doc/effective_go#channels) — buffered channels and the `select`/`default` non-blocking send this fan-out is built on.
- [Rob Pike: Go Concurrency Patterns (talk)](https://go.dev/talks/2012/concurrency.slide) — `select` as the tool for non-blocking and multiplexed channel operations.

---

Back to [02-safe-subscribe-unsubscribe.md](02-safe-subscribe-unsubscribe.md) | Next: [04-domain-event-bus-at-least-once.md](04-domain-event-bus-at-least-once.md)
