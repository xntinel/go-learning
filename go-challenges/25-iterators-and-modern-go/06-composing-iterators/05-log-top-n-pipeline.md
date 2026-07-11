# Exercise 5: Log Top-N Pipeline

A common operational task is "give me the top few rows of a large log or CSV" — the busiest endpoints, the highest scores, the worst latencies. When the input is already ordered by the field of interest (a leaderboard sorted by score, a log sorted by severity, a pre-aggregated metrics dump), the top-N is just the first N matching rows, and a composed iterator pipeline can deliver it while reading only those few lines. This exercise reads lines from a large in-memory buffer with `strings.Lines`, filters out comments and blanks, extracts a CSV field into a typed record, and takes the top N — and the test proves the scan stops the moment N records are found, never touching the rest of the buffer.

This module is fully self-contained. It begins with its own `go mod init`, defines its stages and parser, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
logtop.go            Entry, Filter, Map, Take, Reduce, IsData, ParseEntry
cmd/
  demo/
    main.go          lines -> filter -> parse -> take 3, then Reduce to a sum
logtop_test.go       top-3 aggregate + early-stop pull count, filter/parse correctness
```

- Files: `logtop.go`, `cmd/demo/main.go`, `logtop_test.go`.
- Implement: `Filter`, `Map`, `Take`, the terminal `Reduce`, the line predicate `IsData`, and `ParseEntry` (a `"name,score"` line into an `Entry`).
- Test: a `lines -> Filter -> Map -> Take(3)` pipeline returns the correct top-3 `Entry` values and their score sum; the line scan pulls *exactly* enough lines to find three records and stops, never reaching a hundred thousand lower rows or a poison row below them.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p logtop/cmd/demo && cd logtop
go mod init example.com/logtop
```

### Lines in, records out, stop at N

The pipeline is four composed stages over a line source. `strings.Lines(buf)` is the source: it yields each line of the buffer lazily, one at a time, without splitting the whole string up front. `Filter(lines, IsData)` drops blank lines and `#` comments. `Map(data, ParseEntry)` turns each surviving `"name,score"` line into an `Entry{Name, Score}`. `Take(entries, 3)` keeps the first three. Because every stage is pull-driven and `Take` returns the instant it has three records, the stop propagates back through `Map`, `Filter`, and the line source: `strings.Lines` is asked for exactly as many lines as it takes to produce three records, and then never again.

That early stop is the whole performance story, and it is honest only because the input is sorted by the field we rank on. The top-3 here is "the first three data rows," which equals "the three highest scores" precisely when the buffer is in descending score order — the realistic case for a leaderboard export, a log pre-sorted by severity, or a metrics table ordered by count. State this assumption plainly, because it is the precondition that makes short-circuiting correct: a genuine top-N over *unsorted* data cannot short-circuit at all — it must look at every row, keeping a running heap of the N best seen so far. Trading that full scan for a sorted-input `Take` is exactly the kind of design decision a streaming pipeline lets you make explicit.

### The field extraction and the terminal fold

`ParseEntry` does the field work: it trims the trailing newline that `strings.Lines` leaves on each line, splits on the first comma with `strings.Cut`, and parses the second field with `strconv.Atoi`. It is deliberately total — a missing or non-numeric score parses to `0` rather than returning an error — because the `IsData` filter upstream has already removed the lines that are not records, so anything reaching `ParseEntry` is a row we intend to read. `Reduce` is the terminal: it folds the taken records into a single aggregate (here, the sum of the top-3 scores), driving one pass over the short-circuited pipeline. It has no `yield` because it returns a plain value, not a sequence.

Create `logtop.go`:

```go
package logtop

import (
	"iter"
	"strconv"
	"strings"
)

// Entry is one parsed leaderboard / metrics row: a label and its numeric score.
type Entry struct {
	Name  string
	Score int
}

// Filter yields only the values for which keep returns true.
func Filter[V any](seq iter.Seq[V], keep func(V) bool) iter.Seq[V] {
	return func(yield func(V) bool) {
		for v := range seq {
			if keep(v) && !yield(v) {
				return
			}
		}
	}
}

// Map yields transform(v) for each upstream value, possibly changing the type.
func Map[A, B any](seq iter.Seq[A], transform func(A) B) iter.Seq[B] {
	return func(yield func(B) bool) {
		for a := range seq {
			if !yield(transform(a)) {
				return
			}
		}
	}
}

// Take yields at most the first n values and pulls exactly n from upstream
// (zero when n <= 0). It is the short-circuit: once n values are yielded it
// returns, which stops every upstream stage and the line scan with it.
func Take[V any](seq iter.Seq[V], n int) iter.Seq[V] {
	return func(yield func(V) bool) {
		if n <= 0 {
			return
		}
		count := 0
		for v := range seq {
			if !yield(v) {
				return
			}
			count++
			if count == n {
				return
			}
		}
	}
}

// Reduce folds seq into a single value, starting from initial. It is terminal:
// it consumes one pass and drives the whole pipeline.
func Reduce[V, R any](seq iter.Seq[V], initial R, combine func(R, V) R) R {
	result := initial
	for v := range seq {
		result = combine(result, v)
	}
	return result
}

// IsData reports whether a raw line carries a record: non-blank and not a
// comment beginning with '#'.
func IsData(line string) bool {
	t := strings.TrimSpace(line)
	return t != "" && !strings.HasPrefix(t, "#")
}

// ParseEntry splits a "name,score" CSV line (with or without a trailing
// newline) into an Entry. A missing or non-numeric score parses to 0.
func ParseEntry(line string) Entry {
	line = strings.TrimRight(line, "\r\n")
	name, scoreStr, _ := strings.Cut(line, ",")
	score, _ := strconv.Atoi(strings.TrimSpace(scoreStr))
	return Entry{Name: strings.TrimSpace(name), Score: score}
}
```

Read the stages as variations on one loop. `Filter` yields a line only when `IsData` holds and returns only on a downstream stop. `Map` applies `ParseEntry`, turning a `string` stream into an `Entry` stream. `Take` yields-then-counts-then-returns, so it pulls exactly three records from `Map` and, through it, exactly as many lines as those three records cost. `Reduce` ranges to the end of the (already short) taken stream and folds. `IsData` and `ParseEntry` are plain helpers with no iterator machinery — the kind of small, testable functions a real pipeline is mostly made of.

### The runnable demo

The demo runs the pipeline over a small inline buffer of five scored rows behind a comment header, prints the top three, folds their scores into a total with `Reduce`, and reports how many lines it scanned. The scan count is `4` — one comment line plus the three data rows that fill the `Take` — which shows the two trailing rows (`dave` and `erin`) were never read.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"slices"
	"strings"

	"example.com/logtop"
)

func main() {
	buf := "# daily scores\nalice,300\nbob,275\ncarol,250\ndave,225\nerin,200\n"

	scanned := 0
	lines := func(yield func(string) bool) {
		for line := range strings.Lines(buf) {
			scanned++
			if !yield(line) {
				return
			}
		}
	}

	data := logtop.Filter(lines, logtop.IsData)
	entries := logtop.Map(data, logtop.ParseEntry)
	top := logtop.Take(entries, 3)

	fmt.Println("top 3:")
	collected := make([]logtop.Entry, 0, 3)
	for e := range top {
		fmt.Printf("  %s %d\n", e.Name, e.Score)
		collected = append(collected, e)
	}

	sum := logtop.Reduce(slices.Values(collected), 0, func(acc int, e logtop.Entry) int { return acc + e.Score })
	fmt.Println("total of top 3:", sum)
	fmt.Println("lines scanned:", scanned)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
top 3:
  alice 300
  bob 275
  carol 250
total of top 3: 825
lines scanned: 4
```

### Tests

`TestTopThreeShortCircuits` is the headline. It builds a buffer with a comment header, three real leaders, a hundred thousand lower rows, and a final poison row `evil,1000000` whose score would dominate a naive global top-N. It wraps `strings.Lines` in a counting source, runs the pipeline, and asserts three things: the top-3 records are `alice, bob, carol`; their score sum is `825`; and the scan pulled *exactly four* lines (one comment, three data) — so the hundred thousand rows and the poison row below them were never read. The poison row is the proof that the pipeline short-circuits rather than scanning: had it reached the bottom, `evil` would have surfaced. `TestFilterAndParse` pins the filter-and-parse behavior on a small buffer with blanks and comments interleaved.

Create `logtop_test.go`:

```go
package logtop

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// bigLeaderboard returns a CSV buffer sorted by score descending: a comment
// header, the three real leaders, then a hundred thousand lower rows, and
// finally a poison row whose score would dominate a naive global top-N. A
// short-circuiting pipeline never reaches it.
func bigLeaderboard() string {
	var b strings.Builder
	b.WriteString("# leaderboard\n")
	b.WriteString("alice,300\n")
	b.WriteString("bob,275\n")
	b.WriteString("carol,250\n")
	for i := 0; i < 100_000; i++ {
		fmt.Fprintf(&b, "user%d,%d\n", i, 200-i%200)
	}
	b.WriteString("evil,1000000\n")
	return b.String()
}

func TestTopThreeShortCircuits(t *testing.T) {
	t.Parallel()

	buf := bigLeaderboard()

	pulled := 0
	lines := func(yield func(string) bool) {
		for line := range strings.Lines(buf) {
			pulled++
			if !yield(line) {
				return
			}
		}
	}

	data := Filter(lines, IsData)
	entries := Map(data, ParseEntry)
	top := Take(entries, 3)

	got := slices.Collect(top)
	want := []Entry{{"alice", 300}, {"bob", 275}, {"carol", 250}}
	if !slices.Equal(got, want) {
		t.Fatalf("top 3 = %v, want %v", got, want)
	}

	sum := Reduce(slices.Values(got), 0, func(acc int, e Entry) int { return acc + e.Score })
	if sum != 825 {
		t.Fatalf("sum of top 3 = %d, want 825", sum)
	}

	// One comment line dropped plus three data lines yielded = four pulls. The
	// 100k lower rows and the poison "evil,1000000" row below them are never
	// scanned; a non-short-circuiting global top-N would have surfaced evil.
	if pulled != 4 {
		t.Fatalf("scanned %d lines, want exactly 4", pulled)
	}
}

func TestFilterAndParse(t *testing.T) {
	t.Parallel()

	buf := "# header\n\nalice,300\n# note\nbob,275\n"
	data := Filter(slices.Values(slices.Collect(strings.Lines(buf))), IsData)
	got := slices.Collect(Map(data, ParseEntry))
	want := []Entry{{"alice", 300}, {"bob", 275}}
	if !slices.Equal(got, want) {
		t.Fatalf("entries = %v, want %v", got, want)
	}
}
```

## Review

The pipeline is correct when `Take(3)` makes the line scan stop after the third record. The decisive evidence is `TestTopThreeShortCircuits`: a buffer of a hundred thousand rows plus a poison row is scanned exactly four lines deep, and the result is `alice, bob, carol` rather than `evil` — which can only happen if the stop from `Take` propagates through `Map` and `Filter` to `strings.Lines` before the lower rows are ever pulled. Confirm the score sum is `825` and that `IsData` removes the blank line and both `#` comments in the smaller test.

Common mistakes for this pattern. The first is claiming a short-circuiting top-N over *unsorted* input — it is wrong; without a sorted source you must scan every row and keep a running heap of the N best, and no `Take` can save you. The second is forgetting that `strings.Lines` leaves the trailing newline on each line, so a parser that does not trim it stores `"250\n"` and mis-parses the score; `ParseEntry` trims with `strings.TrimRight` first. The third is the familiar bare `yield(v)` with no stop check in a stage, which would keep scanning the whole buffer after `Take` is satisfied, throwing away the entire benefit. The fourth is collecting all lines into a slice before filtering, which reads the whole buffer up front and defeats the early stop the pipeline is built to provide.

## Resources

- [`strings.Lines`](https://pkg.go.dev/strings#Lines) — the lazy line source this pipeline reads, yielding one line at a time with its trailing newline.
- [`strings.Cut`](https://pkg.go.dev/strings#Cut) and [`strconv.Atoi`](https://pkg.go.dev/strconv#Atoi) — the field split and number parse inside `ParseEntry`.
- [`iter` package](https://pkg.go.dev/iter) and [Go Blog: Range Over Function Types](https://go.dev/blog/range-functions) — the `Seq` type and the pull/stop protocol that makes `Take` short-circuit the scan.
- [`slices.Collect`](https://pkg.go.dev/slices#Collect) and [`slices.Values`](https://pkg.go.dev/slices#Values) — the slice sink and source used by the demo and tests.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-lazy-etl-pipeline.md](04-lazy-etl-pipeline.md) | Next: [../07-iter-package-usage/00-concepts.md](../07-iter-package-usage/00-concepts.md)
