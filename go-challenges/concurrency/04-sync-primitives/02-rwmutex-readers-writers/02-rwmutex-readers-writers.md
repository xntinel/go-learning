# Exercise 2: RWMutex: Readers-Writers

A regular `sync.Mutex` serializes every access, even when a goroutine only
wants to read. For read-heavy shared state that is wasteful: think of a
configuration store where dozens of goroutines constantly check feature flags
and timeouts while a reload happens maybe once every few minutes. Forcing all
those readers to queue behind one another adds latency to every request for no
reason. `sync.RWMutex` fixes it by splitting the lock in two -- many readers
can hold it at once, but a writer takes it exclusively -- and this exercise
builds a config store to show exactly when that trade pays off.

## What you'll build

```text
02-rwmutex-readers-writers/
  main.go        RWMutex-protected ConfigStore plus a Mutex-vs-RWMutex
                 benchmark (each step is its own runnable program)
```

- Build: a read-heavy `ConfigStore` protected by `sync.RWMutex`, and a Mutex-vs-RWMutex benchmark.
- Implement: `Get`/`IsFeatureEnabled`/`Snapshot` under `RLock`, `Set`/`Reload` under `Lock`, and `benchMutex`/`benchRWMutex`.
- Verify: `go run main.go` (each step is its own runnable program).

### Why splitting reads from writes pays off

`sync.RWMutex` solves this with two levels of locking:
- **Read lock** (`RLock`): multiple goroutines can hold a read lock simultaneously. They only block if a writer holds the exclusive lock.
- **Write lock** (`Lock`): only one goroutine can hold the write lock. It blocks until all readers release their read locks, and no new readers can acquire a read lock while a writer is waiting.

This makes `RWMutex` ideal for data structures that are read far more often than they are written -- configuration stores, feature flag systems, routing tables, and similar shared state.

## Step 1 -- Build a Configuration Store

A configuration store holds application settings that many goroutines read but only an admin endpoint or config watcher updates:

```go
package main

import (
	"fmt"
	"sync"
)

const featureKeyPrefix = "feature."

type ConfigStore struct {
	mu       sync.RWMutex
	settings map[string]string
}

func NewConfigStore() *ConfigStore {
	return &ConfigStore{settings: make(map[string]string)}
}

func (cs *ConfigStore) Get(key string) (string, bool) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	val, ok := cs.settings[key]
	return val, ok
}

func (cs *ConfigStore) Set(key, value string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.settings[key] = value
}

func (cs *ConfigStore) IsFeatureEnabled(feature string) bool {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.settings[featureKeyPrefix+feature] == "true"
}

func (cs *ConfigStore) Reload(newConfig map[string]string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.settings = make(map[string]string, len(newConfig))
	for k, v := range newConfig {
		cs.settings[k] = v
	}
}

// Snapshot returns a COPY to avoid exposing internal state.
func (cs *ConfigStore) Snapshot() map[string]string {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	result := make(map[string]string, len(cs.settings))
	for k, v := range cs.settings {
		result[k] = v
	}
	return result
}

func loadInitialConfig(store *ConfigStore) {
	store.Set("db.host", "postgres.internal:5432")
	store.Set("db.pool_size", "25")
	store.Set("feature.dark_mode", "true")
	store.Set("feature.beta_api", "false")
	store.Set("http.timeout", "30s")
}

func printConfigSummary(store *ConfigStore) {
	host, _ := store.Get("db.host")
	fmt.Printf("db.host = %s\n", host)
	fmt.Printf("dark_mode enabled: %v\n", store.IsFeatureEnabled("dark_mode"))
	fmt.Printf("beta_api enabled: %v\n", store.IsFeatureEnabled("beta_api"))

	snap := store.Snapshot()
	fmt.Printf("\nAll settings (%d entries):\n", len(snap))
	for key, value := range snap {
		fmt.Printf("  %s = %s\n", key, value)
	}
}

func main() {
	store := NewConfigStore()
	loadInitialConfig(store)
	printConfigSummary(store)
}
```

Expected output:
```
db.host = postgres.internal:5432
dark_mode enabled: true
beta_api enabled: false

All settings (5 entries):
  db.host = postgres.internal:5432
  db.pool_size = 25
  feature.dark_mode = true
  feature.beta_api = false
  http.timeout = 30s
```

### Verification
```bash
go run main.go
```
The basic operations test should print correct settings and feature flag states.

## Step 2 -- Demonstrate Concurrent Readers

In production, many goroutines check feature flags simultaneously. With `RLock`, they all proceed in parallel instead of waiting for each other. To show the difference, this example holds the read lock for the full duration of simulated work. With a regular `Mutex`, this would serialize all 10 handlers; with `RWMutex`, they run concurrently:

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

const (
	concurrentReaders   = 10
	simulatedWorkDelay  = 100 * time.Millisecond
)

type ConfigStore struct {
	mu       sync.RWMutex
	settings map[string]string
}

func NewConfigStore() *ConfigStore {
	return &ConfigStore{settings: make(map[string]string)}
}

// GetAndProcess simulates reading config and doing work while holding the read lock.
// In real code, you would release the lock sooner. This exaggerates the effect to
// demonstrate that multiple readers can hold an RLock simultaneously.
func (cs *ConfigStore) GetAndProcess(key string) string {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	val := cs.settings[key]
	time.Sleep(simulatedWorkDelay)
	return val
}

func (cs *ConfigStore) Set(key, value string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.settings[key] = value
}

func demonstrateConcurrentReaders(store *ConfigStore) time.Duration {
	var wg sync.WaitGroup
	start := time.Now()

	for i := 0; i < concurrentReaders; i++ {
		wg.Add(1)
		go func(handlerID int) {
			defer wg.Done()
			val := store.GetAndProcess("feature.dark_mode")
			fmt.Printf("Handler %d: dark_mode=%s (at %v)\n", handlerID, val, time.Since(start).Round(time.Millisecond))
		}(i)
	}

	wg.Wait()
	return time.Since(start)
}

func main() {
	store := NewConfigStore()
	store.Set("feature.dark_mode", "true")

	elapsed := demonstrateConcurrentReaders(store)

	fmt.Printf("\nAll %d handlers finished in %v\n", concurrentReaders, elapsed.Round(time.Millisecond))
	fmt.Println("(With a regular Mutex, this would take ~1s because only one goroutine holds the lock at a time.)")
	fmt.Println("(With RWMutex, ~100ms because all readers hold RLock simultaneously.)")
}
```

Expected output:
```
Handler 0: dark_mode=true (at 100ms)
Handler 1: dark_mode=true (at 100ms)
...
All 10 handlers finished in 100ms
(With a regular Mutex, this would take ~1s because only one goroutine holds the lock at a time.)
(With RWMutex, ~100ms because all readers hold RLock simultaneously.)
```

### Verification
```bash
go run main.go
```
All 10 handlers should finish in approximately 100ms, proving they ran concurrently. With a regular `Mutex` and the lock held during the sleep, this would take ~1s because each handler would wait for the previous one to release the lock.

## Step 3 -- Config Reload Blocks All Readers

When the configuration watcher reloads settings, it acquires the write lock. All readers block until the reload completes, ensuring they never see a partially updated config:

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

const (
	readerCount         = 3
	writerAcquireDelay  = 10 * time.Millisecond
	reloadSimDelay      = 200 * time.Millisecond
)

type ConfigStore struct {
	mu       sync.RWMutex
	settings map[string]string
}

func NewConfigStore(initial map[string]string) *ConfigStore {
	settings := make(map[string]string, len(initial))
	for k, v := range initial {
		settings[k] = v
	}
	return &ConfigStore{settings: settings}
}

func (cs *ConfigStore) Get(key string) (string, bool) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	val, ok := cs.settings[key]
	return val, ok
}

func (cs *ConfigStore) Reload(newConfig map[string]string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	fmt.Printf("[%v] Config reload: acquired write lock, updating %d settings...\n",
		time.Now().Format("15:04:05.000"), len(newConfig))
	time.Sleep(reloadSimDelay)
	cs.settings = make(map[string]string, len(newConfig))
	for k, v := range newConfig {
		cs.settings[k] = v
	}
	fmt.Printf("[%v] Config reload: complete\n", time.Now().Format("15:04:05.000"))
}

func startConfigReload(store *ConfigStore, wg *sync.WaitGroup, newConfig map[string]string) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		store.Reload(newConfig)
	}()
}

func startBlockedReaders(store *ConfigStore, wg *sync.WaitGroup, start time.Time) {
	for i := 0; i < readerCount; i++ {
		wg.Add(1)
		go func(handlerID int) {
			defer wg.Done()
			fmt.Printf("[%v] Handler %d: waiting for read lock...\n", time.Since(start).Round(time.Millisecond), handlerID)
			val, _ := store.Get("db.host")
			fmt.Printf("[%v] Handler %d: db.host=%s\n", time.Since(start).Round(time.Millisecond), handlerID, val)
		}(i)
	}
}

func main() {
	store := NewConfigStore(map[string]string{
		"db.host": "old-host:5432",
	})
	var wg sync.WaitGroup
	start := time.Now()

	newConfig := map[string]string{
		"db.host": "new-host:5432",
		"db.pool": "50",
	}
	startConfigReload(store, &wg, newConfig)

	time.Sleep(writerAcquireDelay) // let writer acquire lock first

	startBlockedReaders(store, &wg, start)

	wg.Wait()
	fmt.Println("\nReaders saw the NEW config because they waited for the reload to finish.")
}
```

Expected output:
```
Config reload: acquired write lock, updating 2 settings...
[10ms] Handler 0: waiting for read lock...
[10ms] Handler 1: waiting for read lock...
[10ms] Handler 2: waiting for read lock...
Config reload: complete
[210ms] Handler 0: db.host=new-host:5432
[210ms] Handler 1: db.host=new-host:5432
[210ms] Handler 2: db.host=new-host:5432

Readers saw the NEW config because they waited for the reload to finish.
```

### Verification
```bash
go run main.go
```
Readers should start waiting around 10ms and only succeed after ~200ms when the reload finishes.

## Step 4 -- Performance Comparison: Mutex vs RWMutex

The real payoff of RWMutex shows in benchmarks. This program simulates a read-heavy workload (feature flag checks) and compares both approaches:

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

const (
	benchReaders       = 100
	benchWriters       = 2
	opsPerGoroutine    = 10000
	writeRatio         = 10 // writers do ops/writeRatio operations
)

func runReadWriteWorkload(readFn, writeFn func(), readers, writers, readOps, writeOps int) time.Duration {
	var wg sync.WaitGroup
	start := time.Now()

	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < readOps; j++ {
				readFn()
			}
		}()
	}

	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < writeOps; j++ {
				writeFn()
			}
		}()
	}

	wg.Wait()
	return time.Since(start)
}

func benchMutex(readers, writers, ops int) time.Duration {
	var mu sync.Mutex
	config := map[string]string{"feature.dark_mode": "true"}

	readFn := func() { mu.Lock(); _ = config["feature.dark_mode"]; mu.Unlock() }
	writeFn := func() { mu.Lock(); config["feature.dark_mode"] = "true"; mu.Unlock() }

	return runReadWriteWorkload(readFn, writeFn, readers, writers, ops, ops/writeRatio)
}

func benchRWMutex(readers, writers, ops int) time.Duration {
	var mu sync.RWMutex
	config := map[string]string{"feature.dark_mode": "true"}

	readFn := func() { mu.RLock(); _ = config["feature.dark_mode"]; mu.RUnlock() }
	writeFn := func() { mu.Lock(); config["feature.dark_mode"] = "true"; mu.Unlock() }

	return runReadWriteWorkload(readFn, writeFn, readers, writers, ops, ops/writeRatio)
}

func printBenchResults(mutexTime, rwTime time.Duration) {
	fmt.Printf("Mutex:   %v\n", mutexTime.Round(time.Millisecond))
	fmt.Printf("RWMutex: %v\n", rwTime.Round(time.Millisecond))

	if rwTime < mutexTime {
		speedup := float64(mutexTime) / float64(rwTime)
		fmt.Printf("\nRWMutex is %.1fx faster for this read-heavy config workload.\n", speedup)
	} else {
		fmt.Println("\nMutex was faster (this can happen on machines with few cores).")
	}

	fmt.Println("\nRule of thumb: use RWMutex when reads outnumber writes by 10:1 or more.")
}

func main() {
	fmt.Printf("Benchmark: %d readers, %d writers, %d ops/reader\n\n", benchReaders, benchWriters, opsPerGoroutine)

	mutexTime := benchMutex(benchReaders, benchWriters, opsPerGoroutine)
	rwTime := benchRWMutex(benchReaders, benchWriters, opsPerGoroutine)

	printBenchResults(mutexTime, rwTime)
}
```

Expected output (times vary by machine):
```
Benchmark: 100 readers, 2 writers, 10000 ops/reader

Mutex:   45ms
RWMutex: 15ms

RWMutex is 3.0x faster for this read-heavy config workload.

Rule of thumb: use RWMutex when reads outnumber writes by 10:1 or more.
```

### Verification
```bash
go run main.go
```
RWMutex should be noticeably faster for the read-heavy workload.

## Common Mistakes

### Using Lock When RLock Suffices

```go
func (cs *ConfigStore) Get(key string) string {
	cs.mu.Lock() // WRONG: exclusive lock for a read-only operation
	defer cs.mu.Unlock()
	return cs.settings[key]
}
```

**What happens:** Readers serialize unnecessarily, losing the concurrency benefit of RWMutex. You have essentially turned your RWMutex into a regular Mutex. Every feature flag check blocks every other handler.

**Fix:** Use `RLock/RUnlock` for read-only operations.

### Upgrading RLock to Lock

```go
package main

import (
	"fmt"
	"sync"
)

func main() {
	var mu sync.RWMutex
	config := map[string]string{"db.host": ""}

	mu.RLock()
	val := config["db.host"]
	if val == "" {
		mu.Lock() // DEADLOCK: cannot upgrade RLock to Lock
		config["db.host"] = "localhost:5432"
		mu.Unlock()
	}
	mu.RUnlock()
	fmt.Println("This line is never reached")
}
```

**What happens:** Deadlock. You cannot acquire a write lock while holding a read lock. The write lock waits for all read locks to release, but this goroutine holds a read lock that will never release because it is waiting for the write lock.

**Fix:** Release the read lock first, then acquire the write lock with a double-check:
```go
mu.RLock()
val := config["db.host"]
mu.RUnlock()

if val == "" {
	mu.Lock()
	// Double-check after acquiring write lock -- another goroutine may have set it
	if config["db.host"] == "" {
		config["db.host"] = "localhost:5432"
	}
	mu.Unlock()
}
```

### RWMutex for Write-Heavy Workloads
Using `RWMutex` when writes are frequent provides no benefit over `Mutex` and adds overhead. RWMutex tracks reader counts internally, which costs more than a simple Mutex when there are few or no concurrent readers. `RWMutex` shines only when reads vastly outnumber writes -- like a config store, not a write log.

## Review

An `RWMutex` is two locks sharing one guard: `RLock` admits any number of
readers at once, while `Lock` is exclusive and blocks until every reader has
released. That is the whole benefit -- a config store read by dozens of
handlers and written a few times a minute stops serializing its readers, which
is where the speedup in the benchmark comes from. The cost side is just as
sharp: reader bookkeeping makes an `RWMutex` slower than a plain `Mutex` when
writes are frequent, so it earns its keep only when reads vastly outnumber
writes. Two invariants keep it correct. You cannot upgrade an `RLock` to a
`Lock` while holding it -- the write lock waits for all readers, you among
them, so you deadlock against yourself; release the read lock first, then take
the write lock and double-check. And any method that reads under `RLock` should
hand back a copy, never the internal map, or a caller mutates shared state
entirely outside the lock.

You should be able to prove the trade to yourself. Build a feature-flag service
with `IsEnabled(flag)` and `SetFlag(flag, enabled)` over a `ConfigStore`, then
run it two ways from a hundred goroutines: ninety percent reads and ten percent
writes, then an even fifty-fifty split. The read-heavy run should put `RWMutex`
clearly ahead of a plain `Mutex`; the balanced run should erase that lead or
reverse it, because now the reader-count bookkeeping costs more than it saves.
That crossover is the rule of thumb -- roughly ten reads per write -- made
concrete.

## Resources
- [sync.RWMutex documentation](https://pkg.go.dev/sync#RWMutex) -- the RLock/RUnlock/Lock/Unlock API and its exact blocking rules.
- [Go Blog: Share Memory by Communicating](https://go.dev/blog/codelab-share) -- the channels-vs-locks framing for deciding when a mutex is even the right tool.
- [Bryan Mills - Rethinking Classical Concurrency Patterns (GopherCon 2018)](https://www.youtube.com/watch?v=5zXAHh5tJqQ) -- a critical look at when RWMutex and condition variables help versus hurt.

---

Back to [Concurrency](../../concurrency.md) | Next: [03-waitgroup-wait-for-all](../03-waitgroup-wait-for-all/03-waitgroup-wait-for-all.md)
