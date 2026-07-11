# Exercise 24: Read-Write Lock Upgrade — Demote on Early Return

**Nivel: Intermedio** — validacion rapida (un test corto).

`sync.RWMutex` has no atomic "upgrade my read lock to a write lock"
operation — a reader who discovers it needs to write has to give up the read
lock and take the write lock as two separate steps, with a window in
between where anything can happen. This module builds the standard
lazily-populated-cache shape around that gap: a double-checked lookup after
the upgrade, and — the `defer`-stacking trick — an explicit demote back to
read level before returning, so the single `defer RUnlock()` registered at
the top is correct no matter which path the function took.

## What you'll build

```text
rwupgrade/                   independent module: example.com/rwupgrade
  go.mod
  rwupgrade/rwupgrade.go       Cache (RWMutex-guarded); GetOrCreate (RLock -> upgrade -> demote)
  rwupgrade/rwupgrade_test.go  existing key (no upgrade); missing key (upgrade + insert); 32-worker concurrent create-once
  cmd/demo/main.go             runnable demo: first call creates, second call reuses
```

- Files: `rwupgrade/rwupgrade.go`, `rwupgrade/rwupgrade_test.go`, `cmd/demo/main.go`.
- Implement: a `Cache` wrapping a `sync.RWMutex`-guarded map, with `GetOrCreate(key string, create func() string) (val string, created bool)` that `RLock`s and defers `RUnlock`, returns immediately if the key exists, otherwise `RUnlock`s, `Lock`s, defers a closure that `Unlock`s and re-`RLock`s (the demote), re-checks the key under the write lock, and creates it if it is still missing.
- Test: a key that already exists (`create` never called); a missing key (upgraded, created, and a subsequent unrelated call still succeeds — proving the cache is left fully unlocked); 32 goroutines requesting the same missing key concurrently, asserting `create` ran exactly once.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/rwupgrade/rwupgrade ~/go-exercises/rwupgrade/cmd/demo
cd ~/go-exercises/rwupgrade
go mod init example.com/rwupgrade
go mod edit -go=1.24
```

### Two defers, stacked, do the demotion for you

Trace the lock calls in acquisition order for the "key is missing" path:
`RLock` (with `defer RUnlock` registered immediately after), then — once the
key turns out to be missing — an explicit `RUnlock`, then `Lock`, then a
*second* `defer` that runs `Unlock(); RLock()`. Two defers are now stacked;
Go runs them LIFO, so the second one (the demote) fires first: it releases
the write lock and re-acquires a read lock. Only after that does the first
deferred call run — `RUnlock()` — which now has a real read lock to release,
because the demote closure just put one back. The function's contract,
"`GetOrCreate` always returns holding no lock, released via exactly one
`RUnlock` call," holds on every exit path this way — including a panic
inside `create()`, since defers run on panic too — without the top-level
defer ever needing to know whether an upgrade happened.

### The double-check is not optional

Between the explicit `RUnlock` and the following `Lock`, this goroutine
holds *no* lock at all — any other goroutine can run, including another
`GetOrCreate` call for the same key. That is exactly what the 32-worker test
exercises: many goroutines can all observe the key missing under their own
`RLock`, all release it, and all race for the write lock. Whichever one gets
it first re-checks the map — still under the write lock — creates the entry,
and every other goroutine's own re-check, once it gets its turn at the write
lock, finds the entry already there and returns it instead of creating a
second one. Skip the re-check and assume "I saw it missing, so it's still
missing" and every one of those goroutines calls `create()` and overwrites
the others' work.

Create `rwupgrade/rwupgrade.go`:

```go
package rwupgrade

import "sync"

// Cache is a read-mostly, lazily-populated map. GetOrCreate is the
// interesting method: sync.RWMutex has no atomic "upgrade my read lock to a
// write lock" operation, so a caller that finds a key missing has to give up
// the read lock and take the write lock instead -- and then, having
// mutated under the write lock, demote back to holding a read lock before
// returning, so the reader-lock invariant callers can rely on is restored.
type Cache struct {
	mu   sync.RWMutex
	data map[string]string
}

// NewCache returns an empty Cache.
func NewCache() *Cache {
	return &Cache{data: make(map[string]string)}
}

// GetOrCreate returns the value for key, creating it with create if it does
// not already exist. created reports whether this call performed the
// creation.
func (c *Cache) GetOrCreate(key string, create func() string) (val string, created bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if v, ok := c.data[key]; ok {
		return v, false
	}

	// Upgrade: release the read lock and take the write lock. There is no
	// atomic way to do this with sync.RWMutex, so between RUnlock and Lock
	// another goroutine can run -- which is exactly why the lookup below is
	// repeated after acquiring the write lock.
	c.mu.RUnlock()
	c.mu.Lock()

	// Demote back to holding a read lock before this function returns, on
	// every path -- including a panic inside create(). Because Go's defers
	// run LIFO, this demote runs BEFORE the RUnlock deferred above, so by
	// the time that RUnlock executes, a read lock has already been
	// re-acquired for it to release. The function's lock-holding invariant
	// -- "GetOrCreate always returns holding no lock, via one RUnlock" --
	// therefore holds on every exit path without the outer defer needing to
	// know which level the lock was actually left at.
	defer func() {
		c.mu.Unlock()
		c.mu.RLock()
	}()

	// Re-check: another goroutine may have inserted key while this call held
	// no lock at all, between the RUnlock above and this Lock.
	if v, ok := c.data[key]; ok {
		return v, false
	}

	v := create()
	c.data[key] = v
	return v, true
}

// Len reports how many entries are currently cached.
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

	"example.com/rwupgrade/rwupgrade"
)

func main() {
	c := rwupgrade.NewCache()

	v, created := c.GetOrCreate("config", func() string {
		fmt.Println("creating config (upgrade to write lock)")
		return "default-config"
	})
	fmt.Println("first call:", v, created)

	v, created = c.GetOrCreate("config", func() string {
		fmt.Println("this must not print")
		return "should-not-happen"
	})
	fmt.Println("second call:", v, created)

	fmt.Println("cache size:", c.Len())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
creating config (upgrade to write lock)
first call: default-config true
second call: default-config false
cache size: 1
```

### Tests

Create `rwupgrade/rwupgrade_test.go`:

```go
package rwupgrade

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestGetOrCreateReturnsExistingWithoutCreating(t *testing.T) {
	t.Parallel()

	c := NewCache()
	_, _ = c.GetOrCreate("a", func() string { return "first" })

	calls := 0
	v, created := c.GetOrCreate("a", func() string {
		calls++
		return "second"
	})

	if created {
		t.Fatal("created = true, want false: key already existed")
	}
	if v != "first" {
		t.Fatalf("v = %q, want %q", v, "first")
	}
	if calls != 0 {
		t.Fatalf("create was called %d times, want 0", calls)
	}
}

func TestGetOrCreateInsertsMissingKeyViaUpgrade(t *testing.T) {
	t.Parallel()

	c := NewCache()
	v, created := c.GetOrCreate("b", func() string { return "made" })

	if !created {
		t.Fatal("created = false, want true: key was missing")
	}
	if v != "made" {
		t.Fatalf("v = %q, want %q", v, "made")
	}
	if got := c.Len(); got != 1 {
		t.Fatalf("Len() = %d, want 1", got)
	}

	// The cache must be left fully unlocked -- a second, unrelated call must
	// not block or deadlock.
	if _, created := c.GetOrCreate("c", func() string { return "also-made" }); !created {
		t.Fatal("expected c to be created")
	}
}

func TestGetOrCreateConcurrentRequestsForSameKeyCreateOnlyOnce(t *testing.T) {
	t.Parallel()

	c := NewCache()
	const workers = 32
	var createCount int64
	var wg sync.WaitGroup

	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.GetOrCreate("shared", func() string {
				atomic.AddInt64(&createCount, 1)
				return "value"
			})
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt64(&createCount); got != 1 {
		t.Fatalf("create was called %d times, want exactly 1", got)
	}
	if got := c.Len(); got != 1 {
		t.Fatalf("Len() = %d, want 1", got)
	}
}
```

Verify:

```bash
go test -count=1 -race ./...
```

## Review

The invariant this module protects is narrow but easy to break: every
return from `GetOrCreate` must leave the mutex completely unlocked, whether
the fast path (key already present, still holding the original read lock)
or the slow path (upgrade, demote, then the same original `RUnlock`) was
taken. `TestGetOrCreateInsertsMissingKeyViaUpgrade`'s trailing
unrelated-key call exists specifically to prove that: if the demote defer
were missing or wrong, the cache would still be holding the write lock (or a
mismatched lock count) when that second call tries to `RLock`, and the test
would hang instead of failing cleanly — which is why this test, unlike most
in this chapter, is also a deadlock detector by construction. Run with
`-race`: the 32-worker test is what actually exercises the gap between
`RUnlock` and `Lock` where the double-check matters, and the race detector
will flag it immediately if the re-check after `Lock()` is ever removed.

## Resources

- [sync.RWMutex](https://pkg.go.dev/sync#RWMutex) — note explicitly that it has no upgrade operation.
- [Go Language Specification: Defer statements](https://go.dev/ref/spec#Defer_statements)
- [Go Memory Model](https://go.dev/ref/mem) — why the re-check after acquiring the write lock is required, not defensive paranoia.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [23-circuit-breaker-state-unwinding.md](23-circuit-breaker-state-unwinding.md) | Next: [25-nested-transaction-chain-rollback.md](25-nested-transaction-chain-rollback.md)
