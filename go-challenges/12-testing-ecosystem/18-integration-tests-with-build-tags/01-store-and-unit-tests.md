# Exercise 1: The In-Memory Store And Its Default-Build Unit Tests

Before there is an integration tier, there is the hermetic tier the integration
tier will mirror: a keyed store with a wrapped sentinel error, tested in the
default build with no tags, no environment, and no external dependency. This is the
baseline `go test ./...` gate that must stay sub-second.

This module is fully self-contained: its own `go mod init`, its own demo, and its
own tests. Nothing here imports any other exercise.

## What you'll build

```text
intgtest/                  independent module: example.com/intgtest
  go.mod
  store.go                 Store (Put, Get, Len) with wrapped ErrNotFound
  cmd/
    demo/
      main.go              stores and reads a record back
  store_test.go            TestPutAndGet, TestGetMissing (errors.Is), ExampleStore
```

- Files: `store.go`, `cmd/demo/main.go`, `store_test.go`.
- Implement: a concurrency-safe `Store` with `Put`, `Get` returning a wrapped `ErrNotFound`, and `Len`.
- Test: assert a Put-then-Get round-trip, assert a missing key returns `ErrNotFound` via `errors.Is`, and add an `ExampleStore`.
- Verify: `gofmt -l .`, `go vet ./...`, `go test -count=1 -race ./...` — all pass with zero tags and zero env.

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/18-integration-tests-with-build-tags/01-store-and-unit-tests/cmd/demo
cd go-solutions/12-testing-ecosystem/18-integration-tests-with-build-tags/01-store-and-unit-tests
```

### Why the hermetic tier comes first

The integration tier exists to test the code path that a fake cannot: a real
`INSERT`, a real transaction, a real constraint violation. Everything *else* — the
key encoding, the not-found contract, the concurrency guard — belongs in the fast
tier, where it is tested in microseconds with no database. Getting this split right
is the whole discipline: the more behavior you can pin in the default build, the
smaller and faster the integration tier is, and the fewer places you can flake.

Two details make this a real artifact rather than a toy. First, `Get` returns
`ErrNotFound` *wrapped* with `%w`, so callers can match it with `errors.Is`
regardless of how many layers wrap it on the way up — this is the sentinel-error
contract every repository layer in a real service exposes. Second, the store is
guarded by a `sync.RWMutex`, so `go test -race` actually exercises the guard under
concurrent access; a map with no lock passes a single-threaded test and corrupts in
production.

Create `store.go`:

```go
package intgtest

import (
	"errors"
	"fmt"
	"sync"
)

// ErrNotFound is returned (wrapped) by Get when a key is absent. Callers match it
// with errors.Is, never by string comparison.
var ErrNotFound = errors.New("intgtest: key not found")

// Store is the fast, hermetic in-memory tier. It has the same shape a real
// database-backed store exposes, so the integration tier can mirror it.
type Store struct {
	mu   sync.RWMutex
	data map[string]string
}

// NewStore returns an empty, ready-to-use Store.
func NewStore() *Store {
	return &Store{data: make(map[string]string)}
}

// Put stores value under key, overwriting any existing value.
func (s *Store) Put(key, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = value
}

// Get returns the value stored under key, or a wrapped ErrNotFound if absent.
func (s *Store) Get(key string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.data[key]
	if !ok {
		return "", fmt.Errorf("get %q: %w", key, ErrNotFound)
	}
	return v, nil
}

// Len reports the number of stored keys.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.data)
}
```

### The runnable demo

The demo touches only the exported API, so it doubles as a check that the surface
is usable from another package.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/intgtest"
)

func main() {
	s := intgtest.NewStore()
	s.Put("acct:1", "alice")

	v, err := s.Get("acct:1")
	fmt.Printf("get acct:1 -> %s (len=%d)\n", v, s.Len())

	if _, err = s.Get("acct:404"); errors.Is(err, intgtest.ErrNotFound) {
		fmt.Println("get acct:404 -> not found")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
get acct:1 -> alice (len=1)
get acct:404 -> not found
```

### Tests

The tests are table-driven where it helps and assert the not-found path through
`errors.Is` against the sentinel — never against the error string. The `Example`
is auto-verified by `go test` against its `// Output:` comment.

Create `store_test.go`:

```go
package intgtest

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

func TestPutAndGet(t *testing.T) {
	t.Parallel()
	s := NewStore()
	s.Put("k1", "v1")

	got, err := s.Get("k1")
	if err != nil {
		t.Fatalf("Get(k1) returned error: %v", err)
	}
	if got != "v1" {
		t.Fatalf("Get(k1) = %q, want v1", got)
	}
	if n := s.Len(); n != 1 {
		t.Fatalf("Len() = %d, want 1", n)
	}
}

func TestGetMissing(t *testing.T) {
	t.Parallel()
	s := NewStore()

	_, err := s.Get("absent")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(absent) error = %v, want wrapped ErrNotFound", err)
	}
}

func TestConcurrentPutGet(t *testing.T) {
	t.Parallel()
	s := NewStore()
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key := fmt.Sprintf("k%d", i)
			s.Put(key, "v")
			_, _ = s.Get(key)
		}()
	}
	wg.Wait()
	if n := s.Len(); n != 100 {
		t.Fatalf("Len() = %d, want 100", n)
	}
}

func ExampleStore() {
	s := NewStore()
	s.Put("answer", "42")
	v, _ := s.Get("answer")
	fmt.Println(v)
	// Output: 42
}
```

## Review

The store is correct when `Get` returns the stored value for a present key and a
value that satisfies `errors.Is(err, ErrNotFound)` for an absent one, and when
`Len` reflects exactly the keys written. The mistake to avoid is asserting the
not-found case by comparing the error string: that breaks the moment a caller wraps
the error with more context, which is exactly why `Get` uses `%w` and the tests use
`errors.Is`. Run `go test -race` to confirm the `RWMutex` actually guards the map —
drop the lock and the concurrency test fails the race detector. This hermetic tier
is the reference shape for the integration tier in the exercises that follow: same
methods, same sentinel, but backed by a real database behind a build tag.

## Resources

- [testing](https://pkg.go.dev/testing) — `T`, `T.Parallel`, `T.Fatalf`, and the `Example`/`// Output:` mechanism.
- [errors.Is](https://pkg.go.dev/errors#Is) — matching a wrapped sentinel error.
- [fmt.Errorf and %w](https://pkg.go.dev/fmt#Errorf) — wrapping a sentinel so callers can unwrap it.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-integration-tag-env-gate.md](02-integration-tag-env-gate.md)
