# Exercise 13: Compacted Log Tombstones: Delete Versus Explicit Empty Value

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A Kafka log-compacted topic keeps, for every key, only its most recent
record; a write-ahead log backing a key-value store does the same thing on
recovery. Both need a way to say "delete this key" inside a format whose
only payload is a key and a value, and both settle on the same convention:
a `null` value marks a tombstone, and compaction is free to drop that key
once every older record for it has been superseded. A record with a value
that merely has zero length is not a tombstone -- it is a legitimate write of
an empty value, and a compactor that cannot tell the two apart deletes data
its own log never asked it to delete.

That distinction maps directly onto a fact about Go's slice representation
this whole lesson has been building toward: `[]byte(nil)` and `[]byte{}`
both have length zero, and only one of them is nil. A compactor whose
tombstone check is `len(value) == 0` is checking the wrong property. It
happens to work for every tombstone, because a tombstone's value genuinely
has zero length, but it also fires for every legitimate empty write, because
an empty write has zero length too. The correct check, `value == nil`,
distinguishes a value the producer meant to be absent from a value the
producer meant to be present and empty -- and only Go's nil-vs-empty-slice
distinction, carried faithfully from parsing through to the compaction
decision, makes that check possible at all.

This module builds `compactlog`, a command-line tool that reads a
tab-separated key/value log from stdin, compacts it in memory, and writes
the surviving records to stdout -- keeping a nil tombstone and a non-nil
empty value distinct at every step from parsing to output.

This module is fully self-contained: its own `go mod init`, an executable
command, and its tests. Nothing here imports another exercise.

## What you'll build

```text
compactlog/                    module example.com/compactlog
  go.mod                       go 1.24
  compactlog.go                package main — Record, Compactor; ParseLine, NewCompactor,
                                Apply, Compacted; one sentinel error
  compactlog_test.go           package main — ParseLine table, latest-wins compaction,
                                the buggy-compactor contrast, run() end to end
  main.go                      package main — -tombstones flag, streaming stdin, exit codes
```

- Files: `compactlog.go`, `compactlog_test.go`, `main.go`.
- Implement: `ParseLine(line string) (Record, error)` decoding a bare key as a tombstone (`Value` nil) and a `key\tvalue` line as a live record (`Value` always non-nil, even when empty), returning `ErrMalformedLine` for an empty line or an empty key; `Compactor` with `NewCompactor() *Compactor`, `(*Compactor).Apply(rec Record)` folding one record into the running state, and `(*Compactor).Compacted() (live []Record, tombstones int)` returning survivors in first-seen-key order.
- Tool: `compactlog` streams stdin line by line, writes compacted `key\tvalue` pairs to stdout in first-seen-key order, and with `-tombstones` set also prints the deleted-key count to stderr. Exit 0 on success, exit 2 for a malformed line or an unrecognized flag, exit 1 is reserved for a runtime failure this tool never actually produces.
- Test: `ParseLine` over a bare key, a `key\tvalue` line, a trailing-tab empty value, an empty line, and an empty key; latest-state-wins compaction in first-seen-key order, including the all-tombstone case; the buggy-compactor contrast; and `run` end to end over `strings.Reader` and two `bytes.Buffer`s.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/compactlog
cd ~/go-exercises/compactlog
go mod init example.com/compactlog
go mod edit -go=1.24
```

### Carrying nil all the way from a tab character to a compaction decision

`ParseLine` makes the nil-vs-empty distinction the moment it sees the line,
and every downstream piece of logic just has to preserve it rather than
re-derive it. A line with no tab at all is a tombstone by definition -- there
is no value to record, so `Value` stays nil. A line with a tab always
produces a live record, and critically, `ParseLine` never lets the value
slice come out nil for that branch, even when there is nothing after the
tab:

```go
raw := line[i+1:]
value := make([]byte, len(raw)) // make guarantees a non-nil slice, even at length 0
copy(value, raw)
```

`make([]byte, 0)` is non-nil by construction -- it is exactly the fact the
`00-concepts.md` lesson opens with, that an empty slice built with `make` or
a composite literal has a real, if zero-size, backing array. Using `make`
here instead of a bare slice expression is what guarantees a trailing-tab
line (`"key\t"`) produces `Record{Value: []byte{}}` rather than accidentally
producing `Record{Value: nil}` and colliding with the tombstone case one
line up.

`Compactor.Compacted` then makes its keep-or-drop decision on exactly the
property `ParseLine` preserved:

```go
if rec.Value == nil {
	tombstones++
	continue
}
live = append(live, rec)
```

The naive version of this check, explored in the test file rather than in
the tool's own logic, asks `len(rec.Value) == 0` instead. That question is
true for a tombstone and true for an explicit empty write alike, so the
naive compactor drops both -- a customer's deliberate "clear this field to
empty string" write vanishes on the very next compaction pass, indistin-
guishable in the output from a key that was never written at all.

Create `compactlog.go`:

```go
package main

import (
	"errors"
	"fmt"
	"strings"
)

// ErrMalformedLine is returned by ParseLine when a line cannot be decoded
// into a Record.
var ErrMalformedLine = errors.New("compactlog: malformed line")

// Record is one decoded line of the compaction log. A nil Value marks a
// tombstone ("delete this key"); a non-nil Value, even length zero, marks a
// live write -- the distinction this package exists to preserve.
type Record struct {
	Key   string
	Value []byte
}

// ParseLine decodes one tab-separated line. No tab means a tombstone (Value
// nil). A tab splits key from value; the value is always a non-nil slice,
// even when empty, so a live empty write is never mistaken for a tombstone.
// It returns ErrMalformedLine for an empty line or an empty key.
func ParseLine(line string) (Record, error) {
	if line == "" {
		return Record{}, fmt.Errorf("%w: empty line", ErrMalformedLine)
	}
	i := strings.IndexByte(line, '\t')
	if i == -1 {
		return Record{Key: line, Value: nil}, nil
	}
	key := line[:i]
	if key == "" {
		return Record{}, fmt.Errorf("%w: empty key", ErrMalformedLine)
	}
	raw := line[i+1:]
	value := make([]byte, len(raw)) // make guarantees a non-nil slice, even at length 0
	copy(value, raw)
	return Record{Key: key, Value: value}, nil
}

// Compactor retains, for each key, only the most recently applied Record,
// as a Kafka-style compacted topic or a WAL-backed store does.
//
// Compactor is not safe for concurrent use; a single goroutine must own it.
type Compactor struct {
	order []string
	seen  map[string]int
	state map[string]Record
}

// NewCompactor returns an empty Compactor ready to receive records in
// arrival order.
func NewCompactor() *Compactor {
	return &Compactor{
		seen:  make(map[string]int),
		state: make(map[string]Record),
	}
}

// Apply folds one record into the running state. A later Apply for a key
// already seen replaces its earlier state; the key's first-seen position
// never changes.
func (c *Compactor) Apply(rec Record) {
	if _, ok := c.seen[rec.Key]; !ok {
		c.seen[rec.Key] = len(c.order)
		c.order = append(c.order, rec.Key)
	}
	c.state[rec.Key] = rec
}

// Compacted returns the live records in first-seen-key order, plus the
// count of keys whose latest state is a tombstone -- Value == nil, never
// merely len(Value) == 0, which is what keeps a delete distinct from a live
// write of an explicit empty value. The returned slice is never nil.
func (c *Compactor) Compacted() (live []Record, tombstones int) {
	live = make([]Record, 0, len(c.order))
	for _, k := range c.order {
		rec := c.state[k]
		if rec.Value == nil {
			tombstones++
			continue
		}
		live = append(live, rec)
	}
	return live, tombstones
}
```

### The tool

`run` takes the argument slice and separate `io.Reader`/`io.Writer` values
for stdin, stdout, and stderr, so it never touches `os.Args`, `os.Stdin`, or
`os.Exit` and can be driven end to end from a table test with a
`strings.Reader` and two `bytes.Buffer`s. It streams the input with
`bufio.Scanner` one line at a time rather than reading it all into memory
first, because a compaction log is exactly the kind of input that can be
arbitrarily large in production while this tool only ever needs to hold the
current state per key. Every failure `run` can produce -- an unrecognized
flag or a malformed line -- is something the caller fixes by changing the
input or the command line, so both wrap sentinels (`errUsage` or
`ErrMalformedLine`) that `main` maps to exit code 2; exit code 1 is defined
by convention for a runtime failure, but nothing past flag parsing and line
decoding can fail in this tool, so it is never actually returned.

Create `main.go`:

```go
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

// errUsage marks a failure the caller can fix by changing the command line:
// an unrecognized flag. main maps it, and any error wrapping
// ErrMalformedLine, to exit code 2; every other error maps to exit code 1,
// a code this tool never actually returns since nothing past flag parsing
// and line decoding can fail.
var errUsage = errors.New("usage")

// run streams tab-separated key/value lines from stdin line by line with
// bufio.Scanner, compacts them, and writes survivors to stdout in
// first-seen-key order. It never touches os.Args, os.Stdin, or os.Exit, so
// it is driven in tests with a strings.Reader and two bytes.Buffers. A
// malformed line aborts the run wrapping ErrMalformedLine with its 1-based
// line number. With -tombstones set, run reports the deleted-key count on
// stderr after a successful compaction.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("compactlog", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	reportTombstones := fs.Bool("tombstones", false, "report the deleted-key count on stderr")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}

	c := NewCompactor()
	scanner := bufio.NewScanner(stdin)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		rec, err := ParseLine(scanner.Text())
		if err != nil {
			return fmt.Errorf("line %d: %w", lineNum, err)
		}
		c.Apply(rec)
	}
	if err := scanner.Err(); err != nil {
		return err
	}

	live, tombstones := c.Compacted()
	w := bufio.NewWriter(stdout)
	for _, rec := range live {
		fmt.Fprintf(w, "%s\t%s\n", rec.Key, rec.Value)
	}
	if err := w.Flush(); err != nil {
		return err
	}

	if *reportTombstones {
		fmt.Fprintf(stderr, "tombstones: %d\n", tombstones)
	}
	return nil
}

func main() {
	flag.CommandLine.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: compactlog [-tombstones] < log.tsv")
		fmt.Fprintln(os.Stderr, "compacts tab-separated key/value lines from stdin, keeping only the")
		fmt.Fprintln(os.Stderr, "latest record per key; a bare key with no tab deletes it.")
	}
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "compactlog:", err)
		if errors.Is(err, errUsage) || errors.Is(err, ErrMalformedLine) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}
```

Run it:

```bash
printf 'user:1\tactive\nuser:2\tactive\nuser:1\nuser:3\t\nuser:2\tactive\n' | go run . -tombstones
printf 'a\tone\n\nb\ttwo\n' | go run .
```

Expected output:

```text
user:2	active
user:3	
tombstones: 1
compactlog: line 2: compactlog: malformed line: empty line
```

The first command's log writes `user:1` active, then `user:2` active, then
deletes `user:1` with a bare-key tombstone, then writes `user:3` with an
explicit empty value, then rewrites `user:2` to the same value again. The
compacted output keeps `user:2=active` and `user:3=` (an empty but present
value, printed as a key followed by a tab and nothing else) in first-seen
order, drops `user:1` entirely, and `-tombstones` reports the one deletion
on stderr. The second command's log has a blank line as its second entry --
neither a valid tombstone nor a valid `key\tvalue` pair -- and `run` stops
there, reporting the 1-based line number in the error and exiting 2.

### Tests

`TestParseLine` is the table that anchors the whole module: a bare key
decodes to a nil-valued tombstone, a `key\tvalue` line decodes to a live
record, a trailing-tab line decodes to a live record with a non-nil *empty*
value, and both an empty line and an empty key are rejected with
`ErrMalformedLine`. `TestCompactorLatestStateWinsInFirstSeenOrder` replays a
short log through `Apply` and checks both the survivor list and the
all-tombstone edge case, where `Compacted` must still return a non-nil empty
slice.

`TestBuggyCompactionDropsExplicitEmptyValue` is the heart of the module.
`compactBuggy` is unexported and unreachable from `Compactor`; it exists so
the test can pin the exact defect it ships -- for a single record carrying
an explicit empty value, `compactBuggy` misreads it as a tombstone and
drops it, while `Compactor.Apply` and `Compacted` keep it as a live record
with a non-nil empty `Value`.

`TestRun` drives the whole tool end to end: a mixed log compacting
correctly, `-tombstones` reporting the right count on stderr, a malformed
line producing an error that wraps `ErrMalformedLine`, and an unrecognized
flag producing an error that wraps `errUsage`.

Create `compactlog_test.go`:

```go
package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// compactBuggy treats any zero-length value as a delete, whether nil (a
// tombstone) or a non-nil empty slice (an explicit empty write). Never
// exported or reachable from Compactor: it exists only so the tests can pin
// the defect it ships, a legitimate empty write vanishing on compaction.
func compactBuggy(records []Record) (live []Record, tombstones int) {
	latest := map[string]Record{}
	var order []string
	for _, rec := range records {
		if _, ok := latest[rec.Key]; !ok {
			order = append(order, rec.Key)
		}
		latest[rec.Key] = rec
	}
	for _, k := range order {
		if rec := latest[k]; len(rec.Value) == 0 { // bug: nil and non-nil-empty both look deleted
			tombstones++
		} else {
			live = append(live, rec)
		}
	}
	return live, tombstones
}

func TestParseLine(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		line      string
		wantKey   string
		wantValue string // "" with wantTombstone=false still means a non-nil empty value
		wantTomb  bool
		wantErr   bool
	}{
		{name: "bare key is a tombstone", line: "user:1", wantKey: "user:1", wantTomb: true},
		{name: "key and value", line: "user:2\tactive", wantKey: "user:2", wantValue: "active"},
		{name: "trailing tab is an explicit empty value", line: "user:3\t", wantKey: "user:3", wantValue: ""},
		{name: "empty line is malformed", line: "", wantErr: true},
		{name: "empty key is malformed", line: "\tvalue", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rec, err := ParseLine(tc.line)
			if tc.wantErr {
				if !errors.Is(err, ErrMalformedLine) {
					t.Fatalf("ParseLine(%q) error = %v, want ErrMalformedLine", tc.line, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseLine(%q): %v", tc.line, err)
			}
			if rec.Key != tc.wantKey {
				t.Fatalf("ParseLine(%q).Key = %q, want %q", tc.line, rec.Key, tc.wantKey)
			}
			if tc.wantTomb {
				if rec.Value != nil {
					t.Fatalf("ParseLine(%q).Value = %v, want nil (tombstone)", tc.line, rec.Value)
				}
				return
			}
			if rec.Value == nil {
				t.Fatalf("ParseLine(%q).Value = nil, want a non-nil slice", tc.line)
			}
			if string(rec.Value) != tc.wantValue {
				t.Fatalf("ParseLine(%q).Value = %q, want %q", tc.line, rec.Value, tc.wantValue)
			}
		})
	}
}

func TestCompactorLatestStateWinsInFirstSeenOrder(t *testing.T) {
	t.Parallel()

	c := NewCompactor()
	lines := []string{
		"user:1\tactive",
		"user:2\tactive",
		"user:1", // tombstone: overrides the earlier "active"
		"user:3\t",
		"user:2\tactive",
	}
	for _, line := range lines {
		rec, err := ParseLine(line)
		if err != nil {
			t.Fatalf("ParseLine(%q): %v", line, err)
		}
		c.Apply(rec)
	}

	live, tombstones := c.Compacted()
	if tombstones != 1 {
		t.Fatalf("tombstones = %d, want 1", tombstones)
	}
	if len(live) != 2 {
		t.Fatalf("live = %+v, want 2 records", live)
	}
	if live[0].Key != "user:2" || string(live[0].Value) != "active" {
		t.Fatalf("live[0] = %+v, want user:2=active", live[0])
	}
	if live[1].Key != "user:3" || string(live[1].Value) != "" || live[1].Value == nil {
		t.Fatalf("live[1] = %+v, want user:3 with a non-nil empty value", live[1])
	}

	// Deleting everything must still yield a non-nil, empty slice.
	allGone := NewCompactor()
	allGone.Apply(Record{Key: "a", Value: nil})
	goneLive, goneTomb := allGone.Compacted()
	if goneTomb != 1 || goneLive == nil || len(goneLive) != 0 {
		t.Fatalf("all-tombstone Compacted() = live:%+v tombstones:%d, want empty non-nil live and 1 tombstone", goneLive, goneTomb)
	}
}

// TestBuggyCompactionDropsExplicitEmptyValue is the heart of the module: an
// empty write must survive compaction; compactBuggy drops it instead.
func TestBuggyCompactionDropsExplicitEmptyValue(t *testing.T) {
	t.Parallel()

	records := []Record{
		{Key: "session:9", Value: []byte{}}, // explicit empty write, not a delete
	}

	buggyLive, buggyTomb := compactBuggy(records)
	if len(buggyLive) != 0 || buggyTomb != 1 {
		t.Fatalf("compactBuggy = live:%+v tombstones:%d, want the empty write to be misread as a delete", buggyLive, buggyTomb)
	}

	c := NewCompactor()
	for _, rec := range records {
		c.Apply(rec)
	}
	live, tombstones := c.Compacted()
	if tombstones != 0 {
		t.Fatalf("Compactor tombstones = %d, want 0: an explicit empty value is not a delete", tombstones)
	}
	if len(live) != 1 || live[0].Key != "session:9" || live[0].Value == nil {
		t.Fatalf("Compactor live = %+v, want one live record with a non-nil empty value", live)
	}
}

func TestRun(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		args       []string
		input      string
		wantStdout string
		wantStderr string
		wantErr    bool
	}{
		{
			name:       "compacts and keeps first-seen order",
			args:       nil,
			input:      "a\tone\nb\ttwo\na\ttombstone-me\na\n",
			wantStdout: "b\ttwo\n",
		},
		{
			name:       "reports tombstone count when requested",
			args:       []string{"-tombstones"},
			input:      "a\tone\na\n",
			wantStdout: "",
			wantStderr: "tombstones: 1\n",
		},
		{
			name:    "malformed line is a usage-level failure",
			args:    nil,
			input:   "a\tone\n\nb\ttwo\n",
			wantErr: true,
		},
		{
			name:    "unknown flag is a usage failure",
			args:    []string{"-bogus"},
			input:   "",
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var stdout, stderr bytes.Buffer
			err := run(tc.args, strings.NewReader(tc.input), &stdout, &stderr)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("run(%v): want error, got nil", tc.args)
				}
				if !errors.Is(err, errUsage) && !errors.Is(err, ErrMalformedLine) {
					t.Fatalf("run(%v) error = %v, want it to wrap errUsage or ErrMalformedLine", tc.args, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("run(%v): %v", tc.args, err)
			}
			if stdout.String() != tc.wantStdout {
				t.Fatalf("run(%v) stdout = %q, want %q", tc.args, stdout.String(), tc.wantStdout)
			}
			if stderr.String() != tc.wantStderr {
				t.Fatalf("run(%v) stderr = %q, want %q", tc.args, stderr.String(), tc.wantStderr)
			}
		})
	}
}
```

## Review

The compactor is correct when a nil-valued record is dropped and a
non-nil-empty-valued record survives -- `TestParseLine`'s trailing-tab case
and `TestBuggyCompactionDropsExplicitEmptyValue` together pin the exact
boundary. The mechanism is refusing to let `len(value) == 0` stand in for
"this is a delete": only `value == nil` means that, and getting there
requires `ParseLine` to construct a genuinely non-nil slice for every live
value, even an empty one, with `make` rather than a slice expression that
might collapse to nil. Around that core, `Apply` keeps first-seen key order
so the output is deterministic and readable, `Compacted` never returns a
nil slice even when every key was ultimately deleted, and `run` streams the
input line by line rather than buffering the whole log, reserving exit code
2 for a malformed line or a bad flag and exit code 1 for a runtime failure
this particular tool structurally cannot produce. Run
`go test -count=1 -race ./...`.

## Resources

- [Kafka documentation: Log Compaction](https://kafka.apache.org/documentation/#compaction) — the tombstone convention this module models, including the "null value marks a delete" rule.
- [`bufio.Scanner`](https://pkg.go.dev/bufio#Scanner) — the line-by-line streaming reader `run` uses instead of buffering the whole input.
- [`make`](https://go.dev/ref/spec#Making_slices_maps_and_channels) — why `make([]byte, 0)` is guaranteed non-nil, the fact `ParseLine` relies on.
- [`errors.Is`](https://pkg.go.dev/errors#Is) — how `main` and the tests distinguish `ErrMalformedLine` and `errUsage` from every other error.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [12-reconciler-nil-empty-equality-diff.md](12-reconciler-nil-empty-equality-diff.md) | Next: [14-acl-decision-cache-tristate.md](14-acl-decision-cache-tristate.md)
