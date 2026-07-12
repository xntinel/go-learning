# Exercise 3: Composite Cache/Idempotency Key: A Struct as a Map Key

Backend de-duplication is almost always keyed on a *tuple*, not a single string:
"this tenant, this resource, this region". The clean way to express that in Go is a
small comparable struct used directly as a map key. This exercise builds a
`DedupKey` and a dedup counter around it, and shows that comparability is exactly
the property that makes the struct a legal map key — and that adding a slice field
would make `map[DedupKey]int64` itself illegal.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
dedupkey/                   independent module: example.com/dedupkey
  go.mod                    go 1.26
  dedupkey.go               type DedupKey{TenantID,Resource,Region string}; Counter with Observe/Count
  cmd/
    demo/
      main.go               runnable demo: observe duplicate dimensions, print counts
  dedupkey_test.go          duplicate keys collapse; independent keys with equal fields coincide; maps.Equal
```

- Files: `dedupkey.go`, `cmd/demo/main.go`, `dedupkey_test.go`.
- Implement: a comparable `DedupKey` and a `Counter` backed by `map[DedupKey]int64` with `Observe(key)` and `Count(key)`.
- Test: duplicate keys increment one logical slot; two independently built equal keys collapse to one entry; `maps.Equal` compares two counter snapshots.
- Verify: `go test -count=1 -race ./...`

### Why comparability *is* map-key-ability

Go requires a map key type to be comparable — the runtime uses `==` (and the type's
hash) to find a bucket. That single requirement is why the design works: `DedupKey`
has three `string` fields, strings are comparable, so a struct of strings is
comparable, so it is a legal map key. Two `DedupKey` values are `==` when all three
fields match, which is precisely the identity you want for de-duplication: the same
tenant/resource/region tuple, however it was constructed, hashes to the same bucket
and lands on the same counter slot.

The consequence that makes this exercise worth doing: this only holds because the
struct is comparable. If a well-meaning change added a `Labels []string` field to
`DedupKey`, the struct would become non-comparable and the *map declaration itself*
would stop compiling — `invalid map key type DedupKey`. You would not discover it at
some far-away call site; the type would be rejected at every `map[DedupKey]…`. The
lesson: a type used as a map key has "stay comparable" as a hard design constraint,
and if you truly need a slice-valued dimension, you derive a comparable summary of
it (a joined string, a hash) to key on instead.

Value semantics matter here too. A map lookup copies the key and compares by value,
so two `DedupKey` structs built independently in different code paths — one from a
parsed request, one from a metrics tag — are the *same key* if their fields match.
There is no pointer identity to accidentally split them across two slots. The test
`TestIndependentKeysCollapse` pins exactly this.

Create `dedupkey.go`:

```go
package dedupkey

import "maps"

// DedupKey identifies a request dimension for de-duplication and per-dimension
// metrics. All fields are comparable, so DedupKey is a legal map key. Adding a
// slice/map field here would make map[DedupKey]... fail to compile.
type DedupKey struct {
	TenantID string
	Resource string
	Region   string
}

// Counter tallies observations per DedupKey. It is the idempotency guard: the
// first Observe of a key returns true (first time seen), later ones return false.
type Counter struct {
	counts map[DedupKey]int64
}

// NewCounter returns an empty Counter.
func NewCounter() *Counter {
	return &Counter{counts: make(map[DedupKey]int64)}
}

// Observe records one occurrence of key and reports whether this was the first
// time the key was seen.
func (c *Counter) Observe(key DedupKey) (first bool) {
	first = c.counts[key] == 0
	c.counts[key]++
	return first
}

// Count reports how many times key has been observed.
func (c *Counter) Count(key DedupKey) int64 { return c.counts[key] }

// Distinct reports the number of distinct keys observed.
func (c *Counter) Distinct() int { return len(c.counts) }

// Snapshot returns a copy of the counts map for comparison in tests.
func (c *Counter) Snapshot() map[DedupKey]int64 { return maps.Clone(c.counts) }
```

### The runnable demo

The demo observes the same dimension tuple three times (via two independently
constructed keys), plus a different region once, and prints the resulting counts
and distinct-key total.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/dedupkey"
)

func main() {
	c := dedupkey.NewCounter()

	k1 := dedupkey.DedupKey{TenantID: "acme", Resource: "invoice", Region: "us-east-1"}
	// Independently constructed, same field values: the same logical key.
	k2 := dedupkey.DedupKey{TenantID: "acme", Resource: "invoice", Region: "us-east-1"}
	other := dedupkey.DedupKey{TenantID: "acme", Resource: "invoice", Region: "eu-west-1"}

	fmt.Printf("first observe k1: %v\n", c.Observe(k1))
	fmt.Printf("observe k2 again: %v\n", c.Observe(k2))
	fmt.Printf("observe k1 third: %v\n", c.Observe(k1))
	fmt.Printf("first observe other region: %v\n", c.Observe(other))

	fmt.Printf("count us-east-1: %d\n", c.Count(k1))
	fmt.Printf("distinct keys: %d\n", c.Distinct())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
first observe k1: true
observe k2 again: false
observe k1 third: false
first observe other region: true
count us-east-1: 3
distinct keys: 2
```

### Tests

`TestDuplicateKeysCollapse` observes the same key repeatedly and asserts a single
logical slot with an incrementing count and `first` only on the first call.
`TestIndependentKeysCollapse` builds two `DedupKey` values in separate statements
and asserts they hit the same slot — value equality, not pointer identity.
`TestDistinctDimensions` checks that differing on any single field creates a new
slot. `TestSnapshotMapsEqual` uses `maps.Equal` to compare two counter snapshots,
demonstrating the type-safe map comparison for a `map[DedupKey]int64`.

Create `dedupkey_test.go`:

```go
package dedupkey

import (
	"maps"
	"testing"
)

func TestDuplicateKeysCollapse(t *testing.T) {
	t.Parallel()

	c := NewCounter()
	k := DedupKey{TenantID: "acme", Resource: "invoice", Region: "us-east-1"}

	if !c.Observe(k) {
		t.Fatal("first Observe should report first=true")
	}
	for i := range 4 {
		if c.Observe(k) {
			t.Fatalf("Observe #%d should report first=false", i+2)
		}
	}
	if got := c.Count(k); got != 5 {
		t.Fatalf("Count = %d, want 5", got)
	}
	if got := c.Distinct(); got != 1 {
		t.Fatalf("Distinct = %d, want 1", got)
	}
}

func TestIndependentKeysCollapse(t *testing.T) {
	t.Parallel()

	c := NewCounter()
	a := DedupKey{TenantID: "t", Resource: "r", Region: "z"}
	b := DedupKey{TenantID: "t", Resource: "r", Region: "z"}

	if a != b {
		t.Fatal("keys with equal fields must be ==")
	}
	c.Observe(a)
	c.Observe(b)
	if got := c.Distinct(); got != 1 {
		t.Fatalf("Distinct = %d, want 1 (independent equal keys must collapse)", got)
	}
}

func TestDistinctDimensions(t *testing.T) {
	t.Parallel()

	c := NewCounter()
	base := DedupKey{TenantID: "t", Resource: "r", Region: "z"}
	c.Observe(base)
	c.Observe(DedupKey{TenantID: "t2", Resource: "r", Region: "z"})
	c.Observe(DedupKey{TenantID: "t", Resource: "r2", Region: "z"})
	c.Observe(DedupKey{TenantID: "t", Resource: "r", Region: "z2"})

	if got := c.Distinct(); got != 4 {
		t.Fatalf("Distinct = %d, want 4", got)
	}
}

func TestSnapshotMapsEqual(t *testing.T) {
	t.Parallel()

	k := DedupKey{TenantID: "t", Resource: "r", Region: "z"}

	c1 := NewCounter()
	c1.Observe(k)
	c1.Observe(k)

	c2 := NewCounter()
	c2.Observe(k)
	c2.Observe(k)

	if !maps.Equal(c1.Snapshot(), c2.Snapshot()) {
		t.Fatal("two counters with the same observations should have equal snapshots")
	}

	c2.Observe(k)
	if maps.Equal(c1.Snapshot(), c2.Snapshot()) {
		t.Fatal("snapshots should differ after an extra observe")
	}
}
```

## Review

The counter is correct when observing a key is idempotent in its identity: the same
tuple always lands on the same slot, and differing on any field creates a new one.
`TestIndependentKeysCollapse` is the proof that this is value equality — it builds
the two keys separately so there is no shared pointer, and asserts they still
collapse. The design constraint to remember is the one the concepts file states:
`DedupKey` earns its role as a map key by staying comparable, and the day you need a
slice-valued dimension you key on a comparable derivation of it (a joined string or
hash), never the raw slice. `maps.Equal` in `TestSnapshotMapsEqual` is the type-safe
way to compare two `map[DedupKey]int64` snapshots without reflection.

## Resources

- [Go spec: Map types](https://go.dev/ref/spec#Map_types) — the requirement that map key types be comparable.
- [maps.Equal](https://pkg.go.dev/maps#Equal) — type-safe comparison of two maps of comparable values.
- [Go spec: Comparison operators](https://go.dev/ref/spec#Comparison_operators) — struct comparability that makes a tuple key legal.

---

Back to [00-concepts.md](00-concepts.md) | Next: [04-interface-field-comparison-panic.md](04-interface-field-comparison-panic.md)
