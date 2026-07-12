# Exercise 3: Text Iterators

Scanning text used to mean allocating a `[]string` of every line or field up
front. Go 1.24 added lazy twins of the splitting functions — `strings.Lines`,
`strings.SplitSeq`, `strings.FieldsSeq` — that yield one piece at a time. This
exercise builds a `textiter` package that counts lines, tallies words, and
extracts a CSV column from a block of text, composing those `strings` iterators
with a custom `NonEmptyLines` adapter and with `slices.Collect`.

This module is fully self-contained. It begins with its own `go mod init`,
defines every function it needs, and ships its own demo and tests. Nothing here
imports any other exercise.

## What you'll build

```text
textiter.go          NonEmptyLines, CountLines, WordCounts, Column
cmd/
  demo/
    main.go          count lines, print a sorted word tally, extract a column
textiter_test.go     line counting, newline trimming, word counts, column extraction
```

- Files: `textiter.go`, `cmd/demo/main.go`, `textiter_test.go`.
- Implement: the `NonEmptyLines` iterator adapter, `CountLines`, `WordCounts`, and `Column`.
- Test: `textiter_test.go` checks line counting including the no-trailing-newline case, newline trimming, word tallies, and column extraction with short rows.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/25-iterators-and-modern-go/08-standard-library-iterators/03-text-iterators/cmd/demo && cd go-solutions/25-iterators-and-modern-go/08-standard-library-iterators/03-text-iterators
```

### The newline detail, and composing text iterators

The one fact that governs this whole exercise is how `strings.Lines` yields:
each line keeps its trailing newline. Ranging over `"alpha\n\nbeta\n"` produces
`"alpha\n"`, then `"\n"`, then `"beta\n"` — three values, the middle one a bare
newline for the blank line. If the source does not end in a newline, the final
line is yielded without one. This is correct and deliberate (a consumer can
reconstruct the exact bytes), but it means any code that compares a line to a
newline-free string, or uses it as a map key, must trim first.

`NonEmptyLines` is the adapter that absorbs that detail once so nothing
downstream repeats it. It wraps `strings.Lines`, strips the trailing newline
with `strings.TrimRight(line, "\n")`, skips any line that is empty after
trimming, and yields the clean text. It is an `iter.Seq[string]` producer built
on another `iter.Seq[string]`, and it carries the `!yield(line)` early-stop
guard so a consumer that breaks halts the underlying line walk. Every other
function here ranges over `NonEmptyLines` rather than `strings.Lines` directly,
so none of them has to think about newlines or blank lines again.

`CountLines` is the one place that ranges over `strings.Lines` raw, because its
job is to count physical lines including blanks — it counts how many times the
iterator yields. `WordCounts` layers two producers: it walks `NonEmptyLines` to
get clean lines, then walks `strings.FieldsSeq` within each line to get
whitespace-delimited tokens, incrementing a tally per token. `FieldsSeq` splits
around runs of whitespace and never yields an empty or newline-bearing field, so
it is the right tool for tokenizing and needs no trimming. The resulting tally is
a map, so any code that displays it must impose an order; the demo does that with
`slices.Sorted(maps.Keys(counts))`, closing the loop back to exercise 1.

`Column` extracts one comma-separated field from each row. It ranges over
`NonEmptyLines`, and for each line it materializes the fields with
`slices.Collect(strings.SplitSeq(line, ","))` — a lazy producer consumed
immediately into a slice so the column index can be bounds-checked. A row with
fewer fields than the requested index contributes nothing rather than panicking,
which is the safe behavior for ragged input.

Create `textiter.go`:

```go
package textiter

import (
	"iter"
	"slices"
	"strings"
)

// NonEmptyLines yields each line of text with its trailing newline removed,
// skipping lines that are empty after trimming. It wraps strings.Lines, whose
// yielded lines always keep their terminating newline.
func NonEmptyLines(text string) iter.Seq[string] {
	return func(yield func(string) bool) {
		for line := range strings.Lines(text) {
			line = strings.TrimRight(line, "\n")
			if line == "" {
				continue
			}
			if !yield(line) {
				return
			}
		}
	}
}

// CountLines reports how many lines text contains, counting blank lines and a
// final line with no trailing newline. It ranges over strings.Lines directly.
func CountLines(text string) int {
	n := 0
	for range strings.Lines(text) {
		n++
	}
	return n
}

// WordCounts tallies whitespace-separated words across every non-empty line,
// using strings.FieldsSeq to tokenize each line.
func WordCounts(text string) map[string]int {
	counts := make(map[string]int)
	for line := range NonEmptyLines(text) {
		for word := range strings.FieldsSeq(line) {
			counts[word]++
		}
	}
	return counts
}

// Column extracts the field at index col from each non-empty comma-separated
// line, trimming surrounding spaces. Rows with too few fields are skipped.
func Column(text string, col int) []string {
	var out []string
	for line := range NonEmptyLines(text) {
		fields := slices.Collect(strings.SplitSeq(line, ","))
		if col < len(fields) {
			out = append(out, strings.TrimSpace(fields[col]))
		}
	}
	return out
}
```

### The runnable demo

The demo runs each function over a small document. The word tally is printed in
sorted key order so the output is deterministic despite living in a map.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"maps"
	"slices"

	"example.com/text-iterators"
)

func main() {
	text := "alpha beta gamma\n\nbeta gamma gamma\n"
	fmt.Println("lines:", textiter.CountLines(text))

	counts := textiter.WordCounts(text)
	for _, word := range slices.Sorted(maps.Keys(counts)) {
		fmt.Printf("%s=%d\n", word, counts[word])
	}

	csv := "id,name,score\n1,Alice,95\n2,Bob,80\n"
	fmt.Println("names:", textiter.Column(csv, 1))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
lines: 3
alpha=1
beta=2
gamma=3
names: [name Alice Bob]
```

### Tests

The tests pin the behaviors that the newline detail makes easy to get wrong.
`TestCountLines` includes a string with no trailing newline so the final
unterminated line is still counted. `TestNonEmptyLines` asserts the trailing
newline is gone and blank lines are dropped. `TestWordCounts` checks the
two-level tokenization. `TestColumn` extracts a column and includes a short row
that must be skipped rather than panic.

Create `textiter_test.go`:

```go
package textiter

import (
	"reflect"
	"slices"
	"testing"
)

func TestCountLines(t *testing.T) {
	t.Parallel()

	cases := []struct {
		text string
		want int
	}{
		{"", 0},
		{"a\n", 1},
		{"a\nb\n", 2},
		{"a\nb", 2},     // final line has no trailing newline
		{"a\n\nb\n", 3}, // blank line counts
	}
	for _, tc := range cases {
		if got := CountLines(tc.text); got != tc.want {
			t.Fatalf("CountLines(%q) = %d, want %d", tc.text, got, tc.want)
		}
	}
}

func TestNonEmptyLines(t *testing.T) {
	t.Parallel()

	got := slices.Collect(NonEmptyLines("alpha\n\n  \nbeta\ngamma"))
	want := []string{"alpha", "  ", "beta", "gamma"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("NonEmptyLines = %q, want %q", got, want)
	}
}

func TestWordCounts(t *testing.T) {
	t.Parallel()

	got := WordCounts("alpha beta gamma\n\nbeta gamma gamma\n")
	want := map[string]int{"alpha": 1, "beta": 2, "gamma": 3}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("WordCounts = %v, want %v", got, want)
	}
}

func TestColumn(t *testing.T) {
	t.Parallel()

	csv := "id,name,score\n1,Alice,95\n2,Bob,80\nshort\n"
	if got, want := Column(csv, 1), []string{"name", "Alice", "Bob"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Column(1) = %v, want %v", got, want)
	}
	if got, want := Column(csv, 0), []string{"id", "1", "2", "short"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Column(0) = %v, want %v", got, want)
	}
}
```

## Review

The package is correct when the newline detail is handled in exactly one place.
`NonEmptyLines` trims and filters once, and every higher-level function ranges
over it instead of over `strings.Lines`, so none of them carries stray `\n`
bytes into a comparison or a map key. `CountLines` is the deliberate exception
because it must count physical lines, blanks included, so it ranges over the raw
`strings.Lines`. Confirm the no-trailing-newline rows in `TestCountLines` and
`TestColumn` — they prove the final unterminated line is still seen, which is the
edge a naive split-on-`\n` loop tends to mishandle.

Note in `TestNonEmptyLines` that the line `"  "` survives: `NonEmptyLines` only
drops lines that are empty after trimming the newline, and two spaces are not
empty. Tokenizing with `strings.FieldsSeq` is what discards all-whitespace
content at the word level, which is why `WordCounts` over that same input would
add no tokens for a spaces-only line. The two functions filter at different
granularities on purpose, and mixing them up — expecting `NonEmptyLines` to drop
whitespace-only lines, or `FieldsSeq` to preserve them — is the subtle bug to
watch for. The output map in `WordCounts` is unordered like every map, so the
demo sorts its keys before printing; asserting on its iteration order directly
would flake.

## Resources

- [`strings` package](https://pkg.go.dev/strings) — `Lines`, `SplitSeq`, and `FieldsSeq`, the lazy text iterators used here, plus their slice-returning twins.
- [`slices` package](https://pkg.go.dev/slices) — `Collect`, used to materialize a `strings.SplitSeq` into an indexable slice.
- [Go 1.24 Release Notes](https://go.dev/doc/go1.24) — the release that added the `strings` and `bytes` iterator functions.
- [Go Blog: Range Over Function Types](https://go.dev/blog/range-functions) — the iterator model that the `strings` sequence functions are built on.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-slice-iterator-pipelines.md](02-slice-iterator-pipelines.md) | Next: [04-deterministic-report-export.md](04-deterministic-report-export.md)
