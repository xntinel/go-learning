# Exercise 29: LRU Cache Eviction Policy Selection via Callbacks

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye concurrencia).

A fixed-capacity cache needs exactly one decision made for it when it is
full: which key to throw out. Hard-coding that as "always evict the oldest"
locks every caller into LRU forever. This module pulls the decision out
into an `EvictFunc` callback that receives a snapshot of every key's
access metadata and returns the victim, so LRU, LFU, and one-off custom
policies are all just different functions plugged into the same `Cache`.

## What you'll build

```text
cache/                        independent module: example.com/lru-eviction-policy-selector
  go.mod                       go 1.24
  cache.go                     type AccessInfo, type EvictFunc, type Cache: Get, Put, Len, func LRU, func LFU
  cmd/
    demo/
      main.go                    runnable demo: an LRU cache and an LFU cache under a deterministic fake clock
  cache_test.go                  LRU eviction, LFU eviction, a custom policy, update-not-evict, concurrency (-race)
```

Files: `cache.go`, `cmd/demo/main.go`, `cache_test.go`.
Implement: `type AccessInfo struct { LastAccess int64; Frequency int }`, `type EvictFunc func(entries map[string]AccessInfo) string`, `Cache` with `New(capacity, evict, clock)`, `Get(key) (any, bool)`, `Put(key, value)`, `Len() int`, plus `LRU` and `LFU` as ready-made `EvictFunc` values; `Put` on a new key at capacity must snapshot every key's `AccessInfo` and call `evict` before inserting, and `clock` must be an injected `func() int64`, never `time.Now()` directly.
Test: `LRU` evicts the least recently used key; `LFU` evicts the least frequently used key; a custom policy (evict the lexicographically largest key) plugs in and is honored; updating an existing key at capacity never triggers eviction; concurrent `Get`/`Put` from multiple goroutines stay under capacity and race-free; the injected fake clock advances deterministically.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/06-function-types-and-callbacks/29-lru-eviction-policy-selector/cmd/demo
cd go-solutions/04-functions/06-function-types-and-callbacks/29-lru-eviction-policy-selector
go mod edit -go=1.24
```

### Why the policy sees a snapshot, not the live map

`EvictFunc` takes `map[string]AccessInfo`, a plain value type, not a
pointer into the cache's own `map[string]*entry`. That is deliberate:
handing a policy function a snapshot means it can only read, never mutate
the cache's real state or race with the next `Get`/`Put` that runs after
it returns — and it means `LRU` and `LFU` are pure functions you can unit
test in complete isolation from `Cache`, with no fake struct to construct.
`Put` builds that snapshot by copying every entry's `AccessInfo` under the
cache's own mutex, calls `evict` outside any assumption about what it
does internally (it's just a function, it could sort, scan, or call out
to a real LFU heap), and only then deletes the returned key and inserts
the new one — still under the same critical section, so a concurrent
`Get` can never observe the state between "victim chosen" and "victim
evicted". The `clock` being an injected `func() int64` rather than
`time.Now().UnixNano()` is what makes `TestLRUEvictsLeastRecentlyUsed`
deterministic: a real clock's nanosecond resolution can still produce
ties or non-monotonic reads under a fast test loop, while the fake clock
in the tests below hands out a strictly increasing tick on every call.

Create `cache.go`:

```go
// Package cache implements a fixed-capacity key/value cache whose
// eviction policy is a pluggable callback, so LRU, LFU, or any custom
// replacement algorithm plugs in without touching the cache itself.
package cache

import (
	"sync"
)

// AccessInfo is the read-only snapshot an EvictFunc sees for one key.
type AccessInfo struct {
	LastAccess int64
	Frequency  int
}

// EvictFunc chooses which key to evict from a full cache, given a
// snapshot of every key's access metadata. It must return a key present
// in entries.
type EvictFunc func(entries map[string]AccessInfo) string

type entry struct {
	value      any
	lastAccess int64
	frequency  int
}

// Cache is a fixed-capacity map guarded by a mutex, with eviction
// delegated to an EvictFunc and time delegated to an injected clock so
// tests are deterministic.
type Cache struct {
	mu       sync.Mutex
	capacity int
	data     map[string]*entry
	clock    func() int64
	evict    EvictFunc
}

// New returns a Cache holding at most capacity entries, using evict to
// pick a victim when a Put would exceed capacity, and clock to timestamp
// accesses (inject a fake clock in tests instead of time.Now().UnixNano()).
func New(capacity int, evict EvictFunc, clock func() int64) *Cache {
	if capacity < 1 {
		capacity = 1
	}
	return &Cache{
		capacity: capacity,
		data:     make(map[string]*entry, capacity),
		clock:    clock,
		evict:    evict,
	}
}

// Get returns the value for key and records the access, or reports false
// if key is absent.
func (c *Cache) Get(key string) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.data[key]
	if !ok {
		return nil, false
	}
	e.lastAccess = c.clock()
	e.frequency++
	return e.value, true
}

// Put inserts or updates key. If key is new and the cache is at capacity,
// it asks evict to choose a victim from a snapshot of every current key's
// AccessInfo, then evicts that key before inserting.
func (c *Cache) Put(key string, value any) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.clock()
	if e, ok := c.data[key]; ok {
		e.value = value
		e.lastAccess = now
		e.frequency++
		return
	}

	if len(c.data) >= c.capacity {
		snapshot := make(map[string]AccessInfo, len(c.data))
		for k, e := range c.data {
			snapshot[k] = AccessInfo{LastAccess: e.lastAccess, Frequency: e.frequency}
		}
		victim := c.evict(snapshot)
		delete(c.data, victim)
	}

	c.data[key] = &entry{value: value, lastAccess: now, frequency: 1}
}

// Len reports the number of keys currently cached.
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.data)
}

// LRU evicts the key with the smallest LastAccess (the least recently
// used one), breaking ties by the lexicographically smaller key so the
// choice is deterministic.
func LRU(entries map[string]AccessInfo) string {
	var victim string
	first := true
	for k, info := range entries {
		if first || info.LastAccess < entries[victim].LastAccess ||
			(info.LastAccess == entries[victim].LastAccess && k < victim) {
			victim = k
			first = false
		}
	}
	return victim
}

// LFU evicts the key with the smallest Frequency, breaking ties by the
// smaller LastAccess and then the lexicographically smaller key.
func LFU(entries map[string]AccessInfo) string {
	var victim string
	first := true
	for k, info := range entries {
		if first {
			victim, first = k, false
			continue
		}
		cur := entries[victim]
		switch {
		case info.Frequency < cur.Frequency:
			victim = k
		case info.Frequency == cur.Frequency && info.LastAccess < cur.LastAccess:
			victim = k
		case info.Frequency == cur.Frequency && info.LastAccess == cur.LastAccess && k < victim:
			victim = k
		}
	}
	return victim
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/lru-eviction-policy-selector"
)

func main() {
	var tick int64
	clock := func() int64 {
		tick++
		return tick
	}

	c := cache.New(2, cache.LRU, clock)
	c.Put("a", 1)
	c.Put("b", 2)
	c.Get("a")    // "a" is now more recently used than "b"
	c.Put("c", 3) // capacity reached: "b" is least recently used, evicted

	if _, ok := c.Get("b"); !ok {
		fmt.Println("b evicted:", true)
	}
	if v, ok := c.Get("a"); ok {
		fmt.Println("a still cached:", v)
	}
	if v, ok := c.Get("c"); ok {
		fmt.Println("c still cached:", v)
	}

	lfu := cache.New(2, cache.LFU, clock)
	lfu.Put("x", "x1")
	lfu.Put("y", "y1")
	lfu.Get("x")
	lfu.Get("x")
	lfu.Put("z", "z1") // "y" has the lowest frequency, evicted
	if _, ok := lfu.Get("y"); !ok {
		fmt.Println("y evicted:", true)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
b evicted: true
a still cached: 1
c still cached: 3
y evicted: true
```

### Tests

Create `cache_test.go`:

```go
package cache

import (
	"sync"
	"testing"
)

func fakeClock() func() int64 {
	var tick int64
	return func() int64 {
		tick++
		return tick
	}
}

func TestLRUEvictsLeastRecentlyUsed(t *testing.T) {
	t.Parallel()
	c := New(2, LRU, fakeClock())
	c.Put("a", 1)
	c.Put("b", 2)
	c.Get("a")
	c.Put("c", 3)

	if _, ok := c.Get("b"); ok {
		t.Fatal("b should have been evicted (least recently used)")
	}
	if v, ok := c.Get("a"); !ok || v != 1 {
		t.Fatalf("a should still be cached, got %v, %v", v, ok)
	}
	if v, ok := c.Get("c"); !ok || v != 3 {
		t.Fatalf("c should be cached, got %v, %v", v, ok)
	}
}

func TestLFUEvictsLeastFrequentlyUsed(t *testing.T) {
	t.Parallel()
	c := New(2, LFU, fakeClock())
	c.Put("x", "x1")
	c.Put("y", "y1")
	c.Get("x")
	c.Get("x")
	c.Put("z", "z1")

	if _, ok := c.Get("y"); ok {
		t.Fatal("y should have been evicted (least frequently used)")
	}
	if _, ok := c.Get("x"); !ok {
		t.Fatal("x should still be cached")
	}
	if _, ok := c.Get("z"); !ok {
		t.Fatal("z should be cached")
	}
}

func TestCustomEvictionPolicy(t *testing.T) {
	t.Parallel()
	// A custom policy that always evicts whichever key is
	// lexicographically largest, ignoring recency/frequency entirely.
	evictLargestKey := func(entries map[string]AccessInfo) string {
		var victim string
		for k := range entries {
			if k > victim {
				victim = k
			}
		}
		return victim
	}

	c := New(2, evictLargestKey, fakeClock())
	c.Put("m", 1)
	c.Put("z", 2)
	c.Put("a", 3) // "z" is lexicographically largest, evicted

	if _, ok := c.Get("z"); ok {
		t.Fatal("z should have been evicted by the custom policy")
	}
	if _, ok := c.Get("m"); !ok {
		t.Fatal("m should still be cached")
	}
	if _, ok := c.Get("a"); !ok {
		t.Fatal("a should be cached")
	}
}

func TestPutUpdatingExistingKeyDoesNotEvict(t *testing.T) {
	t.Parallel()
	c := New(2, LRU, fakeClock())
	c.Put("a", 1)
	c.Put("b", 2)
	c.Put("a", 100) // update, not a new key: no eviction

	if v, ok := c.Get("a"); !ok || v != 100 {
		t.Fatalf("a = %v, %v, want 100, true", v, ok)
	}
	if _, ok := c.Get("b"); !ok {
		t.Fatal("b should still be cached, Put on an existing key must not evict")
	}
	if c.Len() != 2 {
		t.Fatalf("Len() = %d, want 2", c.Len())
	}
}

func TestConcurrentGetPutIsRaceFree(t *testing.T) {
	t.Parallel()
	c := New(8, LRU, fakeClock())
	keys := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}

	var wg sync.WaitGroup
	for i, k := range keys {
		wg.Add(1)
		go func(k string, v int) {
			defer wg.Done()
			c.Put(k, v)
			c.Get(k)
		}(k, i)
	}
	wg.Wait()

	if c.Len() > 8 {
		t.Fatalf("Len() = %d, want <= 8", c.Len())
	}
}

func TestFakeClockIsMonotonicForDeterministicTests(t *testing.T) {
	t.Parallel()
	clock := fakeClock()
	a := clock()
	b := clock()
	if b <= a {
		t.Fatalf("clock did not advance: a=%d b=%d", a, b)
	}
}
```

## Review

The design is correct exactly when `Put` treats `evict` as a black box
that only ever sees a consistent, complete snapshot and only ever returns
a key that gets evicted before the new one is inserted.
`TestLRUEvictsLeastRecentlyUsed` and `TestLFUEvictsLeastFrequentlyUsed`
confirm the two built-in policies read the metadata correctly, but
`TestCustomEvictionPolicy` is the test that actually validates the
architecture: a policy with no idea what LRU or LFU even mean plugs into
the exact same `Cache` and is obeyed, which would be impossible if
eviction were hard-coded. `TestPutUpdatingExistingKeyDoesNotEvict` guards
the easy-to-miss branch — `evict` must never run for a key that already
exists, only for a genuinely new one arriving at capacity. The
concurrency test does not assert anything about which key gets evicted
under contention (that would make the test itself racy); it only asserts
the invariant that must hold regardless of ordering — the cache never
exceeds its capacity and every access is data-race-free under `-race`,
because `Get` and `Put` both hold the same mutex across the read of
`c.data`, the snapshot, and the mutation.

## Resources

- [Go Specification: Function types](https://go.dev/ref/spec#Function_types)
- [container/list (classic LRU building block)](https://pkg.go.dev/container/list)
- [sync.Mutex](https://pkg.go.dev/sync#Mutex)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [28-grpc-interceptor-chain-composition.md](28-grpc-interceptor-chain-composition.md) | Next: [30-message-handler-type-registry.md](30-message-handler-type-registry.md)
