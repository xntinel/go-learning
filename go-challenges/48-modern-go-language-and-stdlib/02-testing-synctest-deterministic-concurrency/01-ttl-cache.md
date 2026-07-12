# Exercise 1: The Generic TTL Cache

A cache whose entries expire after a TTL is the simplest piece of time-dependent
code there is, and the hardest to test the obvious way. This exercise builds that
cache the production way — calling `time.Now()` directly, with no injected clock —
and then tests its expiry under a `synctest` bubble, so a two-second TTL is
asserted in microseconds.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
ttlcache/                  independent module: example.com/ttlcache
  go.mod                   go 1.25 (synctest needs it)
  ttlcache.go              type Cache[K,V]; New, Set, Get, Len (uses time.Now directly)
  cmd/
    demo/
      main.go              runnable demo: set, read, sleep past TTL, read again
  ttlcache_test.go         synctest virtual-time expiry tests, -race concurrency test
```

- Files: `ttlcache.go`, `cmd/demo/main.go`, `ttlcache_test.go`.
- Implement: a generic `Cache[K comparable, V any]` with `New`, `Set(key, value, ttl)`, `Get(key) (V, bool)`, and `Len()`, reading the clock through `time.Now()` with no `Clock` abstraction.
- Test: a `synctest` bubble that sets an entry, sleeps past its TTL in virtual time, and asserts it is gone; a near-boundary test; a `-race` concurrency test.
- Verify: `go test -count=1 -race ./...`

Set up the module. `testing/synctest` requires Go 1.25+, so pin the language
version:

```bash
mkdir -p go-solutions/48-modern-go-language-and-stdlib/02-testing-synctest-deterministic-concurrency/01-ttl-cache/cmd/demo
cd go-solutions/48-modern-go-language-and-stdlib/02-testing-synctest-deterministic-concurrency/01-ttl-cache
go mod edit -go=1.25
```

### Why there is no Clock interface

The artifact is a plain concurrency-safe cache whose entries expire after a TTL —
the kind of code that is normally painful to test because expiry depends on the
clock. The crucial design choice here is what is *absent*: there is no `Clock`
interface, no injected `now func() time.Time`. The cache calls `time.Now()`
directly, exactly as production code would.

That is deliberate. The clock-injection pattern exists almost entirely so that a
test can substitute a fake clock and assert expiry without sleeping real seconds.
A `synctest` bubble virtualizes the `time` package *underneath* the code, which
removes the only reason that abstraction had to exist. You build the cache the
obvious way — `time.Now().Add(ttl)` on `Set`, `time.Now().Before(e.expires)` on
`Get` — and then test its TTL expiry under virtual time in the same module. A
two-second expiry is asserted in microseconds, with no clock to inject.

Expiry is *lazy*: `Get` treats an entry as absent once `time.Now()` is no longer
before its `expires` instant, but it does not delete it. `Len` therefore counts
stored entries whether or not they have expired — freeing the memory of an
expired-but-never-read entry is the job of the background janitor in the next
exercise. Keeping expiry lazy here keeps the type small and the synctest lesson
focused on a single goroutine and the fake clock.

Create `ttlcache.go`. Note it calls `time.Now()` directly — no injected clock:

```go
package ttlcache

import (
	"sync"
	"time"
)

type entry[V any] struct {
	value   V
	expires time.Time
}

// Cache is a concurrency-safe map whose entries expire after a TTL. It reads
// the wall clock through time.Now, which a synctest bubble can virtualize.
type Cache[K comparable, V any] struct {
	mu    sync.Mutex
	items map[K]entry[V]
}

func New[K comparable, V any]() *Cache[K, V] {
	return &Cache[K, V]{items: make(map[K]entry[V])}
}

// Set stores value under key, expiring it ttl from now.
func (c *Cache[K, V]) Set(key K, value V, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[key] = entry[V]{value: value, expires: time.Now().Add(ttl)}
}

// Get returns the value if present and unexpired. An expired entry reports
// (zero, false) and is treated as absent.
func (c *Cache[K, V]) Get(key K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[key]
	if !ok || !time.Now().Before(e.expires) {
		var zero V
		return zero, false
	}
	return e.value, true
}

// Len reports the number of entries still stored, expired or not.
func (c *Cache[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}
```

### The runnable demo

The demo uses a real, short sleep (not a bubble) so you can watch an actual
eviction happen against the wall clock: it stores a session for 50 ms, reads it,
sleeps 100 ms, and reads again to see it gone.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/ttlcache"
)

func main() {
	c := ttlcache.New[string, string]()
	c.Set("session", "alice", 50*time.Millisecond)

	if v, ok := c.Get("session"); ok {
		fmt.Printf("before expiry: %s\n", v)
	}

	time.Sleep(100 * time.Millisecond)

	if _, ok := c.Get("session"); !ok {
		fmt.Println("after expiry: evicted")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
before expiry: alice
after expiry: evicted
```

### Tests

The tests are where the bubble earns its keep. `TestLazyExpiry` sets an entry
with a one-second TTL, sleeps two virtual seconds (which returns immediately), and
asserts the entry is gone — a real two-second expiry checked in microseconds.
`TestSurvivesUntilExpiry` sleeps to one millisecond *before* the deadline and
asserts the entry is still live, pinning the boundary exactly; that precision is
only possible because virtual time has no scheduler slack. `TestConcurrentSetGet`
runs outside a bubble and exists to exercise the mutex under `-race`. Note the
outer test calls `t.Parallel()`, but the function passed to `synctest.Test` must
not (and may not) call it.

Create `ttlcache_test.go`:

```go
package ttlcache

import (
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func TestLazyExpiry(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		c := New[string, int]()
		c.Set("a", 1, time.Second)

		if v, ok := c.Get("a"); !ok || v != 1 {
			t.Fatalf("Get(a) = %d,%v right after Set; want 1,true", v, ok)
		}

		time.Sleep(2 * time.Second) // virtual: returns immediately

		if _, ok := c.Get("a"); ok {
			t.Fatal("Get(a) still present after TTL expired")
		}
	})
}

func TestSurvivesUntilExpiry(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		c := New[string, int]()
		c.Set("a", 1, time.Second)

		time.Sleep(time.Second - time.Millisecond) // just before expiry

		if _, ok := c.Get("a"); !ok {
			t.Fatal("entry expired one millisecond early")
		}
	})
}

func TestConcurrentSetGet(t *testing.T) {
	t.Parallel()
	c := New[int, int]()
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Set(i, i, time.Minute)
			c.Get(i)
		}()
	}
	wg.Wait()
}

func Example() {
	c := New[string, int]()
	c.Set("answer", 42, time.Minute)

	v, ok := c.Get("answer")
	fmt.Println(v, ok)
	// Output: 42 true
}
```

## Review

The cache is correct when expiry is a pure function of `time.Now()` and the entry's
stored deadline: `Get` returns `(zero, false)` exactly when the key is missing or
`time.Now()` is not before `expires`, and nothing else moves the boundary. The
synctest proof is that `TestLazyExpiry` advances two virtual seconds instantly and
sees the entry gone, while `TestSurvivesUntilExpiry` lands one virtual millisecond
early and sees it live; if either flakes, the cache is consulting some clock the
bubble is not virtualizing.

The mistakes to avoid here are structural. First, do not reach for a `Clock`
interface "to make it testable" — that is the abstraction synctest exists to
delete; calling `time.Now()` directly is the point. Second, keep the outer test's
`t.Parallel()` outside the bubble: calling `t.Parallel` (or `t.Run`) on the `T`
that `synctest.Test` hands you is forbidden and will fail the test. Third, remember
expiry is lazy: `Len` counting an expired entry is correct behavior, not a bug —
reclaiming that memory is the janitor's job in Exercise 2. Run `go test -race` to
confirm the mutex actually guards the map under concurrent `Set`/`Get`.

## Resources

- [`testing/synctest`](https://pkg.go.dev/testing/synctest) — the bubble, the fake clock, and `synctest.Test`/`synctest.Wait`.
- [Testing concurrent code with testing/synctest](https://go.dev/blog/synctest) — the Go blog's introduction, including why no `Clock` interface is needed.
- [`time` package](https://pkg.go.dev/time) — `time.Now`, `time.Time.Add`, and `time.Time.Before`, all virtualized inside a bubble.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-background-janitor.md](02-background-janitor.md)
