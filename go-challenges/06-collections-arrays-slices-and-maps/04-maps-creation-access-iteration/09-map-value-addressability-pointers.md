# Exercise 9: Map Values Aren't Addressable: pointer values vs read-modify-write structs

`m[k].Field = x` does not compile: map values are not addressable, so you cannot
assign to a field of a struct-valued map element. This exercise builds a
connection-stats registry two correct ways — value structs with read-modify-write,
and `map[K]*Stats` with in-place mutation — and weighs the aliasing and
memory-retention trade-offs between them.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
connstats/                 independent module: example.com/connstats
  go.mod
  registry.go              Stats; ValueRegistry (read-modify-write), PointerRegistry (map[K]*Stats)
  cmd/
    demo/
      main.go              runnable demo: accumulate per-connection stats both ways
  registry_test.go         read-modify-write, in-place pointer mutation, nil for missing, -race
```

- Files: `registry.go`, `cmd/demo/main.go`, `registry_test.go`.
- Implement: `ValueRegistry` using `v := m[k]; v.F++; m[k] = v`, and `PointerRegistry` using `map[string]*Stats` for in-place field mutation.
- Test: read-modify-write increments across calls, the pointer path mutates in place and is visible on the next `Get`, comma-ok returns a nil pointer for a missing key, and concurrent increments are race-free.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/04-maps-creation-access-iteration/09-map-value-addressability-pointers/cmd/demo
cd go-solutions/06-collections-arrays-slices-and-maps/04-maps-creation-access-iteration/09-map-value-addressability-pointers
```

### Why m[k].Field = x does not compile

A map element is not addressable. The runtime may relocate entries when the hash
table grows, so a pointer into the table could dangle; Go forbids taking the
address of `m[k]` at all, and therefore forbids assigning to a field through it:

```text
// Does not compile:
//   r.conns[conn].Requests++
//   cannot assign to struct field r.conns[conn].Requests in map
```

There are two correct fixes, and choosing between them is a genuine design
decision. **Read-modify-write** copies the value out, mutates the copy, and writes
it back: `s := m[conn]; s.Requests++; m[conn] = s`. It preserves value semantics —
each entry is an independent struct, no two names alias it — at the cost of copying
the whole struct on every update. Note the copy-out handles the missing key for
free: `m[conn]` on an absent key yields the zero `Stats{}`, which is the correct
starting accumulator.

**Pointer values** store `*Stats`, so `m[conn].Requests++` compiles and mutates in
place, because you are dereferencing a pointer, not addressing a map element. This
avoids the per-update copy and lets several holders share one `Stats`, but it
introduces aliasing (two names for the same struct, a source of surprising
mutation-at-a-distance bugs) and keeps the pointee alive as long as the map holds
the pointer, which matters for GC in long-lived servers. The pointer path also
needs an explicit nil check on first touch: `s, ok := m[conn]; if !ok { s = &Stats{}; m[conn] = s }`,
and `Get` returns a nil pointer for a missing key that callers must handle.

Prefer read-modify-write for small, independent structs; reach for pointers when
the struct is large, updated on a hot path, or legitimately shared.

Create `registry.go`:

```go
package connstats

import "sync"

// Stats accumulates per-connection counters.
type Stats struct {
	Requests int
	BytesIn  int64
	BytesOut int64
}

// ValueRegistry stores stats by value and updates via read-modify-write.
type ValueRegistry struct {
	mu    sync.Mutex
	conns map[string]Stats
}

// NewValueRegistry returns an empty ValueRegistry.
func NewValueRegistry() *ValueRegistry {
	return &ValueRegistry{conns: make(map[string]Stats)}
}

// AddRequest records one request. It reads the value out (zero if absent),
// mutates the copy, and writes it back, because m[k].Field = x does not compile.
func (r *ValueRegistry) AddRequest(conn string, in, out int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.conns[conn] // copy out; zero Stats if absent
	s.Requests++
	s.BytesIn += in
	s.BytesOut += out
	r.conns[conn] = s // write back
}

// Get returns a copy of the stats for conn via comma-ok.
func (r *ValueRegistry) Get(conn string) (Stats, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.conns[conn]
	return s, ok
}

// PointerRegistry stores *Stats and mutates in place.
type PointerRegistry struct {
	mu    sync.Mutex
	conns map[string]*Stats
}

// NewPointerRegistry returns an empty PointerRegistry.
func NewPointerRegistry() *PointerRegistry {
	return &PointerRegistry{conns: make(map[string]*Stats)}
}

// AddRequest records one request, mutating the pointed-to Stats in place. This
// compiles because s is a pointer; s.Requests++ dereferences it.
func (r *PointerRegistry) AddRequest(conn string, in, out int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.conns[conn]
	if !ok {
		s = &Stats{}
		r.conns[conn] = s
	}
	s.Requests++
	s.BytesIn += in
	s.BytesOut += out
}

// Get returns the *Stats for conn, or a nil pointer and false if absent.
func (r *PointerRegistry) Get(conn string) (*Stats, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.conns[conn]
	return s, ok
}
```

### The runnable demo

The demo accumulates two requests per connection through each registry and shows a
missing-key lookup returning a nil pointer.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/connstats"
)

func main() {
	vr := connstats.NewValueRegistry()
	vr.AddRequest("conn-1", 100, 50)
	vr.AddRequest("conn-1", 200, 80)
	s, _ := vr.Get("conn-1")
	fmt.Printf("value   conn-1: reqs=%d in=%d out=%d\n", s.Requests, s.BytesIn, s.BytesOut)

	pr := connstats.NewPointerRegistry()
	pr.AddRequest("conn-2", 10, 5)
	pr.AddRequest("conn-2", 20, 5)
	ps, _ := pr.Get("conn-2")
	fmt.Printf("pointer conn-2: reqs=%d in=%d out=%d\n", ps.Requests, ps.BytesIn, ps.BytesOut)

	if _, ok := pr.Get("missing"); !ok {
		fmt.Println("missing conn: not found")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
value   conn-1: reqs=2 in=300 out=130
pointer conn-2: reqs=2 in=30 out=10
missing conn: not found
```

Both registries accumulate correctly across the two requests. The value registry
copies the struct out and back on each `AddRequest`; the pointer registry mutates
the pointed-to `Stats` in place.

### Tests

`TestValueReadModifyWrite` proves the copy-back path accumulates across calls, so
the write-back is not forgotten. `TestPointerInPlace` proves the pointer path
mutates in place and the change is visible on the next `Get` (the same pointee).
`TestPointerMissingReturnsNil` documents that `Get` on a missing key returns a nil
pointer. `TestConcurrentPointer` hammers the pointer registry under `-race` to
prove the mutex guards the shared `Stats`.

Create `registry_test.go`:

```go
package connstats

import (
	"fmt"
	"sync"
	"testing"
)

func TestValueReadModifyWrite(t *testing.T) {
	t.Parallel()

	r := NewValueRegistry()
	r.AddRequest("c", 10, 1)
	r.AddRequest("c", 20, 2)

	s, ok := r.Get("c")
	if !ok {
		t.Fatal("Get(c) should be present")
	}
	if s.Requests != 2 || s.BytesIn != 30 || s.BytesOut != 3 {
		t.Fatalf("stats = %+v, want {2 30 3}", s)
	}
}

func TestPointerInPlace(t *testing.T) {
	t.Parallel()

	r := NewPointerRegistry()
	r.AddRequest("c", 10, 1)

	// The pointer obtained now must reflect a later in-place mutation.
	s1, _ := r.Get("c")
	r.AddRequest("c", 20, 2)
	if s1.Requests != 2 || s1.BytesIn != 30 {
		t.Fatalf("in-place mutation not visible through earlier pointer: %+v", s1)
	}
}

func TestPointerMissingReturnsNil(t *testing.T) {
	t.Parallel()

	r := NewPointerRegistry()
	s, ok := r.Get("nope")
	if ok {
		t.Fatal("missing key should report ok=false")
	}
	if s != nil {
		t.Fatal("missing key should return a nil pointer")
	}
}

func TestConcurrentPointer(t *testing.T) {
	t.Parallel()

	r := NewPointerRegistry()
	var wg sync.WaitGroup
	for range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.AddRequest("shared", 1, 1)
		}()
	}
	wg.Wait()

	s, _ := r.Get("shared")
	if s.Requests != 200 {
		t.Fatalf("Requests = %d, want 200", s.Requests)
	}
}

func ExampleValueRegistry_AddRequest() {
	r := NewValueRegistry()
	r.AddRequest("c", 100, 50)
	r.AddRequest("c", 200, 80)
	s, _ := r.Get("c")
	fmt.Println(s.Requests, s.BytesIn, s.BytesOut)
	// Output: 2 300 130
}
```

## Review

Both registries are correct accumulators, and the exercise is really about why
`m[k].Field = x` cannot be one of them. The value registry must write the mutated
copy back — `TestValueReadModifyWrite` would fail if `AddRequest` forgot the
`r.conns[conn] = s` line, which is the classic read-modify-write bug. The pointer
registry mutates in place, and `TestPointerInPlace` proves the aliasing that makes
it work: a pointer fetched earlier sees a later mutation, because both name the same
`Stats`. That aliasing is the double edge — convenient here, a mutation-at-a-distance
hazard elsewhere, and a GC-retention cost in long-lived maps. `TestPointerMissingReturnsNil`
documents the nil-pointer return that pointer-valued maps force callers to handle.
Run `go test -race` to confirm the mutex guards the shared pointee.

## Resources

- [Go Specification: Address operators](https://go.dev/ref/spec#Address_operators) — map elements are not addressable.
- [Go Specification: Assignments](https://go.dev/ref/spec#Assignments) — why `m[k].Field = x` is rejected.
- [Go blog: Go maps in action](https://go.dev/blog/maps) — value vs pointer map entries.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [08-struct-key-idempotency-guard.md](08-struct-key-idempotency-guard.md) | Next: [10-frequency-top-n-aggregation.md](10-frequency-top-n-aggregation.md)
