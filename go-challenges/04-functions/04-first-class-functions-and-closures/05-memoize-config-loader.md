# Exercise 5: Memoize an Expensive Config Loader with a Closure Cache

Memoization is a higher-order function whose private state is a cache map living
inside a closure. `Memoize[K, V](load)` wraps a loader so each distinct key hits
the underlying loader at most once; repeated loads of the same key are served from
the captured cache. Framed here as caching parsed feature-flag records keyed by
name, with one deliberate policy decision: errors are not cached, so a transient
failure is retried on the next call.

This module is fully self-contained.

## What you'll build

```text
memoize/                   independent module: example.com/memoize
  go.mod                   go 1.26
  memoize.go               Memoize[K comparable, V any] closure cache
  cmd/
    demo/
      main.go              loads the same flag twice, a second flag once
  memoize_test.go          hit counting, errors-not-cached, concurrent same key
```

- Files: `memoize.go`, `cmd/demo/main.go`, `memoize_test.go`.
- Implement: `Memoize[K comparable, V any](load func(K) (V, error)) func(K) (V, error)` returning a closure over a mutex-guarded cache map; successful loads are cached, errors are not.
- Test: two loads of one key return equal values with the loader called once; two distinct keys call it twice; a failing load is retried (not cached); concurrent loads of one key are safe under `-race`.
- Verify: `go test -count=1 -race ./...`

### The cache is the captured environment

`Memoize` declares a `cache map[K]V` and a `sync.Mutex` and returns a closure that
consults them. Those two variables are the memoizer's entire state, private to
the returned function and shared by no one else. Because the returned function is
generic over `K comparable` and `V any`, the cache is fully typed — no `any`, no
type assertions — and the same `Memoize` serves a config loader keyed by string
and a user loader keyed by an int.

Two design points deserve care.

First, **the lock is held across the underlying `load` call.** That serializes
concurrent loads: if two goroutines request the same missing key at once, one
loads while the other waits, and the waiter then finds the value already cached.
This guarantees `load` runs at most once per key even under concurrency — the
property the concurrency test asserts. The trade-off is that a slow load of one
key blocks loads of *other* keys too; a production single-flight cache (see
`golang.org/x/sync/singleflight`) uses per-key coordination to avoid that. For a
lesson on closures the single-mutex version is the right size, and its behavior is
easy to state and test.

Second, **errors are not cached.** If `load` returns an error, the memoizer
returns it without storing anything, so the next call retries. This is a
deliberate, documented policy: caching a transient failure (a DNS blip, a
momentary DB outage) would serve that failure forever. The contrast module
(Exercise 10, the `sync.Once` guard) makes the *opposite* choice on purpose — it
caches its build error — precisely so you see both policies and the situations
each fits.

Create `memoize.go`:

```go
package memoize

import "sync"

// Memoize wraps load so each distinct key calls the underlying loader at most
// once; repeated loads of the same key are served from a captured cache.
// Successful values are cached; errors are NOT cached, so a failed load is
// retried on the next call.
func Memoize[K comparable, V any](load func(K) (V, error)) func(K) (V, error) {
	var (
		mu    sync.Mutex
		cache = make(map[K]V)
	)
	return func(key K) (V, error) {
		mu.Lock()
		defer mu.Unlock()

		if v, ok := cache[key]; ok {
			return v, nil
		}
		v, err := load(key)
		if err != nil {
			var zero V
			return zero, err
		}
		cache[key] = v
		return v, nil
	}
}
```

### The runnable demo

The demo memoizes a loader that counts its own calls. Loading `checkout-v2`
twice and `search-v3` once results in two underlying loads, not three.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/memoize"
)

// FeatureFlag is a stand-in for a parsed config record.
type FeatureFlag struct {
	Name    string
	Enabled bool
}

func main() {
	calls := 0
	load := func(name string) (FeatureFlag, error) {
		calls++
		return FeatureFlag{Name: name, Enabled: true}, nil
	}

	get := memoize.Memoize(load)
	_, _ = get("checkout-v2")
	_, _ = get("checkout-v2") // served from cache
	_, _ = get("search-v3")

	fmt.Printf("loader calls=%d\n", calls)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
loader calls=2
```

### Tests

Create `memoize_test.go`:

```go
package memoize

import (
	"errors"
	"sync"
	"testing"
)

func TestMemoizeCachesByKey(t *testing.T) {
	t.Parallel()
	calls := 0
	load := func(k string) (int, error) {
		calls++
		return len(k), nil
	}
	get := Memoize(load)

	a, _ := get("alpha")
	b, _ := get("alpha")
	if a != b || a != 5 {
		t.Fatalf("values = %d,%d, want 5,5", a, b)
	}
	if calls != 1 {
		t.Fatalf("loader called %d times for one key, want 1", calls)
	}

	_, _ = get("beta")
	if calls != 2 {
		t.Fatalf("loader called %d times for two keys, want 2", calls)
	}
}

func TestMemoizeDoesNotCacheErrors(t *testing.T) {
	t.Parallel()
	errBoom := errors.New("boom")
	calls := 0
	load := func(k string) (int, error) {
		calls++
		if calls == 1 {
			return 0, errBoom
		}
		return 42, nil
	}
	get := Memoize(load)

	if _, err := get("k"); !errors.Is(err, errBoom) {
		t.Fatalf("first err = %v, want errBoom", err)
	}
	v, err := get("k") // retried, not served from an error cache
	if err != nil || v != 42 {
		t.Fatalf("second load = %d,%v, want 42,nil", v, err)
	}
	if calls != 2 {
		t.Fatalf("loader called %d times, want 2 (error retried)", calls)
	}
}

func TestMemoizeConcurrentSameKey(t *testing.T) {
	t.Parallel()
	var calls int
	var mu sync.Mutex
	load := func(k string) (int, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return len(k), nil
	}
	get := Memoize(load)

	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = get("same")
		}()
	}
	wg.Wait()

	if calls != 1 {
		t.Fatalf("loader called %d times under concurrency, want 1", calls)
	}
}
```

## Review

The memoizer is correct when it calls the underlying loader once per distinct
key, serves repeats from the captured cache, and — by documented policy — retries
rather than caches errors. The captured `cache` map and mutex are the closure's
private state; holding the lock across `load` is what makes the "called once even
concurrently" guarantee hold, which is why `TestMemoizeConcurrentSameKey` passes
under `-race`. The error-policy test pins the decision that a transient failure
must not be served forever. If you needed to avoid blocking unrelated keys during
a slow load, you would reach for per-key coordination
(`singleflight`); the single-mutex form is the right shape for the closure lesson.
Run `go test -race`.

## Resources

- [pkg.go.dev: sync.Mutex](https://pkg.go.dev/sync#Mutex) — guarding the captured cache map.
- [Go spec: Type parameter declarations](https://go.dev/ref/spec#Type_parameter_declarations) — the `[K comparable, V any]` constraints.
- [pkg.go.dev: golang.org/x/sync/singleflight](https://pkg.go.dev/golang.org/x/sync/singleflight) — production per-key de-duplication of concurrent loads.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-retry-with-backoff-higher-order.md](04-retry-with-backoff-higher-order.md) | Next: [06-sequence-id-generator.md](06-sequence-id-generator.md)
