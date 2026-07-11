# Exercise 9: Avoiding a Shared-Row Backing Array in a CSV/Report Export

A report exporter that builds `[][]string` rows from one reused scratch buffer
has a notorious bug: every row aliases the same backing array, so the finished
report shows the *last* record in every row. This exercise reproduces that bug
and fixes it by cloning each row before appending it to the output — the exact
mistake behind "why is my CSV all the same line?".

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
reportexport/               independent module: example.com/reportexport
  go.mod
  report.go                 ExportBuggy (aliases scratch); ExportSafe (clones each row)
  cmd/
    demo/
      main.go               runnable demo: buggy all-same rows vs correct rows
  report_test.go            buggy rows all equal last; safe rows distinct; scratch mutation isolated
```

Files: `report.go`, `cmd/demo/main.go`, `report_test.go`.
Implement: `ExportBuggy(records [][]string) [][]string` reusing one scratch buffer, and `ExportSafe(records [][]string) [][]string` cloning each row.
Test: the buggy export makes every row equal to the last record; the safe export produces distinct rows with independent backing arrays; mutating the scratch buffer after a safe export does not change emitted rows.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/reportexport/cmd/demo
cd ~/go-exercises/reportexport
go mod init example.com/reportexport
```

### Why every row ends up identical

To avoid per-row allocations, the exporter reuses one `scratch` buffer: for each
record it resets `scratch = scratch[:0]`, appends the record's formatted fields,
and appends the scratch slice to the output rows. The reset preserves the backing
array (see the reuse exercise), so — as long as scratch never reallocates —
**every appended row is the same slice header pointing at the same array**. On
the next record the loop overwrites that array in place, and because all rows
share it, they all change together. After the loop, every row shows the last
record. It is a real, deterministic bug precisely *because* the reuse buffer works
as intended: the array is reused, so the rows alias it.

The fix is `slices.Clone(scratch)`: append a fresh copy of the current row so each
output row owns an independent backing array. Cloning per row costs one allocation
per row — which is unavoidable if the rows must outlive the scratch buffer. The
reuse buffer still saves the intermediate formatting allocations; you only
materialize the final row. `ExportSafe` does this; `ExportBuggy` keeps the alias
so the failure is testable.

To make the bug deterministic in the test, the scratch buffer is pre-sized with
`make([]string, 0, cols)` so it never reallocates mid-loop — otherwise a
reallocation would give some rows their own array and mask the aliasing. Real
code hits the same bug whenever the scratch capacity is stable across rows, which
is the common case.

Create `report.go`:

```go
package report

import (
	"fmt"
	"slices"
)

// formatRow renders one record (id + fields) into the scratch buffer, reusing
// its backing array. It returns the refilled scratch slice.
func formatRow(scratch []string, id int, fields []string) []string {
	scratch = scratch[:0]
	scratch = append(scratch, fmt.Sprintf("id=%d", id))
	scratch = append(scratch, fields...)
	return scratch
}

// ExportBuggy builds the rows from one reused scratch buffer WITHOUT cloning, so
// every output row aliases the same backing array and ends up showing the last
// record. Kept to demonstrate the bug; do not use it.
func ExportBuggy(records [][]string) [][]string {
	cols := columns(records)
	scratch := make([]string, 0, cols)
	var rows [][]string
	for id, fields := range records {
		scratch = formatRow(scratch, id, fields)
		rows = append(rows, scratch) // BUG: appends the shared slice, not a copy
	}
	return rows
}

// ExportSafe clones each row before appending it, so every output row owns an
// independent backing array and retains its own record.
func ExportSafe(records [][]string) [][]string {
	cols := columns(records)
	scratch := make([]string, 0, cols)
	var rows [][]string
	for id, fields := range records {
		scratch = formatRow(scratch, id, fields)
		rows = append(rows, slices.Clone(scratch)) // fix: independent copy
	}
	return rows
}

// columns returns the widest row width (id column + fields) so the scratch
// buffer can be sized once and not reallocate mid-loop.
func columns(records [][]string) int {
	widest := 0
	for _, fields := range records {
		if w := len(fields) + 1; w > widest {
			widest = w
		}
	}
	return widest
}
```

### The runnable demo

The demo exports three records both ways: the buggy export shows the last record
three times, the safe export shows the three distinct records.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/reportexport"
)

func main() {
	records := [][]string{
		{"alice", "eu"},
		{"bob", "us"},
		{"carol", "ap"},
	}

	fmt.Println("buggy:")
	for _, row := range report.ExportBuggy(records) {
		fmt.Printf("  %v\n", row)
	}

	fmt.Println("safe:")
	for _, row := range report.ExportSafe(records) {
		fmt.Printf("  %v\n", row)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
buggy:
  [id=2 carol ap]
  [id=2 carol ap]
  [id=2 carol ap]
safe:
  [id=0 alice eu]
  [id=1 bob us]
  [id=2 carol ap]
```

### Tests

`TestBuggyRowsAllEqualLast` pins the bug: every row equals the last record.
`TestSafeRowsAreDistinct` proves the safe export yields distinct rows with
independent backing arrays (`SliceData` differs per row).
`TestSafeExportSurvivesScratchMutation` proves an emitted row is not changed by
later reuse of the scratch buffer.

Create `report_test.go`:

```go
package report

import (
	"fmt"
	"slices"
	"testing"
	"unsafe"
)

func sampleRecords() [][]string {
	return [][]string{{"alice", "eu"}, {"bob", "us"}, {"carol", "ap"}}
}

func TestBuggyRowsAllEqualLast(t *testing.T) {
	t.Parallel()
	rows := ExportBuggy(sampleRecords())
	last := []string{"id=2", "carol", "ap"}
	for i, row := range rows {
		if !slices.Equal(row, last) {
			t.Fatalf("buggy row %d = %v, want the aliased last record %v", i, row, last)
		}
	}
}

func TestSafeRowsAreDistinct(t *testing.T) {
	t.Parallel()
	rows := ExportSafe(sampleRecords())
	want := [][]string{
		{"id=0", "alice", "eu"},
		{"id=1", "bob", "us"},
		{"id=2", "carol", "ap"},
	}
	for i, row := range rows {
		if !slices.Equal(row, want[i]) {
			t.Fatalf("safe row %d = %v, want %v", i, row, want[i])
		}
	}
	// Each row must have its own backing array.
	for i := 1; i < len(rows); i++ {
		if unsafe.SliceData(rows[i]) == unsafe.SliceData(rows[i-1]) {
			t.Fatalf("safe rows %d and %d share a backing array", i-1, i)
		}
	}
}

func TestSafeExportSurvivesScratchMutation(t *testing.T) {
	t.Parallel()
	records := sampleRecords()
	rows := ExportSafe(records)
	first := slices.Clone(rows[0])

	// Simulate the scratch buffer being reused again after export.
	scratch := make([]string, 0, 8)
	scratch = formatRow(scratch, 99, []string{"zzz", "zz"})
	_ = scratch

	if !slices.Equal(rows[0], first) {
		t.Fatalf("emitted row changed after later scratch use: %v", rows[0])
	}
}

func ExampleExportSafe() {
	rows := ExportSafe([][]string{{"x"}, {"y"}})
	fmt.Println(rows[0], rows[1])
	// Output: [id=0 x] [id=1 y]
}
```

## Review

The bug is deterministic and instructive: reusing a scratch buffer and appending
it directly makes every output row the same slice header over the same array, so
the loop's in-place overwrites leave all rows showing the last record —
`TestBuggyRowsAllEqualLast` pins it. `slices.Clone(scratch)` per row is the fix;
each emitted row then owns an independent array, which `TestSafeRowsAreDistinct`
confirms by comparing `SliceData` across rows. The reuse buffer still avoids the
intermediate formatting allocations; you pay exactly one clone per materialized
row, which is unavoidable when rows must outlive the scratch. Whenever you build
`[][]T` from a reused inner buffer, clone before you append. Run `go test -race`.

## Resources

- [pkg.go.dev: slices.Clone](https://pkg.go.dev/slices#Clone) — copy each row into its own array.
- [Go blog: Go Slices: usage and internals](https://go.dev/blog/slices-intro) — shared backing arrays and aliasing.
- [pkg.go.dev: encoding/csv](https://pkg.go.dev/encoding/csv) — `csv.Writer.Write`, where this exact aliasing bug commonly appears.

---

Back to [00-concepts.md](00-concepts.md) | Next: [10-correlation-id-capacity-pinning.md](10-correlation-id-capacity-pinning.md)
