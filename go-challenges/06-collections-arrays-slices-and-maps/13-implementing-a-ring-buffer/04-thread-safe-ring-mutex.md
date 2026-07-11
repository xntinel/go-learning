# Exercise 4: Make the Ring Concurrency-Safe for Multi-Producer Telemetry

The bare `Ring[T]` is single-goroutine. The moment many request handlers push
telemetry samples into one shared buffer, you need synchronization. This module
wraps the ring in a `SafeRing[T]` guarded by a `sync.RWMutex`: writers take the
write lock, the read-mostly `Snapshot` takes the read lock, and `Len` returns a
value that is only ever a hint under concurrency.

Self-contained: its own module, an inlined ring, the `SafeRing` wrapper, a demo,
and a `-race` stress test.

## What you'll build

```text
safering/                  independent module: example.com/safering
  go.mod                   go 1.24
  safering.go              ring[T] + SafeRing[T] (Push, Pop, Snapshot, Len)
  cmd/
    demo/
      main.go              concurrent producers, then a snapshot
  safering_test.go         -race: N producers x M pushes + concurrent readers
```

Files: `safering.go`, `cmd/demo/main.go`, `safering_test.go`.
Implement: `SafeRing[T]` with `Push`/`Pop` under `mu.Lock`, `Snapshot`/`Len` under `mu.RLock`.
Test: N goroutines each push M items while readers call `Snapshot`/`Len`; assert no race, `Len` never exceeds `Cap`, and observed values stay bounded.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/safering/cmd/demo
cd ~/go-exercises/safering
go mod init example.com/safering
go mod edit -go=1.24
```

### RWMutex, and why Len is only a hint

`Push` and `Pop` mutate the ring, so they take the exclusive write lock
(`mu.Lock`). `Snapshot` and `Len` only read, so they take the shared read lock
(`mu.RLock`), which lets many readers proceed concurrently as long as no writer
holds the write lock. That is the right split for a telemetry buffer where reads
(a `/debug` scrape, a metrics flush) are frequent and must not serialize against
each other, while writes are the hot path but each is short.

The important honesty: under concurrency, `Len` is a *hint*. By the time
`Len` returns, another goroutine may have pushed or popped, so the value is already
stale. This is not a bug to fix; it is the nature of a shared counter. Never write
`if r.Len() < r.Cap() { r.Push(x) }` expecting the two calls to be atomic — between
them the buffer can fill. If you need "push only if room," that decision must be
made *inside* a single locked operation (which is what the blocking queue in a later
module does). `Len` is for observability and rough sizing, not for control flow.

### Snapshot copies under the lock, then releases

`Snapshot` must take the read lock, copy the logical range into a fresh slice, and
release the lock before returning. Copying under the lock is what makes the returned
slice a consistent point-in-time view: no writer can tear it. Releasing before
returning is what keeps the lock held for O(size) and no longer — the caller can
then sort or marshal the copy at its leisure without blocking producers. Returning a
slice that aliases the internal array, or holding the lock across the caller's
processing, are the two ways to get this wrong.

Create `safering.go`:

```go
package safering

import (
	"errors"
	"sync"
)

// ErrEmpty is returned by Pop when the buffer is empty.
var ErrEmpty = errors.New("safering: buffer is empty")

type ring[T any] struct {
	data []T
	head int
	tail int
	size int
}

func newRing[T any](capacity int) *ring[T] {
	if capacity <= 0 {
		capacity = 1
	}
	return &ring[T]{data: make([]T, capacity)}
}

func (r *ring[T]) push(v T) {
	r.data[r.head] = v
	r.head = (r.head + 1) % len(r.data)
	if r.size < len(r.data) {
		r.size++
	} else {
		r.tail = (r.tail + 1) % len(r.data)
	}
}

func (r *ring[T]) pop() (T, bool) {
	var zero T
	if r.size == 0 {
		return zero, false
	}
	v := r.data[r.tail]
	r.data[r.tail] = zero
	r.tail = (r.tail + 1) % len(r.data)
	r.size--
	return v, true
}

func (r *ring[T]) snapshot() []T {
	out := make([]T, r.size)
	for i := range r.size {
		out[i] = r.data[(r.tail+i)%len(r.data)]
	}
	return out
}

// SafeRing is a concurrency-safe overwrite-oldest ring for multi-producer
// telemetry. Writes take an exclusive lock; reads take a shared lock.
type SafeRing[T any] struct {
	mu sync.RWMutex
	r  *ring[T]
}

// NewSafeRing returns a SafeRing holding at most capacity elements.
func NewSafeRing[T any](capacity int) *SafeRing[T] {
	return &SafeRing[T]{r: newRing[T](capacity)}
}

// Push adds v, overwriting the oldest element when full.
func (s *SafeRing[T]) Push(v T) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.r.push(v)
}

// Pop removes and returns the oldest element, or ErrEmpty if empty.
func (s *SafeRing[T]) Pop() (T, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.r.pop()
	if !ok {
		return v, ErrEmpty
	}
	return v, nil
}

// Snapshot returns an independent copy of the current elements, oldest first.
func (s *SafeRing[T]) Snapshot() []T {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.r.snapshot()
}

// Len returns the current element count. Under concurrency it is only a hint:
// the count may change the instant this returns.
func (s *SafeRing[T]) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.r.size
}

// Cap returns the fixed capacity.
func (s *SafeRing[T]) Cap() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.r.data)
}
```

### The runnable demo

The demo launches four producer goroutines, each pushing 100 samples into a
capacity-8 buffer, waits for them, then reports the final state. Because eviction
is racy across producers, the demo prints only the size and capacity (which are
deterministic) rather than the contents.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"

	"example.com/safering"
)

func main() {
	s := safering.NewSafeRing[int](8)
	var wg sync.WaitGroup
	for p := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 100 {
				s.Push(p*100 + i)
			}
		}()
	}
	wg.Wait()
	fmt.Printf("len=%d cap=%d\n", s.Len(), s.Cap())
	fmt.Printf("survivors=%d\n", len(s.Snapshot()))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
len=8 cap=8
survivors=8
```

Four hundred pushes into a capacity-8 buffer leave exactly eight survivors; which
eight is nondeterministic, but the count is not.

### Tests

The stress test is the point: `N` producers each push `M` distinct values while a
pool of readers hammers `Snapshot` and `Len`. Under `-race` this fails loudly if any
access is unsynchronized. The assertions that survive nondeterminism are the
invariants: `Len` never exceeds `Cap`, every `Snapshot` has length ≤ `Cap`, and the
final survivor count is exactly `Cap` (because far more than `Cap` items were
pushed).

Create `safering_test.go`:

```go
package safering

import (
	"sync"
	"testing"
)

func TestConcurrentProducersAndReaders(t *testing.T) {
	t.Parallel()
	const (
		producers = 8
		perProd   = 500
		capacity  = 16
	)
	s := NewSafeRing[int](capacity)

	var wg sync.WaitGroup
	// Producers.
	for p := range producers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range perProd {
				s.Push(p*perProd + i)
			}
		}()
	}
	// Concurrent readers, checking invariants live.
	stop := make(chan struct{})
	var rwg sync.WaitGroup
	for range 4 {
		rwg.Add(1)
		go func() {
			defer rwg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if n := s.Len(); n > s.Cap() {
					t.Errorf("Len %d exceeded Cap %d", n, s.Cap())
					return
				}
				if snap := s.Snapshot(); len(snap) > capacity {
					t.Errorf("Snapshot len %d exceeded capacity %d", len(snap), capacity)
					return
				}
			}
		}()
	}

	wg.Wait()
	close(stop)
	rwg.Wait()

	if got := s.Len(); got != capacity {
		t.Fatalf("final Len = %d, want %d (buffer should be full)", got, capacity)
	}
}

func TestPopUnderConcurrency(t *testing.T) {
	t.Parallel()
	s := NewSafeRing[int](32)
	for i := range 32 {
		s.Push(i)
	}
	var wg sync.WaitGroup
	popped := make([]int, 0, 32)
	var mu sync.Mutex
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				v, err := s.Pop()
				if err != nil {
					return
				}
				mu.Lock()
				popped = append(popped, v)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(popped) != 32 {
		t.Fatalf("popped %d items, want 32 (a Pop was lost or double-counted)", len(popped))
	}
}
```

## Review

The wrapper is correct when every field access happens under the appropriate lock
and the invariants hold under `-race`: writes exclusive, reads shared, `Len` and
`Cap` bounded. `TestConcurrentProducersAndReaders` proves both safety (the race
detector stays silent) and the bounded-memory contract (final `Len == Cap` after a
flood). The two mistakes to avoid: treating `Len` as a control-flow guard — it is
stale the instant it returns, so a compare-then-push is not atomic — and holding the
read lock across the caller's use of the snapshot, which turns a short copy into a
long stall on producers. Copy under the lock, release, then process. And remember
that a buffered channel already gives you a mutex-guarded ring for free; reach for
`SafeRing` when you need overwrite-oldest or an all-contents `Snapshot`, which a
channel cannot provide.

## Resources

- [`sync` package](https://pkg.go.dev/sync) — `sync.Mutex`, `sync.RWMutex`, `sync.WaitGroup`.
- [The Go Memory Model](https://go.dev/ref/mem) — what "safe for concurrent use" requires.
- [Go Code Review Comments: concurrency](https://go.dev/wiki/CodeReviewComments) — mutex and lock-scope idioms.

---

Back to [03-recent-log-ring-debug-endpoint.md](03-recent-log-ring-debug-endpoint.md) | Next: [05-sliding-window-latency-percentiles.md](05-sliding-window-latency-percentiles.md)
