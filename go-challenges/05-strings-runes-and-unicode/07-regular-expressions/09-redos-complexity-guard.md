# Exercise 9: Safely Accepting User-Supplied Search Patterns

An API that lets a user supply a search pattern hands an outsider the regex
engine. On a backtracking engine that is a ReDoS invitation; on Go's RE2 it is
safe *in match time*, but the pattern and input can still abuse memory and CPU by
sheer size. This module builds the guard: it validates a user pattern under a
length and complexity budget by parsing it with `regexp/syntax` before compiling,
compiles with `Compile`, and caps the input size before matching — the honest way
to accept an untrusted pattern.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
patternguard/               independent module: example.com/patternguard
  go.mod                    go 1.26
  patternguard.go           type Guard; Compile (syntax.Parse + budget); Match (input cap); errors
  cmd/
    demo/
      main.go               runnable demo: accept a pattern, match under a cap
  patternguard_test.go       linear-time proof, budget rejects, invalid syntax, input cap
```

- Files: `patternguard.go`, `cmd/demo/main.go`, `patternguard_test.go`.
- Implement: `Guard.Compile(pattern string) (*regexp.Regexp, error)` enforcing length and nesting budgets via `regexp/syntax.Parse` then `regexp.Compile`; `Guard.Match(re *regexp.Regexp, input string) (bool, error)` capping input size.
- Test: a pattern that is catastrophic on a backtracking engine (`(a+)+$`) still matches in linear time (timed to prove no blowup); an over-length pattern and an over-length input are rejected; invalid syntax returns a clear error; a benchmark shows match time scales with input length, not pattern nesting.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/05-strings-runes-and-unicode/07-regular-expressions/09-redos-complexity-guard/cmd/demo
cd go-solutions/05-strings-runes-and-unicode/07-regular-expressions/09-redos-complexity-guard
```

### What RE2 gives you, and what it does not

The pattern `(a+)+$` matched against a long run of `a` ending in a non-`a` is the
textbook ReDoS: a backtracking engine tries exponentially many ways to split the
`a`s and hangs for seconds on a few dozen characters. Go's RE2-derived engine
cannot do this — it simulates an NFA in a single pass, so the same match is linear
in the input length. The first test *proves* this by timing: it matches `(a+)+$`
against a thousand `a`s plus a `!` and asserts the call returns in well under a
second. That is the guarantee that makes it safe to compile a user's pattern at
all.

What RE2 does *not* give you is a bound on the pattern's size or nesting, or a
per-match timeout. A user who sends a 10 MB pattern, or a deeply nested one, makes
compilation itself expensive; a user who sends a modest pattern with a 500 MB input
still costs CPU linear in that 500 MB. So the guard adds the controls the engine
omits: `regexp/syntax.Parse` inspects the pattern's tree (walking `.Sub` for
nesting depth) and a length check rejects an over-large pattern *before* compiling;
`Compile` (never `MustCompile`) turns a bad pattern into a returned error, not a
panic; and `Match` rejects an over-large input before running. The honest framing
for a code review: RE2 removes catastrophic backtracking, but bounding resources is
still the caller's job — there is no per-match timeout, so length caps and
`context` cancellation at the call site are the real controls.

Create `patternguard.go`:

```go
package patternguard

import (
	"errors"
	"fmt"
	"regexp"
	"regexp/syntax"
)

// Sentinel errors let the API layer map failures to a 400 with a clear reason.
var (
	ErrPatternTooLong = errors.New("pattern too long")
	ErrTooComplex     = errors.New("pattern too complex")
	ErrInputTooLong   = errors.New("input too long")
	ErrBadSyntax      = errors.New("invalid pattern syntax")
)

// Guard bounds an untrusted pattern and the input it runs against.
type Guard struct {
	MaxPatternLen int
	MaxNestDepth  int
	MaxInputLen   int
}

// Default returns a Guard with sensible caps for a search API.
func Default() Guard {
	return Guard{MaxPatternLen: 1024, MaxNestDepth: 20, MaxInputLen: 1 << 20}
}

// Compile validates pattern under the guard's budget and compiles it. It parses
// with regexp/syntax first so an over-complex pattern is rejected before the
// (more expensive) automaton is built.
func (g Guard) Compile(pattern string) (*regexp.Regexp, error) {
	if len(pattern) > g.MaxPatternLen {
		return nil, fmt.Errorf("%w: %d > %d", ErrPatternTooLong, len(pattern), g.MaxPatternLen)
	}
	tree, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadSyntax, err)
	}
	if d := depth(tree); d > g.MaxNestDepth {
		return nil, fmt.Errorf("%w: nesting %d > %d", ErrTooComplex, d, g.MaxNestDepth)
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadSyntax, err)
	}
	return re, nil
}

// Match runs re against input after capping the input size. RE2 bounds match time
// in input length, but there is no per-match timeout, so the size cap is the
// control that keeps a huge input from burning CPU.
func (g Guard) Match(re *regexp.Regexp, input string) (bool, error) {
	if len(input) > g.MaxInputLen {
		return false, fmt.Errorf("%w: %d > %d", ErrInputTooLong, len(input), g.MaxInputLen)
	}
	return re.MatchString(input), nil
}

// depth is the maximum nesting depth of the parsed pattern tree.
func depth(re *syntax.Regexp) int {
	max := 0
	for _, sub := range re.Sub {
		if d := depth(sub); d > max {
			max = d
		}
	}
	return max + 1
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"strings"

	"example.com/patternguard"
)

func main() {
	g := patternguard.Default()

	// The classic ReDoS pattern compiles and matches in linear time under RE2.
	re, err := g.Compile(`(a+)+$`)
	if err != nil {
		panic(err)
	}
	ok, err := g.Match(re, strings.Repeat("a", 1000)+"!")
	fmt.Printf("catastrophic-pattern match=%v err=%v\n", ok, err)

	// An over-long input is rejected before matching.
	_, err = g.Match(re, strings.Repeat("a", 2<<20))
	fmt.Printf("oversized input rejected: %v\n", err != nil)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
catastrophic-pattern match=false err=<nil>
oversized input rejected: true
```

### Tests

Create `patternguard_test.go`:

```go
package patternguard

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestLinearTimeNoBlowup(t *testing.T) {
	t.Parallel()
	g := Default()
	re, err := g.Compile(`(a+)+$`)
	if err != nil {
		t.Fatal(err)
	}
	// On a backtracking engine this would hang; on RE2 it returns immediately.
	start := time.Now()
	ok, err := g.Match(re, strings.Repeat("a", 4000)+"!")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("pattern unexpectedly matched")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("match took %v; expected linear time", elapsed)
	}
}

func TestRejectsOverLongPattern(t *testing.T) {
	t.Parallel()
	g := Guard{MaxPatternLen: 8, MaxNestDepth: 20, MaxInputLen: 1024}
	if _, err := g.Compile("aaaaaaaaaaaa"); !errors.Is(err, ErrPatternTooLong) {
		t.Fatalf("err = %v, want ErrPatternTooLong", err)
	}
}

func TestRejectsOverNestedPattern(t *testing.T) {
	t.Parallel()
	g := Guard{MaxPatternLen: 1024, MaxNestDepth: 5, MaxInputLen: 1024}
	nested := strings.Repeat("(", 8) + "a" + strings.Repeat(")", 8)
	if _, err := g.Compile(nested); !errors.Is(err, ErrTooComplex) {
		t.Fatalf("err = %v, want ErrTooComplex", err)
	}
}

func TestRejectsInvalidSyntax(t *testing.T) {
	t.Parallel()
	g := Default()
	if _, err := g.Compile("(unclosed"); !errors.Is(err, ErrBadSyntax) {
		t.Fatalf("err = %v, want ErrBadSyntax", err)
	}
}

func TestRejectsOverLongInput(t *testing.T) {
	t.Parallel()
	g := Guard{MaxPatternLen: 1024, MaxNestDepth: 20, MaxInputLen: 16}
	re, err := g.Compile("a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Match(re, strings.Repeat("a", 100)); !errors.Is(err, ErrInputTooLong) {
		t.Fatalf("err = %v, want ErrInputTooLong", err)
	}
}

func BenchmarkMatchScalesWithInput(b *testing.B) {
	g := Default()
	re, err := g.Compile(`(a+)+$`)
	if err != nil {
		b.Fatal(err)
	}
	input := strings.Repeat("a", 10_000) + "!"
	b.ReportAllocs()
	for b.Loop() {
		g.Match(re, input)
	}
}
```

## Review

The guard is correct when it treats the linear-time guarantee honestly — as
protection against *catastrophic backtracking*, not as blanket safety.
`TestLinearTimeNoBlowup` is the proof that carries the lesson: `(a+)+$`, the
canonical ReDoS pattern, matches thousands of characters in microseconds under
RE2, so accepting a user's pattern does not expose the exponential-time failure a
backtracking engine would. But the guard still adds what RE2 omits: a length and
nesting budget checked via `regexp/syntax.Parse` *before* the automaton is built,
`Compile` rather than `MustCompile` so a bad pattern is a 400 and not a panic, and
an input-size cap because there is no per-match timeout and CPU is still linear in
input length. The mistake this exercise exists to correct is "RE2 is linear, so
user input is safe" — true for time-per-byte, false for total bytes. Run
`go test -race`.

## Resources

- [Russ Cox: Regular Expression Matching Can Be Simple And Fast](https://swtch.com/~rsc/regexp/regexp1.html) — why RE2 avoids catastrophic backtracking, with the `(a+)+` example.
- [`regexp/syntax` package](https://pkg.go.dev/regexp/syntax) — `Parse` and the tree used to budget a pattern's complexity.
- [OWASP: Regular expression Denial of Service (ReDoS)](https://owasp.org/www-community/attacks/Regular_expression_Denial_of_Service_-_ReDoS) — the attack RE2 defeats and the resource limits it does not.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [08-streaming-log-grep.md](08-streaming-log-grep.md) | Next: [../08-unicode-normalization-and-collation/00-concepts.md](../08-unicode-normalization-and-collation/00-concepts.md)
