# Exercise 10: Deep-Copying at the HTTP Boundary — A Snapshot That Must Not Alias

Returning `[]Job` value copies feels safe, and it is — until a `Job` grows a nested
`Tags []string`. The value copy duplicates the slice *header* but shares the
backing array, so a caller mutating `snap[0].Tags[0]` writes straight into the
store. This module builds the HTTP-facing snapshot the right way: a `DeepCopy` that
clones every nested reference field, served under concurrent writers.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
snapexport/                   independent module: example.com/snapexport
  go.mod                      go 1.24
  store.go                    Job with nested Tags []string; Store; shallowSnapshot vs ExportSnapshot (deep)
  export_test.go              value copy aliases nested slice; deep copy is independent; export is -race clean
  cmd/demo/main.go            runnable demo contrasting shallow and deep snapshots
```

Files: `store.go`, `export_test.go`, `cmd/demo/main.go`.
Implement: a `Job` with a nested `Tags []string`; a `shallowSnapshot` returning
`[]Job` value copies; a `DeepCopy(Job) Job` that `slices.Clone`s `Tags`; an
`ExportSnapshot` returning fully independent data under a `sync.RWMutex`.
Test: mutating `snap[0].Tags[0]` from the shallow snapshot corrupts the store;
mutating every level of the deep snapshot leaves the store untouched;
`ExportSnapshot` runs `-race` clean under concurrent writers.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/09-pointers/08-pointers-in-slices-and-maps/10-deep-copy-api-boundary/cmd/demo
cd go-solutions/09-pointers/08-pointers-in-slices-and-maps/10-deep-copy-api-boundary
```

### Why a value copy is still not independent

A struct value copy copies each field. For a value-typed field (`string`, `int`,
`time.Time`) that is a genuine, independent copy. But a slice field is a header
`{ptr, len, cap}`, and copying the header copies the *pointer* to the backing
array — not the array. So after `snap := *j` (or `out = append(out, *j)`), the
`snap.Tags` header is new but points at the same backing array as the store's
`j.Tags`. `snap.Tags[0] = "x"` writes through that shared array into the store. The
top-level struct is isolated; the nested slice is not. This is the exact trap of
Exercise 2 one level deeper: `slices.Clone`/`maps.Clone` isolate the container, and
a plain value copy isolates the struct, but neither reaches *inside* a nested
reference field.

A real defensive copy must be deep: copy the struct, then explicitly clone every
reference-typed field. `DeepCopy` copies the `Job` by value and replaces `Tags`
with `slices.Clone(j.Tags)`, giving the copy its own backing array. If `Job` had a
nested map you would `maps.Clone` it; if it had a slice of structs that themselves
held slices, you would clone recursively. `ExportSnapshot` builds a `[]Job` by
`DeepCopy`ing each stored job under the read lock, so the returned data shares no
memory with the store at any level and can be serialized to an HTTP response while
writers keep mutating. The `RWMutex` makes the export a consistent read; the deep
copy makes it a *safe* one.

Create `store.go`:

```go
package snapexport

import (
	"slices"
	"sync"
)

// Job carries a nested reference-typed field (Tags), which a plain value copy
// does NOT duplicate.
type Job struct {
	ID     string
	Status string
	Tags   []string
}

// DeepCopy returns a Job that shares no memory with j: the struct is copied by
// value and Tags gets its own backing array via slices.Clone.
func DeepCopy(j Job) Job {
	cp := j // copies value fields and the Tags header (a shared slice pointer)
	// slices.Clone gives the copy its own backing array, severing the alias.
	cp.Tags = slices.Clone(j.Tags)
	return cp
}

type Store struct {
	mu   sync.RWMutex
	byID map[string]*Job
	ord  []string
}

func NewStore() *Store {
	return &Store{byID: make(map[string]*Job)}
}

func (s *Store) Put(j *Job) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byID[j.ID]; !ok {
		s.ord = append(s.ord, j.ID)
	}
	s.byID[j.ID] = j
}

// AppendTag mutates a stored job's nested slice in place (a concurrent writer).
func (s *Store) AppendTag(id, tag string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if j, ok := s.byID[id]; ok {
		j.Tags = append(j.Tags, tag)
	}
}

// Tag0 reads the first tag of a stored job (0-safe helper for tests/demo).
func (s *Store) Tag0(id string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if j, ok := s.byID[id]; ok && len(j.Tags) > 0 {
		return j.Tags[0]
	}
	return ""
}

// shallowSnapshot returns []Job value copies. The top-level struct is isolated
// but each Tags slice still aliases the store's backing array: the trap.
func (s *Store) shallowSnapshot() []Job {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Job, 0, len(s.ord))
	for _, id := range s.ord {
		out = append(out, *s.byID[id]) // value copy: shares Tags backing array
	}
	return out
}

// ExportSnapshot returns fully independent data safe to hand across an HTTP
// boundary while writers keep mutating.
func (s *Store) ExportSnapshot() []Job {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Job, 0, len(s.ord))
	for _, id := range s.ord {
		out = append(out, DeepCopy(*s.byID[id]))
	}
	return out
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/snapexport"
)

func main() {
	s := snapexport.NewStore()
	s.Put(&snapexport.Job{ID: "j1", Status: "active", Tags: []string{"urgent"}})

	// Deep snapshot: mutating it cannot touch the store.
	deep := s.ExportSnapshot()
	deep[0].Tags[0] = "MUTATED"
	fmt.Printf("after deep mutation:    store tag=%q\n", s.Tag0("j1"))

	// A concurrent writer appends; the earlier snapshot is unaffected.
	s.AppendTag("j1", "second")
	fmt.Printf("snapshot len=%d store first tag=%q\n", len(deep[0].Tags), s.Tag0("j1"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
after deep mutation:    store tag="urgent"
snapshot len=1 store first tag="urgent"
```

### Tests

`TestValueCopyStillAliasesNestedSlice` documents the hazard: mutate
`snap[0].Tags[0]` from the shallow snapshot and the store's tag changed.
`TestDeepCopyIsFullyIndependent` mutates every level of the deep snapshot — the top
field and the nested slice element — and asserts the store is untouched.
`TestExportUnderConcurrentWrites` runs `ExportSnapshot` while goroutines mutate and
must be `-race` clean.

Create `export_test.go`:

```go
package snapexport

import (
	"sync"
	"testing"
)

func TestValueCopyStillAliasesNestedSlice(t *testing.T) {
	t.Parallel()

	s := NewStore()
	s.Put(&Job{ID: "j1", Status: "active", Tags: []string{"urgent"}})

	snap := s.shallowSnapshot()
	snap[0].Tags[0] = "leaked" // writes through the shared backing array

	if got := s.Tag0("j1"); got != "leaked" {
		t.Fatalf("store tag = %q, want leaked (value copy should alias the nested slice)", got)
	}
}

func TestDeepCopyIsFullyIndependent(t *testing.T) {
	t.Parallel()

	s := NewStore()
	s.Put(&Job{ID: "j1", Status: "active", Tags: []string{"urgent"}})

	snap := s.ExportSnapshot()
	snap[0].Status = "MUTATED"  // top-level field
	snap[0].Tags[0] = "MUTATED" // nested slice element

	if got := s.Tag0("j1"); got != "urgent" {
		t.Fatalf("store tag = %q, want urgent (deep copy must not alias)", got)
	}
	got := s.ExportSnapshot()
	if got[0].Status != "active" {
		t.Fatalf("store status = %q, want active", got[0].Status)
	}
}

func TestExportUnderConcurrentWrites(t *testing.T) {
	t.Parallel()

	s := NewStore()
	for i := range 20 {
		s.Put(&Job{ID: string(rune('a' + i)), Status: "active", Tags: []string{"t"}})
	}

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.AppendTag("a", "x")
		}()
	}
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			snap := s.ExportSnapshot()
			// Mutating the export must never touch the store.
			for i := range snap {
				if len(snap[i].Tags) > 0 {
					snap[i].Tags[0] = "z"
				}
			}
		}()
	}
	wg.Wait()
}
```

## Review

The lesson is that "return values, not pointers" is necessary but not sufficient
once a struct holds a nested reference type. `TestValueCopyStillAliasesNestedSlice`
makes the residual alias concrete: the shallow snapshot's top-level `Job` is
independent, yet writing `snap[0].Tags[0]` still corrupts the store because the
`Tags` header was copied but the backing array was shared. `DeepCopy` closes the
gap by `slices.Clone`ing every reference-typed field, so
`TestDeepCopyIsFullyIndependent` can mutate every level of the export and leave the
store pristine. `ExportSnapshot` combines the `RWMutex` (a consistent read) with the
deep copy (a safe one), which is exactly what an HTTP handler serializing internal
state under concurrent writers needs — `TestExportUnderConcurrentWrites` confirms it
under `-race`. Deep-copy every nested slice, map, and pointer field; the depth of
your copy is the real boundary of your data structure.

## Resources

- [`slices.Clone`](https://pkg.go.dev/slices#Clone) — used to give each nested `Tags` its own backing array.
- [Go Blog: Arrays, slices (and strings)](https://go.dev/blog/slices) — why copying a slice header shares the backing array.
- [`sync.RWMutex`](https://pkg.go.dev/sync#RWMutex) — the read lock that makes the export a consistent snapshot.

---

Back to [00-concepts.md](00-concepts.md) | Next: [../09-pointer-aliasing-and-data-races/00-concepts.md](../09-pointer-aliasing-and-data-races/00-concepts.md)
