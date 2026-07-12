# Exercise 3: The -update flag — regenerate golden files without hand-editing

Once a golden lives in a file, editing it by hand on every intentional output change is tedious and error-prone. The standard ergonomic is a package-level `-update` flag: the same test compares against the golden by default and rewrites it when you pass `-update`. This module renders a multi-line report and gates it behind exactly that pattern.

## What you'll build

```text
report/                       independent module: example.com/reportgold
  go.mod                      go 1.26
  report.go                   Render(title, []Line) []byte  (deterministic text report)
  cmd/
    demo/
      main.go                 renders a sample report and prints it
  report_test.go              flag.Bool("update"); assertGolden helper; -short skip
  testdata/
    summary.golden            committed expected output
    large.golden              committed expected output (skipped under -short)
```

Files: `report.go`, `cmd/demo/main.go`, `report_test.go`, `testdata/summary.golden`, `testdata/large.golden`.
Implement: `Render` producing a deterministic multi-line report (regions sorted, a total line).
Test: `assertGolden(t, name, got)` that writes the golden when `*update` is set and otherwise `bytes.Equal`-compares, with a failure message telling the reader to rerun with `-update`.
Verify: `go test -count=1 -race ./...`, and `go test -run TestRenderSummaryGolden -update` to regenerate.

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/07-test-fixtures-and-testdata/03-golden-update-flag/cmd/demo go-solutions/12-testing-ecosystem/07-test-fixtures-and-testdata/03-golden-update-flag/testdata
cd go-solutions/12-testing-ecosystem/07-test-fixtures-and-testdata/03-golden-update-flag
```

### The read/write asymmetry and why the diff must be reviewed

A golden test is a comparison. The `-update` flag turns that same test into a generator: when `*update` is true it writes what the code produced to the golden file and returns without asserting anything; when it is false it reads the committed golden and compares. That asymmetry is the whole trick, and it carries a discipline that is easy to skip and dangerous to skip. A regenerated golden is a snapshot of whatever the code produced at that moment — including a bug. `go test -update` followed by `git add` without reading the diff is rubber-stamping the current output as correct. The golden diff must be reviewed exactly like a code diff; a golden you cannot explain is a bug you just blessed. The flag exists to save you from retyping fifty lines, not from thinking about them.

Two mechanical points make the pattern robust. Write the golden with an explicit permission (`os.WriteFile(path, got, 0o644)`) so the generated file is readable and committable rather than inheriting an accidental mode. And make the failure message on a mismatch actionable: print both sides and tell the reader the exact command to regenerate, so a legitimate output change is a ten-second fix instead of a hand-edit.

The renderer is deterministic on purpose: it sorts regions before emitting, so the output does not depend on map iteration order or input order. Determinism is the precondition for any golden test — an output that varies run to run cannot be pinned to a committed file. (The next exercises tackle outputs that are *not* naturally deterministic.)

Create `report.go`:

```go
package report

import (
	"fmt"
	"sort"
	"strings"
)

// Line is one region's contribution to a report.
type Line struct {
	Region string
	Orders int
	Cents  int64
}

// Render produces a deterministic multi-line text report. Regions are sorted
// so the output is stable regardless of input order.
func Render(title string, lines []Line) []byte {
	sorted := append([]Line(nil), lines...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Region < sorted[j].Region })

	var b strings.Builder
	fmt.Fprintf(&b, "== %s ==\n", title)
	var total int64
	for _, ln := range sorted {
		fmt.Fprintf(&b, "%s orders=%d revenue=%d\n", ln.Region, ln.Orders, ln.Cents)
		total += ln.Cents
	}
	fmt.Fprintf(&b, "TOTAL revenue=%d\n", total)
	return []byte(b.String())
}
```

Now the committed goldens. These are the expected outputs — a reviewer reads them as the contract.

Create `testdata/summary.golden`:

```text
== Daily Orders ==
amer orders=2 revenue=3000
apac orders=5 revenue=9900
emea orders=3 revenue=4500
TOTAL revenue=17400
```

Create `testdata/large.golden`:

```text
== Regional Rollup ==
amer orders=2 revenue=3000
apac orders=5 revenue=9900
emea orders=3 revenue=4500
latam orders=1 revenue=1500
TOTAL revenue=18900
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/reportgold"
)

func main() {
	out := report.Render("Daily Orders", []report.Line{
		{Region: "emea", Orders: 3, Cents: 4500},
		{Region: "apac", Orders: 5, Cents: 9900},
		{Region: "amer", Orders: 2, Cents: 3000},
	})
	fmt.Print(string(out))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
== Daily Orders ==
amer orders=2 revenue=3000
apac orders=5 revenue=9900
emea orders=3 revenue=4500
TOTAL revenue=17400
```

### The test

The `update` flag is declared once at package scope. `assertGolden` branches on it: write-and-return under `-update`, read-and-compare otherwise. The large-report case is skipped under `-short` to model the real habit of skipping expensive golden regeneration during quick iteration — a genuine use of `testing.Short`, not decoration.

Create `report_test.go`:

```go
package report

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var update = flag.Bool("update", false, "regenerate golden files")

func assertGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)

	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("update golden %s: %v", name, err)
		}
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s (rerun with -update to create): %v", name, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("golden mismatch for %s\ngot:\n%s\nwant:\n%s\nrerun: go test -run %s -update",
			name, got, want, t.Name())
	}
}

func TestRenderSummaryGolden(t *testing.T) {
	t.Parallel()

	got := Render("Daily Orders", []Line{
		{Region: "emea", Orders: 3, Cents: 4500},
		{Region: "apac", Orders: 5, Cents: 9900},
		{Region: "amer", Orders: 2, Cents: 3000},
	})
	assertGolden(t, "summary.golden", got)
}

func TestRenderLargeGolden(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping large golden under -short")
	}

	got := Render("Regional Rollup", []Line{
		{Region: "latam", Orders: 1, Cents: 1500},
		{Region: "emea", Orders: 3, Cents: 4500},
		{Region: "apac", Orders: 5, Cents: 9900},
		{Region: "amer", Orders: 2, Cents: 3000},
	})
	assertGolden(t, "large.golden", got)
}
```

## Review

The pattern is correct when the default run compares and only `-update` writes, and when a mismatch names the regenerate command. The failure this exercise guards against is the social one, not a compile error: regenerating a golden and committing it without reading the diff blesses whatever the code emitted, bug included. Treat the golden diff as a code diff. Two smaller traps: write the golden with an explicit `0o644` so it is committable, and keep the renderer deterministic (sort before emit) so the golden is stable — a golden test over non-deterministic output flakes and teaches the team to ignore red, which is worse than no test. Regenerate deliberately with `go test -run TestRenderSummaryGolden -update`, then read what changed.

## Resources

- [flag package](https://pkg.go.dev/flag) — `flag.Bool` for a custom test flag like `-update`.
- [os.WriteFile](https://pkg.go.dev/os#WriteFile) — writing a regenerated golden with an explicit file mode.
- [testing: Short](https://pkg.go.dev/testing#Short) — skipping expensive cases under `-short`.

---

Back to [02-load-fixture-from-testdata.md](02-load-fixture-from-testdata.md) | Next: [04-glob-driven-table.md](04-glob-driven-table.md)
