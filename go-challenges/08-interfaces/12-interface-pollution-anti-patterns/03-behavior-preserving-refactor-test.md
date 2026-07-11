# Exercise 3: Proving the Refactor Preserves Behavior

Deleting an interface is only safe if the behavior is identical before and after.
This module bundles both the fat-interface `MemoryStore` and the concrete `Store`
in one package and pins the contract with a differential test: feed both the same
key/value sequence and assert byte-identical results and identical error behavior.
That test is the license to delete the interface with confidence.

## What you'll build

```text
pollute/                    independent module: example.com/pollute
  go.mod                    go 1.26
  before.go                 Service interface + MemoryStore (the anti-pattern)
  after.go                  concrete Store (the refactor)
  cmd/
    demo/
      main.go               runs the same ops through both, prints they agree
  pollute_test.go           before-via-interface, after-via-struct, differential test
```

- Files: `before.go`, `after.go`, `cmd/demo/main.go`, `pollute_test.go`.
- Implement: bundle the `Service`/`MemoryStore` pair and the concrete `Store` in one package so a single test can drive both.
- Test: `TestBeforeGetReturnsValue` (through the `Service` interface), `TestAfterGetReturnsValue` (through the concrete `Store`), and `TestBeforeAndAfterProduceSameOutput` comparing `Get` outputs on identical input.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/pollute/cmd/demo
cd ~/go-exercises/pollute
go mod init example.com/pollute
```

### Why a differential test is the right proof

A refactor that removes an interface is a claim: "these two shapes have the same
observable behavior." The cheapest honest way to check a claim like that is a
differential (or characterization) test — run both implementations against the
same inputs and assert their outputs match, rather than re-asserting the expected
value twice. If `MemoryStore.Get` and `Store.Get` ever diverge — a different
zero value, a different error on a missing key, a different handling of the empty
string — the differential test fails and names the input that broke, whereas two
independent tests asserting a hard-coded expectation might both drift and still
pass. This is the pattern you use in real refactors when you are about to delete
or replace a component and need a safety net that pins current behavior exactly.

To compare them the test still touches both shapes the polluted and clean ways:
`TestBeforeGetReturnsValue` goes through `var s Service = m`, keeping the fat
interface in the test surface, while `TestAfterGetReturnsValue` uses the bare
`*Store`. `TestBeforeAndAfterProduceSameOutput` then runs a sequence of `Put`s
into each, reads every key back from both, and asserts the `(value, error)`
pairs are identical. Because both stores treat a missing key as `("", nil)` and
store the empty string faithfully, the outputs agree on every case, including the
edge cases that most often diverge.

Create `before.go` (the anti-pattern, kept intact so the test can drive it):

```go
package pollute

import "context"

// Service is the fat 8-method interface from Exercise 1, kept here so the
// differential test can drive the old shape through the interface.
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

func (m *MemoryStore) Backup(ctx context.Context, path string) error {
	return nil
}

func (m *MemoryStore) Restore(ctx context.Context, path string) error {
	return nil
}
```

Create `after.go` (the refactor):

```go
package pollute

import "context"

// Store is the concrete refactor from Exercise 2: three methods, no interface.
type Store struct {
	data map[string]string
}

func NewStore() *Store {
	return &Store{data: make(map[string]string)}
}

func (s *Store) Get(ctx context.Context, id string) (string, error) {
	v, ok := s.data[id]
	if !ok {
		return "", nil
	}
	return v, nil
}

func (s *Store) Put(ctx context.Context, id, value string) error {
	s.data[id] = value
	return nil
}

func (s *Store) Delete(ctx context.Context, id string) error {
	delete(s.data, id)
	return nil
}
```

### The runnable demo

The demo runs the same `Put`/`Get` sequence through both stores and reports
whether they agree, which is exactly what the differential test asserts.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/pollute"
)

func main() {
	ctx := context.Background()
	var before pollute.Service = pollute.NewMemoryStore()
	after := pollute.NewStore()

	inputs := []struct{ id, value string }{
		{"u1", "alice"},
		{"u2", "bob"},
		{"u3", ""},
	}
	for _, in := range inputs {
		_ = before.Put(ctx, in.id, in.value)
		_ = after.Put(ctx, in.id, in.value)
	}

	agree := true
	for _, in := range inputs {
		b, _ := before.Get(ctx, in.id)
		a, _ := after.Get(ctx, in.id)
		if a != b {
			agree = false
		}
	}
	// A key neither stored: both must agree it is absent.
	b, _ := before.Get(ctx, "absent")
	a, _ := after.Get(ctx, "absent")
	if a != b {
		agree = false
	}

	fmt.Printf("before and after agree on every key: %v\n", agree)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
before and after agree on every key: true
```

### Tests

Alongside the three tests, `ExampleStore_Get` pins the refactored store's output
so `go test` verifies the snippet too.

Create `pollute_test.go`:

```go
package pollute

import (
	"context"
	"fmt"
	"testing"
)

func TestBeforeGetReturnsValue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	m := NewMemoryStore()
	var s Service = m
	if err := s.Put(ctx, "u1", "alice"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	v, err := s.Get(ctx, "u1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if v != "alice" {
		t.Fatalf("Get = %q, want alice", v)
	}
}

func TestAfterGetReturnsValue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	s := NewStore()
	if err := s.Put(ctx, "u1", "alice"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	v, err := s.Get(ctx, "u1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if v != "alice" {
		t.Fatalf("Get = %q, want alice", v)
	}
}

// TestBeforeAndAfterProduceSameOutput pins the refactor contract: the same
// key/value sequence yields byte-identical (value, error) pairs from both.
func TestBeforeAndAfterProduceSameOutput(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	before := NewMemoryStore()
	after := NewStore()

	inputs := []struct{ id, value string }{
		{"u1", "alice"},
		{"u2", "bob"},
		{"u3", ""},        // empty value must round-trip identically
		{"u1", "alice-2"}, // overwrite must behave identically
	}
	for _, in := range inputs {
		if err := before.Put(ctx, in.id, in.value); err != nil {
			t.Fatalf("before.Put: %v", err)
		}
		if err := after.Put(ctx, in.id, in.value); err != nil {
			t.Fatalf("after.Put: %v", err)
		}
	}

	// Compare every stored key plus a key that was never stored.
	for _, id := range []string{"u1", "u2", "u3", "absent"} {
		bv, berr := before.Get(ctx, id)
		av, aerr := after.Get(ctx, id)
		if bv != av {
			t.Fatalf("Get(%q) value: before=%q after=%q", id, bv, av)
		}
		if (berr == nil) != (aerr == nil) {
			t.Fatalf("Get(%q) error mismatch: before=%v after=%v", id, berr, aerr)
		}
	}
}

// ExampleStore_Get shows the refactored concrete store agreeing with the old
// interface path on a stored key; the // Output line is auto-verified by `go test`.
func ExampleStore_Get() {
	ctx := context.Background()
	after := NewStore()
	_ = after.Put(ctx, "u1", "alice")
	v, _ := after.Get(ctx, "u1")
	fmt.Println(v)
	// Output: alice
}
```

## Review

The differential test is the deliverable: it proves that removing the fat
`Service` interface and the two stubs did not change any observable behavior, so
the deletion in Exercise 2 was safe rather than merely plausible. Note what the
test checks that a naive one would miss — the empty-string value, the overwrite,
and a key that was never stored — because those are the cases where two
"obviously equivalent" implementations most often quietly diverge. When you do
this in a real codebase, write the characterization test against the current
implementation first, watch it pass, then refactor underneath it; if it goes red,
the input it names is the behavior you changed by accident.

## Resources

- [Go Code Review Comments — Interfaces](https://go.dev/wiki/CodeReviewComments#interfaces) — the rule the refactor follows.
- [Martin Fowler — Characterization / Yagni](https://martinfowler.com/bliki/Yagni.html) — pinning current behavior before a structural change.
- [Effective Go — Interfaces](https://go.dev/doc/effective_go#interfaces) — implicit satisfaction, which is what lets the same test drive both shapes.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-after-concrete-store.md](02-after-concrete-store.md) | Next: [04-consumer-defined-narrow-interface.md](04-consumer-defined-narrow-interface.md)
