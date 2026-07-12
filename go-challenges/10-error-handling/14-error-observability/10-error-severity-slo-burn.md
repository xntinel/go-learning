# Exercise 10: Mapping errors to severity and firing on an SLO error budget

Paging a human on every error trains them to ignore the pager. This final module
builds the discipline that fixes that: classify each error into a severity (client
`4xx` = warn, dependency/internal `5xx` = error and pageable), feed the pageable
ones into a sliding-window rate tracker, and raise an alert only when the burn over
the window crosses the error budget — SLO-based alerting, not alert-on-every-error.

This module is fully self-contained: its own `go mod init`, demo, and tests.
Nothing here imports any other exercise.

## What you'll build

```text
sloburn/                     independent module: example.com/sloburn
  go.mod                     go 1.25
  sloburn.go                 Classify (errors.As/Is -> Category+Level); Tracker sliding window + budget
  cmd/
    demo/
      main.go                runnable demo: client noise never pages; a 5xx burst does
  sloburn_test.go            client warns never page; dependency burst pages; window rolls forward
```

- Files: `sloburn.go`, `cmd/demo/main.go`, `sloburn_test.go`.
- Implement: `Classify(err)` mapping categories to `slog.Level` via `errors.As`/`errors.Is`; a `Tracker` with a sliding window and a budget whose `Record(err)` returns the level and whether to page, counting only pageable errors and evicting timestamps older than the window.
- Test: feed client errors within a window and assert none page (all warn); feed a dependency burst crossing the budget and assert it pages; advance the clock so an old burst ages out and assert it stops paging.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.25
```

### Severity classification, then windowed burn

Two ideas combine here. The first is that not all errors deserve the same
response. A client `4xx` — a malformed request, a 404 — is the client's problem;
it is worth a `warn`-level log for debugging but never a page. A dependency or
internal `5xx` is your problem and, if sustained, pageable. `Classify` turns an
error into a `(Category, slog.Level)` using the two matching tools together:
`errors.As` to pull a structured `*HTTPError` out of the chain and branch on its
status class, and `errors.Is` to match a `ErrDependency` sentinel. This is the
error-handling machinery of the whole chapter put to an operational purpose —
severity is a function of the error's category, which is why classification lives
with error handling, not off in a separate alerting config.

The second idea is that even a pageable category should not page on a single
occurrence — one transient `5xx` is noise; a *sustained rate* of them is an
incident. The `Tracker` keeps a sliding window of pageable-error timestamps and a
budget (the number of pageable errors tolerated within the window). `Record`
appends the current time for a pageable error, evicts any timestamps older than
`window` from the front, and reports `page = len(window) > budget`. Client errors
are classified and returned but never entered into the window, so a flood of
`4xx` — however large — never trips the pager. This is a simplified error-budget
burn-rate alert: real SLO alerting compares burn against a budget derived from a
target, but the mechanism — count failures over a rolling window, fire when the
count crosses a threshold, let old failures age out — is exactly this.

The window "rolling forward" is the property that makes it an alert and not a
fuse: because eviction drops timestamps older than `window`, a burst that happened
and stopped ages out, and the tracker returns to not-paging once the burst is
older than the window. An alert that latched on forever after one bad minute would
be as useless as no alert. The clock is injected as `now func() time.Time` so the
test can drive time deterministically.

Create `sloburn.go`:

```go
package sloburn

import (
	"errors"
	"log/slog"
	"sync"
	"time"
)

// Category is the severity class an error maps to.
type Category int

const (
	CategoryClient     Category = iota // 4xx: the caller's fault; warn, never page
	CategoryDependency                 // downstream failure; error, pageable
	CategoryInternal                   // our 5xx; error, pageable
)

// ErrDependency marks a downstream failure; wrap it with %w at the call site.
var ErrDependency = errors.New("dependency failure")

// HTTPError carries a status code so Classify can branch on its class.
type HTTPError struct {
	Status int
	Msg    string
}

func (e *HTTPError) Error() string { return e.Msg }

// Classify maps an error to its category and log level using errors.As for the
// structured HTTPError and errors.Is for the dependency sentinel.
func Classify(err error) (Category, slog.Level) {
	var he *HTTPError
	if errors.As(err, &he) {
		switch {
		case he.Status >= 500:
			return CategoryInternal, slog.LevelError
		case he.Status >= 400:
			return CategoryClient, slog.LevelWarn
		}
	}
	if errors.Is(err, ErrDependency) {
		return CategoryDependency, slog.LevelError
	}
	return CategoryInternal, slog.LevelError
}

// pageable reports whether a category should count toward the pager.
func pageable(c Category) bool { return c == CategoryDependency || c == CategoryInternal }

// Tracker is a sliding-window burn-rate alerter: it pages when the count of
// pageable errors within the window exceeds the budget.
type Tracker struct {
	now    func() time.Time
	window time.Duration
	budget int

	mu    sync.Mutex
	times []time.Time // pageable-error timestamps within the window
}

// NewTracker builds a Tracker. window is the sliding window; budget is how many
// pageable errors are tolerated within it before paging; now injects the clock.
func NewTracker(window time.Duration, budget int, now func() time.Time) *Tracker {
	return &Tracker{now: now, window: window, budget: budget}
}

// Record classifies err, records it if pageable, and returns its log level and
// whether the pager should fire now. Client errors are classified but never paged.
func (t *Tracker) Record(err error) (level slog.Level, page bool) {
	cat, level := Classify(err)
	if !pageable(cat) {
		return level, false
	}

	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()

	t.times = append(t.times, now)
	t.evictLocked(now)
	return level, len(t.times) > t.budget
}

// evictLocked drops timestamps older than the window from the front.
func (t *Tracker) evictLocked(now time.Time) {
	cutoff := now.Add(-t.window)
	i := 0
	for i < len(t.times) && t.times[i].Before(cutoff) {
		i++
	}
	t.times = t.times[i:]
}
```

### The runnable demo

The demo drives a realistic mix through a controllable clock: a flood of client
`4xx` that never pages, then a burst of dependency `5xx` that crosses the budget
and pages, then — after advancing the clock past the window — a single `5xx` that
does not page because the old burst has aged out.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync/atomic"
	"time"

	"example.com/sloburn"
)

type clock struct{ ns atomic.Int64 }

func (c *clock) Now() time.Time          { return time.Unix(0, c.ns.Load()).UTC() }
func (c *clock) Advance(d time.Duration) { c.ns.Add(int64(d)) }

func main() {
	var clk clock
	tr := sloburn.NewTracker(time.Minute, 3, clk.Now)

	clientErr := &sloburn.HTTPError{Status: 400, Msg: "bad request"}
	depErr := fmt.Errorf("query: %w", sloburn.ErrDependency)

	paged := 0
	for range 100 {
		if _, page := tr.Record(clientErr); page {
			paged++
		}
	}
	fmt.Printf("after 100 client 4xx: paged=%d\n", paged)

	firstPageAt := 0
	for i := 1; i <= 5; i++ {
		if _, page := tr.Record(depErr); page && firstPageAt == 0 {
			firstPageAt = i
		}
	}
	fmt.Printf("dependency 5xx first paged at occurrence %d\n", firstPageAt)

	clk.Advance(2 * time.Minute) // old burst ages out
	_, page := tr.Record(depErr)
	fmt.Printf("after window rolls forward, single 5xx pages=%v\n", page)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
after 100 client 4xx: paged=0
dependency 5xx first paged at occurrence 4
after window rolls forward, single 5xx pages=false
```

### Tests

`TestClientErrorsNeverPage` floods the tracker with `4xx` and asserts none page and
all warn. `TestDependencyBurstPages` proves the pager fires once the burst exceeds
the budget (occurrence 4 with budget 3). `TestWindowRollsForward` fires a paging
burst, advances the clock past the window, and asserts a fresh `5xx` no longer
pages — the old burst aged out.

Create `sloburn_test.go`:

```go
package sloburn

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"log/slog"
)

type fakeClock struct{ ns atomic.Int64 }

func (c *fakeClock) Now() time.Time          { return time.Unix(0, c.ns.Load()).UTC() }
func (c *fakeClock) Advance(d time.Duration) { c.ns.Add(int64(d)) }

func TestClassify(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		err   error
		cat   Category
		level slog.Level
	}{
		{"client 4xx", &HTTPError{Status: 404, Msg: "nf"}, CategoryClient, slog.LevelWarn},
		{"internal 5xx", &HTTPError{Status: 503, Msg: "down"}, CategoryInternal, slog.LevelError},
		{"dependency", fmt.Errorf("q: %w", ErrDependency), CategoryDependency, slog.LevelError},
		{"unknown", fmt.Errorf("weird"), CategoryInternal, slog.LevelError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cat, level := Classify(tc.err)
			if cat != tc.cat || level != tc.level {
				t.Fatalf("Classify = (%v,%v), want (%v,%v)", cat, level, tc.cat, tc.level)
			}
		})
	}
}

func TestClientErrorsNeverPage(t *testing.T) {
	t.Parallel()
	var clk fakeClock
	tr := NewTracker(time.Minute, 3, clk.Now)
	client := &HTTPError{Status: 400, Msg: "bad"}

	for range 100 {
		level, page := tr.Record(client)
		if page {
			t.Fatal("client 4xx paged; it must never page")
		}
		if level != slog.LevelWarn {
			t.Fatalf("client level = %v, want Warn", level)
		}
	}
}

func TestDependencyBurstPages(t *testing.T) {
	t.Parallel()
	var clk fakeClock
	tr := NewTracker(time.Minute, 3, clk.Now)
	dep := fmt.Errorf("q: %w", ErrDependency)

	// budget 3: occurrences 1..3 do not page, occurrence 4 does.
	for i := 1; i <= 3; i++ {
		if _, page := tr.Record(dep); page {
			t.Fatalf("paged at occurrence %d, want first page at 4", i)
		}
	}
	if _, page := tr.Record(dep); !page {
		t.Fatal("did not page at occurrence 4 despite crossing budget")
	}
}

func TestWindowRollsForward(t *testing.T) {
	t.Parallel()
	var clk fakeClock
	tr := NewTracker(time.Minute, 3, clk.Now)
	dep := fmt.Errorf("q: %w", ErrDependency)

	for range 5 { // cross the budget: now paging
		tr.Record(dep)
	}
	if _, page := tr.Record(dep); !page {
		t.Fatal("expected to be paging before the window rolls")
	}

	clk.Advance(2 * time.Minute) // all prior timestamps age out
	if _, page := tr.Record(dep); page {
		t.Fatal("a single 5xx paged after the old burst aged out of the window")
	}
}
```

## Review

The tracker is correct when severity and windowing both hold. Severity:
`TestClassify` and `TestClientErrorsNeverPage` prove `4xx` warns and never pages
while `5xx` and dependency errors are error-level and pageable. Windowing:
`TestDependencyBurstPages` proves the pager fires only after the burst exceeds the
budget (not on the first error), and `TestWindowRollsForward` proves an old burst
ages out so the alert clears — the two properties that separate SLO-burn alerting
from alert-on-every-error and from a latching fuse.

The mistake this closes the chapter on is paging on every error, including expected
`4xx`: it floods the on-call and destroys the pager's signal value. Classify by
category, alert on sustained pageable burn over a window. Note the classification
reuses exactly the `errors.As`/`errors.Is` machinery from earlier in this chapter —
observability severity is not a separate system, it is error handling read at the
operational altitude. A production tracker would compute a burn *rate* against a
budget derived from an SLO target and use multiple windows (fast and slow) to
balance detection speed against false pages; the rolling-count mechanism here is
that idea in its simplest honest form.

## Resources

- [Google SRE Workbook: Alerting on SLOs](https://sre.google/workbook/alerting-on-slos/) — error-budget burn-rate alerting and multi-window alerts.
- [`errors`](https://pkg.go.dev/errors) — `errors.As` and `errors.Is`, the classification primitives.
- [`log/slog` Levels](https://pkg.go.dev/log/slog#Level) — `LevelWarn` vs `LevelError` and severity semantics.

---

Back to [00-concepts.md](00-concepts.md) | Next: [../../11-packages-and-modules/01-package-declaration-and-imports/01-package-declaration-and-imports.md](../../11-packages-and-modules/01-package-declaration-and-imports/01-package-declaration-and-imports.md)
