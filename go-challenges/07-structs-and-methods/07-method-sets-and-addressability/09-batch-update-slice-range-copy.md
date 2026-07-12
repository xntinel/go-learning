# Exercise 9: Silent No-Op Batch Update — Ranging Over Struct-Value Copies

A batch status migration that "runs clean" and changes nothing is one of the
quietest bugs in Go: `for _, r := range records { r.Activate() }` mutates a fresh
copy each iteration and throws it away. This module builds the migration, proves
the range-value form is a no-op, and fixes it with the index form — because a
slice index expression is addressable and the auto-address sugar can reach it.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
batchmigrate/                  independent module: example.com/batchmigrate
  go.mod                       module path + go directive
  migrate.go                   Record with Activate(); buggy range-value + fixed index migrations
  cmd/
    demo/
      main.go                  run both migrations, show which mutates
  migrate_test.go              prove range-value is a no-op; index and []*Record mutate
```

- Files: `migrate.go`, `cmd/demo/main.go`, `migrate_test.go`.
- Implement: a `Record` with a pointer-receiver `Activate()`, a buggy migration ranging by value, a fixed migration using the index, and a pointer-slice migration.
- Test: the range-value form leaves the slice unchanged; the index form activates every record; the `[]*Record` form mutates under either loop.
- Verify: `go vet ./...`, `go test -count=1 -race ./...`.

### Why ranging by value is a no-op

`for _, r := range records` copies each element into the loop variable `r` on
every iteration. `r` is a local variable, so it is addressable, and `r.Activate()`
compiles fine — the compiler rewrites it to `(&r).Activate()` and the method
mutates `r`. But `r` is a copy of `records[i]`, not `records[i]` itself. The
method sets the copy's state to active; when the loop advances, that copy is
discarded and the next copy is made. The backing slice is never touched. The loop
runs, the code compiles, `go vet` is silent, and the migration changes nothing.

The fix is to mutate through the slice index: `for i := range records` then
`records[i].Activate()`. A slice index expression `records[i]` is addressable —
the backing array does not move under you — so the auto-address sugar forms
`&records[i]` and the pointer method mutates the element in place. Every record's
state flips.

A `[]*Record` sidesteps the whole issue. Ranging by value there copies the
*pointer*, not the `Record`; `p.Activate()` follows the pointer and mutates the
shared pointee, so `for _, p := range ptrs { p.Activate() }` works. That is why
batch code that must mutate often stores pointers — though it costs an allocation
per record and loses value-slice locality. When you hold a `[]Record`, use the
index form.

Note the modern loop idioms: `for i := range records` iterates indices, and
`for range n` (no variable) iterates a count — both are the Go 1.22+ range forms,
no manual `i := 0; i < len; i++`.

Create `migrate.go`:

```go
package batchmigrate

// Record is a row whose state a migration flips to "active". Activate mutates,
// so it takes a pointer receiver.
type Record struct {
	ID    string
	State string
}

// Activate sets the record's state in place. Pointer receiver: it mutates.
func (r *Record) Activate() { r.State = "active" }

// MigrateBuggy ranges by value: r is a COPY of each element, so Activate mutates
// the copy and the slice is left unchanged. A silent no-op.
func MigrateBuggy(records []Record) {
	for _, r := range records {
		r.Activate()
	}
}

// MigrateFixed ranges by index: records[i] is an addressable slice element, so
// records[i].Activate() mutates in place.
func MigrateFixed(records []Record) {
	for i := range records {
		records[i].Activate()
	}
}

// MigratePointers ranges over pointers: the loop copies the pointer, and
// Activate mutates the shared pointee, so this works under either loop form.
func MigratePointers(records []*Record) {
	for _, r := range records {
		r.Activate()
	}
}

// CountActive reports how many records are active (test/demo helper).
func CountActive(records []Record) int {
	n := 0
	for i := range records {
		if records[i].State == "active" {
			n++
		}
	}
	return n
}
```

### The runnable demo

The demo runs the buggy migration over a batch and shows zero records activated,
then the fixed migration over the same shape and shows all activated.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/batchmigrate"
)

func main() {
	sample := func() []batchmigrate.Record {
		return []batchmigrate.Record{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	}

	buggy := sample()
	batchmigrate.MigrateBuggy(buggy)
	fmt.Printf("buggy: %d/%d active\n", batchmigrate.CountActive(buggy), len(buggy))

	fixed := sample()
	batchmigrate.MigrateFixed(fixed)
	fmt.Printf("fixed: %d/%d active\n", batchmigrate.CountActive(fixed), len(fixed))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
buggy: 0/3 active
fixed: 3/3 active
```

### Tests

The tests prove the range-value form is a no-op (nothing activated), the index
form activates every record, and the `[]*Record` form mutates under the value-range
loop.

Create `migrate_test.go`:

```go
package batchmigrate

import "testing"

func sample() []Record {
	return []Record{{ID: "a"}, {ID: "b"}, {ID: "c"}}
}

func TestMigrateBuggyIsNoOp(t *testing.T) {
	t.Parallel()
	records := sample()
	MigrateBuggy(records)
	if got := CountActive(records); got != 0 {
		t.Fatalf("range-value migration activated %d records, want 0 (it mutates copies)", got)
	}
}

func TestMigrateFixedActivatesAll(t *testing.T) {
	t.Parallel()
	records := sample()
	MigrateFixed(records)
	if got := CountActive(records); got != len(records) {
		t.Fatalf("index migration activated %d/%d records", got, len(records))
	}
	for i := range records {
		if records[i].State != "active" {
			t.Fatalf("record %s not active", records[i].ID)
		}
	}
}

func TestMigratePointersActivatesAll(t *testing.T) {
	t.Parallel()
	ptrs := []*Record{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	MigratePointers(ptrs)
	for _, p := range ptrs {
		if p.State != "active" {
			t.Fatalf("pointer record %s not active", p.ID)
		}
	}
}
```

## Review

The migration is correct when it mutates the backing slice, not a copy.
`TestMigrateBuggyIsNoOp` pins the failure: ranging by value activates zero
records because each `r` is a throwaway copy. `TestMigrateFixedActivatesAll`
proves the index form works, because `records[i]` is an addressable slice element
the pointer method can reach in place.

The mistake this module exists to prevent is `for _, r := range records`
followed by a mutating method on `r` when `records` is a `[]Struct`. It compiles
and runs and silently does nothing to the slice. Mutate through the index
(`records[i]`) or hold a `[]*Record` so the loop shares the pointee. The bug is
invisible to the compiler and to `go vet`, so a test asserting post-migration
state is the only reliable catch. Run `go vet` and `go test -race`.

## Resources

- [Go Language Specification: For statements with range clause](https://go.dev/ref/spec#For_range) — the range variable is a copy of each element.
- [Go Language Specification: Address operators](https://go.dev/ref/spec#Address_operators) — why a slice index expression is addressable.
- [Go Wiki: Range and loop variables](https://go.dev/wiki/Range) — common range pitfalls including value copies.

---

Back to [00-concepts.md](00-concepts.md) | Next: [10-promoted-method-set-embedding-pointer.md](10-promoted-method-set-embedding-pointer.md)
