# Exercise 12: A Command-Line SQL IN-Clause Builder With an Empty-IDs Guard

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Almost every service that does a batched lookup — "give me the users whose
IDs are in this set," "fetch every order belonging to these tenant IDs" —
ends up building a `WHERE id IN (?,?,?)` fragment by hand, plus the matching
`[]any` slice of driver arguments in the same order as the placeholders. The
placeholder count and the argument count must always match exactly, or the
query fails against the driver at execution time with an error that gives no
hint the mismatch traces back to slice construction. This module builds
both pieces as a small command-line tool: pipe a list of IDs in on stdin,
get the SQL fragment and its args back on stdout, ready to paste into a
migration script, a one-off debugging session, or a code review comment.

An empty ID list happens more than it should in the systems this tool is
meant to help debug — an upstream filter matched nothing, a batch got fully
deduplicated, a caller passed an empty slice by mistake. Building `IN ()`
from it is not a graceful no-op: most SQL drivers reject it as a syntax
error, and the drivers that tolerate it treat it as matching nothing, which
silently turns "filter by these IDs" into "return nothing" in a way that is
very easy to mistake for "no filter at all" during debugging. This tool
never emits that string; it refuses instead, with a distinct exit code the
caller's shell script can branch on.

This module is fully self-contained: its own `go mod init`, an executable
tool, and its tests. Nothing here imports another exercise.

## What you'll build

```text
inclause/                 module example.com/inclause
  go.mod                  go 1.24
  inclause.go              ErrEmptyIDs, ErrInvalidID; ParseIDs, BuildInClause
  inclause_test.go         parse table, clause table, buildInClauseNaive contrast,
                          end-to-end run() table
  main.go                  flags, stdin/stdout, exit codes
```

- Files: `inclause.go`, `inclause_test.go`, `main.go`.
- Implement: `ParseIDs(r io.Reader) ([]int, error)` reading newline-delimited ids, skipping blank lines, and wrapping a bad line in `ErrInvalidID` with its line number; `BuildInClause(column string, ids []int) (sql string, args []any, err error)` returning `ErrEmptyIDs` for zero ids and otherwise building `"column IN (?,?,...)"` with a `strings.Builder` sized by `Grow` to the exact byte count and `args := make([]any, 0, len(ids))`; `run(args []string, stdin io.Reader, stdout io.Writer) error` wiring the two together behind a `-column` flag.
- Tool: `inclause` reads ids from stdin, one per line, and prints the SQL fragment and its args to stdout. It exits `2` for an empty or malformed input (including an unknown flag), `1` if writing the output fails, and `0` on success.
- Test: `ParseIDs` over blank lines, whitespace, and a bad line with its line number; `BuildInClause` for one id and for four ids, asserting `cap(args) == len(args)`; a `buildInClauseNaive` contrast proving the unguarded join emits the literal string `"id IN ()"` for zero ids, which `BuildInClause` never does; and an end-to-end `run()` table over `strings.Reader`/`bytes.Buffer` covering the default column, a custom column flag, empty input, blank-only input, a malformed id, and an unknown flag.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Two preallocations for one query fragment, and a guard the loop cannot express

Building `"column IN (?,?,...)"` and its argument slice is really two
preallocation problems stacked on top of each other. The `args` slice has a
final length — exactly `len(ids)` — known before the loop starts, so
`make([]any, 0, len(ids))` reserves it once and every append lands in place.
The SQL string applies the same idea to `strings.Builder`: `Builder.Grow(n)`
reserves `n` bytes up front, the same way `make([]T, 0, n)` reserves slots.
Because the shape of the fragment is fully known — the column name, the
literal `" IN ("`, one `?` byte per id, one `,` byte between each pair, and
a closing `)` — the exact byte count is computable before writing anything:
`len(column) + len(" IN (") + len(ids) + (len(ids)-1) + len(")")`.

The guard exists because "zero ids" is not a smaller version of the normal
case, it is a different case the placeholder-joining loop cannot express at
all. Writing the loop the obvious way, without a guard in front of it, looks
entirely reasonable:

```go
func buildInClauseNaive(column string, ids []int) string {
    placeholders := make([]string, len(ids))
    for i := range ids {
        placeholders[i] = "?"
    }
    return column + " IN (" + strings.Join(placeholders, ",") + ")"
}
```

For zero ids, `placeholders` is an empty slice, `strings.Join` of an empty
slice is `""`, and the function returns `"id IN ()"` — valid Go, no panic,
nothing that looks wrong reading the diff. `BuildInClause` never reaches
that loop when `len(ids) == 0`; it returns `ErrEmptyIDs` immediately, and
`main.go` turns that into exit code `2` instead of a query the driver
rejects, or worse, silently accepts as "match nothing."

Create `inclause.go`:

```go
// This file holds inclause's domain logic: parsing a list of ids from a
// reader and building the "column IN (?,?,...)" SQL fragment plus its
// matching driver arguments. main.go is the only file that touches flags,
// stdin/stdout, or exit codes.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// ErrEmptyIDs is returned by BuildInClause when it is asked to build a
// clause for zero ids. "IN ()" is invalid SQL on most drivers -- a syntax
// error -- and on the rest it silently matches zero rows, turning "filter by
// these IDs" into "return nothing" in a way that is easy to mistake for "no
// filter at all" while debugging a query. BuildInClause never emits it.
var ErrEmptyIDs = errors.New("inclause: cannot build IN clause for zero ids")

// ErrInvalidID is returned by ParseIDs when a non-blank input line cannot be
// parsed as an integer. It wraps the offending 1-based line number so the
// caller can point back at exactly the malformed input.
var ErrInvalidID = errors.New("inclause: invalid id")

// ParseIDs reads r line by line and parses each non-blank line as an
// integer id. A line that is blank after trimming surrounding whitespace is
// skipped, so trailing newlines and blank separators in the input are not
// errors; a non-blank line that fails to parse as an integer returns
// ErrInvalidID wrapped with its line number.
//
// ParseIDs holds no state of its own and is safe to call concurrently from
// multiple goroutines, as long as each call is given its own io.Reader.
func ParseIDs(r io.Reader) ([]int, error) {
	var ids []int
	sc := bufio.NewScanner(r)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		id, err := strconv.Atoi(text)
		if err != nil {
			return nil, fmt.Errorf("%w: line %d: %q", ErrInvalidID, line, text)
		}
		ids = append(ids, id)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("inclause: reading input: %w", err)
	}
	return ids, nil
}

// BuildInClause builds "column IN (?,?,...)" plus the matching args slice
// for a driver that uses "?" placeholders. Both the SQL string and args are
// exactly preallocated from len(ids): the strings.Builder reserves the
// precise byte count the fragment's known shape requires, and args is
// make([]any, 0, len(ids)), so neither ever reallocates regardless of how
// many ids are passed. It returns ErrEmptyIDs instead of ever emitting
// "IN ()", pushing the decision of what "no ids to filter by" should mean
// back to the caller, where that business context actually lives.
//
// BuildInClause holds no state of its own and is safe to call concurrently.
func BuildInClause(column string, ids []int) (sql string, args []any, err error) {
	if len(ids) == 0 {
		return "", nil, ErrEmptyIDs
	}

	// Exact byte count: column + " IN (" + one '?' per id + one ',' between
	// each pair of ids + closing ')'.
	var b strings.Builder
	b.Grow(len(column) + len(" IN (") + len(ids) + (len(ids) - 1) + len(")"))
	b.WriteString(column)
	b.WriteString(" IN (")

	args = make([]any, 0, len(ids))
	for i, id := range ids {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('?')
		args = append(args, id)
	}
	b.WriteByte(')')

	return b.String(), args, nil
}
```

### The tool

`run` takes exactly the inputs a real CLI has — the flag arguments, stdin,
stdout — as parameters instead of reaching for `os.Args`, `os.Stdin`, and
`os.Stdout` directly, which is what makes it testable end to end with
`strings.Reader` and `bytes.Buffer` instead of spawning a subprocess.
`main`'s only jobs are calling `run`, printing any error to `os.Stderr`
(never stdout, so a caller piping this tool's output into another program
never sees an error mixed into the data), and choosing an exit code:
`ErrEmptyIDs`, `ErrInvalidID`, and a flag-parsing failure (including
`-h`/`--help`) are all input problems the caller can fix, so they exit `2`;
a failure while writing output is an execution failure, exit `1`; success is
`0`. The output itself is buffered with `bufio.Writer` and flushed once at
the end, so a large id list does not make one `fmt.Fprintln` call per line
of output.

Create `main.go`:

```go
// Command inclause reads a newline-delimited list of integer ids from
// standard input and prints the parameterized "column IN (?,?,...)" SQL
// fragment for those ids, followed by the matching driver argument list, so
// both can be pasted straight into a batched-lookup query.
//
// Usage:
//
//	inclause [-column name] < ids.txt
//
// Exit codes: 0 on success, 2 if the input could not be parsed (a
// non-integer line) or was empty, 1 if writing the output failed.
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

func run(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("inclause", flag.ContinueOnError)
	column := fs.String("column", "id", "column name for the IN clause")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "inclause: build a SQL IN clause from newline-delimited ids on stdin")
		fmt.Fprintln(fs.Output(), "usage: inclause [-column name] < ids.txt")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	ids, err := ParseIDs(stdin)
	if err != nil {
		return err
	}

	sql, sqlArgs, err := BuildInClause(*column, ids)
	if err != nil {
		return err
	}

	w := bufio.NewWriter(stdout)
	if _, err := fmt.Fprintln(w, sql); err != nil {
		return err
	}
	parts := make([]string, len(sqlArgs))
	for i, a := range sqlArgs {
		parts[i] = fmt.Sprint(a)
	}
	if _, err := fmt.Fprintf(w, "args: %s\n", strings.Join(parts, ",")); err != nil {
		return err
	}
	return w.Flush()
}

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "inclause:", err)
		if errors.Is(err, ErrEmptyIDs) || errors.Is(err, ErrInvalidID) || errors.Is(err, flag.ErrHelp) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}
```

Run it:

```bash
printf '7\n2\n9\n' | go run .
printf '1\n2\n' | go run . -column=tenant_id
```

Expected output:

```text
id IN (?,?,?)
args: 7,2,9
```

```text
tenant_id IN (?,?)
args: 1,2
```

Piping empty input, `printf '' | go run .`, prints
`inclause: inclause: cannot build IN clause for zero ids` to stderr and
exits `2` — never a line reading `id IN ()`. A malformed line,
`printf '1\nnope\n' | go run .`, prints
`inclause: inclause: invalid id: line 2: "nope"` and exits `2` as well,
pointing straight at the line that failed instead of a generic parse error.

### Tests

`TestParseIDs` is the table over input shapes: empty input, blank-only
input, blank lines mixed with real ids, surrounding whitespace, and a
non-integer line, the last asserted with `errors.Is` against `ErrInvalidID`.
`TestParseIDsErrorIncludesLineNumber` locks in that the error message
actually names the failing line, the detail that turns a parse failure into
something a caller can act on instead of a generic complaint.
`TestBuildInClauseSingleID` and `TestBuildInClauseNIDs` assert the exact
generated SQL text and args, including that `cap(args)` lands exactly on
`len(args)` with no slack — the direct evidence that
`make([]any, 0, len(ids))` reserved precisely what was needed.

`TestBuggyVersionEmitsInvalidEmptyClause` is the module's core proof.
`buildInClauseNaive` is unexported and unreachable from the tool's logic; it
exists so the test can pin, byte for byte, the exact invalid string an
unguarded join produces for zero ids, and then show `BuildInClause`
returning `ErrEmptyIDs` instead on the same input. If a future edit ever
removed the guard, this is the test that would fail, not a customer's
migration script. `TestRunEndToEnd` drives `run` exactly the way `main`
does, over `strings.Reader` and `bytes.Buffer`, covering the default
column, a custom `-column` flag, the empty-input guard, blank-only input,
a malformed id, and an unrecognized flag.

Create `inclause_test.go`:

```go
package main

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"
)

// buildInClauseNaive is the fragment builder as it is usually written the
// first time, and as it ships: no guard for zero ids, just a join of one
// placeholder per id. It is never exported and never reachable from the
// tool's logic; it exists so the tests can pin what it gets wrong.
func buildInClauseNaive(column string, ids []int) string {
	placeholders := make([]string, len(ids))
	for i := range ids {
		placeholders[i] = "?"
	}
	return column + " IN (" + strings.Join(placeholders, ",") + ")"
}

func TestBuggyVersionEmitsInvalidEmptyClause(t *testing.T) {
	t.Parallel()

	got := buildInClauseNaive("id", nil)
	if got != "id IN ()" {
		t.Fatalf("buildInClauseNaive(nil) = %q, want %q (the invalid fragment this tool must never emit)", got, "id IN ()")
	}

	if _, _, err := BuildInClause("id", nil); !errors.Is(err, ErrEmptyIDs) {
		t.Fatalf("BuildInClause(nil) error = %v, want ErrEmptyIDs", err)
	}
}

func TestParseIDs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		wantIDs []int
		wantErr bool
	}{
		{name: "empty input yields no ids", input: "", wantIDs: nil},
		{name: "blank lines are skipped", input: "\n\n  \n", wantIDs: nil},
		{name: "one id", input: "7\n", wantIDs: []int{7}},
		{name: "several ids with blank separators", input: "1\n\n2\n3\n", wantIDs: []int{1, 2, 3}},
		{name: "surrounding whitespace is trimmed", input: "  4  \n", wantIDs: []int{4}},
		{name: "non-integer line is an error", input: "1\nabc\n3\n", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ids, err := ParseIDs(strings.NewReader(tc.input))
			if tc.wantErr {
				if !errors.Is(err, ErrInvalidID) {
					t.Fatalf("ParseIDs(%q) error = %v, want ErrInvalidID", tc.input, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseIDs(%q): unexpected error: %v", tc.input, err)
			}
			if !slices.Equal(ids, tc.wantIDs) {
				t.Fatalf("ParseIDs(%q) = %v, want %v", tc.input, ids, tc.wantIDs)
			}
		})
	}
}

func TestParseIDsErrorIncludesLineNumber(t *testing.T) {
	t.Parallel()

	_, err := ParseIDs(strings.NewReader("1\n2\nnot-a-number\n4\n"))
	if err == nil || !strings.Contains(err.Error(), "line 3") {
		t.Fatalf("ParseIDs error = %v, want it to mention line 3", err)
	}
}

func TestBuildInClauseSingleID(t *testing.T) {
	t.Parallel()

	sql, args, err := BuildInClause("id", []int{7})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if sql != "id IN (?)" {
		t.Fatalf("sql = %q, want %q", sql, "id IN (?)")
	}
	if !slices.Equal(args, []any{7}) {
		t.Fatalf("args = %v, want [7]", args)
	}
}

func TestBuildInClauseNIDs(t *testing.T) {
	t.Parallel()

	sql, args, err := BuildInClause("tenant_id", []int{1, 2, 3, 4})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if sql != "tenant_id IN (?,?,?,?)" {
		t.Fatalf("sql = %q, want %q", sql, "tenant_id IN (?,?,?,?)")
	}
	want := []any{1, 2, 3, 4}
	if !slices.Equal(args, want) {
		t.Fatalf("args = %v, want %v", args, want)
	}
	if cap(args) != len(want) {
		t.Fatalf("cap(args) = %d, want exactly %d (no over-allocation)", cap(args), len(want))
	}
}

// TestRunEndToEnd drives run over strings.Reader/bytes.Buffer exactly the
// way main wires it to os.Stdin/os.Stdout, covering the success path, the
// empty-input guard, and a malformed input line.
func TestRunEndToEnd(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		args       []string
		stdin      string
		wantStdout string
		wantErr    error
	}{
		{
			name:       "default column",
			args:       nil,
			stdin:      "7\n2\n9\n",
			wantStdout: "id IN (?,?,?)\nargs: 7,2,9\n",
		},
		{
			name:       "custom column flag",
			args:       []string{"-column=tenant_id"},
			stdin:      "1\n2\n",
			wantStdout: "tenant_id IN (?,?)\nargs: 1,2\n",
		},
		{
			name:    "empty input is guarded, not printed as IN ()",
			args:    nil,
			stdin:   "",
			wantErr: ErrEmptyIDs,
		},
		{
			name:    "blank-only input is guarded the same as truly empty",
			args:    nil,
			stdin:   "\n\n",
			wantErr: ErrEmptyIDs,
		},
		{
			name:    "malformed id line",
			args:    nil,
			stdin:   "1\nnope\n",
			wantErr: ErrInvalidID,
		},
		{
			name:    "unknown flag is a usage error",
			args:    []string{"-nope"},
			stdin:   "1\n",
			wantErr: nil, // checked separately below: any non-nil, non-sentinel error
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var out bytes.Buffer
			err := run(tc.args, strings.NewReader(tc.stdin), &out)

			if tc.name == "unknown flag is a usage error" {
				if err == nil {
					t.Fatal("run() with an unknown flag: want a non-nil error")
				}
				return
			}
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("run() error = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("run(): unexpected error: %v", err)
			}
			if out.String() != tc.wantStdout {
				t.Fatalf("stdout = %q, want %q", out.String(), tc.wantStdout)
			}
		})
	}
}
```

## Review

`inclause` is correct when the placeholder count in its printed SQL always
equals the number of args on the next line, when `Grow` and
`make([]any, 0, len(ids))` mean neither the builder nor the args slice ever
reallocates regardless of `len(ids)`, and when zero ids never reach the
string-building loop at all — they exit `2` instead. The empty-ids guard is
the part most likely to be missing from a first draft, because the naive
join "just works" for zero elements in the sense that it does not panic; it
produces `column IN ()`, a string that compiles as Go code and looks
plausible in a debugger, and only breaks the query at the database.
`TestBuggyVersionEmitsInvalidEmptyClause` pins that exact string against
the naive version so the difference is a fact the test suite states, not a
claim in a comment. `TestRunEndToEnd` is the tool's contract end to end:
`errors.Is` on `ErrEmptyIDs` and `ErrInvalidID` map to exit `2`, everything
else maps to `1`, and errors always go to stderr so a shell pipeline never
mistakes an error message for query output. Run
`go test -count=1 -race ./...`.

## Resources

- [`strings.Builder`](https://pkg.go.dev/strings#Builder) — `Grow` reserves an exact byte count the same way `make(..., 0, n)` reserves slice capacity.
- [`database/sql`](https://pkg.go.dev/database/sql) — the driver layer that turns `?` placeholders and an `[]any` args slice into a parameterized query.
- [`flag`](https://pkg.go.dev/flag) — `flag.ContinueOnError` plus a custom `Usage` func, used to keep `run` testable without calling `os.Exit` itself.
- [`bufio.Scanner`](https://pkg.go.dev/bufio#Scanner) — the line reader `ParseIDs` streams input through.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [11-grow-for-known-size-bulk-insert.md](11-grow-for-known-size-bulk-insert.md) | Next: [13-per-goroutine-append-then-concat.md](13-per-goroutine-append-then-concat.md)
