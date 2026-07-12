# Exercise 15: Bounded Idempotency-Key Dedup Window

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Every webhook receiver and every Kafka consumer that promises exactly-once
processing needs the same primitive: a bounded memory of "IDs I have already
handled," so a retried delivery is recognized and skipped instead of applied
twice. Stripe's webhook guide tells integrators to keep a recent-events
cache keyed by event ID; Kafka's idempotent producer keeps exactly this kind
of window server-side. The window has to be bounded -- a process that runs
for months cannot remember every ID it has ever seen -- which means it is a
ring buffer of IDs backed by a map for O(1) membership checks. The ring
bounds memory; the map does the actual answering.

That pairing is where a hand-rolled version usually breaks. The ring's job
is easy: overwrite the oldest slot when full, exactly like every other ring
here. The map's job is easy to get wrong: when the ring overwrites a slot,
the ID that lived there must be deleted from the map in the same step, or
the map keeps growing even though the ring stays fixed size. The ring looks
bounded by every metric you'd check -- its length never changes -- while the
map silently becomes unbounded, and an aged-out ID keeps reporting as a
duplicate indefinitely.

This exercise builds `dedupring`, a command-line tool that reads event IDs
from stdin and reports `NEW` or `DUP` for each within a bounded window.

This module is fully self-contained: its own `go mod init`, an executable
command, and its tests. Nothing here imports another exercise.

## What you'll build

```text
dedupring/                    module example.com/dedupring
  go.mod                       go 1.24
  dedupring.go                 package main — DedupWindow; NewDedupWindow, Seen;
                               one sentinel error
  dedupring_test.go             package main — capacity validation, new/duplicate
                               table, eviction aging out, the leaky-map contrast,
                               run() end to end, a scanner-failure case
  main.go                      package main — -window flag, exit codes
```

- Files: `dedupring.go`, `dedupring_test.go`, `main.go`.
- Implement: `NewDedupWindow(capacity int) (*DedupWindow, error)` rejecting a non-positive capacity with `ErrInvalidCapacity`; `(*DedupWindow).Seen(id string) bool` reporting whether `id` is already in the window and recording it if not, evicting the oldest ID from both the ring and the membership map when full.
- Tool: `dedupring` reads event IDs from stdin, one per line. `-window` sets the capacity (default 1000). It prints `NEW <id>` or `DUP <id>` per line to stdout, streaming rather than buffering the whole input. Exit 0 on success, exit 2 for an unparseable flag or a rejected window size (usage errors), exit 1 for a failure reading the input stream itself (a runtime failure distinct from a usage error).
- Test: capacity validation; new-then-duplicate; eviction letting an aged-out ID be seen as new again; a table across small and single-slot windows; the leaky-map contrast; `run` end to end over a `strings.Reader` and a `bytes.Buffer`, including the invalid-window and unknown-flag usage errors, empty input, and an oversized line surfacing as a non-usage error.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### A bounded ring next to an unbounded map is not a bounded window

The ring half of this structure is nothing new: `Seen` overwrites the oldest
ID at `tail` when the window is full, exactly like `Push` in every earlier
module. What is new is that the ring is not, by itself, what answers "have I
seen this before" -- a linear scan of the ring for every incoming ID would
work but would make the tool O(window) per line instead of O(1), which
defeats the purpose the moment `-window` is set to anything realistic for
production traffic. The map is what makes membership cheap, and the map
has to be kept in lockstep with the ring by hand, because nothing in the
language does it automatically:

```go
func (w *leakyWindow) seenAndAdmit(id string) bool {
    if _, dup := w.seen[id]; dup {
        return true
    }
    // ring bookkeeping happens here, correctly...
    w.ids[w.head] = id
    w.seen[id] = struct{}{}
    w.head = (w.head + 1) % len(w.ids)
    // ...but nothing ever calls delete(w.seen, evictedID).
    return false
}
```

This compiles, and it is *more* correct-looking than the eviction bugs
earlier in this lesson, because the ring itself genuinely never grows past
its configured length -- `len(w.ids)` is a compile-time constant, so a
casual glance at memory usage looks perfectly bounded. The map is where the
leak lives, and its growth is not visible from the ring's own fields at all.
Every distinct ID ever admitted stays in `w.seen` forever, whether or not
the ring has since overwritten its slot -- an ID from months ago still
answers `Seen` with `true`. That is worse than a leak: two unrelated events
that happen to share an ID (a replayed fixture, a reused key after a client
bug) get silently treated as duplicates of each other.

The fix is one extra line in the eviction branch: before overwriting the
ring slot, delete the ID that currently occupies it from the map. There is
no way for the type system to enforce that a companion index stays in sync
with the collection it indexes; it is a discipline the code has to carry.

Create `dedupring.go`:

```go
package main

import (
	"errors"
	"fmt"
)

// ErrInvalidCapacity is returned by NewDedupWindow when the requested
// capacity is not positive.
var ErrInvalidCapacity = errors.New("dedupring: capacity must be positive")

// DedupWindow reports whether an event ID has already been seen within the
// most recent Cap() admissions, the pattern behind an idempotent webhook
// receiver or a Kafka consumer's exactly-once dedup cache: bound memory to a
// fixed window instead of remembering every ID a long-running process has
// ever seen.
//
// Membership is answered in O(1) through a companion map, but the map must
// never hold more entries than the ring names: every eviction from the ring
// deletes the matching entry from the map in the same step. Forgetting that
// deletion leaves the ring bounded while the map grows without limit --
// defeating the entire point of a bounded window -- and, worse, makes an
// evicted ID permanently report as a duplicate instead of aging out. See
// the package tests for a side-by-side demonstration.
//
// DedupWindow is not safe for concurrent use; the caller must synchronize
// access.
type DedupWindow struct {
	ids  []string
	seen map[string]struct{}
	head int
	tail int
	size int
}

// NewDedupWindow returns a DedupWindow remembering up to capacity of the
// most recently admitted IDs. It returns ErrInvalidCapacity if capacity is
// not positive.
func NewDedupWindow(capacity int) (*DedupWindow, error) {
	if capacity <= 0 {
		return nil, fmt.Errorf("%w: got %d", ErrInvalidCapacity, capacity)
	}
	return &DedupWindow{
		ids:  make([]string, capacity),
		seen: make(map[string]struct{}, capacity),
	}, nil
}

// Cap reports the maximum number of IDs this DedupWindow remembers at once.
func (w *DedupWindow) Cap() int { return len(w.ids) }

// Len reports how many IDs are currently remembered.
func (w *DedupWindow) Len() int { return w.size }

// Seen reports whether id is already inside the window. If it is not, Seen
// admits it: when the window is full this evicts the oldest ID from both
// the ring and the membership map before recording the new one, so an ID
// that ages out of the window can legitimately be seen as new again.
func (w *DedupWindow) Seen(id string) bool {
	if _, dup := w.seen[id]; dup {
		return true
	}

	if w.size == len(w.ids) {
		oldest := w.ids[w.tail]
		delete(w.seen, oldest) // keep the map in lockstep with the ring
		w.ids[w.tail] = ""
		w.tail = (w.tail + 1) % len(w.ids)
		w.size--
	}
	w.ids[w.head] = id
	w.seen[id] = struct{}{}
	w.head = (w.head + 1) % len(w.ids)
	w.size++
	return false
}
```

### The tool

`dedupring` has one job per line of input, so `run` takes the argument
slice, an `io.Reader` for stdin, and an `io.Writer` for stdout -- no global
state, trivial to drive from a table test with a `strings.Reader` and a
`bytes.Buffer`. `flag.NewFlagSet` with `flag.ContinueOnError` lets `run`
return a parse error instead of the process exiting under a test. A bad
flag or a non-positive `-window` are both usage errors, wrapping `errUsage`
and mapping to exit code 2. A `bufio.Scanner` failure reading the stream
itself -- for example a line longer than the scanner's token buffer -- has
nothing to do with usage, so it is left unwrapped and maps to exit code 1.
The tool streams line by line rather than buffering all of stdin, which
matters specifically here: bounding memory under unbounded input is
`DedupWindow`'s entire purpose, and buffering the stream first would
reintroduce the exact problem the ring exists to solve.

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

// errUsage marks a failure the caller can fix by changing the command
// line or its input: a bad flag or a rejected window size. main maps it to
// exit code 2; every other error maps to exit code 1.
var errUsage = errors.New("usage")

// run reads event IDs from stdin, one per line, and writes "NEW <id>" or
// "DUP <id>" to stdout for each. It takes stdin and stdout as parameters
// rather than touching os.Stdin/os.Stdout directly, so it can be driven
// end to end in a test with a strings.Reader and a bytes.Buffer. It streams
// line by line rather than buffering the whole input, which matters for a
// tool whose entire point is bounding memory under unbounded input.
func run(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("dedupring", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	window := fs.Int("window", 1000, "number of most recent event IDs to remember")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}

	w, err := NewDedupWindow(*window)
	if err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}

	scanner := bufio.NewScanner(stdin)
	for scanner.Scan() {
		id := scanner.Text()
		if w.Seen(id) {
			fmt.Fprintln(stdout, "DUP", id)
		} else {
			fmt.Fprintln(stdout, "NEW", id)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("reading input: %w", err)
	}
	return nil
}

func main() {
	flag.CommandLine.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: dedupring [-window N]")
		fmt.Fprintln(os.Stderr, "reads event IDs from stdin, one per line, and reports NEW or DUP")
		fmt.Fprintln(os.Stderr, "for each within a bounded window of the N most recent IDs.")
	}
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "dedupring:", err)
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}
```

Run it:

```bash
printf 'order-1\norder-2\norder-1\norder-3\norder-1\n' | go run . -window 2
printf 'order-1\norder-2\norder-1\norder-3\norder-1\n' | go run . -window 10
printf 'x\n' | go run . -window 0
```

Expected output:

```text
NEW order-1
NEW order-2
DUP order-1
NEW order-3
NEW order-1
```

```text
NEW order-1
NEW order-2
DUP order-1
NEW order-3
DUP order-1
```

```text
dedupring: usage: dedupring: capacity must be positive: got 0
```

The first two runs process the identical five-line input against two window
sizes and diverge on the last line precisely because of eviction: with
`-window 2`, by the time the second `order-1` arrives the window holds only
`order-2` and `order-3`, so `order-1` legitimately aged out and is reported
`NEW`. With `-window 10` nothing has evicted yet, so the same `order-1` is
still on file and correctly reported `DUP`. The third run shows the exit-2
usage path: `dedupring:` followed by the error `run` returns, itself
wrapping `ErrInvalidCapacity`.

### Tests

`TestSeenReportsNewThenDuplicate` and `TestSeenEvictsOldestAndForgetsIt` are
the two-line story of the algorithm: admit, then recognize; overflow, then
forget the right one. `TestSeenTable` generalizes across capacities
including a single-slot window that overwrites on every push.

`TestLeakyWindowNeverForgetsAnEvictedID` is the heart of the module.
`leakyDedupWindow` is unexported and unreachable from the package API; the
test pushes twice as many distinct IDs as the capacity through both it and
`DedupWindow`, asserts the leaky map's size equals the full count admitted
(unbounded growth, stated as a number) while `DedupWindow`'s map never
exceeds capacity, then re-submits the first ID and shows the leaky version
still reports a duplicate while `DedupWindow` reports new.

`TestRunReportsNewAndDuplicateLines` drives the tool end to end and pins the
exact `NEW`/`DUP` sequence. `TestRunRejectsInvalidWindow` and
`TestRunRejectsUnknownFlag` both assert the error wraps `errUsage`.
`TestRunSurfacesScannerFailureAsRuntimeError` feeds a line far longer than
`bufio.Scanner`'s token limit and asserts the error does *not* wrap
`errUsage` -- a stream-reading failure, unrelated to how the command was
invoked.

Create `dedupring_test.go`:

```go
package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// leakyDedupWindow is the companion-map version as it is usually written
// first: the ring evicts its oldest slot on overwrite, but the map entry
// for the evicted ID is never deleted. It is unexported and unreachable
// from the package API; it exists only so the tests can pin the bug: the
// ring stays bounded, the map does not, and an evicted ID keeps reporting
// as a duplicate forever.
type leakyDedupWindow struct {
	ids  []string
	seen map[string]struct{}
	head int
	size int
}

func newLeakyDedupWindow(capacity int) *leakyDedupWindow {
	return &leakyDedupWindow{ids: make([]string, capacity), seen: make(map[string]struct{})}
}

func (w *leakyDedupWindow) seenAndAdmit(id string) bool {
	if _, dup := w.seen[id]; dup {
		return true
	}
	if w.size == len(w.ids) {
		// Bug: the ring slot is overwritten below, but nothing removes
		// w.ids[w.head]'s old value from w.seen first.
	} else {
		w.size++
	}
	w.ids[w.head] = id
	w.seen[id] = struct{}{}
	w.head = (w.head + 1) % len(w.ids)
	return false
}

func TestNewDedupWindowRejectsNonPositiveCapacity(t *testing.T) {
	t.Parallel()
	for _, capacity := range []int{0, -1} {
		if _, err := NewDedupWindow(capacity); !errors.Is(err, ErrInvalidCapacity) {
			t.Errorf("NewDedupWindow(%d) error = %v, want ErrInvalidCapacity", capacity, err)
		}
	}
}

func TestSeenReportsNewThenDuplicate(t *testing.T) {
	t.Parallel()

	w, err := NewDedupWindow(4)
	if err != nil {
		t.Fatalf("NewDedupWindow: %v", err)
	}
	if w.Seen("evt-1") {
		t.Fatal("first Seen(evt-1): want new, got duplicate")
	}
	if !w.Seen("evt-1") {
		t.Fatal("second Seen(evt-1): want duplicate, got new")
	}
	if w.Len() != 1 {
		t.Fatalf("Len() = %d, want 1", w.Len())
	}
}

func TestSeenEvictsOldestAndForgetsIt(t *testing.T) {
	t.Parallel()

	w, err := NewDedupWindow(2)
	if err != nil {
		t.Fatalf("NewDedupWindow: %v", err)
	}
	w.Seen("a")
	w.Seen("b")
	w.Seen("c") // evicts a

	if w.Seen("a") == true {
		t.Fatal("Seen(a) after eviction: want new (a aged out), got duplicate")
	}
	if w.Len() != w.Cap() {
		t.Fatalf("Len() = %d, want Cap() = %d", w.Len(), w.Cap())
	}
}

func TestSeenTable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		capacity int
		ids      []string
		wantDup  []bool
	}{
		{
			name:     "empty window, single id",
			capacity: 3,
			ids:      []string{"x"},
			wantDup:  []bool{false},
		},
		{
			name:     "immediate repeat",
			capacity: 3,
			ids:      []string{"x", "x"},
			wantDup:  []bool{false, true},
		},
		{
			name:     "capacity one wraps every push",
			capacity: 1,
			ids:      []string{"x", "y", "x"},
			wantDup:  []bool{false, false, false},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			w, err := NewDedupWindow(tc.capacity)
			if err != nil {
				t.Fatalf("NewDedupWindow: %v", err)
			}
			for i, id := range tc.ids {
				got := w.Seen(id)
				if got != tc.wantDup[i] {
					t.Fatalf("Seen(%q) at step %d = %v, want %v", id, i, got, tc.wantDup[i])
				}
			}
		})
	}
}

// TestLeakyWindowNeverForgetsAnEvictedID is the whole point of the module:
// once the ring wraps past capacity, the leaky version's map keeps growing
// forever and an evicted ID is reported as a duplicate no matter how long
// ago it left the window, while DedupWindow correctly lets it be seen as
// new again and never holds more map entries than its capacity.
func TestLeakyWindowNeverForgetsAnEvictedID(t *testing.T) {
	t.Parallel()

	const capacity = 4
	leaky := newLeakyDedupWindow(capacity)
	correct, err := NewDedupWindow(capacity)
	if err != nil {
		t.Fatalf("NewDedupWindow: %v", err)
	}

	// Push twice as many distinct IDs as the capacity through both.
	ids := make([]string, 2*capacity)
	for i := range ids {
		ids[i] = string(rune('a' + i))
	}
	for _, id := range ids {
		leaky.seenAndAdmit(id)
		correct.Seen(id)
	}

	if len(leaky.seen) != len(ids) {
		t.Fatalf("leaky map holds %d entries after %d admissions to a capacity-%d window, want %d (unbounded growth)",
			len(leaky.seen), len(ids), capacity, len(ids))
	}
	if len(correct.seen) > capacity {
		t.Fatalf("DedupWindow map holds %d entries, want at most Cap() = %d", len(correct.seen), capacity)
	}

	firstID := ids[0] // evicted long ago from both
	if !leaky.seenAndAdmit(firstID) {
		t.Fatal("leaky window reported the long-evicted first ID as new; want it to still incorrectly report duplicate")
	}
	if correct.Seen(firstID) {
		t.Fatal("DedupWindow reported the long-evicted first ID as a duplicate; it should have aged out")
	}
}

func TestRunReportsNewAndDuplicateLines(t *testing.T) {
	t.Parallel()

	in := strings.NewReader("evt-1\nevt-2\nevt-1\nevt-3\n")
	var out bytes.Buffer
	if err := run([]string{"-window", "2"}, in, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	want := "NEW evt-1\nNEW evt-2\nDUP evt-1\nNEW evt-3\n"
	if out.String() != want {
		t.Fatalf("run output = %q, want %q", out.String(), want)
	}
}

func TestRunRejectsInvalidWindow(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	err := run([]string{"-window", "0"}, strings.NewReader(""), &out)
	if !errors.Is(err, errUsage) {
		t.Fatalf("run(-window 0) error = %v, want it to wrap errUsage", err)
	}
}

func TestRunRejectsUnknownFlag(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	err := run([]string{"-bogus"}, strings.NewReader(""), &out)
	if !errors.Is(err, errUsage) {
		t.Fatalf("run(-bogus) error = %v, want it to wrap errUsage", err)
	}
}

func TestRunOnEmptyInputProducesNoOutput(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	if err := run(nil, strings.NewReader(""), &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("run output = %q, want empty", out.String())
	}
}

// TestRunSurfacesScannerFailureAsRuntimeError feeds a line far longer than
// bufio.Scanner's default token limit, which is a genuine runtime failure
// distinct from a usage error: the command line and the window size were
// both fine, but the input stream itself could not be read as given.
func TestRunSurfacesScannerFailureAsRuntimeError(t *testing.T) {
	t.Parallel()

	huge := strings.Repeat("x", 70_000)
	var out bytes.Buffer
	err := run(nil, strings.NewReader(huge), &out)
	if err == nil {
		t.Fatal("run with an oversized line: want error, got nil")
	}
	if errors.Is(err, errUsage) {
		t.Fatalf("run with an oversized line: want a non-usage error, got %v", err)
	}
}
```

## Review

`DedupWindow` is correct when the map answering "have I seen this ID" never
holds more entries than the ring can name, and an ID that genuinely ages out
can be seen as new again. `Seen` gets both right by treating eviction as one
atomic step: delete the outgoing ID from the map in the same branch that
overwrites its ring slot, never as an afterthought. The bug this module
names is a companion structure that quietly drifts out of sync with the
collection it indexes -- the ring stays a fixed length by construction, so
nothing about its own size reveals the map is leaking, and a forgotten ID
answers duplicate forever. `run` keeps failure modes separated: a bad flag
or rejected window maps to exit code 2, a failure reading the stream itself
maps to exit code 1, and success streams output line by line rather than
buffering all of stdin, which matters because bounding memory under
unbounded input is this tool's entire purpose. Run
`go test -count=1 -race ./...` to confirm the eviction behavior, the
leaky-map contrast, and `run`'s end-to-end exit-code discipline.

## Resources

- [Stripe: Best practices for webhook endpoints](https://docs.stripe.com/webhooks#handle-duplicate-events) — the idempotency-key dedup pattern this tool models, from a production webhook receiver's perspective.
- [Kafka: idempotent producer](https://kafka.apache.org/documentation/#semantics) — the server-side sequence-number dedup window that inspired this exercise's shape.
- [`bufio.Scanner`](https://pkg.go.dev/bufio#Scanner) — `Scan`, `Text`, and `Err`, including the default token-size limit this module's runtime-failure test exercises.
- [`flag.NewFlagSet`](https://pkg.go.dev/flag#NewFlagSet) — `flag.ContinueOnError`, used so `run` can return a parse error instead of the process exiting.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [14-clock-second-chance-page-cache.md](14-clock-second-chance-page-cache.md) | Next: [16-connection-pool-health-checked-release.md](16-connection-pool-health-checked-release.md)
