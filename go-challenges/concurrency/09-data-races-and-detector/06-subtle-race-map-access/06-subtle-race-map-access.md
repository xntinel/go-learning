# Exercise 6: Subtle Race: Map Access

Unlike a counter race, which quietly produces the wrong number, concurrent map access in Go does not corrupt silently -- it kills the process. The runtime carries a built-in check that terminates the program the instant two goroutines touch a map with at least one of them writing, printing `fatal error: concurrent map writes` (or `concurrent map read and map write`) with no recovery, no graceful shutdown, and no handler that can catch it. It is one of Go's most common production crashes, and this exercise reproduces it inside a session store and then fixes it with the primitive a read-heavy store actually wants.

## What you'll build

```text
subtle-race-map-access/
  main.go        the write+write fatal crash, the read+write fatal crash, an
                 RWMutex-protected SessionStore that survives -race, and a
                 Mutex-vs-RWMutex benchmark under a read-heavy workload
```

- Build: a thread-safe user session store and a benchmark showing why `RWMutex` beats `Mutex` when reads dominate.
- Implement: a `RacySessionStore` that reproduces both fatal crashes; a `SessionStore` guarding every map operation with `sync.RWMutex` (`RLock` for reads, `Lock` for writes, delete, and iteration); and `MutexMapBench` / `RWMutexMapBench`.
- Verify: `go run main.go` to watch the racy versions crash, then `go run -race main.go` on the fixed store -- correct counts, zero warnings.

### Why the runtime chooses a crash

Unlike the counter race from exercises 01-05 (which produces silently wrong results), concurrent map access in Go causes the program to **crash immediately** with a fatal error. The Go runtime detects concurrent map read/write or write/write operations and terminates the process.

This is NOT a data race in the traditional sense. The runtime's built-in map concurrency check is a separate mechanism from the `-race` flag. It is a **hard crash**: no recovery, no graceful shutdown, no error handling. Your server goes down instantly.

This is one of Go's most common production crashes. It typically appears when:
- Multiple HTTP handlers read/write a shared session map
- A cache is accessed without synchronization
- Configuration is reloaded while being read

The Go designers intentionally chose a crash over silent corruption, because a corrupted map can cause memory safety violations, infinite loops during hash table traversal, and corruption of unrelated memory.

## Step 1 -- Build the Racy Session Store

Create a user session store where multiple HTTP handlers read and write sessions concurrently. This simulates a real authentication layer:

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

const (
	writerHandlers      = 10
	sessionsPerHandler  = 100
	sessionDuration     = 1 * time.Hour
)

// Session represents a user authentication session.
type Session struct {
	UserID    string
	Token     string
	ExpiresAt time.Time
}

// RacySessionStore demonstrates a session store WITHOUT synchronization.
// BUG: concurrent map writes cause a fatal crash.
type RacySessionStore struct {
	sessions map[string]Session
}

func NewRacySessionStore() *RacySessionStore {
	return &RacySessionStore{sessions: make(map[string]Session)}
}

// Create writes a session to the unprotected map.
// FATAL: concurrent calls cause "fatal error: concurrent map writes".
func (s *RacySessionStore) Create(token string, session Session) {
	s.sessions[token] = session
}

func (s *RacySessionStore) Count() int {
	return len(s.sessions)
}

func simulateConcurrentLogins(store *RacySessionStore) {
	var wg sync.WaitGroup

	for i := 0; i < writerHandlers; i++ {
		wg.Add(1)
		go func(handlerID int) {
			defer wg.Done()
			for j := 0; j < sessionsPerHandler; j++ {
				token := fmt.Sprintf("token-%d-%d", handlerID, j)
				store.Create(token, Session{
					UserID:    fmt.Sprintf("user-%d", handlerID),
					Token:     token,
					ExpiresAt: time.Now().Add(sessionDuration),
				})
			}
		}(i)
	}

	wg.Wait()
	fmt.Printf("Sessions stored: %d\n", store.Count())
}

func main() {
	fmt.Println("=== Concurrent Map Write Crash ===")
	fmt.Println("This WILL crash with: fatal error: concurrent map writes")
	fmt.Println()

	store := NewRacySessionStore()
	simulateConcurrentLogins(store)
}
```

### Verification
```bash
go run main.go
```
Expected output:
```
fatal error: concurrent map writes

goroutine 19 [running]:
...
exit status 2
```

You may need to run it a few times. The crash is non-deterministic but highly likely with 10 goroutines. Even though every goroutine writes to **different keys**, the map's internal hash table is a shared data structure: bucket resizing during growth affects all keys.

## Step 2 -- The Read+Write Crash

Even reading while another goroutine writes causes a fatal crash. This surprises many developers who assume "reading is safe":

```go
package main

import (
	"fmt"
	"sync"
)

const (
	prePopulatedSessions = 100
	concurrentWriters    = 5
	concurrentReaders    = 5
	operationsPerWorker  = 200
)

// Session represents a user authentication session.
type Session struct {
	UserID    string
	Token     string
}

// RacySessionStore demonstrates a session store WITHOUT synchronization.
// BUG: concurrent reads and writes cause a fatal crash.
type RacySessionStore struct {
	sessions map[string]Session
}

func NewRacySessionStore() *RacySessionStore {
	return &RacySessionStore{sessions: make(map[string]Session)}
}

func (s *RacySessionStore) Create(token string, session Session) {
	s.sessions[token] = session // FATAL when called concurrently with Get
}

func (s *RacySessionStore) Get(token string) Session {
	return s.sessions[token] // FATAL when called concurrently with Create
}

func (s *RacySessionStore) Count() int {
	return len(s.sessions)
}

func prePopulate(store *RacySessionStore, count int) {
	for i := 0; i < count; i++ {
		token := fmt.Sprintf("token-%d", i)
		store.Create(token, Session{
			UserID: fmt.Sprintf("user-%d", i),
			Token:  token,
		})
	}
}

func launchWriters(store *RacySessionStore, wg *sync.WaitGroup) {
	for i := 0; i < concurrentWriters; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < operationsPerWorker; j++ {
				token := fmt.Sprintf("new-token-%d-%d", id, j)
				store.Create(token, Session{
					UserID: fmt.Sprintf("user-%d", id),
					Token:  token,
				})
			}
		}(i)
	}
}

func launchReaders(store *RacySessionStore, wg *sync.WaitGroup) {
	for i := 0; i < concurrentReaders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < operationsPerWorker; j++ {
				token := fmt.Sprintf("token-%d", j%prePopulatedSessions)
				_ = store.Get(token) // FATAL: concurrent read + write
			}
		}()
	}
}

func main() {
	fmt.Println("=== Concurrent Map Read + Write Crash ===")
	fmt.Println("This WILL crash with: fatal error: concurrent map read and map write")
	fmt.Println()

	store := NewRacySessionStore()
	prePopulate(store, prePopulatedSessions)

	var wg sync.WaitGroup
	launchWriters(store, &wg)
	launchReaders(store, &wg)
	wg.Wait()

	fmt.Printf("Sessions: %d\n", store.Count())
}
```

### Verification
```bash
go run main.go
```
Expected:
```
fatal error: concurrent map read and map write
```

In a real server, this crash happens when the authentication middleware reads the session map while a login handler writes to it. The server goes down without warning.

## Step 3 -- Fix with sync.RWMutex

For a session store, reads vastly outnumber writes. `sync.RWMutex` allows multiple concurrent readers while writers get exclusive access:

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

type Session struct {
	UserID    string
	Token     string
	ExpiresAt time.Time
}

type SessionStore struct {
	mu       sync.RWMutex
	sessions map[string]Session
}

func NewSessionStore() *SessionStore {
	return &SessionStore{
		sessions: make(map[string]Session),
	}
}

func (s *SessionStore) Create(token string, session Session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[token] = session
}

func (s *SessionStore) Get(token string) (Session, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess, ok := s.sessions[token]
	return sess, ok
}

func (s *SessionStore) Delete(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, token)
}

func (s *SessionStore) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.sessions)
}

func (s *SessionStore) CleanExpired() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	cleaned := 0
	now := time.Now()
	for token, sess := range s.sessions {
		if now.After(sess.ExpiresAt) {
			delete(s.sessions, token)
			cleaned++
		}
	}
	return cleaned
}

func main() {
	store := NewSessionStore()
	var wg sync.WaitGroup

	fmt.Println("=== Thread-Safe Session Store ===")

	// Writers: login handlers creating sessions.
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(handlerID int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				token := fmt.Sprintf("token-%d-%d", handlerID, j)
				store.Create(token, Session{
					UserID:    fmt.Sprintf("user-%d", handlerID),
					Token:     token,
					ExpiresAt: time.Now().Add(1 * time.Hour),
				})
			}
		}(i)
	}

	// Readers: auth middleware checking sessions.
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(readerID int) {
			defer wg.Done()
			found := 0
			for j := 0; j < 200; j++ {
				token := fmt.Sprintf("token-%d-%d", readerID%10, j%100)
				if _, ok := store.Get(token); ok {
					found++
				}
			}
		}(i)
	}

	wg.Wait()

	fmt.Printf("  Total sessions: %d (expected 1000)\n", store.Count())
	fmt.Println()
	fmt.Println("Why RWMutex for session stores:")
	fmt.Println("  - Auth middleware checks sessions on EVERY request (reads)")
	fmt.Println("  - Login/logout create/delete sessions occasionally (writes)")
	fmt.Println("  - Ratio is ~100:1 reads to writes")
	fmt.Println("  - RWMutex allows all readers to proceed simultaneously")
	fmt.Println("  - Only writers need exclusive access")
}
```

Key design points:
- `RLock()`/`RUnlock()` for read operations: multiple readers proceed simultaneously
- `Lock()`/`Unlock()` for write operations: exclusive access, blocks all readers and writers
- ALL map operations are protected, including `len()`, `delete()`, and iteration
- `CleanExpired()` takes a write lock for the entire cleanup operation because it modifies and iterates the map

### Verification
```bash
go run -race main.go
```
Expected: 1000 sessions, zero race warnings, no crashes.

## Step 4 -- Demonstrate the Read Concurrency Advantage

Show that `RWMutex` allows parallel reads while `Mutex` serializes everything:

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

const (
	preloadEntries    = 1000
	writesPerWriter   = 100
	readsPerReader    = 1000
	benchReaders      = 50
	benchWriters      = 2
)

// LockBenchResult holds the timing outcome of a lock strategy benchmark.
type LockBenchResult struct {
	Label   string
	Elapsed time.Duration
}

// MutexMapBench benchmarks sync.Mutex on a read-heavy map workload.
type MutexMapBench struct {
	mu sync.Mutex
	m  map[int]int
}

func NewMutexMapBench() *MutexMapBench {
	m := make(map[int]int, preloadEntries)
	for i := 0; i < preloadEntries; i++ {
		m[i] = i
	}
	return &MutexMapBench{m: m}
}

func (b *MutexMapBench) Run(readers, writers int) LockBenchResult {
	var wg sync.WaitGroup
	start := time.Now()

	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < writesPerWriter; j++ {
				b.mu.Lock()
				b.m[preloadEntries+id*writesPerWriter+j] = j
				b.mu.Unlock()
			}
		}(w)
	}

	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < readsPerReader; j++ {
				b.mu.Lock()
				_ = b.m[j%preloadEntries]
				b.mu.Unlock()
			}
		}()
	}

	wg.Wait()
	return LockBenchResult{"Mutex", time.Since(start)}
}

// RWMutexMapBench benchmarks sync.RWMutex on a read-heavy map workload.
type RWMutexMapBench struct {
	mu sync.RWMutex
	m  map[int]int
}

func NewRWMutexMapBench() *RWMutexMapBench {
	m := make(map[int]int, preloadEntries)
	for i := 0; i < preloadEntries; i++ {
		m[i] = i
	}
	return &RWMutexMapBench{m: m}
}

func (b *RWMutexMapBench) Run(readers, writers int) LockBenchResult {
	var wg sync.WaitGroup
	start := time.Now()

	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < writesPerWriter; j++ {
				b.mu.Lock()
				b.m[preloadEntries+id*writesPerWriter+j] = j
				b.mu.Unlock()
			}
		}(w)
	}

	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < readsPerReader; j++ {
				b.mu.RLock()
				_ = b.m[j%preloadEntries]
				b.mu.RUnlock()
			}
		}()
	}

	wg.Wait()
	return LockBenchResult{"RWMutex", time.Since(start)}
}

func printBenchComparison(mutex, rwMutex LockBenchResult) {
	fmt.Printf("  %-10s %v\n", mutex.Label+":", mutex.Elapsed)
	fmt.Printf("  %-10s %v\n", rwMutex.Label+":", rwMutex.Elapsed)
	fmt.Println()
	fmt.Println("RWMutex wins because 50 readers proceed in parallel,")
	fmt.Println("while Mutex forces all 50 to take turns.")
}

func main() {
	fmt.Printf("=== Read-Heavy Workload: %d readers, %d writers ===\n\n",
		benchReaders, benchWriters)

	mutexBench := NewMutexMapBench()
	mutexResult := mutexBench.Run(benchReaders, benchWriters)

	rwBench := NewRWMutexMapBench()
	rwResult := rwBench.Run(benchReaders, benchWriters)

	printBenchComparison(mutexResult, rwResult)
}
```

### Verification
```bash
go run main.go
```

With 50 readers and 2 writers, `RWMutex` allows all readers to proceed simultaneously. `Mutex` serializes all 50 readers, making them take turns.

## Common Mistakes

### Thinking "I Only Write to Different Keys"
**Wrong assumption:** "Each goroutine writes to different keys, so there is no conflict."
**Reality:** The map's internal hash table is shared. Even if goroutines use different keys, internal bucket restructuring during growth affects the entire map. Any concurrent write triggers the fatal error.

### Forgetting to Protect Map Reads
```go
mu.Lock()
m[key] = value
mu.Unlock()
// ...
val := m[key] // BUG: read without lock -- FATAL ERROR
```
**Fix:** Protect ALL map operations (read, write, delete, range) with the same mutex.

### Using Mutex When RWMutex Would Help
For session stores, caches, and configuration maps where reads dominate, `sync.Mutex` forces unnecessary serialization of readers. Use `sync.RWMutex` when the read-to-write ratio is high (10:1 or more).

### Using RWMutex When Writes Are Frequent
`sync.RWMutex` has higher overhead per operation than `sync.Mutex` due to writer starvation prevention. If writes are frequent (more than 10% of operations), `sync.Mutex` is simpler and often faster.

## Review

Concurrent map access is a fatal error, not a wrong answer. Both write+write and read+write trip it, and it is a hard crash the runtime raises on its own -- a separate mechanism from the `-race` detector -- that kills the process immediately with no recovery, no graceful shutdown, and no handler. The fix for a read-heavy store is `sync.RWMutex`: readers share the lock and proceed in parallel while a writer takes it exclusively, and every map touch must go through it -- reads, writes, deletes, and iteration alike -- because a single unguarded operation is enough to crash. Writing to different keys does not save you either: the map's hash table is one shared structure, and a growth-triggered rehash restructures buckets across all keys, so any concurrent write is unsafe regardless of which key it targets. The default stays `sync.Mutex`; you upgrade to `RWMutex` only when reads vastly outnumber writes, since its extra per-operation bookkeeping costs more once writes become frequent.

You should be able to answer the questions the exercise raises without rereading. Why does Go crash on concurrent map access rather than hand back garbage -- what memory-safety and hash-traversal hazards is the runtime refusing to risk? When would you reach for `RWMutex` over `Mutex`, and when would that same choice actually be slower? And does confining each goroutine to its own keys make a plain map safe to share? Run the fixed store under `go run -race main.go`, confirm a thousand sessions and zero warnings, and you have the template for every session store, cache, and reloadable config map you will build.

## Resources

- [Go Blog: Go Maps in Action](https://go.dev/blog/maps) -- how maps work internally, including the bucket growth that makes different-key writes unsafe.
- [sync.RWMutex Documentation](https://pkg.go.dev/sync#RWMutex) -- the read/write lock the fixed store uses, with the RLock/Lock semantics.
- [Go FAQ: Why are map operations not defined to be atomic?](https://go.dev/doc/faq#atomic_maps) -- the language designers' rationale for crashing rather than serializing map access.

---

Back to [Concurrency](../../concurrency.md) | Next: [07-race-in-closure-loops](../07-race-in-closure-loops/07-race-in-closure-loops.md)
