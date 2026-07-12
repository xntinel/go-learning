# Exercise 20: Cascade Queue Priorities With Genuine Fallthrough

**Nivel: Intermedio** — validacion rapida (un test corto).

A work queue with Critical, High, Normal, and Batch lanes needs a timeout
and retry budget for each lane, and the natural rule is additive: Critical
work gets its own generous allowance plus everything every lane below it
gets, all the way down to Batch's bare minimum. This module resolves that
budget with an expression switch and a genuine `fallthrough` — the same
accumulating-hierarchy pattern the permission-cascade exercise introduced,
applied to numeric budgets instead of a permission slice, with a dedicated
test guarding the one regression that matters most: Batch silently
inheriting Critical's timeout. It is self-contained: its own `go mod init`,
code, demo, and test.

## What you'll build

```text
queuecascade/                independent module: example.com/queue-priority-level-inheritance
  go.mod                      go 1.24
  queuecascade.go               package queuecascade; ErrUnknownLevel; Budget; BudgetFor(level) (Budget, error)
  cmd/demo/main.go              runnable demo over all four levels plus normalization and an unknown level
  queuecascade_test.go           table over each level's cumulative budget plus a dedicated Batch-vs-Critical guard
```

- Implement: `BudgetFor(level string) (Budget, error)` — normalize the level, then an expression switch ordered Critical down to Batch that uses `fallthrough` to accumulate `Timeout` and `Retries` on the way down.
- Test: a table over each level's exact cumulative budget, normalization, an unknown level, and a standalone test asserting Batch's timeout is strictly less than Critical's.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Accumulating numeric fields instead of appending to a slice

This is structurally the same cascade the permission-level exercise used —
execution enters directly at the matched case, and `fallthrough` carries it
down through every case below — but here each case adds to two numeric
fields (`b.Timeout += ...`, `b.Retries += ...`) instead of appending strings
to a slice. The mechanics are identical: a request for `"high"` enters at
`case "high":`, adds High's own increment, then falls through into
`"normal"`'s body and `"batch"`'s body, picking up their increments too,
while never touching `"critical"`'s increment above it. A request for
`"batch"` enters at the last case and falls through nothing, because there
is nothing below it — that's the whole reason Batch's total stays the
smallest of the four, and it is exactly the property the dedicated
`TestBatchNeverGetsCriticalTimeout` test locks in: if a future edit
reordered the cases or moved `fallthrough` to the wrong case, that test
fails immediately instead of the regression surfacing as a production
incident where low-priority batch jobs quietly get critical-grade
timeouts.

Create `queuecascade.go`:

```go
// Package queuecascade computes the timeout and retry budget for a work
// queue's priority level using an expression switch with a genuine
// fallthrough: every level above Batch adds its own increment on top of
// everything the levels below it already contribute.
package queuecascade

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrUnknownLevel marks a level that isn't part of the closed priority
// hierarchy (Critical, High, Normal, Batch).
var ErrUnknownLevel = errors.New("queuecascade: unknown priority level")

// Budget is the resolved timeout and retry allowance for a priority level.
type Budget struct {
	Timeout time.Duration
	Retries int
}

// BudgetFor resolves the cumulative budget for level. Critical accumulates
// its own increment plus High's, Normal's, and Batch's; Batch, at the
// bottom of the switch, only ever gets its own base increment, because
// execution enters directly at the matched case and Batch has nothing
// below it to fall into.
func BudgetFor(level string) (Budget, error) {
	level = strings.ToLower(strings.TrimSpace(level))

	var b Budget
	switch level {
	case "critical":
		b.Timeout += 30 * time.Second
		b.Retries += 5
		fallthrough
	case "high":
		b.Timeout += 15 * time.Second
		b.Retries += 3
		fallthrough
	case "normal":
		b.Timeout += 5 * time.Second
		b.Retries += 1
		fallthrough
	case "batch":
		b.Timeout += 2 * time.Second
		// Retries += 0: batch jobs get no extra retry budget of their own.
	default:
		return Budget{}, fmt.Errorf("%w: %q", ErrUnknownLevel, level)
	}
	return b, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	queuecascade "example.com/queue-priority-level-inheritance"
)

func main() {
	levels := []string{"critical", "high", "normal", "batch", " Critical ", "urgent"}

	for _, level := range levels {
		b, err := queuecascade.BudgetFor(level)
		if err != nil {
			fmt.Printf("%-10s error: %v\n", level, err)
			continue
		}
		fmt.Printf("%-10s timeout=%-6s retries=%d\n", level, b.Timeout, b.Retries)
	}
}
```

Run `go run ./cmd/demo`, expected output:

```
critical   timeout=52s    retries=9
high       timeout=22s    retries=4
normal     timeout=7s     retries=1
batch      timeout=2s     retries=0
 Critical  timeout=52s    retries=9
urgent     error: queuecascade: unknown priority level: "urgent"
```

### Tests

`TestBudgetFor` runs a table over each level's exact cumulative timeout and
retry count, one entry proving normalization (stray whitespace and mixed
case), and an unknown level checked with `errors.Is`.
`TestBatchNeverGetsCriticalTimeout` is a second, standalone test that exists
purely to guard the ordering invariant: Batch's resolved timeout must be
strictly less than Critical's, which fails immediately if the cascade is
ever wired backwards.

Create `queuecascade_test.go`:

```go
package queuecascade

import (
	"errors"
	"testing"
	"time"
)

func TestBudgetFor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		level   string
		want    Budget
		wantErr bool
	}{
		{"critical", Budget{Timeout: 52 * time.Second, Retries: 9}, false},
		{"high", Budget{Timeout: 22 * time.Second, Retries: 4}, false},
		{"normal", Budget{Timeout: 7 * time.Second, Retries: 1}, false},
		{"batch", Budget{Timeout: 2 * time.Second, Retries: 0}, false},
		{" Critical ", Budget{Timeout: 52 * time.Second, Retries: 9}, false},
		{"urgent", Budget{}, true},
	}

	for _, tc := range tests {
		got, err := BudgetFor(tc.level)
		if tc.wantErr {
			if !errors.Is(err, ErrUnknownLevel) {
				t.Errorf("BudgetFor(%q) error = %v, want errors.Is match for ErrUnknownLevel", tc.level, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("BudgetFor(%q) unexpected error: %v", tc.level, err)
			continue
		}
		if got != tc.want {
			t.Errorf("BudgetFor(%q) = %+v, want %+v", tc.level, got, tc.want)
		}
	}
}

// TestBatchNeverGetsCriticalTimeout guards against the exact regression a
// misordered or misplaced fallthrough would cause: a Batch job silently
// inheriting Critical's much larger timeout.
func TestBatchNeverGetsCriticalTimeout(t *testing.T) {
	t.Parallel()

	batch, err := BudgetFor("batch")
	if err != nil {
		t.Fatalf("BudgetFor(batch) unexpected error: %v", err)
	}
	critical, err := BudgetFor("critical")
	if err != nil {
		t.Fatalf("BudgetFor(critical) unexpected error: %v", err)
	}
	if batch.Timeout >= critical.Timeout {
		t.Fatalf("batch timeout %s >= critical timeout %s, want strictly less", batch.Timeout, critical.Timeout)
	}
}
```

Verify with:

```bash
go test -count=1 ./...
```

## Review

The cascade is correct when each level's budget is exactly the sum of its
own increment and every increment below it, never one above, and when Batch
— sitting at the bottom of the switch with no `fallthrough` of its own —
can never accumulate more than its own base increment. Carry this forward:
whenever a hierarchy's higher tiers are meant to receive everything the
lower tiers get plus their own addition, order the switch highest-to-lowest
and let `fallthrough` do the accumulating, then write a test that checks
the ordering invariant directly instead of trusting the arithmetic by
inspection.

## Resources

- [Go Specification: Switch statements](https://go.dev/ref/spec#Switch_statements) — the fallthrough statement and its restrictions.
- [time.Duration](https://pkg.go.dev/time#Duration) — arithmetic on accumulated timeout budgets.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [19-binary-frame-type-demultiplexer.md](19-binary-frame-type-demultiplexer.md) | Next: [21-api-auth-scheme-classifier.md](21-api-auth-scheme-classifier.md)
