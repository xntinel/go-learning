# Exercise 1: Mutex: Protect Shared State

When multiple goroutines read and write the same variable without
synchronization, the result is a data race -- non-deterministic, invisible in
testing, and prone to failing silently in production. In an API server,
concurrent HTTP handlers sharing an in-memory cache are the classic instance:
corrupted entries, vanished keys, or an outright panic from a concurrent map
write. A `sync.Mutex` serializes access so only one goroutine touches the
shared state at a time. This exercise starts from a cache that panics under load
and walks it to the idiomatic struct-embedded mutex, with the race detector
confirming each step.

## What you'll build

```text
01-mutex-protect-shared-state/
  main.go        an in-memory API cache guarded by sync.Mutex: the race,
                 the Lock/Unlock fix, defer Unlock, and an embedded-mutex type
```

- Build: a concurrent in-memory API cache protected by a mutex, from unsafe to idiomatic.
- Implement: an unsafe map demo that panics under concurrent writes, Lock/Unlock-wrapped `writeCacheEntry`/`readCacheEntry`, a `SimpleCache` using `defer Unlock`, and an `APICache` with an embedded mutex and a copy-returning `Snapshot`.
- Verify: `go run -race main.go`

### Why a mutex eliminates the race

A `sync.Mutex` (mutual exclusion lock) solves this by ensuring that only one goroutine at a time can execute a critical section of code. When a goroutine calls `Lock()`, any other goroutine that also calls `Lock()` will block until the first goroutine calls `Unlock()`. This serializes access to shared state, eliminating the race.

The idiomatic Go pattern is to call `defer mu.Unlock()` immediately after `Lock()`. This guarantees the lock is released even if the critical section panics, preventing deadlocks caused by forgotten unlocks.

## Step 1 -- Observe the Race Condition in a Shared Cache

Imagine an API server that caches responses in memory. Multiple HTTP handler goroutines write to the same map concurrently. Without protection, the cache is corrupted:

```go
package main

import (
	"fmt"
	"sync"
)

const (
	simulatedHandlers = 100
	endpointCount     = 10
)

func simulateUnsafeCacheAccess(cache map[string]string, handlerID int) {
	key := fmt.Sprintf("endpoint-%d", handlerID%endpointCount)
	cache[key] = fmt.Sprintf(`{"handler":%d,"status":"ok"}`, handlerID)
	_ = cache[key]
}

func runUnsafeCacheDemo() {
	cache := make(map[string]string)
	var wg sync.WaitGroup

	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("PANIC: %v\n", r)
			fmt.Println("This is what happens when concurrent goroutines write to an unprotected map.")
		}
	}()

	for i := 0; i < simulatedHandlers; i++ {
		wg.Add(1)
		go func(handlerID int) {
			defer wg.Done()
			simulateUnsafeCacheAccess(cache, handlerID)
		}(i)
	}

	wg.Wait()
	fmt.Printf("Cache has %d entries\n", len(cache))
}

func main() {
	runUnsafeCacheDemo()
}
```

Run it:

```bash
go run main.go
```

You should see a fatal panic:
```
PANIC: concurrent map writes
This is what happens when concurrent goroutines write to an unprotected map.
```

Now run with the race detector to get detailed diagnostics:

```bash
go run -race main.go
```

The race detector reports `DATA RACE` warnings with stack traces showing the conflicting accesses.

### Verification
You should see a panic from concurrent map writes, or `WARNING: DATA RACE` output from the race detector. This is the real consequence of sharing a cache without synchronization in a production server.

## Step 2 -- Protect the Cache with sync.Mutex

Wrap every cache access in a Lock/Unlock pair. Every goroutine must acquire the mutex before touching the map:

```go
package main

import (
	"fmt"
	"sync"
)

const (
	simulatedHandlers = 100
	endpointCount     = 10
)

func writeCacheEntry(mu *sync.Mutex, cache map[string]string, key, value string) {
	mu.Lock()
	cache[key] = value
	mu.Unlock()
}

func readCacheEntry(mu *sync.Mutex, cache map[string]string, key string) string {
	mu.Lock()
	defer mu.Unlock()
	return cache[key]
}

func simulateSafeCacheAccess(mu *sync.Mutex, cache map[string]string, handlerID int) {
	key := fmt.Sprintf("endpoint-%d", handlerID%endpointCount)
	response := fmt.Sprintf(`{"handler":%d,"status":"ok"}`, handlerID)
	writeCacheEntry(mu, cache, key, response)
	_ = readCacheEntry(mu, cache, key)
}

func main() {
	cache := make(map[string]string)
	var mu sync.Mutex
	var wg sync.WaitGroup

	for i := 0; i < simulatedHandlers; i++ {
		wg.Add(1)
		go func(handlerID int) {
			defer wg.Done()
			simulateSafeCacheAccess(&mu, cache, handlerID)
		}(i)
	}

	wg.Wait()
	fmt.Printf("Cache has %d entries (expected %d)\n", len(cache), endpointCount)
}
```

```bash
go run -race main.go
```

Expected output:
```
Cache has 10 entries (expected 10)
```

No panics, no race warnings. The mutex serializes access so only one goroutine touches the map at a time.

### Verification
Run `go run -race main.go`. The program completes cleanly with exactly 10 cache entries and no `DATA RACE` warnings.

## Step 3 -- The defer Unlock Pattern

In production code, critical sections often contain logic that can return early or panic. Using `defer mu.Unlock()` immediately after `Lock()` guarantees the lock is always released:

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

const (
	writerCount  = 50
	userKeyCount = 5
)

type SimpleCache struct {
	mu      sync.Mutex
	entries map[string]string
}

func NewSimpleCache() *SimpleCache {
	return &SimpleCache{entries: make(map[string]string)}
}

func (c *SimpleCache) Set(key, value string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = value
}

func (c *SimpleCache) Get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	val, ok := c.entries[key]
	return val, ok
}

func populateCacheConcurrently(cache *SimpleCache) {
	var wg sync.WaitGroup

	for i := 0; i < writerCount; i++ {
		wg.Add(1)
		go func(handlerID int) {
			defer wg.Done()
			key := fmt.Sprintf("user-%d", handlerID%userKeyCount)
			value := fmt.Sprintf(`{"id":%d,"ts":"%s"}`, handlerID, time.Now().Format(time.RFC3339Nano))
			cache.Set(key, value)
		}(i)
	}

	wg.Wait()
}

func printCacheContents(cache *SimpleCache) {
	for i := 0; i < userKeyCount; i++ {
		key := fmt.Sprintf("user-%d", i)
		val, ok := cache.Get(key)
		fmt.Printf("  %s: found=%v value=%s\n", key, ok, val)
	}
}

func main() {
	cache := NewSimpleCache()
	populateCacheConcurrently(cache)
	printCacheContents(cache)
}
```

The `defer mu.Unlock()` line executes when the enclosing function returns, guaranteeing the lock is always released. This is especially important when the critical section might return early or panic.

### Verification
```bash
go run -race main.go
```
All 5 user keys should be present, no race warnings.

## Step 4 -- Struct-Embedded Mutex: The Cache Type

The idiomatic Go pattern places the mutex alongside the data it protects inside a struct. This is how you would build a real API cache:

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

const simulatedRequests = 200

type CacheEntry struct {
	Body     string
	CachedAt time.Time
}

type APICache struct {
	mu      sync.Mutex
	entries map[string]CacheEntry
}

func NewAPICache() *APICache {
	return &APICache{entries: make(map[string]CacheEntry)}
}

func (c *APICache) Set(endpoint string, body string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[endpoint] = CacheEntry{Body: body, CachedAt: time.Now()}
}

func (c *APICache) Get(endpoint string) (CacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[endpoint]
	return entry, ok
}

func (c *APICache) Size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// Snapshot returns a COPY so callers cannot bypass the mutex.
func (c *APICache) Snapshot() map[string]CacheEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make(map[string]CacheEntry, len(c.entries))
	for k, v := range c.entries {
		result[k] = v
	}
	return result
}

func simulateHandlerTraffic(cache *APICache, endpoints []string) {
	var wg sync.WaitGroup

	for i := 0; i < simulatedRequests; i++ {
		wg.Add(1)
		go func(handlerID int) {
			defer wg.Done()
			endpoint := endpoints[handlerID%len(endpoints)]
			cache.Set(endpoint, fmt.Sprintf(`{"handler":%d,"status":"ok"}`, handlerID))

			if entry, ok := cache.Get(endpoint); ok {
				_ = entry.Body
			}
		}(i)
	}

	wg.Wait()
}

func printCacheSnapshot(cache *APICache) {
	snap := cache.Snapshot()
	fmt.Printf("Cache has %d endpoints:\n", len(snap))
	for endpoint, entry := range snap {
		fmt.Printf("  %s -> cached at %s\n", endpoint, entry.CachedAt.Format("15:04:05.000"))
	}
}

func main() {
	cache := NewAPICache()
	endpoints := []string{"/api/users", "/api/orders", "/api/products", "/api/health", "/api/config"}

	simulateHandlerTraffic(cache, endpoints)
	printCacheSnapshot(cache)
}
```

Expected output:
```
Cache has 5 endpoints:
  /api/users -> cached at 14:23:01.123
  /api/orders -> cached at 14:23:01.124
  /api/products -> cached at 14:23:01.123
  /api/health -> cached at 14:23:01.124
  /api/config -> cached at 14:23:01.123
```

The key design points:
- The mutex is unexported (`mu`), preventing external code from locking it incorrectly.
- `Snapshot()` returns a copy of the map, so callers cannot mutate internal state without the mutex.
- Every method that touches `entries` acquires the lock first.

### Verification
```bash
go run -race main.go
```
All 5 endpoints cached, no race warnings.

## Common Mistakes

### Forgetting to Unlock

```go
package main

import (
	"fmt"
	"sync"
)

func main() {
	var mu sync.Mutex
	mu.Lock()
	fmt.Println("acquired lock")
	// Forgot mu.Unlock() -- any goroutine that calls mu.Lock() will block forever.
	// This causes a deadlock if main also tries to lock again.
	mu.Lock() // DEADLOCK: blocks forever waiting for itself
	fmt.Println("this line is never reached")
}
```

**What happens:** Deadlock. All goroutines waiting on Lock will block permanently.

**Fix:** Always pair Lock with Unlock. Use `defer mu.Unlock()` immediately after Lock.

### Copying a Mutex

```go
package main

import (
	"fmt"
	"sync"
)

func cacheSet(mu sync.Mutex, cache map[string]string, key, val string) {
	mu.Lock()
	defer mu.Unlock()
	cache[key] = val // this lock is independent of the original -- no protection!
}

func main() {
	var mu sync.Mutex
	cache := make(map[string]string)
	var wg sync.WaitGroup

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			cacheSet(mu, cache, fmt.Sprintf("k%d", id), "v") // each goroutine gets its own mutex copy!
		}(i)
	}

	wg.Wait()
	fmt.Printf("Entries: %d (likely panic or wrong count due to copied mutex)\n", len(cache))
}
```

**What happens:** Each goroutine locks its own copy -- no mutual exclusion at all. Concurrent map writes will panic.

**Fix:** Always pass `*sync.Mutex` (a pointer), or better, embed the mutex in a struct:
```go
func cacheSet(mu *sync.Mutex, cache map[string]string, key, val string) {
	mu.Lock()
	defer mu.Unlock()
	cache[key] = val
}
```

### Locking Too Broadly

```go
mu.Lock()
result := fetchFromDatabase(userID) // holds the lock during slow I/O
cache[userID] = result
mu.Unlock()
```

**What happens:** All goroutines are serialized through the database call, eliminating concurrency benefits. Your API server handles one request at a time.

**Fix:** Only hold the lock for the shared state access:
```go
result := fetchFromDatabase(userID) // no lock needed here
mu.Lock()
cache[userID] = result
mu.Unlock()
```

## Review

A data race is what you get when two goroutines touch the same variable without
ordering and at least one writes; the outcome depends on scheduling, so it can
pass every test and still corrupt data in production. In a server the shared
in-memory cache is the usual victim -- an unprotected map panics outright on
concurrent writes. `sync.Mutex` fixes this by mutual exclusion: `Lock()` blocks
any other locker until `Unlock()`, so exactly one goroutine is in the critical
section at a time. Two disciplines keep that correct: pair every `Lock` with a
`defer Unlock` immediately, so an early return or panic can never strand the
lock, and never copy a `Mutex` -- pass it by pointer or, better, embed it in the
struct beside the data it guards, because a copied mutex protects nothing. Keep
the critical section tight (guard the map access, not a slow database call) and
return copies from locked methods so callers cannot reach inside without the
lock.

To confirm the ideas stuck, extend the `APICache`. Add a `Delete(endpoint)`
method, a `SetWithTTL(endpoint, body, ttl)` that records an expiration time, and
a `CleanExpired()` that removes every entry past its TTL -- each acquiring the
lock the same way the existing methods do. Then launch a hundred goroutines that
randomly set, get, and delete entries and run the whole thing under
`go run -race`. A clean race report is the proof that every path through your
new methods takes the lock before it touches the map.

## Resources
- [sync.Mutex documentation](https://pkg.go.dev/sync#Mutex) -- Lock/Unlock semantics and the warning that a Mutex must not be copied after first use.
- [Go Data Race Detector](https://go.dev/doc/articles/race_detector) -- how `-race` instruments your program to catch exactly the bug this lesson provokes.
- [Go Blog: Share Memory by Communicating](https://go.dev/blog/codelab-share) -- the broader guidance on when to reach for a mutex versus a channel.

---

Back to [Concurrency](../../concurrency.md) | Next: [02-rwmutex-readers-writers](../02-rwmutex-readers-writers/02-rwmutex-readers-writers.md)
