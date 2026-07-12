# Exercise 4: Cache-stampede guard (single-flight load per key)

When a hot key expires and a hundred requests miss the cache at once, a naive
read-through cache fires a hundred identical loads at the database — a cache
stampede, or thundering herd. The fix is single-flight: the first goroutine to
miss a key loads it while every concurrent requester for the SAME key blocks and
then shares the one result. This module builds that guard from a `Cond`, which is
exactly the mechanism behind `golang.org/x/sync/singleflight`. It is a per-key
fan-in: many waiters block on "is this key's load done yet" and are released
together by a `Broadcast`.

## What you'll build

```text
sfcache/                    independent module: example.com/sfcache
  go.mod                    module path example.com/sfcache
  cache.go                  type Cache[K,V]: New(loader), Get; single-flight per key
  cmd/
    demo/
      main.go               concurrent Gets on a cold key -> one load
  cache_test.go             one-load fan-in, error fan-out + retry, -race stress
```

- Files: `cache.go`, `cmd/demo/main.go`, `cache_test.go`.
- Implement: a generic `Cache[K comparable, V any]` built with a `loader func(K) (V, error)`, exposing `Get(key) (V, error)` that de-duplicates concurrent loads of the same key and caches successful results.
- Test: `K` concurrent `Get(key)` run the loader exactly once and all observe the same value; a loader error is delivered to every current waiter and a later `Get` retries.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/15-sync-primitives/06-sync-cond/04-single-flight-cache/cmd/demo
cd go-solutions/15-sync-primitives/06-sync-cond/04-single-flight-cache
```

### The in-flight generation, and why Broadcast

On a miss, the first goroutine records an in-flight `*call` for the key, drops the
lock, and runs the (slow) loader. Any other goroutine that arrives while the load
is in flight finds the same `*call`, parks on the `Cond` until `call.done` is set,
and then reads the SHARED result off that `*call`. Because every waiter for the
key must wake when the load completes, the loader `Broadcast`s. A single `Cond`
guards all keys, so a `Broadcast` for key A also wakes key B's waiters; each waiter
loops on ITS OWN `call.done`, re-checks, and re-parks if its load is not the one
that finished. That cross-key wakeup is a little wasteful but perfectly correct —
the predicate (`!cl.done`) keeps every waiter honest, and this is the standard
price of sharing one condition variable across keys.

The result is stored on the heap-allocated `*call`, not looked up again in the
map, so a waiter that woke after the map entry was already deleted still reads the
right value through its captured pointer. Successful values are promoted into a
`values` map (a real cache hit next time); errors are NOT cached — the in-flight
`call` is removed, so the next `Get` starts a fresh load. That gives the two
behaviors the tests pin: current waiters all share the one error, and a subsequent
call retries.

Dropping the lock across the loader call is essential. If the loader ran while
holding the mutex, every other `Get` — even for different keys — would serialize
behind it, defeating the point. The lock protects only the bookkeeping (the maps
and the `call` state); the slow work happens unlocked.

Create `cache.go`:

```go
package sfcache

import "sync"

// call is one in-flight (or just-completed) load for a single key.
type call[V any] struct {
	done bool
	val  V
	err  error
}

// Cache is a read-through cache that collapses concurrent loads of the same key
// into a single loader call. Successful results are memoized; errors are not.
type Cache[K comparable, V any] struct {
	mu     sync.Mutex
	cond   *sync.Cond
	loader func(K) (V, error)
	values map[K]V
	calls  map[K]*call[V]
}

// New returns a Cache that loads missing keys with loader.
func New[K comparable, V any](loader func(K) (V, error)) *Cache[K, V] {
	c := &Cache[K, V]{
		loader: loader,
		values: make(map[K]V),
		calls:  make(map[K]*call[V]),
	}
	c.cond = sync.NewCond(&c.mu)
	return c
}

// Get returns the cached value for key, loading it on a miss. Concurrent Gets
// for the same key share a single loader invocation.
func (c *Cache[K, V]) Get(key K) (V, error) {
	c.mu.Lock()

	if v, ok := c.values[key]; ok {
		c.mu.Unlock()
		return v, nil
	}
	if cl, ok := c.calls[key]; ok {
		for !cl.done {
			c.cond.Wait()
		}
		v, err := cl.val, cl.err
		c.mu.Unlock()
		return v, err
	}

	// We are the loader for this key.
	cl := &call[V]{}
	c.calls[key] = cl
	c.mu.Unlock()

	v, err := c.loader(key)

	c.mu.Lock()
	cl.val, cl.err, cl.done = v, err, true
	delete(c.calls, key)
	if err == nil {
		c.values[key] = v
	}
	c.cond.Broadcast()
	c.mu.Unlock()
	return v, err
}
```

### The runnable demo

The demo fires many concurrent `Get` calls for one cold key against a loader that
counts its own invocations and sleeps to simulate a slow database. Despite the
concurrency, the loader runs once.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"example.com/sfcache"
)

func main() {
	var loads atomic.Int64
	c := sfcache.New(func(key string) (string, error) {
		loads.Add(1)
		time.Sleep(10 * time.Millisecond) // slow backend
		return strings.ToUpper(key), nil
	})

	const callers = 50
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = c.Get("user:42")
		}()
	}
	wg.Wait()

	fmt.Printf("%d concurrent Gets caused %d load(s)\n", callers, loads.Load())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
50 concurrent Gets caused 1 load(s)
```

### Tests

`TestSingleFlightFanIn` makes the collapse deterministic: the loader blocks on a
channel the test controls, so `synctest.Wait()` can confirm one goroutine is in
the loader and the rest are parked on the `Cond` before the loader is released;
then it asserts the loader ran exactly once and all callers got the same value.
`TestErrorFanOutThenRetry` asserts a loader error reaches every current waiter and
that a later `Get` triggers a fresh load. `TestConcurrentDistinctKeys` stresses
distinct keys under `-race`.

Create `cache_test.go`:

```go
package sfcache

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
)

func TestSingleFlightFanIn(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var loads atomic.Int64
		release := make(chan struct{})
		c := New(func(key string) (int, error) {
			loads.Add(1)
			<-release // hold the load open until the test releases it
			return 7, nil
		})

		const callers = 20
		results := make(chan int, callers)
		for range callers {
			go func() {
				v, err := c.Get("k")
				if err != nil {
					t.Errorf("Get error: %v", err)
				}
				results <- v
			}()
		}

		synctest.Wait() // one goroutine in loader, the rest parked on Cond
		if got := loads.Load(); got != 1 {
			t.Fatalf("loads = %d while in flight, want 1", got)
		}

		close(release)
		synctest.Wait() // all callers woken and returned

		for range callers {
			if v := <-results; v != 7 {
				t.Fatalf("caller got %d, want 7", v)
			}
		}
		if got := loads.Load(); got != 1 {
			t.Fatalf("total loads = %d, want 1", got)
		}
	})
}

func TestErrorFanOutThenRetry(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		wantErr := errors.New("backend down")
		var loads atomic.Int64
		release := make(chan struct{})
		c := New(func(key string) (int, error) {
			n := loads.Add(1)
			if n == 1 {
				<-release
				return 0, wantErr // first load fails
			}
			return 42, nil // retry succeeds
		})

		const callers = 5
		errs := make(chan error, callers)
		for range callers {
			go func() {
				_, err := c.Get("k")
				errs <- err
			}()
		}

		synctest.Wait()
		close(release)
		synctest.Wait()

		for range callers {
			if err := <-errs; !errors.Is(err, wantErr) {
				t.Fatalf("waiter error = %v, want %v", err, wantErr)
			}
		}

		// A later Get retries because the error was not cached.
		v, err := c.Get("k")
		if err != nil {
			t.Fatalf("retry error = %v, want nil", err)
		}
		if v != 42 {
			t.Fatalf("retry value = %d, want 42", v)
		}
		if got := loads.Load(); got != 2 {
			t.Fatalf("total loads = %d, want 2 (one failed, one retried)", got)
		}
	})
}

func TestConcurrentDistinctKeys(t *testing.T) {
	t.Parallel()

	var loads atomic.Int64
	c := New(func(key int) (int, error) {
		loads.Add(1)
		return key * key, nil
	})

	var wg sync.WaitGroup
	for k := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := c.Get(k)
			if err != nil || v != k*k {
				t.Errorf("Get(%d) = %d,%v; want %d,nil", k, v, err, k*k)
			}
		}()
	}
	wg.Wait()

	if got := loads.Load(); got != 50 {
		t.Fatalf("loads = %d, want 50 (one per distinct key)", got)
	}
}

func Example() {
	c := New(func(key string) (int, error) {
		return len(key), nil
	})
	v, _ := c.Get("hello")
	fmt.Println(v)
	// Output: 5
}
```

## Review

The cache is correct when concurrent misses on one key produce exactly one loader
call and one shared result. `TestSingleFlightFanIn` proves it by freezing the load
open and asserting `loads == 1` while twenty callers are parked; the deterministic
`synctest.Wait()` removes any doubt about whether the waiters actually reached the
`Cond`. The error contract is the other half: because errors are not memoized,
current waiters share the one failure and the next call retries — pinned by
`TestErrorFanOutThenRetry`, which asserts exactly two total loads.

The mistakes here are structural. Holding the mutex across the loader serializes
every key and destroys the concurrency the cache exists to provide — drop the lock
before the slow call. Caching an error would turn a transient backend blip into a
permanent poisoned key. And storing the result only in the map (not on the
`*call`) would break a waiter that woke after the map entry was deleted — the
captured `*call` pointer is what makes the hand-off safe. Run `go test -race` to
confirm the maps and the `call` fields are only touched under the lock.

## Resources

- [`golang.org/x/sync/singleflight`](https://pkg.go.dev/golang.org/x/sync/singleflight) — the production single-flight primitive this module reconstructs.
- [`sync.Cond`](https://pkg.go.dev/sync#Cond) — `Broadcast` and the per-waiter for-loop predicate.
- [`testing/synctest`](https://pkg.go.dev/testing/synctest) — freezing an in-flight load to assert the fan-in deterministically.

---

Back to [03-connection-pool.md](03-connection-pool.md) | Next: [05-drain-barrier.md](05-drain-barrier.md)
