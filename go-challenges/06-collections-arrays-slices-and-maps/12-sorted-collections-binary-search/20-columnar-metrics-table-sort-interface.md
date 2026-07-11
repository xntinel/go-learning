# Exercise 20: A Columnar Metrics Table and sort.Interface

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Time-series and analytics engines -- Prometheus's TSDB, ClickHouse, Apache
Arrow's record batches -- rarely store a batch of events as a slice of
structs. They store it as a *struct of arrays*: one slice per column, so a
scan that only needs the timestamp column never has to touch host names or
values sitting elsewhere in memory, and the CPU cache stays full of the data
that scan actually cares about. The generic `slices.Sort` and
`slices.SortFunc` this lesson has used everywhere else operate on exactly
one slice, though, which is precisely the layout a columnar table does not
have. Sorting a struct-of-arrays table by one column while keeping every
other column aligned to it is the one place in modern Go where the older
`sort.Interface` -- `Len`, `Less`, `Swap` -- still earns its keep: `Swap` is
free to reorder every column together, in lockstep, in one call.

The trap is that the naive fix compiles, runs, and looks identical to the
correct one from the caller's side. An engineer who reaches for
`slices.Sort(table.TS)` to sort the timestamp column gets a perfectly sorted
`TS` slice back -- and a `Host` and `Value` column that never moved, so row
`i`'s timestamp now belongs to whatever host and value happened to already
sit at index `i`. There is no panic and no test failure unless the test
happens to check that specific pairing: it is the columnar equivalent of a
two-column spreadsheet where someone sorted only column A.

This module builds `colidx`, a command-line tool that reads NDJSON metric
events, sorts them into a columnar `Table` by timestamp using `sort.Sort`
with a `Table`-wide `Swap`, and prints the rows inside a requested half-open
time window as TSV. It is fully self-contained: its own `go mod init`, an
executable command, and its tests. Nothing here imports another exercise.

## What you'll build

```text
colidx/                        module example.com/colidx
  go.mod                       go 1.24
  colidx.go                    package main — Event, Table (sort.Interface); NewTable, Slice
  colidx_test.go                package main — NewTable/Slice tables, the naive
                                single-column-sort contrast, aliasing, run() end to end
  main.go                      package main — -from/-to flags, NDJSON stdin, exit codes
```

- Files: `colidx.go`, `colidx_test.go`, `main.go`.
- Implement: `NewTable(events []Event) (*Table, error)` rejecting any event with an empty `Host` via `ErrEmptyHost`, otherwise building the three columns and sorting once with `sort.Sort`; `Table` implementing `sort.Interface` (`Len`, `Less`, `Swap`) so `Swap` reorders `TS`, `Host`, and `Value` together; `(*Table).Slice(from, to int64) (Table, error)` returning the half-open window `[from, to)` located by two `sort.Search` calls, rejecting `from > to` with `ErrInvalidRange`.
- Tool: `colidx` reads newline-delimited JSON events (`{"ts":...,"host":...,"value":...}`) from stdin and prints the rows in `[-from, -to)`, sorted by `ts`, as tab-separated `ts`, `host`, `value` lines to stdout. `-from` and `-to` default to unbounded. Exit 0 on success; exit 2 for a bad flag, malformed JSON, an empty host, or `from > to` (all usage errors); exit 1 is reserved for a runtime failure -- none exists in this tool once the input validates.
- Test: `NewTable` over scrambled events, an empty host, and nil input; `Slice` over a table of windows including both boundaries, an empty table, and the `from > to` rejection; the aliasing contract on `Slice`'s returned columns; the single-column-sort contrast against an unexported `sortByTimestampOnlyNaive` helper; and `run` end to end over a `strings.Reader` and a `bytes.Buffer`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/colidx
cd ~/go-exercises/colidx
go mod init example.com/colidx
go mod edit -go=1.24
```

### Sorting one column of a struct-of-arrays table desyncs the rest

The bug looks like the obviously correct move, which is exactly why it
survives review:

```go
func sortByTimestampOnlyNaive(events []Event) (ts []int64, host []string, value []float64) {
    ts = make([]int64, len(events))
    host = make([]string, len(events))
    value = make([]float64, len(events))
    for i, e := range events {
        ts[i], host[i], value[i] = e.TS, e.Host, e.Value
    }
    slices.Sort(ts) // BUG: reorders ts in place; host and value never move
    return
}
```

`slices.Sort(ts)` does exactly what its name promises: it sorts `ts`.
`slices.Sort` has never heard of `host` or `value`, has no way to know they
are meant to travel with `ts`, and cannot reorder them even if it wanted
to -- its signature only accepts the one slice. After this call, `ts[0]`
holds the smallest timestamp, but `host[0]` and `value[0]` still hold
whatever host and value happened to occupy index 0 *before* the sort. If
that was originally a different row's data, the table now silently
attributes one host's metric value to a different host at a different time.

`sort.Interface` fixes this because `Swap(i, j int)` is a single method that
sees the whole receiver, not one column: `Table.Swap` moves `TS[i]`,
`Host[i]`, and `Value[i]` together in the same call that moves `TS[j]`,
`Host[j]`, and `Value[j]`. `sort.Sort` never touches the columns directly --
it only ever calls `Less` to compare and `Swap` to reorder -- so as long as
`Swap` keeps every column in lockstep, the columns cannot drift apart no
matter what order the sort algorithm chooses to make its comparisons and
swaps in.

Create `colidx.go`:

```go
package main

import (
	"errors"
	"fmt"
	"sort"
)

// Sentinel errors returned by NewTable and Table.Slice. Callers should test
// for them with errors.Is rather than comparing error strings.
var (
	// ErrEmptyHost means an event's Host field was the empty string.
	ErrEmptyHost = errors.New("colidx: event has an empty host")
	// ErrInvalidRange means Slice was asked for a range with from > to.
	ErrInvalidRange = errors.New("colidx: from must be <= to")
)

// Event is one row of input, as decoded from a line of NDJSON.
type Event struct {
	TS    int64   `json:"ts"`
	Host  string  `json:"host"`
	Value float64 `json:"value"`
}

// Table is an in-memory columnar (struct-of-arrays) store of events, kept
// sorted by TS: TS[i], Host[i], and Value[i] together describe row i, the
// layout time-series and analytics engines (Prometheus's TSDB, ClickHouse,
// Apache Arrow) use so a scan over one column never touches the others. It
// implements sort.Interface so the three columns reorder in lockstep under
// sort.Sort; NewTable is the only place that calls it. After NewTable
// returns nothing here mutates a Table, so it is safe for concurrent
// read-only use; a caller that sorts or mutates one directly must serialize
// that against concurrent readers.
type Table struct {
	TS    []int64
	Host  []string
	Value []float64
}

// NewTable builds a Table from events, sorted by TS ascending. It returns
// ErrEmptyHost, wrapped with the offending event's index and timestamp, if
// any event has an empty Host. A nil or empty events slice is not an error;
// it produces a valid, empty Table. It builds the three columns first and
// sorts once at the end, rather than inserting one row at a time: for a bulk
// load that is O(n log n) total, not O(n log n) per insert.
func NewTable(events []Event) (*Table, error) {
	for i, e := range events {
		if e.Host == "" {
			return nil, fmt.Errorf("%w: event %d (ts=%d)", ErrEmptyHost, i, e.TS)
		}
	}
	t := &Table{
		TS:    make([]int64, len(events)),
		Host:  make([]string, len(events)),
		Value: make([]float64, len(events)),
	}
	for i, e := range events {
		t.TS[i] = e.TS
		t.Host[i] = e.Host
		t.Value[i] = e.Value
	}
	sort.Sort(t)
	return t, nil
}

// Len implements sort.Interface.
func (t *Table) Len() int { return len(t.TS) }

// Less implements sort.Interface, ordering rows by TS ascending.
func (t *Table) Less(i, j int) bool { return t.TS[i] < t.TS[j] }

// Swap implements sort.Interface. It exchanges row i and row j across all
// three columns together -- the reason Table implements sort.Interface
// instead of relying on slices.Sort over a single column: slices.Sort only
// ever has one slice's worth of Swap to perform, and a struct-of-arrays
// table needs all three moved together or the columns misalign.
func (t *Table) Swap(i, j int) {
	t.TS[i], t.TS[j] = t.TS[j], t.TS[i]
	t.Host[i], t.Host[j] = t.Host[j], t.Host[i]
	t.Value[i], t.Value[j] = t.Value[j], t.Value[i]
}

// Slice returns the rows whose TS lies in the half-open interval
// [from, to), located by two binary searches over the sorted TS column. It
// returns ErrInvalidRange if from > to.
//
// The returned Table's columns are sub-slices of the receiver's backing
// arrays: it aliases the receiver and must not be mutated by the caller.
// Clone the columns if you need an independent copy.
func (t *Table) Slice(from, to int64) (Table, error) {
	if from > to {
		return Table{}, fmt.Errorf("%w: got from=%d to=%d", ErrInvalidRange, from, to)
	}
	lo := sort.Search(len(t.TS), func(i int) bool { return t.TS[i] >= from })
	hi := sort.Search(len(t.TS), func(i int) bool { return t.TS[i] >= to })
	return Table{TS: t.TS[lo:hi], Host: t.Host[lo:hi], Value: t.Value[lo:hi]}, nil
}
```

### The tool

`colidx` never touches `os.Args`, `os.Stdin`, `os.Stdout`, or `os.Exit`
inside `run`: it takes the argument slice and an `io.Reader`/`io.Writer`
pair instead, so a test can drive the whole pipeline -- flag parsing, NDJSON
decoding, table construction, and the range query -- over a
`strings.Reader` and a `bytes.Buffer` with no real process involved.
`-from` and `-to` default to `math.MinInt64` and `math.MaxInt64`, so both
bounds can be omitted independently: an unset `-from` really does mean "the
beginning of time" for this query. Reading is line-by-line with
`bufio.Scanner` rather than decoding the whole input as one JSON array, so a
malformed line deep in a large file is reported by line number instead of
requiring the whole file to be buffered before the first error can even be
detected.

Every failure `run` can produce -- a bad flag, a line that fails to decode,
an empty host, or `from > to` -- is something the caller fixes by changing
the input or the command line, so all of them wrap the `errUsage` sentinel
and `main` maps that to exit code 2. Go 1.20's support for multiple `%w`
verbs in one `fmt.Errorf` call keeps both sentinels checkable with
`errors.Is`: `fmt.Errorf("%w: %w", errUsage, err)` lets a caller test for
`errUsage` *and* for `ErrEmptyHost` or `ErrInvalidRange` on the same
returned error. Exit code 1 is reserved for a runtime failure, but this
tool has none once its flags and NDJSON validate.

Create `main.go`:

```go
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
)

// errUsage marks a failure the caller can fix by changing the input or the
// command line. main maps it to exit code 2; every other error maps to 1.
var errUsage = errors.New("usage")

// run reads NDJSON events from stdin, builds a sorted Table, and writes the
// rows within [-from, -to) to stdout as TSV. It never touches os.Args or
// os.Exit, so it can be driven in a test with a strings.Reader and buffer.
func run(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("colidx", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	from := fs.Int64("from", math.MinInt64, "start of range, inclusive (unix seconds)")
	to := fs.Int64("to", math.MaxInt64, "end of range, exclusive (unix seconds)")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}

	var events []Event
	sc := bufio.NewScanner(stdin)
	line := 0
	for sc.Scan() {
		line++
		text := sc.Text()
		if text == "" {
			continue
		}
		var e Event
		if err := json.Unmarshal([]byte(text), &e); err != nil {
			return fmt.Errorf("%w: line %d: %v", errUsage, line, err)
		}
		events = append(events, e)
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("reading input: %w", err)
	}

	table, err := NewTable(events)
	if err != nil {
		return fmt.Errorf("%w: %w", errUsage, err)
	}

	window, err := table.Slice(*from, *to)
	if err != nil {
		return fmt.Errorf("%w: %w", errUsage, err)
	}

	for i := range window.TS {
		fmt.Fprintf(stdout, "%d\t%s\t%v\n", window.TS[i], window.Host[i], window.Value[i])
	}
	return nil
}

func main() {
	flag.CommandLine.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: colidx [-from N] [-to N] < events.ndjson")
		fmt.Fprintln(os.Stderr, "sorts NDJSON {ts,host,value} events by ts and prints those in [from,to) as TSV.")
	}
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "colidx:", err)
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}
```

Run it:

```bash
printf '{"ts":30,"host":"c","value":3}\n{"ts":10,"host":"a","value":1}\n{"ts":20,"host":"b","value":2}\n' | go run .
printf '{"ts":30,"host":"c","value":3}\n{"ts":10,"host":"a","value":1}\n{"ts":20,"host":"b","value":2}\n' | go run . -from 10 -to 30
printf '{"ts":30,"host":"c","value":3}\n{"ts":10,"host":"a","value":1}\n{"ts":20,"host":"b","value":2}\n' | go run . -from 30 -to 10
```

Expected output:

```text
10	a	1
20	b	2
30	c	3
```

```text
10	a	1
20	b	2
```

```text
colidx: usage: colidx: from must be <= to: got from=30 to=10
```

The first run has no `-from`/`-to`, so the defaults span all time and the
output is every event, sorted -- proof that `NewTable`'s single `sort.Sort`
call correctly reordered all three columns together even though the input
arrived with `c` first. The second run asks for `[10, 30)`: it includes the
row at `ts=10` and excludes the row at `ts=30`, pinning the half-open
boundary. The third run swaps `from` and `to`; `Slice` rejects it before any
row is printed, and the message is exactly what `run` returns: `errUsage`
wrapping `ErrInvalidRange` wrapping the offending values.

### Tests

`TestNewTable` is the construction table: a scrambled input that must come
back with `TS`, `Host`, and `Value` all correctly paired after sorting, an
event with an empty `Host` rejected with `ErrEmptyHost`, and `nil` producing
a valid empty table. `TestSlice` is the half-open interval table this
lesson builds on throughout -- a full range, a middle window, both
boundaries, both empty-window shapes, an empty table, and the `from > to`
rejection -- run against one shared `Table` built once. `TestSliceAliasesReceiver`
pins the aliasing contract documented on `Slice` by mutating a returned
column and checking the change is visible on the original `Table`.

`TestNaiveSingleColumnSortDesyncsPairing` is the test that matters most: it
runs `sortByTimestampOnlyNaive` over the same scrambled events `NewTable`
uses, and asserts the exact defect -- the sorted `ts` column's first entry
(`10`) ends up paired with `host[0]` still holding `"c"`, the host that
happened to occupy index 0 before the sort, not `"a"`, the host that
timestamp `10` actually belongs to. The same input through `NewTable`
produces the correct pairing. `TestRun` drives the whole command end to end
over `strings.Reader`/`bytes.Buffer`: the default full range, a half-open
window, empty input, a malformed JSON line, an empty host, `from > to`, and
an unknown flag, each checked against `errUsage` with `errors.Is` where an
error is expected.

Create `colidx_test.go`:

```go
package main

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"
)

// sortByTimestampOnlyNaive sorts the ts column alone with slices.Sort,
// leaving host/value where they were -- the bug that ships when someone
// reaches for a single-column sort on a struct-of-arrays table. It is never
// exported and never reachable from Table's API.
func sortByTimestampOnlyNaive(events []Event) (ts []int64, host []string, value []float64) {
	ts = make([]int64, len(events))
	host = make([]string, len(events))
	value = make([]float64, len(events))
	for i, e := range events {
		ts[i], host[i], value[i] = e.TS, e.Host, e.Value
	}
	slices.Sort(ts) // BUG: reorders ts in place; host and value never move
	return
}

func scrambledEvents() []Event {
	return []Event{
		{TS: 30, Host: "c", Value: 3},
		{TS: 10, Host: "a", Value: 1},
		{TS: 20, Host: "b", Value: 2},
	}
}

func TestNewTable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		events   []Event
		wantLen  int
		wantTS   []int64
		wantHost []string
		wantErr  error
	}{
		{name: "sorts all columns together", events: scrambledEvents(), wantLen: 3, wantTS: []int64{10, 20, 30}, wantHost: []string{"a", "b", "c"}},
		{name: "empty host rejected", events: []Event{{TS: 1, Host: "", Value: 1}}, wantErr: ErrEmptyHost},
		{name: "nil events produce an empty table", events: nil, wantLen: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			table, err := NewTable(tc.events)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("NewTable error = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewTable: %v", err)
			}
			if table.Len() != tc.wantLen {
				t.Fatalf("Len() = %d, want %d", table.Len(), tc.wantLen)
			}
			for i, ts := range tc.wantTS {
				if table.TS[i] != ts || table.Host[i] != tc.wantHost[i] {
					t.Fatalf("row %d = (%d,%s), want (%d,%s)", i, table.TS[i], table.Host[i], ts, tc.wantHost[i])
				}
			}
		})
	}
}

func TestSlice(t *testing.T) {
	t.Parallel()
	table, err := NewTable(scrambledEvents())
	if err != nil {
		t.Fatalf("NewTable: %v", err)
	}
	empty, err := NewTable(nil)
	if err != nil {
		t.Fatalf("NewTable(nil): %v", err)
	}
	tests := []struct {
		name     string
		table    *Table
		from, to int64
		wantTS   []int64
		wantErr  error
	}{
		{name: "full range", table: table, from: 0, to: 100, wantTS: []int64{10, 20, 30}},
		{name: "middle window", table: table, from: 15, to: 25, wantTS: []int64{20}},
		{name: "lower bound inclusive", table: table, from: 10, to: 20, wantTS: []int64{10}},
		{name: "upper bound exclusive", table: table, from: 20, to: 30, wantTS: []int64{20}},
		{name: "from equals to is empty", table: table, from: 20, to: 20, wantTS: nil},
		{name: "before all rows", table: table, from: -100, to: 5, wantTS: nil},
		{name: "after all rows", table: table, from: 40, to: 100, wantTS: nil},
		{name: "empty table", table: empty, from: 0, to: 100, wantTS: nil},
		{name: "from after to rejected", table: table, from: 30, to: 10, wantErr: ErrInvalidRange},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			window, err := tc.table.Slice(tc.from, tc.to)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Slice(%d,%d) error = %v, want %v", tc.from, tc.to, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Slice(%d,%d): %v", tc.from, tc.to, err)
			}
			if len(window.TS) != len(tc.wantTS) {
				t.Fatalf("Slice(%d,%d) got %v, want %v", tc.from, tc.to, window.TS, tc.wantTS)
			}
			for i, ts := range tc.wantTS {
				if window.TS[i] != ts {
					t.Fatalf("Slice(%d,%d)[%d] = %d, want %d", tc.from, tc.to, i, window.TS[i], ts)
				}
			}
		})
	}
}

// TestSliceAliasesReceiver pins Slice's documented aliasing contract.
func TestSliceAliasesReceiver(t *testing.T) {
	t.Parallel()
	table, err := NewTable(scrambledEvents())
	if err != nil {
		t.Fatalf("NewTable: %v", err)
	}
	window, err := table.Slice(10, 20)
	if err != nil {
		t.Fatalf("Slice: %v", err)
	}
	window.Host[0] = "mutated"
	if table.Host[0] != "mutated" {
		t.Fatalf("table.Host[0] = %q after mutating the slice window, want %q", table.Host[0], "mutated")
	}
}

// TestNaiveSingleColumnSortDesyncsPairing is the heart of the module: it
// pins the exact defect a single-column sort produces, so a future edit
// that reintroduces it fails here instead of silently swapping which host a
// metric value belongs to in production.
func TestNaiveSingleColumnSortDesyncsPairing(t *testing.T) {
	t.Parallel()
	events := scrambledEvents() // ts=30/host=c, ts=10/host=a, ts=20/host=b
	naiveTS, naiveHost, _ := sortByTimestampOnlyNaive(events)
	if naiveTS[0] != 10 {
		t.Fatalf("naiveTS[0] = %d, want 10 (the ts column itself sorts correctly)", naiveTS[0])
	}
	if naiveHost[0] != "c" {
		t.Fatalf("naiveHost[0] = %q, want %q -- if this changes to %q the desync this test exists to catch was fixed by accident", naiveHost[0], "c", "a")
	}
	table, err := NewTable(events)
	if err != nil {
		t.Fatalf("NewTable: %v", err)
	}
	if table.TS[0] != 10 || table.Host[0] != "a" {
		t.Fatalf("Table row 0 = (%d,%s), want (10,a)", table.TS[0], table.Host[0])
	}
}

func TestRun(t *testing.T) {
	t.Parallel()
	const input = `{"ts":30,"host":"c","value":3}
{"ts":10,"host":"a","value":1}
{"ts":20,"host":"b","value":2}
`
	tests := []struct {
		name    string
		args    []string
		input   string
		want    string
		wantErr bool
	}{
		{name: "full default range sorted", input: input, want: "10\ta\t1\n20\tb\t2\n30\tc\t3\n"},
		{name: "half-open window", args: []string{"-from", "10", "-to", "30"}, input: input, want: "10\ta\t1\n20\tb\t2\n"},
		{name: "empty input produces no rows", input: "", want: ""},
		{name: "malformed json line is a usage error", input: "{not json}\n", wantErr: true},
		{name: "empty host is a usage error", input: `{"ts":1,"host":"","value":1}` + "\n", wantErr: true},
		{name: "from after to is a usage error", args: []string{"-from", "30", "-to", "10"}, input: input, wantErr: true},
		{name: "unknown flag is a usage error", args: []string{"-bogus"}, input: input, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var stdout bytes.Buffer
			err := run(tc.args, strings.NewReader(tc.input), &stdout)
			if tc.wantErr {
				if !errors.Is(err, errUsage) {
					t.Fatalf("run(%v) error = %v, want it to wrap errUsage", tc.args, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("run(%v): %v", tc.args, err)
			}
			if stdout.String() != tc.want {
				t.Fatalf("run(%v) stdout = %q, want %q", tc.args, stdout.String(), tc.want)
			}
		})
	}
}
```

## Review

`NewTable` is correct when its single `sort.Sort` call leaves every column
correctly paired with the row it started attached to; `TestNewTable`'s
first case checks that directly, and `TestNaiveSingleColumnSortDesyncsPairing`
pins the alternative failure mode where a single-column `slices.Sort` call
sorts the timestamps but silently leaves every other column's row
attribution wrong. The mechanism that prevents it is `Table` implementing
`sort.Interface` directly: `sort.Sort` only ever calls `Less` and `Swap`,
so as long as `Swap` moves all three columns together, no sequence of
comparisons and swaps the algorithm chooses can pull them apart. `Slice`
layers the same half-open interval discipline this lesson uses throughout
on top of the sorted `TS` column, and its result aliases the receiver,
which is what `TestSliceAliasesReceiver` pins. `run` maps every
input-driven failure -- a bad flag, a bad line of JSON, an empty host,
`from > to` -- to exit code 2 by wrapping `errUsage`, reserving exit code 1
for a runtime failure this tool never produces once its input has
validated. Run `go test -count=1 -race ./...`.

## Resources

- [`sort.Interface`](https://pkg.go.dev/sort#Interface) — the three-method interface `Table` implements so `Swap` can move every column together.
- [`sort.Sort`](https://pkg.go.dev/sort#Sort) — the algorithm that drives `Table` purely through `Less` and `Swap`, never touching a column slice directly.
- [`sort.Search`](https://pkg.go.dev/sort#Search) — the lower-bound primitive `Slice` uses to locate both ends of the half-open window.
- [Apache Arrow columnar format](https://arrow.apache.org/docs/format/Columnar.html) — the struct-of-arrays layout `Table` mirrors, used across modern analytics engines for cache-friendly column scans.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [19-order-book-price-level-insert.md](19-order-book-price-level-insert.md) | Next: [../13-implementing-a-ring-buffer/00-concepts.md](../13-implementing-a-ring-buffer/00-concepts.md)
