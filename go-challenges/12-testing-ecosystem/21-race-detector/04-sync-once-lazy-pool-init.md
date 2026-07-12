# Exercise 4: Eliminating a Check-Then-Act Race in Lazy Connection-Pool Init

A lazily-initialized shared resource -- a database handle, a connection pool, a
client -- is where the check-then-act race lives: `if p == nil { p = open() }`
looks innocent but double-initializes under concurrency, and races on the
assignment. This exercise reproduces that race in `cmd/racy` and fixes it with
`sync.Once`, proving with an atomic init-counter that initialization happens
exactly once no matter how many goroutines call in at once.

This module is self-contained: its own `go mod init`, its own racy demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
connpool/                   independent module: example.com/connpool
  go.mod                    go 1.26
  pool.go                   type Pool, type Manager (sync.Once): GetPool, InitCount
  cmd/
    demo/
      main.go               fixed version: 100 goroutines get the same pool
    racy/
      main.go               check-then-act version that double-inits; run with -race
  pool_test.go              200 concurrent GetPool: same pointer, InitCount==1, under -race
```

Files: `pool.go`, `cmd/demo/main.go`, `cmd/racy/main.go`, `pool_test.go`.
Implement: a `Manager` whose `GetPool` uses `sync.Once` to initialize exactly one
`*Pool`, counting inits with an `atomic.Int64`.
Test: `TestGetPoolInitializesExactlyOnce` launches 200 concurrent `GetPool`
calls, asserts one shared pointer and `InitCount()==1`, under `-race`.
Verify: `go test -count=1 -race ./...`; then `go run -race ./cmd/racy`.

### Why check-then-act races, and why sync.Once fixes it

The naive lazy init is three steps: read `p`, test it against `nil`, and if nil,
write `p`. Two goroutines can both execute the read-and-test before either does
the write, so both see `nil` and both call `open()`. That is two problems at
once: a data race on the `p` assignment (two writes with no ordering edge, which
`-race` reports), and a resource leak (two pools opened, one silently
overwritten and never closed). "It usually only inits once" is the undefined
behavior talking; under load it inits twice and leaks.

`sync.Once` is built for exactly this. `once.Do(f)` runs `f` exactly once across
all goroutines and all calls; concurrent callers block until the first `f`
completes, and every call's return happens-after that completion. So the pool is
opened once, and the memory model guarantees every caller observes the fully
initialized pool -- the completion of `f` happens-before the return of any
`Do`, which publishes the `m.pool` write to every reader without an extra lock.
This is the check-then-act branch of the fix-by-access-pattern decision tree.

`sync.Once` is the right tool here rather than a plain mutex-guarded lazy init
(which works but takes the lock on every subsequent call) or an
`atomic.Pointer` compare-and-swap (which can open a pool that then loses the CAS
and must be discarded -- wasteful if `open` is expensive). `Once` takes the lock
only until the first init completes and never opens more than one resource.

Create `pool.go`:

```go
package connpool

import (
	"sync"
	"sync/atomic"
)

// Pool is a stand-in for an expensive-to-open shared resource (a DB handle or
// connection pool). Each real open gets a unique id so the test can prove only
// one was created.
type Pool struct {
	ID int
}

var nextID atomic.Int64

func openPool() *Pool {
	return &Pool{ID: int(nextID.Add(1))}
}

// Manager lazily initializes a single shared *Pool. All callers of GetPool
// observe the same pool, and openPool runs exactly once.
type Manager struct {
	once      sync.Once
	pool      *Pool
	initCount atomic.Int64
}

// GetPool returns the shared pool, initializing it on the first call.
func (m *Manager) GetPool() *Pool {
	m.once.Do(func() {
		m.initCount.Add(1)
		m.pool = openPool()
	})
	return m.pool
}

// InitCount reports how many times initialization actually ran. It must be 1
// after any number of GetPool calls.
func (m *Manager) InitCount() int64 { return m.initCount.Load() }
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"

	"example.com/connpool"
)

func main() {
	var m connpool.Manager

	const n = 100
	var wg sync.WaitGroup
	ids := make([]int, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ids[i] = m.GetPool().ID
		}()
	}
	wg.Wait()

	same := true
	for _, id := range ids {
		if id != ids[0] {
			same = false
		}
	}
	fmt.Printf("all goroutines got the same pool: %v\n", same)
	fmt.Printf("initializations: %d\n", m.InitCount())
}
```

Run it:

```bash
go run -race ./cmd/demo
```

Expected output:

```text
all goroutines got the same pool: true
initializations: 1
```

### The racy version, for the report

Create `cmd/racy/main.go`. Run it with `go run -race ./cmd/racy` to see the
check-then-act race and the double init:

```go
// Command racy shows the check-then-act lazy-init race. Run manually:
//
//	go run -race ./cmd/racy
//
// It is a main package with no test, so `go test -race ./...` only builds it.
package main

import (
	"fmt"
	"sync"
	"sync/atomic"
)

type pool struct{ id int }

type racyManager struct {
	p         *pool // read and written with no synchronization
	initCount int   // incremented racily
	nextID    atomic.Int64
}

func (m *racyManager) getPool() *pool {
	if m.p == nil { // racy read
		m.initCount++                         // racy write
		m.p = &pool{id: int(m.nextID.Add(1))} // racy write
	}
	return m.p
}

func main() {
	var m racyManager

	var wg sync.WaitGroup
	for range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = m.getPool()
		}()
	}
	wg.Wait()

	fmt.Printf("initializations: %d (expected 1; >1 means it double-initialized)\n", m.initCount)
}
```

### Tests

`TestGetPoolInitializesExactlyOnce` launches 200 concurrent `GetPool` calls,
collects every returned pointer, and asserts they are all identical and that
`InitCount()` is exactly 1. The pointer identity proves every caller got the same
pool; the init count proves `open` ran once. It passes under `-race` because
`sync.Once` provides the ordering the check-then-act version lacked.

Create `pool_test.go`:

```go
package connpool

import (
	"sync"
	"testing"
)

func TestGetPoolInitializesExactlyOnce(t *testing.T) {
	t.Parallel()

	var m Manager
	const n = 200
	got := make([]*Pool, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i] = m.GetPool()
		}()
	}
	wg.Wait()

	first := got[0]
	if first == nil {
		t.Fatal("GetPool returned nil")
	}
	for i, p := range got {
		if p != first {
			t.Fatalf("goroutine %d got a different pool pointer %p, want %p", i, p, first)
		}
	}
	if c := m.InitCount(); c != 1 {
		t.Fatalf("InitCount() = %d, want exactly 1", c)
	}
}

func TestGetPoolSerialIdempotent(t *testing.T) {
	t.Parallel()

	var m Manager
	a := m.GetPool()
	b := m.GetPool()
	if a != b {
		t.Fatalf("serial GetPool returned different pointers %p and %p", a, b)
	}
	if c := m.InitCount(); c != 1 {
		t.Fatalf("InitCount() = %d after two serial calls, want 1", c)
	}
}
```

## Review

The manager is correct when initialization is exactly-once regardless of caller
count: every `GetPool` returns the same `*Pool`, and `InitCount()` is 1. The
proof is `TestGetPoolInitializesExactlyOnce` passing under `-race` -- 200
goroutines race into `GetPool` and the detector finds no unordered access, while
the pointer-identity and init-count assertions confirm one pool, one init. The
racy `cmd/racy` shows the contrast: check-then-act double-initializes and races on
the assignment.

The mistake to avoid is precisely check-then-act (`if p == nil { p = open() }`)
for any shared lazy init. Reach for `sync.Once` when a mutex-guarded init would
lock on every call and you never want more than one resource opened; reach for an
atomic CAS only when a speculatively-opened-then-discarded resource is cheap. Run
`go test -count=1 -race ./...`.

## Resources

- [`sync.Once`](https://pkg.go.dev/sync#Once) -- exactly-once execution and the happens-before it publishes to every caller.
- [`sync/atomic`](https://pkg.go.dev/sync/atomic) -- `atomic.Int64` for the init counter and the pool-id generator.
- [The Go Memory Model](https://go.dev/ref/mem) -- the `Once` ordering guarantee the read of `m.pool` relies on.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [03-rwmutex-feature-flag-store.md](03-rwmutex-feature-flag-store.md) | Next: [05-copy-on-write-config-snapshot.md](05-copy-on-write-config-snapshot.md)
