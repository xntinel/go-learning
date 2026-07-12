# Exercise 2: Reproduce and Read a Real -race Report on an Unsynchronized Cache

The primary diagnostic skill is not seeing a test fail -- it is reading the
`WARNING: DATA RACE` block and naming the two conflicting accesses. This exercise
ships a deliberately unsynchronized map-backed cache under `cmd/racy` that
produces a genuine report when you run it with `-race`, walks you through
annotating that report field by field, exercises the `GORACE` options that
control it, and then proves the diagnosis with a fixed `sync.RWMutex` cache whose
concurrent test passes under `-race`.

This module is self-contained: its own `go mod init`, its own racy demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
racecache/                  independent module: example.com/racecache
  go.mod                    go 1.26
  cache.go                  type Cache (sync.RWMutex): New, Set, Get -- the FIXED version
  cmd/
    racy/
      main.go               deliberately-unsynchronized map cache; run manually with -race
  cache_test.go             TestCacheConcurrentSetGet hammers the fixed cache under -race
```

Files: `cache.go`, `cmd/racy/main.go`, `cache_test.go`.
Implement: the racy map cache in `cmd/racy` (for the report) and the fixed
`RWMutex` cache in the package root.
Test: `TestCacheConcurrentSetGet` hammers `Set`/`Get` from many goroutines and
passes under `-race`.
Verify: `go test -count=1 -race ./...`; then `go run -race ./cmd/racy` to observe
the report; then `GORACE="exitcode=77 halt_on_error=1" go run -race ./cmd/racy`.

### The racy artifact lives under cmd/racy on purpose

The whole point of this exercise is to see a real report, but a deliberately
racy program cannot live in the test suite -- `go test -race` would trip on it and
the CI gate would be red forever. The convention that keeps the gate green is to
put the intentionally-racy code in a `main` package under `cmd/racy` that the
normal test run only *builds*, never *executes*. You reproduce the report by
running it by hand with `go run -race ./cmd/racy`. The package-root code is the
fixed, tested version.

The racy cache is the most common real-world shape: a plain `map` written by
background aggregators and read on the request path, with no synchronization.
Concurrent map access is doubly dangerous -- a memory-model data race and,
separately, a runtime that may `throw("concurrent map writes")` and abort the
process unrecoverably -- so this is exactly the bug `-race` exists to surface
before production does.

Create `cmd/racy/main.go`:

```go
// Command racy is a deliberately-unsynchronized in-memory cache used to
// reproduce a real "WARNING: DATA RACE" report. Run it manually with:
//
//	go run -race ./cmd/racy
//
// It is a main package with no test, so `go test -race ./...` only builds it and
// never executes the race. Do NOT copy this pattern into production code.
package main

import (
	"fmt"
	"sync"
)

// racyCache has no mutex: Set and Get touch the same map with no happens-before
// edge between them. This is the bug.
type racyCache struct {
	items map[string]int
}

// Set writes the map; Get reads it. There is no happens-before edge between
// them, which is the data race.
func (c *racyCache) Set(k string, v int) { c.items[k] = v }
func (c *racyCache) Get(k string) int    { return c.items[k] }

func main() {
	c := &racyCache{items: make(map[string]int)}

	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Set(fmt.Sprintf("k%d", i), i) // writers
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = c.Get(fmt.Sprintf("k%d", i)) // readers
		}()
	}
	wg.Wait()

	fmt.Println("done (if you saw no report, the scheduler missed the race; re-run)")
}
```

### Reading the report it produces

Running `go run -race ./cmd/racy` prints a block shaped like this (addresses,
goroutine numbers, and line offsets vary per run):

```text
==================
WARNING: DATA RACE
Write at 0x00c000112018 by goroutine 9:
  main.(*racyCache).Set()
      /path/cmd/racy/main.go:24 +0x4c
  main.main.func1()
      /path/cmd/racy/main.go:36 +0x5c

Previous read at 0x00c000112018 by goroutine 8:
  main.(*racyCache).Get()
      /path/cmd/racy/main.go:25 +0x64
  main.main.func2()
      /path/cmd/racy/main.go:41 +0x5c

Goroutine 9 (running) created at:
  main.main()
      /path/cmd/racy/main.go:35 +0x108

Goroutine 8 (finished) created at:
  main.main()
      /path/cmd/racy/main.go:40 +0xf8
==================
```

Annotate it field by field, because this is the skill:

- Line 1 of the pair: `Write at 0x...018 by goroutine 9` in `(*racyCache).Set`
  at `main.go:24`. That is the map write `c.items[k] = v`.
- Line 2 of the pair: `Previous read at 0x...018 by goroutine 8` in
  `(*racyCache).Get` at `main.go:25`. That is `return c.items[k]`.
- The address `0x...018` is identical in both, confirming it is one memory
  location (the map's internal state), and one access is a write, so the three
  conditions for a data race are met.
- The two `Goroutine N ... created at` stacks point at `main.go:35` and
  `main.go:40` -- the `go func()` launches -- telling you the writers and readers
  were started with no synchronization edge between them.

The diagnostic conclusion: `Set` and `Get` touch the same map with no
happens-before edge. The fix is to introduce one -- a mutex.

### Controlling the report with GORACE

The runtime reads `GORACE` from the environment. Two options you will actually
use in CI:

```bash
# Default: report goes to stderr, process exits 66 after the run.
go run -race ./cmd/racy

# Fail fast with a custom exit code: stop at the first race, exit 77.
GORACE="exitcode=77 halt_on_error=1" go run -race ./cmd/racy
echo $?   # -> 77

# Write reports to a file <path>.<pid> instead of stderr, and deepen history.
GORACE="log_path=./race history_size=3 halt_on_error=1" go run -race ./cmd/racy
```

`exitcode` sets the process exit status when a race is detected (default 66);
`halt_on_error=1` stops at the first race instead of continuing; `log_path`
redirects reports to `<path>.<pid>` (with `stdout`/`stderr` as special values);
`history_size` enlarges the per-goroutine access history so deep call chains do
not print "failed to restore the stack." A CI race target sets these so a
detected race fails the build with a known exit code.

### The fixed cache

The fix is the read-heavy-map branch of the decision tree: a `sync.RWMutex`.
Many request goroutines read (`RLock`), the occasional writer takes the exclusive
`Lock`. Every `Set` now happens-before a subsequent `Get` that observes it,
through the mutex's release/acquire edge, so the race is gone.

Create `cache.go`:

```go
package racecache

import "sync"

// Cache is a concurrency-safe in-memory cache. Reads take a shared RLock; the
// writer takes the exclusive Lock. This is the fixed version of the racy map
// cache under cmd/racy.
type Cache struct {
	mu    sync.RWMutex
	items map[string]int
}

// New returns an empty Cache.
func New() *Cache {
	return &Cache{items: make(map[string]int)}
}

// Set stores v under k.
func (c *Cache) Set(k string, v int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[k] = v
}

// Get returns the value under k and whether it was present.
func (c *Cache) Get(k string) (int, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.items[k]
	return v, ok
}

// Len reports the number of stored entries.
func (c *Cache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.items)
}
```

### Tests

`TestCacheConcurrentSetGet` is the proof that the diagnosed race is gone: it runs
many writer and reader goroutines against the fixed cache simultaneously and
passes under `-race`. It is the same contention shape as `cmd/racy`, but on the
mutex-guarded cache, so the detector finds no unordered access.

Create `cache_test.go`:

```go
package racecache

import (
	"fmt"
	"strconv"
	"sync"
	"testing"
)

func TestCacheConcurrentSetGet(t *testing.T) {
	t.Parallel()

	c := New()
	const n = 200
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Set(strconv.Itoa(i), i)
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = c.Get(strconv.Itoa(i))
		}()
	}
	wg.Wait()

	if got := c.Len(); got != n {
		t.Fatalf("Len() = %d after %d Sets, want %d", got, n, n)
	}
}

func TestCacheGetMissing(t *testing.T) {
	t.Parallel()

	c := New()
	if v, ok := c.Get("absent"); ok || v != 0 {
		t.Fatalf("Get(absent) = %d,%v, want 0,false", v, ok)
	}
}

func ExampleCache() {
	c := New()
	c.Set("hits", 7)
	v, ok := c.Get("hits")
	fmt.Println(v, ok)
	// Output: 7 true
}
```

## Review

You have read a real report correctly when you can point at the exact write and
the exact read, confirm they share one address with at least one write, and name
the missing happens-before edge -- here, a mutex between `Set` and `Get`. The
fixed cache proves the diagnosis: `TestCacheConcurrentSetGet` hammers the same
contention under `-race` and stays green because the `RWMutex` orders every
access. The `GORACE` invocations show you control the detector's exit code and
output, which is what a CI race target depends on.

The mistakes to avoid: do not put the racy program in the test suite (it lives in
`cmd/racy`, run manually), and do not conclude "no report this run means no race"
-- a data race on a map is nondeterministic, so a single clean run can miss it;
re-run, or trust the reasoning that an unsynchronized shared map is a race by
construction. Run `go test -count=1 -race ./...` for the fixed cache, and
`go run -race ./cmd/racy` to see the report.

## Resources

- [Data Race Detector](https://go.dev/doc/articles/race_detector) -- the report format, `GORACE` options (`exitcode`, `halt_on_error`, `log_path`, `history_size`), and how the detector runs.
- [The Go Memory Model](https://go.dev/ref/mem) -- the precise happens-before definition the report is checking against.
- [`sync.RWMutex`](https://pkg.go.dev/sync#RWMutex) -- `RLock`/`RUnlock` for readers, `Lock`/`Unlock` for the writer.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-concurrent-metrics-counter.md](01-concurrent-metrics-counter.md) | Next: [03-rwmutex-feature-flag-store.md](03-rwmutex-feature-flag-store.md)
