# Exercise 9: Benchmark The Hot Path And Guard Allocations

A cache sits on the hot path, so an allocation regression in `Get` or `Set`
silently doubles GC pressure under load. This module builds a benchmark suite with
`b.Loop()` (Go 1.24) instead of the legacy `b.N` loop, `b.ReportAllocs()` to track
`allocs/op`, and `b.RunParallel` to measure the contended-`RWMutex` throughput a
real cache experiences under concurrent readers.

## What you'll build

```text
benchcache/                 independent module: example.com/benchcache
  go.mod
  cache.go                  the cache under test
  cmd/
    demo/
      main.go               runnable demo
  cache_test.go             BenchmarkGet/BenchmarkSet/BenchmarkGetParallel + a sanity test
```

Files: `cache.go`, `cmd/demo/main.go`, `cache_test.go`.
Implement: reuse the cache.
Test: `BenchmarkGet` and `BenchmarkSet` with `for b.Loop()` and `b.ReportAllocs()`; `BenchmarkGetParallel` with `b.RunParallel`; plus a normal `TestGetSet` so `go test` has something to run.
Verify: `go test -count=1 -race ./...` and `go test -bench . -benchmem -run '^$'`

### Why b.Loop replaces the b.N loop

The legacy benchmark shape, `for i := 0; i < b.N; i++`, has two well-known
footguns. First, any setup you write before the loop is included in the timed
region unless you manually call `b.ResetTimer()`, and forgetting it inflates the
measurement. Second, if the loop body's result is not consumed, the compiler is
free to delete the work as dead code, so you benchmark nothing and never notice.
`for b.Loop()` (Go 1.24) fixes both by construction: it resets the timer on the
first iteration and stops it on the last, so setup written *before* the loop is
automatically excluded, and it keeps the loop's arguments and results alive across
iterations so the compiler cannot eliminate the measured work. The common case
needs no `b.ResetTimer` at all.

`b.ReportAllocs()` is the guard that matters most for a cache. It makes the
benchmark report `allocs/op` and `B/op` alongside `ns/op`, so a change that adds a
per-call allocation — a defensive copy, a `fmt.Sprintf` on the hot path, a map
resize — shows up as a number you can diff run to run with `-benchmem` and
`benchstat`. An allocation regression is exactly the kind of defect that passes
every correctness test and only surfaces as GC pressure and tail latency in
production; the benchmark is where you catch it.

`BenchmarkGetParallel` measures something the serial benchmarks cannot: throughput
under contention. `b.RunParallel(func(pb *testing.PB){ for pb.Next() { ... } })`
runs the body on `GOMAXPROCS` goroutines, so `Get`'s `RLock` is exercised under
real concurrent read pressure. That number tells you whether the `RWMutex` lets
readers proceed in parallel (it should) or whether some accidental write-lock
serializes them.

Create `cache.go`:

```go
package benchcache

import (
	"errors"
	"sync"
	"time"
)

var (
	ErrNotFound = errors.New("cache: key not found")
	ErrExpired  = errors.New("cache: key expired")
)

type entry struct {
	value     []byte
	expiresAt time.Time
}

type Cache struct {
	mu   sync.RWMutex
	data map[string]entry
	now  func() time.Time
}

func New() *Cache {
	return &Cache{data: make(map[string]entry), now: time.Now}
}

func (c *Cache) Get(key string) ([]byte, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.data[key]
	if !ok {
		return nil, ErrNotFound
	}
	if !e.expiresAt.IsZero() && c.now().After(e.expiresAt) {
		return nil, ErrExpired
	}
	return e.value, nil
}

func (c *Cache) Set(key string, value []byte, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var expiresAt time.Time
	if ttl > 0 {
		expiresAt = c.now().Add(ttl)
	}
	c.data[key] = entry{value: value, expiresAt: expiresAt}
}

func (c *Cache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.data)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/benchcache"
)

func main() {
	c := benchcache.New()
	c.Set("k", []byte("v"), 0)
	v, _ := c.Get("k")
	fmt.Printf("k = %s\n", v)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
k = v
```

### The benchmark suite

Setup (populating the cache, building the value) lives *before* `for b.Loop()`, so
it is excluded from the measurement automatically. The read result is assigned to a
package-level sink so the compiler cannot prove it unused.

Create `cache_test.go`:

```go
package benchcache

import (
	"fmt"
	"testing"
)

// sink defeats dead-code elimination of benchmark results.
var sink []byte

func TestGetSet(t *testing.T) {
	t.Parallel()
	c := New()
	c.Set("k", []byte("v"), 0)
	got, err := c.Get("k")
	if err != nil || string(got) != "v" {
		t.Fatalf("Get(k) = %q, %v; want v, nil", got, err)
	}
}

func BenchmarkGet(b *testing.B) {
	c := New()
	c.Set("k", []byte("value"), 0) // setup: excluded from timing by b.Loop
	b.ReportAllocs()

	var v []byte
	for b.Loop() {
		v, _ = c.Get("k")
	}
	sink = v
}

func BenchmarkSet(b *testing.B) {
	c := New()
	value := []byte("value") // setup: excluded from timing
	b.ReportAllocs()

	for b.Loop() {
		c.Set("k", value, 0)
	}
}

func BenchmarkGetParallel(b *testing.B) {
	c := New()
	for i := range 1000 {
		c.Set(fmt.Sprintf("k:%d", i), []byte("value"), 0)
	}
	b.ReportAllocs()

	b.RunParallel(func(pb *testing.PB) {
		var v []byte
		for pb.Next() {
			v, _ = c.Get("k:500")
		}
		sink = v
	})
}
```

Run the benchmarks (not run by a plain `go test`):

```bash
go test -bench . -benchmem -run '^$'
```

Expected output shape (numbers vary by machine):

```
goos: darwin
goarch: arm64
pkg: example.com/benchcache
BenchmarkGet-10             	100000000	        11.5 ns/op	       0 B/op	       0 allocs/op
BenchmarkSet-10             	20000000	        58.2 ns/op	       0 B/op	       0 allocs/op
BenchmarkGetParallel-10     	200000000	         6.3 ns/op	       0 B/op	       0 allocs/op
```

The `0 allocs/op` on `BenchmarkGet` is the property to protect: any future change
that makes `Get` allocate will show a non-zero number here, and `benchstat old.txt
new.txt` turns that into a reviewable regression.

## Review

The benchmarks are correct when setup is outside `for b.Loop()` (so it is not
timed) and results flow into a sink (so they are not eliminated). `b.Loop()` gives
both for free where the old `b.N` loop needed a manual `b.ResetTimer` and a
hand-rolled sink. `b.ReportAllocs()` is the line that turns a benchmark into an
allocation guard — `0 allocs/op` on the read path is a contract worth defending
with `-benchmem` and `benchstat`. `BenchmarkGetParallel` via `b.RunParallel`
measures the contended-`RWMutex` throughput that serial benchmarks miss. Note that
`go test` does not run benchmarks by default; you invoke them with `-bench`, and
`-run '^$'` skips the ordinary tests so only the benchmarks run.

## Resources

- [`(*testing.B).Loop`](https://pkg.go.dev/testing#B.Loop) — the Go 1.24 benchmark loop that excludes setup and keeps values alive.
- [`(*testing.B).ReportAllocs` and `RunParallel`](https://pkg.go.dev/testing#B.RunParallel) — allocation reporting and contended-throughput measurement.
- [`golang.org/x/perf/cmd/benchstat`](https://pkg.go.dev/golang.org/x/perf/cmd/benchstat) — comparing benchmark runs to catch regressions.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [08-synctest-deterministic-ttl.md](08-synctest-deterministic-ttl.md) | Next: [10-fuzz-key-value-roundtrip.md](10-fuzz-key-value-roundtrip.md)
