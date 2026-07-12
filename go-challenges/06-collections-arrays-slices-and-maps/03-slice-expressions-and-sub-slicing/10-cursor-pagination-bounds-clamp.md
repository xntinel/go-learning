# Exercise 10: Cursor Pagination That Clamps Instead Of Panicking

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Every list endpoint in a service — sessions, audit events, orders — ends up
calling the same shared pagination helper, and that helper sits directly on
top of a slice expression. A client-supplied `offset` and `limit` drive
`items[offset:offset+limit]` almost verbatim, and the moment a client pages
past the last record or asks for a limit larger than the dataset, the naive
version panics with a slice-bounds-out-of-range instead of returning an
empty page. Worse, when the naive slice does *not* panic, it silently
inherits whatever spare capacity the source array has past its length, so a
caller's `append` to the "page" it received can clobber data the source still
owns — the exact inherited-capacity trap this lesson's concepts file opens
with.

This exercise builds the clamped version as a small reusable package: a
`Paginator` that validates its own configuration once at startup, then
answers every request either with a `slices.Clone`d, capacity-bounded page or
with a sentinel error a caller can check with `errors.Is`. Legitimate
overshoot — a client paging past the last page, or a limit larger than what
remains — is silently bounded to `len(items)`. A negative offset or limit, or
a limit above the configured maximum, is rejected outright rather than
coerced into something that looks valid.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
cursorpage/                module example.com/cursorpage
  go.mod                   go 1.24
  cursorpage.go             Paginator; New; Page[T any]; four sentinel errors
  cursorpage_test.go        clamp table, rejection table, independent-copy test, concurrency
                            test, the naive-slice contrast, ExamplePage
```

- Files: `cursorpage.go`, `cursorpage_test.go`.
- Implement: `New(maxLimit int) (*Paginator, error)` rejecting a non-positive
  limit with `ErrInvalidMaxLimit`; `Page[T any](p *Paginator, items []T, offset,
  limit int) ([]T, error)` returning `ErrNegativeOffset`, `ErrNegativeLimit`, or
  `ErrLimitExceedsMax` for requests it cannot honor, clamping an overshooting
  offset or offset+limit to `len(items)`, and returning
  `slices.Clone(items[start:end:end])`.
- Test: the clamp table (normal window, offset beyond end, limit overshoot,
  zero limit, offset exactly at end); the rejection table mapped to each
  sentinel with `errors.Is`; `New` rejecting a non-positive max limit; the page
  never aliases the source; `Paginator` safe for concurrent use; a `pageNaive`
  contrast proving the two-index expression panics past the end and aliases
  spare capacity when it doesn't; and `ExamplePage` as the runnable
  demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why overshoot is clamped but negative input is rejected

A two/three-index slice expression `s[low:high]` is legal for any `high` up
to `cap(s)`, and panics the instant `low` or `high` falls outside
`[0, cap(s)]` or `low > high`. A cursor-paginated endpoint receives `offset`
and `limit` straight from query parameters, so both bounds are attacker- and
bug-controlled: a client that keeps incrementing `offset` past the last page,
or asks for `limit=10000` against a 40-row table, is not misbehaving — it is
exactly what "keep paging until the page comes back empty" looks like from
the client's side. Clamping `offset` and `offset+limit` to `len(items)`
before slicing turns that ordinary case into an empty, non-nil page instead
of a panic.

A negative `offset` or `limit`, on the other hand, has no sensible clamp
target. Coercing `offset=-1` to `0` would silently reinterpret "page before
the first page" as "the first page" — the caller asked for something that
does not exist, and returning data anyway hides a client bug that should
surface as a 400. `Page` treats the two failure modes differently on
purpose: `offset > len(items)` clamps, `offset < 0` errors. A limit above the
`Paginator`'s configured maximum is a third, separate failure mode: it is the
one thing a client can do to make the server allocate without bound, so it is
rejected even though it is a positive number that could otherwise be
clamped.

Even a correctly clamped window is not enough on its own. `items[start:end]`
by itself inherits every byte of `items`' spare capacity past `end`, exactly
as this lesson's concepts file describes: `s[1:3]` on a slice with `cap 10`
has `cap 9`, not `2`. Handing that window straight to a caller means their
`append` can silently overwrite records `items` still owns. The fix is the
three-index-plus-Clone idiom used throughout this lesson:
`slices.Clone(items[start:end:end])` — the three-index expression bounds the
intermediate window's capacity to its own length first, and `Clone` then
copies it into a fresh array so the caller can mutate or append without
reaching back into the source.

Create `cursorpage.go`:

```go
// Package cursorpage clamps offset/limit slice windows to a source's actual
// length instead of letting an out-of-range client request panic.
//
// It exists to get one detail right that a hand-rolled two-index slice
// expression routinely gets wrong: items[offset:offset+limit] panics the
// instant offset or offset+limit exceeds cap(items), and even when it does
// not panic, the returned window can inherit spare capacity from items and
// let a caller's append silently corrupt data still owned by the source. See
// the package tests for a side-by-side demonstration.
package cursorpage

import (
	"errors"
	"fmt"
	"slices"
)

// Sentinel errors returned by New and Page. Callers should test for them
// with errors.Is rather than by comparing error strings.
var (
	// ErrInvalidMaxLimit means the configured maximum limit was not positive.
	ErrInvalidMaxLimit = errors.New("cursorpage: max limit must be positive")
	// ErrNegativeOffset means the requested offset was below zero.
	ErrNegativeOffset = errors.New("cursorpage: offset must not be negative")
	// ErrNegativeLimit means the requested limit was below zero.
	ErrNegativeLimit = errors.New("cursorpage: limit must not be negative")
	// ErrLimitExceedsMax means the requested limit exceeded the configured maximum.
	ErrLimitExceedsMax = errors.New("cursorpage: limit exceeds configured maximum")
)

// Paginator bounds every page request to a configured maximum limit.
//
// A Paginator is immutable after construction and is safe for concurrent use
// by multiple goroutines: Page only reads p.maxLimit and never mutates the
// Paginator.
type Paginator struct {
	maxLimit int
}

// New returns a Paginator that refuses any limit above maxLimit. It returns
// ErrInvalidMaxLimit if maxLimit is not positive.
func New(maxLimit int) (*Paginator, error) {
	if maxLimit <= 0 {
		return nil, fmt.Errorf("%w: got %d", ErrInvalidMaxLimit, maxLimit)
	}
	return &Paginator{maxLimit: maxLimit}, nil
}

// MaxLimit reports the largest limit p accepts.
func (p *Paginator) MaxLimit() int { return p.maxLimit }

// Page returns the [offset, offset+limit) window of items as an independent
// copy, clamping both offset and offset+limit to len(items) instead of
// letting a two-index slice expression panic. An offset at or past
// len(items) is not an error: it yields an empty, non-nil page, since a
// client that keeps paging past the last page is normal, expected behavior.
// Page returns ErrNegativeOffset, ErrNegativeLimit, or ErrLimitExceedsMax for
// requests it cannot honor.
//
// The returned slice is a fresh copy and never aliases items; the caller may
// mutate or append to it freely without reaching back into the source.
func Page[T any](p *Paginator, items []T, offset, limit int) ([]T, error) {
	if offset < 0 {
		return nil, fmt.Errorf("%w: got %d", ErrNegativeOffset, offset)
	}
	if limit < 0 {
		return nil, fmt.Errorf("%w: got %d", ErrNegativeLimit, limit)
	}
	if limit > p.maxLimit {
		return nil, fmt.Errorf("%w: got %d, max %d", ErrLimitExceedsMax, limit, p.maxLimit)
	}

	start := min(offset, len(items))
	end := min(start+limit, len(items))

	// Three-index expression bounds capacity to length; Clone detaches into a
	// fresh, right-sized array so the caller can mutate or append without
	// reaching back into items.
	return slices.Clone(items[start:end:end]), nil
}
```

### Using it

`Paginator` is the whole surface: construct it once at startup with the
maximum limit your endpoint advertises, then call `Page` per request with the
current page's `items`, `offset`, and `limit`. Because `Page` is a package
function taking `*Paginator` as its first argument rather than a method, one
`Paginator` value can back list endpoints over any element type — sessions,
audit events, orders — without a separate instantiation per type; Go methods
cannot introduce their own type parameters, so this is the idiomatic shape
for a generic operation configured by a non-generic value (the same pattern
`slices.SortFunc` uses).

Two contracts cross the package boundary and both are documented on `Page`.
The returned slice is a fresh copy and never aliases `items`, so a caller may
retain it, sort it, or append to it without corrupting data the source still
owns. And an out-of-range page is empty rather than nil, so a JSON encoder
downstream emits `[]`, not `null`. `Paginator` itself carries no mutable
state after construction, so a single value can be shared by every handler
goroutine without a mutex.

The module has no `main.go`, because a pagination helper is a library, not a
tool. Its executable demonstration is `ExamplePage`: `go test` runs it and
compares its standard output against the `// Output:` comment, so the usage
shown below cannot drift away from the code.

```go
func ExamplePage() {
	p, err := New(10)
	if err != nil {
		panic(err)
	}

	sessions := []string{"s0", "s1", "s2", "s3", "s4"}

	page, err := Page(p, sessions, 1, 2)
	if err != nil {
		panic(err)
	}
	fmt.Printf("page: %v len=%d cap=%d\n", page, len(page), cap(page))

	empty, err := Page(p, sessions, 99, 2)
	if err != nil {
		panic(err)
	}
	fmt.Printf("past the end: %v nil=%v\n", empty, empty == nil)

	if _, err := Page(p, sessions, 0, 20); errors.Is(err, ErrLimitExceedsMax) {
		fmt.Println("limit 20 rejected:", err)
	}

	// Output:
	// page: [s1 s2] len=2 cap=2
	// past the end: [] nil=false
	// limit 20 rejected: cursorpage: limit exceeds configured maximum: got 20, max 10
}
```

`cap=2` on the first line is the three-index-plus-Clone contract made
visible: the page has exactly as much capacity as it has length, so the next
`append` to it must reallocate rather than reach back into `sessions`.

### Tests

The clamp table drives the boundary cases where a bare slice expression
would otherwise panic or misbehave: an offset far past the end, a limit that
overshoots what remains, a zero limit, and an offset landing exactly on
`len(items)`. The rejection table pins each sentinel error independently —
negative offset, negative limit, and a limit above the configured maximum —
and `TestNewRejectsNonPositiveMaxLimit` does the same for construction.
`TestPageReturnsIndependentCopy` and `TestPaginatorIsSafeForConcurrentUse`
confirm the two contracts the doc comments promise: no aliasing, and safe
sharing across goroutines.

`pageNaive` is the heart of the module's contrast. It is unexported and
unreachable from the package API; it exists so the tests can pin the two-index
expression's exact failure modes: `TestNaiveSlicePanicsPastEnd` recovers a
real panic from `items[2:7]` on a three-element slice, and
`TestNaiveSliceAliasesSpareCapacity` shows that even a naive call that does
not panic hands back a window still welded to the source's backing array —
an `append` to it lands inside `items`' own spare capacity, visible by
reading `items[:4]` afterward. If a future edit reintroduces a bare
`items[offset:offset+limit]` into `Page`, this is the failure mode it would
reintroduce.

Create `cursorpage_test.go`:

```go
package cursorpage

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
)

func TestPageClampsBounds(t *testing.T) {
	t.Parallel()

	sessions := []string{"s0", "s1", "s2", "s3", "s4"}

	tests := []struct {
		name   string
		offset int
		limit  int
		want   []string
	}{
		{"normal window", 1, 2, []string{"s1", "s2"}},
		{"offset beyond end", 100, 5, []string{}},
		{"limit overshooting", 3, 100, []string{"s3", "s4"}},
		{"zero limit", 0, 0, []string{}},
		{"offset exactly at end", 5, 3, []string{}},
	}

	p, err := New(100)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := Page(p, sessions, tc.offset, tc.limit)
			if err != nil {
				t.Fatalf("Page(%d,%d) unexpected error: %v", tc.offset, tc.limit, err)
			}
			if got == nil {
				t.Fatal("Page returned nil; want non-nil slice")
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("Page(%d,%d) = %v, want %v", tc.offset, tc.limit, got, tc.want)
			}
		})
	}
}

func TestPageRejectsBadRequests(t *testing.T) {
	t.Parallel()

	sessions := []string{"s0", "s1", "s2"}
	p, err := New(10)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	tests := []struct {
		name    string
		offset  int
		limit   int
		wantErr error
	}{
		{"negative offset", -1, 1, ErrNegativeOffset},
		{"negative limit", 0, -1, ErrNegativeLimit},
		{"limit above configured max", 0, 11, ErrLimitExceedsMax},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := Page(p, sessions, tc.offset, tc.limit)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Page(%d,%d) err = %v, want %v", tc.offset, tc.limit, err, tc.wantErr)
			}
		})
	}
}

func TestNewRejectsNonPositiveMaxLimit(t *testing.T) {
	t.Parallel()

	for _, max := range []int{0, -1} {
		if _, err := New(max); !errors.Is(err, ErrInvalidMaxLimit) {
			t.Errorf("New(%d) error = %v, want ErrInvalidMaxLimit", max, err)
		}
	}
}

func TestPageReturnsIndependentCopy(t *testing.T) {
	t.Parallel()

	items := []int{1, 2, 3}
	p, err := New(10)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := Page(p, items, 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	got[0] = 99
	got = append(got, 4) // capacity is bounded to len, so this must reallocate

	if items[0] != 1 {
		t.Fatalf("mutation leaked into source: items[0] = %d", items[0])
	}
	if len(items) != 3 {
		t.Fatalf("append leaked into source: len(items) = %d", len(items))
	}
}

func TestPaginatorIsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()

	items := make([]int, 100)
	for i := range items {
		items[i] = i
	}
	p, err := New(20)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := Page(p, items, i*5, 5)
			if err != nil {
				t.Errorf("Page: %v", err)
				return
			}
			if len(got) != 5 || got[0] != i*5 {
				t.Errorf("goroutine %d got %v", i, got)
			}
		}()
	}
	wg.Wait()
}

// pageNaive is the two-index slice expression a first draft of this helper
// usually reaches for: items[offset:offset+limit], with no clamp and no
// three-index-plus-clone detach. It is never exported and never reachable
// from the package API; it exists only so the tests below can pin what it
// gets wrong.
func pageNaive[T any](items []T, offset, limit int) []T {
	return items[offset : offset+limit]
}

// TestNaiveSlicePanicsPastEnd shows the first failure mode: a two-index
// expression panics the moment offset+limit exceeds cap(items), which is
// exactly the ordinary "client keeps paging past the last page" request Page
// is built to answer without panicking.
func TestNaiveSlicePanicsPastEnd(t *testing.T) {
	t.Parallel()

	items := []int{1, 2, 3}

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("pageNaive(items, 2, 5) did not panic; want a slice-bounds panic")
		}
	}()
	_ = pageNaive(items, 2, 5)
}

// TestNaiveSliceAliasesSpareCapacity shows the second failure mode: even
// when the naive slice does not panic, it inherits whatever spare capacity
// the source has past its length, so an append to the "page" writes straight
// into the source's backing array instead of allocating a fresh one.
func TestNaiveSliceAliasesSpareCapacity(t *testing.T) {
	t.Parallel()

	items := make([]int, 3, 10) // len 3, cap 10: spare room for the bug to bite
	items[0], items[1], items[2] = 1, 2, 3

	naive := pageNaive(items, 0, 3)
	naive = append(naive, 99) // append writes into items' spare capacity in place

	full := items[:4] // legal: within cap(items), even though len(items) is 3
	if full[3] != 99 {
		t.Fatalf("naive append did not alias the source array: items[3] = %d, want 99", full[3])
	}
	_ = naive
}

// ExamplePage is the runnable demonstration of this module: go test executes
// it and compares its stdout against the Output comment below.
func ExamplePage() {
	p, err := New(10)
	if err != nil {
		panic(err)
	}

	sessions := []string{"s0", "s1", "s2", "s3", "s4"}

	page, err := Page(p, sessions, 1, 2)
	if err != nil {
		panic(err)
	}
	fmt.Printf("page: %v len=%d cap=%d\n", page, len(page), cap(page))

	empty, err := Page(p, sessions, 99, 2)
	if err != nil {
		panic(err)
	}
	fmt.Printf("past the end: %v nil=%v\n", empty, empty == nil)

	if _, err := Page(p, sessions, 0, 20); errors.Is(err, ErrLimitExceedsMax) {
		fmt.Println("limit 20 rejected:", err)
	}

	// Output:
	// page: [s1 s2] len=2 cap=2
	// past the end: [] nil=false
	// limit 20 rejected: cursorpage: limit exceeds configured maximum: got 20, max 10
}
```

## Review

`Page` is correct when every offset and limit a client could plausibly send
— in range, past the end, zero, or overshooting — comes back as a valid,
non-nil, capacity-bounded slice with no panic, while the inputs that cannot
be interpreted as a valid request (negative offset, negative limit, a limit
above the configured maximum) come back as distinct, `errors.Is`-checkable
sentinel errors. The wrong turn `pageNaive` pins is two-fold: a bare
`items[offset:offset+limit]` panics the instant a client pages past the end,
and even when it survives, it inherits the source's spare capacity, so an
`append` on the caller's side silently corrupts records `items` still owns.
`Page` closes both holes with the same idiom this lesson uses throughout:
clamp first, then `slices.Clone(items[start:end:end])` to bound capacity to
length and detach into a fresh array. `Paginator` is immutable after
construction and therefore safe to share across goroutines, and `ExamplePage`
is the executable documentation: `go test` verifies its output. Run
`go test -count=1 -race ./...`.

## Resources

- [Go Specification: Slice expressions](https://go.dev/ref/spec#Slice_expressions)
- [`slices.Clone`](https://pkg.go.dev/slices#Clone)
- [`errors.Is`](https://pkg.go.dev/errors#Is) — how callers should test for the sentinel errors this package returns.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [09-ordered-schedule-insert-delete.md](09-ordered-schedule-insert-delete.md) | Next: [11-fixed-layout-record-offsets.md](11-fixed-layout-record-offsets.md)
