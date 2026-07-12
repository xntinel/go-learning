# Exercise 2: The Refactor — a Concrete Store With No Interface

The store from Exercise 1 has exactly one implementation and no test that
substitutes for it, so the interface earns nothing. This module builds the
refactored version: a concrete `Store` with three real methods, no interface, no
stubs. The point it proves is the one engineers most often disbelieve — a single
implementation is fully testable with no interface at all.

## What you'll build

```text
after/                      independent module: example.com/after
  go.mod                    go 1.26
  store.go                  concrete Store; NewStore; Get/Put/Delete (no interface)
  cmd/
    demo/
      main.go               drives *Store directly
  store_test.go             put/get round-trip and delete, all on *Store, no mock
```

- Files: `store.go`, `cmd/demo/main.go`, `store_test.go`.
- Implement: a concrete `Store` struct with `NewStore` and exactly three methods — `Get`, `Put`, `Delete` — and no interface, no stubs.
- Test: operate directly on `*Store`; `Put` then `Get` round-trips, `Delete` then `Get` returns empty. No mock and no interface satisfaction anywhere.
- Verify: `go test -count=1 -race ./...`

### Why deleting the interface is the improvement

Compare this to Exercise 1 method by method. The fat `Service` interface is gone.
The two do-nothing stubs (`Backup`, `Restore`) are gone, because nothing forces
them to exist. `List`, `Stats`, and `Reset` — which the real callers did not need
— are gone too; add them back only when a caller actually calls them. What remains
is a concrete type with the three operations a key-value store genuinely provides.

The reflex objection is "but now it is not testable / not mockable." The test
below refutes that directly: it constructs a `*Store`, calls its methods, and
asserts on the results, with no interface and no mock anywhere. A single concrete
implementation is testable precisely because you can build it and call it. The
interface would only start to earn its place when a second implementation appears
or when a consumer needs to fake this dependency at a boundary — and at that point
the right move is a narrow, consumer-side interface (Exercise 4), not the fat
producer-side one this refactor deleted.

Returning `*Store` from `NewStore` (not some `Store` interface) is the "return
structs" half of "accept interfaces, return structs." Callers get every method,
including any you add later — Exercise 8 adds a method to a concrete type to make
exactly this point — and they get real go-to-definition on `Get`.

Create `store.go`:

```go
package after

import "context"

// Store is a concrete key-value store. There is no interface: it has a single
// implementation and is fully testable by constructing and calling it directly.
type Store struct {
	data map[string]string
}

func NewStore() *Store {
	return &Store{data: make(map[string]string)}
}

// Get returns the value for id, or ("", nil) if the key is absent.
func (s *Store) Get(ctx context.Context, id string) (string, error) {
	v, ok := s.data[id]
	if !ok {
		return "", nil
	}
	return v, nil
}

// Put stores value under id.
func (s *Store) Put(ctx context.Context, id, value string) error {
	s.data[id] = value
	return nil
}

// Delete removes id if present.
func (s *Store) Delete(ctx context.Context, id string) error {
	delete(s.data, id)
	return nil
}
```

### The runnable demo

The demo uses `*Store` directly — no interface variable — so the reader sees the
concrete type flow through the program with full method visibility.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/after"
)

func main() {
	ctx := context.Background()
	s := after.NewStore()

	if err := s.Put(ctx, "u1", "alice"); err != nil {
		panic(err)
	}
	v, _ := s.Get(ctx, "u1")
	fmt.Printf("get u1: %s\n", v)

	if err := s.Delete(ctx, "u1"); err != nil {
		panic(err)
	}
	v, _ = s.Get(ctx, "u1")
	fmt.Printf("get u1 after delete: %q\n", v)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
get u1: alice
get u1 after delete: ""
```

### Tests

The tests operate on `*Store` directly. There is no `var s Store = ...` line
because there is no interface; there is no mock because the concrete type is the
thing under test. `TestPutGetRoundTrip` is table-driven over stored values;
`TestDeleteRemovesKey` deletes and asserts the follow-up `Get` returns empty.
`ExampleStore_Get` pins the round-trip output so `go test` verifies the snippet too.

Create `store_test.go`:

```go
package after

import (
	"context"
	"fmt"
	"testing"
)

func TestPutGetRoundTrip(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		id    string
		value string
	}{
		{name: "alice", id: "u1", value: "alice"},
		{name: "bob", id: "u2", value: "bob"},
		{name: "empty value", id: "u3", value: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			s := NewStore()

			if err := s.Put(ctx, tc.id, tc.value); err != nil {
				t.Fatalf("Put: %v", err)
			}
			got, err := s.Get(ctx, tc.id)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got != tc.value {
				t.Fatalf("Get(%q) = %q, want %q", tc.id, got, tc.value)
			}
		})
	}
}

func TestDeleteRemovesKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := NewStore()

	if err := s.Put(ctx, "u1", "alice"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.Delete(ctx, "u1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	got, err := s.Get(ctx, "u1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "" {
		t.Fatalf("Get after Delete = %q, want empty", got)
	}
}

func TestDeleteAbsentKeyIsNoOp(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := NewStore()

	if err := s.Delete(ctx, "never-existed"); err != nil {
		t.Fatalf("Delete(absent) = %v, want nil", err)
	}
}

// ExampleStore_Get shows the concrete store round-tripping a value; the // Output
// line is auto-verified by `go test`.
func ExampleStore_Get() {
	ctx := context.Background()
	s := NewStore()
	_ = s.Put(ctx, "u1", "alice")
	v, _ := s.Get(ctx, "u1")
	fmt.Println(v)
	// Output: alice
}
```

## Review

The refactor removed eight interface methods, two do-nothing stubs, and three
unused concrete methods, and lost nothing: the store still does what its callers
need, and the tests exercise it with no interface and no mock. That is the whole
argument against speculative interfaces — a single implementation is not made
more testable by wrapping it in an interface, only more indirect. The next module
proves the refactor is behavior-preserving by running the `before` `MemoryStore`
and this `after` `Store` against identical input and asserting byte-identical
results, so you can trust the deletion did not change semantics. Reach for an
interface again only under the cost/benefit test: a second implementation, a
boundary you must fake, or genuine runtime polymorphism.

## Resources

- [Go Code Review Comments — Interfaces](https://go.dev/wiki/CodeReviewComments#interfaces) — "return concrete types"; the producer should not define the interface.
- [Dave Cheney — SOLID Go Design](https://dave.cheney.net/2016/08/20/solid-go-design) — accept interfaces, return structs, and why speculative interfaces cost more than they save.
- [Martin Fowler — Yagni](https://martinfowler.com/bliki/Yagni.html) — do not build the abstraction for a hypothetical future.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-before-fat-service-interface.md](01-before-fat-service-interface.md) | Next: [03-behavior-preserving-refactor-test.md](03-behavior-preserving-refactor-test.md)
