# Exercise 2: Race-safe coverage of a concurrent TTL cache

The default coverage counter is a plain memory write. Instrument a piece of
concurrent code, run it under parallel tests, and those writes race — corrupting
the count and tripping the race detector on the instrumentation itself. This
module builds a small in-memory TTL cache guarded by a `sync.RWMutex`, hammers it
from parallel subtests, and uses it to make `-covermode` concrete: why `atomic`
is mandatory under `-race`, and what `count` mode reveals about hot versus cold
branches.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
ttlcov/                    independent module: example.com/ttlcov
  go.mod
  cache.go                 Cache[K,V]: Get, Set, evict; RWMutex-guarded
  cmd/
    demo/
      main.go              runnable demo: set, read, expire, read again
  cache_test.go            parallel Get/Set subtests; boundary expiry test; Example
```

- Files: `cache.go`, `cmd/demo/main.go`, `cache_test.go`.
- Implement: a generic `Cache[K comparable, V any]` with `Set(key, value, ttl)`, `Get(key) (V, bool)` that lazily treats expired entries as absent, and an internal eviction step that deletes an expired entry on read.
- Test: parallel subtests that concurrently `Set` and `Get` under `-race`; a boundary test that reads just before and just after expiry; an `Example`.
- Verify: `go test -count=1 -race -covermode=atomic -cover ./...`

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/20-test-coverage-analysis/02-covermode-atomic-race-cache/cmd/demo
cd go-solutions/12-testing-ecosystem/20-test-coverage-analysis/02-covermode-atomic-race-cache
```

### Why this cache needs atomic coverage

The cache stores each value with an expiry instant and reads `time.Now()`
directly. `Get` takes a read lock to look up the entry; if the entry has expired
it upgrades to a write lock to delete it (active eviction on read) so memory does
not leak for keys that are read after expiry. `Set` takes a write lock. Nothing
here is exotic — it is the shape of a thousand real caches.

The point is what happens under coverage instrumentation. Every basic block in
`Get`, `Set`, and the eviction path carries a counter. In the default `set` mode
(and in `count` mode) that counter is a non-atomic write. When twenty goroutines
call `Get` at once — which the test does on purpose — they all execute the same
instrumented blocks and write the same counters concurrently. Those writes are a
data race independent of whether *your* code is correctly synchronized: your
`RWMutex` protects the map, but it does not protect the compiler-inserted
coverage counters. Run `go test -race -covermode=set` and the race detector
reports a race inside the instrumentation. The `atomic` mode replaces the write
with an atomic add, which is race-free but slower. Because this is a real and
easy trap, the toolchain upgrades `-covermode` to `atomic` automatically whenever
`-race` is present; the only way to get bitten is to override it back to `set`.

`count` mode has a different use here. Because it records how many times each
block ran, the `-func`/profile shows which branch of the cache is *hot*. Under a
read-heavy workload the lookup block runs on every `Get` while the eviction block
runs only when an entry has actually expired — hot versus cold. That is exactly
the frequency information `set` mode throws away, and it is how you find a branch
that is technically covered but almost never exercised (a good place for a bug to
hide).

Create `cache.go`:

```go
package ttlcov

import (
	"sync"
	"time"
)

type entry[V any] struct {
	value   V
	expires time.Time
}

// Cache is a concurrency-safe TTL map. It reads the wall clock through time.Now
// and evicts an expired entry actively when it is read.
type Cache[K comparable, V any] struct {
	mu    sync.RWMutex
	items map[K]entry[V]
}

// New returns an empty cache.
func New[K comparable, V any]() *Cache[K, V] {
	return &Cache[K, V]{items: make(map[K]entry[V])}
}

// Set stores value under key, expiring it ttl from now.
func (c *Cache[K, V]) Set(key K, value V, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[key] = entry[V]{value: value, expires: time.Now().Add(ttl)}
}

// Get returns the value if present and unexpired. An expired entry is evicted
// and reported absent.
func (c *Cache[K, V]) Get(key K) (V, bool) {
	c.mu.RLock()
	e, ok := c.items[key]
	c.mu.RUnlock()

	var zero V
	if !ok {
		return zero, false
	}
	if !time.Now().Before(e.expires) {
		c.evict(key)
		return zero, false
	}
	return e.value, true
}

// evict deletes key if it is still expired under the write lock.
func (c *Cache[K, V]) evict(key K) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[key]; ok && !time.Now().Before(e.expires) {
		delete(c.items, key)
	}
}

// Len reports the number of entries currently stored.
func (c *Cache[K, V]) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.items)
}
```

### The runnable demo

The demo uses a real short sleep so you can watch an actual eviction. It stores a
session for 40 ms, reads it, sleeps past the TTL, reads again, and prints the
length to show the entry was evicted.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/ttlcov"
)

func main() {
	c := ttlcov.New[string, string]()
	c.Set("session", "alice", 40*time.Millisecond)

	if v, ok := c.Get("session"); ok {
		fmt.Printf("before expiry: %s (len=%d)\n", v, c.Len())
	}

	time.Sleep(80 * time.Millisecond)

	if _, ok := c.Get("session"); !ok {
		fmt.Printf("after expiry: evicted (len=%d)\n", c.Len())
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
before expiry: alice (len=1)
after expiry: evicted (len=0)
```

On the second line `Get` returns `false` and actively evicts the expired entry
before `Len` is read, so the length is `0` — the active-eviction-on-read path in
action.

### The tests

`TestConcurrentSetGet` is the coverage-relevant test: it launches many goroutines
that `Set` and `Get` overlapping keys, so the instrumented blocks in `Get`,
`Set`, and `evict` all execute concurrently. Under `-race -covermode=atomic` it
passes; it is the workload that would trip the detector in `set` mode.
`TestExpiryBoundary` uses a real short TTL to check the read-before/read-after
behavior and drives the eviction branch. Both use `t.Parallel()` where safe.

Create `cache_test.go`:

```go
package ttlcov

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestConcurrentSetGet(t *testing.T) {
	t.Parallel()
	c := New[int, int]()
	const workers = 32

	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 100 {
				key := (w*100 + i) % 50 // overlapping keys force contention
				c.Set(key, i, time.Minute)
				c.Get(key)
			}
		}()
	}
	wg.Wait()

	if c.Len() == 0 {
		t.Fatal("expected some live entries after concurrent Set")
	}
}

func TestExpiryBoundary(t *testing.T) {
	t.Parallel()
	c := New[string, int]()
	c.Set("k", 7, 30*time.Millisecond)

	if v, ok := c.Get("k"); !ok || v != 7 {
		t.Fatalf("Get right after Set = %d,%v; want 7,true", v, ok)
	}

	time.Sleep(60 * time.Millisecond)

	if _, ok := c.Get("k"); ok {
		t.Fatal("entry still present after TTL; eviction branch not taken")
	}
	if c.Len() != 0 {
		t.Fatalf("Len after eviction = %d, want 0", c.Len())
	}
}

func TestMissingKey(t *testing.T) {
	t.Parallel()
	c := New[string, int]()
	if _, ok := c.Get("absent"); ok {
		t.Fatal("Get on missing key reported present")
	}
}

func ExampleCache() {
	c := New[string, int]()
	c.Set("answer", 42, time.Minute)
	v, ok := c.Get("answer")
	fmt.Println(v, ok)
	// Output: 42 true
}
```

### Comparing covermodes

Run the race-safe form the way CI does. `-race` forces atomic mode; the explicit
flag documents it:

```bash
go test -count=1 -race -covermode=atomic -cover ./...
```

Now see the trap. Forcing `set` mode under `-race` reintroduces the counter race:

```bash
go test -race -covermode=set ./...
```

Expected output (abbreviated) — the detector fires on the instrumentation:

```
==================
WARNING: DATA RACE
...
Read at 0x... by goroutine ...:
  example.com/ttlcov.(*Cache[...]).Get
...
FAIL    example.com/ttlcov      ...
```

Finally, use `count` mode (no `-race`) to see hot versus cold branches. The lookup
block runs on every `Get`; the eviction block runs only for entries read after
expiry:

```bash
go test -covermode=count -coverprofile=count.out ./...
go tool cover -func=count.out
```

The per-function numbers are the same as `set` mode, but the profile now carries
execution counts per block; `go tool cover -html=count.out` shades hotter blocks
more strongly, which is how you spot a branch that is covered but rarely run.

## Review

The cache is correct when `Get` returns a live value under a read lock, evicts an
expired entry under a write lock, and reports missing and expired keys as absent;
`Len` reflects the stored count after eviction. The coverage lesson is correct
when `go test -race -covermode=atomic` passes cleanly and `go test -race
-covermode=set` reports a data race *inside the coverage instrumentation* — proof
that the default-to-atomic behavior is protecting you, not decoration.

The mistake to avoid is hardcoding `-covermode=set` (or `count`) alongside
`-race` because "set is the default" — under `-race` it is not, and forcing it
races the counters your `RWMutex` cannot protect. Use `atomic` under `-race`, and
reach for `count` (without `-race`) only when you specifically want hot/cold
frequency data. Run `go test -race` to confirm the map access itself is clean.

## Resources

- [Testing flags (`-covermode`, `-race`)](https://pkg.go.dev/cmd/go#hdr-Testing_flags) — how `-race` interacts with `-covermode`.
- [`go tool cover`](https://pkg.go.dev/cmd/cover) — count-mode profiles and the HTML heat view.
- [`sync` package](https://pkg.go.dev/sync#RWMutex) — `RWMutex` semantics for the read/evict path.

---

Back to [01-coverage-basics-math-lib.md](01-coverage-basics-math-lib.md) | Next: [03-coverpkg-cross-package-service.md](03-coverpkg-cross-package-service.md)
