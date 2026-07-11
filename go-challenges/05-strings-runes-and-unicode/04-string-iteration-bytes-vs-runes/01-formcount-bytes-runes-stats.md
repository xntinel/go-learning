# Exercise 1: A Form-Field Counter: Bytes, Runes, and One-Pass Stats

Every text field that reaches your backend carries two lengths at once — the bytes
it costs to store and the characters a user thinks they typed. This module builds
the small package that reports both, plus a single-pass statistic that most text
handlers eventually need, so the byte-vs-rune distinction is concrete before the
harder boundaries that follow.

## What you'll build

```text
formcount/                 independent module: example.com/formcount
  go.mod                   go 1.26
  formcount.go             Bytes, Runes, Stats, RuneStat
  cmd/
    demo/
      main.go              prints byte/rune split for a mixed string
  formcount_test.go        table tests + empty-string + range-equivalence
```

Files: `formcount.go`, `cmd/demo/main.go`, `formcount_test.go`.
Implement: `Bytes(s) int`, `Runes(s) int`, `Stats(s) RuneStat`.
Test: byte/rune split over ASCII, Latin, CJK, empty; one-pass stats; `Runes` equals range-iteration count.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/formcount/cmd/demo
cd ~/go-exercises/formcount
go mod init example.com/formcount
```

### Why two counters and one pass

`Bytes(s)` is `len(s)` — a single field read, O(1), the number your storage layer
and wire limits care about. `Runes(s)` is `utf8.RuneCountInString(s)` — O(n) but
allocation-free, the number a user means by "characters". Exposing both under names
that say which unit they measure is the whole point: a reader of the call site can
see, at a glance, whether a limit is a storage limit or a character limit.

`Stats(s)` earns its keep by doing in one range pass what three separate calls would
do in three: it counts runes, sums their encoded byte widths with `utf8.RuneLen`,
and tracks the largest and smallest rune seen. A single pass matters once the string
is large or the call is hot; more importantly, it forces the correct initialization
question. `MaxRune`/`MinRune` are set to `-1` up front so the empty string reports a
sentinel rather than a misleading `0` (which is a real rune, NUL). The `Count == 1`
guard seeds both extremes from the first actual rune, so a one-rune string reports
that rune as both max and min. Summing `utf8.RuneLen(r)` across the pass reproduces
`len(s)` exactly for valid UTF-8, which is a cheap internal consistency check: if
`Stats(s).Bytes != len(s)`, the input was not valid UTF-8.

Create `formcount.go`:

```go
// formcount.go
package formcount

import "unicode/utf8"

// Bytes reports the storage/wire length of s in bytes. It is O(1).
func Bytes(s string) int {
	return len(s)
}

// Runes reports the number of Unicode code points in s — the usual meaning of
// a "character" count for a form field. It is O(n) but allocation-free.
func Runes(s string) int {
	return utf8.RuneCountInString(s)
}

// RuneStat is the result of a single-pass scan of a string. For the empty
// string, Count and Bytes are 0 and MaxRune/MinRune are the -1 sentinel.
type RuneStat struct {
	Count   int  // number of runes (code points)
	Bytes   int  // total encoded byte width; equals len(s) for valid UTF-8
	MaxRune rune // largest code point seen, or -1 if s is empty
	MinRune rune // smallest code point seen, or -1 if s is empty
}

// Stats scans s once, reporting rune count, byte count, and the extreme runes.
func Stats(s string) RuneStat {
	st := RuneStat{MaxRune: -1, MinRune: -1}
	for _, r := range s {
		st.Count++
		st.Bytes += utf8.RuneLen(r)
		if st.Count == 1 || r > st.MaxRune {
			st.MaxRune = r
		}
		if st.Count == 1 || r < st.MinRune {
			st.MinRune = r
		}
	}
	return st
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/formcount"
)

func main() {
	for _, s := range []string{"hello", "café", "中文"} {
		st := formcount.Stats(s)
		fmt.Printf("%-7q bytes=%d runes=%d max=%c\n",
			s, formcount.Bytes(s), formcount.Runes(s), st.MaxRune)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
"hello" bytes=5 runes=5 max=o
"café"  bytes=5 runes=4 max=é
"中文"    bytes=6 runes=2 max=文
```

### Tests

The tests pin the two facts that make this lesson: the byte/rune split for real
Unicode input, and the equivalence between `Runes` and counting range iterations —
proving `utf8.RuneCountInString` is exactly "how many times does `for range` step".

Create `formcount_test.go`:

```go
// formcount_test.go
package formcount

import (
	"fmt"
	"testing"
)

func TestBytes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"hello", 5},
		{"café", 5},
		{"中文", 6},
	}
	for _, tc := range cases {
		if got := Bytes(tc.in); got != tc.want {
			t.Errorf("Bytes(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestRunes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"hello", 5},
		{"café", 4},
		{"中文", 2},
	}
	for _, tc := range cases {
		if got := Runes(tc.in); got != tc.want {
			t.Errorf("Runes(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestStatsReportsByteAndRuneCounts(t *testing.T) {
	t.Parallel()
	st := Stats("café 中文")
	if st.Count != 7 {
		t.Errorf("Count = %d, want 7 (c a f é space 中 文)", st.Count)
	}
	if st.Bytes != 12 {
		t.Errorf("Bytes = %d, want 12 (5 + 1 + 6)", st.Bytes)
	}
	if st.MaxRune != '文' {
		t.Errorf("MaxRune = %U, want U+6587", st.MaxRune)
	}
	if st.MinRune != ' ' {
		t.Errorf("MinRune = %U, want U+0020 (space)", st.MinRune)
	}
}

func TestStatsOnEmptyString(t *testing.T) {
	t.Parallel()
	st := Stats("")
	if st.Count != 0 || st.Bytes != 0 {
		t.Errorf("Stats(\"\") = %+v, want zero counts", st)
	}
	if st.MaxRune != -1 || st.MinRune != -1 {
		t.Errorf("MaxRune/MinRune = %d/%d, want both -1", st.MaxRune, st.MinRune)
	}
}

func TestRunesMatchesRangeCount(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"hello", "café", "中文", ""} {
		iters := 0
		for range s {
			iters++
		}
		if got := Runes(s); got != iters {
			t.Errorf("Runes(%q) = %d, range iterations = %d", s, got, iters)
		}
	}
}

func ExampleStats() {
	st := Stats("café")
	fmt.Println(st.Count, st.Bytes, string(st.MaxRune))
	// Output: 4 5 é
}
```

## Review

The package is correct when each name reports its own unit and `Stats` needs only
one pass. `Bytes` must be `len` (never a rune count), `Runes` must equal the number
of `for range` steps — which `TestRunesMatchesRangeCount` proves directly — and
`Stats` must seed `MaxRune`/`MinRune` to `-1` so the empty string is distinguishable
from a string containing NUL. The internal check that `Stats(s).Bytes == len(s)`
holds only for valid UTF-8 is worth remembering; later modules validate that
precondition rather than assume it. The classic mistake this exercise inoculates
against is using `len` where a character count is meant — correct for ASCII, quietly
wrong for everything else.

## Resources

- [unicode/utf8: RuneCountInString, RuneLen](https://pkg.go.dev/unicode/utf8) — the exact O(n) rune count and per-rune byte width.
- [Go Blog: Strings, bytes, runes and characters in Go](https://go.dev/blog/strings) — why `len` is bytes and `range` decodes.
- [Go Specification: For range](https://go.dev/ref/spec#For_range) — the rule that the range index is a byte offset.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-rune-safe-truncate-to-byte-budget.md](02-rune-safe-truncate-to-byte-budget.md)
