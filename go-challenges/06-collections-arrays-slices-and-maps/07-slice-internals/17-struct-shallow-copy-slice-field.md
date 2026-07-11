# Exercise 17: A Manifest Revision History That Archives Live Aliases

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Every deployment controller keeps a revision history for rollback: a
Kubernetes `Deployment` records its past `ReplicaSet` specs, a
config-management tool logs every applied manifest, and both work the same
way -- on each apply, archive a snapshot of the spec that was just applied,
so a rollback can read one back out later. The spec being archived typically
has slice-valued fields: container args, environment variables, image
pull secrets. The controller keeps applying the live spec, mutating its args
and env in place as new deploys come in, on the reasonable assumption that
whatever got archived earlier is a frozen, independent copy.

Go's copy semantics make that assumption false unless the archiving code
does one extra thing. Assigning or appending a `Manifest` value genuinely
copies the struct -- but a struct copy is only as deep as its fields.
`Name string` copies by value, byte for byte, a real independent copy.
`Args []string` is a slice: the struct copy duplicates the three-word
header, not the backing array it points at. `hist = append(hist, current)`
looks exactly like archiving a value, and for `Name` it is one. For `Args`
and `Env` it archives a second reference to the same memory `current` is
still using. The next call that mutates `current.Args` -- and in a
long-running controller there will be one -- silently rewrites a revision
that rollback believes is immutable history.

This module builds `revhistory`, a bounded revision log whose `Record`
method does the one thing a naive struct-append does not: clone every slice
field before archiving it. The naive version is not part of that API -- it
lives only in the test file, as the thing the tests prove wrong.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
revhistory/               module example.com/revhistory
  go.mod                  go 1.24
  revhistory.go           Manifest, History; New, Record, At, Len
  revhistory_test.go       revision table, eviction, clone-on-write, clone-on-read,
                           the naive-append contrast, concurrency, Example
```

- Files: `revhistory.go`, `revhistory_test.go`.
- Implement: `New(capacity int) (*History, error)` rejecting a non-positive capacity with `ErrInvalidCapacity`; `(*History).Record(m Manifest)` cloning `m.Args` and `m.Env` with `slices.Clone` before archiving, evicting the oldest revision with `slices.Delete` once `capacity` is exceeded; `(*History).At(i int) (Manifest, error)` returning `ErrOutOfRange` outside `[0, Len())` and cloning the archived slices again on the way out; `(*History).Len() int`.
- Test: the revision table (oldest, newest, negative index, index at length, far past length); an empty history's `At(0)`; eviction once a third revision exceeds capacity 2; a live `Manifest` mutated after `Record` proven not to reach the archive; a returned `Manifest` mutated by the caller proven not to reach internal storage; a `recordNaive` contrast proving a plain struct append lets a later mutation corrupt an already-archived revision; `History` safe for concurrent use; and `Example` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/revhistory
cd ~/go-exercises/revhistory
go mod init example.com/revhistory
go mod edit -go=1.24
```

### A struct copy is exactly as deep as its fields, no deeper

Assigning a struct value in Go copies every field. That statement is true
without qualification, and it is also not the whole story a slice field
needs. Copying `Args []string` copies the field's *value*, and the value of
a slice is its three-word header -- pointer, length, capacity -- not the
elements the pointer refers to. So `b := a` for two `Manifest` values does
give `b` its own, independent `Name`, and simultaneously gives `b.Args` a
header that points at the exact same backing array as `a.Args`. Both are
true at once, because both are consequences of the same rule: Go copies
fields, and a slice field's value is a header, not a data structure.

That is precisely what makes the naive archiving pattern dangerous -- it
reads as "append a copy":

```go
func Record(hist []Manifest, current Manifest) []Manifest {
    return append(hist, current) // looks like archiving a snapshot
}
```

`current` is copied into the new slot by value, which is correct as far as
it goes: the `Manifest` struct in `hist` is a distinct value from `current`.
But `hist[len(hist)-1].Args` and `current.Args` are two headers pointing at
one array. The controller's next reconciliation loop does `current.Args[0]
= newImageTag`, intending only to update the live spec it is about to
reapply -- and the "archived" revision changes underneath it, because it
was never actually a separate copy of the data, only a separate copy of the
pointer to it. `slices.Clone` is the fix, applied to every slice field at
the moment of archiving: it allocates a new backing array and copies the
elements into it, so the header stored in history and the header still held
by the live `Manifest` diverge for good.

Create `revhistory.go`:

```go
// Package revhistory keeps a bounded history of Manifest revisions for a
// deployment controller, the same shape as a Kubernetes Deployment's
// rollout history or any config-management revision log: every apply
// archives a snapshot, and rollback reads one back out.
//
// Its whole job is to survive a fact about Go that a plain "archive the
// struct" implementation gets wrong: copying a Manifest value copies its
// Name field by value, but Args and Env are slices, so copying the struct
// only copies their headers -- pointer, length, capacity. The archived
// revision and the live Manifest still point at the same backing arrays.
// Record clones both slice fields before archiving so a later mutation of
// the live Manifest can never reach an already-recorded revision. See the
// package tests for a naive "just append the struct" helper that gets this
// wrong, isolated from this package's API.
package revhistory

import (
	"errors"
	"fmt"
	"slices"
	"sync"
)

// ErrInvalidCapacity is returned by New for a non-positive capacity.
var ErrInvalidCapacity = errors.New("revhistory: capacity must be positive")

// ErrOutOfRange is returned by At for an index outside the currently
// retained revisions.
var ErrOutOfRange = errors.New("revhistory: index out of range")

// Manifest is one deployment specification: a name and the argument and
// environment slices that fully describe how it runs.
type Manifest struct {
	Name string
	Args []string
	Env  []string
}

// History retains up to a fixed number of past Manifest revisions, oldest
// first, evicting the oldest once that limit is exceeded.
//
// History is safe for concurrent use by multiple goroutines; every method
// takes an internal mutex.
type History struct {
	mu       sync.Mutex
	capacity int
	revs     []Manifest
}

// New returns a History that retains at most capacity revisions. It returns
// ErrInvalidCapacity if capacity is not positive.
func New(capacity int) (*History, error) {
	if capacity <= 0 {
		return nil, fmt.Errorf("%w: got %d", ErrInvalidCapacity, capacity)
	}
	return &History{capacity: capacity}, nil
}

// Record archives a snapshot of m. It clones m's Args and Env with
// slices.Clone before storing them, so the archived revision owns its own
// backing arrays: mutating m (or the slices m.Args and m.Env point at)
// after Record returns can never reach the archived copy.
//
// If the history is already at capacity, Record evicts the oldest revision
// before appending the new one; slices.Delete zeroes the evicted slot so
// its Manifest's slice fields become garbage-collectable rather than
// pinned by a stale tail reference.
func (h *History) Record(m Manifest) {
	h.mu.Lock()
	defer h.mu.Unlock()

	clone := Manifest{
		Name: m.Name,
		Args: slices.Clone(m.Args),
		Env:  slices.Clone(m.Env),
	}
	h.revs = append(h.revs, clone)
	if len(h.revs) > h.capacity {
		h.revs = slices.Delete(h.revs, 0, 1)
	}
}

// At returns the revision at index i, where 0 is the oldest revision
// currently retained. It returns ErrOutOfRange if i is outside
// [0, Len()).
//
// The returned Manifest's Args and Env are themselves clones of the
// archived slices, so the caller may mutate them freely without affecting
// this History's internal storage.
func (h *History) At(i int) (Manifest, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if i < 0 || i >= len(h.revs) {
		return Manifest{}, fmt.Errorf("%w: %d, have %d revisions", ErrOutOfRange, i, len(h.revs))
	}
	m := h.revs[i]
	return Manifest{
		Name: m.Name,
		Args: slices.Clone(m.Args),
		Env:  slices.Clone(m.Env),
	}, nil
}

// Len reports how many revisions are currently retained.
func (h *History) Len() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.revs)
}
```

### Using it

Call `Record` every time the controller applies a `Manifest`, using the same
live value it is about to reconcile with the cluster -- there is no need to
clone it yourself first, `Record` already does that at the boundary. Read
history back with `At`, indexed from the oldest retained revision at `0`;
once `Len()` revisions have been recorded, the next `Record` silently drops
the oldest to keep the log bounded, which is the right default for a
rollback log that should not grow forever.

Both directions of the aliasing contract are documented and tested. Writing
in: `TestRecordClonesSoLaterMutationDoesNotReachTheArchive` mutates the live
`Manifest`'s slices immediately after `Record` and shows the archive is
unaffected. Reading out: `TestAtReturnsAnIndependentCopy` mutates what `At`
returns and shows the next `At` call for the same index is unaffected --
`At` clones on the way out for the same reason `Record` clones on the way
in. `Example` is the runnable demonstration of this module: `go test`
executes it and compares its stdout against the `// Output:` comment below,
so the usage shown here cannot drift away from the code.

```go
func Example() {
	h, err := New(2)
	if err != nil {
		panic(err)
	}

	v1 := Manifest{Name: "v1", Args: []string{"--replicas=3"}, Env: []string{"STAGE=prod"}}
	h.Record(v1)
	v1.Args[0] = "--replicas=CORRUPTED" // must not reach the archive

	v2 := Manifest{Name: "v2", Args: []string{"--replicas=5"}, Env: []string{"STAGE=prod"}}
	h.Record(v2)

	v3 := Manifest{Name: "v3", Args: []string{"--replicas=7"}, Env: []string{"STAGE=prod"}}
	h.Record(v3) // evicts v1

	fmt.Println("revisions retained:", h.Len())
	for i := range h.Len() {
		m, err := h.At(i)
		if err != nil {
			panic(err)
		}
		fmt.Println(m.Name, m.Args[0])
	}

	// Output:
	// revisions retained: 2
	// v2 --replicas=5
	// v3 --replicas=7
}
```

### Tests

`TestRecordAndAt` is the table over a two-revision history: the oldest and
newest indices, plus a negative index, an index exactly at the current
length, and one far past it -- all three rejected with `ErrOutOfRange`.
`TestAtOnEmptyHistory` checks the same rejection holds before anything has
ever been recorded. `TestRecordEvictsOldestAtCapacity` fills a
capacity-2 history with three revisions and confirms the oldest is gone and
the remaining two are in order.

`TestRecordNaiveLetsLaterMutationReachArchivedRevision` is the heart of the
module. `recordNaive` is unexported and unreachable from the package API; it
appends a `Manifest` value the way a first attempt usually does, with no
cloning. The test mutates the live `Manifest` after the naive call and shows
the mutation reaches the "archived" slot, then repeats the identical
mutation against a real `History` and shows the archive is untouched. If a
future edit removes the `slices.Clone` calls from `Record`, this test fails
here instead of in a rollback that restores the wrong container image.

Create `revhistory_test.go`:

```go
package revhistory

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

func manifest(name string) Manifest {
	return Manifest{
		Name: name,
		Args: []string{"--replicas=3", "--image=v1"},
		Env:  []string{"STAGE=prod"},
	}
}

// recordNaive is how a revision log is usually written the first time: just
// append the struct. Go copies Manifest's Name field by value, but Args and
// Env are slices, so the "copy" only copies their headers -- the archived
// element and the caller's m still point at the same backing arrays. It is
// never exported and never reachable from the package API; it exists only
// so the tests can pin what it gets wrong.
func recordNaive(revs []Manifest, m Manifest) []Manifest {
	return append(revs, m) // BUG: shares m's Args/Env backing arrays with the caller
}

func TestNewRejectsNonPositiveCapacity(t *testing.T) {
	t.Parallel()

	for _, capacity := range []int{0, -1, -5} {
		if _, err := New(capacity); !errors.Is(err, ErrInvalidCapacity) {
			t.Errorf("New(%d) error = %v, want ErrInvalidCapacity", capacity, err)
		}
	}
}

func TestRecordAndAt(t *testing.T) {
	t.Parallel()

	h, err := New(10)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.Record(manifest("v1"))
	h.Record(manifest("v2"))

	tests := []struct {
		name    string
		index   int
		wantErr error
		wantRev string
	}{
		{name: "oldest revision", index: 0, wantRev: "v1"},
		{name: "newest revision", index: 1, wantRev: "v2"},
		{name: "negative index", index: -1, wantErr: ErrOutOfRange},
		{name: "index at length", index: 2, wantErr: ErrOutOfRange},
		{name: "index far past length", index: 99, wantErr: ErrOutOfRange},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			m, err := h.At(tc.index)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("At(%d) error = %v, want %v", tc.index, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("At(%d): %v", tc.index, err)
			}
			if m.Name != tc.wantRev {
				t.Fatalf("At(%d).Name = %q, want %q", tc.index, m.Name, tc.wantRev)
			}
		})
	}
}

func TestAtOnEmptyHistory(t *testing.T) {
	t.Parallel()

	h, err := New(5)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := h.At(0); !errors.Is(err, ErrOutOfRange) {
		t.Fatalf("At(0) on empty history: err = %v, want ErrOutOfRange", err)
	}
}

func TestRecordEvictsOldestAtCapacity(t *testing.T) {
	t.Parallel()

	h, err := New(2)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.Record(manifest("v1"))
	h.Record(manifest("v2"))
	h.Record(manifest("v3"))

	if got := h.Len(); got != 2 {
		t.Fatalf("Len() = %d, want 2", got)
	}
	first, err := h.At(0)
	if err != nil {
		t.Fatalf("At(0): %v", err)
	}
	if first.Name != "v2" {
		t.Fatalf("At(0).Name = %q, want %q (v1 should have been evicted)", first.Name, "v2")
	}
	second, err := h.At(1)
	if err != nil {
		t.Fatalf("At(1): %v", err)
	}
	if second.Name != "v3" {
		t.Fatalf("At(1).Name = %q, want %q", second.Name, "v3")
	}
}

// TestRecordClonesSoLaterMutationDoesNotReachTheArchive is the positive half
// of the module: it mutates the live Manifest's slices after Record has
// already archived a copy, and shows the archived copy is unaffected.
func TestRecordClonesSoLaterMutationDoesNotReachTheArchive(t *testing.T) {
	t.Parallel()

	h, err := New(5)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	current := manifest("v1")
	h.Record(current)

	current.Args[0] = "--replicas=CORRUPTED"
	current.Env[0] = "STAGE=CORRUPTED"

	archived, err := h.At(0)
	if err != nil {
		t.Fatalf("At(0): %v", err)
	}
	if archived.Args[0] != "--replicas=3" {
		t.Fatalf("archived.Args[0] = %q, want %q (mutation of the live manifest reached the archive)", archived.Args[0], "--replicas=3")
	}
	if archived.Env[0] != "STAGE=prod" {
		t.Fatalf("archived.Env[0] = %q, want %q", archived.Env[0], "STAGE=prod")
	}
}

// TestAtReturnsAnIndependentCopy pins the read-side half of the same
// contract: mutating what At returns must not reach internal storage.
func TestAtReturnsAnIndependentCopy(t *testing.T) {
	t.Parallel()

	h, err := New(5)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.Record(manifest("v1"))

	got, err := h.At(0)
	if err != nil {
		t.Fatalf("At(0): %v", err)
	}
	got.Args[0] = "mutated"

	again, err := h.At(0)
	if err != nil {
		t.Fatalf("At(0): %v", err)
	}
	if again.Args[0] != "--replicas=3" {
		t.Fatalf("mutating a returned Manifest reached internal storage: %q", again.Args[0])
	}
}

// TestRecordNaiveLetsLaterMutationReachArchivedRevision is the whole point
// of the module: it pins the exact defect a "just append the struct"
// revision log ships. Struct assignment copies Args and Env as slice
// headers, not as new backing arrays, so a later mutation of the live
// Manifest silently corrupts a revision that was supposedly already
// archived and immutable.
func TestRecordNaiveLetsLaterMutationReachArchivedRevision(t *testing.T) {
	t.Parallel()

	current := manifest("v1")
	revs := recordNaive(nil, current)

	current.Args[0] = "--replicas=CORRUPTED"

	if revs[0].Args[0] != "--replicas=CORRUPTED" {
		t.Fatalf("revs[0].Args[0] = %q, want %q (naive record must alias the live manifest)", revs[0].Args[0], "--replicas=CORRUPTED")
	}

	// The real History, given the identical mutation, is unaffected.
	h, err := New(5)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	safe := manifest("v1")
	h.Record(safe)
	safe.Args[0] = "--replicas=CORRUPTED"

	archived, err := h.At(0)
	if err != nil {
		t.Fatalf("At(0): %v", err)
	}
	if archived.Args[0] != "--replicas=3" {
		t.Fatalf("archived.Args[0] = %q, want %q", archived.Args[0], "--replicas=3")
	}
}

func TestHistorySafeForConcurrentUse(t *testing.T) {
	t.Parallel()

	h, err := New(50)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			h.Record(manifest(fmt.Sprintf("v%d", i)))
			_ = h.Len()
		}(i)
	}
	wg.Wait()

	if got := h.Len(); got != 20 {
		t.Fatalf("Len() = %d, want 20", got)
	}
}

// Example demonstrates archiving two revisions, mutating the live Manifest
// after each Record call, and reading revisions back unaffected by either
// mutation -- plus eviction once a third revision exceeds capacity 2.
func Example() {
	h, err := New(2)
	if err != nil {
		panic(err)
	}

	v1 := Manifest{Name: "v1", Args: []string{"--replicas=3"}, Env: []string{"STAGE=prod"}}
	h.Record(v1)
	v1.Args[0] = "--replicas=CORRUPTED" // must not reach the archive

	v2 := Manifest{Name: "v2", Args: []string{"--replicas=5"}, Env: []string{"STAGE=prod"}}
	h.Record(v2)

	v3 := Manifest{Name: "v3", Args: []string{"--replicas=7"}, Env: []string{"STAGE=prod"}}
	h.Record(v3) // evicts v1

	fmt.Println("revisions retained:", h.Len())
	for i := range h.Len() {
		m, err := h.At(i)
		if err != nil {
			panic(err)
		}
		fmt.Println(m.Name, m.Args[0])
	}

	// Output:
	// revisions retained: 2
	// v2 --replicas=5
	// v3 --replicas=7
}
```

## Review

`Record` is correct when the archived revision owns backing arrays no live
`Manifest` still points at, which is what `slices.Clone` on `Args` and `Env`
guarantees before the struct is appended; `At` extends the same guarantee to
the read side by cloning again on the way out, so neither direction lets a
mutation cross the package boundary. The trap is trusting that copying a
struct copies everything it contains -- it copies `Name` completely and
copies `Args`/`Env` only as far as their headers, and a naive `append(hist,
current)` inherits that gap silently. `New` validates its capacity with
`ErrInvalidCapacity`, `At` reports `ErrOutOfRange` for any index outside
`[0, Len())`, both checkable with `errors.Is`, and once `capacity` revisions
are retained the oldest is evicted with `slices.Delete`, which also zeroes
the freed slot so its slices are not pinned by a stale tail. `History` is
safe for concurrent use behind an internal mutex. `Example` is the
executable documentation: `go test` verifies its output. Run `go test
-count=1 -race ./...`.

## Resources

- [`slices.Clone`](https://pkg.go.dev/slices#Clone) — the allocation that gives an archived slice field its own backing array.
- [`slices.Delete`](https://pkg.go.dev/slices#Delete) — the eviction primitive used here, and why it zeroes the slot it frees.
- [Go Specification: Struct types](https://go.dev/ref/spec#Struct_types) — struct assignment as fieldwise copy, the rule this module's bug and fix both follow from.
- [Kubernetes: `ControllerRevision`](https://kubernetes.io/docs/reference/kubernetes-api/workload-resources/controller-revision-v1/) — the production shape this exercise's revision log mirrors.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [16-arena-allocator-capacity-ceiling.md](16-arena-allocator-capacity-ceiling.md) | Next: [18-length-prefixed-frame-decoder.md](18-length-prefixed-frame-decoder.md)
