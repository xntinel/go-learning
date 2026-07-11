# Exercise 10: The make(len) vs make(len, cap) Mapper Bug

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Every service that speaks JSON has a mapper: a function that walks a slice of
internal records and projects each one into the narrower DTO the API is allowed
to expose. It is the least glamorous code in the repository and it is where a
specific slice bug ships more often than any other. The mapper reserves room for
the page with `make([]DTO, len(recs))`, loops, appends, returns. The tests pass,
because the tests check that alice is in the response. Nobody checks the length.

What actually goes out on the wire is a page of `2n` entries: `n` zero-value
DTOs, `{"id":0,"name":""}`, followed by the `n` real ones. `make([]DTO, n)`
does not reserve `n` slots, it *creates* `n` elements, each the zero value, and
`append` writes behind them. The response is twice the size it should be, every
client that iterates it hits a wall of phantom records with id 0, and the
pagination cursor the frontend derives from `len(page)` is silently wrong. The
fix is one character-cluster wide: `make([]DTO, 0, n)`.

This is not a beginner's typo. It survives review because both forms look like
"preallocate a slice of n DTOs", and it survives testing because the assertions
people write for a mapper are about content, not cardinality. It is the reason
`slices.Grow` exists as a named operation and the reason the `slices` package
documents length and capacity as separate ideas rather than one number.

This module builds the mapper as a package you can drop into a service: a
`Paginator` that validates its own configuration, clamps out-of-range pages
instead of panicking, returns an empty non-nil page rather than `null`, and
guarantees the page it hands back never aliases the caller's records. The buggy
form is not part of that API. It lives in the test file, where it belongs, as
the thing the tests prove wrong.

This module is fully self-contained: its own `go mod init`, a reusable package,
and its tests. Nothing here imports another exercise.

## What you'll build

```text
dtomapper/               module example.com/dtomapper
  go.mod                 go 1.24
  mapper.go              Record, DTO, Paginator; New, Page, Project; three sentinel errors
  mapper_test.go         page table, request validation, the buggy-mapper contrast,
                         allocation property, aliasing, concurrency, ExamplePaginator_Page
```

- Files: `mapper.go`, `mapper_test.go`.
- Implement: `New(maxPageSize int) (*Paginator, error)` rejecting a non-positive size with `ErrInvalidPageSize`; `(*Paginator).Page(recs []Record, offset, limit int) ([]DTO, error)` returning `ErrNegativeOffset` or `ErrInvalidLimit` for requests it cannot honor, clamping an overshooting offset or limit to the end of `recs`, and building the page with `make([]DTO, 0, n)`; `Project(Record) DTO`.
- Test: the page table (first, middle, last-partial, limit overshoot, offset at end, offset past end, empty input, single record); each rejected request mapped to its sentinel with `errors.Is`; a `pageBuggy` contrast proving `2n` length and `n` zero-value DTOs; the allocation property `exact < buggy`; the page never aliases `recs`; `Paginator` is safe for concurrent use; and `ExamplePaginator_Page` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/dtomapper
cd ~/go-exercises/dtomapper
go mod init example.com/dtomapper
go mod edit -go=1.24
```

### make(len) creates elements; make(len, cap) reserves room for them

`make([]T, n)` returns a slice whose length is `n`. All `n` elements exist, all
of them are the zero value, and `s[0]` through `s[n-1]` are addressable right
now. `make([]T, 0, n)` returns a slice whose length is `0` and whose capacity is
`n`: the backing array is the same size, but no element exists yet, and `s[0]`
would panic. Capacity is a promise about future appends; length is a statement
about present contents. The mapper bug is what happens when you write the first
and mean the second:

```go
page := make([]DTO, len(recs))   // len 3: three zero-value DTOs already exist
for _, r := range recs {
    page = append(page, Project(r))   // appends *behind* them, at index 3, 4, 5
}
return page                      // len 6: [{0 } {0 } {0 } {1 alice} {2 bob} {3 carol}]
```

`append` never overwrites existing elements. It writes at index `len(s)`, and
`len(s)` is already 3. The zero values are not placeholders that the loop fills
in; they are real elements the loop steps over. Reversing that to
`make([]DTO, 0, len(recs))` gives `append` an empty slice with three slots of
reserved capacity, so the first append lands at index 0 and the backing array is
never reallocated: exactly `n` elements, exactly one allocation.

There is a second correct form, `page := make([]DTO, len(recs))` followed by
indexed assignment `page[i] = Project(r)`, and it is equally valid. What is never
valid is mixing them, and the mixed form is what the loop above writes. Choose
either "create the elements and assign into them" or "reserve the room and append
into it", and the compiler will not help you notice when you have chosen both.

The rest of `Page` is the part that makes the mapper usable rather than merely
correct. An offset past the end of `recs` is a routine request from a client
paging through a shrinking result set, not an error, so it clamps to an empty
page. That page must be non-nil: `[]DTO{}` encodes as `[]`, while a nil slice
encodes as `null`, and a frontend that does `data.map(...)` crashes on the
second. A limit above the configured maximum *is* an error, because it is the
one thing a client can do to make the server allocate without bound.

Create `mapper.go`:

```go
// Package dtomapper converts internal records into API DTOs one page at a
// time, preallocating each page to its exact size.
//
// It exists to get one detail right that a hand-rolled mapper routinely gets
// wrong: a page slice must be created with make([]DTO, 0, n), not
// make([]DTO, n). The second form creates n zero-value elements and then
// appends n more behind them, so the response carries 2n entries, half of them
// {"id":0,"name":""}. See the package tests for a side-by-side demonstration.
package dtomapper

import (
	"errors"
	"fmt"
)

// Sentinel errors returned by New and by Paginator.Page. Callers should test
// for them with errors.Is rather than by comparing error strings.
var (
	// ErrInvalidPageSize means the configured maximum page size was not positive.
	ErrInvalidPageSize = errors.New("dtomapper: max page size must be positive")
	// ErrInvalidLimit means the requested limit was outside [1, MaxPageSize].
	ErrInvalidLimit = errors.New("dtomapper: limit outside allowed range")
	// ErrNegativeOffset means the requested offset was below zero.
	ErrNegativeOffset = errors.New("dtomapper: offset must not be negative")
)

// Record is one row as it exists internally, including fields that must never
// cross the API boundary.
type Record struct {
	ID       int
	Name     string
	Email    string
	IsAdmin  bool
	Password string
}

// DTO is the public projection of a Record. Only these fields are serialized.
type DTO struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// Paginator maps pages of Records to DTOs, rejecting page requests that exceed
// its configured maximum size.
//
// A Paginator is immutable after construction and is safe for concurrent use by
// multiple goroutines.
type Paginator struct {
	maxPageSize int
}

// New returns a Paginator that refuses any limit above maxPageSize. It returns
// ErrInvalidPageSize if maxPageSize is not positive.
func New(maxPageSize int) (*Paginator, error) {
	if maxPageSize <= 0 {
		return nil, fmt.Errorf("%w: got %d", ErrInvalidPageSize, maxPageSize)
	}
	return &Paginator{maxPageSize: maxPageSize}, nil
}

// MaxPageSize reports the largest limit this Paginator accepts.
func (p *Paginator) MaxPageSize() int { return p.maxPageSize }

// Page projects up to limit records starting at offset into DTOs.
//
// An offset at or past len(recs) is not an error: it yields an empty, non-nil
// page, which encodes as [] rather than null. A limit that overshoots the end
// of recs is likewise clamped. Page returns ErrNegativeOffset or
// ErrInvalidLimit for inputs it cannot honor.
//
// The returned slice is freshly allocated and never aliases recs; the caller
// may retain and mutate it freely.
func (p *Paginator) Page(recs []Record, offset, limit int) ([]DTO, error) {
	if offset < 0 {
		return nil, fmt.Errorf("%w: got %d", ErrNegativeOffset, offset)
	}
	if limit < 1 || limit > p.maxPageSize {
		return nil, fmt.Errorf("%w: got %d, want 1..%d", ErrInvalidLimit, limit, p.maxPageSize)
	}

	start := min(offset, len(recs))
	end := min(start+limit, len(recs))
	window := recs[start:end]

	// Exact capacity, zero length: append writes into reserved space without a
	// single reallocation, and never leaves a zero-value DTO behind.
	page := make([]DTO, 0, len(window))
	for _, r := range window {
		page = append(page, Project(r))
	}
	return page, nil
}

// Project reduces a Record to the fields the API exposes.
func Project(r Record) DTO {
	return DTO{ID: r.ID, Name: r.Name}
}
```

### Using it

`Paginator` is the whole surface: construct it once at startup with the maximum
page size your endpoint advertises, then call `Page` per request. It carries no
mutable state after construction, so a single value can be shared by every
handler goroutine without a mutex -- that is what the doc comment on the type
promises, and what `TestPaginatorIsSafeForConcurrentUse` holds it to.

Two contracts cross the package boundary and both are documented on `Page`. The
returned slice is freshly allocated and never aliases `recs`, so a caller may
retain it, sort it, or mutate it without corrupting the records it came from --
`TestPageDoesNotAliasInput` pins that. And an out-of-range page is empty rather
than nil, so the JSON encoder emits `[]`. Those are the two properties a caller
would otherwise have to discover by reading the implementation.

The module has no `main.go`, because a DTO mapper is a library, not a tool. Its
executable demonstration is `ExamplePaginator_Page`: `go test` runs it and
compares its standard output against the `// Output:` comment, so the usage shown
below cannot drift away from the code.

### Tests

`TestPage` is the table: the ordinary pages, and then every boundary that a real
client will eventually hit -- a limit that overshoots the end, an offset landing
exactly at `len(recs)`, an offset far past it, an empty input, a single record.
Each case asserts the length first and the contents second, because length is
what the bug corrupts. Each also asserts the page is non-nil, which is the
`[]`-not-`null` contract.

`TestBuggyVersionEmitsZeroValueDTOs` is the heart of the module. `pageBuggy` is
unexported and unreachable from the package API; it exists so the test can state
the defect numerically -- `len(buggy) == 2*len(recs)`, and exactly `len(recs)` of
those entries equal `DTO{}` -- and then show the same input through `Page`
producing `n` elements and no zero values. If a future edit reintroduces
`make([]DTO, n)` into `Page`, this test fails here instead of in a customer's
response body.

`TestExactPreallocAllocatesLess` measures the second cost. It asserts a property,
`exact < buggy`, and never a count: how many times `append` reallocates while
growing depends on the runtime's growth curve, which is not a documented contract
and has changed between Go releases. Note that this test does not call
`t.Parallel` -- `testing.AllocsPerRun` panics if it runs from a parallel test,
because a goroutine allocating concurrently would corrupt its measurement.

Create `mapper_test.go`:

```go
package dtomapper

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
)

func records(n int) []Record {
	recs := make([]Record, 0, n)
	for i := 1; i <= n; i++ {
		recs = append(recs, Record{ID: i, Name: fmt.Sprintf("user-%d", i), Password: "secret"})
	}
	return recs
}

// pageBuggy is the mapper as it is usually written the first time, and as it
// ships. make([]DTO, n) creates n zero-value elements; every append then adds a
// real DTO *behind* them. It is never exported, never reachable from the
// package API, and exists only so the tests can pin what it gets wrong.
func pageBuggy(recs []Record) []DTO {
	page := make([]DTO, len(recs))
	for _, r := range recs {
		page = append(page, Project(r))
	}
	return page
}

func TestPage(t *testing.T) {
	t.Parallel()

	recs := records(5)
	tests := []struct {
		name    string
		recs    []Record
		offset  int
		limit   int
		wantIDs []int
	}{
		{name: "first page", recs: recs, offset: 0, limit: 2, wantIDs: []int{1, 2}},
		{name: "middle page", recs: recs, offset: 2, limit: 2, wantIDs: []int{3, 4}},
		{name: "last partial page", recs: recs, offset: 4, limit: 2, wantIDs: []int{5}},
		{name: "limit overshoots end", recs: recs, offset: 0, limit: 50, wantIDs: []int{1, 2, 3, 4, 5}},
		{name: "offset at end yields empty page", recs: recs, offset: 5, limit: 2, wantIDs: []int{}},
		{name: "offset past end yields empty page", recs: recs, offset: 99, limit: 2, wantIDs: []int{}},
		{name: "empty input", recs: nil, offset: 0, limit: 2, wantIDs: []int{}},
		{name: "single record", recs: records(1), offset: 0, limit: 1, wantIDs: []int{1}},
	}

	p, err := New(50)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			page, err := p.Page(tc.recs, tc.offset, tc.limit)
			if err != nil {
				t.Fatalf("Page: unexpected error: %v", err)
			}
			if len(page) != len(tc.wantIDs) {
				t.Fatalf("len(page) = %d, want %d: %+v", len(page), len(tc.wantIDs), page)
			}
			for i, want := range tc.wantIDs {
				if page[i].ID != want {
					t.Errorf("page[%d].ID = %d, want %d", i, page[i].ID, want)
				}
			}
			if page == nil {
				t.Error("page is nil; it must encode as [] and not null")
			}
		})
	}
}

func TestPageRejectsBadRequests(t *testing.T) {
	t.Parallel()

	p, err := New(10)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tests := []struct {
		name          string
		offset, limit int
		want          error
	}{
		{name: "negative offset", offset: -1, limit: 5, want: ErrNegativeOffset},
		{name: "zero limit", offset: 0, limit: 0, want: ErrInvalidLimit},
		{name: "negative limit", offset: 0, limit: -3, want: ErrInvalidLimit},
		{name: "limit above max page size", offset: 0, limit: 11, want: ErrInvalidLimit},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if _, err := p.Page(records(3), tc.offset, tc.limit); !errors.Is(err, tc.want) {
				t.Fatalf("Page error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestNewRejectsNonPositivePageSize(t *testing.T) {
	t.Parallel()

	for _, size := range []int{0, -1} {
		if _, err := New(size); !errors.Is(err, ErrInvalidPageSize) {
			t.Errorf("New(%d) error = %v, want ErrInvalidPageSize", size, err)
		}
	}
}

// TestBuggyVersionEmitsZeroValueDTOs is the whole point of the module: it pins
// the exact defect that make([]DTO, n) ships to production, so that a future
// edit reintroducing it fails here rather than in a customer's response body.
func TestBuggyVersionEmitsZeroValueDTOs(t *testing.T) {
	t.Parallel()

	recs := records(3)

	buggy := pageBuggy(recs)
	if len(buggy) != 2*len(recs) {
		t.Fatalf("len(buggy) = %d, want %d (n zero values plus n appended)", len(buggy), 2*len(recs))
	}
	var zeros int
	for _, d := range buggy {
		if d == (DTO{}) {
			zeros++
		}
	}
	if zeros != len(recs) {
		t.Errorf("buggy page carries %d zero-value DTOs, want %d", zeros, len(recs))
	}

	p, err := New(10)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	good, err := p.Page(recs, 0, 10)
	if err != nil {
		t.Fatalf("Page: %v", err)
	}
	if len(good) != len(recs) {
		t.Fatalf("len(good) = %d, want %d", len(good), len(recs))
	}
	for i, d := range good {
		if d == (DTO{}) {
			t.Errorf("good page carries a zero-value DTO at index %d", i)
		}
	}
}

// TestExactPreallocAllocatesLess shows the second cost of the bug. The exact
// number of reallocations is a runtime detail and is not asserted; that the
// buggy version needs strictly more allocations than the correct one is the
// property that holds across toolchains.
//
// This test deliberately does not call t.Parallel: testing.AllocsPerRun panics
// when run from a parallel test, because a concurrent goroutine allocating in
// the background would corrupt its measurement.
func TestExactPreallocAllocatesLess(t *testing.T) {
	recs := records(64)
	p, err := New(128)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	exact := testing.AllocsPerRun(100, func() {
		_, _ = p.Page(recs, 0, 128)
	})
	buggy := testing.AllocsPerRun(100, func() {
		_ = pageBuggy(recs)
	})
	if !(exact < buggy) {
		t.Fatalf("allocations: exact = %v, buggy = %v; want exact < buggy", exact, buggy)
	}
}

func TestPageDoesNotAliasInput(t *testing.T) {
	t.Parallel()

	recs := records(3)
	p, err := New(10)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	page, err := p.Page(recs, 0, 10)
	if err != nil {
		t.Fatalf("Page: %v", err)
	}

	page[0].Name = "mutated"
	if recs[0].Name != "user-1" {
		t.Fatalf("mutating the page changed the source record: %q", recs[0].Name)
	}
}

func TestPaginatorIsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()

	recs := records(100)
	p, err := New(20)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			page, err := p.Page(recs, i*5, 5)
			if err != nil {
				t.Errorf("Page: %v", err)
				return
			}
			if len(page) != 5 || page[0].ID != i*5+1 {
				t.Errorf("goroutine %d got %+v", i, page)
			}
		}()
	}
	wg.Wait()
}

// ExamplePaginator_Page is the runnable demonstration of this module: go test
// executes it and compares its stdout against the Output comment below.
func ExamplePaginator_Page() {
	recs := []Record{
		{ID: 1, Name: "alice", Email: "alice@example.com", Password: "hunter2"},
		{ID: 2, Name: "bob", Email: "bob@example.com", IsAdmin: true},
		{ID: 3, Name: "carol", Email: "carol@example.com"},
	}

	p, err := New(50)
	if err != nil {
		panic(err)
	}

	page, err := p.Page(recs, 1, 10)
	if err != nil {
		panic(err)
	}
	fmt.Printf("len=%d cap=%d\n", len(page), cap(page))
	_ = json.NewEncoder(os.Stdout).Encode(page)

	empty, err := p.Page(recs, 99, 10)
	if err != nil {
		panic(err)
	}
	fmt.Printf("past the end: len=%d nil=%v -> ", len(empty), empty == nil)
	_ = json.NewEncoder(os.Stdout).Encode(empty)

	if _, err := p.Page(recs, 0, 999); errors.Is(err, ErrInvalidLimit) {
		fmt.Println("limit 999 rejected:", err)
	}

	// Output:
	// len=2 cap=2
	// [{"id":2,"name":"bob"},{"id":3,"name":"carol"}]
	// past the end: len=0 nil=false -> []
	// limit 999 rejected: dtomapper: limit outside allowed range: got 999, want 1..50
}
```

## Review

The mapper is correct when `len(page)` equals the number of records the request
actually selected -- never twice it. `make([]DTO, 0, n)` is what makes that true:
zero length so the first `append` lands at index 0, capacity `n` so no
reallocation happens. `make([]DTO, n)` creates `n` zero-value elements that
`append` then steps over, and the resulting page carries phantom records with id
0 into the response. Either create the elements and assign by index, or reserve
capacity and append; never both. Around that core, `New` rejects a non-positive
page size with `ErrInvalidPageSize`, `Page` rejects a negative offset with
`ErrNegativeOffset` and an out-of-range limit with `ErrInvalidLimit`, all
checkable with `errors.Is`, while an offset past the end is clamped to an empty
non-nil page so the encoder emits `[]` and not `null`. The returned page never
aliases `recs`, and `Paginator` is immutable after construction and therefore
safe to share across goroutines. `ExamplePaginator_Page` is the executable
documentation: `go test` verifies its output. Run
`go test -count=1 -race ./...`.

## Resources

- [`make`](https://go.dev/ref/spec#Making_slices_maps_and_channels) — the spec paragraph that distinguishes the length and capacity arguments.
- [`append`](https://go.dev/ref/spec#Appending_and_copying_slices) — why it writes at index `len(s)` and never overwrites an existing element.
- [`slices.Grow`](https://pkg.go.dev/slices#Grow) — the named operation for reserving capacity without creating elements.
- [`testing.AllocsPerRun`](https://pkg.go.dev/testing#AllocsPerRun) — the allocation probe, and its restriction against parallel tests.
- [Go Wiki: CodeReviewComments](https://go.dev/wiki/CodeReviewComments#declaring-empty-slices) — why an empty slice is preferred to a nil one at an API boundary.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [09-defensive-copy-of-pooled-read-buffer.md](09-defensive-copy-of-pooled-read-buffer.md) | Next: [11-grow-for-known-size-bulk-insert.md](11-grow-for-known-size-bulk-insert.md)
