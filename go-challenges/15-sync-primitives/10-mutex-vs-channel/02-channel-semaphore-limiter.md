# Exercise 2: Buffered-Channel Semaphore Limiter with Lifecycle Close

Same admission contract as Exercise 1 — burst of N, refill over time — built
on the other primitive: a buffered channel pre-filled with tokens, drained by
`Allow` and topped up by a ticker-driven goroutine. The new work is not the
happy path; it is the lifecycle. This type starts a goroutine, so `Close`
becomes part of its API, must be idempotent, and must provably terminate the
goroutine.

## What you'll build

```text
chanlimiter/                    independent module: example.com/chanlimiter
  go.mod
  limiter/
    channel.go                  type ChannelLimiter; NewChannelLimiter, Allow,
                                Close; refill goroutine signaling exit via done
    channel_test.go             burst/deny, exact-N storm, refill-over-time,
                                idempotent Close, goroutine-exit proof
  cmd/
    demo/
      main.go                   drain, deny, real refill, clean shutdown
```

- Files: `limiter/channel.go`, `limiter/channel_test.go`, `cmd/demo/main.go`.
- Implement: `ChannelLimiter` over a pre-filled `chan struct{}`, non-blocking try-acquire via `select`/`default`, a ticker refill goroutine that drops tokens when the bucket is full, and an idempotent `Close` guarded by `sync.Once` with a `done` channel proving goroutine exit.
- Test: initial burst and deny-after-drain with refill effectively off, a 50-goroutine exact-N storm, a poll-with-deadline refill test, double-`Close` safety, and a goroutine-exit assertion.
- Verify: `go test -count=1 -race ./...` and `go run ./cmd/demo`.

Set up the module:

```bash
mkdir -p ~/go-exercises/chanlimiter/limiter ~/go-exercises/chanlimiter/cmd/demo
cd ~/go-exercises/chanlimiter
go mod init example.com/chanlimiter
```

### A buffered channel is a counting semaphore

`make(chan struct{}, n)` pre-filled with n empty structs is a token store
with built-in synchronization: a receive atomically takes a token or reports
none available (with `select`/`default`), and a send atomically returns one
or is dropped when the store is full (same idiom, other direction). All the
locking lives inside the channel runtime — there is no field in
`ChannelLimiter` that two goroutines mutate directly, so there is nothing for
a mutex to guard. `struct{}` is the idiomatic token type because it occupies
zero bytes; the channel's buffer is effectively just a counter with wait
queues.

The refill side is where this design diverges architecturally from
Exercise 1. There is no elapsed-time arithmetic; instead a dedicated
goroutine wakes on a `time.Ticker` and offers one token per tick:

```
select {
case cl.tokens <- struct{}{}:
default: // bucket full: the tick is dropped, not banked
}
```

That non-blocking send is load-bearing twice over. It implements the cap —
a full bucket silently discards the tick — and it keeps the refill goroutine
from ever blocking on a full channel, which would delay its response to
`stop` and turn shutdown into a race. The trade-off relative to continuous
refill is real and worth stating precisely: one token per tick means a 100ms
interval caps sustained throughput at 10/s *regardless of demand pattern*,
refill granularity is a whole token (a caller arriving 99ms after the last
tick gets nothing), and ticks offered while the bucket is full are lost
rather than credited later. The mutex version's fractional refill has none
of these artifacts — but also none of this version's free blocking receive,
which Exercise 7 will exploit for `Wait(ctx)`.

### Lifecycle: the goroutine is part of the public contract

Because the constructor starts a goroutine, three obligations attach to the
type, and each maps to a specific line of code and a specific test:

1. There must be a way to stop it: `Close` closes the `stop` channel, which
   the refill loop selects on.
2. `Close` must be idempotent and safe from any goroutine: `close` on an
   already-closed channel panics, so the `close(stop)` is wrapped in
   `sync.Once.Do`. Real shutdown paths call `Close` from `defer`s, signal
   handlers, and error paths simultaneously; "only call Close once" is not a
   contract you can enforce on callers.
3. The exit must be provable, not assumed: the goroutine closes a `done`
   channel on return, and a test receives from it with a timeout. Without
   this, a refactor that breaks the `stop` select (say, a nested select that
   shadows it) leaks one goroutine plus one ticker per constructor call —
   invisible in unit tests, fatal over weeks in a long-lived server.

Note also `defer ticker.Stop()` inside the goroutine: a `time.Ticker` holds a
runtime timer that keeps firing until stopped; tying its lifetime to the
goroutine's with `defer` means proving the goroutine exits also proves the
ticker is released.

Create `limiter/channel.go`:

```go
package limiter

import (
	"sync"
	"time"
)

// ChannelLimiter is a token-bucket rate limiter built on a buffered channel.
// The channel's buffer is the token store; a background goroutine refills
// one token per tick and drops ticks when the bucket is full.
type ChannelLimiter struct {
	tokens chan struct{}
	stop   chan struct{}
	done   chan struct{} // closed by the refill goroutine on exit
	once   sync.Once
}

// NewChannelLimiter returns a limiter that starts full (a burst of maxTokens)
// and refills one token every refillInterval. Callers must call Close when
// done with the limiter, or the refill goroutine and its ticker leak.
func NewChannelLimiter(maxTokens int, refillInterval time.Duration) *ChannelLimiter {
	cl := &ChannelLimiter{
		tokens: make(chan struct{}, maxTokens),
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	for range maxTokens {
		cl.tokens <- struct{}{}
	}
	go cl.refill(refillInterval)
	return cl
}

func (cl *ChannelLimiter) refill(interval time.Duration) {
	defer close(cl.done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			select {
			case cl.tokens <- struct{}{}:
			default: // bucket full: drop the tick, never block
			}
		case <-cl.stop:
			return
		}
	}
}

// Allow reports whether one request may proceed, spending one token if so.
// It never blocks: an empty bucket means an immediate deny.
func (cl *ChannelLimiter) Allow() bool {
	select {
	case <-cl.tokens:
		return true
	default:
		return false
	}
}

// Close stops the refill goroutine and releases its ticker. It is idempotent
// and safe to call from any goroutine.
func (cl *ChannelLimiter) Close() {
	cl.once.Do(func() { close(cl.stop) })
}
```

One structural observation: `Allow` and the refill goroutine never touch a
shared field — they communicate exclusively through `tokens`. The race
detector has nothing to find here by construction, which is the channel
design's genuine advantage. Its price is everything in the previous section.

### The demo: watch a real refill and a clean shutdown

This demo runs against the wall clock (the ticker is real), so it is written
to be timing-safe: 3 tokens, a 50ms interval, and a 120ms sleep guarantee at
least two ticks land before the post-refill check.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/chanlimiter/limiter"
)

func main() {
	l := limiter.NewChannelLimiter(3, 50*time.Millisecond)
	defer l.Close()

	allowed := 0
	for range 3 {
		if l.Allow() {
			allowed++
		}
	}
	fmt.Printf("initial burst: allowed %d/3\n", allowed)
	fmt.Printf("bucket empty: allow=%v\n", l.Allow())

	time.Sleep(120 * time.Millisecond) // at least two 50ms ticks land
	fmt.Printf("after refill: allow=%v\n", l.Allow())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
initial burst: allowed 3/3
bucket empty: allow=false
after refill: allow=true
```

### Tests: exact counts, then lifecycle proofs

Two testing techniques here are transferable to any goroutine-owning type.
First, whenever a test asserts exact counts, refill is disabled by setting
the interval to `time.Hour` — determinism is manufactured, not hoped for.
Second, time-dependent assertions poll with a deadline instead of sleeping a
fixed amount: `TestChannelLimiterRefillsOverTime` loops on `Allow` until it
succeeds or two seconds pass, so it cannot flake on a slow CI scheduler yet
normally finishes in ~20ms. The goroutine-exit test is the one most codebases
are missing: `Close`, then receive from `done` with a timeout — if the refill
loop's `stop` handling ever regresses, this test hangs the 2s timer and fails
loudly instead of leaking silently.

Create `limiter/channel_test.go`:

```go
package limiter

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestChannelLimiterInitialBurst(t *testing.T) {
	t.Parallel()

	l := NewChannelLimiter(3, time.Hour) // refill effectively off
	defer l.Close()

	for i := range 3 {
		if !l.Allow() {
			t.Fatalf("Allow #%d returned false during initial burst", i)
		}
	}
	if l.Allow() {
		t.Fatal("Allow after burst returned true")
	}
}

func TestChannelLimiterConcurrentExactBurst(t *testing.T) {
	t.Parallel()

	l := NewChannelLimiter(500, time.Hour) // refill off: only the burst counts
	defer l.Close()

	var allowed atomic.Int64
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			for range 50 {
				if l.Allow() {
					allowed.Add(1)
				}
			}
		})
	}
	wg.Wait()

	if got, want := allowed.Load(), int64(500); got != want {
		t.Fatalf("allowed = %d, want exactly %d", got, want)
	}
}

func TestChannelLimiterRefillsOverTime(t *testing.T) {
	t.Parallel()

	l := NewChannelLimiter(1, 20*time.Millisecond)
	defer l.Close()

	if !l.Allow() {
		t.Fatal("initial Allow returned false")
	}
	if l.Allow() {
		t.Fatal("second immediate Allow returned true (no tick yet)")
	}

	// Poll with a deadline instead of sleeping a fixed amount: immune to
	// scheduler delays, and normally finishes after one ~20ms tick.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if l.Allow() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no token refilled within 2s (ticker goroutine dead?)")
}

func TestChannelLimiterCloseIdempotent(t *testing.T) {
	t.Parallel()

	l := NewChannelLimiter(1, time.Hour)
	l.Close()
	l.Close() // second call must not panic: close(stop) is behind sync.Once
}

func TestChannelLimiterCloseStopsRefillGoroutine(t *testing.T) {
	t.Parallel()

	l := NewChannelLimiter(1, time.Millisecond)
	l.Close()

	select {
	case <-l.done:
		// refill goroutine exited and released its ticker
	case <-time.After(2 * time.Second):
		t.Fatal("refill goroutine still running 2s after Close")
	}
}

func ExampleChannelLimiter() {
	l := NewChannelLimiter(1, time.Hour)
	defer l.Close()
	fmt.Println(l.Allow())
	fmt.Println(l.Allow())
	// Output:
	// true
	// false
}
```

## Review

The mistakes this module guards against are all lifecycle mistakes. A
blocking send in the refill loop (forgetting the inner `default`) makes the
goroutine deaf to `stop` whenever the bucket is full — the exit test catches
it. An unguarded `close(stop)` panics the second caller — the idempotency
test catches it. A missing `ticker.Stop()` keeps a runtime timer alive after
shutdown — tying it to the goroutine with `defer` and proving the goroutine
exits covers it. If you take one habit from this exercise, it is that "starts
a goroutine" always implies "ships a Close, an idempotency test, and an exit
test".

Contrast the semantics with Exercise 1 before moving on: this limiter's
sustained rate is quantized to whole tokens per tick, full-bucket ticks are
dropped, and there is a goroutine to manage — in exchange, the token store is
natively blockable and the type has no shared mutable fields at all. Confirm
with `go test -count=1 -race ./...`; the race detector's silence here is
almost trivially guaranteed by the design, which is exactly the point.

## Resources

- [Effective Go: channels](https://go.dev/doc/effective_go#channels) — the buffered-channel-as-semaphore idiom, from the source.
- [sync package: Once](https://pkg.go.dev/sync#Once) — the guarantee that makes `Close` idempotent.
- [time package: Ticker](https://pkg.go.dev/time#Ticker) — why `Stop` matters and what `NewTicker` allocates.
- [Go blog: Share Memory By Communicating](https://go.dev/blog/codelab-share) — the proverb this design embodies.

---

Prev: [01-mutex-token-bucket.md](01-mutex-token-bucket.md) | Back to [00-concepts.md](00-concepts.md) | Next: [03-limiter-interface.md](03-limiter-interface.md)
