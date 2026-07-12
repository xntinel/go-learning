# Exercise 14: An NDJSON Batch Redactor That Clears Its Reused Buffer

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Log-shipping agents -- Fluent Bit, Vector, a homegrown sidecar reading access
logs off stdin -- do not allocate a fresh slice for every batch they forward.
On a long-running stream, allocating and discarding a `[]*Entry` per batch
would put constant pressure on the garbage collector for no reason, so the
standard move is to keep one buffer and reuse it: append records into it,
flush it, reset its length to zero, append the next batch into the same
backing array. That reuse is exactly right for the array's *capacity*. It is
exactly wrong if the reset is only `batch = batch[:0]`, because shrinking a
slice's length changes nothing about what the Go garbage collector considers
reachable through it.

The mechanism is the same one this lesson's subscriber-registry module
proved for a single removed element, now at the scale of an entire batch: the
GC does not scan a slice by its current length, it scans the backing array by
its *allocated capacity* -- every pointer-sized slot in that array is a
potential live pointer as long as the array itself is reachable through any
header, regardless of what any one header's `len` claims. A tool that
processes a burst of one thousand records, batches of one thousand at a time,
and then settles into a steady state of ten records per batch, keeps every
one of the peak batch's thousand `*Entry` pointers reachable through the
reused array's unused tail forever after -- and each of those entries can
carry a multi-kilobyte raw header block, exactly the kind of data a redaction
tool exists to keep off disk and out of memory it does not need. The fix is
the `clear` builtin: it zeroes every element of a slice up to its current
length in one call, and calling it on the *full* batch right before resetting
the length is what actually drops those pointers, instead of merely hiding
them behind a smaller `len`.

This module builds `redact`, a command that reads NDJSON access-log records
from stdin and writes redacted summaries to stdout, batching internally with
a reused buffer that clears itself correctly. The `batch = batch[:0]`-only
reset is not part of that tool; it exists only in the test file, as the thing
the tests measure and reject.

This module is fully self-contained: its own `go mod init`, an executable
command, and its tests. Nothing here imports another exercise.

## What you'll build

```text
redact/                   module example.com/redact
  go.mod                  go 1.24
  redact.go                package main -- Entry, Summary, Batcher; NewBatcher, Add, Flush, Len
  redact_test.go           package main -- batching table, the clear-vs-no-clear contrast via
                           MemStats, run() end to end
  main.go                  package main -- -batch flag, stdin/stdout streaming, exit codes
```

- Files: `redact.go`, `redact_test.go`, `main.go`.
- Implement: `NewBatcher(size int) (*Batcher, error)` rejecting a non-positive size with `ErrInvalidBatchSize`; `(*Batcher).Add(e *Entry) (full bool)` queuing a pointer and reporting when the batch has reached its configured size; `(*Batcher).Flush(w io.Writer) error` writing one redacted `Summary` per queued entry, `clear`-ing the buffer, and resetting its length for reuse; `(*Batcher).Len() int`.
- Tool: `redact` reads NDJSON access-log records from stdin, one JSON object per line, and writes one redacted `Summary` per line to stdout as each batch fills (and any partial batch at end of input). `-batch N` sets the flush size (default 100). Exit 0 on success, exit 2 for an unknown flag, an invalid `-batch` value, or a line that fails to parse as JSON (all usage/invalid-input errors), exit 1 is reserved for a runtime failure such as a write error to stdout.
- Test: `Batcher` construction and fullness reporting; `Flush` writing exactly the redacted fields and never `RawHeaders`, and leaving the batch empty and reusable; the clear-vs-no-clear contrast -- a naive reset that only shrinks length retains every entry's raw headers, `Flush`'s `clear` call does not; and `run` end to end over a `strings.Reader` and a `bytes.Buffer`, covering batching boundaries and every usage-error path.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Shrinking length is not the same operation as releasing what it held

`b.entries = b.entries[:0]` and `clear(b.entries); b.entries = b.entries[:0]`
produce headers that look identical from the outside -- same pointer, same
zero length, same capacity. The difference is entirely in what the backing
array contains after each one runs:

```go
// The trap: length reset alone, no clear.
func (b *naiveBatcher) flushBuggy() {
    b.entries = b.entries[:0]   // len 0, but the array still holds every pointer
}
```

Picture a batch of 1000 during a traffic spike, followed by ten batches of
10 during steady state. After the spike's `flushBuggy`, the array's first
1000 slots still point at 1000 `*Entry` values. The next ten batches of 10
only overwrite slots 0 through 9 each time -- slots 10 through 999 are never
touched again, because `append` only ever writes starting at the current
`len`, and `len` never exceeds 10 from here on. Those 990 stale pointers, and
every raw header string they hold, are reachable for as long as the
`Batcher` itself runs -- which, for a log-shipping agent, is the lifetime of
the process. `Flush`'s actual reset is one call earlier in the sequence:

```go
func (b *Batcher) Flush(w io.Writer) error {
    // ... write every entry as a redacted Summary ...
    clear(b.entries)        // zero all len(b.entries) slots -- the whole spike batch
    b.entries = b.entries[:0]
    return nil
}
```

`clear` walks every element up to the *current* length -- which, at the
moment `Flush` runs, is however big that batch actually was, spike or not --
and sets each one to its zero value. For `[]*Entry`, that zero value is
`nil`, so every one of those 1000 pointers is gone from the array the moment
this `Flush` returns, regardless of how small the next batch happens to be.

Create `redact.go`:

```go
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// ErrInvalidBatchSize means NewBatcher was called with a non-positive size.
var ErrInvalidBatchSize = errors.New("redact: batch size must be positive")

// Entry is one raw NDJSON access-log record as read from the input stream.
// RawHeaders may carry cookies, bearer tokens, or other sensitive data and
// must never reach stdout.
type Entry struct {
	Path       string `json:"path"`
	Status     int    `json:"status"`
	RawHeaders string `json:"raw_headers"`
}

// Summary is the redacted projection written to stdout: everything except
// RawHeaders.
type Summary struct {
	Path   string `json:"path"`
	Status int    `json:"status"`
}

// Batcher accumulates *Entry records up to a fixed size and flushes them as
// redacted Summaries, reusing its internal buffer across flushes -- the
// pattern a log-shipping agent (Fluent Bit, Vector) uses to avoid a fresh
// allocation for every batch on a long-running stream.
//
// Batcher is not safe for concurrent use.
type Batcher struct {
	entries []*Entry
	size    int
}

// NewBatcher returns a Batcher that reports itself full once it holds size
// entries. It returns ErrInvalidBatchSize if size is not positive.
func NewBatcher(size int) (*Batcher, error) {
	if size <= 0 {
		return nil, fmt.Errorf("%w: got %d", ErrInvalidBatchSize, size)
	}
	return &Batcher{entries: make([]*Entry, 0, size), size: size}, nil
}

// Add appends e to the current batch and reports whether the batch has
// reached its configured size and is ready for Flush.
//
// Add does not copy e: the Batcher retains the pointer until the batch
// containing it is flushed, so the caller must not mutate *e afterward.
func (b *Batcher) Add(e *Entry) (full bool) {
	b.entries = append(b.entries, e)
	return len(b.entries) >= b.size
}

// Len reports how many entries are queued in the current batch.
func (b *Batcher) Len() int { return len(b.entries) }

// Flush writes one redacted Summary per queued entry to w, one JSON object
// per line, then empties the batch so the same backing array can be reused
// for the next one.
//
// Flush zeroes every slot with the clear builtin before shrinking the
// length back to zero. Without that, a batch smaller than an earlier one
// would leave the backing array's tail still holding *Entry pointers from
// the larger batch -- each one's RawHeaders can run to several kilobytes --
// reachable for as long as the Batcher itself lives, even though nothing
// will ever read them again: the Go garbage collector scans a slice's
// entire backing array by its allocated capacity, not by any one header's
// current length. clear removes every pointer the moment its batch is
// flushed, regardless of how the next batch's size compares to this one's.
func (b *Batcher) Flush(w io.Writer) error {
	enc := json.NewEncoder(w)
	for _, e := range b.entries {
		if err := enc.Encode(Summary{Path: e.Path, Status: e.Status}); err != nil {
			return fmt.Errorf("redact: writing summary: %w", err)
		}
	}
	clear(b.entries)
	b.entries = b.entries[:0]
	return nil
}
```

### The tool

`redact` streams: it reads one line at a time with `bufio.Scanner` rather
than loading the whole input into memory, because a real access log is
exactly the kind of arbitrarily large stream this lesson keeps coming back
to. `run` takes the argument slice, an `io.Reader` for stdin, and an
`io.Writer` for stdout, so it can be driven end to end in a test without
touching `os.Args`, `os.Stdin`, or `os.Exit`. Every failure `run` can
produce before it starts writing -- an unknown flag, an invalid `-batch`
value, a line that is not valid JSON -- is something the caller fixes by
changing the command line or the input, so all three wrap the `errUsage`
sentinel and `main` maps that to exit code 2. Exit code 1 is reserved for a
failure writing to stdout itself (a broken pipe, a full disk), which
`Batcher.Flush` reports as a plain wrapped error that does not satisfy
`errors.Is(err, errUsage)`.

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
	"os"
)

// errUsage marks a failure the caller can fix by changing the command line
// or the input: a bad flag, an invalid batch size, or a malformed JSON
// line. main maps it to exit code 2; every other error maps to exit code 1.
var errUsage = errors.New("usage")

// run reads NDJSON access-log records from stdin, batches them through a
// Batcher, and writes redacted summaries to stdout. It never touches
// os.Args or os.Exit, so it can be exercised in a test with a
// strings.Reader and a bytes.Buffer.
func run(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("redact", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	batchSize := fs.Int("batch", 100, "number of records per flush")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}

	b, err := NewBatcher(*batchSize)
	if err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}

	scanner := bufio.NewScanner(stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	line := 0
	for scanner.Scan() {
		line++
		var e Entry
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			return fmt.Errorf("%w: line %d: invalid JSON: %v", errUsage, line, err)
		}
		if b.Add(&e) {
			if err := b.Flush(stdout); err != nil {
				return err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("reading input: %w", err)
	}
	if b.Len() > 0 {
		if err := b.Flush(stdout); err != nil {
			return err
		}
	}
	return nil
}

func main() {
	flag.CommandLine.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: redact [-batch N] < access-log.ndjson")
		fmt.Fprintln(os.Stderr, "reads NDJSON access-log records from stdin, writes redacted summaries to stdout.")
	}
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "redact:", err)
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}
```

Run it:

```bash
printf '{"path":"/login","status":200,"raw_headers":"Cookie: session=abc123; Authorization: Bearer xyz"}\n{"path":"/health","status":204,"raw_headers":"User-Agent: kube-probe/1.29"}\n' | go run . -batch 2
printf 'not json\n' | go run . -batch 10
printf '' | go run . -batch 0
```

Expected output:

```text
{"path":"/login","status":200}
{"path":"/health","status":204}
```

```text
redact: usage: line 1: invalid JSON: invalid character 'o' in literal null (expecting 'u')
```

```text
redact: usage: redact: batch size must be positive: got 0
```

The first command feeds exactly one full batch of two records: `-batch 2`
means the buffer flushes as soon as the second record lands, and both
summaries reach stdout with `raw_headers` -- the field carrying the session
cookie and bearer token -- gone entirely. The second and third commands show
the exit-2 usage path: an unparseable JSON line and a rejected `-batch`
value both produce the `redact: usage: ...` prefix `main` prints for any
error wrapping `errUsage`.

### Tests

`TestNewBatcherRejectsNonPositiveSizeAndAddReportsFull` pins the
constructor's validation and the fullness signal `Add` returns.
`TestFlushWritesRedactedSummariesAndEmptiesBatch` checks the redacted
output field-by-field, confirms `RawHeaders` never appears in it, and
confirms a second `Flush` on the now-empty batch writes nothing.

`TestFlushBuggyRetainsStaleEntries` and `TestFlushReleasesStaleEntries` are
the heart of the module, reusing the `runtime.ReadMemStats` discipline from
Exercise 2: force GC twice, read `HeapAlloc`, compare a 4 MiB allocation's
delta (64 synthetic entries, 64 KiB of header data each) against a 2 MiB
threshold. `naiveBatcher` is an unexported test type whose `flushBuggy`
method resets the buffer to length zero without calling `clear` first --
never exported, never reachable from the package API -- and its test shows
the heap still carrying the full batch's data afterward. `Batcher.Flush`'s
test performs the identical setup and shows the heap dropping far below the
threshold, because `clear` removed every pointer before the length reset.
Neither test calls `t.Parallel`: `HeapAlloc` is process-global, so a
concurrently allocating test would perturb the reading.

`TestRun` drives the command end to end: batching boundaries (a batch that
fills exactly at end of input, a batch size of 1 flushing every line, an
empty input producing no output) and every usage-error path (an invalid
`-batch`, a malformed JSON line, an unknown flag), all asserted against
`errUsage` via `errors.Is`.

Create `redact_test.go`:

```go
package main

import (
	"bytes"
	"errors"
	"runtime"
	"strings"
	"testing"
)

// readHeap returns HeapAlloc after two full GC cycles. The second GC
// completes the sweep started by the first, so the reading is stable.
func readHeap() uint64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// fillString returns an n-byte string of a repeating pattern, standing in
// for a real request's raw header block.
func fillString(n int, seed byte) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i) + seed
	}
	return string(b)
}

func TestNewBatcherRejectsNonPositiveSizeAndAddReportsFull(t *testing.T) {
	t.Parallel()

	for _, s := range []int{0, -1} {
		if _, err := NewBatcher(s); !errors.Is(err, ErrInvalidBatchSize) {
			t.Errorf("NewBatcher(%d) error = %v, want ErrInvalidBatchSize", s, err)
		}
	}

	b, err := NewBatcher(2)
	if err != nil {
		t.Fatalf("NewBatcher: %v", err)
	}
	if full := b.Add(&Entry{Path: "/a"}); full {
		t.Fatal("Add 1/2 reported full")
	}
	if full := b.Add(&Entry{Path: "/b"}); !full {
		t.Fatal("Add 2/2 did not report full")
	}
	if got := b.Len(); got != 2 {
		t.Fatalf("Len() = %d, want 2", got)
	}
}

func TestFlushWritesRedactedSummariesAndEmptiesBatch(t *testing.T) {
	t.Parallel()

	b, err := NewBatcher(10)
	if err != nil {
		t.Fatalf("NewBatcher: %v", err)
	}
	b.Add(&Entry{Path: "/login", Status: 200, RawHeaders: "Authorization: Bearer secret"})
	b.Add(&Entry{Path: "/health", Status: 204, RawHeaders: "User-Agent: probe"})

	var out bytes.Buffer
	if err := b.Flush(&out); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	want := `{"path":"/login","status":200}` + "\n" + `{"path":"/health","status":204}` + "\n"
	if out.String() != want {
		t.Fatalf("Flush output = %q, want %q", out.String(), want)
	}
	if strings.Contains(out.String(), "secret") || strings.Contains(out.String(), "raw_headers") {
		t.Fatal("Flush output leaked RawHeaders")
	}
	if got := b.Len(); got != 0 {
		t.Fatalf("Len() after Flush = %d, want 0", got)
	}
	// Flushing again, now empty, must write nothing and not error.
	out.Reset()
	if err := b.Flush(&out); err != nil || out.Len() != 0 {
		t.Fatalf("Flush on empty batch: out=%q err=%v, want empty, nil", out.String(), err)
	}
}

// naiveBatcher mimics the batch-reuse mistake this module exists to avoid:
// it resets the buffer to length zero without clearing it first. It is
// never exported and never reachable from the package API.
type naiveBatcher struct {
	entries []*Entry
}

func (nb *naiveBatcher) add(e *Entry) { nb.entries = append(nb.entries, e) }

func (nb *naiveBatcher) flushBuggy() {
	nb.entries = nb.entries[:0] // no clear: stale pointers survive in the tail
}

// TestFlushBuggyRetainsStaleEntries is the core of this module: resetting a
// reused pointer slice to length zero without clearing it first leaves
// every *Entry -- and its multi-kilobyte RawHeaders -- reachable through
// the backing array's unused capacity, because the garbage collector scans
// a slice's whole backing array by its allocated capacity, not by any one
// header's current length.
//
// This test deliberately does not call t.Parallel: it forces GC and reads
// process-global heap stats, which a concurrently allocating goroutine
// would perturb.
func TestFlushBuggyRetainsStaleEntries(t *testing.T) {
	const n = 64
	const headerSize = 64 << 10 // 64 KiB per synthetic entry
	half := int64(n*headerSize) / 2

	base := readHeap()

	var nb naiveBatcher
	for i := range n {
		nb.add(&Entry{Path: "/x", Status: 200, RawHeaders: fillString(headerSize, byte(i))})
	}
	nb.flushBuggy()

	after := readHeap()
	if delta := int64(after) - int64(base); delta < half {
		t.Fatalf("flushBuggy did not retain stale entries: delta %d bytes, want >= %d", delta, half)
	}
	runtime.KeepAlive(&nb)
}

// TestFlushReleasesStaleEntries is the fix, measured the same way: Flush's
// clear call drops every pointer from the backing array, so the same
// volume of entries leaves the heap far below the naive footprint.
func TestFlushReleasesStaleEntries(t *testing.T) {
	const n = 64
	const headerSize = 64 << 10
	half := int64(n*headerSize) / 2

	base := readHeap()

	b, err := NewBatcher(n)
	if err != nil {
		t.Fatalf("NewBatcher: %v", err)
	}
	for i := range n {
		b.Add(&Entry{Path: "/x", Status: 200, RawHeaders: fillString(headerSize, byte(i))})
	}
	var discard bytes.Buffer
	if err := b.Flush(&discard); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	after := readHeap()
	if delta := int64(after) - int64(base); delta >= half {
		t.Fatalf("Flush retained stale entries: delta %d bytes, want < %d", delta, half)
	}
	runtime.KeepAlive(b)
}

func TestRun(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		args    []string
		input   string
		want    string
		wantErr bool
	}{
		{name: "single batch flushed at end of input", args: []string{"-batch", "10"},
			input: `{"path":"/login","status":200,"raw_headers":"secret"}` + "\n",
			want:  `{"path":"/login","status":200}` + "\n"},
		{name: "batch size 1 flushes every line", args: []string{"-batch", "1"},
			input: `{"path":"/a","status":200,"raw_headers":"x"}` + "\n" + `{"path":"/b","status":404,"raw_headers":"y"}` + "\n",
			want:  `{"path":"/a","status":200}` + "\n" + `{"path":"/b","status":404}` + "\n"},
		{name: "empty input produces no output", args: []string{"-batch", "10"}},
		{name: "invalid batch size is a usage error", args: []string{"-batch", "0"}, wantErr: true},
		{name: "malformed JSON line is a usage error", args: []string{"-batch", "10"}, input: "not json\n", wantErr: true},
		{name: "unknown flag is a usage error", args: []string{"-bogus"}, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var out bytes.Buffer
			err := run(tc.args, strings.NewReader(tc.input), &out)
			if tc.wantErr {
				if !errors.Is(err, errUsage) {
					t.Fatalf("run(%v) error = %v, want it to wrap errUsage", tc.args, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("run(%v): %v", tc.args, err)
			}
			if out.String() != tc.want {
				t.Fatalf("run(%v) stdout = %q, want %q", tc.args, out.String(), tc.want)
			}
		})
	}
}
```

## Review

`Batcher` is correct when `Flush` writes exactly the non-sensitive fields
and leaves the batch empty and reusable -- `TestFlushWritesRedactedSummariesAndEmptiesBatch`
pins both halves. The mistake the module exists to prevent is treating
`batch = batch[:0]` as a full reset: it is only a length change, and the Go
garbage collector keeps scanning the backing array by its allocated
capacity, not by that length, so every pointer left in the unused tail stays
reachable indefinitely. `TestFlushBuggyRetainsStaleEntries` and
`TestFlushReleasesStaleEntries` measure that difference directly with a
`runtime.ReadMemStats` delta: `clear` before the length reset is what
actually drops the stale pointers, and it costs nothing extra since `Flush`
was already iterating the batch to write it. `redact` maps every usage
mistake -- a bad flag, an invalid `-batch`, unparseable JSON -- to exit code
2 via the `errUsage` sentinel, reserving exit code 1 for a genuine runtime
failure such as a broken stdout pipe. Run `go test -count=1 -race ./...`.

## Resources

- [`builtin.clear`](https://pkg.go.dev/builtin#clear) — zeroes every element of a slice up to its length, or deletes every key of a map.
- [`runtime.MemStats` and `ReadMemStats`](https://pkg.go.dev/runtime#MemStats) — the leak-detection technique this module's core test reuses from Exercise 2.
- [Fluent Bit documentation: Buffering & Storage](https://docs.fluentbit.io/manual/administration/buffering-and-storage) — a real log-shipping agent's batching model this module's `Batcher` mirrors.
- [`bufio.Scanner`](https://pkg.go.dev/bufio#Scanner) — the line-oriented streaming reader the tool uses instead of loading all of stdin into memory.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [13-wal-lazy-iter-seq-reader.md](13-wal-lazy-iter-seq-reader.md) | Next: [15-memtable-compact-clip-guard.md](15-memtable-compact-clip-guard.md)
