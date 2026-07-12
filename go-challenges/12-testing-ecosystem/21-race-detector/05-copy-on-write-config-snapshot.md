# Exercise 5: Lock-Free Hot-Reloadable Config via atomic.Pointer Copy-On-Write

Configuration that reloads while the server runs -- from a file watcher, a
control-plane push, a SIGHUP -- is read on the request path far more often than it
is written. This exercise serves it with zero reader locking using copy-on-write:
readers `Load` a pointer to an immutable snapshot, and the reload goroutine builds
a brand-new `*Config` and `Store`s it. The atomic swap gives readers a fully
consistent snapshot every time, and never a half-mutated struct.

This module is self-contained: its own `go mod init`, its own racy demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
cowconfig/                  independent module: example.com/cowconfig
  go.mod                    go 1.26
  config.go                 type Config (immutable), type Store (atomic.Pointer): Load, Reload
  cmd/
    demo/
      main.go               load, reload, load again; print generations
    racy/
      main.go               in-place field mutation of a shared *Config; run with -race
  config_test.go            readers Load+read fields while writer Stores; consistent, under -race
```

Files: `config.go`, `cmd/demo/main.go`, `cmd/racy/main.go`, `config_test.go`.
Implement: an immutable `Config`, a `Store` wrapping `atomic.Pointer[Config]`
with `Load` and `Reload`.
Test: `TestConfigSnapshotConcurrentReload` runs readers that read multiple fields
of a loaded snapshot while a writer stores new ones, asserting each reader sees
one consistent generation, under `-race`.
Verify: `go test -count=20 -race ./...`; then `go run -race ./cmd/racy`.

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/21-race-detector/05-copy-on-write-config-snapshot/cmd/demo go-solutions/12-testing-ecosystem/21-race-detector/05-copy-on-write-config-snapshot/cmd/racy
cd go-solutions/12-testing-ecosystem/21-race-detector/05-copy-on-write-config-snapshot
```

### Why copy-on-write, and why the memory model makes it correct

Locking config on every request works but adds a lock acquisition to the hottest
path in the server, and readers do not conflict with each other -- only with the
rare writer. Copy-on-write removes the reader lock entirely. The mechanism rests
on the memory model, not on luck: the writer allocates a brand-new `*Config`,
fully initializes every field, and then calls `ptr.Store(newCfg)`. A reader calls
`ptr.Load()`. The atomic `Store` and the `Load` that observes it form a
release/acquire edge -- everything the writer wrote to the struct before the
`Store` happens-before everything the reader does after the `Load`. So a reader
sees either the entire old snapshot or the entire new one, never a mix, and never
takes a lock. This is the whole-object-hot-swap branch of the decision tree.

The absolute rule is immutability after publish: once a `*Config` has been
`Store`d, nobody mutates the struct it points to. A new configuration is a new
allocation and a new `Store`. That is what makes the snapshot a reader `Load`s
safe to read field by field without synchronization -- the struct it points to
will never change under it. The contrasting bug, shown in `cmd/racy`, is mutating
`cfg.MaxConns` and `cfg.Timeout` in place on a shared `*Config` while readers read
those fields: readers see a struct with the new `MaxConns` and the old `Timeout`,
a torn generation, and the writes race with the reads.

The invariant used to detect a torn read: in this store, `MaxConns` always equals
`Generation * 10`. A reader that loads a snapshot and finds `MaxConns !=
Generation*10` has observed fields from two different generations -- exactly the
bug copy-on-write prevents and in-place mutation causes.

Create `config.go`:

```go
package cowconfig

import (
	"sync/atomic"
	"time"
)

// Config is an immutable configuration snapshot. Once stored in a Store it is
// never mutated; a new configuration is a brand-new Config value. The invariant
// MaxConns == Generation*10 lets a test detect a torn (mixed-generation) read.
type Config struct {
	Generation int
	MaxConns   int
	Timeout    time.Duration
}

// Store holds the current config behind an atomic pointer so readers never lock.
type Store struct {
	cfg atomic.Pointer[Config]
}

// NewStore returns a Store initialized to generation 1.
func NewStore() *Store {
	s := &Store{}
	s.cfg.Store(&Config{Generation: 1, MaxConns: 10, Timeout: time.Second})
	return s
}

// Load returns the current immutable snapshot. Lock-free; the caller may read
// its fields freely because the snapshot never changes after publication.
func (s *Store) Load() *Config {
	return s.cfg.Load()
}

// Reload publishes a brand-new snapshot for the given generation. It builds a
// fresh Config and atomically swaps the pointer (copy-on-write); it never
// mutates the previously published snapshot.
func (s *Store) Reload(generation int) {
	next := &Config{
		Generation: generation,
		MaxConns:   generation * 10,
		Timeout:    time.Duration(generation) * time.Second,
	}
	s.cfg.Store(next)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/cowconfig"
)

func main() {
	s := cowconfig.NewStore()

	c := s.Load()
	fmt.Printf("gen=%d maxconns=%d timeout=%s\n", c.Generation, c.MaxConns, c.Timeout)

	s.Reload(3)

	c = s.Load()
	fmt.Printf("gen=%d maxconns=%d timeout=%s\n", c.Generation, c.MaxConns, c.Timeout)
}
```

Run it:

```bash
go run -race ./cmd/demo
```

Expected output:

```text
gen=1 maxconns=10 timeout=1s
gen=3 maxconns=30 timeout=3s
```

### The racy version, for the report

Create `cmd/racy/main.go`. Run with `go run -race ./cmd/racy` to see the in-place
mutation race:

```go
// Command racy mutates a shared *Config in place while readers read its fields,
// producing a data race and torn (mixed-generation) reads. Run manually:
//
//	go run -race ./cmd/racy
//
// It is a main package with no test, so `go test -race ./...` only builds it.
package main

import (
	"fmt"
	"sync"
)

type config struct {
	generation int
	maxConns   int // invariant: maxConns == generation*10
}

func main() {
	shared := &config{generation: 1, maxConns: 10}

	var wg sync.WaitGroup

	// Writer mutates fields in place -- the bug.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for g := 2; g <= 1000; g++ {
			shared.generation = g    // racy write
			shared.maxConns = g * 10 // racy write (torn window between the two)
		}
	}()

	// Reader reads both fields and checks the invariant.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 1000 {
			g := shared.generation // racy read
			m := shared.maxConns   // racy read
			if m != g*10 {
				fmt.Printf("torn read: generation=%d maxConns=%d\n", g, m)
			}
		}
	}()

	wg.Wait()
	fmt.Println("done")
}
```

### Tests

`TestConfigSnapshotConcurrentReload` runs many reader goroutines that each `Load`
a snapshot and read all three fields, checking the `MaxConns == Generation*10`
invariant, while a writer stores new generations in a loop, bounded by a
deadline. Because every reader reads from a single immutable snapshot, the
invariant always holds; a failure would mean a torn read. Under `-race -count=20`
the scheduler explores twenty runs' worth of interleavings.

Create `config_test.go`:

```go
package cowconfig

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestConfigSnapshotConcurrentReload(t *testing.T) {
	t.Parallel()

	s := NewStore()

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	var wg sync.WaitGroup

	// One writer publishing new generations.
	wg.Add(1)
	go func() {
		defer wg.Done()
		gen := 1
		for ctx.Err() == nil {
			gen++
			s.Reload(gen)
		}
	}()

	// Many readers, each reading a whole snapshot and checking the invariant.
	const readers = 8
	for range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				c := s.Load()
				if c.MaxConns != c.Generation*10 {
					t.Errorf("torn snapshot: gen=%d maxconns=%d", c.Generation, c.MaxConns)
					return
				}
				wantTimeout := time.Duration(c.Generation) * time.Second
				if c.Timeout != wantTimeout {
					t.Errorf("torn snapshot: gen=%d timeout=%s", c.Generation, c.Timeout)
					return
				}
			}
		}()
	}

	wg.Wait()
}

func TestReloadPublishesNewGeneration(t *testing.T) {
	t.Parallel()

	s := NewStore()
	before := s.Load()
	s.Reload(5)
	after := s.Load()

	if before.Generation != 1 {
		t.Fatalf("initial generation = %d, want 1", before.Generation)
	}
	if after.Generation != 5 || after.MaxConns != 50 {
		t.Fatalf("reloaded gen=%d maxconns=%d, want 5,50", after.Generation, after.MaxConns)
	}
	// The old snapshot the reader still holds is unchanged (immutability).
	if before.Generation != 1 || before.MaxConns != 10 {
		t.Fatal("Reload mutated a previously published snapshot")
	}
}
```

## Review

The store is correct when every reader observes one whole generation of config
with no lock: `MaxConns == Generation*10` and `Timeout == Generation*second`
inside each loaded snapshot, always. The proof is
`TestConfigSnapshotConcurrentReload` passing under `-race -count=20` -- readers
and a reloader run concurrently, the detector finds no unordered access, and the
invariant never breaks. `TestReloadPublishesNewGeneration` pins that a stored
snapshot is immutable: reloading does not reach back and change a snapshot a
reader already holds.

The mistake to avoid is mutating a published `*Config` in place on a read-heavy
path, as `cmd/racy` demonstrates -- readers see mixed-generation fields and the
writes race with the reads. Build a new immutable value and `Store` the pointer;
never mutate a snapshot after publishing it. Copy-on-write wins when reads vastly
outnumber writes; if writes were frequent, the per-write allocation would argue
for a lock instead. Run `go test -count=20 -race ./...`.

## Resources

- [`sync/atomic` Pointer](https://pkg.go.dev/sync/atomic#Pointer) -- `Load`, `Store`, and `CompareAndSwap` on a typed atomic pointer.
- [The Go Memory Model](https://go.dev/ref/mem) -- the release/acquire edge between atomic `Store` and `Load` that makes copy-on-write safe.
- [Go Blog: Introducing the Go Race Detector](https://go.dev/blog/race-detector) -- why in-place mutation of shared state is a race.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [04-sync-once-lazy-pool-init.md](04-sync-once-lazy-pool-init.md) | Next: [06-worker-pool-result-fan-in.md](06-worker-pool-result-fan-in.md)
