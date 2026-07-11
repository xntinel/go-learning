# Exercise 3: Ranging Over []T Copies — The Batch Update That Does Nothing

A batch reconciler that marks expired jobs failed is a one-loop function, and the
first version everyone writes silently reconciles nothing. `for _, j := range jobs`
binds `j` to a *copy* of each element; the write to `j.Status` lands on the copy
and is thrown away. This module builds the reconciler both ways and tests that the
buggy one changes nothing while the indexed one persists every write.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
batchreconcile/               independent module: example.com/batchreconcile
  go.mod                      go 1.24
  reconcile.go                []Job value slice; failExpiredBuggy (no-op) vs failExpired (indexed)
  reconcile_test.go           proves range-copy drops writes, indexed mutation persists
  cmd/demo/main.go            runnable demo contrasting the two
```

Files: `reconcile.go`, `reconcile_test.go`, `cmd/demo/main.go`.
Implement: over a `[]Job`, a buggy `failExpiredBuggy` using `for _, j := range`
and a correct `FailExpired` using `for i := range jobs` that returns the count
changed.
Test: the buggy version leaves the slice untouched; the correct version changes
every expired element and returns the right count.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/batchreconcile/cmd/demo
cd ~/go-exercises/batchreconcile
go mod init example.com/batchreconcile
```

### Why the copy loop drops the write

A `[]Job` stores values inline in the backing array. `for _, j := range jobs`
copies each element into the loop variable `j` on every iteration — `j` is a fresh
`Job` value, not a window into the slice. Assigning `j.Status = StatusFailed`
mutates that local copy, which is discarded when the iteration ends. The slice is
untouched. The compiler does not warn, because the code is legal; it is just
useless. This is one of the most common "why didn't my update take" bugs in Go,
and it is entirely about `[]T` addressability: the element `jobs[i]` is
addressable and assignable, but the range-value copy `j` is a different object.

The fix is to write through the slice index. `for i := range jobs` iterates
positions, and `jobs[i].Status = StatusFailed` assigns to the addressable element
in place. Equivalently, take a pointer once — `p := &jobs[i]; p.Status = ...` —
which is handy when you mutate several fields. Either way the write reaches the
backing array. `FailExpired` uses the indexed form and returns how many elements it
changed, which is the signal a real reconciler reports to its metrics.

Create `reconcile.go`:

```go
package batchreconcile

import "time"

type Status string

const (
	StatusActive Status = "active"
	StatusFailed Status = "failed"
)

// Job is a value type; a []Job stores these inline in the backing array.
type Job struct {
	ID        string
	Status    Status
	ExpiresAt time.Time
}

// failExpiredBuggy is the classic bug: j is a per-iteration COPY, so the write
// to j.Status is discarded and the slice is never modified. Kept to test the
// hazard. It reports a count it "changed" that never actually lands.
func failExpiredBuggy(jobs []Job, now time.Time) int {
	changed := 0
	for _, j := range jobs {
		if j.Status == StatusActive && !j.ExpiresAt.After(now) {
			j.Status = StatusFailed // writes to the copy; lost
			changed++
		}
	}
	return changed
}

// FailExpired writes through the slice index, so the mutation persists. It
// returns the number of jobs it transitioned to failed.
func FailExpired(jobs []Job, now time.Time) int {
	changed := 0
	for i := range jobs {
		if jobs[i].Status == StatusActive && !jobs[i].ExpiresAt.After(now) {
			jobs[i].Status = StatusFailed
			changed++
		}
	}
	return changed
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/batchreconcile"
)

func main() {
	now := time.Now()
	fixed := []batchreconcile.Job{
		{ID: "j1", Status: batchreconcile.StatusActive, ExpiresAt: now.Add(-time.Hour)},
		{ID: "j2", Status: batchreconcile.StatusActive, ExpiresAt: now.Add(time.Hour)},
		{ID: "j3", Status: batchreconcile.StatusActive, ExpiresAt: now.Add(-time.Minute)},
	}

	n := batchreconcile.FailExpired(fixed, now)
	fmt.Printf("failed %d expired jobs\n", n)
	for _, j := range fixed {
		fmt.Printf("%s %s\n", j.ID, j.Status)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
failed 2 expired jobs
j1 failed
j2 active
j3 failed
```

### Tests

`TestRangeCopyDoesNotMutate` runs the buggy function and asserts the slice is
unchanged even though it *reported* a nonzero count — the count is a lie because
the write went to a copy. `TestIndexedMutationPersists` runs `FailExpired` and
asserts both the returned count and that every targeted element actually changed.
No loop-variable aliasing anywhere; this is `go 1.24` semantics.

Create `reconcile_test.go`:

```go
package batchreconcile

import (
	"testing"
	"time"
)

func sample(now time.Time) []Job {
	return []Job{
		{ID: "j1", Status: StatusActive, ExpiresAt: now.Add(-time.Hour)},
		{ID: "j2", Status: StatusActive, ExpiresAt: now.Add(time.Hour)},
		{ID: "j3", Status: StatusActive, ExpiresAt: now.Add(-time.Minute)},
	}
}

func TestRangeCopyDoesNotMutate(t *testing.T) {
	t.Parallel()

	now := time.Now()
	jobs := sample(now)

	reported := failExpiredBuggy(jobs, now)
	if reported == 0 {
		t.Fatal("test setup: expected the buggy loop to iterate at least one expired job")
	}
	for i := range jobs {
		if jobs[i].Status != StatusActive {
			t.Fatalf("jobs[%d].Status = %q; buggy range-copy must not mutate the slice", i, jobs[i].Status)
		}
	}
}

func TestIndexedMutationPersists(t *testing.T) {
	t.Parallel()

	now := time.Now()
	jobs := sample(now)

	changed := FailExpired(jobs, now)
	if changed != 2 {
		t.Fatalf("changed = %d, want 2", changed)
	}

	want := map[string]Status{"j1": StatusFailed, "j2": StatusActive, "j3": StatusFailed}
	for i := range jobs {
		if got := jobs[i].Status; got != want[jobs[i].ID] {
			t.Fatalf("%s status = %q, want %q", jobs[i].ID, got, want[jobs[i].ID])
		}
	}
}
```

## Review

The bug and the fix differ by one token — `_, j := range` versus `i := range` plus
`jobs[i]` — and that token is the whole correctness of a batch update.
`TestRangeCopyDoesNotMutate` is deliberately pointed: the buggy function returns a
nonzero count *and* leaves the slice unchanged, so a caller that trusts the return
value ships silent data loss. `FailExpired` writes through the addressable element
`jobs[i]`, so `TestIndexedMutationPersists` sees every expired job flipped and the
count honest. Reach for `&jobs[i]` when you mutate multiple fields per element; use
`jobs[i].Field = x` for a single write. Never mutate the range value of a `[]T`
slice and expect it to persist.

## Resources

- [Go spec: For statements with range clause](https://go.dev/ref/spec#For_range) — the range value is a copy of the element.
- [Go spec: Address operators](https://go.dev/ref/spec#Address_operators) — why `&s[i]` is legal (slice elements are addressable) but a range value is not the element.
- [Effective Go: Pointers vs. Values](https://go.dev/doc/effective_go#pointers_vs_values) — mutating through the value requires a stable address.

---

Back to [00-concepts.md](00-concepts.md) | Next: [04-map-value-not-addressable-session-store.md](04-map-value-not-addressable-session-store.md)
