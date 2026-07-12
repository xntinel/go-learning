# Exercise 12: Sliding-Window Rate Limiter: Front-Truncation vs Compaction

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A sliding-window rate limiter -- the shape behind Envoy's `local_ratelimit`
filter, or a Redis sorted-set limiter that trims expired members on every
request -- keeps a running list of admitted event timestamps and, on every
new arrival, evicts whatever has aged out of the trailing window. The
obvious Go implementation reslices from the front: `events =
events[cut:]` drops everything before the eviction cutoff in one line, no
loop, no allocation for the eviction itself. It looks free, and for a single
call it is. Called on every admission, for the lifetime of a long-running
process, it is not.

Reslicing from the front never reclaims the space it drifts past. A slice's
header carries a pointer to its current starting element and a capacity
measured from there to the end of the *original* backing array; `events =
events[cut:]` only ever moves that pointer forward, it never moves data. So
each eviction quietly consumes a slice of the array's remaining capacity
that nothing will ever give back, and once that capacity is exhausted the
next `append` must allocate an entirely new backing array -- which then
starts drifting forward and running out in exactly the same way. A limiter
that admits steadily for hours triggers a fresh reallocation every few dozen
admissions, forever, purely because eviction never reused the space in front
of the live window. This is capacity pinning in its temporal form: the same
underlying fact as a sub-slice keeping a whole buffer alive, playing out
across time instead of across two variables.

The fix keeps the header's pointer fixed and moves the *data* instead:
`copy(events, events[cut:])` shifts the surviving elements back to the front
of the same array, then a reslice trims the length. `copy` is defined to
behave correctly even when source and destination overlap -- it does not
corrupt data the way a naive forward-copying loop would when shifting
elements leftward over themselves. Once the live window has been compacted
back to the front a few times, the same backing array serves the limiter
indefinitely: no more reallocations, no more capacity bleeding away one
eviction at a time.

This module is fully self-contained: its own `go mod init`, an executable
command, and its tests. Nothing here imports another exercise.

## What you'll build

```text
windowprune/                   module example.com/windowprune
  go.mod                       go 1.24
  windowprune.go                package main — Window, NewWindow, (*Window).Admit, (*Window).Events
  windowprune_test.go           package main — admission table, out-of-order rejection,
                                aliasing pin, front-truncation-vs-compaction contrast, run() end to end
  main.go                       package main — -window flag, streaming stdin, exit codes
```

- Files: `windowprune.go`, `windowprune_test.go`, `main.go`.
- Implement: `NewWindow(size int) (*Window, error)` rejecting a non-positive size with `ErrInvalidWindow`; `(*Window).Admit(tick int) error`, which evicts entries older than `tick-Size+1` by compacting the survivors to the front of the same backing array with `copy`, rejects a `tick` smaller than the last admitted one with `ErrOutOfOrderTick`, and appends `tick`; `(*Window).Events() []int`, aliasing the internal storage.
- Tool: `windowprune -window N` reads one integer logical tick per line from stdin and prints the current window and its `cap()` after every line. Exit 0 on success, exit 2 on a bad flag, a non-positive window, a non-integer tick, or an out-of-order tick (all usage errors), exit 1 is reserved for a stdin read failure.
- Test: the admission table (all within window, full eviction, duplicate ticks, window of one, a single tick); rejecting an out-of-order tick without mutating the window; `Events` aliasing internal storage, pinned by a snapshot's contents changing after a later `Admit`; the heart of the module -- over an identical long tick sequence, compaction triggers strictly fewer backing-array replacements than an unexported front-truncation-only `naiveWindow`; and `run` end to end over a `strings.Reader` and a `bytes.Buffer`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Reslicing from the front only ever moves the pointer forward

`events[cut:]` computes a new header: pointer advanced by `cut` elements,
length and capacity both reduced by `cut`. What it does *not* do is touch
the memory those `cut` elements occupied -- that memory still belongs to the
same backing array, it is simply no longer reachable through this header.
The bug this module is about is calling that expression, alone, on every
admission for the life of a process:

```go
func (w *naiveWindow) admit(tick int) {
    threshold := tick - w.size + 1
    cut := 0
    for cut < len(w.events) && w.events[cut] < threshold {
        cut++
    }
    w.events = w.events[cut:]           // pointer advances, capacity shrinks
    w.events = append(w.events, tick)   // eventually forces a fresh array
}
```

Every call that evicts anything (`cut > 0`) permanently loses that much
capacity from the current backing array -- there is no operation left in
this function that can get it back. Once `len(w.events) == cap(w.events)`,
`append` has no choice but to allocate a new array, copy the live window
into it, and let the drift start over. The window itself never grows
unbounded; what grows is the *rate* of reallocation, because each fresh
array is only ever consumed by the same forward-only drift before it too
runs out.

`copy(w.events, w.events[cut:])` is the fix, and it is a genuinely different
operation from reslicing: it writes bytes, moving the surviving elements to
the front of the *same* array the header has always pointed at. The header's
own pointer never moves. `copy`'s documented behavior for overlapping source
and destination -- correct regardless of whether the ranges overlap, the way
`memmove` is and a naive `for i := range` forward-copy is not -- is what
makes shifting data leftward over itself safe here.

Create `windowprune.go`:

```go
// Command windowprune maintains a sliding-window admission log over logical
// ticks read from stdin, evicting expired ticks on every arrival by
// compacting its backing array in place rather than only reslicing from the
// front.
package main

import (
	"errors"
	"fmt"
)

// ErrInvalidWindow is returned by NewWindow when size is not positive.
var ErrInvalidWindow = errors.New("windowprune: window size must be positive")

// ErrOutOfOrderTick is returned by Admit when tick is smaller than the most
// recently admitted tick.
var ErrOutOfOrderTick = errors.New("windowprune: tick must not be smaller than the previous tick")

// Window is a sliding-window admission log over logical ticks: after each
// Admit it retains exactly the ticks that fall within the trailing span of
// Size ticks ending at the most recently admitted one.
//
// Window is not safe for concurrent use; the caller must synchronize calls
// to Admit and Events across goroutines.
type Window struct {
	size   int
	events []int
}

// NewWindow returns a Window retaining ticks within a trailing span of size
// logical ticks. It returns ErrInvalidWindow if size is not positive.
func NewWindow(size int) (*Window, error) {
	if size <= 0 {
		return nil, fmt.Errorf("%w: got %d", ErrInvalidWindow, size)
	}
	return &Window{size: size}, nil
}

// Admit records tick as newly arrived and evicts every previously admitted
// tick older than tick-Size+1. tick must be greater than or equal to the
// most recently admitted tick; a smaller tick returns ErrOutOfOrderTick and
// leaves the window unchanged.
//
// Expired entries are removed by compacting the surviving ones to the front
// of the same backing array with copy, rather than by reslicing from the
// front (events = events[cut:]). Reslicing-only eviction never reclaims the
// space it drifts past: the header's start pointer only ever advances, so
// the array's usable capacity shrinks on every eviction and a fresh, larger
// array must be allocated each time it runs out -- an allocation for every
// few admits, for the life of the process. Compacting with copy leaves the
// header's pointer at its original base, so the freed space in front is
// immediately available to later appends and the same array serves
// indefinitely once it reaches steady state. copy is safe here even though
// its source and destination overlap; it behaves like memmove, not a naive
// forward-copying loop that would corrupt data moving left over itself.
func (w *Window) Admit(tick int) error {
	if len(w.events) > 0 && tick < w.events[len(w.events)-1] {
		return fmt.Errorf("%w: got %d after %d", ErrOutOfOrderTick, tick, w.events[len(w.events)-1])
	}

	threshold := tick - w.size + 1
	cut := 0
	for cut < len(w.events) && w.events[cut] < threshold {
		cut++
	}
	if cut > 0 {
		n := copy(w.events, w.events[cut:])
		w.events = w.events[:n]
	}

	w.events = append(w.events, tick)
	return nil
}

// Events returns the ticks currently within the window, oldest first.
//
// The returned slice aliases Window's internal storage: it is valid only
// until the next call to Admit, which may overwrite or reallocate it.
// Callers that need to retain a snapshot past the next Admit must copy it,
// for example with slices.Clone.
func (w *Window) Events() []int {
	return w.events
}
```

### The tool

`windowprune` streams: it reads one tick per line with `bufio.Scanner`
rather than buffering the whole input, matching the fact that a rate
limiter processes an unbounded stream, never a batch with a known end.
`run` takes the argument slice, an `io.Reader` for stdin, and an
`io.Writer` for stdout, and returns an error instead of calling `os.Exit`,
so a test can drive it with a `strings.Reader` and a `bytes.Buffer` with no
process boundary involved. Every failure `run` can produce -- a bad flag, a
non-positive `-window`, a non-integer tick, an out-of-order tick -- is
something the caller fixes by changing the command line or the input, so
all of them wrap `errUsage` and `main` maps that to exit code 2. Exit code
1 is reserved for a genuine I/O failure reading stdin, which `scanner.Err()`
would report but which a well-formed pipe never triggers.

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
// or the input stream: a bad flag, an invalid window size, a non-integer
// tick, or an out-of-order tick. main maps it to exit code 2; every other
// error maps to exit code 1.
var errUsage = errors.New("usage")

// run reads one logical tick per line from stdin, admits each into a Window
// of the configured size, and writes the current window and its capacity to
// stdout after every line. It never touches os.Args or os.Exit, so it can be
// exercised in a test with a strings.Reader and a bytes.Buffer.
func run(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("windowprune", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	size := fs.Int("window", 10, "sliding window width, in logical ticks")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}

	w, err := NewWindow(*size)
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
		tick, err := strconv.Atoi(text)
		if err != nil {
			return fmt.Errorf("%w: line %d: invalid tick %q", errUsage, line, text)
		}
		if err := w.Admit(tick); err != nil {
			return fmt.Errorf("%w: line %d: %v", errUsage, line, err)
		}
		fmt.Fprintf(stdout, "tick=%d window=%v cap=%d\n", tick, w.Events(), cap(w.Events()))
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("reading stdin: %w", err)
	}
	return nil
}

func main() {
	flag.CommandLine.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: windowprune -window N")
		fmt.Fprintln(os.Stderr, "reads one integer logical tick per line from stdin.")
	}
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "windowprune:", err)
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}
```

Run it:

```bash
printf '1\n2\n3\n9\n10\n' | go run . -window 5
printf '5\n3\n' | go run . -window 5
```

Expected output:

```text
tick=1 window=[1] cap=1
tick=2 window=[1 2] cap=2
tick=3 window=[1 2 3] cap=4
tick=9 window=[9] cap=4
tick=10 window=[9 10] cap=4
tick=5 window=[5] cap=1
windowprune: usage: line 2: windowprune: tick must not be smaller than the previous tick: got 3 after 5
```

Ticks 9 and 10 evict 1, 2, and 3 outright -- none of them fall within
`[9-5+1, 9] = [5,9]` -- and the window's capacity does not need to grow
further to hold them: it stays at 4, the size the third admission already
grew it to, because compaction reused the space the eviction freed instead
of drifting forward into fresh territory. The second run shows the
out-of-order rejection: tick 3 arriving after tick 5 is invalid for a
monotonically-arriving event stream, and `run` reports it on line 2 with
exit code 2, wrapping the same `ErrOutOfOrderTick` the package returns.

### Tests

`TestAdmitSlidesWindow` is the table of ordinary sliding behavior: ticks all
within the window, a jump that evicts everything, duplicate ticks (which are
not out of order), a window of size one, and a single admission.
`TestAdmitRejectsOutOfOrderTick` confirms a smaller tick is rejected and
leaves the window untouched. `TestEventsAliasesInternalStorage` pins the
documented aliasing contract directly: a snapshot taken from `Events`
silently changes when a later `Admit` compacts in place, because the
snapshot was never a copy.

`TestCompactionReallocatesFarLessThanFrontTruncationAlone` is the heart of
the module. `naiveWindow` is unexported and never reachable from the
package's API; the test runs both it and `Window` over the identical,
long tick sequence and counts how many times each one's backing array
changes identity (via `unsafe.SliceData`), asserting only the inequality
`compactReallocs < naiveReallocs` -- never an exact count, since the
runtime's own growth curve is not part of this module's contract.
`TestRun` drives the command end to end, including the exact stdout shown
above and every usage-error path.

Create `windowprune_test.go`:

```go
package main

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"
	"unsafe"
)

// naiveWindow is the antipattern this module warns about: it evicts by
// reslicing from the front (events = events[cut:]) and never compacts, so
// its header's pointer only ever advances -- once the remaining capacity
// runs out, append must allocate a brand new array, over and over.
type naiveWindow struct {
	size   int
	events []int
}

func (w *naiveWindow) admit(tick int) {
	threshold := tick - w.size + 1
	cut := 0
	for cut < len(w.events) && w.events[cut] < threshold {
		cut++
	}
	w.events = w.events[cut:]
	w.events = append(w.events, tick)
}

func TestNewWindowRejectsNonPositiveSize(t *testing.T) {
	t.Parallel()

	for _, size := range []int{0, -1, -5} {
		if _, err := NewWindow(size); !errors.Is(err, ErrInvalidWindow) {
			t.Errorf("NewWindow(%d) error = %v, want ErrInvalidWindow", size, err)
		}
	}
}

func TestAdmitSlidesWindow(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		size  int
		ticks []int
		want  []int
	}{
		{name: "all within window", size: 5, ticks: []int{1, 2, 3}, want: []int{1, 2, 3}},
		{name: "evicts everything older", size: 5, ticks: []int{1, 2, 3, 9, 10}, want: []int{9, 10}},
		{name: "duplicate ticks are not out of order", size: 3, ticks: []int{4, 4, 4}, want: []int{4, 4, 4}},
		{name: "window of one keeps only the newest tick group", size: 1, ticks: []int{1, 2, 3}, want: []int{3}},
		{name: "single tick", size: 5, ticks: []int{7}, want: []int{7}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			w, err := NewWindow(tc.size)
			if err != nil {
				t.Fatalf("NewWindow: %v", err)
			}
			for _, tick := range tc.ticks {
				if err := w.Admit(tick); err != nil {
					t.Fatalf("Admit(%d): %v", tick, err)
				}
			}
			if got := w.Events(); !slices.Equal(got, tc.want) {
				t.Fatalf("Events() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAdmitRejectsOutOfOrderTick(t *testing.T) {
	t.Parallel()

	w, err := NewWindow(5)
	if err != nil {
		t.Fatalf("NewWindow: %v", err)
	}
	if err := w.Admit(10); err != nil {
		t.Fatalf("Admit(10): %v", err)
	}
	if err := w.Admit(9); !errors.Is(err, ErrOutOfOrderTick) {
		t.Fatalf("Admit(9) after 10: err = %v, want ErrOutOfOrderTick", err)
	}
	if got := w.Events(); !slices.Equal(got, []int{10}) {
		t.Fatalf("Events() after rejected admit = %v, want unchanged [10]", got)
	}
}

// TestEventsAliasesInternalStorage pins the aliasing contract: a slice
// returned by Events can have its contents rewritten by a later Admit even
// though the snapshot's own len and cap never change. Three admits into a
// window of size 3 leave enough spare capacity that a fourth, window-
// clearing admit compacts in place, so the old snapshot silently stops
// reading "1" at index 0.
func TestEventsAliasesInternalStorage(t *testing.T) {
	t.Parallel()

	w, err := NewWindow(3)
	if err != nil {
		t.Fatalf("NewWindow: %v", err)
	}
	for _, tick := range []int{1, 2, 3} {
		if err := w.Admit(tick); err != nil {
			t.Fatalf("Admit(%d): %v", tick, err)
		}
	}
	snapshot := w.Events()
	if snapshot[0] != 1 {
		t.Fatalf("snapshot[0] = %d, want 1", snapshot[0])
	}

	if err := w.Admit(6); err != nil { // evicts 1, 2, 3; compacts in place
		t.Fatalf("Admit(6): %v", err)
	}
	if unsafe.SliceData(snapshot) != unsafe.SliceData(w.Events()) {
		t.Fatal("backing array changed; this test needs the in-place compaction path to fire")
	}
	if snapshot[0] != 6 {
		t.Fatalf("snapshot[0] = %d after a later Admit, want 6: it aliases internal storage", snapshot[0])
	}
}

// TestCompactionReallocatesFarLessThanFrontTruncationAlone is the heart of
// the module: over the same long tick sequence, compaction must trigger
// strictly fewer backing-array replacements than front-truncation alone.
// Exact counts are a runtime detail and are deliberately not asserted.
func TestCompactionReallocatesFarLessThanFrontTruncationAlone(t *testing.T) {
	t.Parallel()

	const size = 8
	const ticks = 2000

	w, err := NewWindow(size)
	if err != nil {
		t.Fatalf("NewWindow: %v", err)
	}
	naive := &naiveWindow{size: size}

	var compactReallocs, naiveReallocs int
	var lastCompact, lastNaive unsafe.Pointer

	for tick := 0; tick < ticks; tick++ {
		if err := w.Admit(tick); err != nil {
			t.Fatalf("Admit(%d): %v", tick, err)
		}
		naive.admit(tick)

		if p := unsafe.Pointer(unsafe.SliceData(w.Events())); p != lastCompact {
			if lastCompact != nil {
				compactReallocs++
			}
			lastCompact = p
		}
		if p := unsafe.Pointer(unsafe.SliceData(naive.events)); p != lastNaive {
			if lastNaive != nil {
				naiveReallocs++
			}
			lastNaive = p
		}
	}

	if !(compactReallocs < naiveReallocs) {
		t.Fatalf("reallocations: compact = %d, naive = %d; want compact < naive", compactReallocs, naiveReallocs)
	}
}

func TestRun(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		args    []string
		stdin   string
		want    string
		wantErr bool
	}{
		{name: "sliding window", args: []string{"-window", "5"}, stdin: "1\n2\n3\n9\n10\n", want: "tick=1 window=[1] cap=1\n" +
			"tick=2 window=[1 2] cap=2\ntick=3 window=[1 2 3] cap=4\ntick=9 window=[9] cap=4\ntick=10 window=[9 10] cap=4\n"},
		{name: "blank lines skipped", args: []string{"-window", "5"}, stdin: "1\n\n2\n", want: "tick=1 window=[1] cap=1\ntick=2 window=[1 2] cap=2\n"},
		{name: "non-integer tick", args: []string{"-window", "5"}, stdin: "1\nabc\n", wantErr: true},
		{name: "out-of-order tick", args: []string{"-window", "5"}, stdin: "5\n3\n", wantErr: true},
		{name: "non-positive window", args: []string{"-window", "0"}, stdin: "1\n", wantErr: true},
		{name: "unknown flag", args: []string{"-bogus"}, stdin: "1\n", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var stdout bytes.Buffer
			err := run(tc.args, strings.NewReader(tc.stdin), &stdout)
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

`Admit` is correct when eviction never leaks capacity: `TestCompaction...`
pins that, over the same long run, `Window` triggers strictly fewer
backing-array replacements than the unexported `naiveWindow`, which is
never part of the package's API. The mechanism is `copy(w.events,
w.events[cut:])`, which shifts survivors to the front of the same array the
header has always pointed to, rather than `events = events[cut:]`, which
only ever advances the pointer and permanently loses whatever capacity it
drifts past. Around that core, `NewWindow` rejects a non-positive window
size with `ErrInvalidWindow`, `Admit` rejects a tick smaller than the last
admitted one with `ErrOutOfOrderTick` and leaves the window unchanged, and
`Events`'s aliasing contract is documented and pinned by a test that catches
a snapshot's contents changing underneath it. The tool streams one tick at
a time with `bufio.Scanner`, maps every input mistake to exit code 2, and
reserves exit code 1 for a stdin read failure. Run
`go test -count=1 -race ./...` to confirm the admission table, the
out-of-order rejection, the aliasing pin, the reallocation-count contrast,
and `run`'s end-to-end behavior.

## Resources

- [`copy`](https://go.dev/ref/spec#Appending_and_copying_slices) — the built-in's documented, `memmove`-like behavior for overlapping source and destination.
- [`unsafe.SliceData`](https://pkg.go.dev/unsafe#SliceData) — the diagnostic used here to detect when a slice's backing array has actually changed.
- [Envoy: Local rate limit filter](https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_filters/local_rate_limit_filter) — a production sliding-window limiter with this same eviction shape.
- [Go Wiki: SliceTricks](https://go.dev/wiki/SliceTricks) — in-place filtering and compaction idioms built on the same `copy` behavior.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [11-scatter-gather-fixed-index-results.md](11-scatter-gather-fixed-index-results.md) | Next: [13-slices-concat-segment-merge.md](13-slices-concat-segment-merge.md)
