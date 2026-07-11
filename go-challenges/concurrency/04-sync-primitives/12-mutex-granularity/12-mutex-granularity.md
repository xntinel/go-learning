# Exercise 12: Mutex Granularity

A mutex serializes access to shared data. If all fields of a struct share one
mutex, updating the user's avatar blocks reading the user's settings -- even
though those operations are completely independent. Under high concurrency that
unnecessary serialization becomes the bottleneck, and the fix is to shrink what
each lock covers. But finer locks are not free: they cost code, memory, and
fresh deadlock risk if two are ever taken in different orders. This exercise
walks the whole spectrum -- one big mutex, per-field mutexes, sharded locks --
and measures the payoff at each step.

## What you'll build

```text
12-mutex-granularity/
  main.go        benchmarks across the locking spectrum: one big mutex,
                 per-field RWMutex, and an FNV-sharded map under load
```

- Build: benchmarks that compare coarse, per-field, and sharded locking under identical concurrent load.
- Implement: `CoarseProfile` (a single mutex over all fields), `FineProfile` (a per-field `RWMutex`), and `ShardedMap` (keys distributed across FNV-hashed shards, each with its own lock), plus the benchmark harnesses that drive them.
- Verify: `go run -race main.go` for each step.

### Why granularity trades contention for complexity

Consider a user profile service handling 10,000 requests per second. Each request
touches one field: bio, avatar URL, notification preferences, or theme settings.
With one big mutex, all 10,000 requests compete for the same lock. With per-field
mutexes, requests that touch different fields proceed in parallel, and only
requests touching the same field wait for each other.

The tradeoff is complexity. More mutexes mean more code, more potential for
deadlocks (if you ever lock two mutexes in different orders), and more memory
overhead. The right granularity depends on the contention profile of your system.

The principle is: **lock the data, not the function.** A mutex should protect the
smallest unit of shared state, not an entire method or struct. This exercise
demonstrates the spectrum from one lock to many and measures the performance
impact.

## Step 1 -- One Big Mutex (Simple, High Contention)

Build a user profile service with a single mutex protecting all fields. Simple to implement, but every operation serializes against every other operation.

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

type CoarseProfile struct {
	mu            sync.Mutex
	bio           string
	avatarURL     string
	theme         string
	notifications bool
}

func NewCoarseProfile() *CoarseProfile {
	return &CoarseProfile{
		bio:           "default bio",
		avatarURL:     "default.png",
		theme:         "light",
		notifications: true,
	}
}

func (p *CoarseProfile) SetBio(bio string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.bio = bio
}

func (p *CoarseProfile) Bio() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.bio
}

func (p *CoarseProfile) SetAvatarURL(url string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.avatarURL = url
}

func (p *CoarseProfile) AvatarURL() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.avatarURL
}

func (p *CoarseProfile) SetTheme(theme string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.theme = theme
}

func (p *CoarseProfile) Theme() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.theme
}

func (p *CoarseProfile) SetNotifications(enabled bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.notifications = enabled
}

func (p *CoarseProfile) Notifications() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.notifications
}

func benchmarkCoarse(goroutines, opsPerG int) time.Duration {
	profile := NewCoarseProfile()
	var wg sync.WaitGroup
	start := time.Now()

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < opsPerG; j++ {
				switch j % 4 {
				case 0:
					profile.SetBio(fmt.Sprintf("bio-%d-%d", id, j))
				case 1:
					profile.SetAvatarURL(fmt.Sprintf("avatar-%d.png", id))
				case 2:
					_ = profile.Theme()
				case 3:
					_ = profile.Notifications()
				}
			}
		}(i)
	}

	wg.Wait()
	return time.Since(start)
}

func main() {
	const goroutines = 100
	const opsPerG = 5000

	fmt.Printf("=== Coarse-Grained Lock (%d goroutines x %d ops) ===\n", goroutines, opsPerG)
	elapsed := benchmarkCoarse(goroutines, opsPerG)
	fmt.Printf("  single mutex for all fields: %v\n", elapsed.Round(time.Millisecond))
	fmt.Println("  pros: simple, impossible to deadlock (one lock)")
	fmt.Println("  cons: updating bio blocks reading theme")
}
```

### Verification
```
=== Coarse-Grained Lock (100 goroutines x 5000 ops) ===
  single mutex for all fields: 45ms
  pros: simple, impossible to deadlock (one lock)
  cons: updating bio blocks reading theme
```
All operations serialize through one lock. Simple but slow under contention.

## Step 2 -- Per-Field Mutexes (Better Concurrency)

Replace the single mutex with one mutex per field. Operations on different fields proceed in parallel.

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

type FineProfile struct {
	bioMu    sync.RWMutex
	bio      string

	avatarMu sync.RWMutex
	avatarURL string

	themeMu  sync.RWMutex
	theme    string

	notifMu  sync.RWMutex
	notifications bool
}

func NewFineProfile() *FineProfile {
	return &FineProfile{
		bio:           "default bio",
		avatarURL:     "default.png",
		theme:         "light",
		notifications: true,
	}
}

func (p *FineProfile) SetBio(bio string) {
	p.bioMu.Lock()
	defer p.bioMu.Unlock()
	p.bio = bio
}

func (p *FineProfile) Bio() string {
	p.bioMu.RLock()
	defer p.bioMu.RUnlock()
	return p.bio
}

func (p *FineProfile) SetAvatarURL(url string) {
	p.avatarMu.Lock()
	defer p.avatarMu.Unlock()
	p.avatarURL = url
}

func (p *FineProfile) AvatarURL() string {
	p.avatarMu.RLock()
	defer p.avatarMu.RUnlock()
	return p.avatarURL
}

func (p *FineProfile) SetTheme(theme string) {
	p.themeMu.Lock()
	defer p.themeMu.Unlock()
	p.theme = theme
}

func (p *FineProfile) Theme() string {
	p.themeMu.RLock()
	defer p.themeMu.RUnlock()
	return p.theme
}

func (p *FineProfile) SetNotifications(enabled bool) {
	p.notifMu.Lock()
	defer p.notifMu.Unlock()
	p.notifications = enabled
}

func (p *FineProfile) Notifications() bool {
	p.notifMu.RLock()
	defer p.notifMu.RUnlock()
	return p.notifications
}

func benchmarkFine(goroutines, opsPerG int) time.Duration {
	profile := NewFineProfile()
	var wg sync.WaitGroup
	start := time.Now()

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < opsPerG; j++ {
				switch j % 4 {
				case 0:
					profile.SetBio(fmt.Sprintf("bio-%d-%d", id, j))
				case 1:
					profile.SetAvatarURL(fmt.Sprintf("avatar-%d.png", id))
				case 2:
					_ = profile.Theme()
				case 3:
					_ = profile.Notifications()
				}
			}
		}(i)
	}

	wg.Wait()
	return time.Since(start)
}

func main() {
	const goroutines = 100
	const opsPerG = 5000

	fmt.Printf("=== Fine-Grained Locks (%d goroutines x %d ops) ===\n", goroutines, opsPerG)
	elapsed := benchmarkFine(goroutines, opsPerG)
	fmt.Printf("  per-field RWMutex: %v\n", elapsed.Round(time.Millisecond))
	fmt.Println("  pros: operations on different fields run in parallel")
	fmt.Println("  cons: more mutexes, more memory, deadlock risk if locking multiple fields")
	fmt.Println("  rule: NEVER lock two field mutexes in the same operation without a fixed order")
}
```

### Verification
```
=== Fine-Grained Locks (100 goroutines x 5000 ops) ===
  per-field RWMutex: 28ms
  pros: operations on different fields run in parallel
  cons: more mutexes, more memory, deadlock risk if locking multiple fields
  rule: NEVER lock two field mutexes in the same operation without a fixed order
```
Faster than the single mutex because operations on different fields do not contend.

## Step 3 -- Sharded Locks for Large Maps

For maps with many keys accessed by many goroutines, a single mutex creates a bottleneck. Sharding splits the map into N segments, each with its own lock. Only goroutines accessing keys in the same shard contend.

```go
package main

import (
	"fmt"
	"hash/fnv"
	"sync"
	"time"
)

type SingleLockMap struct {
	mu   sync.RWMutex
	data map[string]string
}

func NewSingleLockMap() *SingleLockMap {
	return &SingleLockMap{data: make(map[string]string)}
}

func (m *SingleLockMap) Set(key, value string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[key] = value
}

func (m *SingleLockMap) Get(key string) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	v, ok := m.data[key]
	return v, ok
}

type ShardedMap struct {
	shards    []shard
	shardMask uint32
}

type shard struct {
	mu   sync.RWMutex
	data map[string]string
}

func NewShardedMap(shardCount int) *ShardedMap {
	// Round up to next power of 2 for efficient masking.
	n := 1
	for n < shardCount {
		n *= 2
	}

	shards := make([]shard, n)
	for i := range shards {
		shards[i].data = make(map[string]string)
	}

	return &ShardedMap{
		shards:    shards,
		shardMask: uint32(n - 1),
	}
}

func (sm *ShardedMap) shardIndex(key string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(key))
	return h.Sum32() & sm.shardMask
}

func (sm *ShardedMap) Set(key, value string) {
	idx := sm.shardIndex(key)
	sm.shards[idx].mu.Lock()
	defer sm.shards[idx].mu.Unlock()
	sm.shards[idx].data[key] = value
}

func (sm *ShardedMap) Get(key string) (string, bool) {
	idx := sm.shardIndex(key)
	sm.shards[idx].mu.RLock()
	defer sm.shards[idx].mu.RUnlock()
	v, ok := sm.shards[idx].data[key]
	return v, ok
}

func (sm *ShardedMap) Len() int {
	total := 0
	for i := range sm.shards {
		sm.shards[i].mu.RLock()
		total += len(sm.shards[i].data)
		sm.shards[i].mu.RUnlock()
	}
	return total
}

func benchmarkSingleLock(goroutines, opsPerG int) time.Duration {
	m := NewSingleLockMap()
	var wg sync.WaitGroup
	start := time.Now()

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < opsPerG; j++ {
				key := fmt.Sprintf("user:%d:%d", id, j%100)
				if j%3 == 0 {
					m.Set(key, fmt.Sprintf("value-%d", j))
				} else {
					m.Get(key)
				}
			}
		}(i)
	}

	wg.Wait()
	return time.Since(start)
}

func benchmarkSharded(goroutines, opsPerG, shardCount int) time.Duration {
	m := NewShardedMap(shardCount)
	var wg sync.WaitGroup
	start := time.Now()

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < opsPerG; j++ {
				key := fmt.Sprintf("user:%d:%d", id, j%100)
				if j%3 == 0 {
					m.Set(key, fmt.Sprintf("value-%d", j))
				} else {
					m.Get(key)
				}
			}
		}(i)
	}

	wg.Wait()
	return time.Since(start)
}

func main() {
	const goroutines = 100
	const opsPerG = 5000

	fmt.Printf("=== Map Lock Benchmark (%d goroutines x %d ops) ===\n\n", goroutines, opsPerG)

	singleTime := benchmarkSingleLock(goroutines, opsPerG)
	fmt.Printf("  single RWMutex:    %v\n", singleTime.Round(time.Millisecond))

	shard4Time := benchmarkSharded(goroutines, opsPerG, 4)
	fmt.Printf("  sharded (4):       %v\n", shard4Time.Round(time.Millisecond))

	shard16Time := benchmarkSharded(goroutines, opsPerG, 16)
	fmt.Printf("  sharded (16):      %v\n", shard16Time.Round(time.Millisecond))

	shard64Time := benchmarkSharded(goroutines, opsPerG, 64)
	fmt.Printf("  sharded (64):      %v\n", shard64Time.Round(time.Millisecond))

	fmt.Println("\n=== Analysis ===")
	fmt.Println("  More shards = less contention = faster under high concurrency.")
	fmt.Println("  Diminishing returns after shard count exceeds goroutine count.")
	fmt.Println("  Each shard has its own lock, so different keys in different shards")
	fmt.Println("  are accessed in parallel.")
}
```

The sharded map distributes keys across N internal maps using FNV hash. Each shard has its own `RWMutex`. With 16 shards and 100 goroutines, contention drops roughly 16x compared to a single lock because goroutines accessing different shards never wait for each other.

### Verification
```
=== Map Lock Benchmark (100 goroutines x 5000 ops) ===

  single RWMutex:    85ms
  sharded (4):       40ms
  sharded (16):      22ms
  sharded (64):      18ms

=== Analysis ===
  More shards = less contention = faster under high concurrency.
  Diminishing returns after shard count exceeds goroutine count.
  Each shard has its own lock, so different keys in different shards
  are accessed in parallel.
```
Sharding consistently outperforms a single lock. Times vary by machine.

## Step 4 -- Full Comparison and Decision Framework

Combine all approaches into one program that benchmarks under identical conditions and prints a decision matrix.

```go
package main

import (
	"fmt"
	"hash/fnv"
	"sync"
	"time"
)

// --- Coarse: single mutex for all fields ---

type CoarseProfile struct {
	mu            sync.Mutex
	bio           string
	avatarURL     string
	theme         string
	notifications bool
}

func NewCoarseProfile() *CoarseProfile {
	return &CoarseProfile{bio: "bio", avatarURL: "url", theme: "light", notifications: true}
}

func (p *CoarseProfile) SetBio(v string)        { p.mu.Lock(); p.bio = v; p.mu.Unlock() }
func (p *CoarseProfile) Bio() string             { p.mu.Lock(); defer p.mu.Unlock(); return p.bio }
func (p *CoarseProfile) SetAvatar(v string)      { p.mu.Lock(); p.avatarURL = v; p.mu.Unlock() }
func (p *CoarseProfile) Avatar() string          { p.mu.Lock(); defer p.mu.Unlock(); return p.avatarURL }
func (p *CoarseProfile) SetTheme(v string)       { p.mu.Lock(); p.theme = v; p.mu.Unlock() }
func (p *CoarseProfile) Theme() string           { p.mu.Lock(); defer p.mu.Unlock(); return p.theme }
func (p *CoarseProfile) SetNotif(v bool)         { p.mu.Lock(); p.notifications = v; p.mu.Unlock() }
func (p *CoarseProfile) Notif() bool             { p.mu.Lock(); defer p.mu.Unlock(); return p.notifications }

// --- Fine: per-field RWMutex ---

type FineProfile struct {
	bioMu    sync.RWMutex
	bio      string
	avatarMu sync.RWMutex
	avatarURL string
	themeMu  sync.RWMutex
	theme    string
	notifMu  sync.RWMutex
	notifications bool
}

func NewFineProfile() *FineProfile {
	return &FineProfile{bio: "bio", avatarURL: "url", theme: "light", notifications: true}
}

func (p *FineProfile) SetBio(v string)    { p.bioMu.Lock(); p.bio = v; p.bioMu.Unlock() }
func (p *FineProfile) Bio() string        { p.bioMu.RLock(); defer p.bioMu.RUnlock(); return p.bio }
func (p *FineProfile) SetAvatar(v string) { p.avatarMu.Lock(); p.avatarURL = v; p.avatarMu.Unlock() }
func (p *FineProfile) Avatar() string     { p.avatarMu.RLock(); defer p.avatarMu.RUnlock(); return p.avatarURL }
func (p *FineProfile) SetTheme(v string)  { p.themeMu.Lock(); p.theme = v; p.themeMu.Unlock() }
func (p *FineProfile) Theme() string      { p.themeMu.RLock(); defer p.themeMu.RUnlock(); return p.theme }
func (p *FineProfile) SetNotif(v bool)    { p.notifMu.Lock(); p.notifications = v; p.notifMu.Unlock() }
func (p *FineProfile) Notif() bool        { p.notifMu.RLock(); defer p.notifMu.RUnlock(); return p.notifications }

// --- Sharded map ---

type ShardedMap struct {
	shards    []mapShard
	shardMask uint32
}

type mapShard struct {
	mu   sync.RWMutex
	data map[string]string
}

func NewShardedMap(count int) *ShardedMap {
	n := 1
	for n < count {
		n *= 2
	}
	shards := make([]mapShard, n)
	for i := range shards {
		shards[i].data = make(map[string]string)
	}
	return &ShardedMap{shards: shards, shardMask: uint32(n - 1)}
}

func (sm *ShardedMap) idx(key string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(key))
	return h.Sum32() & sm.shardMask
}

func (sm *ShardedMap) Set(k, v string) {
	i := sm.idx(k)
	sm.shards[i].mu.Lock()
	sm.shards[i].data[k] = v
	sm.shards[i].mu.Unlock()
}

func (sm *ShardedMap) Get(k string) (string, bool) {
	i := sm.idx(k)
	sm.shards[i].mu.RLock()
	defer sm.shards[i].mu.RUnlock()
	v, ok := sm.shards[i].data[k]
	return v, ok
}

// --- Benchmark helpers ---

func benchCoarse(goroutines, ops int) time.Duration {
	p := NewCoarseProfile()
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < ops; j++ {
				switch j % 4 {
				case 0: p.SetBio(fmt.Sprintf("b-%d", id))
				case 1: p.SetAvatar(fmt.Sprintf("a-%d", id))
				case 2: _ = p.Theme()
				case 3: _ = p.Notif()
				}
			}
		}(i)
	}
	wg.Wait()
	return time.Since(start)
}

func benchFine(goroutines, ops int) time.Duration {
	p := NewFineProfile()
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < ops; j++ {
				switch j % 4 {
				case 0: p.SetBio(fmt.Sprintf("b-%d", id))
				case 1: p.SetAvatar(fmt.Sprintf("a-%d", id))
				case 2: _ = p.Theme()
				case 3: _ = p.Notif()
				}
			}
		}(i)
	}
	wg.Wait()
	return time.Since(start)
}

func benchSharded(goroutines, ops, shards int) time.Duration {
	m := NewShardedMap(shards)
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < ops; j++ {
				key := fmt.Sprintf("k:%d:%d", id, j%50)
				if j%3 == 0 {
					m.Set(key, "v")
				} else {
					m.Get(key)
				}
			}
		}(i)
	}
	wg.Wait()
	return time.Since(start)
}

func main() {
	const g = 100
	const ops = 5000

	fmt.Printf("=== Mutex Granularity Benchmark (%d goroutines x %d ops) ===\n\n", g, ops)

	fmt.Println("--- Struct Fields ---")
	coarseTime := benchCoarse(g, ops)
	fineTime := benchFine(g, ops)
	fmt.Printf("  coarse (1 mutex):     %v\n", coarseTime.Round(time.Millisecond))
	fmt.Printf("  fine (per-field):      %v\n", fineTime.Round(time.Millisecond))

	fmt.Println("\n--- Map Access ---")
	singleMapTime := benchSharded(g, ops, 1)
	sharded16Time := benchSharded(g, ops, 16)
	sharded64Time := benchSharded(g, ops, 64)
	fmt.Printf("  single lock:          %v\n", singleMapTime.Round(time.Millisecond))
	fmt.Printf("  sharded (16):         %v\n", sharded16Time.Round(time.Millisecond))
	fmt.Printf("  sharded (64):         %v\n", sharded64Time.Round(time.Millisecond))

	fmt.Println("\n=== Decision Framework ===")
	fmt.Println("  +-------------------+-----------------------------+----------------------------+")
	fmt.Println("  | Approach          | Use When                    | Avoid When                 |")
	fmt.Println("  +-------------------+-----------------------------+----------------------------+")
	fmt.Println("  | Single mutex      | Few fields, low contention  | High concurrency,          |")
	fmt.Println("  |                   | Simple code preferred       | independent field access   |")
	fmt.Println("  +-------------------+-----------------------------+----------------------------+")
	fmt.Println("  | Per-field mutex   | High contention on          | Fields often updated       |")
	fmt.Println("  |                   | independent fields          | together (use single)      |")
	fmt.Println("  +-------------------+-----------------------------+----------------------------+")
	fmt.Println("  | Sharded map       | Large map, many goroutines  | Small map, few goroutines  |")
	fmt.Println("  |                   | accessing different keys    | (overhead not worth it)    |")
	fmt.Println("  +-------------------+-----------------------------+----------------------------+")

	fmt.Println("\n=== The Principle ===")
	fmt.Println("  \"Lock the data, not the function.\"")
	fmt.Println("  A mutex should protect the smallest unit of shared state.")
	fmt.Println("  Start with one mutex. Profile. Split only when contention is measured.")
}
```

### Verification
```
=== Mutex Granularity Benchmark (100 goroutines x 5000 ops) ===

--- Struct Fields ---
  coarse (1 mutex):     42ms
  fine (per-field):      25ms

--- Map Access ---
  single lock:          90ms
  sharded (16):         20ms
  sharded (64):         16ms

=== Decision Framework ===
  +-------------------+-----------------------------+----------------------------+
  | Approach          | Use When                    | Avoid When                 |
  +-------------------+-----------------------------+----------------------------+
  | Single mutex      | Few fields, low contention  | High concurrency,          |
  |                   | Simple code preferred       | independent field access   |
  +-------------------+-----------------------------+----------------------------+
  | Per-field mutex   | High contention on          | Fields often updated       |
  |                   | independent fields          | together (use single)      |
  +-------------------+-----------------------------+----------------------------+
  | Sharded map       | Large map, many goroutines  | Small map, few goroutines  |
  |                   | accessing different keys    | (overhead not worth it)    |
  +-------------------+-----------------------------+----------------------------+

=== The Principle ===
  "Lock the data, not the function."
  A mutex should protect the smallest unit of shared state.
  Start with one mutex. Profile. Split only when contention is measured.
```
Times vary by machine, but the relative ordering is consistent.

## Verification

Run each step with the race detector:
```bash
go run -race main.go
```
All steps should complete without race warnings.

## Common Mistakes

### 1. Splitting Locks When Fields Are Updated Together
If two fields must always be updated atomically (e.g., balance and last_transaction_id), they must share a lock. Splitting them creates a window where one is updated but not the other:

```go
// BAD: balance and lastTxID can be inconsistent between the two locks.
func (a *Account) Transfer(amount int64, txID string) {
    a.balanceMu.Lock()
    a.balance -= amount
    a.balanceMu.Unlock()
    // Another goroutine reads balance here: sees deducted balance with old txID.
    a.txMu.Lock()
    a.lastTxID = txID
    a.txMu.Unlock()
}

// GOOD: one lock for coupled fields.
func (a *Account) Transfer(amount int64, txID string) {
    a.mu.Lock()
    defer a.mu.Unlock()
    a.balance -= amount
    a.lastTxID = txID
}
```

### 2. Holding a Lock Across I/O or Sleep
A lock held during a network call or sleep blocks all other goroutines waiting for that lock. Extract the critical section to be as short as possible:

```go
// BAD: lock held during network call.
func (s *Service) Update(id string) {
    s.mu.Lock()
    defer s.mu.Unlock()
    data := fetchFromNetwork(id) // 200ms network call while holding lock!
    s.cache[id] = data
}

// GOOD: lock only the map update.
func (s *Service) Update(id string) {
    data := fetchFromNetwork(id) // no lock held during I/O.
    s.mu.Lock()
    s.cache[id] = data
    s.mu.Unlock()
}
```

### 3. Premature Optimization
Do not shard until profiling proves contention is the bottleneck. A sharded map with 64 shards for 10 goroutines adds complexity for no measurable benefit. Start simple, measure, then optimize.

### 4. Locking Two Mutexes in Different Orders
If function A locks mutex1 then mutex2, and function B locks mutex2 then mutex1, you have a deadlock. Always lock multiple mutexes in a consistent, documented order.

## Review

Lock granularity is a spectrum between one big lock (simple, high contention)
and many small locks (complex, low contention), and the benchmarks make the
trade concrete. For structs, start with a single mutex and split into per-field
`RWMutex` locks only when profiling shows independent fields actually contend --
splitting fields that are always updated together just opens an inconsistency
window. For maps, sharding spreads keys across N internal maps with independent
locks, cutting contention roughly in proportion to the shard count, with
diminishing returns once shards exceed the goroutine count; powers of two let the
shard index be a cheap mask instead of a modulo. The governing rule is "lock the
data, not the function": protect the smallest unit of shared state, keep critical
sections short, never hold a lock across I/O or sleep, and always take multiple
mutexes in one consistent order. Start simple, measure, and split only when
contention is the proven bottleneck.

Make sure you can answer the questions the benchmarks set up: when per-field
mutexes are actually slower than a single one, how shard count changes contention
and why powers of two are chosen, which fields in a struct must share a lock
versus hold independent ones, and why holding a lock during I/O is a problem and
how to restructure the critical section to avoid it.

## Resources

- [sync.Mutex documentation](https://pkg.go.dev/sync#Mutex) -- the basic mutual-exclusion primitive and its non-reentrant Lock/Unlock contract.
- [sync.RWMutex documentation](https://pkg.go.dev/sync#RWMutex) -- read/write locking, which the per-field and sharded designs use to let readers run in parallel.
- [Go Wiki: MutexOrChannel](https://go.dev/wiki/MutexOrChannel) -- guidance on when a lock beats a channel for guarding shared state.
- [hash/fnv documentation](https://pkg.go.dev/hash/fnv) -- the fast non-cryptographic hash the sharded map uses to pick a shard.

---

Back to [Concurrency](../../concurrency.md) | Next: [01-atomic-add-counter](../../05-atomic-and-memory-ordering/01-atomic-add-counter/01-atomic-add-counter.md)
