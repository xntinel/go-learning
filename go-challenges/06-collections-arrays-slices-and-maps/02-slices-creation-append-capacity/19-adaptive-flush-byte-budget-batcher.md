# Exercise 19: A Batcher That Flushes on a Byte Budget, Not a Record Count

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Producer clients for systems like Kafka, or any writer batching rows toward
a size-limited network frame or disk block, cannot simply flush every N
records: records vary in encoded size, and the constraint that actually
matters is wire or disk *bytes* per batch, not how many logical records
happen to fit. This module builds that batcher as a command-line tool: it
reads newline-delimited JSON from stdin, appends each record's bytes into
one reusable buffer, and flushes as soon as adding the next record would
exceed a fixed byte budget. Getting the boundary condition, the case of a
single record bigger than the whole budget, and the buffer's capacity
behavior across many flush cycles all correct is what separates a batcher
that is merely "close enough" from one that is safe to run in production
with a hard downstream size limit.

The `Batcher` type is the reusable core: `New` validates its configuration
and returns an error, `Add` and `Flush` never let a single oversized record
push the reusable buffer's capacity past the budget. The naive reset that
throws the buffer away on every flush instead of keeping it for reuse is not
part of that API. It lives in the test file, where it belongs, as the thing
the tests prove wrong.

This module is fully self-contained: its own `go mod init`, an executable
tool, and its tests. Nothing here imports another exercise.

## What you'll build

```text
bytebatcher/                  module example.com/bytebatcher
  go.mod                       go 1.24
  bytebatcher.go                 FlushFunc, sentinel errors, Batcher; New, Add, Flush, Len, Cap
  bytebatcher_test.go            byte-boundary table, oversized record, cap retention, naive-reset
                                 contrast, run() end to end
  main.go                        flags, NDJSON on stdin, exit codes
```

- Files: `bytebatcher.go`, `bytebatcher_test.go`, `main.go`.
- Implement: `Batcher` holding a reusable `[]byte` buffer and a byte `budget`; `New(budget int, flush FlushFunc) (*Batcher, error)` rejecting a non-positive budget with `ErrInvalidBudget` and a `nil` flush with `ErrNilFlushFunc`; `Add(record []byte)` appending into the buffer, flushing the pending batch first whenever the addition would exceed `budget`, and flushing an oversized record (`len(record) > budget`) directly, bypassing the buffer entirely; `Flush()` sending the pending bytes through `flush` and resetting with `buf = buf[:0]` to keep the backing array for reuse.
- Tool: reads NDJSON from stdin, one JSON value per line; a `-budget` flag sets the byte budget (default 64); each flushed batch is written to stdout with a header line, and a rejected line or flag reports the offending input to stderr. Exit codes: 0 on success, 2 for a bad flag or an unparsable JSON line, 1 for a failure reading stdin itself.
- Test: a table of record sequences asserting exactly which batches get flushed, including a record that lands the running total exactly on the budget without an early flush; an oversized record flushes alone without inflating the reusable buffer's capacity; 21 fill-then-flush cycles assert `Cap()` stays constant after the first flush establishes it; a manual `Flush()` with nothing pending is a no-op; a `naiveFlushReset` contrast proving that resetting to `nil` on every flush allocates every cycle while `buf = buf[:0]` allocates none; and `TestRunEndToEnd` driving `run` over a `strings.Reader` and `bytes.Buffer`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/bytebatcher
cd ~/go-exercises/bytebatcher
go mod init example.com/bytebatcher
go mod edit -go=1.24
```

### Why the trigger is a byte sum, not a record count, and why buf[:0] matters

A count-based batcher ("flush every 100 records") is the wrong tool whenever
records have variable, unpredictable size: 100 tiny records might total a
few hundred bytes, well under a network frame limit, while 100 records that
happen to include a handful of large payloads can blow well past it. The
byte-budget batcher tracks `len(buf)` directly and checks, before every
`Add`, whether appending the next record would push that total over
`budget`. If it would, the *pending* batch is flushed first — sending
exactly what fits under the budget — and the new record starts a fresh
batch. A record landing exactly on the budget (`len(buf) + len(record) ==
budget`) is deliberately allowed to join the current batch rather than
triggering an early flush; only strictly exceeding the budget forces one,
which keeps every flushed batch as large as the budget allows instead of
flushing one byte earlier than necessary.

A record whose own length already exceeds `budget` cannot ever fit inside
any batch, no matter how empty the buffer is, so it needs its own path: flush
whatever is pending first (preserving output order), then hand the oversized
record straight to `flush` without ever appending it into `buf`. Routing it
through the buffer instead would force `append` to grow `buf`'s backing
array past `budget` to fit it, and that larger capacity would stick around
for every batch after it, since `buf = buf[:0]` reuses whatever backing
array is currently there. That reuse is also the whole point of the reset:
`buf[:0]` keeps the *same* array and its existing capacity for the next
batch, so a long-running batcher settles into one steady-state allocation
instead of allocating a fresh buffer on every single flush cycle. The
oversized-record path exists specifically to protect that steady state from
being knocked upward by one large outlier. The tempting alternative — reset
with `buf = nil` after every flush instead of `buf = buf[:0]` — throws that
steady state away: the very next `Add` has to grow a brand-new array from
nothing, every single cycle, forever.

Create `bytebatcher.go`:

```go
// Command bytebatcher reads newline-delimited JSON records from stdin and
// writes them to stdout grouped into batches bounded by a byte budget, not a
// record count.
package main

import (
	"errors"
	"fmt"
)

// ErrInvalidBudget means New was called with a non-positive budget.
var ErrInvalidBudget = errors.New("bytebatcher: budget must be positive")

// ErrNilFlushFunc means New was called with a nil FlushFunc.
var ErrNilFlushFunc = errors.New("bytebatcher: flush function must not be nil")

// FlushFunc receives a completed batch's bytes. The slice is a view into the
// Batcher's internal buffer and is only valid for the call, exactly like the
// contract of io.Writer.Write: a FlushFunc that needs to keep the bytes past
// the call must copy them.
type FlushFunc func(batch []byte)

// Batcher appends encoded records into a reusable buffer and flushes when
// the total buffered size would exceed a fixed byte budget -- not when a
// record count is reached, the shape of a Kafka-style producer batcher or a
// WAL segment writer, where wire or disk bytes per flush is the constraint
// that matters.
//
// A Batcher is not safe for concurrent use; it is meant to be driven by a
// single reader loop, exactly as run does in main.go.
type Batcher struct {
	buf    []byte
	budget int
	flush  FlushFunc
}

// New returns a Batcher that sends completed batches to flush whenever the
// buffered bytes would exceed budget. It returns ErrInvalidBudget if budget
// is not positive, or ErrNilFlushFunc if flush is nil.
func New(budget int, flush FlushFunc) (*Batcher, error) {
	if budget <= 0 {
		return nil, fmt.Errorf("%w: got %d", ErrInvalidBudget, budget)
	}
	if flush == nil {
		return nil, ErrNilFlushFunc
	}
	return &Batcher{budget: budget, flush: flush}, nil
}

// Add appends record's bytes to the current batch. If record alone exceeds
// the budget, any pending batch is flushed first (to preserve ordering) and
// record is then flushed by itself, never touching the reusable buffer -- so
// one huge record cannot force the buffer's capacity past the budget.
// Otherwise, if appending record would push the buffered size over budget,
// the pending batch is flushed first so record starts a fresh one.
func (b *Batcher) Add(record []byte) {
	if len(record) > b.budget {
		b.Flush()
		b.flush(record)
		return
	}
	if len(b.buf)+len(record) > b.budget {
		b.Flush()
	}
	b.buf = append(b.buf, record...)
}

// Flush sends any pending buffered bytes to the FlushFunc and resets the
// buffer with buf = buf[:0], which keeps the same backing array (and its
// capacity) for the next batch instead of allocating a new one. Flush is a
// no-op if nothing is buffered.
func (b *Batcher) Flush() {
	if len(b.buf) == 0 {
		return
	}
	b.flush(b.buf)
	b.buf = b.buf[:0]
}

// Len reports the number of bytes currently buffered, pending the next
// flush.
func (b *Batcher) Len() int { return len(b.buf) }

// Cap reports the buffer's current backing-array capacity.
func (b *Batcher) Cap() int { return cap(b.buf) }
```

### The tool

`main.go` is deliberately thin: it owns flag parsing, wiring stdin/stdout,
and choosing an exit code, and delegates every actual decision to `run`,
which takes `args []string`, an `io.Reader`, and an `io.Writer` instead of
reaching for `os.Args`/`os.Stdin`/`os.Stdout` directly. That is what makes
`run` testable end to end over a `strings.Reader` and a `bytes.Buffer`
without ever spawning a process. `run` scans stdin line by line with
`bufio.Scanner`, so the tool streams records instead of loading the whole
input into memory — the same property `Batcher` itself was built around.
Each line is validated as JSON with `encoding/json.Valid` and copied into a
fresh slice before it ever reaches `Add`, because `scanner.Bytes()` is only
valid until the next `Scan` call — the same pooled-buffer retention hazard
this lesson's concepts cover. A bad flag or an unparsable line is wrapped
with the sentinel `errBadInput`, which `main` checks with `errors.Is` to
choose exit code 2; any other error (a failure reading stdin itself) exits
1; success exits 0.

Create `main.go`:

```go
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

// errBadInput marks a failure caused by how the tool was invoked or shaped
// (a bad flag, an unparsable NDJSON line), as opposed to a runtime failure.
// main uses it to choose exit code 2 instead of 1.
var errBadInput = errors.New("bytebatcher: bad input")

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		if errors.Is(err, errBadInput) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

// run parses flags, batches the NDJSON records read from stdin by byte
// budget, and writes each flushed batch to stdout. It is the entire
// testable surface of the tool; main only wires it to the real os.Args,
// os.Stdin, and os.Stdout and translates its error into an exit code.
func run(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("bytebatcher", flag.ContinueOnError)
	budget := fs.Int("budget", 64, "maximum bytes buffered per batch")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: bytebatcher -budget=N < records.ndjson")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %v", errBadInput, err)
	}

	batchNum := 0
	flushFn := func(batch []byte) {
		batchNum++
		records := bytes.Count(batch, []byte("\n"))
		fmt.Fprintf(stdout, "--- batch %d: %d bytes, %d records ---\n", batchNum, len(batch), records)
		stdout.Write(batch)
	}
	b, err := New(*budget, flushFn)
	if err != nil {
		return fmt.Errorf("%w: %v", errBadInput, err)
	}

	// scanner.Bytes() is only valid until the next Scan -- the pooled-buffer
	// retention hazard this lesson's concepts cover -- so every line is
	// copied into a fresh slice before it ever reaches the Batcher.
	scanner := bufio.NewScanner(stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	line := 0
	for scanner.Scan() {
		line++
		raw := scanner.Bytes()
		if !json.Valid(raw) {
			return fmt.Errorf("%w: line %d: not valid JSON: %q", errBadInput, line, raw)
		}
		record := make([]byte, len(raw)+1)
		copy(record, raw)
		record[len(raw)] = '\n'
		b.Add(record)
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("bytebatcher: reading stdin: %w", err)
	}
	b.Flush()
	fmt.Fprintf(stdout, "--- total: %d batches ---\n", batchNum)
	return nil
}
```

Run it:

```bash
printf '{"id":1}\n{"id":2}\n{"id":3,"payload":"a-fairly-long-value-here"}\n{"id":4}\n{"id":5}\n{"id":6}\n' | go run . -budget=24
```

Expected output:

```text
--- batch 1: 18 bytes, 2 records ---
{"id":1}
{"id":2}
--- batch 2: 46 bytes, 1 records ---
{"id":3,"payload":"a-fairly-long-value-here"}
--- batch 3: 18 bytes, 2 records ---
{"id":4}
{"id":5}
--- batch 4: 9 bytes, 1 records ---
{"id":6}
--- total: 4 batches ---
```

Records 1 and 2 (9 bytes each) sum to 18, under the 24-byte budget, and
batch together. The 46-byte record with the long payload is itself well
over budget, so batch 1 flushes first to preserve ordering and the oversized
record then flushes alone as batch 2, never touching the reusable buffer.
Records 4 and 5 refill the same buffer to demonstrate `buf = buf[:0]` in
action, and the trailing `b.Flush()` after the scan loop sends record 6 as
the final partial batch. Feeding the tool a line that fails
`encoding/json.Valid` — for example `printf '{"id":1}\nnot-json\n' | go run
. -budget=24` — instead prints `bytebatcher: bad input: line 2: not valid
JSON: "not-json"` to stderr and exits 2.

### Tests

`TestNewRejectsInvalidConfig` covers `New`'s two validation paths.
`TestFlushBoundariesByByteSize` is the table: the ordinary batches, and the
tightest boundary case, a record that lands the running total precisely on
the budget and must *not* trigger an early flush — that is where an
off-by-one (`>=` instead of `>`) would flush one batch earlier than
necessary. `TestOversizedRecordFlushesAlone` checks record ordering around
an oversized record and confirms the buffer's capacity is completely
unaffected by it. `TestCapRetentionAcrossFlushes` runs 21 fill-then-flush
cycles and asserts `Cap()` never changes after the first flush establishes
it. `TestManualFlushSendsPendingBytes` covers the explicit end-of-stream
`Flush()` call, including that a second call with nothing pending is a
no-op.

`TestNaiveResetAllocatesEveryCycleWhileFlushDoesNot` is the contrast test at
the heart of the module: it drives 50 fill-then-flush cycles through a real
`Batcher` at steady state and through the unexported `naiveFlushReset`
helper, and asserts the real path allocates exactly zero per cycle while the
naive one allocates on every cycle — an every-call allocation, so
`testing.AllocsPerRun`'s average lands safely above its 1-per-run rounding
floor (contrast Exercise 16, where a periodic-not-every-call allocation
needed a raw `runtime.MemStats` comparison instead). `TestRunEndToEnd`
drives `run` over a `strings.Reader` and `bytes.Buffer` and checks the exact
stdout produced. `TestRunRejectsBadInput` tables three ways `run` can fail
with exit code 2: an invalid JSON line, an unparsable flag value, and a
non-positive budget.

Create `bytebatcher_test.go`:

```go
package main

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"
)

// recorder collects flushed batches as independent copies, matching the
// FlushFunc contract that the batch slice is only valid for the call.
type recorder struct{ batches [][]byte }

func (r *recorder) flush(batch []byte) {
	r.batches = append(r.batches, append([]byte(nil), batch...))
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	t.Parallel()

	if _, err := New(0, func([]byte) {}); !errors.Is(err, ErrInvalidBudget) {
		t.Errorf("New(0, ...) error = %v, want ErrInvalidBudget", err)
	}
	if _, err := New(10, nil); !errors.Is(err, ErrNilFlushFunc) {
		t.Errorf("New(10, nil) error = %v, want ErrNilFlushFunc", err)
	}
}

func TestFlushBoundariesByByteSize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		budget  int
		records []string
		want    []string // flushed batches, in order; excludes a trailing manual Flush
	}{
		{"fills the budget without flushing early", 6, []string{"ab", "cd", "ef"}, nil},
		{"one more byte over budget flushes the prior batch", 6, []string{"ab", "cd", "ef", "g"}, []string{"abcdef"}},
		{"a record landing exactly on the budget does not flush early", 10, []string{"ab", "cd", "ef", "ghij"}, nil},
		{"several boundary crossings flush once per crossing", 4, []string{"ab", "cd", "ef", "gh", "ij"}, []string{"abcd", "efgh"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rec := &recorder{}
			b, err := New(tc.budget, rec.flush)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			for _, r := range tc.records {
				b.Add([]byte(r))
			}
			got := make([]string, len(rec.batches))
			for i, batch := range rec.batches {
				got[i] = string(batch)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("flushed batches = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestOversizedRecordFlushesAlone(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	b, err := New(5, rec.flush)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	b.Add([]byte("ab")) // 2 bytes pending, under budget
	capBefore := b.Cap()
	b.Add([]byte("OVERSIZED-PAYLOAD")) // 18 bytes, alone over the 5-byte budget
	capAfter := b.Cap()
	b.Add([]byte("cd")) // starts a fresh batch after the oversized one
	b.Flush()
	got := make([]string, len(rec.batches))
	for i, batch := range rec.batches {
		got[i] = string(batch)
	}
	if want := []string{"ab", "OVERSIZED-PAYLOAD", "cd"}; !slices.Equal(got, want) {
		t.Fatalf("flushed batches = %v, want %v", got, want)
	}
	// The oversized record must bypass the reusable buffer entirely, so it
	// cannot force buf's capacity up to fit its 18 bytes.
	if capAfter != capBefore {
		t.Errorf("buffer capacity changed from %d to %d after an oversized Add, want it untouched", capBefore, capAfter)
	}
}

func TestCapRetentionAcrossFlushes(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	b, err := New(8, rec.flush)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	b.Add([]byte("abcdefgh")) // fills the budget exactly
	b.Flush()
	firstCap := b.Cap()
	if firstCap == 0 {
		t.Fatal("expected a non-zero capacity to be retained after the first flush")
	}
	for i := 0; i < 20; i++ {
		b.Add([]byte("abcdefgh"))
		b.Flush()
		if b.Cap() != firstCap {
			t.Fatalf("iteration %d: Cap() = %d, want stable %d (buf = buf[:0] must reuse the array)", i, b.Cap(), firstCap)
		}
	}
	if len(rec.batches) != 21 {
		t.Fatalf("got %d flushed batches, want 21", len(rec.batches))
	}
}

func TestManualFlushSendsPendingBytes(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	b, err := New(100, rec.flush)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	b.Add([]byte("tail"))
	b.Flush()
	if len(rec.batches) != 1 || string(rec.batches[0]) != "tail" {
		t.Fatalf("after manual Flush, batches = %v, want [\"tail\"]", rec.batches)
	}
	b.Flush() // a second Flush with nothing pending must be a no-op
	if len(rec.batches) != 1 {
		t.Fatalf("Flush with nothing pending sent an extra batch: %v", rec.batches)
	}
}

// naiveFlushReset is the reset every batcher is tempted to write first: hand
// the pending bytes to flush, then restart from a fresh nil slice instead of
// the one already sized to the steady-state batch. Unexported, lives only
// here, and throws away exactly what Flush's buf = buf[:0] keeps.
func naiveFlushReset(buf []byte, flush FlushFunc) []byte {
	if len(buf) == 0 {
		return buf
	}
	flush(buf)
	return nil
}

// TestNaiveResetAllocatesEveryCycleWhileFlushDoesNot is the contrast test at
// the heart of the module. At steady state, a real Batcher's fill-then-flush
// cycle allocates nothing: buf = buf[:0] reuses the same backing array. The
// naive reset allocates on every cycle, since restarting from nil forces
// append to build a fresh array each time. This test does not call
// t.Parallel: testing.AllocsPerRun panics if it runs from a parallel test.
func TestNaiveResetAllocatesEveryCycleWhileFlushDoesNot(t *testing.T) {
	noop := func([]byte) {}
	data := []byte("abcdefgh")
	b, err := New(8, noop)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	b.Add(data)
	b.Flush() // establish steady-state capacity
	realAllocs := testing.AllocsPerRun(50, func() {
		b.Add(data)
		b.Flush()
	})
	var naiveBuf []byte
	naiveAllocs := testing.AllocsPerRun(50, func() {
		naiveBuf = append(naiveBuf, data...)
		naiveBuf = naiveFlushReset(naiveBuf, noop)
	})
	if realAllocs != 0 {
		t.Fatalf("Batcher fill-then-flush allocated %v per cycle at steady state, want 0", realAllocs)
	}
	if !(naiveAllocs > realAllocs) {
		t.Fatalf("allocations: naive = %v, real = %v; want naive > real", naiveAllocs, realAllocs)
	}
}

func TestRunEndToEnd(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	if err := run([]string{"-budget=20"}, strings.NewReader("{\"id\":1}\n{\"id\":2}\n{\"id\":3}\n"), &stdout); err != nil {
		t.Fatalf("run: %v", err)
	}
	want := "--- batch 1: 18 bytes, 2 records ---\n" +
		"{\"id\":1}\n{\"id\":2}\n" +
		"--- batch 2: 9 bytes, 1 records ---\n" +
		"{\"id\":3}\n" +
		"--- total: 2 batches ---\n"
	if stdout.String() != want {
		t.Fatalf("stdout =\n%q\nwant\n%q", stdout.String(), want)
	}
}

func TestRunRejectsBadInput(t *testing.T) {
	t.Parallel()

	tests := []struct{ name, args0, stdin string }{
		{"invalid JSON line", "-budget=64", "{\"id\":1}\nnot-json\n"},
		{"unparsable flag value", "-budget=notanumber", ""},
		{"non-positive budget", "-budget=0", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var stdout bytes.Buffer
			err := run([]string{tc.args0}, strings.NewReader(tc.stdin), &stdout)
			if !errors.Is(err, errBadInput) {
				t.Fatalf("run error = %v, want errBadInput", err)
			}
		})
	}
}
```

## Review

The batcher is correct when the flushed batches, in order, are exactly what
the byte budget dictates — the boundary table pins the tightest case, a
record that lands the running total precisely on the budget. `New` rejects
an invalid budget or a nil `FlushFunc` with sentinel errors checkable via
`errors.Is`. The oversized-record path is correct when it neither loses
ordering nor inflates the reusable buffer, which
`TestOversizedRecordFlushesAlone` checks directly. The steady-state
promise — one allocation, reused forever — is what
`TestCapRetentionAcrossFlushes` and the naive-reset contrast both exist for:
`buf = buf[:0]` is what makes repeated flush cycles settle onto one capacity
instead of allocating fresh storage every cycle, proven not just by
observation but by a direct allocation-count comparison against the reset
every batcher is tempted to write first. The tool wraps every bad flag and
unparsable NDJSON line in `errBadInput` for exit code 2, reserves exit code
1 for a genuine failure reading stdin, and streams records through
`bufio.Scanner` rather than buffering the whole input, copying each scanned
line before it can be overwritten by the next `Scan` call. Run
`go test -count=1 -race ./...` to confirm.

## Resources

- [Go blog: Arrays, slices (and strings): The mechanics of 'append'](https://go.dev/blog/slices) — how `s[:0]` retains capacity while append controls growth.
- [`io.Writer`](https://pkg.go.dev/io#Writer) — the "valid only for the call" contract `FlushFunc` mirrors.
- [`bufio.Scanner`](https://pkg.go.dev/bufio#Scanner) — the streaming line reader, and why `Bytes()` is only valid until the next `Scan`.
- [Kafka producer batching (`batch.size` / `linger.ms`)](https://kafka.apache.org/documentation/#producerconfigs_batch.size) — a real system that flushes producer batches by byte size, not record count.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [18-copy-on-write-snapshot-publisher.md](18-copy-on-write-snapshot-publisher.md) | Next: [../03-slice-expressions-and-sub-slicing/00-concepts.md](../03-slice-expressions-and-sub-slicing/00-concepts.md)
