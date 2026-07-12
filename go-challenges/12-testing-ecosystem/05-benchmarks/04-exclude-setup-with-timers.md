# Exercise 4: Exclude Setup Cost with ResetTimer/StopTimer/StartTimer

When a benchmark needs expensive setup — filling a cache with N entries, building a
fixture — that cost must be kept out of the measured region or every per-op number is
inflated by it. This module benchmarks a read-through cache guarded by an `RWMutex`,
uses `b.ResetTimer()` to exclude the one-time fill, and `StopTimer`/`StartTimer` to
exclude per-iteration setup, then shows the wrong version where the fill leaks into
the measurement.

## What you'll build

```text
readcache/                 independent module: example.com/readcache
  go.mod                   go 1.24
  cache.go                 type Cache (map guarded by sync.RWMutex); Set, Get, Len
  cmd/
    demo/
      main.go              runnable demo: fill, hit, miss
  cache_test.go            TestCacheGet (hit/miss); BenchmarkGetReset (ResetTimer);
                           BenchmarkGetStopStart (StopTimer/StartTimer); concurrency test; Example
```

- Files: `cache.go`, `cmd/demo/main.go`, `cache_test.go`.
- Implement: a concurrency-safe `Cache` with `Set`, `Get`, `Len` over an `RWMutex`.
- Test: hit and miss semantics, a `-race` concurrent test, and two setup-excluding benchmarks.
- Verify: `go test -count=1 -race ./...` then `go test -bench=. -benchmem`.

Set up the module:

```bash
go mod edit -go=1.24
```

### Where the setup cost hides

The benchmark of interest is a cache `Get`: how fast is a hit? To measure that you
first need a populated cache, and populating it with, say, 10,000 entries is far more
expensive than a single lookup. If the fill runs inside the timed region, its cost is
divided across the `b.N` lookups and the reported `ns/op` is dominated by fill, not
lookup. The naive, wrong shape looks like this — do not assemble it:

```text
// WRONG: the fill is inside the timed region, inflating every reported op
func BenchmarkGetWrong(b *testing.B) {
	c := New()
	for i := range 10_000 {          // this fill is measured...
		c.Set(strconv.Itoa(i), i)
	}
	for range b.N {
		_, _ = c.Get("5000")         // ...and folded into this lookup's ns/op
	}
}
```

`b.ResetTimer()` fixes it: called after the fill, it zeroes the elapsed-time and
allocation counters so only the lookups that follow are measured. That is
`BenchmarkGetReset`. A subtler case is when each *iteration* needs fresh setup that
must not be measured — here, computing a fresh key per lookup. `StopTimer` before that
work and `StartTimer` after it exclude just that slice of each iteration; time between
them is not counted. That is `BenchmarkGetStopStart`. The caveat from the concepts
holds: the toggle calls have overhead, so in a loop this tight the stop/start version
is a teaching illustration — for real per-iteration inputs you would precompute a
slice of keys above the loop instead, which this module also demonstrates in the
reset variant.

The cache itself is a `map[string]int` guarded by a `sync.RWMutex`: `Get` takes the
read lock (so concurrent readers do not serialize), `Set` takes the write lock. That
read/write split is the standard shape of a read-through cache and is what the
`-race` test exercises.

Create `cache.go`:

```go
package readcache

import "sync"

// Cache is a concurrency-safe string->int cache. Reads take the read lock so
// concurrent hits do not serialize; writes take the write lock.
type Cache struct {
	mu    sync.RWMutex
	items map[string]int
}

// New returns an empty cache.
func New() *Cache {
	return &Cache{items: make(map[string]int)}
}

// Set stores value under key.
func (c *Cache) Set(key string, value int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[key] = value
}

// Get returns the value for key and whether it was present.
func (c *Cache) Get(key string) (int, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.items[key]
	return v, ok
}

// Len reports the number of entries.
func (c *Cache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.items)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/readcache"
)

func main() {
	c := readcache.New()
	c.Set("user:1", 100)
	c.Set("user:2", 200)

	if v, ok := c.Get("user:1"); ok {
		fmt.Printf("hit  user:1 -> %d\n", v)
	}
	if _, ok := c.Get("user:9"); !ok {
		fmt.Println("miss user:9")
	}
	fmt.Printf("len = %d\n", c.Len())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
hit  user:1 -> 100
miss user:9
len = 2
```

### Tests

`TestCacheGet` covers hit and miss. The two benchmarks both fill 10,000 entries and
then measure only the lookups: `BenchmarkGetReset` calls `b.ResetTimer()` after the
fill and looks up precomputed keys, and `BenchmarkGetStopStart` shows the
`StopTimer`/`StartTimer` bracket around per-iteration key construction. A concurrent
test drives `Set`/`Get` from many goroutines so `-race` proves the `RWMutex` guards
the map.

Create `cache_test.go`:

```go
package readcache

import (
	"fmt"
	"strconv"
	"sync"
	"testing"
)

func TestCacheGet(t *testing.T) {
	t.Parallel()
	c := New()
	c.Set("a", 1)

	if v, ok := c.Get("a"); !ok || v != 1 {
		t.Fatalf("Get(a) = %d,%v; want 1,true", v, ok)
	}
	if _, ok := c.Get("missing"); ok {
		t.Fatal("Get(missing) reported present")
	}
}

func TestConcurrentAccess(t *testing.T) {
	t.Parallel()
	c := New()
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			k := strconv.Itoa(i)
			c.Set(k, i)
			c.Get(k)
		}()
	}
	wg.Wait()
	if c.Len() != 100 {
		t.Fatalf("Len = %d, want 100", c.Len())
	}
}

const fillN = 10_000

func BenchmarkGetReset(b *testing.B) {
	c := New()
	keys := make([]string, fillN)
	for i := range fillN {
		keys[i] = strconv.Itoa(i)
		c.Set(keys[i], i)
	}
	b.ReportAllocs()
	b.ResetTimer() // exclude the fill above from the measured region
	for i := range b.N {
		_, _ = c.Get(keys[i%fillN])
	}
}

func BenchmarkGetStopStart(b *testing.B) {
	c := New()
	for i := range fillN {
		c.Set(strconv.Itoa(i), i)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		b.StopTimer()
		key := strconv.Itoa(i % fillN) // per-iteration setup, not measured
		b.StartTimer()
		_, _ = c.Get(key)
	}
}

func ExampleCache() {
	c := New()
	c.Set("k", 42)
	v, ok := c.Get("k")
	fmt.Println(v, ok)
	// Output: 42 true
}
```

Run the benchmarks; with the fill excluded, the reported cost is a single lookup:

```bash
go test -bench=. -benchmem
```

```text
BenchmarkGetReset-8        38104922      31.2 ns/op     0 B/op    0 allocs/op
BenchmarkGetStopStart-8     4922013     243 ns/op      7 B/op    0 allocs/op
PASS
```

## Review

`TestCacheGet` and the concurrent test establish the cache is correct and race-free
under the `RWMutex`. The benchmark lesson is setup exclusion: `BenchmarkGetReset`
reports the true cost of a hit because `ResetTimer` dropped the 10,000-entry fill from
the measurement, and if you deleted that `ResetTimer` line the number would jump by
orders of magnitude — the fill re-entering the average. `BenchmarkGetStopStart` shows
the finer `StopTimer`/`StartTimer` tool for per-iteration setup, and also shows its
cost: it reports a larger `ns/op` than the reset variant partly because the toggle
calls and `strconv.Itoa` are heavier than the lookup, which is exactly why the concepts
warn against timer toggling in tight loops and why the reset variant precomputes its
keys instead. Prefer precomputation; reach for `StopTimer` only when setup genuinely
cannot leave the loop.

## Resources

- [`testing.B.ResetTimer`](https://pkg.go.dev/testing#B.ResetTimer) — zero the timer and memory counters to exclude preceding setup.
- [`testing.B.StopTimer`](https://pkg.go.dev/testing#B.StopTimer) / [`StartTimer`](https://pkg.go.dev/testing#B.StartTimer) — bracket an unmeasured region inside the loop.
- [`sync.RWMutex`](https://pkg.go.dev/sync#RWMutex) — the read/write lock guarding the cache map.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-prevent-dead-code-elimination.md](03-prevent-dead-code-elimination.md) | Next: [05-throughput-with-setbytes.md](05-throughput-with-setbytes.md)
