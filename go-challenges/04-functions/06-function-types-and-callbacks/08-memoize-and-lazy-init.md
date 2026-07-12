# Exercise 8: Higher-Order Memoization and Lazy Singletons

Two higher-order tools show up in every backend: a memoizer that caches expensive
lookups, and a lazy singleton that initializes a shared resource exactly once. This
module builds `Memoize` over a mutex-guarded map (caching successes only, never errors)
and a lazy config loader with `sync.OnceValues`, and proves the exactly-once and
panic-memoization semantics.

This module is fully self-contained: its own `go mod init`, all code inline, its own
demo and tests.

## What you'll build

```text
memo/                       independent module: example.com/memo
  go.mod                    go 1.26
  memo.go                   Memoize[K,V]; LazyConfig via sync.OnceValues
  cmd/
    demo/
      main.go               runnable demo: cache hits, one-time init
  memo_test.go              once-per-key, no-error-caching, once, panic tests
```

Files: `memo.go`, `cmd/demo/main.go`, `memo_test.go`.
Implement: `Memoize[K comparable, V any](fn func(K) (V, error)) func(K) (V, error)` caching successful results in a mutex-guarded map, and a lazy singleton built with `sync.OnceValues`.
Test: a counting fn is called once per distinct key and zero times on hits; an error result is not cached; concurrent access is race-free; the `OnceValues` initializer runs exactly once across many goroutines; a panicking initializer re-panics every call.
Verify: `go test -count=1 -race ./...`

### Memoize caches successes only; OnceValues initializes exactly once

`Memoize` wraps a `func(K) (V, error)` and returns a caching version. The subtlety that
separates a correct memoizer from a broken one is the error policy: only *successful*
results are cached. If the underlying function returns an error — a transient DB
timeout, a rate-limit — caching it would serve that failure forever; the next call must
re-invoke the function. So the cache stores a value only when `err == nil`.

Concurrency is the second subtlety. Two goroutines asking for the same missing key at the
same time both miss and both call the underlying function. This naive version guards the
map with a `sync.Mutex` so the map itself is race-free, but it does *not* single-flight:
under a stampede the underlying function may run more than once for the same key
(bounded by the number of concurrent first-callers). That is a deliberate, documented
trade-off — true single-flight (one in-flight call per key, others wait) needs
`golang.org/x/sync/singleflight` or a per-key lock, which is more machinery than a
teaching memoizer needs. The test therefore asserts the call count is *bounded*, not
exactly one, under concurrency, and exactly one in the sequential case.

The lazy singleton is the exactly-once counterpart. `sync.OnceValues(f)` returns a
function that runs `f` a single time, no matter how many goroutines call it
concurrently, and caches the `(value, error)` pair. Every later call returns the cached
pair without re-running `f`. This is how you lazily build a DB pool, parse a config, or
compile templates: init runs on first use, once, safely. Two guarantees define correct
use: the initializer runs at most once across all goroutines, and a *panic* in the
initializer is memoized — `sync.OnceValue`/`OnceValues` (via the underlying
`sync.Once`) re-raise the same panic on every subsequent call rather than retrying.
`OnceValues` is not a retry: if init can transiently fail, handle it inside `f`.

Create `memo.go`:

```go
package memo

import (
	"fmt"
	"sync"
)

// Memoize returns a caching wrapper around fn. Only successful results are
// cached; an error is never cached, so a later call re-invokes fn. The wrapper
// is safe for concurrent use but does not single-flight: concurrent first
// callers for the same key may each invoke fn once.
func Memoize[K comparable, V any](fn func(K) (V, error)) func(K) (V, error) {
	var mu sync.Mutex
	cache := make(map[K]V)
	return func(key K) (V, error) {
		mu.Lock()
		if v, ok := cache[key]; ok {
			mu.Unlock()
			return v, nil
		}
		mu.Unlock()

		v, err := fn(key)
		if err != nil {
			var zero V
			return zero, err // do not cache failures
		}

		mu.Lock()
		cache[key] = v
		mu.Unlock()
		return v, nil
	}
}

// Config is an expensive-to-build resource stood up lazily and exactly once.
type Config struct {
	DSN     string
	Workers int
}

// NewLazyConfig returns a loader that parses the config exactly once, even under
// concurrent first callers, memoizing the (Config, error) result.
func NewLazyConfig(source map[string]string) func() (Config, error) {
	return sync.OnceValues(func() (Config, error) {
		dsn, ok := source["dsn"]
		if !ok {
			return Config{}, fmt.Errorf("config: missing dsn")
		}
		return Config{DSN: dsn, Workers: 8}, nil
	})
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"strings"

	"example.com/memo"
)

func main() {
	calls := 0
	upper := memo.Memoize(func(s string) (string, error) {
		calls++
		return strings.ToUpper(s), nil
	})

	fmt.Println(mustGet(upper("go")))
	fmt.Println(mustGet(upper("go"))) // cache hit, no new call
	fmt.Println(mustGet(upper("rust")))
	fmt.Printf("underlying calls: %d\n", calls)

	load := memo.NewLazyConfig(map[string]string{"dsn": "postgres://localhost"})
	c1, _ := load()
	c2, _ := load() // same instance, no re-parse
	fmt.Printf("same config: %v\n", c1 == c2)
}

func mustGet(s string, err error) string {
	if err != nil {
		panic(err)
	}
	return s
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
GO
GO
RUST
underlying calls: 2
same config: true
```

### Tests

Create `memo_test.go`:

```go
package memo

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

func TestCachesOncePerKey(t *testing.T) {
	t.Parallel()
	var calls int
	double := Memoize(func(n int) (int, error) {
		calls++
		return n * 2, nil
	})

	for range 3 {
		if v, _ := double(21); v != 42 {
			t.Fatalf("double(21) = %d, want 42", v)
		}
	}
	double(10)
	if calls != 2 {
		t.Fatalf("underlying called %d times, want 2 (one per distinct key)", calls)
	}
}

func TestErrorsAreNotCached(t *testing.T) {
	t.Parallel()
	var calls int
	sentinel := errors.New("transient")
	flaky := Memoize(func(k string) (int, error) {
		calls++
		if calls == 1 {
			return 0, sentinel // first call fails
		}
		return 99, nil
	})

	if _, err := flaky("k"); !errors.Is(err, sentinel) {
		t.Fatalf("first call err = %v, want sentinel", err)
	}
	v, err := flaky("k") // must re-invoke because the error was not cached
	if err != nil || v != 99 {
		t.Fatalf("second call = %d,%v; want 99,nil (error must not be cached)", v, err)
	}
	if calls != 2 {
		t.Fatalf("underlying called %d times, want 2", calls)
	}
}

func TestConcurrentMemoizeBounded(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	get := Memoize(func(k int) (int, error) {
		calls.Add(1)
		return k, nil
	})

	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			get(7) // all ask for the same key
		}()
	}
	wg.Wait()
	// Not single-flight: bounded by concurrent first-callers, never zero.
	if n := calls.Load(); n < 1 || n > 100 {
		t.Fatalf("underlying called %d times, want within [1,100]", n)
	}
}

func TestOnceValuesRunsOnce(t *testing.T) {
	t.Parallel()
	var inits atomic.Int64
	load := sync.OnceValues(func() (int, error) {
		inits.Add(1)
		return 42, nil
	})

	var wg sync.WaitGroup
	results := make([]int, 50)
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, _ := load()
			results[i] = v
		}()
	}
	wg.Wait()
	if inits.Load() != 1 {
		t.Fatalf("initializer ran %d times, want exactly 1", inits.Load())
	}
	for i, v := range results {
		if v != 42 {
			t.Fatalf("caller %d got %d, want 42", i, v)
		}
	}
}

func TestPanicIsMemoized(t *testing.T) {
	t.Parallel()
	var inits int
	load := sync.OnceValue(func() int {
		inits++
		panic("init failed")
	})

	for i := range 3 {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("call %d did not re-panic", i)
				}
			}()
			load()
		}()
	}
	if inits != 1 {
		t.Fatalf("initializer body ran %d times, want 1 (panic is memoized)", inits)
	}
}

func ExampleMemoize() {
	calls := 0
	square := Memoize(func(n int) (int, error) {
		calls++
		return n * n, nil
	})
	square(5)
	square(5)
	v, _ := square(5)
	fmt.Println(v, calls)
	// Output: 25 1
}
```

## Review

The memoizer is correct when its cache policy is "successes only": `TestErrorsAreNotCached`
proves a first-call failure is re-attempted rather than served from cache, which is the
one bug that turns a memoizer into a permanent outage after a transient blip. Sequential
call counts are exact (one per distinct key); concurrent counts are *bounded*, not one,
because this memoizer deliberately does not single-flight — that limitation is documented
and the test asserts the bound rather than pretending otherwise. The lazy singleton's two
guarantees are the exactly-once initialization across 50 goroutines and the panic
memoization: `sync.OnceValue` caches a panic and re-raises it on every call, so an
initializer that panics is not silently retried. Run `-race` to confirm the mutex guards
the map and `sync.Once` guards the initializer.

## Resources

- [sync.OnceFunc / OnceValue / OnceValues](https://pkg.go.dev/sync#OnceValue)
- [sync.Mutex](https://pkg.go.dev/sync#Mutex)
- [golang.org/x/sync/singleflight](https://pkg.go.dev/golang.org/x/sync/singleflight)
- [Go blog: sync.OnceFunc and friends (Go 1.21 release notes)](https://go.dev/doc/go1.21#sync)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [07-validation-rule-pipeline.md](07-validation-rule-pipeline.md) | Next: [09-predicate-stream-filter.md](09-predicate-stream-filter.md)
