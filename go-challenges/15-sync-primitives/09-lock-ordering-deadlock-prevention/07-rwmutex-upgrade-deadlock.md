# Exercise 7: The Read-to-Write Upgrade Trap in a Config Cache

The most common self-deadlock in Go services is not two mutexes — it is one
`sync.RWMutex` and a cache-miss path that tries to "upgrade" a read lock into
a write lock. Go's RWMutex has no upgrade operation, so the writer waits for
all readers, including the goroutine asking. This exercise builds a
feature-flag cache with the correct release-then-recheck fill.

This module is fully self-contained. It begins with its own `go mod init`,
defines every type it needs, and ships its own demo and tests. Nothing here
imports any other exercise.

## What you'll build

```text
flagcache/                 independent module: example.com/flagcache
  go.mod
  flagcache.go             type Cache; New, GetOrLoad (double-checked fill),
                           Invalidate; ErrLoad
  cmd/
    demo/
      main.go              runnable demo: cache hits vs loads, error path
  flagcache_test.go        loads-exactly-once under 100 concurrent callers,
                           error propagation with errors.Is, retry-after-error
```

- Files: `flagcache.go`, `cmd/demo/main.go`, `flagcache_test.go`.
- Implement: `GetOrLoad(key)` — `RLock` check, `RUnlock`, `Lock`, *re-check*, load, store — plus `Invalidate` and a loader-error contract using a wrapped `ErrLoad` sentinel.
- Test: an atomic load counter proving exactly one load for 100 concurrent callers of the same key; independent loading per key; error wrapping asserted with `errors.Is` and no negative caching.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/15-sync-primitives/09-lock-ordering-deadlock-prevention/07-rwmutex-upgrade-deadlock/cmd/demo
cd go-solutions/15-sync-primitives/09-lock-ordering-deadlock-prevention/07-rwmutex-upgrade-deadlock
```

### Why there is no upgrade, and what to do instead

The intuitive miss path reads beautifully and deadlocks unconditionally:

```
// BROKEN: self-deadlock. Do not write this.
func (c *Cache) GetOrLoad(key string) (string, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if v, ok := c.flags[key]; ok {
		return v, nil
	}
	c.mu.Lock() // waits for all readers to leave -- including US
	// never reached
}
```

`Lock` on an RWMutex waits until every reader has released. This goroutine
*is* a reader, and its `RUnlock` is deferred until return, which cannot happen
until `Lock` acquires. One goroutine, one lock, permanent wedge — and because
it only triggers on a cache *miss*, it passes any test whose fixtures pre-fill
the cache. (Exercise 9's watchdog harness is how you demonstrate a hang like
this in a test suite without hanging the suite.)

Why does Go not provide an upgrade operation? Because upgradeable read locks
deadlock *by pairs*: if two readers both request an upgrade, each waits for
the other reader to leave, and neither can. Any sound upgrade API must be
allowed to fail or to release-then-reacquire internally — at which point it
is the release-then-recheck pattern with extra machinery, so the stdlib gives
you the pattern instead of the trap.

The correct fill is double-checked locking:

1. `RLock`, check, `RUnlock` — the fast path stays shared and cheap; a
   thousand concurrent readers of hot flags never serialize.
2. On a miss: `Lock` (with *nothing* held), then **check again**. Between
   your `RUnlock` and your `Lock`, another goroutine may have taken the write
   lock and filled the key. Skipping the re-check does not deadlock — it
   double-loads, which for an expensive or non-idempotent loader is its own
   production incident.
3. Still missing: call the loader, store, return.

There is a second, less famous deadlock documented on `sync.RWMutex` itself,
and it justifies a hard rule: *no recursive read locking*. A blocked `Lock`
call stops later `RLock` calls from acquiring (that is the anti-starvation
guarantee for writers). So goroutine G holds `RLock`, a writer arrives and
blocks, and G — deep in some helper that also takes `RLock` on the same lock —
blocks behind the writer: a three-party cycle, G waiting on the writer waiting
on G. The practical consequence: helpers called under a lock must never
re-acquire that lock, which is also why `GetOrLoad` computes everything it
needs and releases before calling anything else.

One honest trade-off is visible in this design: the loader runs *while
holding the write lock*, which is what makes "loads exactly once" true — every
other caller for any key waits until the fill finishes. For a config cache
(small values, one upstream fetch, tens of keys) that serialization at startup
is exactly what you want: one fetch instead of a hundred identical ones (a
thundering herd). For a large cache with slow, per-key loads, blocking *all*
keys behind one load is not acceptable — the tools there are a per-key
`sync.Once` held in the map, or `golang.org/x/sync/singleflight`, which
deduplicate in-flight loads per key without a global write lock. Knowing which
regime you are in is the design skill; this module implements the config-cache
regime and names the boundary.

Create `flagcache.go`:

```go
// Package flagcache caches feature-flag values fetched from a slow
// upstream (a flag service, a database). Reads are shared and lock-free
// of each other; misses fill under the write lock using the
// release-then-recheck (double-checked) pattern, because sync.RWMutex
// has no read-to-write upgrade.
package flagcache

import (
	"errors"
	"fmt"
	"sync"
)

// ErrLoad wraps every loader failure so callers can branch on the
// category with errors.Is while the message keeps the key and cause.
var ErrLoad = errors.New("flagcache: load failed")

// Cache is a concurrency-safe read-mostly flag cache.
type Cache struct {
	mu     sync.RWMutex
	flags  map[string]string
	loader func(key string) (string, error)
}

// New returns a Cache that fills misses by calling loader. The loader
// runs while the cache's write lock is held: it is called at most once
// per missing key, and other callers wait for it. Keep loaders bounded
// (add their own timeout) — a hung loader stalls the whole cache.
func New(loader func(key string) (string, error)) *Cache {
	return &Cache{flags: make(map[string]string), loader: loader}
}

// GetOrLoad returns the cached value for key, loading and caching it on
// first use. Failed loads are not cached: the next call retries.
func (c *Cache) GetOrLoad(key string) (string, error) {
	// Fast path: shared lock, released BEFORE any write-lock attempt.
	// Upgrading in place would self-deadlock: Lock waits for all
	// readers, and we would be one of them.
	c.mu.RLock()
	v, ok := c.flags[key]
	c.mu.RUnlock()
	if ok {
		return v, nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	// Re-check: another goroutine may have filled the key between our
	// RUnlock and our Lock. Without this, concurrent misses double-load.
	if v, ok := c.flags[key]; ok {
		return v, nil
	}
	v, err := c.loader(key)
	if err != nil {
		return "", fmt.Errorf("%w: key %q: %w", ErrLoad, key, err)
	}
	c.flags[key] = v
	return v, nil
}

// Invalidate drops key from the cache; the next GetOrLoad reloads it.
// Used when the upstream pushes a flag-change event.
func (c *Cache) Invalidate(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.flags, key)
}
```

### The runnable demo

The demo's loader counts its calls, making the cache's whole value visible in
four lines: two reads of the same flag cost one load, a second flag costs a
second load, and an unknown flag returns the wrapped error without being
cached.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/flagcache"
)

func main() {
	upstream := map[string]string{
		"beta-banner": "on",
		"dark-mode":   "off",
	}
	loads := 0
	cache := flagcache.New(func(key string) (string, error) {
		loads++
		v, ok := upstream[key]
		if !ok {
			return "", fmt.Errorf("no flag %q in upstream", key)
		}
		return v, nil
	})

	for _, key := range []string{"beta-banner", "beta-banner", "dark-mode"} {
		v, err := cache.GetOrLoad(key)
		if err != nil {
			fmt.Println("err:", err)
			continue
		}
		fmt.Printf("%s=%s (loads so far: %d)\n", key, v, loads)
	}

	if _, err := cache.GetOrLoad("unknown"); err != nil {
		fmt.Println("err:", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
beta-banner=on (loads so far: 1)
beta-banner=on (loads so far: 1)
dark-mode=off (loads so far: 2)
err: flagcache: load failed: key "unknown": no flag "unknown" in upstream
```

### Tests

`TestLoadsExactlyOnce` is the contract test for the re-check: 100 goroutines
request the same cold key through a start-gate channel (so they pile onto the
miss path together), and an `atomic.Int32` in the loader must read exactly 1
afterward. Delete the re-check inside the write lock and this test fails with
a count above 1 — which is precisely the bug the naive "I already checked
under RLock" reasoning produces. The error tests pin three properties at once
with `errors.Is`: the sentinel `ErrLoad` matches, the *underlying* cause also
matches (two `%w` verbs, Go 1.20+), and a failed load is not negatively
cached — the next call must retry the loader.

Create `flagcache_test.go`:

```go
package flagcache

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

func TestGetOrLoadCachesValue(t *testing.T) {
	t.Parallel()

	var loads atomic.Int32
	c := New(func(key string) (string, error) {
		loads.Add(1)
		return "value-of-" + key, nil
	})

	for range 3 {
		v, err := c.GetOrLoad("k")
		if err != nil {
			t.Fatal(err)
		}
		if v != "value-of-k" {
			t.Fatalf("GetOrLoad = %q, want value-of-k", v)
		}
	}
	if got := loads.Load(); got != 1 {
		t.Fatalf("loader ran %d times for one key, want 1", got)
	}
}

func TestLoadsExactlyOnce(t *testing.T) {
	t.Parallel()

	var loads atomic.Int32
	c := New(func(key string) (string, error) {
		loads.Add(1)
		return "on", nil
	})

	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // pile every goroutine onto the cold-miss path at once
			v, err := c.GetOrLoad("beta-banner")
			if err != nil || v != "on" {
				t.Errorf("GetOrLoad = %q, %v", v, err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := loads.Load(); got != 1 {
		t.Fatalf("loader ran %d times under concurrency, want exactly 1 (missing re-check?)", got)
	}
}

func TestDistinctKeysLoadIndependently(t *testing.T) {
	t.Parallel()

	var loads atomic.Int32
	c := New(func(key string) (string, error) {
		loads.Add(1)
		return key + "-v", nil
	})

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key := fmt.Sprintf("flag-%d", i)
			for range 10 {
				if _, err := c.GetOrLoad(key); err != nil {
					t.Errorf("GetOrLoad(%q): %v", key, err)
					return
				}
			}
		}()
	}
	wg.Wait()

	if got := loads.Load(); got != 8 {
		t.Fatalf("loader ran %d times for 8 keys, want 8", got)
	}
}

func TestLoadErrorWrappedAndNotCached(t *testing.T) {
	t.Parallel()

	errSourceDown := errors.New("flag service unavailable")
	var loads atomic.Int32
	c := New(func(key string) (string, error) {
		if loads.Add(1) == 1 {
			return "", errSourceDown
		}
		return "recovered", nil
	})

	_, err := c.GetOrLoad("k")
	if !errors.Is(err, ErrLoad) {
		t.Fatalf("err = %v, want ErrLoad", err)
	}
	if !errors.Is(err, errSourceDown) {
		t.Fatalf("err = %v, should wrap the loader's cause", err)
	}

	// No negative caching: the retry must reach the loader again.
	v, err := c.GetOrLoad("k")
	if err != nil || v != "recovered" {
		t.Fatalf("retry = %q, %v; want recovered, nil", v, err)
	}
	if got := loads.Load(); got != 2 {
		t.Fatalf("loader ran %d times, want 2", got)
	}
}

func TestInvalidateForcesReload(t *testing.T) {
	t.Parallel()

	var loads atomic.Int32
	c := New(func(key string) (string, error) {
		return fmt.Sprintf("v%d", loads.Add(1)), nil
	})

	v1, _ := c.GetOrLoad("k")
	c.Invalidate("k")
	v2, _ := c.GetOrLoad("k")
	if v1 != "v1" || v2 != "v2" {
		t.Fatalf("got %q then %q, want v1 then v2", v1, v2)
	}
}

func ExampleCache_GetOrLoad() {
	c := New(func(key string) (string, error) {
		return "on", nil
	})
	v, _ := c.GetOrLoad("beta-banner")
	fmt.Println(v)
	// Output: on
}
```

## Review

The pattern to burn in: with an RWMutex, *check shared, fill exclusive, and
between the two hold nothing*. Both deviations are bugs with very different
signatures. Holding `RLock` into `Lock` deadlocks one goroutine forever — and
only on the miss path, so warm-cache tests never see it. Skipping the
re-check after `Lock` never deadlocks but multiplies loads under concurrent
misses; `TestLoadsExactlyOnce` exists to catch exactly that regression, and it
must gate any refactor of this function. Keep the companion rule in view too:
no recursive `RLock` on the same lock anywhere below a function that holds it,
because a blocked writer turns the second `RLock` into a deadlock — the
RWMutex documentation states this prohibition explicitly.

Also know this design's boundary. Loader-under-write-lock deliberately
serializes all misses; that is correct for a small config cache and wrong for
a large cache with slow per-key loads, where the fix is per-key deduplication
(`singleflight` or a `sync.Once` per entry) rather than a cleverer lock dance.
Confirm with `go test -count=1 -race ./...`.

## Resources

- [sync package — RWMutex](https://pkg.go.dev/sync#RWMutex) — the no-recursive-read-lock rule and writer-starvation guarantee behind it.
- [singleflight package](https://pkg.go.dev/golang.org/x/sync/singleflight) — per-key load deduplication for when loader-under-lock does not scale.
- [errors package — Is and wrapping](https://pkg.go.dev/errors) — the multi-`%w` wrapping asserted in the error tests.

---

Back to [00-concepts.md](00-concepts.md) | Next: [08-mixed-resource-hierarchy-pool.md](08-mixed-resource-hierarchy-pool.md)
