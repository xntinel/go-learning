# Exercise 1: A Hermetic Repository Fixture with t.Cleanup

Every repository or database-layer test a senior writes starts from the same
shape: a fixture that opens the store, and a guarantee that it closes at test end
no matter how the test exits. This exercise builds that canonical fixture — an
in-memory key/value repository plus a `t.Helper()`-marked constructor that opens
it and registers the close as a `t.Cleanup`.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports any other exercise.

## What you'll build

```text
repofixture/                 independent module: example.com/repofixture
  go.mod                     go 1.24
  store.go                   Store (Put/Get/Close), ErrNotFound, ErrClosed, idempotent Close
  cmd/
    demo/
      main.go                runnable demo: put, get, close, reject-after-close
  store_test.go              newTestStore(t) fixture + table tests + Example
```

- Files: `store.go`, `cmd/demo/main.go`, `store_test.go`.
- Implement: a `Store` with `NewStore`, `Put`, `Get`, and an idempotent `Close`, plus sentinel errors `ErrNotFound` and `ErrClosed` wrapped where relevant.
- Test: a `newTestStore(t)` helper that registers `t.Cleanup(store.Close)`; table tests for put/get, missing key, and reject-after-close; a test proving `Close` is idempotent.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why the fixture owns the close

The design choice that matters is who is responsible for teardown. If the test
body opens the store and the test body closes it, then every test must remember to
close, every early `return` must close first, and every `t.Fatal` skips the close.
That is three ways to leak a resource. The fix is to move ownership into the
fixture: `newTestStore(t)` opens the store *and* registers `t.Cleanup(func(){
store.Close() })` before it returns. From that point the caller cannot forget to
close, cannot leak on an early return, and cannot leak on a `Fatal` — the cleanup
runs at test completion on every path.

`Close` must be idempotent. The fixture always closes, but a test may legitimately
close the store itself mid-test — for example, to assert that operations are
rejected afterward. If `Close` errored on the second call, that test would fail its
own cleanup. So `Close` returns `nil` when the store is already closed, and only
the fixture's cleanup is guaranteed to run; a redundant explicit close is harmless.

Create `store.go`:

```go
package repofixture

import "errors"

// Sentinel errors let callers branch with errors.Is regardless of wrapping.
var (
	ErrNotFound = errors.New("repofixture: key not found")
	ErrClosed   = errors.New("repofixture: store closed")
)

// Store is an in-memory key/value repository. It stands in for a real
// database-backed store in tests: open it, use it, and close it exactly once.
type Store struct {
	data map[string]string
	open bool
}

// NewStore returns an open, empty store.
func NewStore() *Store {
	return &Store{data: make(map[string]string), open: true}
}

// Put stores value under key. It returns ErrClosed if the store is closed.
func (s *Store) Put(key, value string) error {
	if !s.open {
		return ErrClosed
	}
	s.data[key] = value
	return nil
}

// Get returns the value for key, ErrNotFound if absent, or ErrClosed if closed.
func (s *Store) Get(key string) (string, error) {
	if !s.open {
		return "", ErrClosed
	}
	v, ok := s.data[key]
	if !ok {
		return "", ErrNotFound
	}
	return v, nil
}

// Close releases the store. It is idempotent: a second call returns nil, so a
// fixture cleanup can always close without risking a double-close error.
func (s *Store) Close() error {
	if !s.open {
		return nil
	}
	s.open = false
	s.data = nil
	return nil
}
```

### The runnable demo

The demo exercises the exported API end to end: put a value, read it back, observe
a missing key, then close and confirm operations are rejected.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/repofixture"
)

func main() {
	s := repofixture.NewStore()

	_ = s.Put("user:1", "alice")
	if v, err := s.Get("user:1"); err == nil {
		fmt.Printf("user:1 = %s\n", v)
	}

	if _, err := s.Get("user:2"); errors.Is(err, repofixture.ErrNotFound) {
		fmt.Println("user:2 not found")
	}

	_ = s.Close()
	if err := s.Put("user:3", "carol"); errors.Is(err, repofixture.ErrClosed) {
		fmt.Println("put after close rejected")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
user:1 = alice
user:2 not found
put after close rejected
```

### The tests

`newTestStore` is the whole point: it is a `t.Helper()` so failures report the
caller's line, and it registers `t.Cleanup(store.Close)` so every test that uses it
is hermetic for free. `TestStoreRejectsOperationsAfterClose` closes the store
mid-test and asserts `ErrClosed`, which both exercises the closed path and proves
the fixture's second close (in the cleanup) does not panic. `TestStoreCloseIdempotent`
pins the idempotency contract directly.

Create `store_test.go`:

```go
package repofixture

import (
	"errors"
	"fmt"
	"testing"
)

// newTestStore opens a store and registers its close as a cleanup, so callers
// get a hermetic fixture without owning teardown.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	s := NewStore()
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("store close: %v", err)
		}
	})
	return s
}

func TestStorePutGet(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		key   string
		value string
	}{
		{"simple", "user:1", "alice"},
		{"empty value", "user:2", ""},
		{"namespaced key", "session:abc", "token-xyz"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			if err := s.Put(tc.key, tc.value); err != nil {
				t.Fatalf("Put(%q): %v", tc.key, err)
			}
			got, err := s.Get(tc.key)
			if err != nil {
				t.Fatalf("Get(%q): %v", tc.key, err)
			}
			if got != tc.value {
				t.Fatalf("Get(%q) = %q, want %q", tc.key, got, tc.value)
			}
		})
	}
}

func TestStoreMissingKey(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	if _, err := s.Get("absent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(absent) err = %v, want ErrNotFound", err)
	}
}

func TestStoreRejectsOperationsAfterClose(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	if err := s.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := s.Put("k", "v"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Put after Close err = %v, want ErrClosed", err)
	}
	if _, err := s.Get("k"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Get after Close err = %v, want ErrClosed", err)
	}
}

func TestStoreCloseIdempotent(t *testing.T) {
	t.Parallel()
	s := NewStore()
	if err := s.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close should be nil, got %v", err)
	}
}

func ExampleStore() {
	s := NewStore()
	_ = s.Put("region", "eu-west-1")
	v, _ := s.Get("region")
	fmt.Println(v)
	// Output: eu-west-1
}
```

## Review

The fixture is correct when no test in the file owns teardown yet every store is
closed exactly at test end. The proof is `TestStoreRejectsOperationsAfterClose`:
it closes the store explicitly, and the fixture's cleanup closes it a second time —
if `Close` were not idempotent, that second close would surface as a cleanup error
and fail the test. The mistakes to avoid are structural: do not close the store in
the test body and expect the fixture not to close it too (make `Close` idempotent
instead); do not reach for `defer store.Close()` here, because the moment you add a
parallel subtest that shares the store, the defer fires too early. Run
`go test -race` to confirm the fixture composes cleanly across the parallel
subtests of `TestStorePutGet`.

## Resources

- [`testing.T.Cleanup`](https://pkg.go.dev/testing#T.Cleanup) — registration, LIFO order, and running on failure.
- [`testing.T.Helper`](https://pkg.go.dev/testing#T.Helper) — why fixtures mark themselves as helpers.
- [`errors.Is`](https://pkg.go.dev/errors#Is) — matching sentinel errors through wrapping.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-tempdir-config-scratch.md](02-tempdir-config-scratch.md)
