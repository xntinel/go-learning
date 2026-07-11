# Exercise 6: Labeled Break/Continue in a Two-Dimensional Availability Search

Finding the first free window in a scheduler's availability matrix — a booking
system, a meeting-room finder, a capacity-placement scan — is the textbook case
where a *label* earns its keep. The search is two nested loops (days x half-hour
slots), and on the first fit it must exit *both* loops, while a fully-unusable tail
of a day should skip straight to the next day. A plain `break` exits only the inner
loop and the search wastefully continues; a labeled `break outer` and `continue
outer` collapse the nested search into a single decision point.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
scheduler/                   module example.com/scheduler
  go.mod
  scheduler.go               FindSlot(booked, duration) (Slot, error); ErrNoSlot, ErrInvalidDuration
  scheduler_test.go          first-fit, no-fit, labeled-continue skip, single-cell, stops-at-first
  cmd/demo/
    main.go                  scans a small week grid and prints the first fitting slot
```

- Files: `scheduler.go`, `scheduler_test.go`, `cmd/demo/main.go`.
- Implement: `FindSlot(booked [][]bool, duration time.Duration) (Slot, error)` — a `for day := range booked` over a `for slot := range booked[day]`, a labeled `break outer` on the first run of free slots long enough, and a labeled `continue outer` to skip a day once its remaining slots cannot fit.
- Test: match in the middle (exact first-fit day/slot), no match (`ErrNoSlot`), a match reachable only after a labeled-continue skip, a single-cell grid, and a case proving the search returns the FIRST fit, not a later one.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/scheduler/cmd/demo
cd ~/go-exercises/scheduler
go mod init example.com/scheduler
```

### Where a label pays for itself, and where it does not

The grid is `booked [][]bool`: `booked[day][slot]` is `true` when that half-hour is
already taken. A request for `duration` needs `ceil(duration / 30min)` consecutive
free slots within a single day. The search scans days outer, slots inner, tracking
the current run of consecutive free slots; when the run reaches the needed length,
that is the first fit.

Two label uses appear here, and each is the *right* tool for its job:

- `break outer` on a match. The match is found deep in the inner loop, but the
  answer ends the entire search — there is no point scanning the rest of the day or
  any later day. A plain `break` would leave the outer `day` loop running, scanning
  for a "better" slot that the first-fit contract says we do not want. The label
  makes "stop everything, we found it" a single statement.
- `continue outer` to abandon a day early. The moment a booked slot leaves fewer
  than `need` slots remaining in the current day, no window can fit in what is left,
  so there is no reason to keep scanning that day's slots — jump to the next day.
  A plain `continue` would only advance to the next slot in the same doomed day.

The honest counterpoint, which the concepts file makes: a label is not always the
right answer. If the inner loop's body were substantial, extracting "does a fit
start on this day?" into a helper that returns the slot (or -1) would read better
than a label, because an early `return` from a helper is even cleaner than
`break outer`. Labels win *here* because the state the loop carries (the running
count, the day and slot indices) is cheap to keep inline and the two loops are
tight. Reach for a label when a helper would have to close over several locals;
otherwise prefer the helper.

Create `scheduler.go`:

```go
package scheduler

import (
	"errors"
	"time"
)

// slotLen is the granularity of the availability grid.
const slotLen = 30 * time.Minute

var (
	// ErrNoSlot means no free window long enough exists in the grid.
	ErrNoSlot = errors.New("no available slot")
	// ErrInvalidDuration means a non-positive duration was requested.
	ErrInvalidDuration = errors.New("duration must be positive")
)

// Slot identifies a window by its day index and starting slot index.
type Slot struct {
	Day   int
	Start int
}

// FindSlot returns the earliest window of consecutive free half-hour slots that
// fits duration, scanning days in order and slots within a day in order. It
// returns the first fit (never a later one) or ErrNoSlot.
func FindSlot(booked [][]bool, duration time.Duration) (Slot, error) {
	if duration <= 0 {
		return Slot{}, ErrInvalidDuration
	}
	need := int((duration + slotLen - 1) / slotLen) // ceil

	found := Slot{Day: -1}
outer:
	for day := range booked {
		run := 0
		for slot := range booked[day] {
			if booked[day][slot] {
				run = 0
				// If the rest of this day is too short to fit, skip to the next day.
				if len(booked[day])-slot-1 < need {
					continue outer
				}
				continue
			}
			run++
			if run >= need {
				found = Slot{Day: day, Start: slot - need + 1}
				break outer
			}
		}
	}

	if found.Day < 0 {
		return Slot{}, ErrNoSlot
	}
	return found, nil
}
```

### The runnable demo

The demo scans a three-day grid where day 0 is nearly full, day 1 has a gap in the
middle, and asks for a one-hour window (two consecutive slots).

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/scheduler"
)

func main() {
	// true = booked. Six half-hour slots per day.
	week := [][]bool{
		{false, true, false, true, false, true},    // day 0: no two-in-a-row free
		{true, true, false, false, true, true},     // day 1: slots 2-3 free
		{false, false, false, false, false, false}, // day 2: wide open
	}

	slot, err := scheduler.FindSlot(week, time.Hour)
	if err != nil {
		fmt.Printf("no slot: %v\n", err)
		return
	}
	fmt.Printf("first hour-long slot: day %d starting slot %d\n", slot.Day, slot.Start)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
first hour-long slot: day 1 starting slot 2
```

### Tests

The suite is table-driven over grids that exercise each control-flow path. The
"skip via labeled continue" case has a day whose only free run is broken by a
booking near the end, forcing the `continue outer` to reach a later day. The
"stops at first fit" case has two valid windows and asserts the earlier one is
returned, proving the `break outer` really does stop the search.

Create `scheduler_test.go`:

```go
package scheduler

import (
	"errors"
	"testing"
	"time"
)

func TestFindSlot(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		grid     [][]bool
		duration time.Duration
		want     Slot
		wantErr  error
	}{
		{
			name:     "first fit in the middle of a day",
			grid:     [][]bool{{true, false, false, true}},
			duration: time.Hour, // needs 2 slots
			want:     Slot{Day: 0, Start: 1},
		},
		{
			name:     "no fit anywhere",
			grid:     [][]bool{{true, false, true, false}},
			duration: time.Hour,
			wantErr:  ErrNoSlot,
		},
		{
			name: "reachable only after skipping an unusable day tail",
			grid: [][]bool{
				{false, true, false, false}, // free run 2-3 is only length 2... but slot 1 booked
				{false, false, false, false},
			},
			duration: 90 * time.Minute, // needs 3 slots
			want:     Slot{Day: 1, Start: 0},
		},
		{
			name:     "single free cell fits a single slot",
			grid:     [][]bool{{false}},
			duration: 20 * time.Minute, // ceil to 1 slot
			want:     Slot{Day: 0, Start: 0},
		},
		{
			name: "returns the first fit, not a later one",
			grid: [][]bool{
				{false, false},        // day 0 fits at slot 0
				{false, false, false}, // day 1 also fits
			},
			duration: time.Hour,
			want:     Slot{Day: 0, Start: 0},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := FindSlot(tc.grid, tc.duration)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("FindSlot() = %v, want nil", err)
			}
			if got != tc.want {
				t.Fatalf("slot = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestFindSlotRejectsInvalidDuration(t *testing.T) {
	t.Parallel()

	if _, err := FindSlot([][]bool{{false}}, 0); !errors.Is(err, ErrInvalidDuration) {
		t.Fatalf("err = %v, want ErrInvalidDuration", err)
	}
	if _, err := FindSlot([][]bool{{false}}, -time.Hour); !errors.Is(err, ErrInvalidDuration) {
		t.Fatalf("err = %v, want ErrInvalidDuration", err)
	}
}
```

## Review

The search is correct when the label targets exactly the loop each control-flow
decision means to affect: `break outer` ends the whole search on the first fit (so
the first-fit contract holds and no later window is returned), and `continue outer`
abandons a day the instant its remaining slots cannot fit the request. The trap
this guards against is the plain `break`, which exits only the inner loop and lets
the outer scan continue past the answer — a bug that silently returns a later slot
or wastes work. `TestFindSlot`'s "returns the first fit" case is the direct proof
that the labeled break stops the search. The honest caveat from the concepts file
still stands: a label is the right call here because the loops are tight and carry
little state; when an inner body grows, prefer a helper with an early `return`. Run
`go test -count=1 -race ./...`.

## Resources

- [Go Specification: Break statements](https://go.dev/ref/spec#Break_statements) — labeled break targeting an enclosing loop.
- [Go Specification: Continue statements](https://go.dev/ref/spec#Continue_statements) — labeled continue.
- [Go Specification: For statements](https://go.dev/ref/spec#For_statements) — range over a slice and nested loops.
- [Effective Go: Control structures](https://go.dev/doc/effective_go#control-structures) — when labels read well and when to prefer a helper.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [05-bounded-worker-pool.md](05-bounded-worker-pool.md) | Next: [07-graceful-shutdown-drain.md](07-graceful-shutdown-drain.md)
