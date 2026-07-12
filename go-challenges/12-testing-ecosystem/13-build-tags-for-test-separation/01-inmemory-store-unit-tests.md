# Exercise 1: The Store and its default-build unit tests

Every tiered test suite is measured against one baseline: the fast, hermetic unit
tests that run under a plain `go test ./...` with no tags. This module builds that
baseline — a concurrency-safe in-memory store and the untagged tests that exercise
it — so the later tiers have something concrete to be separated *from*.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
kvstore/                   independent module: example.com/kvstore
  go.mod
  store.go                 ErrNotFound sentinel; type Store; NewStore, Put, Get, Len
  store_test.go            untagged unit tests: TestPutAndGet, TestGetReturnsNotFound, race test, Example
  cmd/
    demo/
      main.go              stores a session, reads it back, shows a wrapped ErrNotFound
```

- Files: `store.go`, `store_test.go`, `cmd/demo/main.go`.
- Implement: a concurrency-safe `Store` with `NewStore`, `Put(key, value)`, `Get(key) (string, error)` wrapping `ErrNotFound` with `%w`, and `Len()`.
- Test: `TestPutAndGet` and `TestGetReturnsNotFound` (both `t.Parallel`), a `-race` concurrency test, and a testable `Example`.
- Verify: `go test -count=1 -race ./...` runs both named tests with no `-tags` flag.

### Why this tier is the untagged baseline

The store is the in-memory stand-in a unit test reaches for instead of a real
database: it is deterministic, needs no external process, and returns in
nanoseconds. Because it lives in ordinary, untagged files, it is part of the
*default build graph* — `go build ./...`, `go vet ./...`, and `go test ./...`
compile and run it with no flags at all. That is the whole point of this module:
it establishes the tier that must stay fast, so that when later modules push slow
database and end-to-end tests behind `//go:build integration`, you can prove the
default run still contains exactly these tests and nothing heavier.

`Get` wraps its sentinel with `%w` — `fmt.Errorf("get %q: %w", key, ErrNotFound)`
— so a caller can branch on the category with `errors.Is(err, ErrNotFound)` while
the message still carries the offending key. That is the sentinel-plus-`%w`
contract the rest of the chapter assumes. The `sync.RWMutex` makes concurrent
`Get` calls cheap (read lock) while `Put` takes the write lock, and the `-race`
test exists to prove the mutex actually guards the map.

Create `store.go`:

```go
package kvstore

import (
	"errors"
	"fmt"
	"sync"
)

// ErrNotFound is returned (wrapped) by Get when a key is absent.
var ErrNotFound = errors.New("kvstore: key not found")

// Store is a concurrency-safe in-memory key/value store. It is the fast,
// hermetic stand-in a unit test uses in place of a real database.
type Store struct {
	mu   sync.RWMutex
	data map[string]string
}

// NewStore returns an empty store ready for use.
func NewStore() *Store {
	return &Store{data: make(map[string]string)}
}

// Put stores value under key, overwriting any previous value.
func (s *Store) Put(key, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = value
}

// Get returns the value stored under key. If the key is absent it returns the
// empty string and an error wrapping ErrNotFound.
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

The demo stores a session, reads it back, then asks for a missing key to show the
wrapped sentinel surfacing through `errors.Is`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/kvstore"
)

func main() {
	s := kvstore.NewStore()
	s.Put("session:1", "alice")
	fmt.Println("stored session:1 -> alice")

	if v, err := s.Get("session:1"); err == nil {
		fmt.Printf("get session:1 -> %s\n", v)
	}

	_, err := s.Get("missing")
	fmt.Printf("get missing -> not found (errors.Is ErrNotFound = %v)\n", errors.Is(err, kvstore.ErrNotFound))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
stored session:1 -> alice
get session:1 -> alice
get missing -> not found (errors.Is ErrNotFound = true)
```

### Tests

`TestPutAndGet` and `TestGetReturnsNotFound` are the two named unit tests that
must run in the default `go test ./...`. Both call `t.Parallel()` because they
share no state. `TestConcurrentPutGet` drives 100 goroutines through `Put`/`Get`
to give `-race` something to inspect, and the `Example` is auto-verified against
its `// Output:` comment.

Create `store_test.go`:

```go
package kvstore

import (
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"
)

func TestPutAndGet(t *testing.T) {
	t.Parallel()
	s := NewStore()
	s.Put("session:1", "alice")

	got, err := s.Get("session:1")
	if err != nil {
		t.Fatalf("Get after Put: unexpected error: %v", err)
	}
	if got != "alice" {
		t.Fatalf("Get = %q, want %q", got, "alice")
	}
}

func TestGetReturnsNotFound(t *testing.T) {
	t.Parallel()
	s := NewStore()

	_, err := s.Get("missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(missing) error = %v, want a wrap of ErrNotFound", err)
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
			k := strconv.Itoa(i)
			s.Put(k, k)
			_, _ = s.Get(k)
		}()
	}
	wg.Wait()
	if s.Len() != 100 {
		t.Fatalf("Len = %d, want 100", s.Len())
	}
}

func ExampleStore() {
	s := NewStore()
	s.Put("region", "eu-west-1")

	v, _ := s.Get("region")
	fmt.Println(v, s.Len())
	// Output: eu-west-1 1
}
```

## Review

The tier is correct when the two named unit tests run with no `-tags` flag and the
suite stays hermetic: no file here imports a driver, opens a socket, or reads the
clock, so `go test ./...` is deterministic and fast. `Get` returns a wrap of
`ErrNotFound` exactly when the key is absent, which is why `TestGetReturnsNotFound`
asserts with `errors.Is` rather than string-matching the message — the message can
change, the sentinel identity must not. The `-race` test is not decoration: remove
the `RWMutex` and it fails, proving the lock guards the map. Every later module
measures its tagged tiers against this baseline; keep it clean and fast.

## Resources

- [testing package](https://pkg.go.dev/testing) — `T.Parallel`, `T.Fatalf`, and the `Example`/`// Output:` mechanism.
- [errors package](https://pkg.go.dev/errors) — `errors.New` and `errors.Is` for sentinel matching.
- [sync.RWMutex](https://pkg.go.dev/sync#RWMutex) — the read/write lock guarding the map.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-integration-tag-env-gate.md](02-integration-tag-env-gate.md)
