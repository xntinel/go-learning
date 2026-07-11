# Exercise 17: Compacting a WAL Replay Queue's Acknowledged Prefix

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Every consumer of an append-only log -- etcd replaying its WAL on startup, a
Kafka-style consumer tracking how far it has committed, a message broker's
in-memory backlog between "delivered" and "acknowledged" -- keeps a pending
buffer between two offsets: how far it has read, and how far downstream has
confirmed. The obvious way to advance the second offset is to re-slice the
buffer: `pending = pending[n:]`. It is also the trap. Re-slicing moves the
header's pointer forward and shrinks its length, but the backing array
underneath is exactly the array it always was, sized for the high-water
mark of everything ever appended. A queue that peaks at a million pending
records and settles at a steady ten keeps a million records' worth of
array reachable through that header, forever, because the header still
points into the middle of it.

This is the same idea as a sub-slice pinning its source array, applied to a
buffer that a service keeps re-slicing from the front over its entire
runtime rather than once at a boundary. The fix is not "never re-slice" --
re-slicing the acknowledged prefix away is the correct O(1) first step, and
doing it on every single ack is fine. The fix is to notice when the
*wasted* fraction of the array -- capacity that used to hold acknowledged
records and can hold nothing new -- has grown large enough that it is worth
paying for a copy, and to make that copy into a freshly, tightly sized
array. `slices.Clip` cannot help here: clipping only bounds a value's own
future growth, and this array's problem is that most of it is already
unreachable weight the *current* header still carries.

This module builds `replayq`, a command-line tool that reads a stream of
`append` and `ack` commands and reports, after every `ack`, whether it
compacted. The naive re-slice-only version that never compacts is not part
of the tool's component API -- it lives in the test file, isolated as the
thing the tests prove wastes memory.

This module is fully self-contained: its own `go mod init`, an executable
command, and its tests. Nothing here imports another exercise.

## What you'll build

```text
replayq/                  module example.com/replayq
  go.mod                  go 1.24
  replayq.go              Record, ReplayQueue; NewReplayQueue, Append, Ack, Len
  replayq_test.go          ack table, threshold-crossing behavior, the naive-vs-
                           compacting capacity contrast, run() end to end
  main.go                 package main — -threshold flag, stdin loop, exit codes
```

- Files: `replayq.go`, `replayq_test.go`, `main.go`.
- Implement: `NewReplayQueue(compactThreshold float64) (*ReplayQueue, error)` rejecting a threshold outside `(0, 1)` with `ErrInvalidThreshold`; `(*ReplayQueue).Append(data string) Record` assigning a monotonic sequence number; `(*ReplayQueue).Ack(n int) (compacted bool, err error)` rejecting an out-of-range count with `ErrAckOutOfRange`, re-slicing the acknowledged prefix away and copying the tail into a fresh array once the wasted capacity fraction reaches the configured threshold; `(*ReplayQueue).Len() int`.
- Tool: `replayq` reads `append TEXT` and `ack N` commands from stdin, one per line, and writes one status line per command to stdout. `-threshold` sets the compaction trigger (default `0.8`). Exit 0 on success, exit 2 for a bad flag, a malformed command line, or an out-of-range ack (all usage errors), exit 1 for a stdin read failure.
- Test: the ack table (none, all, negative, over the pending count, on an empty queue); a threshold-crossing case where a small ack does not compact and a large one does, using a property (`cap == len` after compaction) rather than an exact number; the capacity contrast between the naive re-slice-only path and the real `Ack` after an identical workload, asserted as `compacting < naive`, never an exact count; `run` end to end over its argument slice, a `strings.Reader`, and a `bytes.Buffer`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/replayq
cd ~/go-exercises/replayq
go mod init example.com/replayq
go mod edit -go=1.24
```

### Re-slicing a consumed prefix never shrinks the array underneath

A slice header carries a pointer, a length, and a capacity, and `s[n:]`
only ever changes the first two: it moves the pointer to element `n` and
recomputes the length as `len(s) - n`. It has no way to touch the array
`s`'s pointer used to point at the *start* of -- that array is a single
allocation, and Go's allocator does not support shrinking one in place.
Whatever memory the array occupied before the slice is exactly the memory
it occupies after, whether the header now covers all of it, half of it, or
one element of it. A naive `Ack` that only re-slices looks correct, and for
any single call, it is:

```go
// replayq.go -- the bug, if Ack never compacted.
func (q *ReplayQueue) ackOnly(n int) error {
    if n < 0 || n > len(q.pending) {
        return fmt.Errorf("%w: ack %d, have %d pending", ErrAckOutOfRange, n, len(q.pending))
    }
    q.pending = q.pending[n:] // O(1) -- and the array never gets smaller
    return nil
}
```

Call this on every ack, for the life of a long-running consumer, and
`cap(q.pending)` is a one-way ratchet: it only ever grows, driven by
whatever the largest backlog was at any point in the process's history. A
consumer that briefly fell behind by a million records during a downstream
outage carries that million-record array for the rest of its uptime, even
once it is caught up to the last ten. The fix is not to stop re-slicing --
`pending[n:]` is still the right first move, an O(1) way to drop the
acknowledged records from what the header exposes. The fix is to
periodically follow it with a real copy, `append([]Record(nil),
q.pending...)`, into an array sized for what is left, once enough of the
current array has become wasted weight to justify paying for that copy.

Create `replayq.go`:

```go
// Command replayq models the pending-replay buffer a WAL-based consumer
// keeps between the offset it has appended up to and the offset it has
// acknowledged downstream -- the shape shared by etcd's WAL replay path and
// a Kafka-style consumer's in-memory backlog. Acknowledging records by
// re-slicing a prefix away, `pending = pending[n:]`, never gives the
// backing array back. ReplayQueue.Ack reclaims it by copying the surviving
// tail into a freshly sized array once the acknowledged-but-still-pinned
// prefix crosses a configured fraction of the array's capacity.
package main

import (
	"errors"
	"fmt"
)

// Sentinel errors returned by NewReplayQueue and Ack. Callers should test
// for them with errors.Is rather than by comparing error strings.
var (
	// ErrInvalidThreshold means the configured compaction threshold was not
	// strictly between 0 and 1.
	ErrInvalidThreshold = errors.New("replayq: compact threshold must be in (0, 1)")
	// ErrAckOutOfRange means Ack was asked to acknowledge more records than
	// are currently pending, or a negative count.
	ErrAckOutOfRange = errors.New("replayq: ack count out of range")
)

// Record is one pending WAL entry awaiting acknowledgment.
type Record struct {
	Seq  int
	Data string
}

// ReplayQueue holds WAL records that have been appended but not yet
// acknowledged by a downstream consumer.
//
// ReplayQueue is not safe for concurrent use.
type ReplayQueue struct {
	pending   []Record
	nextSeq   int
	compactAt float64
}

// NewReplayQueue returns an empty ReplayQueue that compacts its backing
// array once an Ack leaves at least compactThreshold of that array's
// capacity unreachable through pending. It returns ErrInvalidThreshold if
// compactThreshold is not strictly between 0 and 1.
func NewReplayQueue(compactThreshold float64) (*ReplayQueue, error) {
	if compactThreshold <= 0 || compactThreshold >= 1 {
		return nil, fmt.Errorf("%w: got %v", ErrInvalidThreshold, compactThreshold)
	}
	return &ReplayQueue{compactAt: compactThreshold}, nil
}

// Append adds a new record with the given data and an automatically
// assigned, monotonically increasing sequence number.
func (q *ReplayQueue) Append(data string) Record {
	r := Record{Seq: q.nextSeq, Data: data}
	q.nextSeq++
	q.pending = append(q.pending, r)
	return r
}

// Len reports the number of records currently pending.
func (q *ReplayQueue) Len() int { return len(q.pending) }

// Ack acknowledges the first n pending records -- they have been committed
// downstream and are dropped from the queue -- and reports whether it
// compacted the backing array to reclaim the space they were pinning. It
// returns ErrAckOutOfRange if n is negative or exceeds the number of
// records currently pending.
//
// Ack always advances by re-slicing the acknowledged prefix away first, an
// O(1) step that leaves the backing array exactly as large as it was: the
// array is still referenced by q.pending's header and cannot be reclaimed
// on that basis alone. Once the wasted fraction of that array reaches the
// configured threshold, Ack copies the surviving tail into a freshly sized
// array -- the only operation that actually returns memory to the
// allocator.
func (q *ReplayQueue) Ack(n int) (compacted bool, err error) {
	if n < 0 || n > len(q.pending) {
		return false, fmt.Errorf("%w: ack %d, have %d pending", ErrAckOutOfRange, n, len(q.pending))
	}
	q.pending = q.pending[n:]
	if cap(q.pending) > 0 {
		wasted := 1 - float64(len(q.pending))/float64(cap(q.pending))
		if wasted >= q.compactAt {
			q.pending = append([]Record(nil), q.pending...)
			compacted = true
		}
	}
	return compacted, nil
}
```

### The tool

`replayq` has no configuration beyond the compaction threshold, so `run`
takes the argument slice, an `io.Reader` for stdin, and an `io.Writer` for
stdout -- nothing tied to `os.Stdin` or `os.Stdout` directly, which is what
lets the table test below drive it with a `strings.Reader` and a
`bytes.Buffer`. Input is streamed line by line through a `bufio.Scanner`
rather than read in full, which matters for the exact reason this module
exists: a tool about not letting a buffer accumulate unbounded should not
itself load an unbounded input into memory before processing it. Every
failure `run` can produce -- a bad flag, a malformed command line, an
out-of-range ack -- is something the caller fixes by changing the input, so
all of them wrap `errUsage` and `main` maps that to exit code 2; a stdin
read failure is the one path that maps to exit code 1.

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
	"strconv"
	"strings"
)

// errUsage marks a failure the caller can fix by changing the command line
// or the input stream: a bad flag, a malformed command line, or an
// out-of-range ack. main maps it to exit code 2; every other error maps to
// exit code 1.
var errUsage = errors.New("usage")

// run parses args, then streams commands from stdin one line at a time --
// "append TEXT" or "ack N" -- writing one status line per command to
// stdout. It never touches os.Stdin, os.Stdout, or os.Exit directly, so it
// can be exercised in a test with a strings.Reader and a bytes.Buffer.
func run(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("replayq", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	threshold := fs.Float64("threshold", 0.8, "wasted-capacity fraction that triggers compaction")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}

	q, err := NewReplayQueue(*threshold)
	if err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}

	scanner := bufio.NewScanner(stdin)
	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		fields := strings.SplitN(text, " ", 2)
		switch fields[0] {
		case "append":
			if len(fields) < 2 || fields[1] == "" {
				return fmt.Errorf("%w: line %d: append requires data", errUsage, line)
			}
			r := q.Append(fields[1])
			fmt.Fprintf(stdout, "appended seq=%d pending=%d\n", r.Seq, q.Len())
		case "ack":
			if len(fields) < 2 {
				return fmt.Errorf("%w: line %d: ack requires a count", errUsage, line)
			}
			n, err := strconv.Atoi(fields[1])
			if err != nil {
				return fmt.Errorf("%w: line %d: invalid ack count %q", errUsage, line, fields[1])
			}
			compacted, err := q.Ack(n)
			if err != nil {
				return fmt.Errorf("%w: line %d: %v", errUsage, line, err)
			}
			fmt.Fprintf(stdout, "acked=%d pending=%d compacted=%t\n", n, q.Len(), compacted)
		default:
			return fmt.Errorf("%w: line %d: unknown command %q", errUsage, line, fields[0])
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}
	return nil
}

func main() {
	flag.CommandLine.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: replayq [-threshold F] < commands")
		fmt.Fprintln(os.Stderr, "reads 'append TEXT' and 'ack N' lines from stdin, one status line per command.")
	}
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "replayq:", err)
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}
```

Run it:

```bash
printf 'append r0\nappend r1\nappend r2\nappend r3\nappend r4\nappend r5\nappend r6\nappend r7\nack 1\nappend r8\nappend r9\nack 8\nack 1\n' | go run . -threshold=0.7
printf 'ack 1\n' | go run .
```

Expected output:

```text
appended seq=0 pending=1
appended seq=1 pending=2
appended seq=2 pending=3
appended seq=3 pending=4
appended seq=4 pending=5
appended seq=5 pending=6
appended seq=6 pending=7
appended seq=7 pending=8
acked=1 pending=7 compacted=false
appended seq=8 pending=8
appended seq=9 pending=9
acked=8 pending=1 compacted=true
acked=1 pending=0 compacted=false
```

```text
replayq: usage: line 1: replayq: ack count out of range: ack 1, have 0 pending
```

The first run shows the whole lifecycle: acking a single record out of
eight leaves plenty of the array still reachable, so `compacted=false`;
acking eight out of nine, later, pushes the wasted fraction past `0.7`, and
`compacted=true` fires. The very last line, acking the sole remaining
record, shows the zero-capacity edge case working correctly: after a
compaction to a one-record array, acknowledging that last record leaves a
`cap` of zero, and `Ack` skips the wasted-fraction division rather than
dividing by it. The second run shows the exit-2 usage error: acking against
an empty queue is rejected by `ErrAckOutOfRange`, wrapped by `errUsage`.

### Tests

`TestAckTable` covers `Ack`'s ordinary and invalid inputs together: acking
none, acking all, a negative count, a count past what is pending, and a
count against an empty queue -- the last three all resolving to
`ErrAckOutOfRange`, checkable with `errors.Is`. `TestAckCompactsOnlyPastThreshold`
pins the threshold behavior itself: acking a small fraction of a hundred-
record queue must not compact, and acking nearly all of it must, and after
compaction `cap(q.pending)` must equal `len(q.pending)` exactly -- a
property, not a specific number, since it only claims the array is sized
for what survived, not what that size happens to be.

`TestNaiveAckNeverReclaimsCapacityButCompactDoes` is the module's center of
gravity. `ackNaive` is unexported and unreachable from the package API; it
is `Ack` with the compaction step deleted, re-slicing only. The test runs
an identical two-hundred-record workload through both a naive queue and a
compacting one, acknowledges nearly everything in each, and asserts
`cap(compacting) < cap(naive)` -- never an exact number, since the precise
capacities involved are a runtime growth-curve detail, but the *direction*
of the comparison is exactly what this module is built to guarantee.
`TestNewReplayQueueRejectsInvalidThreshold` sweeps the boundary and
out-of-range thresholds. `TestRun` drives the command end to end: a
successful append-then-ack session against its exact expected stdout, and
four ways the input can be rejected, each asserted to wrap `errUsage`.

Create `replayq_test.go`:

```go
package main

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// ackNaive is Ack without the fix: it advances the pending prefix by
// re-slicing only, and never compacts. It is never exported and never
// reachable from the package API; it exists so the tests can pin the
// unbounded capacity growth it causes.
func ackNaive(q *ReplayQueue, n int) error {
	if n < 0 || n > len(q.pending) {
		return fmt.Errorf("%w: ack %d, have %d pending", ErrAckOutOfRange, n, len(q.pending))
	}
	q.pending = q.pending[n:]
	return nil
}

func TestAckTable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		appendN int
		ackN    int
		wantErr error
		wantLen int
	}{
		{name: "ack none", appendN: 5, ackN: 0, wantLen: 5},
		{name: "ack all", appendN: 5, ackN: 5, wantLen: 0},
		{name: "ack negative", appendN: 5, ackN: -1, wantErr: ErrAckOutOfRange},
		{name: "ack more than pending", appendN: 5, ackN: 6, wantErr: ErrAckOutOfRange},
		{name: "ack on empty queue", appendN: 0, ackN: 1, wantErr: ErrAckOutOfRange},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			q, err := NewReplayQueue(0.9)
			if err != nil {
				t.Fatalf("NewReplayQueue: %v", err)
			}
			for i := range tc.appendN {
				if r := q.Append(fmt.Sprintf("r%d", i)); r.Seq != i {
					t.Fatalf("Append seq = %d, want %d", r.Seq, i)
				}
			}

			_, err = q.Ack(tc.ackN)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Ack(%d) err = %v, want %v", tc.ackN, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Ack(%d): %v", tc.ackN, err)
			}
			if q.Len() != tc.wantLen {
				t.Fatalf("Len() = %d, want %d", q.Len(), tc.wantLen)
			}
		})
	}
}

func TestAckCompactsOnlyPastThreshold(t *testing.T) {
	t.Parallel()

	q, err := NewReplayQueue(0.8)
	if err != nil {
		t.Fatalf("NewReplayQueue: %v", err)
	}
	for i := range 100 {
		q.Append(fmt.Sprintf("r%d", i))
	}

	// A small ack leaves the wasted fraction well below 0.8 regardless of
	// append's exact growth curve for 100 sequential appends.
	compacted, err := q.Ack(5)
	if err != nil {
		t.Fatalf("Ack(5): %v", err)
	}
	if compacted {
		t.Errorf("Ack(5) compacted = true, want false (wasted fraction too low)")
	}

	// Acking nearly everything else pushes it well above 0.8 regardless of
	// the exact starting capacity.
	compacted, err = q.Ack(90)
	if err != nil {
		t.Fatalf("Ack(90): %v", err)
	}
	if !compacted {
		t.Errorf("Ack(90) compacted = false, want true (wasted fraction high)")
	}
	if cap(q.pending) != len(q.pending) {
		t.Errorf("after compaction cap = %d, want == len (%d): compaction should size the array exactly", cap(q.pending), len(q.pending))
	}
}

// TestNaiveAckNeverReclaimsCapacityButCompactDoes is the heart of the
// module: it asserts a property, not an exact capacity, since the growth
// curve behind it is a runtime detail -- the compacting queue's capacity
// must end up strictly smaller than the naive queue's after an identical
// workload.
func TestNaiveAckNeverReclaimsCapacityButCompactDoes(t *testing.T) {
	t.Parallel()

	naive, err := NewReplayQueue(0.5)
	if err != nil {
		t.Fatalf("NewReplayQueue: %v", err)
	}
	compacting, err := NewReplayQueue(0.5)
	if err != nil {
		t.Fatalf("NewReplayQueue: %v", err)
	}

	const n = 200
	for i := range n {
		naive.Append(fmt.Sprintf("r%d", i))
		compacting.Append(fmt.Sprintf("r%d", i))
	}
	if err := ackNaive(naive, n-1); err != nil {
		t.Fatalf("ackNaive: %v", err)
	}
	if _, err := compacting.Ack(n - 1); err != nil {
		t.Fatalf("Ack: %v", err)
	}

	if !(cap(compacting.pending) < cap(naive.pending)) {
		t.Fatalf("cap(compacting)=%d, cap(naive)=%d; want compacting strictly smaller", cap(compacting.pending), cap(naive.pending))
	}
}

func TestNewReplayQueueRejectsInvalidThreshold(t *testing.T) {
	t.Parallel()

	for _, th := range []float64{0, 1, -0.1, 1.1} {
		if _, err := NewReplayQueue(th); !errors.Is(err, ErrInvalidThreshold) {
			t.Errorf("NewReplayQueue(%v) error = %v, want ErrInvalidThreshold", th, err)
		}
	}
}

func TestRun(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		args    []string
		in      string
		want    string
		wantErr bool
	}{
		{name: "append then ack", in: "append r0\nappend r1\nack 1\n", want: "appended seq=0 pending=1\nappended seq=1 pending=2\nacked=1 pending=1 compacted=false\n"},
		{name: "unknown command is a usage error", in: "wat\n", wantErr: true},
		{name: "ack out of range is a usage error", in: "ack 1\n", wantErr: true},
		{name: "non-numeric ack is a usage error", in: "append r0\nack banana\n", wantErr: true},
		{name: "bad flag is a usage error", args: []string{"-threshold=2"}, in: "append r0\n", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var stdout bytes.Buffer
			err := run(tc.args, strings.NewReader(tc.in), &stdout)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("run(%v): want error, got nil", tc.args)
				}
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

`Ack` is correct when re-slicing and compacting are both present and
sequenced correctly: re-slice first, always, because it is the cheap O(1)
step that shrinks what the queue exposes; compact second, only once the
array's wasted fraction crosses the configured threshold, because a copy on
every single ack would erase the point of re-slicing at all. `NewReplayQueue`
rejects a threshold outside `(0, 1)` with `ErrInvalidThreshold`, and `Ack`
rejects an invalid count with `ErrAckOutOfRange`, both checkable with
`errors.Is`. The naive path, confined to the test file as `ackNaive`, is
never offered as a mode on `ReplayQueue` -- the only way to get the
never-shrinks behavior is to delete the compaction step entirely, which is
exactly what the contrast test does to prove why that step matters. The
tool's exit codes follow directly from that split: a bad flag or a
malformed or out-of-range command is something the caller fixes by
changing the input, exit 2; a stdin read failure is exit 1. Run `go test
-count=1 -race ./...`.

## Resources

- [Go Spec: Slice expressions](https://go.dev/ref/spec#Slice_expressions) — why `s[n:]` changes a header's pointer and length but never the array underneath.
- [`append`](https://pkg.go.dev/builtin#append) — the mechanism `Ack` uses, via `append([]Record(nil), ...)`, to force a fresh, tightly sized array.
- [etcd: wal package](https://pkg.go.dev/go.etcd.io/etcd/server/v3/storage/wal) — a real write-ahead log whose replay path keeps exactly this kind of pending-versus-committed buffer.
- [`bufio.Scanner`](https://pkg.go.dev/bufio#Scanner) — the streaming line reader `run` uses instead of loading all of stdin into memory.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [16-connpool-element-pointer-pin.md](16-connpool-element-pointer-pin.md) | Next: [18-dispatch-context-cancel-goroutine-leak.md](18-dispatch-context-cancel-goroutine-leak.md)
