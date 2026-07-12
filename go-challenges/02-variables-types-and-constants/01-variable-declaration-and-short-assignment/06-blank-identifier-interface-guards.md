# Exercise 6: Blank Identifier for Interface Guards and Discarded Returns

The blank identifier is a declaration tool, not a lint silencer. This exercise uses
`var _ Store = (*PostgresStore)(nil)` as a zero-cost compile-time guard that an
implementation is complete, and uses `_` to discard a return only where its
irrelevance is provable — contrasting the safe case with the one that hides a real
failure.

This module is fully self-contained. It has its own `go mod init`, defines every
type it needs, and ships its own demo and tests. Nothing here imports any other
exercise.

## What you'll build

```text
storage/                       independent module: example.com/storage
  go.mod                       module example.com/storage
  storage.go                   Store interface; guards var _ Store = (*T)(nil)
  memory.go                    MemoryStore implementation
  pg.go                        PostgresStore (in-memory fake standing in for PG)
  cmd/
    demo/
      main.go                  both stores behave identically through the interface
  storage_test.go              both satisfy Store; identical behavior; discard cases
```

- Files: `storage.go`, `memory.go`, `pg.go`, `cmd/demo/main.go`, `storage_test.go`.
- Implement: a `Store` interface, two implementations, compile-time guards `var _ Store = (*MemoryStore)(nil)` and `var _ Store = (*PostgresStore)(nil)`, and a serializer that discards `bytes.Buffer.Write`'s always-nil error correctly.
- Test: assign both concrete types to `Store`; prove identical behavior for the same operations; show a discard that is provably safe vs one that would not be.
- Verify: `go test -count=1 -race ./...`

### Why the interface guard exists

`var _ Store = (*PostgresStore)(nil)` declares a variable named `_` (discarded)
whose declared type is `Store` and whose value is a typed nil `*PostgresStore`. The
assignment forces the compiler to check that `*PostgresStore` implements `Store`.
Because the variable is `_`, nothing is allocated and nothing is kept — it is a
pure compile-time assertion. Its payoff is *where* the failure surfaces: without the
guard, an incomplete implementation compiles fine until some distant call site tries
to pass a `*PostgresStore` as a `Store`, and the error points at the call site, not
the type. With the guard, adding a method to `Store` and forgetting to implement it
on `*PostgresStore` fails to build *in the file that owns the type*, with a message
that names the missing method. It is the cheapest possible early-warning for an API
contract.

The typed-nil form `(*PostgresStore)(nil)` matters: it asserts that the *pointer*
type satisfies the interface (which is what callers hold), without constructing a
real value. If the methods have pointer receivers — as they usually do — only the
pointer type satisfies the interface, and this is the form that checks it.

### Why `_` for a discarded return is sometimes right and sometimes a bug

Discarding a return with `_` is honest only when the discarded value is provably
irrelevant. Writing to a `bytes.Buffer` is the textbook safe case: `bytes.Buffer`'s
`Write` documents that it always returns a nil error (it panics only on an
impossible out-of-memory), so `n, _ := buf.Write(b)` — or the common
`buf.Write(b)` with no capture — hides nothing. The same pattern on a real network
connection is a bug: `conn.Write` can perform a short write or fail on a reset, and
`n, _ := conn.Write(b)` throws away exactly the information you need to detect it.
The rule is not "never discard"; it is "discard only what you can prove you do not
need".

Create `storage.go`:

```go
package storage

import "errors"

// ErrNotFound is returned when a key is absent.
var ErrNotFound = errors.New("key not found")

// Store is a minimal key/value contract.
type Store interface {
	Put(key, value string)
	Get(key string) (string, error)
	Len() int
}

// Compile-time guards: if either type stops satisfying Store, the package fails to
// build here, in the file that owns the contract, rather than at a distant caller.
var (
	_ Store = (*MemoryStore)(nil)
	_ Store = (*PostgresStore)(nil)
)
```

Create `memory.go`:

```go
package storage

import "sync"

// MemoryStore is an in-memory Store.
type MemoryStore struct {
	mu   sync.RWMutex
	data map[string]string
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{data: make(map[string]string)}
}

func (s *MemoryStore) Put(key, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = value
}

func (s *MemoryStore) Get(key string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.data[key]
	if !ok {
		return "", ErrNotFound
	}
	return v, nil
}

func (s *MemoryStore) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.data)
}
```

Create `pg.go`:

```go
package storage

import (
	"bytes"
	"fmt"
	"sync"
)

// PostgresStore stands in for a real Postgres-backed Store; here it keeps rows in
// memory so the module builds and tests offline, while modeling the same contract.
type PostgresStore struct {
	mu   sync.RWMutex
	rows map[string]string
}

func NewPostgresStore() *PostgresStore {
	return &PostgresStore{rows: make(map[string]string)}
}

func (s *PostgresStore) Put(key, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows[key] = value
}

func (s *PostgresStore) Get(key string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.rows[key]
	if !ok {
		return "", ErrNotFound
	}
	return v, nil
}

func (s *PostgresStore) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.rows)
}

// Dump serializes the store as key=value lines. Writing to a bytes.Buffer never
// fails, so discarding Write's error with _ is provably safe here; the same
// discard on a real net.Conn would hide short writes and is a bug.
func Dump(s Store, keys []string) string {
	var buf bytes.Buffer
	for _, k := range keys {
		v, err := s.Get(k)
		if err != nil {
			continue
		}
		// bytes.Buffer.Write documents a nil error: the discard is honest.
		_, _ = fmt.Fprintf(&buf, "%s=%s\n", k, v)
	}
	return buf.String()
}
```

### The runnable demo

Both concrete types are driven purely through the `Store` interface and behave
identically, which is exactly what the compile-time guard makes safe to rely on.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/storage"
)

func run(name string, s storage.Store) {
	s.Put("region", "eu-west-1")
	v, _ := s.Get("region")
	_, err := s.Get("missing")
	fmt.Printf("%s: region=%s len=%d missing=%v\n", name, v, s.Len(), errors.Is(err, storage.ErrNotFound))
}

func main() {
	run("memory", storage.NewMemoryStore())
	run("postgres", storage.NewPostgresStore())

	pg := storage.NewPostgresStore()
	pg.Put("a", "1")
	pg.Put("b", "2")
	fmt.Print(storage.Dump(pg, []string{"a", "b", "gone"}))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
memory: region=eu-west-1 len=1 missing=true
postgres: region=eu-west-1 len=1 missing=true
a=1
b=2
```

### Tests

The behavioral test runs the *same* table against both implementations through the
`Store` interface, which is what makes the guard meaningful: the guard proves they
satisfy the contract, the test proves they satisfy it *identically*.

Create `storage_test.go`:

```go
package storage

import (
	"errors"
	"fmt"
	"testing"
)

// storeConstructors gives each Store implementation a fresh instance.
var storeConstructors = map[string]func() Store{
	"memory":   func() Store { return NewMemoryStore() },
	"postgres": func() Store { return NewPostgresStore() },
}

func TestStoresBehaveIdentically(t *testing.T) {
	t.Parallel()
	for name, ctor := range storeConstructors {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := ctor()

			if _, err := s.Get("nope"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("Get miss = %v, want ErrNotFound", err)
			}
			s.Put("k", "v")
			if got, err := s.Get("k"); err != nil || got != "v" {
				t.Fatalf("Get(k) = %q,%v; want v,nil", got, err)
			}
			if s.Len() != 1 {
				t.Fatalf("Len = %d, want 1", s.Len())
			}
		})
	}
}

func TestGuardsCompile(t *testing.T) {
	t.Parallel()
	// These assignments only compile if the guards in storage.go hold. They also
	// document, at runtime, that the pointer types satisfy Store.
	var s Store = NewMemoryStore()
	if _, ok := s.(*MemoryStore); !ok {
		t.Fatal("MemoryStore does not satisfy Store")
	}
	s = NewPostgresStore()
	if _, ok := s.(*PostgresStore); !ok {
		t.Fatal("PostgresStore does not satisfy Store")
	}
}

func TestDumpSkipsMissing(t *testing.T) {
	t.Parallel()
	pg := NewPostgresStore()
	pg.Put("a", "1")
	if got, want := Dump(pg, []string{"a", "gone"}), "a=1\n"; got != want {
		t.Fatalf("Dump = %q, want %q", got, want)
	}
}

func ExampleDump() {
	m := NewMemoryStore()
	m.Put("x", "1")
	fmt.Print(Dump(m, []string{"x"}))
	// Output: x=1
}
```

## Review

The design is correct when the guards catch an incomplete implementation at build
time and the discards are provably safe. `var _ Store = (*T)(nil)` is a zero-cost
assertion in the file that owns the contract, so removing a method from a type turns
into a local compile error rather than a distant one. `Dump` discards
`Fprintf`/`Write`'s error only because a `bytes.Buffer` cannot fail; the same
discard on a real connection would be a bug.

The mistakes to avoid: using `_` to silence an error you actually need (a short
write on a network writer), and writing the guard with a value form instead of the
typed nil `(*T)(nil)` when methods have pointer receivers. Run `go test -race` to
confirm both implementations are concurrency-safe and behave identically.

## Resources

- [Effective Go: Interface checks (the blank-identifier guard)](https://go.dev/doc/effective_go#blank_implements)
- [Go Specification: Blank identifier](https://go.dev/ref/spec#Blank_identifier)
- [bytes.Buffer.Write (documented nil error)](https://pkg.go.dev/bytes#Buffer.Write)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [05-grouped-var-build-metadata.md](05-grouped-var-build-metadata.md) | Next: [07-multiple-assignment-address-parsing.md](07-multiple-assignment-address-parsing.md)
