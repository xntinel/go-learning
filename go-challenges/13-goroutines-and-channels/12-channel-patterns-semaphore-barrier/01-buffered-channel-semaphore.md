# Exercise 1: A Buffered Channel as a Counting Semaphore

The most common concurrency bug in a backend is a fan-out with no cap: a batch
job that spawns one outbound call per row and buries the downstream service. The
fix is a counting semaphore, and in Go the whole thing is a buffered channel.
This exercise builds that primitive as a reusable type and proves, under the race
detector, that it holds at most N concurrent callers.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
chpatterns/                 independent module: example.com/chpatterns
  go.mod                    go 1.26
  semaphore.go              type Semaphore chan struct{}; Acquire, Release, TryAcquire
  cmd/
    demo/
      main.go               fan out 12 calls through a size-3 semaphore, report peak
  semaphore_test.go         bounds-concurrency, TryAcquire, serial-reuse tests (-race)
```

- Files: `semaphore.go`, `cmd/demo/main.go`, `semaphore_test.go`.
- Implement: a `Semaphore` with `NewSemaphore(n)`, blocking `Acquire`, `Release`, and non-blocking `TryAcquire`.
- Test: prove at most N hold the semaphore under `-race`; prove `TryAcquire` rejects when full and succeeds after a release; prove the semaphore is reusable across serial acquire/release cycles.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### Why a bare send is the correct Acquire

A buffered channel of capacity `n` is a counting semaphore with no extra
machinery. A send occupies one buffer slot and blocks when all `n` are full; a
receive frees a slot. Because you only care about occupancy and not about any
value, the element type is the empty struct: `chan struct{}` costs zero bytes per
slot, so a size-1000 semaphore is just the ring buffer.

The blocking `Acquire` is therefore a *bare send*: `s <- struct{}{}`. That send
already blocks when the semaphore is full, which is exactly the semantics you
want. Wrapping it in a `select` whose only ready-or-not case is that same send,
with a `default` that then does the send anyway, adds nothing — it is a
redundant detour that some codebases carry as cargo. `TryAcquire`, by contrast,
*must* be a `select` with a `default`, because its whole purpose is to not block:
if a slot is free it takes it and returns true, otherwise it returns false
immediately. `Release` is the receive that frees a slot.

Frame this concretely: `NewSemaphore(3)` in front of a payment provider means at
most three of your goroutines are inside the provider call at any instant, no
matter how many rows the batch has. The semaphore does not reduce how many
goroutines you spawn — it caps how many are in the guarded section at once.

Create `semaphore.go`:

```go
package chpatterns

// Semaphore is a counting semaphore backed by a buffered channel. A send
// acquires a slot and blocks when all slots are held; a receive releases one.
// The empty-struct element type carries no data and costs zero bytes per slot.
type Semaphore chan struct{}

// NewSemaphore returns a semaphore that admits at most n concurrent holders.
func NewSemaphore(n int) Semaphore {
	return make(Semaphore, n)
}

// Acquire takes a slot, blocking until one is free. The bare send already
// blocks when the semaphore is full; no select is needed for the blocking form.
func (s Semaphore) Acquire() {
	s <- struct{}{}
}

// Release frees one slot. Call it exactly once per successful Acquire, ideally
// via defer so a panic cannot leak the slot.
func (s Semaphore) Release() {
	<-s
}

// TryAcquire takes a slot without blocking. It reports whether a slot was
// taken; a false result means the semaphore is currently full.
func (s Semaphore) TryAcquire() bool {
	select {
	case s <- struct{}{}:
		return true
	default:
		return false
	}
}
```

### The runnable demo

The demo fans out twelve outbound "calls" (each a short sleep standing in for a
network round trip) through a size-3 semaphore and tracks the peak number that
ran concurrently. Because the semaphore admits at most three, the peak is 3 even
though twelve goroutines exist.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"example.com/chpatterns"
)

func main() {
	const workers, limit = 12, 3
	sem := chpatterns.NewSemaphore(limit)

	var live, peak atomic.Int64
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem.Acquire()
			defer sem.Release()

			cur := live.Add(1)
			for {
				old := peak.Load()
				if cur <= old || peak.CompareAndSwap(old, cur) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			live.Add(-1)
		}()
	}
	wg.Wait()

	fmt.Printf("workers=%d limit=%d peak=%d\n", workers, limit, peak.Load())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
workers=12 limit=3 peak=3
```

### Tests

`TestSemaphoreBoundsConcurrency` is the load-bearing test: ten goroutines
contend for a size-2 semaphore, each bumping a live counter and a
`CompareAndSwap` peak on entry and decrementing on exit. The assertion is that
the observed peak never exceeds 2. Run under `-race`, a green result is a real
proof the cap held. `TestSemaphoreTryAcquire` walks the full/reject/release/
succeed cycle. `TestSemaphoreSerialUse` acquires and releases ten times in
sequence to pin the reuse contract — a semaphore is not consumed by use.

Create `semaphore_test.go`:

```go
package chpatterns

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestSemaphoreBoundsConcurrency(t *testing.T) {
	t.Parallel()

	const limit = 2
	sem := NewSemaphore(limit)
	var live, peak atomic.Int64
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem.Acquire()
			defer sem.Release()

			cur := live.Add(1)
			for {
				old := peak.Load()
				if cur <= old || peak.CompareAndSwap(old, cur) {
					break
				}
			}
			live.Add(-1)
		}()
	}
	wg.Wait()

	if got := peak.Load(); got > limit {
		t.Fatalf("peak concurrency = %d, want <= %d", got, limit)
	}
}

func TestSemaphoreTryAcquire(t *testing.T) {
	t.Parallel()

	sem := NewSemaphore(1)
	if !sem.TryAcquire() {
		t.Fatal("first TryAcquire on empty semaphore should succeed")
	}
	if sem.TryAcquire() {
		t.Fatal("second TryAcquire on full semaphore should fail")
	}
	sem.Release()
	if !sem.TryAcquire() {
		t.Fatal("TryAcquire after Release should succeed")
	}
	sem.Release()
}

func TestSemaphoreSerialUse(t *testing.T) {
	t.Parallel()

	sem := NewSemaphore(1)
	for range 10 {
		sem.Acquire()
		sem.Release()
	}
	// A reusable semaphore is empty again and admits a fresh acquire.
	if !sem.TryAcquire() {
		t.Fatal("semaphore not reusable after 10 acquire/release cycles")
	}
	sem.Release()
}
```

## Review

The semaphore is correct when the observed peak concurrency under `-race` never
exceeds the configured limit and the type survives arbitrary reuse. The two
mistakes to avoid are structural. First, never wrap the blocking `Acquire` in a
`select` whose only case is the send — the bare send already blocks correctly,
and the wrapper is dead ceremony. Second, always pair `Acquire` with
`defer Release()`: a slot held past a panic or an early return is a slow-motion
deadlock where concurrency degrades to zero with no error. Do not add a `Close`
method: closing a semaphore channel makes every later send panic and every
receive return instantly, destroying the slot invariant. Reusability is the
reason the serial test matters — a semaphore is a rate limiter, not a
one-shot token.

## Resources

- [Go spec: Channel types](https://go.dev/ref/spec#Channel_types) — buffered-channel send/receive blocking semantics.
- [Effective Go: Channels](https://go.dev/doc/effective_go#channels) — the buffered channel as a limiting semaphore, with the fan-out example.
- [sync/atomic: CompareAndSwap](https://pkg.go.dev/sync/atomic#Int64.CompareAndSwap) — the lock-free peak tracker used to prove the cap held.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-waitgroup-barrier.md](02-waitgroup-barrier.md)
