# Exercise 1: The grep Matcher Library

Almost every backend eventually grows a line-scanning utility — filtering a log
stream, extracting matching rows from an upload, tailing a file. This exercise
builds that reusable core as a library package (`package grep`, no `main`) so a
CLI, an HTTP handler, or a batch job can all import it. It also pins the real
production failure mode of `bufio.Scanner`: it silently gives up on lines longer
than 64 KiB.

This module is fully self-contained: it has its own `go mod init`, defines every
type it needs, and ships its own demo and tests. Nothing here imports another
exercise.

## What you'll build

```text
grepmatcher/                       module: example.com/grepmatcher
  go.mod
  internal/grep/grep.go            package grep: Matcher, New, Result, ErrNoMatch, ErrEmptyPattern, Match(io.Reader)
  cmd/demo/main.go                 runnable demo scanning an in-memory reader
  internal/grep/grep_test.go       table-driven tests incl. the long-line failure and its fix
```

- Files: `internal/grep/grep.go`, `cmd/demo/main.go`, `internal/grep/grep_test.go`.
- Implement: a `Matcher` reading an `io.Reader` line by line, returning one-indexed `Result`s, with sentinel errors and an optional `MaxLineBytes` cap that fixes the long-line failure.
- Test: matching lines, `ErrNoMatch` and `ErrEmptyPattern` via `errors.Is`, one-indexed line numbers, single-line contract, and the 64 KiB long-line drop plus its `Scanner.Buffer` fix.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### Why a library, and why sentinel errors

`package grep` has no `main`; it cannot be run directly, only imported. That is
the whole point — the matching logic belongs in one place that many callers reuse,
each importing `example.com/grepmatcher/internal/grep` via its module path (never a
relative `./internal/grep`, which module mode rejects). Placing it under
`internal/` also enforces a boundary: only code within this module may import it,
which is exactly what you want for a package that is an implementation detail
rather than a public API.

The two failure modes are package-level *sentinel* errors wrapped with `%w` at the
call site, so a caller can branch on them with `errors.Is` instead of matching
error strings. `ErrEmptyPattern` rejects a matcher configured with no substring (a
`grep` with an empty pattern would match every line — almost always a bug in the
caller). `ErrNoMatch` distinguishes "scanned fine, found nothing" from "the scan
itself failed", which a CLI maps to a different exit code than a real I/O error.

### The long-line trap

`bufio.NewScanner` defaults to a maximum token (line) size of
`bufio.MaxScanTokenSize`, which is 64 KiB. Feed it a line longer than that and
`Scan` returns `false` and `Scanner.Err()` reports `bufio.ErrTooLong` — the
scanner *stops*, silently dropping the rest of the input. A JSON-lines log where
one record is a 200 KiB stack trace will cause a naive scanner-based processor to
miss every line after it. The fix is `Scanner.Buffer(buf, max)`, which raises the
cap. This library exposes `MaxLineBytes`: zero keeps the safe 64 KiB default (a
matcher should not allocate unbounded memory by accident), and a positive value
opts into larger lines explicitly. The test pins both halves: the default drops a
long line, and setting `MaxLineBytes` makes the same input match.

Create `internal/grep/grep.go`:

```go
package grep

import (
	"bufio"
	"errors"
	"io"
	"strings"
)

// ErrNoMatch is returned when the scan completes without finding the substring.
var ErrNoMatch = errors.New("grep: no match")

// ErrEmptyPattern is returned when a Matcher has no substring configured.
var ErrEmptyPattern = errors.New("grep: empty pattern")

// Result is one matching line and its one-indexed position in the input.
type Result struct {
	LineNum int
	Line    string
}

// Matcher scans an io.Reader for lines containing Substr.
type Matcher struct {
	Substr string
	// MaxLineBytes caps the longest line Match will scan. Zero uses
	// bufio.Scanner's default (bufio.MaxScanTokenSize, 64 KiB); a line longer
	// than the cap fails the scan with bufio.ErrTooLong.
	MaxLineBytes int
}

// New returns a Matcher for the given substring.
func New(substr string) *Matcher {
	return &Matcher{Substr: substr}
}

// Match scans r line by line and returns every line containing Substr. It
// returns ErrEmptyPattern if Substr is empty, ErrNoMatch if nothing matched,
// or the scanner's error (e.g. bufio.ErrTooLong) if the scan itself failed.
func (m *Matcher) Match(r io.Reader) ([]Result, error) {
	if m.Substr == "" {
		return nil, ErrEmptyPattern
	}
	sc := bufio.NewScanner(r)
	if m.MaxLineBytes > 0 {
		sc.Buffer(make([]byte, 0, 64*1024), m.MaxLineBytes)
	}
	var out []Result
	lineNum := 0
	for sc.Scan() {
		lineNum++
		line := sc.Text()
		if strings.Contains(line, m.Substr) {
			out = append(out, Result{LineNum: lineNum, Line: line})
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, ErrNoMatch
	}
	return out, nil
}
```

### The runnable demo

`cmd/demo` is a separate `package main` that imports the library through its module
path. Because it is a different package, it can touch only exported names — which
is a useful forcing function for keeping a clean public surface.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"strings"

	"example.com/grepmatcher/internal/grep"
)

func main() {
	logs := "GET /health 200\nPOST /login 500\nGET /users 200\nPOST /orders 500"
	m := grep.New("500")

	results, err := m.Match(strings.NewReader(logs))
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	for _, r := range results {
		fmt.Printf("%d:%s\n", r.LineNum, r.Line)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
2:POST /login 500
4:POST /orders 500
```

### Tests

The tests are table-driven where the shape allows, assert sentinel errors with
`errors.Is`, and pin the long-line failure and its fix as two explicit cases.
`TestMatchReturnsResultWithLineNumber` is the preserved single-line contract:
matching one line yields exactly one `Result` with `LineNum: 1`.

Create `internal/grep/grep_test.go`:

```go
package grep

import (
	"bufio"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestMatchReturnsMatchingLines(t *testing.T) {
	t.Parallel()

	m := New("hello")
	got, err := m.Match(strings.NewReader("hello world\nfoo bar\nhello again"))
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	want := []Result{{1, "hello world"}, {3, "hello again"}}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (%+v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestMatchErrors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		substr  string
		input   string
		wantErr error
	}{
		{"no match", "missing", "foo bar\nbaz qux", ErrNoMatch},
		{"empty pattern", "", "anything at all", ErrEmptyPattern},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := New(tc.substr).Match(strings.NewReader(tc.input))
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestMatchLineNumbersAreOneIndexed(t *testing.T) {
	t.Parallel()

	got, err := New("x").Match(strings.NewReader("x\nfoo\nx"))
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if got[0].LineNum != 1 || got[1].LineNum != 3 {
		t.Fatalf("line numbers = %d, %d, want 1, 3", got[0].LineNum, got[1].LineNum)
	}
}

func TestMatchReturnsResultWithLineNumber(t *testing.T) {
	t.Parallel()

	got, err := New("hello").Match(strings.NewReader("hello world"))
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].LineNum != 1 || got[0].Line != "hello world" {
		t.Fatalf("got %+v, want {1 hello world}", got[0])
	}
}

func TestMatchDropsLongLineByDefault(t *testing.T) {
	t.Parallel()

	long := "needle" + strings.Repeat("a", bufio.MaxScanTokenSize) // > 64 KiB
	_, err := New("needle").Match(strings.NewReader(long))
	if !errors.Is(err, bufio.ErrTooLong) {
		t.Fatalf("err = %v, want bufio.ErrTooLong", err)
	}
}

func TestMatchScansLongLineWithBiggerBuffer(t *testing.T) {
	t.Parallel()

	long := "needle" + strings.Repeat("a", bufio.MaxScanTokenSize) // > 64 KiB
	m := &Matcher{Substr: "needle", MaxLineBytes: 1 << 20}         // 1 MiB cap
	got, err := m.Match(strings.NewReader(long))
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if len(got) != 1 || got[0].LineNum != 1 {
		t.Fatalf("got %+v, want one match on line 1", got)
	}
}

func ExampleMatcher_Match() {
	m := New("500")
	res, _ := m.Match(strings.NewReader("GET /a 200\nPOST /b 500"))
	for _, r := range res {
		fmt.Printf("%d:%s\n", r.LineNum, r.Line)
	}
	// Output: 2:POST /b 500
}
```

## Review

The library is correct when matching is a pure function of the substring and the
line content, line numbers are one-indexed, and the two configuration errors are
distinguishable from a real I/O error. The sentinel-error discipline is what makes
that possible: `errors.Is(err, grep.ErrNoMatch)` is a stable contract, whereas
comparing error strings is not. The long-line pair is the load-bearing test — it
proves the default 64 KiB cap actually drops input (a bug most code never notices
until a large record arrives in production) and that `Scanner.Buffer` fixes it.
The common trap is to treat `Scan` returning `false` as "clean EOF" and never
check `Scanner.Err()`; always check it, or a truncated scan looks like a short
file. Run `go test -race` to confirm nothing here shares state unsafely.

## Resources

- [`bufio.Scanner`](https://pkg.go.dev/bufio#Scanner) — `Scan`, `Err`, `Buffer`, and the `ErrTooLong` / `MaxScanTokenSize` contract.
- [`errors.Is`](https://pkg.go.dev/errors#Is) — matching wrapped sentinel errors.
- [Effective Go: Package names](https://go.dev/doc/effective_go#package-names) — naming a library package.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-grep-cli-executable.md](02-grep-cli-executable.md)
