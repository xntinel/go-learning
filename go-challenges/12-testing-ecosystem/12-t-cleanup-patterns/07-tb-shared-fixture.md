# Exercise 7: One testing.TB Fixture Serving Both Tests and a b.Loop Benchmark

A senior suite has both a correctness path and a performance path over the same
subsystem, and duplicating the setup between them is how the two drift apart. The
`testing.TB` interface — satisfied by `*testing.T`, `*testing.B`, and `*testing.F`
— lets one seed helper hydrate a fixture and register its teardown once, then be
called identically from a `Test` and a `Benchmark`.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports any other exercise.

## What you'll build

```text
readcache/                   independent module: example.com/readcache
  go.mod                     go 1.24 (b.Loop needs it)
  cache.go                   Source (slow backing store, hit counter) + read-through Cache
  cmd/
    demo/
      main.go                runnable demo: miss then hit, watch the source-hit counter
  cache_test.go              seedCache(tb testing.TB) shared by a T test and a B benchmark
```

- Files: `cache.go`, `cmd/demo/main.go`, `cache_test.go`.
- Implement: a `Source` with a `Load` that counts backing hits, a read-through `Cache` with `Get`, and a `SourceHits` accessor.
- Test: a `seedCache(tb testing.TB)` helper that warms the cache and registers teardown via `tb.Cleanup`, called from both `TestCacheHitReturnsSeeded` (a `*testing.T`) and `BenchmarkCacheGet` (a `*testing.B`).
- Verify: `go test -count=1 -race ./...` and `go test -bench=. -benchmem ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/readcache/cmd/demo
cd ~/go-exercises/readcache
go mod init example.com/readcache
go mod edit -go=1.24
```

### One fixture, two callers

`Cleanup`, `Helper`, `TempDir`, `Setenv`, and `Context` all live on the
`testing.TB` interface, which `*testing.T`, `*testing.B`, and `*testing.F` all
satisfy. A helper typed `func(tb testing.TB) *Cache` can therefore be called with
either a `*testing.T` or a `*testing.B` and will register its cleanup on whichever
one it was handed. That is the senior payoff: the correctness suite and the
throughput benchmark seed the fixture through the *same* code, so a change to the
seed data or its teardown lands in both at once and they cannot drift.

The benchmark side has one hard rule. The measured window is exactly the body of
`for b.Loop() { ... }`. Anything registered *outside* that loop — the seed, and
the `b.Cleanup` the seed helper registers — is excluded from the measured region
and reused across iterations rather than re-run per iteration. Put setup or
teardown *inside* the loop and you fold seeding cost into the number `go test
-bench` reports. So the shape is invariant: seed before the loop, call the
operation under test inside it, and let teardown fire from the cleanup the helper
already registered.

The guard that proves the fixture is warm is the source-hit counter. Seeding warms
the cache with one backing `Load` per key; every read inside the benchmark loop is
then a cache hit that never touches the source. Asserting the source-hit count did
not grow during the loop proves the measured reads were served from the cache, not
from the slow backing store — otherwise the benchmark would be timing the source,
not the cache.

Create `cache.go`:

```go
package readcache

import (
	"sync"
	"sync/atomic"
)

// Source is a slow backing store (stand-in for a repository or remote service).
// It counts every backing load so a test can prove a read was served from cache.
type Source struct {
	data   map[string]string
	closed atomic.Bool
	hits   atomic.Int64
}

// NewSource returns a source seeded with a copy of data.
func NewSource(data map[string]string) *Source {
	m := make(map[string]string, len(data))
	for k, v := range data {
		m[k] = v
	}
	return &Source{data: m}
}

// Load fetches a key from the backing store, counting the backing hit.
func (s *Source) Load(key string) (string, bool) {
	s.hits.Add(1)
	v, ok := s.data[key]
	return v, ok
}

// Hits reports how many backing loads have occurred.
func (s *Source) Hits() int64 { return s.hits.Load() }

// Close releases the source. It is idempotent so a fixture cleanup is safe.
func (s *Source) Close() error {
	s.closed.Store(true)
	return nil
}

// Cache is a read-through cache over a Source: a miss consults the source once
// and memoizes the result; subsequent reads never touch the source.
type Cache struct {
	src   *Source
	mu    sync.Mutex
	items map[string]string
}

// NewCache returns a cache backed by src.
func NewCache(src *Source) *Cache {
	return &Cache{src: src, items: make(map[string]string)}
}

// Get returns the value for key, consulting the source only on a miss.
func (c *Cache) Get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if v, ok := c.items[key]; ok {
		return v, true
	}
	v, ok := c.src.Load(key)
	if !ok {
		return "", false
	}
	c.items[key] = v
	return v, true
}

// SourceHits exposes the backing-load count for assertions and demos.
func (c *Cache) SourceHits() int64 { return c.src.Hits() }
```

### The runnable demo

The demo reads the same key twice and prints the source-hit counter after each, so
you can watch the second read served from cache without a backing load.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/readcache"
)

func main() {
	src := readcache.NewSource(map[string]string{"user:1": "alice"})
	c := readcache.NewCache(src)

	v, _ := c.Get("user:1") // miss: one backing load
	fmt.Printf("first read: %s (source hits %d)\n", v, c.SourceHits())

	v, _ = c.Get("user:1") // hit: no backing load
	fmt.Printf("second read: %s (source hits %d)\n", v, c.SourceHits())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
first read: alice (source hits 1)
second read: alice (source hits 1)
```

### The tests

`seedCache` is the shared fixture: typed on `testing.TB`, it warms every key
(one backing load each) and registers `tb.Cleanup(src.Close)`. `TestCacheHitReturnsSeeded`
calls it with a `*testing.T` and asserts a warm read returns the seeded value with
no extra source hit. `BenchmarkCacheGet` calls the identical helper with a
`*testing.B`, seeds once before `for b.Loop()`, reads inside the loop, and after
the loop asserts the source-hit count never grew past the seed count — proving the
measured reads were cache hits.

Create `cache_test.go`:

```go
package readcache

import (
	"fmt"
	"testing"
)

// seedData is the fixture's warm set: three keys, warmed once each.
var seedData = map[string]string{
	"user:1": "alice",
	"user:2": "bob",
	"user:3": "carol",
}

// seedCache builds a cache over a fresh source, warms every seed key, and
// registers teardown via tb.Cleanup. Typed on testing.TB, it is called
// identically from a *testing.T test and a *testing.B benchmark.
func seedCache(tb testing.TB) *Cache {
	tb.Helper()
	src := NewSource(seedData)
	c := NewCache(src)
	for k := range seedData {
		if _, ok := c.Get(k); !ok {
			tb.Fatalf("seed: key %q missing from source", k)
		}
	}
	tb.Cleanup(func() {
		if err := src.Close(); err != nil {
			tb.Errorf("source close: %v", err)
		}
	})
	return c
}

func TestCacheHitReturnsSeeded(t *testing.T) {
	t.Parallel()
	c := seedCache(t)
	before := c.SourceHits()

	got, ok := c.Get("user:2")
	if !ok || got != "bob" {
		t.Fatalf("Get(user:2) = %q,%v; want bob,true", got, ok)
	}
	if after := c.SourceHits(); after != before {
		t.Fatalf("warm read caused %d extra source hit(s)", after-before)
	}
}

func TestCacheMissConsultsSource(t *testing.T) {
	t.Parallel()
	c := seedCache(t)
	if _, ok := c.Get("absent"); ok {
		t.Fatal("Get(absent) reported present")
	}
}

func BenchmarkCacheGet(b *testing.B) {
	c := seedCache(b) // setup outside the measured window; teardown via b.Cleanup
	warm := c.SourceHits()
	b.ReportAllocs()

	keys := []string{"user:1", "user:2", "user:3"}
	i := 0
	for b.Loop() {
		if _, ok := c.Get(keys[i%len(keys)]); !ok {
			b.Fatalf("benchmark read missed key %q", keys[i%len(keys)])
		}
		i++
	}

	if got := c.SourceHits(); got != warm {
		b.Fatalf("source hits grew to %d during warm benchmark, want %d", got, warm)
	}
}

func ExampleCache() {
	src := NewSource(map[string]string{"region": "eu-west-1"})
	c := NewCache(src)
	v, _ := c.Get("region")
	fmt.Println(v, c.SourceHits())
	// Output: eu-west-1 1
}
```

## Review

The fixture is correct when the same `seedCache` call satisfies both the test and
the benchmark and neither re-implements setup. The proof that the perf harness is
honest is the source-hit guard: `BenchmarkCacheGet` asserts the counter did not
move during `for b.Loop()`, so the measured reads were cache hits and the seeding
cost sits outside the timed window. The mistakes to avoid are benchmark-shaped:
never register seed or teardown *inside* the loop — the seed belongs before it and
the teardown belongs in the `b.Cleanup` that `seedCache` already registered, both
outside the measured region. Do not fork a second copy of the setup for the
benchmark; the whole point of typing the helper on `testing.TB` is that there is
one setup. Run `go test -race` for the correctness path and `go test -bench=.
-benchmem` to see the warm reads measured with no source traffic.

## Resources

- [`testing.TB`](https://pkg.go.dev/testing#TB) — the interface shared by `*T`, `*B`, and `*F`, exposing `Cleanup`, `Helper`, and `TempDir`.
- [`testing.B.Loop`](https://pkg.go.dev/testing#B.Loop) — the Go 1.24 benchmark loop; setup before it and cleanup after are excluded from the measured window.
- [`testing.B.ReportAllocs`](https://pkg.go.dev/testing#B.ReportAllocs) — report per-op allocations alongside timing.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [06-context-aware-worker-shutdown.md](06-context-aware-worker-shutdown.md) | Next: [08-setenv-chdir-serial-config.md](08-setenv-chdir-serial-config.md)
