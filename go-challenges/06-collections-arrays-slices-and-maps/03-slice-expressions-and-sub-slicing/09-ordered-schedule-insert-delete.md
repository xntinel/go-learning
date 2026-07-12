# Exercise 9: Maintain a Capacity-Bounded Sorted Schedule with Insert and Delete

A scheduler keeps upcoming jobs sorted by run-at time. New jobs are inserted at
their sorted position; cancelled jobs are removed. This is where `slices.Insert` and
`slices.Delete` earn their keep over hand-rolled index shuffling: they shift elements
in place, may reallocate, and — for pointer element types — zero the vacated tail so
a removed `*Job` is collectable. This exercise builds the schedule and pins both the
sorted invariant and the pointer-zeroing behavior that hand-rolled deletion gets
wrong.

This module is fully self-contained. Nothing here imports another exercise.

## What you'll build

```text
schedule/                  independent module: example.com/schedule
  go.mod                   go 1.24
  schedule.go              type Job, Schedule; Insert; Delete; Jobs; Sorted
  cmd/
    demo/
      main.go              runnable demo: insert out of order, delete, stay sorted
  schedule_test.go         sorted-after-insert, delete order/tail-nil, manual-delete leak
```

- Files: `schedule.go`, `cmd/demo/main.go`, `schedule_test.go`.
- Implement: `Insert` placing a job at its sorted index via `sort.Search` +
  `slices.Insert`; `Delete` removing by id via `slices.IndexFunc` + `slices.Delete`.
- Test: `slices.IsSortedFunc` holds after each insert; delete preserves order,
  decrements length, and nils the vacated tail slot; a hand-rolled
  `append(s[:i], s[i+1:]...)` leaves a dangling pointer (the leak `slices.Delete`
  avoids); `slices.BinarySearchFunc` locates a job.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/03-slice-expressions-and-sub-slicing/09-ordered-schedule-insert-delete/cmd/demo
cd go-solutions/06-collections-arrays-slices-and-maps/03-slice-expressions-and-sub-slicing/09-ordered-schedule-insert-delete
go mod edit -go=1.24
```

## Why the slices package, not hand-rolled shifting

Keeping a slice sorted under insert and delete is the textbook place people write
index arithmetic by hand — and quietly introduce two bugs. `slices.Insert(s, i, v)`
and `slices.Delete(s, i, j)` remove the arithmetic and, more importantly, get the
memory right.

For insertion, `sort.Search` finds the index of the first job strictly after the new
one's `RunAt`; inserting there keeps the slice non-decreasing and is stable for equal
times (the new job lands after existing equals). `slices.Insert` opens a gap at that
index by shifting the tail right, reallocating if capacity is exhausted. Doing this
by hand with `append(s[:i], append([]*Job{v}, s[i:]...)...)` allocates a throwaway
intermediate slice and is easy to get subtly wrong.

For deletion the stakes are higher because the elements are pointers. `slices.Delete(s, i, i+1)`
shifts the tail left to close the gap and then *zeroes the now-unused tail slot* — it
sets the old last element to `nil`. The hand-rolled `append(s[:i], s[i+1:]...)` shifts
correctly but leaves the old last slot holding a duplicate `*Job` pointer: that
pointer is still reachable through the backing array (the slice's capacity reaches
it), so the cancelled job cannot be collected. In a long-running scheduler that
inserts and deletes constantly, this is a steady leak of cancelled jobs. The tests
below make the difference concrete: `slices.Delete` leaves the tail slot `nil`, the
hand-rolled version leaves it pointing at the deleted job.

Create `schedule.go`:

```go
package schedule

import (
	"slices"
	"sort"
	"time"
)

// Job is a scheduled unit of work, ordered by RunAt.
type Job struct {
	ID    string
	RunAt time.Time
}

// Schedule keeps jobs sorted ascending by RunAt.
type Schedule struct {
	jobs []*Job
}

// Insert places j at its sorted position, keeping the schedule non-decreasing.
func (s *Schedule) Insert(j *Job) {
	i := sort.Search(len(s.jobs), func(i int) bool {
		return s.jobs[i].RunAt.After(j.RunAt)
	})
	s.jobs = slices.Insert(s.jobs, i, j)
}

// Delete removes the job with the given id, reporting whether one was found.
// slices.Delete zeroes the vacated tail slot so the removed *Job is collectable.
func (s *Schedule) Delete(id string) bool {
	i := slices.IndexFunc(s.jobs, func(j *Job) bool { return j.ID == id })
	if i < 0 {
		return false
	}
	s.jobs = slices.Delete(s.jobs, i, i+1)
	return true
}

// Jobs returns the schedule's internal slice for inspection. Callers must not
// mutate it; it is exposed for read-only iteration and tests.
func (s *Schedule) Jobs() []*Job {
	return s.jobs
}

// Sorted reports whether the schedule is non-decreasing by RunAt.
func (s *Schedule) Sorted() bool {
	return slices.IsSortedFunc(s.jobs, func(a, b *Job) int {
		return a.RunAt.Compare(b.RunAt)
	})
}
```

## The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/schedule"
)

func main() {
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	var s schedule.Schedule

	// Insert out of order.
	s.Insert(&schedule.Job{ID: "c", RunAt: base.Add(30 * time.Minute)})
	s.Insert(&schedule.Job{ID: "a", RunAt: base.Add(5 * time.Minute)})
	s.Insert(&schedule.Job{ID: "b", RunAt: base.Add(15 * time.Minute)})

	fmt.Print("after inserts: ")
	for _, j := range s.Jobs() {
		fmt.Printf("%s ", j.ID)
	}
	fmt.Printf("(sorted=%v)\n", s.Sorted())

	// Cancel one.
	s.Delete("b")
	fmt.Print("after delete b: ")
	for _, j := range s.Jobs() {
		fmt.Printf("%s ", j.ID)
	}
	fmt.Printf("(sorted=%v, len=%d)\n", s.Sorted(), len(s.Jobs()))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
after inserts: a b c (sorted=true)
after delete b: a c (sorted=true, len=2)
```

## Tests

The insert test drops jobs in scrambled order and asserts `Sorted()` after each. The
delete test checks order, length, and that the vacated tail slot is `nil`. The
hand-rolled-deletion test demonstrates the leak `slices.Delete` avoids. A
`BinarySearchFunc` test cross-checks locating a job.

Create `schedule_test.go`:

```go
package schedule

import (
	"slices"
	"testing"
	"time"
)

func at(min int) time.Time {
	return time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(min) * time.Minute)
}

func TestInsertKeepsSorted(t *testing.T) {
	t.Parallel()
	var s Schedule
	order := []struct {
		id  string
		min int
	}{
		{"c", 30}, {"a", 5}, {"e", 50}, {"b", 15}, {"d", 40},
	}
	for _, o := range order {
		s.Insert(&Job{ID: o.id, RunAt: at(o.min)})
		if !s.Sorted() {
			t.Fatalf("not sorted after inserting %s", o.id)
		}
	}
	got := make([]string, len(s.Jobs()))
	for i, j := range s.Jobs() {
		got[i] = j.ID
	}
	if want := []string{"a", "b", "c", "d", "e"}; !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestDeletePreservesOrderAndNilsTail(t *testing.T) {
	t.Parallel()
	var s Schedule
	for _, o := range []struct {
		id  string
		min int
	}{{"a", 5}, {"b", 15}, {"c", 30}} {
		s.Insert(&Job{ID: o.id, RunAt: at(o.min)})
	}
	oldLen := len(s.Jobs())

	if !s.Delete("b") {
		t.Fatal("Delete(b) reported not found")
	}
	if s.Delete("zzz") {
		t.Fatal("Delete(zzz) reported found")
	}

	got := make([]string, len(s.Jobs()))
	for i, j := range s.Jobs() {
		got[i] = j.ID
	}
	if want := []string{"a", "c"}; !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}

	// slices.Delete zeroes the vacated tail slot: index oldLen-1 must be nil.
	full := s.Jobs()[:oldLen]
	if full[oldLen-1] != nil {
		t.Fatalf("tail slot not zeroed: %+v (removed *Job leaked)", full[oldLen-1])
	}
}

// TestHandRolledDeleteLeaks documents why we use slices.Delete: the manual form
// leaves a dangling pointer in the old tail slot.
func TestHandRolledDeleteLeaks(t *testing.T) {
	t.Parallel()
	jobs := []*Job{
		{ID: "a", RunAt: at(5)},
		{ID: "b", RunAt: at(15)},
		{ID: "c", RunAt: at(30)},
	}
	oldLen := len(jobs)
	i := 1 // delete "b"

	manual := append(jobs[:i], jobs[i+1:]...) // shifts, but does NOT zero the tail

	if len(manual) != oldLen-1 {
		t.Fatalf("len = %d, want %d", len(manual), oldLen-1)
	}
	// The old last slot still points at the (now duplicated) job "c": a leak.
	full := manual[:oldLen]
	if full[oldLen-1] == nil {
		t.Fatal("expected hand-rolled delete to leave a dangling tail pointer, but it was nil")
	}
}

func TestBinarySearchFindsJob(t *testing.T) {
	t.Parallel()
	var s Schedule
	for _, o := range []struct {
		id  string
		min int
	}{{"a", 5}, {"b", 15}, {"c", 30}} {
		s.Insert(&Job{ID: o.id, RunAt: at(o.min)})
	}

	target := &Job{RunAt: at(15)}
	i, found := slices.BinarySearchFunc(s.Jobs(), target, func(a, b *Job) int {
		return a.RunAt.Compare(b.RunAt)
	})
	if !found {
		t.Fatal("BinarySearchFunc did not find the 15-minute job")
	}
	if s.Jobs()[i].ID != "b" {
		t.Fatalf("found job %q at %d, want b", s.Jobs()[i].ID, i)
	}
}
```

## Review

The schedule is correct when it is non-decreasing after every insert and delete,
deletions preserve order and length, and removed jobs are actually collectable. The
sorted test and the order assertions pin the ordering; the tail-nil test pins the GC
behavior that separates `slices.Delete` from the hand-rolled shift. The wrong turn is
`append(s[:i], s[i+1:]...)` on a slice of pointers: it looks equivalent, passes an
order check, and leaks the deleted job through the un-zeroed tail slot — exactly the
mistake the hand-rolled test documents. Prefer the `slices` helpers for structured
mutation; they get the shift, the reallocation, and the pointer zeroing right. Run
`go test -race`.

## Resources

- [`slices.Insert`](https://pkg.go.dev/slices#Insert)
- [`slices.Delete`](https://pkg.go.dev/slices#Delete)
- [`slices.BinarySearchFunc`](https://pkg.go.dev/slices#BinarySearchFunc)
- [`sort.Search`](https://pkg.go.dev/sort#Search)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [08-repository-page-defensive-copy.md](08-repository-page-defensive-copy.md) | Next: [10-cursor-pagination-bounds-clamp.md](10-cursor-pagination-bounds-clamp.md)
