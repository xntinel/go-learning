# Exercise 24: Saga Pattern Incomplete Rollback Due to Loop Variable Scope Error in Backward Iteration

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A saga rolls back every already-completed step when a later step fails,
undoing them in reverse order (LIFO) since a later step may depend on an
earlier one's resources still existing. The rollback loop's bounds have to
be computed fresh for backward iteration — `for j := completed - 1; j >=
0; j--` — but the tempting shortcut reuses the *outer forward loop's* own
index variable and its forward-counting pattern instead, and because that
variable already equals the count of completed steps at the moment of
failure, the reused-and-incremented version's loop condition is false
before its very first check: the rollback loop runs zero times, and every
completed step's resources stay held forever. This module is fully
self-contained: its own `go mod init`, all code inline, its own demo and
tests.

## What you'll build

```text
saga/                        independent module: example.com/saga-rollback-loop-scope-error
  go.mod                      go 1.21
  saga.go                      Resource, Step, Run
  cmd/
    demo/
      main.go                  runnable demo: two completed steps, a third that fails, full rollback
  saga_test.go                  table over success, early/mid/late failure, plus a rollback-failure edge case
```

- Files: `saga.go`, `cmd/demo/main.go`, `saga_test.go`.
- Implement: `Run(steps []Step) (rolledBack []string, err error)` that rolls back every completed step's resources in reverse order on the first `Do` failure.
- Test: a table over no-failure, first-step-failure, middle-step-failure, and last-step-failure, each asserting the exact reverse-order rollback list; a further case where a resource's own release fails mid-rollback, asserting the labeled break stops further unwinding.
- Verify: `go test -count=1 -race ./...`.

```bash
mkdir -p ~/go-exercises/saga-rollback-loop-scope-error/cmd/demo
cd ~/go-exercises/saga-rollback-loop-scope-error
go mod init example.com/saga-rollback-loop-scope-error
```

### Why reusing the forward index breaks the backward loop's bounds

At the moment step `i` fails, exactly `i` earlier steps have succeeded —
so `completed`, incremented once per success, equals `i` too. The buggy
rollback loop leans on that coincidence and reuses `i` directly, keeping
the forward loop's own increment style instead of writing a proper
backward count:

```go
for i, s := range steps {
	if doErr := s.Do(); doErr != nil {
		err = fmt.Errorf("step %q failed: %w", s.Name, doErr)

		for j := i; j < completed; j++ { // BUG: i == completed here, so j < completed is false immediately
			// ... release steps[j]'s resources ...
		}
		return rolledBack, err
	}
	completed++
}
```

`i` and `completed` are numerically equal at the point of failure, which
is exactly what makes this bug invisible in a quick read: `j := i` looks
like a reasonable starting point, and `j < completed` looks like a
reasonable bound — but together they describe an interval of *zero
steps*, because the loop starts exactly where it was already told to
stop. The rollback code runs, returns no error of its own, and the
function's `return rolledBack, err` executes normally — nothing crashes,
nothing logs a rollback failure, and `rolledBack` is simply empty. Every
resource acquired by every completed step is silently left held: reserved
inventory never released, a payment authorization never voided, a lock
never dropped — a saga that believes it failed cleanly while quietly
leaking every piece of state it was supposed to unwind. The fix computes
the backward bound explicitly, independent of any forward-loop variable:
`for j := completed - 1; j >= 0; j--`, which correctly visits every
completed step from the most recent back to the first.

Create `saga.go`:

```go
package saga

import "fmt"

// Resource is a unit of state acquired while a Step's Do runs; Release
// undoes it. A Step can acquire more than one Resource, in order.
type Resource struct {
	Name    string
	Release func() error
}

// Step is one forward action of a saga.
type Step struct {
	Name      string
	Do        func() error
	Resources []Resource
}

// Run executes steps in order. If any step's Do fails, Run rolls back
// every already-completed step in reverse order (LIFO) -- the last step
// that succeeded is unwound first -- releasing each step's own resources
// in reverse order too. If a resource fails to release, rollback stops
// immediately via a labeled break out of both loops, rather than
// continuing to unwind state it can no longer account for.
func Run(steps []Step) (rolledBack []string, err error) {
	completed := 0

	for _, s := range steps {
		if doErr := s.Do(); doErr != nil {
			err = fmt.Errorf("step %q failed: %w", s.Name, doErr)

		Rollback:
			for j := completed - 1; j >= 0; j-- {
				step := steps[j]
				for k := len(step.Resources) - 1; k >= 0; k-- {
					if relErr := step.Resources[k].Release(); relErr != nil {
						err = fmt.Errorf("%w (rollback also failed releasing %s.%s: %v)", err, step.Name, step.Resources[k].Name, relErr)
						break Rollback
					}
					rolledBack = append(rolledBack, fmt.Sprintf("%s.%s", step.Name, step.Resources[k].Name))
				}
			}
			return rolledBack, err
		}
		completed++
	}
	return nil, nil
}
```

### The runnable demo

The demo runs two resource-acquiring steps followed by one that fails,
showing both earlier steps' resources released in reverse order.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/saga-rollback-loop-scope-error"
)

func main() {
	steps := []saga.Step{
		{
			Name: "reserve-inventory",
			Do:   func() error { return nil },
			Resources: []saga.Resource{
				{Name: "hold", Release: func() error {
					fmt.Println("released reserve-inventory.hold")
					return nil
				}},
			},
		},
		{
			Name: "charge-card",
			Do:   func() error { return nil },
			Resources: []saga.Resource{
				{Name: "auth", Release: func() error {
					fmt.Println("released charge-card.auth")
					return nil
				}},
			},
		},
		{
			Name: "ship-order",
			Do:   func() error { return errors.New("carrier api unreachable") },
		},
	}

	rolledBack, err := saga.Run(steps)
	fmt.Println("error:", err)
	fmt.Println("rolled back:", rolledBack)
}
```

```bash
go run ./cmd/demo
```

Expected output:

```text
released charge-card.auth
released reserve-inventory.hold
error: step "ship-order" failed: carrier api unreachable
rolled back: [charge-card.auth reserve-inventory.hold]
```

### Tests

`TestRun` is a table over five shapes: complete success, a failure on the
very first step (nothing to roll back), a failure partway through, a
failure on the last step (every prior step rolls back, and a step with
multiple resources unwinds them in reverse too), and the edge case where a
resource's own `Release` fails during rollback — asserting the labeled
`break` stops further unwinding rather than silently skipping the failed
resource and continuing.

Create `saga_test.go`:

```go
package saga

import (
	"errors"
	"reflect"
	"testing"
)

// resource builds a Resource whose Release appends its own qualified name
// to log and, if failAt is non-nil, returns that error instead of
// succeeding.
func resource(log *[]string, step, name string, failErr error) Resource {
	return Resource{
		Name: name,
		Release: func() error {
			if failErr != nil {
				return failErr
			}
			*log = append(*log, step+"."+name)
			return nil
		},
	}
}

func TestRun(t *testing.T) {
	tests := []struct {
		name           string
		buildSteps     func(log *[]string) []Step
		wantErr        bool
		wantRolledBack []string
	}{
		{
			name: "all steps succeed: no rollback",
			buildSteps: func(log *[]string) []Step {
				return []Step{
					{Name: "a", Do: func() error { return nil }},
					{Name: "b", Do: func() error { return nil }},
				}
			},
			wantErr:        false,
			wantRolledBack: nil,
		},
		{
			name: "first step fails: nothing to roll back",
			buildSteps: func(log *[]string) []Step {
				return []Step{
					{Name: "a", Do: func() error { return errors.New("boom") }},
					{Name: "b", Do: func() error { return nil }},
				}
			},
			wantErr:        true,
			wantRolledBack: nil,
		},
		{
			name: "middle step fails: earlier steps roll back in reverse order",
			buildSteps: func(log *[]string) []Step {
				return []Step{
					{Name: "a", Do: func() error { return nil }, Resources: []Resource{resource(log, "a", "r1", nil)}},
					{Name: "b", Do: func() error { return nil }, Resources: []Resource{resource(log, "b", "r1", nil)}},
					{Name: "c", Do: func() error { return errors.New("boom") }},
				}
			},
			wantErr:        true,
			wantRolledBack: []string{"b.r1", "a.r1"},
		},
		{
			name: "last step fails: every prior step rolls back, resources reversed within a step too",
			buildSteps: func(log *[]string) []Step {
				return []Step{
					{Name: "a", Do: func() error { return nil }, Resources: []Resource{
						resource(log, "a", "r1", nil),
						resource(log, "a", "r2", nil),
					}},
					{Name: "b", Do: func() error { return errors.New("boom") }},
				}
			},
			wantErr:        true,
			wantRolledBack: []string{"a.r2", "a.r1"},
		},
		{
			// Edge case: a resource release itself fails during rollback.
			// A labeled break must stop the whole rollback immediately
			// rather than silently continuing to unwind state it can no
			// longer account for.
			name: "resource release failure during rollback stops further unwinding",
			buildSteps: func(log *[]string) []Step {
				releaseErr := errors.New("release backend down")
				return []Step{
					{Name: "a", Do: func() error { return nil }, Resources: []Resource{resource(log, "a", "r1", nil)}},
					{Name: "b", Do: func() error { return nil }, Resources: []Resource{resource(log, "b", "r1", releaseErr)}},
					{Name: "c", Do: func() error { return errors.New("boom") }},
				}
			},
			wantErr:        true,
			wantRolledBack: nil, // b.r1 fails to release before a.r1 is ever reached
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var log []string
			steps := tc.buildSteps(&log)

			rolledBack, err := Run(steps)

			if tc.wantErr && err == nil {
				t.Fatalf("Run() error = nil, want an error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Run() error = %v, want nil", err)
			}
			if !reflect.DeepEqual(rolledBack, tc.wantRolledBack) {
				t.Fatalf("rolledBack = %v, want %v", rolledBack, tc.wantRolledBack)
			}
		})
	}
}
```

Run: `go test -count=1 -race ./...`.

## Review

`Run` is correct when every completed step's resources appear in
`rolledBack` in exact reverse order of acquisition, for a failure at any
position — first, middle, or last — because the rollback loop's bound is
computed independently as `completed - 1`, never borrowed from the
forward loop's own index. The mistake this design avoids is assuming a
loop variable that happens to hold the right *value* at one moment is
safe to reuse for an unrelated loop with the opposite direction and a
different exit condition: `i` and `completed` being numerically equal at
the moment of failure is a coincidence of this specific control flow, not
a guarantee that reusing one for the other is sound. The nested loop and
its labeled `break` are the other half of the design: releasing a step's
own resources also has to go in reverse, and if any single release fails,
the labeled break stops the *entire* rollback rather than pretending the
remaining steps were also safely unwound.

## Resources

- [Go Specification: For statements](https://go.dev/ref/spec#For_statements) — the three-clause `for` and how its init, condition, and post statements interact.
- [Go Specification: Labeled statements](https://go.dev/ref/spec#Labeled_statements) — `break Label` terminates the labeled `for` from inside a nested loop.
- [Saga pattern (microservices.io)](https://microservices.io/patterns/data/saga.html) — compensating transactions and why they must undo in reverse of the order they were applied.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [23-event-dispatch-panic-recovery-masks-failure.md](23-event-dispatch-panic-recovery-masks-failure.md) | Next: [25-sliding-window-rate-limiter.md](25-sliding-window-rate-limiter.md)
