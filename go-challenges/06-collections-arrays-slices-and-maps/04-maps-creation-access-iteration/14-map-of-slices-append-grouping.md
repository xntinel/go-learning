# Exercise 14: map[string][]Event — Grouping With Append and Why It Cannot Be a Pointer Method

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Grouping a stream of events by tenant — the same shape as a metering
pipeline batching usage records, an audit-log fan-in, or a webhook delivery
queue splitting by destination — is a natural fit for `map[string][]Event`:
one bucket per tenant, appended to as events arrive. The trap is not the map,
it is the append: `m[k] = append(m[k], e)` looks like it should be
simplifiable to `append(m[k], e)`, but dropping the assignment silently
breaks it, because `append` can return a slice with a different pointer,
length, and capacity than the one it received, and a map element cannot be
mutated to receive that new header in place. This module builds the
grouping index as a package, rejects the one input that would corrupt the
grouping silently — an event with no tenant — proves the write-back is
load-bearing with a test that removes it, and shows the deeper reason why: a
map element is not addressable, so a pointer-receiver method cannot even be
called on one directly.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
tenantgroup/                  module example.com/tenantgroup
  go.mod                      go 1.24
  group.go                    Event, Index; NewIndex, Add, Bucket; ErrEmptyTenant
  group_test.go                 table grouping, empty-tenant rejection, nil-slice zero
                               value, write-back proof, ExampleNewIndex
```

- Files: `group.go`, `group_test.go`.
- Implement: `Event{Tenant, Name string; At int64}`; `Index map[string][]Event` with `(idx Index) Add(e Event) error` doing `idx[e.Tenant] = append(idx[e.Tenant], e)` and rejecting an empty `e.Tenant` with `ErrEmptyTenant`; `NewIndex(events []Event) (Index, error)` folding a batch through `Add`; `(idx Index) Bucket(tenant string) []Event`.
- Test: a table of interleaved multi-tenant events grouping into per-tenant buckets in arrival order; empty-tenant rejection at the first, middle, and last position; a tenant that was never added reading back as a usable nil slice (safe `len`, safe `range`); a test that discards `append`'s result without writing it back and proves the map is left untouched; the positive counterpart proving `Add` does persist across calls; and `ExampleNewIndex` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/04-maps-creation-access-iteration/14-map-of-slices-append-grouping
cd go-solutions/06-collections-arrays-slices-and-maps/04-maps-creation-access-iteration/14-map-of-slices-append-grouping
go mod edit -go=1.24
```

### Why the assignment back into the map is required, not stylistic

`idx[e.Tenant]` on a tenant that has never been added reads as the zero value
of `[]Event`, which is `nil` — and appending to a nil slice is entirely safe:
`append` sees a length and capacity of zero and allocates a fresh backing
array on the spot. That is what makes `Add` setup-free: there is no
`if _, ok := idx[e.Tenant]; !ok { idx[e.Tenant] = []Event{} }` guard needed
before the first event for a tenant, exactly the same as `m[k]++` needing no
guard for a counter map. So far this mirrors the counter case from earlier
exercises.

Where it differs is what happens *after* the append. `append(s, e)` may or
may not need to grow `s`'s backing array — if there is spare capacity it
writes in place and returns a header with the same pointer but a longer
length; if there is not, it allocates a new, larger array, copies the old
contents over, and returns a header pointing at the new array. Either way,
the caller only finds out which happened by looking at `append`'s return
value — there is no way to know in advance, and no way to mutate the
*existing* slice header to grow it, because a map element is not
addressable: you cannot take `&idx[e.Tenant]` to update its length and
pointer in place the way you could with `&mySlice`. So `idx[e.Tenant] =
append(idx[e.Tenant], e)` is not defensive style, it is the only mechanism
by which the map's entry ever finds out the slice grew:

```go
func addWithoutWriteBack(idx Index, e Event) {
	_ = append(idx[e.Tenant], e)   // grows a local copy, then throws it away
}
```

`addWithoutWriteBack` mutates a local copy of the slice header (and,
whenever no reallocation happened, mutates shared backing-array bytes past
the old length, invisible until something else re-slices into them) and
leaves the map's stored header exactly as it was. `TestAppendWithoutWriteBackDoesNotPersist`
is built to catch exactly that mistake.

The address issue goes one step further if the value type itself grows a
method. Suppose the bucket had a pointer-receiver mutator instead of a
package-level function:

```go
type Bucket []Event
func (b *Bucket) Add(e Event) { *b = append(*b, e) }
```

Calling `idx[e.Tenant].Add(e)` on `idx map[string]Bucket` does not compile.
The compiler needs `&idx[e.Tenant]` to call a pointer-receiver method, and a
map element is not addressable, so it refuses outright:

```text
cannot call pointer method Add on Bucket
```

This is the same rule this lesson's earlier addressability exercise covers
for struct fields (`m[k].Field = x`), applied to methods: whether it is a
field assignment or a pointer-receiver method call, both require taking the
address of a map element, and Go forbids that address from existing at all.
The fix in both cases is the same shape — read, mutate a local, write back —
which is exactly what `Index.Add` does at the package level instead of as a
pointer method on the slice-typed value.

`NewIndex` adds one more real-world guard on top: an event with an empty
`Tenant` cannot be grouped at all, and silently filing it under the empty
string would hide a producer bug — a missing tenant header, a bad JSON field
— behind a bucket nobody will ever look at.

Create `group.go`:

```go
// Package tenantgroup groups multi-tenant events by tenant ID, the shape of
// a metering pipeline, an audit-log fan-in, or a webhook-delivery batcher
// that needs "everything for tenant X" without a per-tenant table scan.
package tenantgroup

import (
	"errors"
	"fmt"
)

// ErrEmptyTenant is returned by Add when an Event's Tenant field is empty:
// an event with no tenant cannot be grouped, and silently dropping it would
// hide a producer bug rather than surface it.
var ErrEmptyTenant = errors.New("tenantgroup: event tenant must not be empty")

// Event is one occurrence recorded for a tenant.
type Event struct {
	Tenant string
	Name   string
	At     int64
}

// Index groups events by tenant ID. The zero value of Index is a nil map:
// safe to read (ranging it yields no entries, indexing a missing key
// yields a nil slice) but not safe to write to directly. Build one with
// NewIndex.
//
// Index is NOT safe for concurrent use: Add mutates the underlying map, so
// concurrent calls from multiple goroutines must be synchronized by the
// caller (for example, build the Index in one goroutine, then treat it as
// read-only once construction finishes).
type Index map[string][]Event

// NewIndex builds an Index from a batch of events, one Add call per event,
// preserving each tenant's events in arrival order. It returns the error
// from the first event whose Tenant is empty, wrapped with that event's
// position in the batch.
func NewIndex(events []Event) (Index, error) {
	idx := make(Index)
	for i, e := range events {
		if err := idx.Add(e); err != nil {
			return nil, fmt.Errorf("event %d: %w", i, err)
		}
	}
	return idx, nil
}

// Add appends e to the bucket for e.Tenant. This works even when e.Tenant
// has never been seen before: the missing-key read idx[e.Tenant] yields a
// nil slice, and append on a nil slice allocates a fresh backing array, so
// no per-tenant initialization is required before the first event. Add
// returns ErrEmptyTenant if e.Tenant is empty.
//
// The assignment idx[e.Tenant] = append(idx[e.Tenant], e) is not optional
// style -- it is the only way the possibly-reallocated slice header gets
// stored back into the map. append may return a different slice (new
// pointer, new length, new capacity) than the one it was given, and a map
// element is not addressable, so there is no way to grow the stored slice
// in place. Skipping the write-back leaves the map's entry unchanged while
// a local copy of the slice header grows and is then discarded.
func (idx Index) Add(e Event) error {
	if e.Tenant == "" {
		return ErrEmptyTenant
	}
	idx[e.Tenant] = append(idx[e.Tenant], e)
	return nil
}

// Bucket returns tenant's events. The returned slice aliases Index's
// internal storage: appending to it directly and discarding the result is
// safe (it only affects the caller's local copy), but writing through an
// index into it (bucket[0] = ...) mutates the Index's own stored data.
// Callers that need an independent copy should clone the returned slice.
func (idx Index) Bucket(tenant string) []Event {
	return idx[tenant]
}
```

### Using it

Build an `Index` in one pass with `NewIndex` when the full batch is already
in hand, or start from `make(Index)` and call `Add` per event as they arrive
from a stream — both paths go through the same write-back-required logic.
`Bucket` is the read path; because `Index`'s underlying type is exported as
`map[string][]Event`, a caller may also index it directly (`idx[tenant]`),
but `Bucket` is the documented entry point and the one whose aliasing
contract is spelled out. `Index` carries no internal locking, so a caller
that needs to build it from multiple goroutines at once must synchronize
that itself; the common pattern is to build it single-threaded and only
share it once construction is finished and it is treated as read-only.

The module has no `main.go`, because a grouping index is a library, not a
tool. Its executable demonstration is `ExampleNewIndex`: `go test` runs it
and compares its standard output against the `// Output:` comment, so the
usage shown below cannot drift away from the code.

### Tests

`TestNewIndexGroupsByTenant` is a table covering no events, a single tenant,
and interleaved multi-tenant events, checking that each tenant's bucket
lands in arrival order. `TestNewIndexRejectsEmptyTenant` sweeps the position
of the offending event — first, middle, last — the same way the previous
exercise's ID validation does, so an off-by-one in the reported index cannot
hide in only one shape. `TestBucketOnMissingTenantYieldsUsableNilSlice` pins
the nil-slice zero-value behavior the demo's last grouping line depends on.

The two tests that carry the exercise's real lesson are
`TestAppendWithoutWriteBackDoesNotPersist`, which drives the unexported
`addWithoutWriteBack` helper — discarding `append`'s result exactly the way
a buggy refactor would — and proves the map is left completely untouched,
and `TestAddMutatesMapNotJustLocalCopy`, its positive counterpart, which
proves `Add`'s write-back does persist and accumulate across repeated calls
for the same tenant.

Create `group_test.go`:

```go
package tenantgroup

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"testing"
)

func TestNewIndexGroupsByTenant(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		events []Event
		want   map[string][]string // tenant -> ordered event names
	}{
		{
			name:   "no events",
			events: nil,
			want:   map[string][]string{},
		},
		{
			name: "single tenant",
			events: []Event{
				{Tenant: "acme", Name: "a"},
				{Tenant: "acme", Name: "b"},
			},
			want: map[string][]string{"acme": {"a", "b"}},
		},
		{
			name: "interleaved tenants preserve per-tenant order",
			events: []Event{
				{Tenant: "acme", Name: "a1"},
				{Tenant: "globex", Name: "g1"},
				{Tenant: "acme", Name: "a2"},
				{Tenant: "globex", Name: "g2"},
				{Tenant: "acme", Name: "a3"},
			},
			want: map[string][]string{
				"acme":   {"a1", "a2", "a3"},
				"globex": {"g1", "g2"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			idx, err := NewIndex(tc.events)
			if err != nil {
				t.Fatalf("NewIndex: %v", err)
			}
			if len(idx) != len(tc.want) {
				t.Fatalf("len(idx) = %d, want %d", len(idx), len(tc.want))
			}
			for tenant, wantNames := range tc.want {
				bucket := idx.Bucket(tenant)
				if len(bucket) != len(wantNames) {
					t.Fatalf("Bucket(%q) has %d events, want %d", tenant, len(bucket), len(wantNames))
				}
				for i, name := range wantNames {
					if bucket[i].Name != name {
						t.Fatalf("Bucket(%q)[%d].Name = %q, want %q", tenant, i, bucket[i].Name, name)
					}
				}
			}
		})
	}
}

func TestNewIndexRejectsEmptyTenant(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		events []Event
	}{
		{"empty tenant first", []Event{{Tenant: ""}, {Tenant: "b"}}},
		{"empty tenant in the middle", []Event{{Tenant: "a"}, {Tenant: ""}, {Tenant: "c"}}},
		{"empty tenant last", []Event{{Tenant: "a"}, {Tenant: "b"}, {Tenant: ""}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if _, err := NewIndex(tc.events); !errors.Is(err, ErrEmptyTenant) {
				t.Fatalf("err = %v, want ErrEmptyTenant", err)
			}
		})
	}
}

// TestBucketOnMissingTenantYieldsUsableNilSlice proves the zero value the
// caller gets back for a tenant that was never Add-ed is a nil slice that is
// safe to range and safe to len -- exactly what makes Add's missing-key path
// setup-free.
func TestBucketOnMissingTenantYieldsUsableNilSlice(t *testing.T) {
	t.Parallel()

	idx := make(Index)
	bucket := idx.Bucket("never-seen")

	if bucket != nil {
		t.Fatalf("bucket = %v, want nil", bucket)
	}
	if len(bucket) != 0 {
		t.Fatalf("len(bucket) = %d, want 0", len(bucket))
	}
	count := 0
	for range bucket {
		count++
	}
	if count != 0 {
		t.Fatalf("ranging a nil bucket produced %d iterations, want 0", count)
	}
}

// addWithoutWriteBack demonstrates why idx[e.Tenant] = append(idx[e.Tenant], e)
// is required rather than optional style: it discards append's result
// exactly the way a careless refactor would, so the map is never told the
// slice grew. It is never exported and never reachable from the package
// API; it exists only so the contrast test below can pin the defect.
func addWithoutWriteBack(idx Index, e Event) {
	_ = append(idx[e.Tenant], e)
}

// TestAppendWithoutWriteBackDoesNotPersist proves addWithoutWriteBack leaves
// the map completely untouched: appending to a local copy of the slice
// header and discarding the result never touches the map, because append
// may return a different slice than the one it received and a map element
// cannot be mutated in place.
func TestAppendWithoutWriteBackDoesNotPersist(t *testing.T) {
	t.Parallel()

	idx := make(Index)
	addWithoutWriteBack(idx, Event{Tenant: "acme", Name: "orphaned"})

	if _, ok := idx["acme"]; ok {
		t.Fatal(`idx["acme"] should not exist: append result was never written back to the map`)
	}
	if got := idx.Bucket("acme"); got != nil {
		t.Fatalf(`idx.Bucket("acme") = %v, want nil (map untouched by the discarded append)`, got)
	}
}

// TestAddMutatesMapNotJustLocalCopy is the positive counterpart: Add's
// write-back does persist across calls, growing the same tenant bucket.
func TestAddMutatesMapNotJustLocalCopy(t *testing.T) {
	t.Parallel()

	idx := make(Index)
	if err := idx.Add(Event{Tenant: "acme", Name: "first"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := idx.Add(Event{Tenant: "acme", Name: "second"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	bucket := idx.Bucket("acme")
	if len(bucket) != 2 {
		t.Fatalf(`len(idx.Bucket("acme")) = %d, want 2`, len(bucket))
	}
	if bucket[0].Name != "first" || bucket[1].Name != "second" {
		t.Fatalf("bucket = %v, want [first second] in order", bucket)
	}
}

// ExampleNewIndex is the runnable demonstration of this module: go test
// executes it and compares its stdout against the Output comment below.
func ExampleNewIndex() {
	events := []Event{
		{Tenant: "acme", Name: "invoice.created", At: 1},
		{Tenant: "globex", Name: "user.login", At: 2},
		{Tenant: "acme", Name: "invoice.paid", At: 3},
		{Tenant: "initech", Name: "webhook.sent", At: 4},
		{Tenant: "acme", Name: "invoice.void", At: 5},
	}

	idx, err := NewIndex(events)
	if err != nil {
		panic(err)
	}

	// Iteration order over idx is randomized, so the report sorts tenant
	// keys before printing to stay deterministic.
	for _, tenant := range slices.Sorted(maps.Keys(idx)) {
		bucket := idx.Bucket(tenant)
		fmt.Printf("%s: %d event(s)\n", tenant, len(bucket))
		for _, e := range bucket {
			fmt.Printf("  - %s\n", e.Name)
		}
	}

	// A tenant that never appeared reads as a usable nil slice: safe to
	// range (zero iterations) and safe to len (zero), never a panic.
	missing := idx.Bucket("hooli")
	fmt.Printf("hooli: %d event(s), nil=%v\n", len(missing), missing == nil)

	if _, err := NewIndex([]Event{{Tenant: ""}}); errors.Is(err, ErrEmptyTenant) {
		fmt.Println("empty tenant rejected:", err)
	}

	// Output:
	// acme: 3 event(s)
	//   - invoice.created
	//   - invoice.paid
	//   - invoice.void
	// globex: 1 event(s)
	//   - user.login
	// initech: 1 event(s)
	//   - webhook.sent
	// hooli: 0 event(s), nil=true
	// empty tenant rejected: event 0: tenantgroup: event tenant must not be empty
}
```

## Review

`Index.Add` is correct exactly when every event lands in its tenant's bucket
in arrival order or is rejected with `ErrEmptyTenant`, and the entire
mechanism hinges on one assignment: `idx[e.Tenant] = append(idx[e.Tenant], e)`.
`TestAppendWithoutWriteBackDoesNotPersist` is the test that exists purely to
make that assignment's necessity concrete rather than assumed — it drives
the unexported `addWithoutWriteBack`, which performs the exact append a
careless refactor would leave dangling, and proves the map never changes.
The addressability angle explains *why* no shortcut exists: a map element
cannot be addressed, so there is no `&idx[e.Tenant]` for `append` to grow
through, and — as the `cannot call pointer method Add on Bucket` compiler
error shows — there is also no way to hang a pointer-receiver mutator method
directly off a slice-typed map value for the same reason. The fix is always
the same shape: read the current value out, compute the new one, write it
back explicitly. Run `go test -count=1 -race ./...`.

## Resources

- [Go Specification: Address operators](https://go.dev/ref/spec#Address_operators) — map elements are not addressable, the root cause behind both the append pitfall and the pointer-method restriction.
- [Go Specification: Appending to and copying slices](https://go.dev/ref/spec#Appending_and_copying_slices) — `append` may return a slice backed by a different array than the one it was given.
- [Effective Go: Slices](https://go.dev/doc/effective_go#slices) — why a slice header must be reassigned after any operation that can grow it.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [13-prealloc-map-index-build.md](13-prealloc-map-index-build.md) | Next: [15-unhashable-interface-key-panic-guard.md](15-unhashable-interface-key-panic-guard.md)
