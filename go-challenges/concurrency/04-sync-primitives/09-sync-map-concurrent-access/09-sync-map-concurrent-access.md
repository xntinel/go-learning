# Exercise 9: sync.Map: Concurrent Access

Go maps are not safe for concurrent use: if several goroutines read and write one
map without synchronization, the runtime detects the collision and kills the
process with `concurrent map read and map write` -- a deliberate choice to crash
loudly rather than corrupt data silently. The scenario that forces the issue is
an HTTP server holding user sessions in memory, where handler goroutines create
sessions on login, read them to validate requests, update them on activity, and
delete them on logout, all at once across thousands of requests. This exercise
shows the panic, then two ways to make the map safe -- and, more importantly,
when to reach for each.

## What you'll build

```text
09-sync-map-concurrent-access/
  main.go        the concurrent-map panic, a sync.Map session store, a
                 concurrent lifecycle test, and a sync.Map vs RWMutex benchmark
```

- Build: four programs that move from the crash to a working session store to a benchmark that tells you which tool to use.
- Implement: a `SessionStore` over `sync.Map` using `Load`, `Store`, `LoadOrStore`, `Delete`, and `Range`; a concurrent login/validate/logout simulation with `atomic` metrics; and `benchSyncMap` vs `benchMutexMap` comparators.
- Verify: `go run main.go` (Steps 1 and 3 are worth `go run -race main.go`)

### Why sync.Map is not the default

The standard fix is wrapping the map with a `sync.RWMutex`. However, `sync.Map`
exists for two specific use cases where it outperforms a mutex-protected map:

1. **Append-only maps**: keys are written once and then only read (session creation followed by many reads). `sync.Map` eliminates lock contention on reads.
2. **Disjoint key access**: different goroutines work on different key subsets (each user's session is touched only by that user's requests). `sync.Map` avoids locking the entire map for unrelated operations.

For general-purpose concurrent maps, a regular `map` with `sync.RWMutex` is
typically simpler and often faster. Use `sync.Map` only when profiling shows it
helps.

## Step 1 -- The Map Panic: Concurrent Session Writes

Multiple HTTP handlers try to write sessions to a regular map simultaneously:

```go
package main

import (
	"fmt"
	"sync"
)

const concurrentUsers = 100

func simulateUnsafeSessionAccess(sessions map[string]string, userID int) {
	sessionID := fmt.Sprintf("sess-%d", userID)
	sessions[sessionID] = fmt.Sprintf("user-%d", userID) // concurrent write -- UNSAFE
	_ = sessions[sessionID]                                // concurrent read -- UNSAFE
}

func demonstrateMapPanic() {
	sessions := make(map[string]string)
	var wg sync.WaitGroup

	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("PANIC: %v\n", r)
			fmt.Println("Regular maps are NOT safe for concurrent session storage!")
		}
	}()

	for i := 0; i < concurrentUsers; i++ {
		wg.Add(1)
		go func(userID int) {
			defer wg.Done()
			simulateUnsafeSessionAccess(sessions, userID)
		}(i)
	}

	wg.Wait()
}

func main() {
	demonstrateMapPanic()
}
```

Expected output:
```
PANIC: concurrent map writes
Regular maps are NOT safe for concurrent session storage!
```

### Verification
```bash
go run main.go
```
The program should panic (caught by recover) with a concurrent map access error.

## Step 2 -- Session Store with sync.Map

Build a concurrent session store using `sync.Map`:

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

type Session struct {
	UserID    string
	CreatedAt time.Time
	Data      map[string]string
}

type SessionStore struct {
	sessions sync.Map
}

func (s *SessionStore) Create(sessionID string, sess Session) {
	s.sessions.Store(sessionID, sess)
}

func (s *SessionStore) Find(sessionID string) (Session, bool) {
	val, ok := s.sessions.Load(sessionID)
	if !ok {
		return Session{}, false
	}
	return val.(Session), true
}

func (s *SessionStore) GetOrCreate(sessionID string, newSession Session) (Session, bool) {
	actual, loaded := s.sessions.LoadOrStore(sessionID, newSession)
	return actual.(Session), loaded
}

func (s *SessionStore) Delete(sessionID string) {
	s.sessions.Delete(sessionID)
}

func (s *SessionStore) CountActive() int {
	count := 0
	s.sessions.Range(func(key, value any) bool {
		count++
		sess := value.(Session)
		fmt.Printf("  Active: %s -> user=%s\n", key, sess.UserID)
		return true
	})
	return count
}

func demonstrateSyncMapOperations() {
	store := &SessionStore{}

	store.Create("sess-abc123", Session{
		UserID:    "user-1",
		CreatedAt: time.Now(),
		Data:      map[string]string{"role": "admin"},
	})
	store.Create("sess-def456", Session{
		UserID:    "user-2",
		CreatedAt: time.Now(),
		Data:      map[string]string{"role": "viewer"},
	})

	if sess, ok := store.Find("sess-abc123"); ok {
		fmt.Printf("Found session: user=%s, role=%s\n", sess.UserID, sess.Data["role"])
	}

	// LoadOrStore: create session only if it does not exist (login idempotency)
	candidate := Session{UserID: "user-2", CreatedAt: time.Now(), Data: map[string]string{"role": "editor"}}
	existing, alreadyExists := store.GetOrCreate("sess-def456", candidate)
	if alreadyExists {
		fmt.Printf("Session already exists: user=%s, role=%s (not overwritten)\n", existing.UserID, existing.Data["role"])
	}

	store.Delete("sess-abc123")
	_, found := store.Find("sess-abc123")
	fmt.Printf("After logout: session found=%v\n", found)

	count := store.CountActive()
	fmt.Printf("Total active sessions: %d\n", count)
}

func main() {
	demonstrateSyncMapOperations()
}
```

Expected output:
```
Found session: user=user-1, role=admin
Session already exists: user=user-2, role=viewer (not overwritten)
After logout: session found=false
  Active: sess-def456 -> user=user-2
Total active sessions: 1
```

### Verification
```bash
go run main.go
```
All operations should work correctly.

## Step 3 -- Concurrent Session Management

Simulate a real server with concurrent login, validation, and logout:

```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

const (
	concurrentUsers     = 50
	validationsPerUser  = 10
)

type Session struct {
	UserID    string
	CreatedAt time.Time
}

type SessionMetrics struct {
	Logins      atomic.Int64
	Validations atomic.Int64
	Logouts     atomic.Int64
}

func simulateUserLifecycle(sessions *sync.Map, userID int, metrics *SessionMetrics) {
	sessKey := fmt.Sprintf("sess-%d", userID)

	sessions.Store(sessKey, Session{
		UserID:    fmt.Sprintf("user-%d", userID),
		CreatedAt: time.Now(),
	})
	metrics.Logins.Add(1)

	for j := 0; j < validationsPerUser; j++ {
		if val, ok := sessions.Load(sessKey); ok {
			_ = val.(Session).UserID
			metrics.Validations.Add(1)
		}
	}

	sessions.Delete(sessKey)
	metrics.Logouts.Add(1)
}

func countRemainingSessions(sessions *sync.Map) int {
	remaining := 0
	sessions.Range(func(_, _ any) bool {
		remaining++
		return true
	})
	return remaining
}

func runConcurrentSessionTest() {
	var sessions sync.Map
	var wg sync.WaitGroup
	metrics := &SessionMetrics{}

	for i := 0; i < concurrentUsers; i++ {
		wg.Add(1)
		go func(userID int) {
			defer wg.Done()
			simulateUserLifecycle(&sessions, userID, metrics)
		}(i)
	}

	wg.Wait()

	remaining := countRemainingSessions(&sessions)

	fmt.Println("=== Session Store Results ===")
	fmt.Printf("Logins:      %d\n", metrics.Logins.Load())
	fmt.Printf("Validations: %d\n", metrics.Validations.Load())
	fmt.Printf("Logouts:     %d\n", metrics.Logouts.Load())
	fmt.Printf("Remaining:   %d (should be 0)\n", remaining)
}

func main() {
	runConcurrentSessionTest()
}
```

Expected output:
```
=== Session Store Results ===
Logins:      50
Validations: 500
Logouts:     50
Remaining:   0 (should be 0)
```

### Verification
```bash
go run -race main.go
```
No panics, no data races. All 50 sessions created, validated, and cleaned up.

## Step 4 -- When to Use What: Benchmark Comparison

Compare `sync.Map` vs `map+RWMutex` for different workloads. sync.Map wins when reads dominate and keys are disjoint:

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

const (
	opsPerGoroutine = 5000
	prePopulateKeys = 1000
	writeRatio      = 10 // writers do ops/writeRatio operations
)

func keyForIndex(base, ops, iteration, keySpace int) string {
	return fmt.Sprintf("key-%d", (base*ops+iteration)%keySpace)
}

func benchSyncMap(readers, writers, ops int) time.Duration {
	var m sync.Map
	var wg sync.WaitGroup

	for i := 0; i < prePopulateKeys; i++ {
		m.Store(fmt.Sprintf("key-%d", i), i)
	}

	start := time.Now()

	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for j := 0; j < ops; j++ {
				m.Load(keyForIndex(workerID, ops, j, prePopulateKeys))
			}
		}(i)
	}

	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for j := 0; j < ops/writeRatio; j++ {
				m.Store(keyForIndex(workerID, ops, j, prePopulateKeys), j)
			}
		}(i)
	}

	wg.Wait()
	return time.Since(start)
}

func benchMutexMap(readers, writers, ops int) time.Duration {
	var mu sync.RWMutex
	m := make(map[string]int)
	var wg sync.WaitGroup

	for i := 0; i < prePopulateKeys; i++ {
		m[fmt.Sprintf("key-%d", i)] = i
	}

	start := time.Now()

	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for j := 0; j < ops; j++ {
				mu.RLock()
				_ = m[keyForIndex(workerID, ops, j, prePopulateKeys)]
				mu.RUnlock()
			}
		}(i)
	}

	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for j := 0; j < ops/writeRatio; j++ {
				mu.Lock()
				m[keyForIndex(workerID, ops, j, prePopulateKeys)] = j
				mu.Unlock()
			}
		}(i)
	}

	wg.Wait()
	return time.Since(start)
}

func runWorkloadComparison(label string, readers, writers int) {
	fmt.Printf("=== %s ===\n", label)
	syncTime := benchSyncMap(readers, writers, opsPerGoroutine)
	mutexTime := benchMutexMap(readers, writers, opsPerGoroutine)
	fmt.Printf("  sync.Map:    %v\n", syncTime.Round(time.Millisecond))
	fmt.Printf("  map+RWMutex: %v\n", mutexTime.Round(time.Millisecond))
}

func main() {
	runWorkloadComparison("Read-Heavy Workload (90% reads, 10% writes)", 90, 10)
	fmt.Println()
	runWorkloadComparison("Write-Heavy Workload (50% reads, 50% writes)", 50, 50)

	fmt.Println("\nConclusion:")
	fmt.Println("  sync.Map can win for read-heavy, disjoint-key workloads.")
	fmt.Println("  map+RWMutex is simpler and often faster for general use.")
	fmt.Println("  Profile your actual workload before choosing sync.Map.")
}
```

Expected output (times vary):
```
=== Read-Heavy Workload (90% reads, 10% writes) ===
  sync.Map:    12ms
  map+RWMutex: 18ms

=== Write-Heavy Workload (50% reads, 50% writes) ===
  sync.Map:    25ms
  map+RWMutex: 15ms

Conclusion:
  sync.Map can win for read-heavy, disjoint-key workloads.
  map+RWMutex is simpler and often faster for general use.
  Profile your actual workload before choosing sync.Map.
```

### Verification
```bash
go run main.go
```
Read-heavy should favor sync.Map; write-heavy should favor map+RWMutex.

## Common Mistakes

### Using sync.Map for Everything

**Wrong:** Replacing all concurrent maps with `sync.Map` blindly.

**Reality:** `sync.Map` is optimized for two patterns (append-only, disjoint keys). For general concurrent map access, `map+RWMutex` is simpler and often faster. A session store with mostly reads is a good fit. A metrics counter that every goroutine writes to is not.

### Type Assertions Everywhere

```go
val, _ := sessions.Load("sess-abc123")
sess := val.(Session) // type assertion on every access -- runtime panic if wrong type
```

**Reality:** `sync.Map` stores `any` types, requiring type assertions on every read. If your map is type-homogeneous (all values are `Session`), a generic `map[string]Session` with a mutex is more ergonomic and type-safe. The compiler catches type errors at build time instead of runtime.

### Mixing Range with Delete

```go
sessions.Range(func(key, value any) bool {
    sess := value.(Session)
    if time.Since(sess.CreatedAt) > 24*time.Hour {
        sessions.Delete(key) // safe (no panic), but order is non-deterministic
    }
    return true
})
```

Deleting during Range is safe (no panic), but the deleted key may or may not be visited by subsequent Range iterations. The behavior is non-deterministic. For session cleanup, this is usually acceptable because the next cleanup pass will catch anything missed.

### Assuming Range Sees a Consistent Snapshot
`Range` does not take a snapshot. Other goroutines can `Store` or `Delete` entries during Range execution. If you need a consistent snapshot, use a regular map with a read lock.

## Review

The mechanism is a safety valve first: a plain `map` under concurrent read-write
does not corrupt quietly, it panics, and `sync.Map` exists to make that access
legal with `Load`, `Store`, `LoadOrStore`, `Delete`, and `Range`. But it is a
specialist, not a default. It earns its keep on read-heavy workloads with stable
or disjoint keys -- session stores, config caches, service registries -- where it
sheds the lock contention a mutex would impose. Everywhere else, a regular `map`
guarded by `sync.RWMutex` is simpler, type-safe, and often faster, because
`sync.Map` stores `any` and forces a type assertion on every read that the
compiler cannot check. Two sharp edges to remember: `LoadOrStore` gives you
atomic get-or-create, but a sequence of separate atomic operations is not itself
atomic; and `Range` never takes a consistent snapshot, so entries can be added or
removed while you iterate. The honest rule is to profile before choosing it.

To pressure-test that judgment, build a concurrent session store exposing
`GetOrCreate(sessionID, factory)` backed by `LoadOrStore`, `Touch(sessionID)`
that updates a `LastAccess` timestamp, and `CleanExpired(maxAge)` that removes
and counts sessions older than the cutoff. Drive it with 100 goroutines doing
random operations, implement it both ways -- `sync.Map` and `map`+`RWMutex` --
and confirm both are clean under `-race`. Which one reads more clearly is itself
part of the answer.

## Resources

- [sync.Map documentation](https://pkg.go.dev/sync#Map) -- the method set and the two workloads it is documented to optimize for.
- [Go Blog: Maps in Action](https://go.dev/blog/maps) -- how plain maps behave, including why concurrent use is unsafe.
- [sync.Map GopherCon Talk](https://www.youtube.com/watch?v=C1EtfDnsdDs) -- a walkthrough of sync.Map's internals and when it beats a mutex.

---

Back to [Concurrency](../../concurrency.md) | Next: [10-build-thread-safe-counter](../10-build-thread-safe-counter/10-build-thread-safe-counter.md)
