# Exercise 1: In-Memory Store Satisfies a Consumer-Defined Store Interface

The foundational seam of almost every backend service is a key/value store behind
a small interface. This module builds a concurrency-safe in-memory `Store` and
proves, with a test, that it satisfies the `Store` interface implicitly — no
`implements` keyword, just a matching method set — and that it is race-free under
concurrent writes.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. It is the baseline every later module builds on.

## What you'll build

```text
storageiface/                 independent module: example.com/storageiface
  go.mod                      go 1.26
  storage.go                  Store interface (Get/Set/Delete); MemoryStore (sync.RWMutex); ErrNotFound
  cmd/
    demo/
      main.go                 runnable demo: set, get, delete, get-missing
  storage_test.go             round-trip, ErrNotFound, interface pin, no-op delete, -race concurrent Set
```

- Files: `storage.go`, `cmd/demo/main.go`, `storage_test.go`.
- Implement: a `Store` interface with `Get`/`Set`/`Delete`, and a `*MemoryStore` backed by a `map[string]string` guarded by a `sync.RWMutex`.
- Test: round-trip, `ErrNotFound` via `errors.Is`, a pin that `*MemoryStore` satisfies `Store`, delete-missing-is-a-no-op, and a 100-goroutine concurrent-`Set` race test.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/08-interfaces/01-implicit-interface-satisfaction/01-store-satisfies-consumer-interface/cmd/demo
cd go-solutions/08-interfaces/01-implicit-interface-satisfaction/01-store-satisfies-consumer-interface
```

### Why the interface has three narrow methods

`Store` declares exactly the three operations a consumer of a key/value store
needs: read one key, write one key, delete one key. It is deliberately small.
Three methods is at the upper end of what a single interface should carry; a later
module splits it into `KeyReader`/`KeyWriter`/`KeyDeleter` for consumers that need
only one. Here we keep all three together because the demo and tests exercise the
full round-trip.

`MemoryStore` holds a `map[string]string` and a `sync.RWMutex`. The read path
(`Get`) takes the read lock (`RLock`), so concurrent reads do not block each
other; the write paths (`Set`, `Delete`) take the exclusive lock. The methods are
on `*MemoryStore`, not `MemoryStore`, for two reasons: a `sync.RWMutex` must not
be copied after first use (copying it is a bug `go vet` catches), and the methods
mutate shared state, so every caller must operate on the same underlying map. That
is why the interface is satisfied by `*MemoryStore` and not by `MemoryStore{}` — a
distinction the next module drills into.

`Get` returns `ErrNotFound` (a package-level sentinel) when the key is absent, so
callers can branch on `errors.Is(err, ErrNotFound)` rather than string-matching.
`Delete` on a missing key is a no-op that returns `nil`: deleting something that is
not there is not an error, and making it idempotent means a retry or a double-
delete does not fail.

Create `storage.go`:

```go
package storageiface

import (
	"errors"
	"sync"
)

// ErrNotFound is returned by Get when the key is absent.
var ErrNotFound = errors.New("key not found")

// Store is the consumer-defined interface: the three operations a caller of a
// key/value store needs. A producer satisfies it implicitly by method set.
type Store interface {
	Get(key string) (string, error)
	Set(key, value string) error
	Delete(key string) error
}

// MemoryStore is an in-memory, concurrency-safe Store backed by a map guarded
// by a sync.RWMutex. Its methods take a pointer receiver: the mutex must not be
// copied, and every caller must share the same map.
type MemoryStore struct {
	mu   sync.RWMutex
	data map[string]string
}

// NewMemoryStore returns a ready-to-use *MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{data: make(map[string]string)}
}

// Get returns the value for key, or ErrNotFound if it is absent.
func (m *MemoryStore) Get(key string) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	v, ok := m.data[key]
	if !ok {
		return "", ErrNotFound
	}
	return v, nil
}

// Set stores value under key, overwriting any existing value.
func (m *MemoryStore) Set(key, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[key] = value
	return nil
}

// Delete removes key. Deleting a missing key is a no-op and returns nil.
func (m *MemoryStore) Delete(key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, key)
	return nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/storageiface"
)

func main() {
	// A *MemoryStore is used through the Store interface, proving implicit
	// satisfaction at the assignment.
	var s storageiface.Store = storageiface.NewMemoryStore()

	_ = s.Set("session:alice", "token-123")

	v, err := s.Get("session:alice")
	if err != nil {
		fmt.Println("unexpected error:", err)
		return
	}
	fmt.Printf("got: %s\n", v)

	_ = s.Delete("session:alice")

	if _, err := s.Get("session:alice"); errors.Is(err, storageiface.ErrNotFound) {
		fmt.Println("after delete: not found")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
got: token-123
after delete: not found
```

### Tests

`TestMemoryStoreSatisfiesStoreInterface` is the pin: it declares a `Store`
variable and assigns `NewMemoryStore()` to it. The assignment is where the
compiler proves the method set matches; if a method signature drifted, this line
would stop compiling. `TestMemoryStoreDeleteMissingKeyIsNoop` pins the idempotent-
delete contract. `TestMemoryStoreIsSafeUnderConcurrentSet` fires 100 goroutines at
`Set` so `go test -race` can prove the mutex actually guards the map.

Create `storage_test.go`:

```go
package storageiface

import (
	"errors"
	"sync"
	"testing"
)

func TestMemoryStoreSetGetDelete(t *testing.T) {
	t.Parallel()

	s := NewMemoryStore()
	if err := s.Set("a", "1"); err != nil {
		t.Fatal(err)
	}
	v, err := s.Get("a")
	if err != nil {
		t.Fatal(err)
	}
	if v != "1" {
		t.Fatalf("Get = %q, want 1", v)
	}
	if err := s.Delete("a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestMemoryStoreReturnsNotFoundForMissingKey(t *testing.T) {
	t.Parallel()

	s := NewMemoryStore()
	if _, err := s.Get("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestMemoryStoreSatisfiesStoreInterface(t *testing.T) {
	t.Parallel()

	var s Store = NewMemoryStore()
	if err := s.Set("a", "1"); err != nil {
		t.Fatal(err)
	}
	v, err := s.Get("a")
	if err != nil {
		t.Fatal(err)
	}
	if v != "1" {
		t.Fatalf("Get = %q, want 1", v)
	}
}

func TestMemoryStoreDeleteMissingKeyIsNoop(t *testing.T) {
	t.Parallel()

	s := NewMemoryStore()
	if err := s.Delete("never-existed"); err != nil {
		t.Fatalf("Delete(missing) = %v, want nil", err)
	}
}

func TestMemoryStoreIsSafeUnderConcurrentSet(t *testing.T) {
	t.Parallel()

	s := NewMemoryStore()
	const n = 100
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key := string(rune('a' + (i % 26)))
			_ = s.Set(key, "v")
		}()
	}
	wg.Wait()
}
```

## Review

The store is correct when `Get` returns `ErrNotFound` exactly for absent keys and
the stored value otherwise, `Set` overwrites, and `Delete` is an idempotent no-op.
The implicit-satisfaction claim is proved by `var s Store = NewMemoryStore()`
compiling and working; if you changed a method's signature the assignment would
fail to build, which is the whole point of a consumer-defined interface — the
contract is checked at the seam. The most common mistake here is putting the
methods on a value receiver and then wondering why `MemoryStore{}` "doesn't
implement Store" or why copies do not share state; the pointer receiver is
mandatory for a type holding a mutex. Run `go test -race` to confirm the
`RWMutex` guards the map under the 100-goroutine `Set` storm.

## Resources

- [Go Specification: Interface types](https://go.dev/ref/spec#Interface_types) — method sets and structural satisfaction.
- [`sync.RWMutex`](https://pkg.go.dev/sync#RWMutex) — read/write lock semantics.
- [`errors.Is`](https://pkg.go.dev/errors#Is) — matching a wrapped sentinel error.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-accept-interfaces-return-structs.md](02-accept-interfaces-return-structs.md)
