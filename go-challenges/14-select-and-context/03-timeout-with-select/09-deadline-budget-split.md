# Exercise 9: Splitting one request deadline across sequential sub-steps

A request carries a single overall deadline, but it runs several sub-steps —
validate, then fetch, then persist. Each step must be bounded by whatever time
remains, not by a fresh full budget, or a slow early step starves the later ones
and the total blows past the deadline. This exercise builds that budget arithmetic
by hand with `time.Until`, which is exactly what `context.WithDeadline` formalizes.

## What you'll build

```text
budgetsplit/                  module example.com/pipeline
  go.mod
  pipeline.go                 ErrTimeout; Result; Run(deadline, validate, fetch, persist)
  cmd/demo/main.go            three fast steps under a shared deadline
  pipeline_test.go            all-fast, step-timeout, shrinking-budget, starved-tail, Example
```

Files: `pipeline.go`, `cmd/demo/main.go`, `pipeline_test.go`.
Implement: `Run(deadline time.Time, validate, fetch, persist func() (string, error)) (Result, error)`, each step bounded by the remaining budget from `time.Until(deadline)`.
Test: all fast completes with each result; a slow step returns `ErrTimeout` and does not start later steps; remaining budget shrinks monotonically; a step consuming nearly the whole budget starves the tail.
Verify: `go test -count=1 -race ./...`

### Remaining budget, computed per step

`Run` holds a single `deadline time.Time` for the whole request. Before each step
it computes `remaining := time.Until(deadline)` — the wall-clock left until the
deadline — and runs the step bounded by that remaining slice. Because time elapses
as steps run, `remaining` shrinks from step to step: the fetch gets less than the
validate, the persist gets less than the fetch. If a step overruns its slice, `Run`
returns `ErrTimeout` wrapped with the step name and does *not* start the later
steps — the request already spent its budget, so there is no point continuing.

Each step runs bounded by the same goroutine-plus-buffered-channel-plus-timer shape
as Exercise 4: `runStep` launches the step, hands its result back on a buffered
size-one channel (so a timed-out step's goroutine finishes and exits rather than
leaking), and selects between the result and `time.After(remaining)`. If `remaining`
is already zero or negative when a step begins — because earlier steps consumed the
whole budget — `runStep` returns `ErrTimeout` immediately without even launching the
step. That near-zero-budget case is not an error in the arithmetic; it is the
correct outcome of a request that ran out of time before reaching the tail.

The `Result` records each step's output plus the `remaining` budget observed at the
start of each step, which the test uses to prove the slices shrink monotonically.
Wrapping the step name with `%w` over `ErrTimeout` lets a caller both match
`errors.Is(err, ErrTimeout)` and read which step overran.

Create `pipeline.go`:

```go
package pipeline

import (
	"errors"
	"fmt"
	"time"
)

// ErrTimeout is returned when a step overruns the remaining request budget.
var ErrTimeout = errors.New("pipeline: deadline exceeded")

// Result holds each step's output and the remaining budget observed when that
// step began.
type Result struct {
	Validate  string
	Fetch     string
	Persist   string
	Remaining []time.Duration
}

// runStep runs fn bounded by the remaining budget. A step that overruns returns
// ErrTimeout; its goroutine still completes into the buffered channel and exits.
func runStep(remaining time.Duration, fn func() (string, error)) (string, error) {
	if remaining <= 0 {
		return "", ErrTimeout
	}
	type res struct {
		v   string
		err error
	}
	done := make(chan res, 1) // buffer of 1: a timed-out step can still send and exit
	go func() {
		v, err := fn()
		done <- res{v: v, err: err}
	}()
	select {
	case r := <-done:
		return r.v, r.err
	case <-time.After(remaining):
		return "", ErrTimeout
	}
}

// Run executes validate, fetch, persist in sequence under a single deadline,
// giving each step the budget remaining at the moment it starts. If any step
// overruns, Run returns ErrTimeout wrapped with the step name and does not start
// the later steps.
func Run(deadline time.Time, validate, fetch, persist func() (string, error)) (Result, error) {
	var r Result
	steps := []struct {
		name string
		fn   func() (string, error)
		set  func(string)
	}{
		{"validate", validate, func(s string) { r.Validate = s }},
		{"fetch", fetch, func(s string) { r.Fetch = s }},
		{"persist", persist, func(s string) { r.Persist = s }},
	}
	for _, st := range steps {
		remaining := time.Until(deadline)
		r.Remaining = append(r.Remaining, remaining)
		v, err := runStep(remaining, st.fn)
		if err != nil {
			return r, fmt.Errorf("step %s: %w", st.name, err)
		}
		st.set(v)
	}
	return r, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/pipeline"
)

func main() {
	deadline := time.Now().Add(200 * time.Millisecond)
	r, err := pipeline.Run(deadline,
		func() (string, error) { time.Sleep(10 * time.Millisecond); return "valid", nil },
		func() (string, error) { time.Sleep(10 * time.Millisecond); return "row-42", nil },
		func() (string, error) { time.Sleep(10 * time.Millisecond); return "committed", nil },
	)
	fmt.Printf("validate=%s fetch=%s persist=%s err=%v\n", r.Validate, r.Fetch, r.Persist, err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
validate=valid fetch=row-42 persist=committed err=<nil>
```

### Tests

`TestAllFast` runs three quick steps under a comfortable deadline and asserts each
result is captured. `TestStepTimesOut` gives a short deadline and a slow fetch,
asserting `ErrTimeout`, that fetch started, and that persist did *not* — proving the
pipeline stops on the first overrun. `TestRemainingShrinks` asserts the recorded
remaining budgets strictly decrease across steps. `TestStarvedTail` has an early
step consume nearly the whole budget so the next step gets a near-zero slice and
times out immediately. Steps use `sync/atomic` flags to record what ran.

Create `pipeline_test.go`:

```go
package pipeline

import (
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

func TestAllFast(t *testing.T) {
	t.Parallel()
	deadline := time.Now().Add(500 * time.Millisecond)
	r, err := Run(deadline,
		func() (string, error) { return "ok", nil },
		func() (string, error) { return "data", nil },
		func() (string, error) { return "saved", nil },
	)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if r.Validate != "ok" || r.Fetch != "data" || r.Persist != "saved" {
		t.Fatalf("results = %+v", r)
	}
}

func TestStepTimesOut(t *testing.T) {
	t.Parallel()
	deadline := time.Now().Add(60 * time.Millisecond)
	var fetchStarted, persistStarted atomic.Bool
	_, err := Run(deadline,
		func() (string, error) { return "ok", nil },
		func() (string, error) {
			fetchStarted.Store(true)
			time.Sleep(300 * time.Millisecond)
			return "data", nil
		},
		func() (string, error) {
			persistStarted.Store(true)
			return "saved", nil
		},
	)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if !fetchStarted.Load() {
		t.Fatalf("fetch should have started")
	}
	if persistStarted.Load() {
		t.Fatalf("persist must not start after a timeout")
	}
}

func TestRemainingShrinks(t *testing.T) {
	t.Parallel()
	deadline := time.Now().Add(300 * time.Millisecond)
	r, err := Run(deadline,
		func() (string, error) { time.Sleep(20 * time.Millisecond); return "a", nil },
		func() (string, error) { time.Sleep(20 * time.Millisecond); return "b", nil },
		func() (string, error) { return "c", nil },
	)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(r.Remaining) != 3 {
		t.Fatalf("recorded %d remaining values, want 3", len(r.Remaining))
	}
	for i := 1; i < len(r.Remaining); i++ {
		if r.Remaining[i] >= r.Remaining[i-1] {
			t.Fatalf("remaining not shrinking: %v", r.Remaining)
		}
	}
}

func TestStarvedTail(t *testing.T) {
	t.Parallel()
	deadline := time.Now().Add(80 * time.Millisecond)
	_, err := Run(deadline,
		func() (string, error) { time.Sleep(75 * time.Millisecond); return "a", nil },
		func() (string, error) { time.Sleep(50 * time.Millisecond); return "b", nil },
		func() (string, error) { return "c", nil },
	)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
}

func ExampleRun() {
	deadline := time.Now().Add(time.Second)
	r, err := Run(deadline,
		func() (string, error) { return "v", nil },
		func() (string, error) { return "f", nil },
		func() (string, error) { return "p", nil },
	)
	fmt.Println(r.Validate, r.Fetch, r.Persist, err)
	// Output: v f p <nil>
}
```

## Review

The splitter is correct when each step is bounded by `time.Until(deadline)` at the
moment it starts, the remaining budget strictly shrinks, and the first overrun stops
the pipeline. The mistake this exercise trains against is giving each step a fresh
full budget — then three 100ms steps under a 150ms deadline would each "succeed"
while the request as a whole ran 300ms, blowing the SLA. The near-zero-budget tail
timing out immediately is correct behavior, not a bug: the request spent its time.
Run `go test -race`; each step's goroutine communicates only through its buffered
result channel, so the timed-out steps exit without racing.

## Resources

- [`time.Until`](https://pkg.go.dev/time#Until) — remaining wall-clock to the deadline.
- [`context.WithDeadline`](https://pkg.go.dev/context#WithDeadline) — the standard-library formalization of this budget arithmetic.
- [`errors.Is`](https://pkg.go.dev/errors#Is) — matching `ErrTimeout` through the step-name wrapping.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [08-debounce-events.md](08-debounce-events.md) | Next: [10-graceful-drain-deadline.md](10-graceful-drain-deadline.md)
