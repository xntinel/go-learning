# Exercise 6: An Aligned Status Table with text/tabwriter

The `status` subcommand of an ops CLI prints a kubectl-style table: resource name,
state, restart count, age. Manual padding breaks the moment a value is wider than
you guessed. This exercise builds the table with `text/tabwriter`, which measures
each column across all rows and aligns them for you.

This module is fully self-contained: its own `go mod init`, code, demo, and tests.

## What you'll build

```text
statustable/               independent module: example.com/statustable
  go.mod                   go 1.24
  statustable.go           type Resource; Render(io.Writer, []Resource); RenderMetrics(...)
  cmd/
    demo/
      main.go              runnable demo: print an aligned status table
  statustable_test.go      exact-alignment + no-Flush-footgun + long-value + right-align tests
```

- Files: `statustable.go`, `cmd/demo/main.go`, `statustable_test.go`.
- Implement: `Render(w io.Writer, rows []Resource)` writing a tab-separated header and rows through a `tabwriter.Writer` and calling `Flush`; `RenderMetrics(w, pairs)` right-aligning a numeric column with `tabwriter.AlignRight` and a trailing tab.
- Test: exact aligned output for a fixed row set; that skipping `Flush` yields empty output; alignment holds when one row has a very long value; the right-aligned metric column lines up.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/statustable/cmd/demo
cd ~/go-exercises/statustable
go mod init example.com/statustable
go mod edit -go=1.24
```

### How tabwriter aligns, and the two footguns

`tabwriter.NewWriter(out, minwidth, tabwidth, padding, padchar, flags)` returns a
writer that buffers everything you write, splits each line into cells on `\t`,
measures the widest cell in each column across *all* buffered rows, and on `Flush`
expands every tab so the columns line up. You write ordinary tab-separated
`fmt.Fprintf(w, "%s\t%s\t%d\t%s\n", ...)`; the writer does the width arithmetic
that manual `%-20s` padding gets wrong the moment a value overflows the guessed
width.

Two rules are non-negotiable, and the tests pin both:

1. **You must call `Flush`.** The writer holds every byte in an internal buffer
   until `Flush`, because it cannot know a column's width until it has seen the
   last row. Forget it and the output is empty (or, if you flushed mid-stream,
   misaligned). `defer w.Flush()` right after `NewWriter` is the habit — but note
   that if you read the destination buffer *before* the deferred flush runs, you
   read nothing, so an explicit `w.Flush()` before reading is clearer in a
   function that returns the bytes.

2. **Text after the final tab on a line is not a cell.** tabwriter only pads
   *cells*, and a cell is text terminated by a tab. The trailing segment after the
   last tab is "trailing text" and is emitted as-is. For a left-aligned table this
   is fine (you want the last column ragged-right anyway). But to *right-align* the
   last column you must terminate it with a trailing tab (`...\t%d\t\n`) so it
   becomes a real, padded cell.

For the parameters here: `minwidth=0`, `tabwidth=0`, `padding=2` (two spaces
between columns), `padchar=' '`, `flags=0` for the left-aligned status table.
`tabwriter.AlignRight` in the flags right-justifies cells across the whole
writer — it is per-writer, not per-column, which is why the numeric-column demo
uses a separate two-column writer.

Create `statustable.go`:

```go
package statustable

import (
	"fmt"
	"io"
	"text/tabwriter"
)

// Resource is one row of the status table.
type Resource struct {
	Name     string
	State    string
	Restarts int
	Age      string
}

// Render writes a column-aligned status table to w. Columns are sized to the
// widest value in each, so alignment holds regardless of value width. The Flush
// is mandatory: without it the buffered output is never emitted.
func Render(w io.Writer, rows []Resource) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSTATE\tRESTARTS\tAGE")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n", r.Name, r.State, r.Restarts, r.Age)
	}
	tw.Flush()
}

// RenderNoFlush is the footgun: it builds the same table but never flushes, so
// nothing is written. Present only so a test can pin the behavior.
func RenderNoFlush(w io.Writer, rows []Resource) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSTATE\tRESTARTS\tAGE")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n", r.Name, r.State, r.Restarts, r.Age)
	}
	// no Flush: buffered output is lost
}

// RenderMetrics writes a two-column key/value table with the numeric value column
// right-aligned. The trailing tab after the value makes it a padded cell so
// AlignRight can right-justify it.
func RenderMetrics(w io.Writer, pairs [][2]any) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	for _, p := range pairs {
		fmt.Fprintf(tw, "%v\t%v\t\n", p[0], p[1])
	}
	tw.Flush()
}
```

### The runnable demo

The demo prints a realistic pod-status table with values of very different widths
(`api-server` vs `worker-abcdef`, `Running` vs `CrashLoopBackOff`) and shows the
columns still line up.

Create `cmd/demo/main.go`:

```go
package main

import (
	"os"

	"example.com/statustable"
)

func main() {
	rows := []statustable.Resource{
		{Name: "api-server", State: "Running", Restarts: 0, Age: "5d"},
		{Name: "worker-abcdef", State: "CrashLoopBackOff", Restarts: 128, Age: "2h"},
		{Name: "db", State: "Running", Restarts: 1, Age: "30d"},
	}
	statustable.Render(os.Stdout, rows)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
NAME           STATE             RESTARTS  AGE
api-server     Running           0         5d
worker-abcdef  CrashLoopBackOff  128       2h
db             Running           1         30d
```

### Tests

`TestRenderAligned` renders a fixed row set into a `bytes.Buffer` and asserts the
exact aligned bytes — the golden output that fixes column widths and padding.
`TestNoFlushIsEmpty` documents the footgun: the un-flushed variant writes nothing.
`TestLongValueStillAligns` adds a row with a very long name and asserts every data
line still starts its STATE column at the same offset, proving the writer re-sized
the column. `TestMetricsRightAligned` checks the right-aligned numeric column.

Create `statustable_test.go`:

```go
package statustable

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

var sample = []Resource{
	{Name: "api-server", State: "Running", Restarts: 0, Age: "5d"},
	{Name: "worker-abcdef", State: "CrashLoopBackOff", Restarts: 128, Age: "2h"},
	{Name: "db", State: "Running", Restarts: 1, Age: "30d"},
}

func TestRenderAligned(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	Render(&buf, sample)

	want := "" +
		"NAME           STATE             RESTARTS  AGE\n" +
		"api-server     Running           0         5d\n" +
		"worker-abcdef  CrashLoopBackOff  128       2h\n" +
		"db             Running           1         30d\n"
	if got := buf.String(); got != want {
		t.Fatalf("Render mismatch:\n got %q\nwant %q", got, want)
	}
}

func TestNoFlushIsEmpty(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	RenderNoFlush(&buf, sample)
	if buf.Len() != 0 {
		t.Fatalf("expected empty output without Flush, got %q", buf.String())
	}
}

func TestLongValueStillAligns(t *testing.T) {
	t.Parallel()

	rows := []Resource{
		{Name: "x", State: "Running", Restarts: 0, Age: "1d"},
		{Name: "a-really-long-resource-name", State: "Pending", Restarts: 0, Age: "2d"},
	}
	var buf bytes.Buffer
	Render(&buf, rows)

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	// The STATE column must start at the same byte offset on every line.
	off := strings.Index(lines[0], "STATE")
	for i, ln := range lines[1:] {
		state := "Running"
		if i == 1 {
			state = "Pending"
		}
		if got := strings.Index(ln, state); got != off {
			t.Fatalf("line %q: STATE at %d, want %d", ln, got, off)
		}
	}
}

func TestMetricsRightAligned(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	RenderMetrics(&buf, [][2]any{
		{"requests_total", 148223},
		{"errors", 7},
		{"p99_ms", 341},
	})
	want := "" +
		"  requests_total  148223\n" +
		"          errors       7\n" +
		"          p99_ms     341\n"
	if got := buf.String(); got != want {
		t.Fatalf("RenderMetrics mismatch:\n got %q\nwant %q", got, want)
	}
}

func Example() {
	Render(&bytes.Buffer{}, nil) // header-only render is exercised in tests
	fmt.Println("ok")
	// Output: ok
}
```

## Review

The table is correct when the rendered bytes match the golden output exactly and
the columns re-size to fit the widest value — `TestRenderAligned` pins the fixed
case and `TestLongValueStillAligns` proves the STATE column shifts right when a
name grows. The two footguns are the whole lesson: `TestNoFlushIsEmpty` shows that
without `Flush` the buffered output is lost, and `RenderMetrics` shows that
right-aligning the last column requires a trailing tab to turn it into a padded
cell (without it, `AlignRight` would jam the value against the previous column).
`tabwriter.AlignRight` is a per-writer flag, not per-column, so a mixed table uses
a dedicated writer for the right-aligned view. Run `go test -race`; the writer is
used per-call with no shared state.

## Resources

- [`text/tabwriter`](https://pkg.go.dev/text/tabwriter) — `NewWriter`, `Flush`, `AlignRight`, and the cell/trailing-text rule.
- [`fmt.Fprintf`](https://pkg.go.dev/fmt#Fprintf) — writing tab-separated cells to the writer.
- [`bytes.Buffer`](https://pkg.go.dev/bytes#Buffer) — capturing rendered output in tests.

---

Back to [00-concepts.md](00-concepts.md) | Next: [07-human-readable-metrics.md](07-human-readable-metrics.md)
