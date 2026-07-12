# Exercise 1: The Anti-Pattern — a Fat Producer-Side Service Interface

This module builds the anti-pattern in full so you can name the smell precisely:
a single `Service` interface with eight methods, and a `MemoryStore` forced to
implement all eight — including two do-nothing stubs that exist only to satisfy
the type. The next two modules refactor it away and prove the refactor is
behavior-preserving; this one is the starting point they fix.

## What you'll build

```text
before/                     independent module: example.com/before
  go.mod                    go 1.26
  pollute.go                Service (8 methods); MemoryStore implementing all 8
  cmd/
    demo/
      main.go               drives Put/Get through the Service interface
  pollute_test.go           round-trip through the interface; stub-method smell test
```

- Files: `pollute.go`, `cmd/demo/main.go`, `pollute_test.go`.
- Implement: an 8-method `Service` interface (`Get`, `Put`, `Delete`, `List`, `Stats`, `Reset`, `Backup`, `Restore`) and a `MemoryStore` that implements every method, with `Backup`/`Restore` as empty stubs.
- Test: a table test that puts then gets a value through a `var s Service = m` variable and asserts the round-trip; a second test asserting the stub methods return `nil`, documenting that they carry no behavior.
- Verify: `go test -count=1 -race ./...`

### Why this is the anti-pattern

The `Service` interface enumerates eight methods. That single fact drives the
whole smell. Any second implementation must supply all eight, even the ones that
make no sense for it — an in-memory store has no meaningful `Backup(path)` or
`Restore(path)`, so it stubs them to `return nil`. Those stubs are worse than
missing: they advertise a capability the type does not have, and a caller that
trusts the interface will call `Backup`, get a silent `nil`, and believe a backup
happened. The interface is fat, so the abstraction is weak — almost nothing
satisfies it without writing filler.

The interface is also producer-side: it lives next to the implementation, so it
grows lockstep with `MemoryStore`. Every consumer that accepts a `Service` is
coupled to all eight methods even if it calls one. This is exactly the shape the
consumer-side, narrow-interface rule (Exercise 4) and interface segregation
(Exercise 5) exist to correct. Build it here honestly, warts and all, so the
refactor has a real target.

The two stub methods are the concrete artifact of the problem. They compile, they
satisfy the interface, and they do nothing. The test at the end pins that fact so
the smell is documented in code, not just prose.

Create `pollute.go`:

```go
package before

import "context"

// Service is a fat, producer-side interface: eight methods, so any
// implementation must supply all eight even where two are meaningless. This is
// the anti-pattern the lesson names; do not copy this shape into real code.
type Service interface {
	Get(ctx context.Context, id string) (string, error)
	Put(ctx context.Context, id, value string) error
	Delete(ctx context.Context, id string) error
	List(ctx context.Context) ([]string, error)
	Stats(ctx context.Context) (int, error)
	Reset(ctx context.Context) error
	Backup(ctx context.Context, path string) error
	Restore(ctx context.Context, path string) error
}

// MemoryStore is the single implementation. It implements all eight methods,
// including Backup and Restore, which are do-nothing stubs present only to
// satisfy Service.
type MemoryStore struct {
	data map[string]string
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{data: make(map[string]string)}
}

func (m *MemoryStore) Get(ctx context.Context, id string) (string, error) {
	v, ok := m.data[id]
	if !ok {
		return "", nil
	}
	return v, nil
}

func (m *MemoryStore) Put(ctx context.Context, id, value string) error {
	m.data[id] = value
	return nil
}

func (m *MemoryStore) Delete(ctx context.Context, id string) error {
	delete(m.data, id)
	return nil
}

func (m *MemoryStore) List(ctx context.Context) ([]string, error) {
	out := make([]string, 0, len(m.data))
	for k := range m.data {
		out = append(out, k)
	}
	return out, nil
}

func (m *MemoryStore) Stats(ctx context.Context) (int, error) {
	return len(m.data), nil
}

func (m *MemoryStore) Reset(ctx context.Context) error {
	m.data = make(map[string]string)
	return nil
}

// Backup is a do-nothing stub. It exists only to satisfy Service; an in-memory
// store has no meaningful backup. This is the smell: a method that lies about a
// capability the type does not have.
func (m *MemoryStore) Backup(ctx context.Context, path string) error {
	return nil
}

// Restore is the second do-nothing stub, present for the same bad reason.
func (m *MemoryStore) Restore(ctx context.Context, path string) error {
	return nil
}
```

### The runnable demo

The demo drives the store through the `Service` interface variable, exactly as a
polluted codebase would, so you can see the round-trip and the empty `Stats`
after a `Reset`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/before"
)

func main() {
	ctx := context.Background()
	var s before.Service = before.NewMemoryStore()

	if err := s.Put(ctx, "u1", "alice"); err != nil {
		panic(err)
	}
	if err := s.Put(ctx, "u2", "bob"); err != nil {
		panic(err)
	}
	v, _ := s.Get(ctx, "u1")
	fmt.Printf("get u1: %s\n", v)

	n, _ := s.Stats(ctx)
	fmt.Printf("stats: %d\n", n)

	// The stub methods satisfy the interface but do nothing.
	if err := s.Backup(ctx, "/tmp/ignored"); err != nil {
		panic(err)
	}
	fmt.Println("backup returned nil (did nothing)")

	if err := s.Reset(ctx); err != nil {
		panic(err)
	}
	n, _ = s.Stats(ctx)
	fmt.Printf("stats after reset: %d\n", n)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
get u1: alice
stats: 2
backup returned nil (did nothing)
stats after reset: 0
```

The `stats: 2` line is two `Put`s (`u1`, `u2`); after `Reset` the map is
replaced and `Stats` reports 0. The `backup returned nil` line is the smell made
visible: a caller trusting the interface believes a backup happened, but the stub
did nothing.

### Tests

`TestGetRoundTripsThroughInterface` is table-driven and drives every case
through `var s Service = m`, so the test itself is coupled to the fat interface —
that coupling is the thing under study. `TestStubMethodsCarryNoBehavior` calls
`Backup` and `Restore` and asserts they return `nil`, documenting in code that
they are behaviorless filler. `ExampleMemoryStore_Get` pins the round-trip output
so `go test` verifies the snippet too.

Create `pollute_test.go`:

```go
package before

import (
	"context"
	"fmt"
	"testing"
)

func TestGetRoundTripsThroughInterface(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		id    string
		value string
		want  string
	}{
		{name: "alice", id: "u1", value: "alice", want: "alice"},
		{name: "bob", id: "u2", value: "bob", want: "bob"},
		{name: "empty value", id: "u3", value: "", want: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			var s Service = NewMemoryStore()

			if err := s.Put(ctx, tc.id, tc.value); err != nil {
				t.Fatalf("Put: %v", err)
			}
			got, err := s.Get(ctx, tc.id)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got != tc.want {
				t.Fatalf("Get(%q) = %q, want %q", tc.id, got, tc.want)
			}
		})
	}
}

func TestMissingKeyReturnsEmptyNoError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var s Service = NewMemoryStore()

	got, err := s.Get(ctx, "absent")
	if err != nil {
		t.Fatalf("Get(absent) err = %v, want nil", err)
	}
	if got != "" {
		t.Fatalf("Get(absent) = %q, want empty", got)
	}
}

// TestStubMethodsCarryNoBehavior documents the smell in code: Backup and Restore
// exist only to satisfy Service and return nil unconditionally.
func TestStubMethodsCarryNoBehavior(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	m := NewMemoryStore()

	if err := m.Backup(ctx, "/does/not/matter"); err != nil {
		t.Fatalf("Backup = %v, want nil (it is a do-nothing stub)", err)
	}
	if err := m.Restore(ctx, "/does/not/matter"); err != nil {
		t.Fatalf("Restore = %v, want nil (it is a do-nothing stub)", err)
	}
}

// ExampleMemoryStore_Get shows the round-trip through the store; the // Output
// line is auto-verified by `go test`.
func ExampleMemoryStore_Get() {
	ctx := context.Background()
	m := NewMemoryStore()
	_ = m.Put(ctx, "u1", "alice")
	v, _ := m.Get(ctx, "u1")
	fmt.Println(v)
	// Output: alice
}
```

## Review

The lesson of this module is a shape, not a bug: a single interface with eight
methods forces stub implementations and couples every consumer to the full set.
The store is correct in the narrow sense — `Get` round-trips, a missing key
returns `("", nil)` — but `Backup` and `Restore` are behaviorless filler that
only exist because the interface demanded them. If you find yourself writing a
method whose entire body is `return nil` to satisfy an interface, that is the
signal the interface is too fat or does not belong. The remaining modules act on
that signal: Exercise 2 drops the interface entirely for the single-implementation
case, Exercise 5 segregates a fat interface into roles, and Exercise 4 moves the
interface to the consumer where it stays naturally narrow.

## Resources

- [Go Code Review Comments — Interfaces](https://go.dev/wiki/CodeReviewComments#interfaces) — interfaces belong in the consuming package; return concrete types.
- [Go Proverbs](https://go-proverbs.github.io/) — "the bigger the interface, the weaker the abstraction".
- [Effective Go — Interfaces](https://go.dev/doc/effective_go#interfaces) — how implicit satisfaction shapes idiomatic interface use.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-after-concrete-store.md](02-after-concrete-store.md)
