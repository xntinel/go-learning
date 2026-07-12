# Exercise 5: Thread-Safe Metrics Registry — defer Unlock and Its Limits

`mu.Lock(); defer mu.Unlock()` is the exception-safe way to hold a critical
section: the unlock happens on every exit, even a panic. Use it by default. But
the same `defer` widens the critical section to the whole function, and that is
wrong the moment slow work follows. This module builds an in-memory metrics
registry that uses the deferred unlock for its short paths and the
copy-under-lock pattern for its `Flush`, which must not hold the lock across a
slow sink.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports any other exercise.

## What you'll build

```text
metrics/                    independent module: example.com/metrics
  go.mod
  metrics/metrics.go         Registry: Inc, Snapshot (defer unlock), Flush (copy-under-lock)
  cmd/demo/main.go           100 goroutines Inc a key, then Snapshot and Flush
  metrics/metrics_test.go    64-goroutine Inc sum under -race; Flush does not hold the lock
```

- Files: `metrics/metrics.go`, `cmd/demo/main.go`, `metrics/metrics_test.go`.
- Implement: a `Registry` over a `sync.RWMutex` with `Inc(name, delta)` and `Snapshot()` using `defer Unlock`/`RUnlock`, and `Flush(sink)` that copies the counters under the read lock, unlocks explicitly, and only then calls the slow sink.
- Test: 64 goroutines `Inc` a shared key and the final `Snapshot` equals the total (under `-race`); a second test blocks the sink on a channel and proves another goroutine's `Inc` still makes progress, so `Flush` is not holding the lock during the slow write.
- Verify: `go test -count=1 -race ./...`

### Two patterns, one lock

`Inc` and `Snapshot` are short: acquire, touch the map, release. For them
`defer mu.Unlock()` is exactly right — the critical section is the whole (tiny)
function, and the defer guarantees the unlock even if a map operation were to
panic. `Snapshot` returns a copy (`maps.Clone`) rather than the live map, because
handing the internal map to a caller would let it be read without the lock while
`Inc` mutates it — a data race the `-race` detector would flag. Copying under the
read lock is cheap and keeps the registry's map private.

`Flush` is where the deferred unlock would be a mistake. Flushing means shipping
the counters to a slow sink — a StatsD socket, an HTTP endpoint, a file. If you
wrote

```go
func (r *Registry) Flush(sink func(map[string]int64) error) error {
	r.mu.RLock()
	defer r.mu.RUnlock()          // WRONG: held across the slow sink
	return sink(maps.Clone(r.counters))
}
```

the read lock is held for the entire duration of the network write. Under an
`RWMutex`, other readers can proceed, but any `Inc` (a writer) blocks until the
flush's network round-trip completes — you have serialized every metric increment
behind the slowest thing in the system. The senior fix is the copy-under-lock
pattern: take the lock, copy the state you need, release the lock explicitly, and
run the slow work on your private copy. The lock protects the map, not the
network.

Create `metrics/metrics.go`:

```go
package metrics

import (
	"maps"
	"sync"
)

// Registry is a concurrency-safe set of named counters.
type Registry struct {
	mu       sync.RWMutex
	counters map[string]int64
}

func New() *Registry {
	return &Registry{counters: make(map[string]int64)}
}

// Inc adds delta to a counter. The critical section is the whole (short)
// function, so a deferred unlock is exactly right.
func (r *Registry) Inc(name string, delta int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counters[name] += delta
}

// Snapshot returns a copy of the counters. It copies under the read lock so the
// caller never touches the live map while Inc mutates it.
func (r *Registry) Snapshot() map[string]int64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return maps.Clone(r.counters)
}

// Flush ships the counters to a slow sink. It copies under the read lock, then
// releases the lock explicitly BEFORE calling sink, so the slow write does not
// block concurrent Inc. This is the copy-under-lock pattern: the lock guards the
// map, not the network.
func (r *Registry) Flush(sink func(map[string]int64) error) error {
	r.mu.RLock()
	snap := maps.Clone(r.counters)
	r.mu.RUnlock() // explicit: do NOT defer this across the slow sink
	return sink(snap)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"

	"example.com/metrics/metrics"
)

func main() {
	r := metrics.New()

	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.Inc("requests", 1)
		}()
	}
	wg.Wait()

	snap := r.Snapshot()
	fmt.Println("requests:", snap["requests"])

	_ = r.Flush(func(m map[string]int64) error {
		fmt.Println("flushed keys:", len(m))
		return nil
	})
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
requests: 100
flushed keys: 1
```

### Tests

The first test is the correctness proof under `-race`: 64 goroutines increment a
shared key, and the final `Snapshot` must equal the exact total. The second test
is the design proof: it blocks the sink inside `Flush` and shows another
goroutine's `Inc` still completes, which can only happen if `Flush` released the
lock before calling the sink.

Create `metrics/metrics_test.go`:

```go
package metrics

import (
	"sync"
	"testing"
	"time"
)

func TestConcurrentIncSumsExactly(t *testing.T) {
	t.Parallel()

	r := New()
	const (
		workers = 64
		each    = 1000
	)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range each {
				r.Inc("hits", 1)
			}
		}()
	}
	wg.Wait()

	if got := r.Snapshot()["hits"]; got != workers*each {
		t.Fatalf("hits = %d, want %d", got, workers*each)
	}
}

func TestFlushDoesNotHoldLockDuringSink(t *testing.T) {
	t.Parallel()

	r := New()
	r.Inc("x", 5)

	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)

	go func() {
		done <- r.Flush(func(m map[string]int64) error {
			close(entered) // we are now inside the slow sink
			<-release      // block here, simulating a slow network write
			return nil
		})
	}()

	<-entered // Flush has reached the sink

	// If Flush still held the lock, this Inc (a writer) would block until the
	// sink returns. It must make progress within the timeout.
	incDone := make(chan struct{})
	go func() {
		r.Inc("x", 1)
		close(incDone)
	}()

	select {
	case <-incDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Inc blocked: Flush held the lock during the slow sink")
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("Flush = %v", err)
	}
	if got := r.Snapshot()["x"]; got != 6 {
		t.Fatalf("x = %d, want 6", got)
	}
}
```

## Review

The registry is correct when concurrent `Inc` calls sum exactly — the 64-worker
test under `-race` is the proof that the mutex actually serializes the map writes,
and that `Snapshot` copies rather than aliases the live map. The design point is
`Flush`: the second test wedges the sink open and shows an `Inc` completing
anyway, which is only possible because `Flush` unlocked before the slow work. The
mistake to avoid is reflexively writing `defer mu.Unlock()` in every method; it is
right for the short paths and a latency bottleneck for any method that keeps doing
slow I/O after reading the shared state. The discipline is: hold the lock for the
state access, copy what you need, release, then be slow. Run `go test -race`.

## Resources

- [sync.RWMutex](https://pkg.go.dev/sync#RWMutex)
- [maps.Clone](https://pkg.go.dev/maps#Clone)
- [Go Blog: Share Memory By Communicating](https://go.dev/blog/codelab-share)
- [Go Wiki: Mutex or Channel](https://go.dev/wiki/MutexOrChannel)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-defer-loop-leak-batch-importer.md](04-defer-loop-leak-batch-importer.md) | Next: [06-latency-timing-defer-and-arg-eval.md](06-latency-timing-defer-and-arg-eval.md)
