# Exercise 5: Budget Guard — Skip Expensive Work When Too Little Time Remains

Under load, the difference between a timely partial answer and a timed-out empty one
is a single decision: is there enough budget left to afford the expensive step? This
exercise builds a response assembler that runs a fast core query, then measures the
remaining budget with `ctx.Deadline()`/`time.Until` and skips an optional slow
enrichment when the budget is below the enrichment's known cost — returning a
degraded-but-timely response instead of blowing the deadline.

This module is fully self-contained. It has its own `go mod init`, defines every
type it needs, and ships its own demo and tests. Nothing here imports any other
exercise.

## What you'll build

```text
budget-guard/                        independent module: example.com/budgetguard
  go.mod                             go 1.26
  builder.go                         Builder, Response, BuildResponse; the budget guard
  cmd/
    demo/
      main.go                        runnable demo: generous enriches, tight degrades
  builder_test.go                    generous/tight/no-deadline, enrich-spy, -race
```

- Files: `builder.go`, `cmd/demo/main.go`, `builder_test.go`.
- Implement: `Builder{Core, Enrich, EnrichmentCost}` and `BuildResponse(ctx) (Response, error)` that always runs `Core`, then runs `Enrich` only if `time.Until(deadline) > EnrichmentCost` (or there is no deadline), setting `Enriched`/`Degraded`.
- Test: a generous ctx runs enrichment (`Enriched=true`); a tight ctx skips it (`Enriched=false`, `Degraded=true`) and never calls the enrich func (spy); a no-deadline ctx always enriches; the skip path returns within budget.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/14-select-and-context/05-context-withtimeout-withdeadline/05-budget-guard-skip-work/cmd/demo
cd go-solutions/14-select-and-context/05-context-withtimeout-withdeadline/05-budget-guard-skip-work
```

### Measuring the budget before spending it

Most timeout code is *reactive*: start the work, and if the deadline fires
mid-flight, abandon it. That is correct but wasteful — you paid part of the cost of
an operation you were never going to finish, and you returned nothing. Graceful
degradation is *proactive*: before starting an expensive step, ask whether it can
plausibly finish, and if not, skip it and return what you already have.

The measurement is `ctx.Deadline()` followed by `time.Until(deadline)`. If the
remaining budget exceeds the enrichment's known cost — a number you have from
historical p99 latency, a config value, or an SLO allocation — run the enrichment.
If not, skip it, mark the response degraded, and return the core result that is
already assembled and timely. The enrichment cost is an estimate, so the guard is a
heuristic, not a guarantee; but a heuristic that skips a step it cannot afford beats
one that starts the step and times out with nothing to show.

The `ok` bool from `Deadline()` is the other half of the rule. A context with *no*
deadline (`ok == false`) has unlimited budget as far as this request is concerned, so
the enrichment always runs — there is nothing to guard against. Treating a missing
deadline as "zero budget" would wrongly degrade every request that happens to run
without a timeout, which is a common and damaging bug.

The core query always runs; degrading it would defeat the purpose. Only the *optional*
enrichment is guarded. The `Response` carries an `Enriched` flag (did we run it) and a
`Degraded` flag (did we skip it because of budget), so the caller and the metrics can
see exactly what happened and a dashboard can track the degradation rate under load.

Create `builder.go`:

```go
package budgetguard

import (
	"context"
	"time"
)

// Response is the assembled result. Enriched reports whether the optional
// enrichment ran; Degraded reports whether it was skipped to stay within budget.
type Response struct {
	Data     string
	Enriched bool
	Degraded bool
}

// Builder assembles a response from a fast core step and an optional slow
// enrichment step, guarded by the remaining request budget.
type Builder struct {
	Core           func(ctx context.Context) (string, error)
	Enrich         func(ctx context.Context) (string, error)
	EnrichmentCost time.Duration
}

// affordable reports whether the remaining budget can cover the enrichment. A
// context with no deadline is treated as unbounded, so enrichment always runs.
func (b *Builder) affordable(ctx context.Context) bool {
	deadline, ok := ctx.Deadline()
	if !ok {
		return true
	}
	return time.Until(deadline) > b.EnrichmentCost
}

// BuildResponse runs the core step, then runs the enrichment only if the budget
// can afford it. When it cannot, it returns a timely, degraded partial response.
func (b *Builder) BuildResponse(ctx context.Context) (Response, error) {
	data, err := b.Core(ctx)
	if err != nil {
		return Response{}, err
	}
	resp := Response{Data: data}

	if !b.affordable(ctx) {
		resp.Degraded = true
		return resp, nil
	}

	extra, err := b.Enrich(ctx)
	if err != nil {
		return Response{}, err
	}
	resp.Data += extra
	resp.Enriched = true
	return resp, nil
}
```

### The runnable demo

The demo builds a `Builder` whose enrichment costs 50ms and calls it twice: once
under a generous 1s budget (enriches) and once under a 20ms budget (degrades).

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/budgetguard"
)

func main() {
	b := &budgetguard.Builder{
		Core:           func(ctx context.Context) (string, error) { return "core", nil },
		Enrich:         func(ctx context.Context) (string, error) { return "+enriched", nil },
		EnrichmentCost: 50 * time.Millisecond,
	}

	ctx1, cancel1 := context.WithTimeout(context.Background(), time.Second)
	defer cancel1()
	r1, _ := b.BuildResponse(ctx1)
	fmt.Printf("generous: data=%q enriched=%v degraded=%v\n", r1.Data, r1.Enriched, r1.Degraded)

	ctx2, cancel2 := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel2()
	r2, _ := b.BuildResponse(ctx2)
	fmt.Printf("tight:    data=%q enriched=%v degraded=%v\n", r2.Data, r2.Enriched, r2.Degraded)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
generous: data="core+enriched" enriched=true degraded=false
tight:    data="core" enriched=false degraded=true
```

### Tests

The enrichment function is a spy that records whether it was called via an atomic
counter, so a test can assert the guard actually *prevented* the call rather than
merely ignoring its result. `TestEnrichesWhenBudgetGenerous` asserts enrichment ran
and the flags are `Enriched=true, Degraded=false`. `TestDegradesWhenBudgetTight`
asserts the spy was never called, the flags flipped, and the call returned within the
tiny budget. `TestNoDeadlineAlwaysEnriches` asserts a context with no deadline runs
enrichment. `TestCorePropagatesError` asserts a core failure short-circuits.

Create `builder_test.go`:

```go
package budgetguard

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func newSpyBuilder(cost time.Duration) (*Builder, *atomic.Int64) {
	var calls atomic.Int64
	b := &Builder{
		Core: func(ctx context.Context) (string, error) { return "core", nil },
		Enrich: func(ctx context.Context) (string, error) {
			calls.Add(1)
			return "+extra", nil
		},
		EnrichmentCost: cost,
	}
	return b, &calls
}

func TestEnrichesWhenBudgetGenerous(t *testing.T) {
	t.Parallel()
	b, calls := newSpyBuilder(50 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	r, err := b.BuildResponse(ctx)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !r.Enriched || r.Degraded {
		t.Fatalf("flags = {enriched:%v degraded:%v}, want {true false}", r.Enriched, r.Degraded)
	}
	if calls.Load() != 1 {
		t.Fatalf("enrich calls = %d, want 1", calls.Load())
	}
	if r.Data != "core+extra" {
		t.Fatalf("data = %q, want %q", r.Data, "core+extra")
	}
}

func TestDegradesWhenBudgetTight(t *testing.T) {
	t.Parallel()
	b, calls := newSpyBuilder(50 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()

	start := time.Now()
	r, err := b.BuildResponse(ctx)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if r.Enriched || !r.Degraded {
		t.Fatalf("flags = {enriched:%v degraded:%v}, want {false true}", r.Enriched, r.Degraded)
	}
	if calls.Load() != 0 {
		t.Fatalf("enrich calls = %d, want 0 (guard must prevent the call)", calls.Load())
	}
	if r.Data != "core" {
		t.Fatalf("data = %q, want %q", r.Data, "core")
	}
	if elapsed > 50*time.Millisecond {
		t.Fatalf("degraded path took %v, want well within budget", elapsed)
	}
}

func TestNoDeadlineAlwaysEnriches(t *testing.T) {
	t.Parallel()
	b, calls := newSpyBuilder(time.Hour)

	r, err := b.BuildResponse(context.Background())
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !r.Enriched {
		t.Fatal("no-deadline context should always enrich")
	}
	if calls.Load() != 1 {
		t.Fatalf("enrich calls = %d, want 1", calls.Load())
	}
}

func TestCorePropagatesError(t *testing.T) {
	t.Parallel()
	errCore := errors.New("core failed")
	b := &Builder{
		Core:           func(ctx context.Context) (string, error) { return "", errCore },
		Enrich:         func(ctx context.Context) (string, error) { return "+extra", nil },
		EnrichmentCost: time.Millisecond,
	}
	_, err := b.BuildResponse(context.Background())
	if !errors.Is(err, errCore) {
		t.Fatalf("err = %v, want errCore", err)
	}
}
```

## Review

The guard is correct when the enrichment runs exactly when the budget can afford it
and never otherwise. The measurement is `time.Until(deadline) > EnrichmentCost`, and
the spy in `TestDegradesWhenBudgetTight` proves the tight path *prevents* the call —
not merely discards its output — which is the whole point of proactive degradation.
The two flags make the outcome observable: `Enriched` and `Degraded` are mutually
exclusive here, and a dashboard tracking the degraded rate is watching the system
shed load gracefully. The no-deadline case in `TestNoDeadlineAlwaysEnriches` guards
the common bug of treating an absent deadline as zero budget.

The mistakes to avoid: reading `Deadline()` without checking its `ok` bool (degrading
every deadline-free request); guarding the core step instead of only the optional
enrichment (degrading the answer itself); and running the enrichment "and cancelling
it if the deadline fires" instead of skipping it up front (you pay part of its cost
and still return nothing). The enrichment cost is a heuristic from real latency data,
so keep it honest as the dependency's p99 shifts. Run `go test -race`; the spy's
atomic counter is checked under the detector.

## Resources

- [context.Context.Deadline](https://pkg.go.dev/context#Context) — reading the effective deadline and its ok bool.
- [time.Until](https://pkg.go.dev/time#Until) — the remaining-budget computation the guard compares against the cost.
- [Google SRE Book: Handling Overload](https://sre.google/sre-book/handling-overload/) — graceful degradation and load shedding as SLO-preserving strategies.

---

Back to [04-total-budget-retry.md](04-total-budget-retry.md) | Next: [06-labeled-timeout-cause.md](06-labeled-timeout-cause.md)
