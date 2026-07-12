# Exercise 6: Hot-Reload Config — Lock-Free Reads with atomic.Pointer[Config]

A running server often reloads its config while thousands of requests read it. This
exercise builds the standard solution: hold the config behind an
`atomic.Pointer[Config]`, let handlers `Load()` a consistent snapshot with no lock,
and let a reloader publish a freshly-built `*Config` with `Store()` or
`CompareAndSwap()`. The point is that you swap a pointer to an immutable snapshot
instead of mutating shared fields in place.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
hotconfig/                  independent module: example.com/hotconfig
  go.mod
  hotconfig.go              Config (+Consistent); Store over atomic.Pointer; NewStore/Current/Reload/CompareAndReload/Build
  cmd/
    demo/
      main.go               reads, reloads, reads again; shows a CAS guard
  hotconfig_test.go         concurrent readers under -race see no torn snapshot; CAS guard test
```

- Files: `hotconfig.go`, `cmd/demo/main.go`, `hotconfig_test.go`.
- Implement: a `Store` wrapping `atomic.Pointer[Config]` with `Current() *Config`, `Reload(*Config)`, and `CompareAndReload(old, new *Config) bool`.
- Test: N reader goroutines call `Current()` while a writer `Reload()`s; assert every observed snapshot is internally consistent and the final `Load` matches the last `Store`; add a CAS guard test.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/09-pointers/02-pointers-and-function-parameters/06-atomic-pointer-config-hot-reload/cmd/demo
cd go-solutions/09-pointers/02-pointers-and-function-parameters/06-atomic-pointer-config-hot-reload
```

### Why swap a snapshot instead of mutating fields

If a reloader did `cfg.Host = newHost; cfg.Port = newPort` while handlers read
`cfg.Host` and `cfg.Port`, two things go wrong. First, it is a data race — even one
field written while another goroutine reads it is undefined behavior, and `-race`
flags it. Second, even ignoring the race, a reader could observe a *torn* config:
the new `Host` with the old `Port`, because the two writes are not one atomic step.

The fix is to never mutate the shared value. The reloader builds a brand-new,
fully-populated `*Config` off to the side, then publishes it in a single atomic
step: `store.Reload(fresh)` calls `atomic.Pointer[Config].Store(fresh)`. A reader's
`Current()` is `atomic.Pointer[Config].Load()`, which returns either the old pointer
or the new one — never a half-updated struct, because the struct is immutable and
only the pointer is swapped. The whole config changes atomically. `CompareAndReload`
adds a compare-and-set guard: publish `newC` only if the current snapshot is still
`old` (by pointer identity), which is how you avoid clobbering a reload that a
concurrent goroutine already applied.

To make "torn read" testable, the snapshot carries an invariant: its `Host` and
`Port` are derived from its `Version`, so a reader can check the fields agree.
`Build(v)` constructs consistent snapshots; `Consistent()` verifies the invariant.

Create `hotconfig.go`:

```go
package hotconfig

import (
	"fmt"
	"sync/atomic"
)

// Config is an immutable snapshot. Host and Port are derived from Version, so a
// torn read (mismatched fields) is detectable.
type Config struct {
	Version int
	Host    string
	Port    int
}

// Build constructs a consistent snapshot for version v.
func Build(v int) *Config {
	return &Config{Version: v, Host: fmt.Sprintf("db-%d", v), Port: 5000 + v}
}

// Consistent reports whether the snapshot's fields agree with its Version.
func (c *Config) Consistent() bool {
	return c.Host == fmt.Sprintf("db-%d", c.Version) && c.Port == 5000+c.Version
}

// Store holds the current config behind an atomic pointer. Readers Load a
// snapshot with no lock; the writer Stores a fresh, fully-built snapshot.
type Store struct {
	ptr atomic.Pointer[Config]
}

// NewStore seeds the store with an initial snapshot.
func NewStore(initial *Config) *Store {
	s := &Store{}
	s.ptr.Store(initial)
	return s
}

// Current returns the latest snapshot with no lock.
func (s *Store) Current() *Config {
	return s.ptr.Load()
}

// Reload atomically publishes a new snapshot.
func (s *Store) Reload(c *Config) {
	s.ptr.Store(c)
}

// CompareAndReload publishes newC only if the current snapshot is still old (by
// pointer identity). It returns whether the swap happened.
func (s *Store) CompareAndReload(old, newC *Config) bool {
	return s.ptr.CompareAndSwap(old, newC)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/hotconfig"
)

func main() {
	store := hotconfig.NewStore(hotconfig.Build(1))
	fmt.Printf("start:  %+v\n", *store.Current())

	store.Reload(hotconfig.Build(2))
	fmt.Printf("reload: %+v\n", *store.Current())

	// A compare-and-set guard: only reload if nobody changed it first.
	cur := store.Current()
	ok := store.CompareAndReload(cur, hotconfig.Build(3))
	fmt.Printf("cas ok=%v now version=%d\n", ok, store.Current().Version)

	// A stale expectation loses the race and does not clobber.
	stale := hotconfig.Build(2)
	ok = store.CompareAndReload(stale, hotconfig.Build(99))
	fmt.Printf("stale cas ok=%v still version=%d\n", ok, store.Current().Version)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
start:  {Version:1 Host:db-1 Port:5001}
reload: {Version:2 Host:db-2 Port:5002}
cas ok=true now version=3
stale cas ok=false still version=3
```

### Tests

The concurrency test is the point: readers spin on `Current()` while a writer
reloads a thousand times, and every observed snapshot must be internally consistent
under `-race`. The CAS test pins the compare-and-set guard, including that identity
(not field equality) is what CAS compares.

Create `hotconfig_test.go`:

```go
package hotconfig

import (
	"sync"
	"testing"
)

func TestConcurrentReadsAreConsistent(t *testing.T) {
	t.Parallel()
	s := NewStore(Build(0))

	var wg sync.WaitGroup
	stop := make(chan struct{})

	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if c := s.Current(); !c.Consistent() {
					t.Errorf("torn read: %+v", *c)
					return
				}
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for v := 1; v <= 1000; v++ {
			s.Reload(Build(v))
		}
		close(stop)
	}()

	wg.Wait()
	if got := s.Current().Version; got != 1000 {
		t.Fatalf("final version = %d, want 1000", got)
	}
}

func TestCompareAndReload(t *testing.T) {
	t.Parallel()
	initial := Build(1)
	s := NewStore(initial)

	next := Build(2)
	if !s.CompareAndReload(initial, next) {
		t.Fatal("CAS should succeed when old matches the current pointer")
	}
	if s.Current().Version != 2 {
		t.Fatalf("version = %d, want 2 after successful CAS", s.Current().Version)
	}

	// A different pointer with the same fields must NOT match: CAS compares
	// pointer identity, not value equality.
	stale := Build(1)
	if s.CompareAndReload(stale, Build(3)) {
		t.Fatal("CAS should fail: stale is a different pointer than current")
	}
	if s.Current().Version != 2 {
		t.Fatalf("version = %d, want unchanged 2 after failed CAS", s.Current().Version)
	}
}
```

## Review

The store is correct when a reader can never see a partially-updated config. The
consistency invariant makes that testable: `Build` only ever produces snapshots
whose `Host`/`Port` match their `Version`, so a `Consistent()` failure would mean a
reader observed fields from two different snapshots — which cannot happen when the
whole struct is swapped by an atomic pointer. Run the test with `-race`; without the
atomic pointer (mutating shared fields instead) the detector would fire immediately.
The CAS test underscores that `CompareAndSwap` compares the *pointer*, not the value:
a freshly built `Build(1)` is a different address than the original `Build(1)` and
correctly loses. Reach for this pattern any time reads vastly outnumber writes and
the value is safe to treat as immutable between reloads.

## Resources

- [`sync/atomic` Pointer](https://pkg.go.dev/sync/atomic#Pointer) — `Load`, `Store`, `Swap`, `CompareAndSwap` on a typed atomic pointer.
- [The Go Memory Model](https://go.dev/ref/mem) — why in-place shared writes are races and atomic publication is not.
- [Go 1.19 release notes: sync/atomic types](https://go.dev/doc/go1.19#atomic_types) — when the typed `atomic.Pointer[T]` was added.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [05-mutate-slice-elements-in-place.md](05-mutate-slice-elements-in-place.md) | Next: [07-nil-receiver-optional-dependency.md](07-nil-receiver-optional-dependency.md)
