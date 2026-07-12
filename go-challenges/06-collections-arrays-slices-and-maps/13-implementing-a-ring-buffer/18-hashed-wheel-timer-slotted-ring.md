# Exercise 18: Hashed Wheel Timer — a Ring Buffer of Timer Buckets

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Every ring buffer earlier in this lesson stores one value per slot. A hashed
timer wheel is the same fixed-size, circular-index array turned to a
different job: each slot holds a *bucket* of pending timers instead of a
single element, and a cursor sweeps around the ring one tick at a time,
firing whatever bucket it lands on. This is how Netty's `HashedWheelTimer`
manages hundreds of thousands of connection idle-timeouts without a
per-timer heap entry, how the Linux kernel's timer wheels schedule
process timeouts, and how Kafka's delayed-operation purgatory batches
request timeouts -- all trade a heap's `O(log n)` insert and expire for a
wheel's `O(1)` amortized cost, at the price of only supporting relative,
tick-granular deadlines instead of arbitrary ones.

The array-indexing trick that makes a *fixed-size* ring represent an
*unbounded* range of delays is what this module actually teaches. A wheel
with N slots can place a 1-tick or an (N-1)-tick delay directly by index.
A longer delay reuses that same slot on a future lap of the wheel, and the
timer records how many additional laps must still pass before it is
genuinely due -- its "rounds" count. Getting the rounds arithmetic off by
one is a real, well-documented bug class in wheel-timer implementations: a
delay that lands on an exact multiple of the slot count fires one full lap
late if the formula does not account for the tick that lands on the target
slot as itself the last lap, not the start of a new one.

This module builds `Wheel[T]`, a generic hashed wheel with cancellable
timers, kept deliberately free of any notion of wall-clock time -- `Advance`
represents one abstract tick, and the caller decides what a tick means (a
`time.Ticker` firing every 100ms, an event loop's own logical clock, or a
test driving it by hand). The off-by-one rounds formula is never left as a
choice in the API; the wheel always uses the corrected one, and the naive
version exists only in the test file as the thing the tests prove wrong.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
hashedwheel/               module example.com/hashedwheel
  go.mod                   go 1.24
  hashedwheel.go           Wheel[T], Handle[T]; New, Schedule, Advance, Cancel; two sentinel errors
  hashedwheel_test.go       tick-timing table, cancel, aliasing, naive-rounds-formula contrast,
                            ExampleWheel
```

- Files: `hashedwheel.go`, `hashedwheel_test.go`.
- Implement: `New[T any](numSlots int) (*Wheel[T], error)` rejecting a non-positive slot count with `ErrInvalidSlots`; `(*Wheel[T]).Schedule(delayTicks int, value T) (Handle[T], error)` rejecting `delayTicks < 1` with `ErrInvalidDelay` and placing the entry using `(delayTicks-1)/numSlots` rounds; `(*Wheel[T]).Advance() []T` firing the current slot's due entries; `(Handle[T]).Cancel()`.
- Test: the exact tick a scheduled value fires on, across delays below, at, and above `numSlots`; a cancelled timer never fires while an uncancelled sibling in the same slot does; `Advance`'s returned slice never aliases wheel state; the naive `delayTicks/numSlots` rounds formula contrasted against the corrected one, pinned as firing one full lap late; and `ExampleWheel` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/13-implementing-a-ring-buffer/18-hashed-wheel-timer-slotted-ring
cd go-solutions/06-collections-arrays-slices-and-maps/13-implementing-a-ring-buffer/18-hashed-wheel-timer-slotted-ring
go mod edit -go=1.24
```

### Buckets instead of values, and the rounds off-by-one

Every other ring in this lesson answers "where does this value go?" with a
single modulus: `head % capacity`. A hashed wheel answers a related but
different question -- "which slot, and how many laps from now?" -- because
the wheel is smaller than the range of delays it must represent. A delay
under `numSlots` ticks needs no laps at all: `slot = (cursor + delayTicks) %
numSlots`. A delay of, say, `3*numSlots + 2` ticks lands on the same slot as
a 2-tick delay, but must not fire until the wheel has swept past that slot
three additional times. That "how many additional times" is the `rounds`
field carried in each bucket entry, decremented once per lap and fired only
once it reaches zero.

The naive way to compute rounds is `delayTicks / numSlots`, and it is wrong
by exactly one whenever `delayTicks` is a multiple of `numSlots`:

```go
// The formula most first drafts write.
rounds := delayTicks / numSlots
```

Walk a 4-slot wheel scheduling a delay of 4: naive rounds is `4/4 = 1`. The
wheel's cursor reaches that slot again after exactly 4 ticks -- the moment
the timer is actually due -- but rounds is 1, not 0, so `Advance`
decrements it and leaves the entry pending for a *second* lap, firing it at
tick 8 instead of tick 4. The fix subtracts one before dividing:
`(delayTicks-1)/numSlots`. For `delayTicks=4, numSlots=4` that gives `3/4 =
0`, so the entry is already due the first time its slot comes back around.
For a delay that is not an exact multiple, say 5 ticks on a 4-slot wheel,
both formulas agree: `(5-1)/4 = 1`, `5/4 = 1`. The two formulas only
diverge at the boundary, which is exactly why the bug survives casual
testing -- most delays chosen at random do not land on it.

Create `hashedwheel.go`:

```go
// Package hashedwheel implements a hashed timer wheel: a ring buffer whose
// slots are not single values but buckets of pending timers, advanced one
// tick at a time. It is the O(1)-amortized alternative to a heap of
// deadlines, the shape behind Netty's HashedWheelTimer (connection idle
// timeouts at massive fan-out) and the Linux kernel's timer wheels.
//
// A wheel with N slots can place a delay of up to N ticks directly by slot
// index. A longer delay reuses that same slot on a later lap and records how
// many additional laps must pass first -- the "rounds" count -- which is
// what turns a fixed-size ring into an unbounded-range timer without ever
// growing the array.
package hashedwheel

import (
	"errors"
	"fmt"
)

// Sentinel errors returned by New and Schedule.
var (
	// ErrInvalidSlots means numSlots was not positive.
	ErrInvalidSlots = errors.New("hashedwheel: numSlots must be positive")
	// ErrInvalidDelay means a Schedule call requested a delay below one tick.
	ErrInvalidDelay = errors.New("hashedwheel: delayTicks must be at least 1")
)

// entry is one scheduled timer sitting in a wheel slot.
type entry[T any] struct {
	rounds    int
	cancelled bool
	value     T
}

// Handle lets a caller cancel a timer it previously scheduled. Cancelling an
// already-fired or already-cancelled Handle is a harmless no-op.
type Handle[T any] struct {
	e *entry[T]
}

// Cancel prevents this timer from firing. It has no effect if the timer
// already fired or was already cancelled.
func (h Handle[T]) Cancel() {
	if h.e != nil {
		h.e.cancelled = true
	}
}

// Wheel is a hashed timer wheel with a fixed number of slots.
//
// Concurrency contract: NOT safe for concurrent use. A Wheel is meant to be
// owned by a single goroutine that calls Advance once per tick and serializes
// any Schedule/Cancel calls from other goroutines itself -- the same shape as
// Netty's HashedWheelTimer, which runs every tick on one dedicated worker.
//
// Aliasing: Advance returns a freshly allocated slice on every call; it never
// aliases wheel-internal storage, so the caller may retain or mutate it
// freely.
type Wheel[T any] struct {
	slots    [][]*entry[T]
	numSlots int
	cursor   int
}

// New returns a Wheel with numSlots slots. It returns ErrInvalidSlots if
// numSlots is not positive.
func New[T any](numSlots int) (*Wheel[T], error) {
	if numSlots <= 0 {
		return nil, fmt.Errorf("%w: got %d", ErrInvalidSlots, numSlots)
	}
	return &Wheel[T]{
		slots:    make([][]*entry[T], numSlots),
		numSlots: numSlots,
	}, nil
}

// Schedule places value delayTicks ticks in the future and returns a Handle
// that can cancel it before it fires. delayTicks must be at least 1: a
// wheel fires timers on ticks, so "fire immediately" is the caller's job,
// not the wheel's. Schedule returns ErrInvalidDelay for delayTicks < 1.
//
// The slot is (cursor+delayTicks) % numSlots and rounds is
// (delayTicks-1) / numSlots: the -1 is what makes a delay that is an exact
// multiple of numSlots fire on the tick it is due, rather than one full lap
// late. See the package tests for the off-by-one this omits when missing.
func (w *Wheel[T]) Schedule(delayTicks int, value T) (Handle[T], error) {
	if delayTicks < 1 {
		return Handle[T]{}, fmt.Errorf("%w: got %d", ErrInvalidDelay, delayTicks)
	}
	slot := (w.cursor + delayTicks) % w.numSlots
	e := &entry[T]{rounds: (delayTicks - 1) / w.numSlots, value: value}
	w.slots[slot] = append(w.slots[slot], e)
	return Handle[T]{e: e}, nil
}

// Advance moves the wheel forward by one tick and returns the values of
// every timer due on the slot it lands on, in the order they were
// scheduled. Cancelled timers in that slot are dropped silently. Timers
// with laps still remaining have their round count decremented and stay in
// the slot for a future visit.
func (w *Wheel[T]) Advance() []T {
	w.cursor = (w.cursor + 1) % w.numSlots
	bucket := w.slots[w.cursor]

	var fired []T
	remaining := bucket[:0]
	for _, e := range bucket {
		switch {
		case e.cancelled:
			// Drop it: freed for GC, never returned, never re-queued.
		case e.rounds == 0:
			fired = append(fired, e.value)
		default:
			e.rounds--
			remaining = append(remaining, e)
		}
	}
	w.slots[w.cursor] = remaining
	return fired
}

// NumSlots reports the wheel's fixed slot count.
func (w *Wheel[T]) NumSlots() int { return w.numSlots }
```

### Using it

A caller drives the wheel by calling `Advance` once per tick -- typically
from a single goroutine reading a `time.Ticker`, though the wheel itself
never touches `time` at all, which is what keeps its tests deterministic
and its type reusable for any notion of "tick." `Schedule` returns a
`Handle[T]`, a thin wrapper around the entry it just queued; the only
operation on it is `Cancel`, and cancelling twice, or cancelling after the
timer already fired, is a safe no-op rather than an error, since a caller
racing a timer's own firing against its own cancellation should not have to
coordinate to avoid a panic.

The concurrency contract is deliberately narrow: a `Wheel` is not safe for
concurrent use, matching how Netty's `HashedWheelTimer` processes every tick
on one dedicated worker and serializes `newTimeout`/`cancel` calls from
other threads into that worker's queue before touching wheel state. This
module does not build that serialization layer -- it is the caller's job,
the same way it is Netty's own outer layer's job, not the wheel's.

`ExampleWheel`, shown in full in the Tests section below, schedules three
timers with different delays -- one due next tick, one due after a full lap,
one cancelled before it can fire -- and prints what each of four `Advance`
calls returns. `go test` runs it and compares its stdout against the `//
Output:` comment, so the usage shown there cannot drift from the code.

### Tests

`TestAdvanceFiresOnTheExpectedTick` is the table this module is built
around: it schedules one value at delays below, at, and above `numSlots`,
and asserts nothing fires on any tick before the delay and exactly the
scheduled value fires on the tick it is due. The "exact multiple of
numSlots" and "two exact laps" cases are the ones that force the rounds
arithmetic's off-by-one to matter; the others pass even with the naive
formula, which is exactly why the bug is easy to ship.

`TestCancelPreventsFiring` schedules two timers into what becomes the same
slot, cancels one, and confirms only the other fires -- including that
calling `Cancel` twice does not panic. `TestAdvanceReturnedSliceDoesNotAliasWheel`
mutates the slice `Advance` returns and confirms the wheel's own state, read
back through a later `Advance`, is unaffected.

`TestNaiveRoundsFormulaFiresOneLapLate` is the module's antipattern
contrast. `entryRoundsNaive` is unexported and unreachable from `Wheel`'s
API; the test hand-injects an entry built with it into the exact slot
`Schedule` would have chosen, drives the wheel through the delay's worth of
ticks, and asserts the naively-built entry has *not* fired where the
correctly-built one has -- then drives one more full lap and confirms it
eventually does, proving the bug delays a timer rather than losing it.

Create `hashedwheel_test.go`:

```go
package hashedwheel

import (
	"errors"
	"fmt"
	"slices"
	"testing"
)

func TestNewRejectsNonPositiveSlots(t *testing.T) {
	t.Parallel()

	for _, n := range []int{0, -1, -5} {
		if _, err := New[string](n); !errors.Is(err, ErrInvalidSlots) {
			t.Errorf("New(%d) err = %v, want ErrInvalidSlots", n, err)
		}
	}
}

func TestScheduleRejectsSubOneTickDelay(t *testing.T) {
	t.Parallel()

	w, err := New[string](4)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, delay := range []int{0, -1, -100} {
		if _, err := w.Schedule(delay, "x"); !errors.Is(err, ErrInvalidDelay) {
			t.Errorf("Schedule(%d, ...) err = %v, want ErrInvalidDelay", delay, err)
		}
	}
}

// TestAdvanceFiresOnTheExpectedTick is the table: schedule a value at a
// known delay and confirm it appears in the fired slice on exactly that
// tick, not before and not after -- across delays below, at, and above
// numSlots, which forces the rounds arithmetic to be exercised.
func TestAdvanceFiresOnTheExpectedTick(t *testing.T) {
	t.Parallel()

	const numSlots = 4
	tests := []struct {
		name  string
		delay int
	}{
		{name: "one tick", delay: 1},
		{name: "last slot before wrap", delay: numSlots - 1},
		{name: "exact multiple of numSlots", delay: numSlots},
		{name: "one lap plus one tick", delay: numSlots + 1},
		{name: "two exact laps", delay: 2 * numSlots},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			w, err := New[string](numSlots)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if _, err := w.Schedule(tc.delay, "due"); err != nil {
				t.Fatalf("Schedule: %v", err)
			}

			var fired []string
			for tick := 1; tick <= tc.delay; tick++ {
				got := w.Advance()
				if tick < tc.delay && len(got) != 0 {
					t.Fatalf("tick %d fired %v, want nothing before tick %d", tick, got, tc.delay)
				}
				fired = append(fired, got...)
			}
			if !slices.Equal(fired, []string{"due"}) {
				t.Fatalf("fired = %v, want [due] exactly on tick %d", fired, tc.delay)
			}
		})
	}
}

func TestCancelPreventsFiring(t *testing.T) {
	t.Parallel()

	w, err := New[string](4)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h, err := w.Schedule(2, "cancel-me")
	if err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if _, err := w.Schedule(2, "keep-me"); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	h.Cancel()
	h.Cancel() // idempotent: cancelling twice must not panic

	fired := w.Advance()
	fired = append(fired, w.Advance()...)
	if !slices.Equal(fired, []string{"keep-me"}) {
		t.Fatalf("fired = %v, want [keep-me]", fired)
	}
}

func TestAdvanceReturnedSliceDoesNotAliasWheel(t *testing.T) {
	t.Parallel()

	w, err := New[int](2)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := w.Schedule(1, 1); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	fired := w.Advance()
	fired[0] = 999 // mutating the caller's copy must not affect the wheel

	if _, err := w.Schedule(1, 2); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	next := w.Advance()
	if !slices.Equal(next, []int{2}) {
		t.Fatalf("second Advance = %v, want [2] (mutation leaked into wheel state)", next)
	}
}

// entryRoundsNaive is the formula most first drafts of a hashed wheel write:
// how many full laps before firing. It is never reachable from Wheel's API;
// it exists to pin the off-by-one Schedule's real formula, (delayTicks-1)/
// numSlots, corrects.
func entryRoundsNaive(delayTicks, numSlots int) int {
	return delayTicks / numSlots
}

// TestNaiveRoundsFormulaFiresOneLapLate isolates the rounds arithmetic from
// everything else: for a delay that is an exact multiple of numSlots, the
// naive formula computes one extra round, so an entry built with it needs a
// full extra lap (numSlots more ticks) before firing. This is the module's
// antipattern, pinned as an observable difference rather than asserted by
// inspection alone.
func TestNaiveRoundsFormulaFiresOneLapLate(t *testing.T) {
	t.Parallel()

	const numSlots = 4
	const delay = numSlots // the exact-multiple case the naive formula misses

	w, err := New[string](numSlots)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := w.Schedule(delay, "correct"); err != nil {
		t.Fatalf("Schedule: %v", err)
	}

	// Hand-inject a second entry into the same slot Schedule would choose,
	// using the naive rounds count instead of the corrected one, to isolate
	// the formula from the rest of Schedule.
	slot := (w.cursor + delay) % numSlots
	w.slots[slot] = append(w.slots[slot], &entry[string]{
		rounds: entryRoundsNaive(delay, numSlots),
		value:  "naive",
	})

	var fired []string
	for tick := 1; tick <= delay; tick++ {
		fired = append(fired, w.Advance()...)
	}
	if !slices.Contains(fired, "correct") {
		t.Fatalf("fired = %v, want it to contain \"correct\" by tick %d", fired, delay)
	}
	if slices.Contains(fired, "naive") {
		t.Fatalf("fired = %v: naive-rounds entry fired on time, want it still pending (one lap late is the bug)", fired)
	}

	// Confirm the naive entry does eventually fire, one full lap later,
	// proving it was delayed rather than lost.
	for tick := 1; tick <= numSlots; tick++ {
		fired = w.Advance()
	}
	if !slices.Contains(fired, "naive") {
		t.Fatalf("naive entry never fired even after an extra lap: %v", fired)
	}
}

// ExampleWheel schedules three timers -- one due next tick, one due after a
// full lap, and one cancelled before it can fire -- and advances through
// four ticks, printing what each tick fires.
func ExampleWheel() {
	w, err := New[string](4)
	if err != nil {
		panic(err)
	}

	if _, err := w.Schedule(1, "one-tick"); err != nil {
		panic(err)
	}
	if _, err := w.Schedule(4, "one-lap"); err != nil {
		panic(err)
	}
	cancelMe, err := w.Schedule(2, "cancelled")
	if err != nil {
		panic(err)
	}
	cancelMe.Cancel()

	for tick := 1; tick <= 4; tick++ {
		fmt.Printf("tick=%d fired=%v\n", tick, w.Advance())
	}
	// Output:
	// tick=1 fired=[one-tick]
	// tick=2 fired=[]
	// tick=3 fired=[]
	// tick=4 fired=[one-lap]
}
```

## Review

The wheel is correct when a timer fires on the tick its delay actually
names, at every delay -- not just the delays under `numSlots` that never
exercise the rounds counter at all. `(delayTicks-1)/numSlots` is the
formula that gets the exact-multiple boundary right; `delayTicks/numSlots`
is the one that ships and is one full lap late exactly there, which
`TestNaiveRoundsFormulaFiresOneLapLate` pins directly by hand-building an
entry with the wrong formula and watching it miss its tick. Around that
core, `New` rejects a non-positive slot count with `ErrInvalidSlots`,
`Schedule` rejects a sub-one-tick delay with `ErrInvalidDelay`, `Cancel` is
idempotent and safe even after a timer has already fired, and `Advance`
always returns a freshly allocated slice that never aliases the wheel's
internal buckets. The type is explicitly not safe for concurrent use,
matching the single-worker-thread design of the real systems it mirrors;
serializing `Schedule` and `Cancel` calls against `Advance` is the caller's
job. `ExampleWheel` is the executable documentation: `go test` verifies its
output. Run `go test -count=1 -race ./...`.

## Resources

- [Netty `HashedWheelTimer`](https://netty.io/4.1/api/io/netty/util/HashedWheelTimer.html) — the production implementation this module's slot-and-rounds design mirrors, including its own documented rounding behavior.
- [Varghese and Lauck, "Hashed and Hierarchical Timing Wheels" (1987)](https://www.cs.columbia.edu/~nahum/w6998/papers/ton97-timing-wheels.pdf) — the original paper describing the data structure and its O(1) amortized cost.
- [Linux kernel timer wheel](https://www.kernel.org/doc/html/latest/timers/timer_migration.html) — a production kernel-level use of the same slotted-ring approach for process and network timeouts.
- [Kafka source: `SystemTimer`](https://github.com/apache/kafka/blob/trunk/server-common/src/main/java/org/apache/kafka/server/util/timer/SystemTimer.java) — Kafka's own hierarchical timing-wheel implementation behind its delayed-operation purgatory.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [17-lock-free-spsc-atomic-ring.md](17-lock-free-spsc-atomic-ring.md) | Next: [19-multi-reader-fanout-ring-watermark.md](19-multi-reader-fanout-ring-watermark.md)
