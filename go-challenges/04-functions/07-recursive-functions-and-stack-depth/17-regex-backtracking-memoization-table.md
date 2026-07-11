# Exercise 17: Regex Matcher with Exponential-Backtracking Memo Cache

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A recursive regex matcher for even a tiny pattern language — a literal
character, `.` for any character, `*` for zero-or-more of the preceding
character — can take exponential time on some inputs, because `x*` branches
into "consume one more `x`" and "stop here," and those branches overlap: many
different sequences of choices lead to checking the exact same remaining
`(string position, pattern position)` pair. An attacker who controls the
pattern (a user-supplied search filter, a validation rule) can pick one that
makes a plain recursive matcher spend seconds or minutes on a short string —
this is the well-known ReDoS (regular expression denial of service) class of
bug. A memoization table over `(i, j)` pairs turns the same recursion from
exponential into polynomial, because it ensures every distinct subproblem is
solved once.

This module is fully self-contained: its own `go mod init`, the matcher
inline, its own demo and tests.

## What you'll build

```text
miniregex/                   independent module: example.com/miniregex
  go.mod                      go 1.24
  miniregex.go                 func Match(s, pattern string) bool
  miniregex_test.go             literal, dot, star, whole-string coverage, adversarial timing
  cmd/
    demo/
      main.go                   matches an adversarial "a*a*a*...c" pattern, times it
```

- Files: `miniregex.go`, `cmd/demo/main.go`, `miniregex_test.go`.
- Implement: `func Match(s, pattern string) bool` supporting `.` and `*`,
  matching the whole string (like `^pattern$`), backed by a recursive
  `match(s, pattern, i, j, memo, done map[[2]int]bool) bool` memoized on
  `(i, j)`.
- Test: literal matching, `.` matching any one character, a table of `*`
  cases (zero occurrences, many occurrences, `.*` matching anything), the
  whole-string-coverage requirement in both directions, and an adversarial
  pattern that completes quickly instead of blowing up.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/miniregex/cmd/demo
cd ~/go-exercises/miniregex
go mod init example.com/miniregex
go mod edit -go=1.24
```

### Why the same (i, j) pair gets visited exponentially many times

Without memoization, `match(s, pattern, i, j)` recurses into two branches
whenever the pattern has a `*` group at `j`: skip the group (recurse to
`j+2`), or consume one character of `s` and stay on the same group (recurse
to `i+1, j`). With several `*` groups in a row, there are many different
sequences of "consume here, skip there" decisions that all end up asking
the identical question "does `pattern[j:]` match `s[i:]`?" for the same
`i, j` — the branches are different paths through the decision tree that
converge on the same subproblem. The number of such paths grows
combinatorially with the string length and the number of `*` groups, which
is exactly why a pattern like `a*a*a*a*a*a*a*a*a*a*a*a*c` against a string
of plain `a`s (with no trailing `c`) can take the naive version seconds or
minutes on inputs only a few dozen characters long: it is re-deriving the
same "no, this doesn't match" answer for the same position pair over and
over, once for every path that happens to arrive there.

The fix does not change what gets computed, only how many times: memoize
`match` on `(i, j)`. The first time a given position pair is asked about,
solve it recursively and record the answer; every subsequent path that
arrives at the same pair gets the cached answer immediately. Since there are
only `len(s)+1` possible values of `i` and `len(pattern)+1` possible values
of `j`, the total number of distinct subproblems is bounded by their
product — polynomial — regardless of how many overlapping paths the
unmemoized recursion would have explored to reach them.

Create `miniregex.go`:

```go
// Package miniregex implements recursive matching for a tiny pattern
// language ('.' for any character, '*' for zero-or-more of the preceding
// character) with a memoization table that turns what is naturally an
// exponential-time backtracking search into a polynomial-time one.
package miniregex

// Match reports whether pattern matches the entirety of s. Supported syntax:
// a literal character matches itself, '.' matches any single character, and
// 'x*' (a character, possibly '.', followed by '*') matches zero or more
// occurrences of x. The match must cover the whole string, as with
// regexp.MustCompile("^" + pattern + "$").
func Match(s, pattern string) bool {
	memo := make(map[[2]int]bool)
	done := make(map[[2]int]bool)
	return match(s, pattern, 0, 0, memo, done)
}

// match reports whether pattern[j:] matches s[i:] in full. memo/done cache
// results keyed by (i, j) so that the same suffix pair is never
// re-explored: without this cache, a pattern like "a*a*a*a*a*a*a*a*c"
// against a non-matching string of a's re-derives the same subproblems an
// exponential number of times, because each '*' can consume any number of
// characters and the branches overlap heavily in (i, j) space.
func match(s, pattern string, i, j int, memo, done map[[2]int]bool) bool {
	key := [2]int{i, j}
	if done[key] {
		return memo[key]
	}

	result := matchUncached(s, pattern, i, j, memo, done)
	done[key] = true
	memo[key] = result
	return result
}

func matchUncached(s, pattern string, i, j int, memo, done map[[2]int]bool) bool {
	if j == len(pattern) {
		return i == len(s)
	}

	firstMatch := i < len(s) && (pattern[j] == s[i] || pattern[j] == '.')

	if j+1 < len(pattern) && pattern[j+1] == '*' {
		// Zero occurrences: skip "x*" entirely.
		if match(s, pattern, i, j+2, memo, done) {
			return true
		}
		// One or more occurrences: consume one matching character of s and
		// stay on the same "x*", trying to consume another.
		if firstMatch && match(s, pattern, i+1, j, memo, done) {
			return true
		}
		return false
	}

	if !firstMatch {
		return false
	}
	return match(s, pattern, i+1, j+1, memo, done)
}
```

### The runnable demo

The demo matches a classic catastrophic-backtracking shape — twelve
overlapping `a*` groups followed by a `c` that the string never has —
against a 25-character string of `a`s, and confirms it completes well under
a second instead of hanging.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"strings"
	"time"

	"example.com/miniregex"
)

func main() {
	// A classic catastrophic-backtracking shape: many overlapping "a*"
	// groups followed by a character that never appears. Unmemoized
	// backtracking on this input explores an exponential number of ways to
	// split the a's among the groups before giving up.
	s := strings.Repeat("a", 25)
	pattern := strings.Repeat("a*", 12) + "c"

	start := time.Now()
	matched := miniregex.Match(s, pattern)
	elapsed := time.Since(start)

	fmt.Printf("matched: %v\n", matched)
	fmt.Printf("completed in under a second: %v\n", elapsed < time.Second)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
matched: false
completed in under a second: true
```

### Tests

`TestMatchLiteral` and `TestMatchDot` cover the base syntax. `TestMatchStar`
is a table covering zero occurrences, many occurrences, and `.*` matching
anything. `TestMatchMustCoverWholeString` checks both directions of the
whole-string requirement: a pattern shorter than the string must fail just
as much as a string shorter than the pattern. `TestMatchAdversarialPatternCompletesQuickly`
is the point of the exercise — it does not just check the (false) result,
it checks the matcher does not take more than a couple of seconds on an
input shaped to defeat unmemoized backtracking.

Create `miniregex_test.go`:

```go
package miniregex

import (
	"strings"
	"testing"
	"time"
)

func TestMatchLiteral(t *testing.T) {
	t.Parallel()
	if !Match("abc", "abc") {
		t.Fatal("expected exact literal match")
	}
	if Match("abc", "abd") {
		t.Fatal("expected literal mismatch to fail")
	}
}

func TestMatchDot(t *testing.T) {
	t.Parallel()
	if !Match("abc", "a.c") {
		t.Fatal("'.' should match any single character")
	}
	if Match("ac", "a.c") {
		t.Fatal("'.' should not match zero characters")
	}
}

func TestMatchStar(t *testing.T) {
	t.Parallel()

	tests := []struct {
		s, pattern string
		want       bool
	}{
		{"", "a*", true},
		{"aaaa", "a*", true},
		{"aab", "a*b", true},
		{"b", "a*b", true},
		{"", "a*b", false},
		{"aaa", "a*a", true},
		{"", ".*", true},
		{"anything", ".*", true},
	}
	for _, tc := range tests {
		if got := Match(tc.s, tc.pattern); got != tc.want {
			t.Errorf("Match(%q, %q) = %v, want %v", tc.s, tc.pattern, got, tc.want)
		}
	}
}

func TestMatchMustCoverWholeString(t *testing.T) {
	t.Parallel()
	if Match("abcd", "abc") {
		t.Fatal("a pattern shorter than the string must not match")
	}
	if Match("ab", "abc") {
		t.Fatal("a string shorter than the pattern must not match")
	}
}

func TestMatchAdversarialPatternCompletesQuickly(t *testing.T) {
	t.Parallel()

	s := strings.Repeat("a", 28)
	pattern := strings.Repeat("a*", 14) + "c"

	start := time.Now()
	got := Match(s, pattern)
	elapsed := time.Since(start)

	if got {
		t.Fatal("pattern requires a trailing 'c' the string never has")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Match took %v, want well under a second with memoization", elapsed)
	}
}
```

Run it: `go test -count=1 ./...`

## Review

`Match` is correct when it agrees with the naive recursive definition on
every case — literal, `.`, and `*` in all their combinations — while never
re-solving the same `(i, j)` subproblem twice. `TestMatchStar`'s table
covers the branch logic (skip the group, or consume and repeat), and
`TestMatchMustCoverWholeString` guards the base case in both directions,
since it is easy to accidentally accept a prefix match instead of a full
match. `TestMatchAdversarialPatternCompletesQuickly` is the test that would
fail (by timing out or taking far too long) on the version of this exercise
that forgets the memo table entirely — that omission is the mistake this
exercise targets: correct logic, but with no protection against an input
designed to make the same correct logic run exponentially many times.

## Resources

- [regexp/syntax package (for a production-grade alternative)](https://pkg.go.dev/regexp/syntax)
- [OWASP: Regular expression Denial of Service (ReDoS)](https://owasp.org/www-community/attacks/Regular_expression_Denial_of_Service_-_ReDoS)
- [Go Specification: Map types](https://go.dev/ref/spec#Map_types)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [16-xml-element-streaming-depth-validation.md](16-xml-element-streaming-depth-validation.md) | Next: [18-graph-shortest-path-explicit-stack.md](18-graph-shortest-path-explicit-stack.md)
