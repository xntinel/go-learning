# Exercise 5: Log Metrics Ingest and Top-N Report

The everyday shape of an observability task is: read a block of log text, pull a
field out of each line, tally how often each distinct value appears, then print
the busiest few. This exercise builds an `ingest` package that does exactly that
over `key=value` log lines, walking the text with `strings.Lines`, tokenizing
each line with `strings.FieldsSeq`, splitting each token with `strings.SplitSeq`,
and emitting a deterministic top-N report by feeding the tally map through
`slices.SortedFunc`. The tests cover both the tally and the ordering, including
the tie-break that makes the ranking reproducible.

This module is fully self-contained. It begins with its own `go mod init`,
defines every function it needs, and ships its own demo and tests. Nothing here
imports any other exercise.

## What you'll build

```text
ingest.go            Count, fieldValue, Tally, TopN, Report
cmd/
  demo/
    main.go          tally a log by service and by level, print top-N reports
ingest_test.go       tally correctness, missing-key handling, top-N ordering and ties
```

- Files: `ingest.go`, `cmd/demo/main.go`, `ingest_test.go`.
- Implement: the `Count` type, the `fieldValue` extractor, `Tally`, `TopN`, and `Report`.
- Test: `ingest_test.go` checks the tally counts, that a missing key yields an empty tally, that `TopN` orders by count then key, that the order is stable across runs, that it clamps to the available entries, and the rendered report.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/25-iterators-and-modern-go/08-standard-library-iterators/05-log-metrics-ingest/cmd/demo && cd go-solutions/25-iterators-and-modern-go/08-standard-library-iterators/05-log-metrics-ingest
```

### Three text iterators, one parse

The input is a multi-line string where each line is a run of whitespace-separated
`key=value` tokens, for example `ts=2 level=error service=auth`. Parsing it
exercises all three `strings` iterators in their natural roles, each chosen for a
property of the level it works at.

`strings.Lines` walks the whole block one line at a time. It is the right tool
for line-oriented input because it yields lazily — a streaming ingest can process
and discard each line without ever holding the whole split in memory — and
because it correctly handles the final line whether or not the source ends in a
newline. The one detail to remember is that each yielded line keeps its trailing
newline, but `fieldValue` never compares the line as a whole, so the newline
simply rides along inside the whitespace that `FieldsSeq` discards.

`strings.FieldsSeq` tokenizes one line into its whitespace-delimited fields. It
is the right tool here because it splits around runs of whitespace and yields
neither empty tokens nor newline-bearing ones, so a line with irregular spacing,
or the trailing newline from `Lines`, produces clean `key=value` tokens with no
special-casing. `strings.SplitSeq` then splits a single token on `=`. Splitting
on a fixed separator is exactly `SplitSeq`'s job, and collecting its two halves
with `slices.Collect` lets the code bounds-check that the token really had the
shape `key=value` — a token like `malformed` with no `=` collects to a
one-element slice and is skipped rather than misread.

`Tally` composes these into the count: for each line, ask `fieldValue` for the
requested key's value, and if the line carries that key, increment its tally.
Lines that lack the key contribute nothing, so partial or malformed lines never
panic and never inflate a count. The result is a `map[string]int`, which means —
as always — its iteration order is randomized and any ordered output must impose
order explicitly.

### Making the ranking deterministic

`TopN` is where the randomized map becomes a stable ranking. It feeds the tally
through `maps.All`, which yields each `(key, count)` pair, wrapping them into
`Count` values, and hands that sequence to `slices.SortedFunc`. The comparison is
the crux: it orders by count descending, and — this is the part that makes the
output reproducible — breaks ties by key ascending. `cmp.Or` expresses that
cleanly by returning the first non-zero comparison, so `cmp.Or(cmp.Compare(b.Count,
a.Count), cmp.Compare(a.Key, b.Key))` reads as "by count, high to low, then by
key, A to Z." Because the comparison is a total order with no ties left
unresolved, `slices.SortedFunc` produces the same slice on every run regardless
of the order `maps.All` happened to yield in. Without the key tie-break, two keys
with equal counts could swap places between runs, and a top-N report would flake
exactly the way a raw map walk does.

After sorting, `TopN` clamps `n` to the number of entries so that asking for more
than exist returns everything rather than slicing out of bounds. `Report` then
renders the top entries as a numbered table, joining the rows with newlines into
a single deterministic string ready to log, diff, or assert on.

Create `ingest.go`:

```go
package ingest

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Count pairs a key with the number of times it was observed.
type Count struct {
	Key   string
	Count int
}

// fieldValue scans the key=value tokens of a single log line and returns the
// value attached to key. It tokenizes with strings.FieldsSeq (whitespace-
// delimited, no empty or newline-bearing tokens) and splits each token on '='
// with strings.SplitSeq, collected so the two halves can be length-checked.
func fieldValue(line, key string) (string, bool) {
	for token := range strings.FieldsSeq(line) {
		parts := slices.Collect(strings.SplitSeq(token, "="))
		if len(parts) == 2 && parts[0] == key {
			return parts[1], true
		}
	}
	return "", false
}

// Tally walks the log one line at a time with strings.Lines and counts how many
// lines carry each distinct value of key. Lines without the key are ignored, so
// blank or malformed lines never panic and never inflate a count.
func Tally(log, key string) map[string]int {
	counts := make(map[string]int)
	for line := range strings.Lines(log) {
		if v, ok := fieldValue(line, key); ok {
			counts[v]++
		}
	}
	return counts
}

// TopN returns the n highest counts, ties broken by ascending key. It feeds the
// map through maps.All into slices.SortedFunc, whose comparison imposes a total
// order (count descending, then key ascending), so the result is fully
// deterministic despite the map's randomized iteration order.
func TopN(counts map[string]int, n int) []Count {
	entries := slices.SortedFunc(
		func(yield func(Count) bool) {
			for k, c := range maps.All(counts) {
				if !yield(Count{Key: k, Count: c}) {
					return
				}
			}
		},
		func(a, b Count) int {
			return cmp.Or(cmp.Compare(b.Count, a.Count), cmp.Compare(a.Key, b.Key))
		},
	)
	if n > len(entries) {
		n = len(entries)
	}
	return entries[:n]
}

// Report tallies the log by key and renders the top n values as a deterministic,
// newline-joined table of "rank. key count" rows.
func Report(log, key string, n int) string {
	top := TopN(Tally(log, key), n)
	rows := make([]string, 0, len(top))
	for i, e := range top {
		rows = append(rows, fmt.Sprintf("%d. %s %d", i+1, e.Key, e.Count))
	}
	return strings.Join(rows, "\n")
}
```

### The runnable demo

The demo ingests a small log twice — once tallying by service, once by level —
and prints a top-N report for each. The blank line and the final line without a
trailing newline are both handled without special-casing.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/log-metrics-ingest"
)

func main() {
	log := "ts=1 level=info service=auth\n" +
		"ts=2 level=error service=auth\n" +
		"ts=3 level=info service=catalog\n" +
		"ts=4 level=info service=auth\n" +
		"ts=5 level=warn service=billing\n" +
		"ts=6 level=info service=catalog\n" +
		"\n" +
		"ts=7 level=info service=auth"

	fmt.Println("top services:")
	fmt.Println(ingest.Report(log, "service", 2))

	fmt.Println("by level:")
	fmt.Println(ingest.Report(log, "level", 3))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
top services:
1. auth 4
2. catalog 2
by level:
1. info 5
2. error 1
3. warn 1
```

### Tests

The tests cover both halves of the task. `TestTally` checks the counts over a
sample that includes a blank line and a final line with no trailing newline.
`TestTallyIgnoresMissingKey` proves a key absent from every line yields an empty
tally rather than a panic. `TestTopNOrdersByCountThenKey` pins the ordering and,
critically, the `error` before `warn` tie-break at count one.
`TestTopNStableAcrossRuns` ranks the same tally a thousand times and asserts the
result never drifts. `TestTopNClampsToAvailable` checks the over-large `n`, and
`TestReport` pins the rendered bytes.

Create `ingest_test.go`:

```go
package ingest

import (
	"reflect"
	"testing"
)

const sample = "ts=1 level=info service=auth\n" +
	"ts=2 level=error service=auth\n" +
	"ts=3 level=info service=catalog\n" +
	"ts=4 level=info service=auth\n" +
	"ts=5 level=warn service=billing\n" +
	"ts=6 level=info service=catalog\n" +
	"\n" +
	"ts=7 level=info service=auth"

func TestTally(t *testing.T) {
	t.Parallel()

	got := Tally(sample, "service")
	want := map[string]int{"auth": 4, "catalog": 2, "billing": 1}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Tally(service) = %v, want %v", got, want)
	}
}

func TestTallyIgnoresMissingKey(t *testing.T) {
	t.Parallel()

	if got := Tally(sample, "region"); len(got) != 0 {
		t.Fatalf("Tally(region) = %v, want empty", got)
	}
}

func TestTopNOrdersByCountThenKey(t *testing.T) {
	t.Parallel()

	got := TopN(Tally(sample, "level"), 3)
	want := []Count{{"info", 5}, {"error", 1}, {"warn", 1}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("TopN(level, 3) = %v, want %v", got, want)
	}
}

func TestTopNStableAcrossRuns(t *testing.T) {
	t.Parallel()

	counts := Tally(sample, "service")
	first := TopN(counts, 3)
	for i := 0; i < 1000; i++ {
		if got := TopN(counts, 3); !reflect.DeepEqual(got, first) {
			t.Fatalf("run %d: TopN drifted: %v vs %v", i, got, first)
		}
	}
}

func TestTopNClampsToAvailable(t *testing.T) {
	t.Parallel()

	if got := TopN(Tally(sample, "service"), 10); len(got) != 3 {
		t.Fatalf("TopN(service, 10) returned %d entries, want 3", len(got))
	}
}

func TestReport(t *testing.T) {
	t.Parallel()

	want := "1. auth 4\n2. catalog 2"
	if got := Report(sample, "service", 2); got != want {
		t.Fatalf("Report =\n%q\nwant\n%q", got, want)
	}
}
```

## Review

The package is correct when the parse is forgiving and the ranking is total.
`fieldValue` length-checks the `SplitSeq` result, so a token without an `=`, or
one with more than one, is skipped rather than misread — that is what lets the
blank line and any malformed token pass through `Tally` harmlessly. Confirm the
sample's final line has no trailing newline yet `auth` still counts four: that
proves `strings.Lines` yields the last unterminated line, the edge a naive
split-on-`\n` loop tends to drop.

The assertion that matters most is the tie-break. At count one, `error` and
`warn` are equal on the primary key, and only the secondary `cmp.Compare(a.Key,
b.Key)` decides their order; remove it and `slices.SortedFunc` would leave the
two in whatever order `maps.All` yielded, which the runtime randomizes, and
`TestTopNStableAcrossRuns` would flake. `cmp.Or` is what keeps the comparison
readable while still resolving every tie: it returns the first non-zero result,
so the count comparison wins when counts differ and the key comparison settles
the rest. The thousand-run stability test is the proof that the order is total,
not merely usually-correct.

## Resources

- [`strings` package](https://pkg.go.dev/strings) — `Lines`, `FieldsSeq`, and `SplitSeq`, the three lazy text iterators that parse each log line.
- [`slices` package](https://pkg.go.dev/slices) — `Collect` and `SortedFunc`, used to materialize a split token and to rank the tally with a total order.
- [`maps` package](https://pkg.go.dev/maps) — `All`, the producer that turns the tally map into a `(key, count)` sequence.
- [`cmp` package](https://pkg.go.dev/cmp) — `Compare` and `Or`, which compose the count-then-key tie-break into one comparison.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-deterministic-report-export.md](04-deterministic-report-export.md) | Next: [../../26-memory-model-and-optimization/01-happens-before-relationships/01-happens-before-relationships.md](../../26-memory-model-and-optimization/01-happens-before-relationships/01-happens-before-relationships.md)
