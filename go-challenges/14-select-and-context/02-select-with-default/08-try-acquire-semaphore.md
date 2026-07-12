# Exercise 8: Non-Blocking Concurrency Limiter (TryAcquire Semaphore)

Fanning out to a fragile downstream — a legacy API, a connection pool with a hard
cap — needs a limit on concurrency, and callers over the limit should be rejected
immediately, not queued behind an unbounded wait. A counting semaphore built on a
buffered channel gives exactly that: `TryAcquire` is a non-blocking send of a token,
`Acquire` is a cancellable blocking send, and `Release` is a receive.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
semaphore/                  independent module: example.com/semaphore
  go.mod                    go 1.26
  semaphore.go              type Semaphore; TryAcquire, Acquire(ctx), Release
  cmd/
    demo/
      main.go               fill the semaphore, reject, release, re-acquire
  semaphore_test.go         capacity, release, cancel, panic-on-overrelease, -race
```

- Files: `semaphore.go`, `cmd/demo/main.go`, `semaphore_test.go`.
- Implement: `New(limit int)`, `TryAcquire() bool` (non-blocking token send), `Acquire(ctx) error` (blocking with cancellation), and `Release()` (token receive; panics on over-release).
- Test: `TryAcquire` succeeds `limit` times then fails; one `Release` frees a slot; `Acquire` with a cancelled context returns `ctx.Err()` without acquiring; `Release` without a matching `Acquire` panics; a `-race` run where in-flight count never exceeds `limit`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/14-select-and-context/02-select-with-default/08-try-acquire-semaphore/cmd/demo
cd go-solutions/14-select-and-context/02-select-with-default/08-try-acquire-semaphore
go mod edit -go=1.26
```

### A buffered channel is the counting semaphore

Model the semaphore as `make(chan struct{}, limit)`, where each buffered element is
a *held* slot. Acquiring is *sending* a token into the buffer: it succeeds while
fewer than `limit` are held and blocks (or fails) once the buffer is full. Releasing
is *receiving* a token, freeing a slot. This orientation — send to acquire, receive
to release — means an empty channel is "nothing held" and a full channel is "at
capacity", which is the intuitive reading.

`TryAcquire` is a non-blocking send: it returns true if a slot was free, false at
capacity, and never blocks — the caller maps false to "reject this fan-out branch,
degrade or 503". `Acquire` is the blocking variant with cancellation: a `select`
over the token send and `ctx.Done()`, so a caller willing to wait for a slot can,
but still bails out if its request context is cancelled or times out. The crucial
subtlety is *ordering under a ready context*: if a slot is free AND the context is
already done, both cases of `Acquire`'s `select` are ready and the runtime picks one
at random — so `Acquire` might acquire a slot even though the context is done. That
is acceptable (the caller got what it asked for), but it means a test for
"cancelled context returns an error" must fill the semaphore first so the send
cannot proceed, leaving only `ctx.Done()` ready. The `TryAcquire` capacity test does
not have this ambiguity — with no `ctx`, it is a plain non-blocking send.

`Release` receives a token. It must be a non-blocking receive with a `default` that
panics: a `Release` with no matching `Acquire` (an empty channel) is a bug in the
caller's acquire/release pairing, and failing loudly beats silently corrupting the
count. A blocking `Release` on an empty channel would instead deadlock, which is
harder to diagnose than a panic with a clear message.

Create `semaphore.go`:

```go
package semaphore

import "context"

// Semaphore is a counting semaphore backed by a buffered channel of tokens. A
// held slot is a token in the buffer; capacity is the concurrency limit.
type Semaphore struct {
	tokens chan struct{}
}

// New returns a Semaphore permitting up to limit concurrent holders.
func New(limit int) *Semaphore {
	return &Semaphore{tokens: make(chan struct{}, limit)}
}

// TryAcquire takes a slot without blocking. It returns true if one was free, or
// false if the semaphore is at capacity.
func (s *Semaphore) TryAcquire() bool {
	select {
	case s.tokens <- struct{}{}:
		return true
	default:
		return false
	}
}

// Acquire takes a slot, blocking until one is free or ctx is done. It returns nil
// on success or ctx.Err() if the context is cancelled or times out first.
func (s *Semaphore) Acquire(ctx context.Context) error {
	select {
	case s.tokens <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Release frees a previously acquired slot. It panics if called without a matching
// Acquire (a symptom of an unbalanced acquire/release), rather than deadlocking.
func (s *Semaphore) Release() {
	select {
	case <-s.tokens:
	default:
		panic("semaphore: Release called without a matching Acquire")
	}
}
```

### The runnable demo

The demo caps concurrency at 2: two `TryAcquire`s succeed, the third is rejected,
a `Release` frees a slot, and the next `TryAcquire` succeeds again.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/semaphore"
)

func main() {
	sem := semaphore.New(2)

	fmt.Println("acquire 1:", sem.TryAcquire())
	fmt.Println("acquire 2:", sem.TryAcquire())
	fmt.Println("acquire 3:", sem.TryAcquire()) // at capacity

	sem.Release()
	fmt.Println("after release:", sem.TryAcquire())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
acquire 1: true
acquire 2: true
acquire 3: false
after release: true
```

### Tests

`TestCapacityAndRelease` pins the counting behavior: `limit` acquires succeed, the
next fails, a `Release` frees exactly one slot. `TestAcquireCancelled` fills the
semaphore, then calls `Acquire` with an already-cancelled context and asserts it
returns `context.Canceled` without taking a slot. `TestReleaseWithoutAcquirePanics`
asserts the over-release contract. `TestConcurrentNeverExceedsLimit` runs many
goroutines that acquire, bump a peak counter, release, and asserts the peak in-flight
count never exceeds `limit` — guaranteed by the buffered channel, verified under
`-race`.

Create `semaphore_test.go`:

```go
package semaphore

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
)

func TestCapacityAndRelease(t *testing.T) {
	t.Parallel()

	const limit = 3
	sem := New(limit)

	for i := range limit {
		if !sem.TryAcquire() {
			t.Fatalf("TryAcquire %d failed below capacity", i)
		}
	}
	if sem.TryAcquire() {
		t.Fatal("TryAcquire succeeded past capacity")
	}

	sem.Release()
	if !sem.TryAcquire() {
		t.Fatal("TryAcquire failed after a Release freed a slot")
	}
	if sem.TryAcquire() {
		t.Fatal("TryAcquire succeeded past capacity again")
	}
}

func TestAcquireCancelled(t *testing.T) {
	t.Parallel()

	sem := New(1)
	if !sem.TryAcquire() {
		t.Fatal("could not fill the single slot")
	}

	// Semaphore is full, so the send cannot proceed: only ctx.Done() is ready.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := sem.Acquire(ctx); err != context.Canceled {
		t.Fatalf("Acquire = %v, want context.Canceled", err)
	}
	// The failed Acquire must not have taken a slot: releasing the one real
	// holder must bring us back to empty.
	sem.Release()
	if !sem.TryAcquire() {
		t.Fatal("slot count corrupted by a cancelled Acquire")
	}
}

func TestReleaseWithoutAcquirePanics(t *testing.T) {
	t.Parallel()

	sem := New(2)
	defer func() {
		if recover() == nil {
			t.Fatal("Release without Acquire did not panic")
		}
	}()
	sem.Release() // nothing held: must panic
}

func TestConcurrentNeverExceedsLimit(t *testing.T) {
	t.Parallel()

	const limit = 5
	sem := New(limit)

	var inFlight, peak atomic.Int64
	var wg sync.WaitGroup
	for range 500 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !sem.TryAcquire() {
				return // rejected; that is fine
			}
			n := inFlight.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			inFlight.Add(-1)
			sem.Release()
		}()
	}
	wg.Wait()

	if peak.Load() > limit {
		t.Fatalf("peak in-flight = %d, exceeds limit %d", peak.Load(), limit)
	}
	// All slots released: the semaphore is empty again.
	if !sem.TryAcquire() {
		t.Fatal("semaphore not empty after all releases")
	}
}
```

## Review

The semaphore is correct when the number of concurrent holders never exceeds
`limit` and acquire/release stay balanced. The buffered channel makes the bound
automatic — at most `limit` sends can be buffered — which is why
`TestConcurrentNeverExceedsLimit` can assert a hard peak under `-race` without any
lock. Two design points are worth restating: `Acquire`'s `select` can pick the send
case even when the context is already done (both ready cases are chosen at random),
so a cancellation test must saturate the semaphore first to make the send
unavailable, as `TestAcquireCancelled` does; and `Release` must panic on an empty
semaphore rather than block, turning an unbalanced acquire/release — a real bug —
into an immediate, diagnosable failure instead of a silent deadlock. For a
richer semaphore with weighted acquisition, `golang.org/x/sync/semaphore` is the
standard choice; this channel version is the right shape when a simple unit-weight,
non-blocking gate is all you need.

## Resources

- [Go Specification: Select statements](https://go.dev/ref/spec#Select_statements) — non-blocking send (TryAcquire) and the send/`ctx.Done` race in Acquire.
- [Effective Go: channels as semaphores](https://go.dev/doc/effective_go#channels) — the buffered-channel semaphore idiom.
- [`golang.org/x/sync/semaphore`](https://pkg.go.dev/golang.org/x/sync/semaphore) — the standard weighted semaphore, for comparison.

---

Back to [00-concepts.md](00-concepts.md) | Next: [09-graceful-shutdown-drain.md](09-graceful-shutdown-drain.md)
