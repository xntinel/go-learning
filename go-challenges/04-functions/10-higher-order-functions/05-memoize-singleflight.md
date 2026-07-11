# Exercise 5: Memoize Decorator with In-Flight Deduplication

A read-through cache in front of a slow dependency must survive a stampede: when N
callers ask for the same cold key at once, exactly one underlying call should run
and all N should share its result. This exercise builds that as a generic decorator
over `func(context.Context, K) (V, error)` using `singleflight`.

## What you'll build

```text
memoize/                     independent module: example.com/memoize
  go.mod                     go 1.25, requires golang.org/x/sync
  memoize.go                 Memoize[K,V]; caches successes, dedups in-flight calls, Forget
  memoize_test.go            stampede->1 call, second key, errors not cached, Forget, -race
  cmd/demo/
    main.go                  wraps a slow fetch and fires concurrent lookups
```

- Files: `memoize.go`, `memoize_test.go`, `cmd/demo/main.go`.
- Implement: `Memoize[K comparable, V any](fn func(context.Context, K) (V, error))` returning a decorated function plus a `Forget(K)` method, backed by a `singleflight.Group` and a mutex-guarded cache that stores only successes.
- Test: concurrent calls for one key trigger exactly one underlying call and share the value; a second key triggers a second call; errors are not cached; `Forget` forces recompute; run under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module (this one has an external dependency):

```bash
mkdir -p ~/go-exercises/memoize/cmd/demo
cd ~/go-exercises/memoize
go mod init example.com/memoize
go mod edit -go=1.25
go get golang.org/x/sync/singleflight
```

### Two jobs: cache and collapse

The decorator has two distinct responsibilities and it is worth separating them in
your head. The *cache* (a `map[K]V` under an `RWMutex`) serves already-computed
results with no call at all. The *singleflight group* handles the moment of a cold
miss under concurrency: the first goroutine to ask for key `k` runs `fn`; any other
goroutine that asks for the same `k` while that call is in flight blocks and then
receives the same `(value, error)` — `Group.Do` guarantees `fn` runs once per key
per in-flight window. The `shared` return value tells you a caller piggybacked on
someone else's call, which is how the test proves deduplication happened.

The order inside the decorated function is: check the cache under a read lock; on a
hit return immediately; on a miss enter `group.Do(key, ...)`; inside `Do`, run `fn`,
and *only if it succeeds* store the result in the cache under a write lock. Errors
are deliberately not cached. Caching a transient failure poisons the key: every
future caller would get the stale error until something evicts it, which is exactly
the opposite of what you want from a resilience layer. A failed call should re-run
next time.

`singleflight.Group.Do` takes a `string` key and a `func() (any, error)`. Since `K`
is `comparable` but not necessarily a string, derive the singleflight key with
`fmt.Sprint(key)`. For the map cache, use `K` directly. `Forget(key)` calls
`group.Forget` and deletes the cache entry so the next call recomputes — useful for
invalidation.

Create `memoize.go`:

```go
package memoize

import (
	"context"
	"fmt"
	"sync"

	"golang.org/x/sync/singleflight"
)

// Func is the decorated function shape: a keyed, cancellable lookup.
type Func[K comparable, V any] func(ctx context.Context, key K) (V, error)

// Memo caches successful results of fn and collapses concurrent calls for the
// same key into a single underlying invocation.
type Memo[K comparable, V any] struct {
	fn    Func[K, V]
	group singleflight.Group

	mu    sync.RWMutex
	cache map[K]V
}

// Memoize wraps fn. The returned *Memo is called via its Do method; Get is a
// convenience matching the original Func signature.
func Memoize[K comparable, V any](fn Func[K, V]) *Memo[K, V] {
	return &Memo[K, V]{fn: fn, cache: make(map[K]V)}
}

// Get returns the memoized result for key, computing it at most once across
// concurrent callers and caching only on success.
func (m *Memo[K, V]) Get(ctx context.Context, key K) (V, error) {
	m.mu.RLock()
	if v, ok := m.cache[key]; ok {
		m.mu.RUnlock()
		return v, nil
	}
	m.mu.RUnlock()

	v, err, _ := m.group.Do(fmt.Sprint(key), func() (any, error) {
		val, err := m.fn(ctx, key)
		if err != nil {
			return val, err // NOT cached: a failure must re-run next time
		}
		m.mu.Lock()
		m.cache[key] = val
		m.mu.Unlock()
		return val, nil
	})
	if err != nil {
		var zero V
		return zero, err
	}
	return v.(V), nil
}

// Forget drops any cached value and in-flight tracking for key, so the next Get
// recomputes it.
func (m *Memo[K, V]) Forget(key K) {
	m.group.Forget(fmt.Sprint(key))
	m.mu.Lock()
	delete(m.cache, key)
	m.mu.Unlock()
}
```

One race subtlety worth naming: the context passed to `fn` inside `group.Do` is the
context of whichever goroutine happened to win the call. If that caller's context is
cancelled, the shared call is cancelled for everyone. In production you often pass a
detached context into the shared call for exactly this reason; here the callers
share a background context so the demo and tests stay simple.

### The runnable demo

The demo wraps a fetch that sleeps 50ms and increments a counter, then fires ten
concurrent lookups for the same key. Because the calls collapse, the underlying
fetch runs once, not ten times.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"example.com/memoize"
)

func main() {
	var calls atomic.Int64
	fetch := func(ctx context.Context, key string) (string, error) {
		calls.Add(1)
		time.Sleep(50 * time.Millisecond) // slow downstream
		return "profile:" + key, nil
	}

	m := memoize.Memoize(fetch)

	var wg sync.WaitGroup
	results := make([]string, 10)
	for i := range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, _ := m.Get(context.Background(), "alice")
			results[i] = v
		}()
	}
	wg.Wait()

	fmt.Printf("underlying calls: %d\n", calls.Load())
	fmt.Printf("result: %s\n", results[0])

	// A cached key needs no further call.
	m.Get(context.Background(), "alice")
	fmt.Printf("calls after cached read: %d\n", calls.Load())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
underlying calls: 1
result: profile:alice
calls after cached read: 1
```

Ten concurrent lookups for `alice` produce one underlying call, and the later
cached read adds none. The count is deterministic because the 50ms sleep guarantees
all ten goroutines are waiting on the same in-flight call before it returns.

### Tests

The stampede test wraps a fetch with an atomic counter and a delay, launches many
goroutines for one key, and asserts the underlying call count is exactly one and
every caller got the same value. The errors-not-cached test makes the fetch fail
once then succeed, and asserts the second call actually re-runs `fn`. `Forget` is
tested by priming the cache, forgetting the key, and asserting the next call
recomputes.

Create `memoize_test.go`:

```go
package memoize

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMemoizeCollapsesStampede(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	m := Memoize(func(ctx context.Context, key string) (int, error) {
		calls.Add(1)
		time.Sleep(20 * time.Millisecond)
		return len(key), nil
	})

	const n = 50
	var wg sync.WaitGroup
	got := make([]int, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := m.Get(context.Background(), "key")
			if err != nil {
				t.Errorf("Get: %v", err)
			}
			got[i] = v
		}()
	}
	wg.Wait()

	if c := calls.Load(); c != 1 {
		t.Fatalf("underlying calls = %d, want 1 (stampede not collapsed)", c)
	}
	for i := range got {
		if got[i] != 3 {
			t.Fatalf("caller %d got %d, want 3 (shared result)", i, got[i])
		}
	}
}

func TestMemoizeSecondKeyTriggersSecondCall(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	m := Memoize(func(ctx context.Context, key string) (string, error) {
		calls.Add(1)
		return key, nil
	})

	if _, err := m.Get(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Get(context.Background(), "b"); err != nil {
		t.Fatal(err)
	}
	if c := calls.Load(); c != 2 {
		t.Fatalf("calls = %d, want 2 (distinct keys)", c)
	}
}

func TestMemoizeDoesNotCacheErrors(t *testing.T) {
	t.Parallel()

	errBoom := errors.New("boom")
	var calls atomic.Int64
	m := Memoize(func(ctx context.Context, key string) (int, error) {
		if calls.Add(1) == 1 {
			return 0, errBoom
		}
		return 42, nil
	})

	if _, err := m.Get(context.Background(), "k"); !errors.Is(err, errBoom) {
		t.Fatalf("first Get err = %v, want boom", err)
	}
	v, err := m.Get(context.Background(), "k")
	if err != nil {
		t.Fatalf("second Get err = %v, want nil (error must not be cached)", err)
	}
	if v != 42 {
		t.Fatalf("second Get = %d, want 42", v)
	}
	if c := calls.Load(); c != 2 {
		t.Fatalf("calls = %d, want 2 (failure must re-run)", c)
	}
}

func TestMemoizeForgetRecomputes(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	m := Memoize(func(ctx context.Context, key string) (int64, error) {
		return calls.Add(1), nil
	})

	first, _ := m.Get(context.Background(), "k")
	cached, _ := m.Get(context.Background(), "k")
	if first != 1 || cached != 1 {
		t.Fatalf("first=%d cached=%d, want 1 and 1", first, cached)
	}

	m.Forget("k")
	recomputed, _ := m.Get(context.Background(), "k")
	if recomputed != 2 {
		t.Fatalf("after Forget = %d, want 2 (recompute)", recomputed)
	}
}
```

## Review

The decorator is correct when a stampede of concurrent lookups for one cold key
produces exactly one underlying call and every caller receives the same value —
that is `singleflight` doing its job, and the atomic counter under `-race` proves
both the deduplication and the map safety. A second, distinct key must trigger a
second call; the cache is per key. The load-bearing decision is not caching errors:
a failed call must re-run, so store the result only inside the `err == nil` branch.
`Forget` exists for invalidation and must clear both the cache entry and the
singleflight tracking. Note the shared-context subtlety: the call inside `Do` runs
under one caller's context, so a production version often detaches it.

## Resources

- [golang.org/x/sync/singleflight](https://pkg.go.dev/golang.org/x/sync/singleflight) — `Group.Do`, `Group.Forget`, and the `shared` return.
- [sync package](https://pkg.go.dev/sync) — `RWMutex` for the cache map.
- [context package](https://pkg.go.dev/context) — the cancellation semantics of the shared call.

---

Back to [04-validation-pipeline.md](04-validation-pipeline.md) | Next: [06-lazy-init-oncevalue.md](06-lazy-init-oncevalue.md)
