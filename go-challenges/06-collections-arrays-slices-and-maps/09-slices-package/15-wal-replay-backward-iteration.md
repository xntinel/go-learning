# Exercise 15: Replay A WAL Backward Without Mutating The Forward Order

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A write-ahead log's crash-recovery path -- the same shape Postgres and etcd
both use -- reads its record slice forward to redo everything since the last
checkpoint. Right alongside it sits a second, less common consumer: a rollback
inspector, or an audit tool, that needs to walk the exact same in-memory
slice in the opposite direction, most-recent-record-first, to explain what an
undo would touch. Both consumers are looking at the same `[]Record`. Neither
one is supposed to be able to see the other's pass happen.

The tempting way to write the backward pass is `slices.Reverse(records)`
followed by an ordinary forward loop. It is also the one operation in this
pairing that is not allowed here: `Reverse` reverses the slice's backing
array in place, which means every other variable that shares that array --
including whatever holds the original slice for the forward redo pass --
is now looking at the reversed order too, permanently, with no signal that
anything changed. `slices.Backward`, added to the package alongside `slices.All`
in Go 1.23, walks the slice back to front and changes nothing: it is a
non-mutating iterator over the same memory, built for exactly this situation,
where reading in a different order must not be the same thing as writing in
a different order.

This module builds `wal-replay`, a tool that reads a small in-memory WAL,
then prints a forward redo pass with `slices.All` immediately followed by a
reverse rollback pass with `slices.Backward` over the identical slice value
-- proof by construction that the second pass never touched what the first
one already walked.

This module is fully self-contained: its own `go mod init`, an executable
command, and its tests. Nothing here imports another exercise.

## What you'll build

```text
wal-replay/                    module example.com/wal-replay
  go.mod                       go 1.24
  wal.go                       package main — Record, ParseRecords, WriteForwardRedo (All),
                                WriteReverseRollback (Backward)
  wal_test.go                  package main — parse table, both passes, non-mutation, the
                                Reverse-corruption contrast, run() end to end
  main.go                      package main — reads stdin, exit codes
```

- Files: `wal.go`, `wal_test.go`, `main.go`.
- Implement: `ParseRecords(r io.Reader) ([]Record, error)` reading `"<lsn> <op>"` lines, skipping blank lines, rejecting an unparseable line with `ErrMalformedRecord`; `WriteForwardRedo(w io.Writer, records []Record)` printing one numbered line per record using `slices.All`; `WriteReverseRollback(w io.Writer, records []Record)` printing one numbered line per record, most recent first, using `slices.Backward`.
- Tool: `wal-replay < records.log` reads every record from stdin, then writes the forward redo pass immediately followed by the reverse rollback pass to stdout. A malformed record line exits 2; a clean run exits 0.
- Test: the parse table (ordinary input, blank lines skipped, empty input, a bad LSN, a missing op); both passes' exact numbered output, including the empty and single-record edge cases; that neither pass mutates the slice it was given; the naive `slices.Reverse`-then-walk contrast corrupting a second holder of the same slice; `run` end to end over `strings.Reader` and `bytes.Buffer`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/09-slices-package/15-wal-replay-backward-iteration
cd go-solutions/06-collections-arrays-slices-and-maps/09-slices-package/15-wal-replay-backward-iteration
go mod edit -go=1.24
```

### Reading backward is not the same operation as writing backward

`slices.All(s)` and `slices.Backward(s)` are both `iter.Seq2[int, E]`
producers: they hand a `for i, v := range ...` loop an index and a value,
front to back for `All`, back to front for `Backward`, and neither one
touches `s` itself. `slices.Reverse(s)`, by contrast, is not an iterator at
all -- it is a mutation, documented the same way `Sort` is: it reorders the
elements `s` points at, in place, and returns nothing, because there is
nothing new to return. The rollback pass only needs to *read* the records in
reverse order; it never needed to *reorder* them. Reaching for `Reverse`
where `Backward` was the right tool trades a borrow for an ownership claim
the code never meant to make:

```go
func rollbackNaive(records []Record) []string {
    slices.Reverse(records)             // records is now permanently reversed
    var lines []string
    for i, r := range records {
        lines = append(lines, fmt.Sprintf("rollback %d: lsn=%d op=%s", i+1, r.LSN, r.Op))
    }
    return lines
}
```

Call `rollbackNaive(records)` and every other variable that shares `records`'s
backing array -- the forward redo pass, a caller that logged the slice
earlier, a test fixture built once and reused -- now iterates it back to
front too, silently, because a slice header does not carry a record of
"someone reversed the array I point at". `slices.Backward` cannot do this: it
has no way to write to `s`, only to read it in the order the loop asks for.

Create `wal.go`:

```go
// Command wal-replay reads a write-ahead log's in-memory record slice and
// walks it twice: once forward, for the redo pass a crash-recovery routine
// runs, and once backward, for a rollback inspector that must see the exact
// reverse order without disturbing the forward order some other consumer of
// the same slice still relies on.
//
// See the package tests for why the backward pass uses slices.Backward
// rather than slices.Reverse: Reverse mutates the slice's backing array in
// place, so any other holder of that same slice -- including the forward
// pass, if it runs after the backward one -- would see it permanently
// reversed.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
)

// ErrMalformedRecord means a non-blank input line did not parse as
// "<lsn> <op>".
var ErrMalformedRecord = errors.New("wal-replay: malformed record, want \"<lsn> <op>\"")

// Record is one write-ahead log entry: its log sequence number and the
// operation it describes.
type Record struct {
	LSN int
	Op  string
}

// ParseRecords reads one record per line from r as "<lsn> <op>", where op
// may itself contain spaces. Blank lines are skipped. A non-blank line
// without a parseable integer LSN and a following operation is rejected
// with ErrMalformedRecord wrapping the 1-based line number.
//
// ParseRecords reads r to completion; it does not retain r.
func ParseRecords(r io.Reader) ([]Record, error) {
	var records []Record
	sc := bufio.NewScanner(r)
	line := 0
	for sc.Scan() {
		line++
		text := sc.Text()
		if text == "" {
			continue
		}
		lsnText, op, ok := strings.Cut(text, " ")
		if !ok || op == "" {
			return nil, fmt.Errorf("%w: line %d: %q", ErrMalformedRecord, line, text)
		}
		lsn, err := strconv.Atoi(lsnText)
		if err != nil {
			return nil, fmt.Errorf("%w: line %d: %q", ErrMalformedRecord, line, text)
		}
		records = append(records, Record{LSN: lsn, Op: op})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("wal-replay: reading input: %w", err)
	}
	return records, nil
}

// WriteForwardRedo writes one numbered line per record to w, in the order
// records already has -- the redo pass a crash-recovery routine runs. It
// uses slices.All purely as a non-mutating (index, value) iterator; it does
// not modify records.
func WriteForwardRedo(w io.Writer, records []Record) {
	for i, r := range slices.All(records) {
		fmt.Fprintf(w, "redo %d: lsn=%d op=%s\n", i+1, r.LSN, r.Op)
	}
}

// WriteReverseRollback writes one numbered line per record to w in reverse
// order -- rollback step 1 is the most recently applied record, matching
// how an undo pass actually proceeds. It walks records with slices.Backward,
// which reads the slice back to front without mutating it, so records is
// exactly as it was before this call once WriteReverseRollback returns.
func WriteReverseRollback(w io.Writer, records []Record) {
	n := len(records)
	for i, r := range slices.Backward(records) {
		fmt.Fprintf(w, "rollback %d: lsn=%d op=%s\n", n-i, r.LSN, r.Op)
	}
}
```

### The tool

`wal-replay` reads every record from stdin -- there is no file-argument mode,
since the point of this tool is the two passes over one in-memory slice, not
file handling -- and `run` takes the argument slice, the input reader, and an
`io.Writer` for stdout so it can be driven entirely from a test without a real
terminal or process. A malformed line is something the caller fixes by
correcting the log, so `ParseRecords` returning `ErrMalformedRecord` is
wrapped in `errUsage` and mapped to exit code 2; exit code 1 is reserved by
convention for a stdin read failure, which none of the runs below trigger.
The two passes run one after the other over the exact same `records` value
`ParseRecords` returned -- `WriteForwardRedo` first, `WriteReverseRollback`
second -- and because neither one mutates it, that ordering is not load-bearing:
running them in the other order would print the same two blocks.

Create `main.go`:

```go
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

// errUsage marks a malformed input record: something the caller fixes by
// correcting the log. main maps it to exit code 2. errRuntime marks a
// failure reading stdin itself; main maps it to exit code 1, though this
// tool never hits it in the runs demonstrated below.
var (
	errUsage   = errors.New("invalid input")
	errRuntime = errors.New("runtime failure")
)

// run reads every record from stdin, then writes the forward redo pass
// followed by the reverse rollback pass to stdout. It never touches
// os.Exit, so it can be exercised in a test with a strings.Reader and a
// bytes.Buffer.
func run(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("wal-replay", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %w", errUsage, err)
	}

	records, err := ParseRecords(stdin)
	if err != nil {
		if errors.Is(err, ErrMalformedRecord) {
			return fmt.Errorf("%w: %w", errUsage, err)
		}
		return fmt.Errorf("%w: %w", errRuntime, err)
	}

	WriteForwardRedo(stdout, records)
	WriteReverseRollback(stdout, records)
	return nil
}

func main() {
	flag.CommandLine.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: wal-replay < records.log")
		fmt.Fprintln(os.Stderr, "reads \"<lsn> <op>\" lines from stdin and prints a forward")
		fmt.Fprintln(os.Stderr, "redo pass followed by a reverse rollback pass.")
	}
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "wal-replay:", err)
		if errors.Is(err, errRuntime) {
			os.Exit(1)
		}
		os.Exit(2)
	}
}
```

Run it:

```bash
printf '1 insert users\n2 update users\n3 delete users\n' | go run .
printf '1 insert users\nnot-a-record\n' | go run .
```

Expected output:

```text
redo 1: lsn=1 op=insert users
redo 2: lsn=2 op=update users
redo 3: lsn=3 op=delete users
rollback 1: lsn=3 op=delete users
rollback 2: lsn=2 op=update users
rollback 3: lsn=1 op=insert users
wal-replay: invalid input: wal-replay: malformed record, want "<lsn> <op>": line 2: "not-a-record"
```

The first run's redo block is `records.log` in its own order; the rollback
block underneath it is the same three records, most recent first --
`lsn=3` (the last one applied) rolls back before `lsn=1`. The second run
feeds a second line with no space in it, which `ParseRecords` rejects before
either pass runs, naming the exact line, and exits 2.

### Tests

`TestParseRecords` is the table for the reader, including the two ways a
line can fail to parse: no space at all, and a non-integer LSN.
`TestWriteForwardRedoAndReverseRollback` pins the exact numbered output of
both passes over the same three-record slice, then asserts the slice is
still equal to a clone taken before either pass ran --the property this
whole module exists to guarantee. `TestForwardAndReverseOverEmptyAndSingleRecord`
covers the two edge cases a fixed-length slice loop always needs: zero
records (both passes print nothing) and one record (both passes agree on
what "the order" even means for a single element).

`TestNaiveReverseCorruptsForwardOrder` is the module's core test.
`rollbackNaive` is unexported and unreachable from the tool: it is the
`slices.Reverse`-then-walk sequence from the prose above. The test gives it
a slice while a second variable, `forwardConsumer`, holds the exact same
slice header the way a forward-pass consumer would, calls `rollbackNaive`,
and shows `forwardConsumer` now reads back to front even though nothing ever
assigned to it directly. It then repeats the scenario through the real
`WriteReverseRollback` and shows the slice is untouched.

Create `wal_test.go`:

```go
package main

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func records(n int) []Record {
	out := make([]Record, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, Record{LSN: i, Op: fmt.Sprintf("op%d", i)})
	}
	return out
}

func TestParseRecords(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    []Record
		wantErr error
	}{
		{"three records", "1 insert users\n2 update users\n3 delete users\n",
			[]Record{{1, "insert users"}, {2, "update users"}, {3, "delete users"}}, nil},
		{"blank lines are skipped", "1 insert\n\n2 update\n",
			[]Record{{1, "insert"}, {2, "update"}}, nil},
		{"empty input yields no records", "", nil, nil},
		{"non-integer lsn is malformed", "x insert\n", nil, ErrMalformedRecord},
		{"missing op is malformed", "1\n", nil, ErrMalformedRecord},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseRecords(strings.NewReader(tc.input))
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("ParseRecords error = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseRecords: unexpected error: %v", err)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("ParseRecords = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestWriteForwardRedoAndReverseRollback(t *testing.T) {
	t.Parallel()

	recs := records(3)
	before := slices.Clone(recs)

	var forward bytes.Buffer
	WriteForwardRedo(&forward, recs)
	wantForward := "redo 1: lsn=1 op=op1\nredo 2: lsn=2 op=op2\nredo 3: lsn=3 op=op3\n"
	if forward.String() != wantForward {
		t.Fatalf("forward = %q, want %q", forward.String(), wantForward)
	}

	var reverse bytes.Buffer
	WriteReverseRollback(&reverse, recs)
	wantReverse := "rollback 1: lsn=3 op=op3\nrollback 2: lsn=2 op=op2\nrollback 3: lsn=1 op=op1\n"
	if reverse.String() != wantReverse {
		t.Fatalf("reverse = %q, want %q", reverse.String(), wantReverse)
	}

	// Neither pass may have mutated the slice the caller still holds.
	if !slices.Equal(recs, before) {
		t.Fatalf("records mutated by the read-only passes: got %+v, want %+v", recs, before)
	}
}

func TestForwardAndReverseOverEmptyAndSingleRecord(t *testing.T) {
	t.Parallel()

	for _, n := range []int{0, 1} {
		recs := records(n)
		var forward, reverse bytes.Buffer
		WriteForwardRedo(&forward, recs)
		WriteReverseRollback(&reverse, recs)
		if n == 0 {
			if forward.Len() != 0 || reverse.Len() != 0 {
				t.Fatalf("n=0: forward=%q reverse=%q, want both empty", forward.String(), reverse.String())
			}
			continue
		}
		if forward.String() != "redo 1: lsn=1 op=op1\n" {
			t.Fatalf("n=1: forward = %q", forward.String())
		}
		if reverse.String() != "rollback 1: lsn=1 op=op1\n" {
			t.Fatalf("n=1: reverse = %q", reverse.String())
		}
	}
}

// rollbackNaive is the shortcut a rollback inspector reaches for instead of
// slices.Backward: reverse the slice in place, then walk it forward. It is
// not part of the tool; it exists only to show what slices.Reverse costs a
// caller who is not the only one holding this slice. Unlike slices.All and
// slices.Backward, slices.Reverse mutates its argument's backing array, so
// the reversal is visible to every other variable that shares it.
func rollbackNaive(records []Record) []string {
	slices.Reverse(records)
	lines := make([]string, 0, len(records))
	for i, r := range records {
		lines = append(lines, fmt.Sprintf("rollback %d: lsn=%d op=%s", i+1, r.LSN, r.Op))
	}
	return lines
}

// TestNaiveReverseCorruptsForwardOrder is the heart of the module. It shows
// slices.Reverse mutating a WAL record slice that a separate consumer --
// the forward redo pass, in a real recovery routine -- still relies on
// seeing in forward order. WriteReverseRollback, built on slices.Backward,
// leaves the same slice untouched.
func TestNaiveReverseCorruptsForwardOrder(t *testing.T) {
	t.Parallel()

	recs := records(4)
	original := slices.Clone(recs)

	// Some other part of the recovery routine -- the forward redo pass --
	// still holds this same slice header and expects forward order.
	forwardConsumer := recs

	_ = rollbackNaive(recs)

	if slices.Equal(forwardConsumer, original) {
		t.Fatal("forwardConsumer unexpectedly still in forward order; rollbackNaive must reverse in place")
	}
	want := slices.Clone(original)
	slices.Reverse(want)
	if !slices.Equal(forwardConsumer, want) {
		t.Fatalf("forwardConsumer = %+v, want the reversed order %+v", forwardConsumer, want)
	}

	// Contrast: WriteReverseRollback never mutates the slice at all.
	recs2 := records(4)
	original2 := slices.Clone(recs2)
	var buf bytes.Buffer
	WriteReverseRollback(&buf, recs2)
	if !slices.Equal(recs2, original2) {
		t.Fatalf("WriteReverseRollback mutated records: got %+v, want %+v", recs2, original2)
	}
}

func TestRun(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		stdin   string
		want    string
		wantErr bool
	}{
		{
			name:  "two records forward then reverse",
			stdin: "1 insert\n2 update\n",
			want: "redo 1: lsn=1 op=insert\n" +
				"redo 2: lsn=2 op=update\n" +
				"rollback 1: lsn=2 op=update\n" +
				"rollback 2: lsn=1 op=insert\n",
		},
		{
			name:  "empty log produces empty output",
			stdin: "",
			want:  "",
		},
		{
			name:    "malformed line is rejected",
			stdin:   "1 insert\nnot-a-record\n",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var stdout bytes.Buffer
			err := run(nil, strings.NewReader(tc.stdin), &stdout)
			if tc.wantErr {
				if !errors.Is(err, errUsage) {
					t.Fatalf("run error = %v, want it to wrap errUsage", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("run: unexpected error: %v", err)
			}
			if stdout.String() != tc.want {
				t.Fatalf("run stdout = %q, want %q", stdout.String(), tc.want)
			}
		})
	}
}
```

## Review

`wal-replay` is correct when its forward and reverse blocks both number
correctly and, together, never leave the input slice in a different state
than `ParseRecords` handed back -- `TestWriteForwardRedoAndReverseRollback`
checks both the exact output and that non-mutation directly. The trap this
module is built around is `slices.Reverse`: it is the right tool when the
goal really is to reorder a slice permanently, and the wrong one the moment
another part of the program still expects the original order, because it
mutates the shared backing array with no signal to anyone else holding that
same slice. `TestNaiveReverseCorruptsForwardOrder` proves that cost directly
against a second variable standing in for a forward-pass consumer.
`slices.All` and `slices.Backward` are the fix precisely because they are
iterators, not mutations: they hand a loop values in a chosen order without
ever writing through the slice they read. Exit codes separate a malformed
record, which the caller fixes by correcting the log, from a stdin read
failure that no run here triggers. Run `go test -count=1 -race ./...` to
confirm the parse table, both passes' output and non-mutation, the
`slices.Reverse` contrast, and `run`'s end-to-end behavior.

## Resources

- [`slices.Backward`](https://pkg.go.dev/slices#Backward) — the non-mutating reverse iterator this module's rollback pass is built on.
- [`slices.All`](https://pkg.go.dev/slices#All) — the non-mutating forward `(index, value)` iterator used for the redo pass.
- [`slices.Reverse`](https://pkg.go.dev/slices#Reverse) — the in-place mutation this module deliberately does not use for a read-only pass.
- [The Go Blog: Range over function types](https://go.dev/blog/range-functions) — the `iter.Seq2` mechanics behind `All` and `Backward`.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [14-shard-slot-table-repeat-sentinel.md](14-shard-slot-table-repeat-sentinel.md) | Next: [16-kafka-segment-prefix-compaction-delete.md](16-kafka-segment-prefix-compaction-delete.md)
