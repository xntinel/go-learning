# Exercise 1: Build the contended single-mutex store (the bottleneck under test)

Before you can profile a lock bottleneck you need one that behaves like a real
service under load. This module builds the store the rest of the chapter picks
apart: a concurrency-safe key-value counter guarded by a single `sync.Mutex`, with
a small simulated critical section so the contention is observable in a profile.
It is the deliberately-slow baseline every later fix is measured against.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
single-store/                 independent module: example.com/single-store
  go.mod                      go 1.26
  single.go                   type Single; NewSingle, Increment, Get; busyWork
  cmd/
    demo/
      main.go                 runnable demo: increment two keys, print totals
  single_test.go              -race serialization test, ExampleSingle
```

- Files: `single.go`, `cmd/demo/main.go`, `single_test.go`.
- Implement: a `Single` with one `sync.Mutex` guarding a `map[string]int`, whose `Increment` and `Get` hold the lock across a small `busyWork` critical section.
- Test: an `ExampleSingle` asserting the increment/get arithmetic; a `-race` test where many goroutines increment one key and the final count equals `goroutines*ops`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/single-store/cmd/demo
cd ~/go-exercises/single-store
go mod init example.com/single-store
```

### Why one lock across a real critical section

The store is a single `sync.Mutex` guarding a `map[string]int`. Both `Increment`
and `Get` take that one lock, so every operation on every key serializes through
it — key `a` and key `z` cannot proceed concurrently even though they touch
different map entries. That is exactly the bottleneck the mutex profile will later
point at, and exactly what sharding (Exercise 2) fixes.

The subtle requirement is the `busyWork` call inside the locked region. A profile
of an empty critical section shows nothing: goroutines grab and release the lock so
fast they almost never queue, so there is no wait time to attribute. Real handlers
hold the lock across something — a map mutation, a small serialization, a bounds
check — and `busyWork` stands in for that. It must do work the compiler cannot
elide: it accumulates into a local and returns the value, and the caller guards on
it with a branch that never fires. If the result were simply discarded, the
compiler would delete the loop, the lock would be held for essentially zero time,
and the whole chapter's contention signal would vanish. That elision trap is one
of the classic profiling mistakes.

`Increment` mutates the map under the lock; `Get` reads under the same lock. The
`-race` test proves the lock actually serializes writes: if it did not, concurrent
`data[key]++` operations would race and lose increments, and the final count would
come back below `goroutines*ops`. The race detector plus the exact-count assertion
together are the correctness contract.

Create `single.go`:

```go
package single

import "sync"

// Single is a concurrency-safe counter store guarded by one mutex. Every
// operation on every key serializes through that single lock, which makes it a
// clean bottleneck to profile and then fix.
type Single struct {
	mu   sync.Mutex
	data map[string]int
}

// NewSingle returns an empty store.
func NewSingle() *Single {
	return &Single{data: make(map[string]int)}
}

// Increment adds one to key's count, holding the lock across a small simulated
// critical section so contention is observable in a mutex profile.
func (s *Single) Increment(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key]++
	guard(busyWork(64))
}

// Get returns key's current count under the lock.
func (s *Single) Get(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data[key]
}

// busyWork simulates work done while holding the lock. It returns the
// accumulated sum so the caller can observe it; a discarded result would be
// elided by the compiler and the lock would be held for essentially no time.
func busyWork(iterations int) uint64 {
	var acc uint64
	for i := range iterations {
		acc += uint64(i)*2 + 1
	}
	return acc
}

// sink keeps a busyWork result observable across function boundaries so the
// optimizer cannot delete the work.
var sink uint64

// guard consumes a busyWork result on a branch that never fires in practice,
// defeating dead-code elimination without changing behavior.
func guard(v uint64) {
	if v == 1<<63 {
		sink = v
	}
}
```

### The runnable demo

The demo increments two keys a few times and prints the totals, so you can watch
the store behave as an ordinary counter before you start stressing it.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	single "example.com/single-store"
)

func main() {
	s := single.NewSingle()
	for range 3 {
		s.Increment("home")
	}
	for range 5 {
		s.Increment("checkout")
	}
	fmt.Printf("home=%d\n", s.Get("home"))
	fmt.Printf("checkout=%d\n", s.Get("checkout"))
	fmt.Printf("missing=%d\n", s.Get("missing"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
home=3
checkout=5
missing=0
```

### Tests

`TestSerializesWrites` is the correctness proof: 50 goroutines each increment the
same key 2000 times, and the final count must be exactly `50*2000`. Under `-race`,
a missing lock would both trip the detector and drop increments. `ExampleSingle`
pins the plain arithmetic.

Create `single_test.go`:

```go
package single

import (
	"fmt"
	"sync"
	"testing"
)

func TestSerializesWrites(t *testing.T) {
	t.Parallel()
	const goroutines, ops = 50, 2000
	s := NewSingle()
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			for range ops {
				s.Increment("k")
			}
		}()
	}
	wg.Wait()
	if got := s.Get("k"); got != goroutines*ops {
		t.Fatalf("Get(k) = %d, want %d (an increment was lost: the lock did not serialize)", got, goroutines*ops)
	}
}

func TestMissingKeyIsZero(t *testing.T) {
	t.Parallel()
	s := NewSingle()
	if got := s.Get("never-set"); got != 0 {
		t.Fatalf("Get(never-set) = %d, want 0", got)
	}
}

func ExampleSingle() {
	s := NewSingle()
	s.Increment("a")
	s.Increment("a")
	fmt.Println(s.Get("a"))
	// Output: 2
}
```

## Review

The store is correct when every `Increment` is serialized by the one mutex, so the
final count under any concurrency equals `goroutines*ops` exactly. The proof is
`TestSerializesWrites` under `-race`: a lost increment means the lock is not
guarding the read-modify-write of `data[key]`. Two traps are specific to this
lesson. First, do not drop the `busyWork` result on the floor — the demo would
still pass, but the mutex profile in later exercises would be empty because the
compiler elided the work and the lock was never actually held long enough to
contend. Second, keep both `Increment` and `Get` under the lock: reading a
`map[string]int` concurrently with a write is a data race even for a plain
increment, and the race detector will say so. This intentionally-serial store is
the baseline; Exercise 2 shards it.

## Resources

- [sync.Mutex](https://pkg.go.dev/sync#Mutex) — `Lock`/`Unlock` and the zero-value-ready contract.
- [The Go Memory Model](https://go.dev/ref/mem) — the happens-before edge a mutex establishes between critical sections.
- [Data Race Detector](https://go.dev/doc/articles/race_detector) — what `-race` catches and why an unguarded map access trips it.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-sharded-store.md](02-sharded-store.md)
