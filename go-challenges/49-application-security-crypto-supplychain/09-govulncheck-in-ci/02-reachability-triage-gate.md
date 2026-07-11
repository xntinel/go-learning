# Exercise 2: Enforce a CI policy with reachable-vs-unreachable triage and suppressions

Parsing the scan gives you typed findings; a gate turns them into a build
decision. This exercise builds that gate as a pure function: reachable
(symbol-level) findings block, module-only advisories are reported but do not
block, and a suppression allowlist — keyed by OSV id, each entry carrying a
mandatory justification and an expiry — lets a team accept a finding for a bounded
window. Crucially, the gate fails closed: an expired suppression re-activates its
finding and blocks the build.

This module is fully self-contained: its own `go mod init`, all types inline, its
own demo and tests. It re-defines a minimal `Finding` so it does not import
Exercise 1.

## What you'll build

```text
vulngate/                    independent module: example.com/vulngate
  go.mod                     go 1.26
  vulngate.go                Finding/Suppression/Policy; LoadPolicy; Evaluate; Triage; Decision; exit codes
  cmd/
    demo/
      main.go                evaluates a finding set against a suppression file and exits with the gate code
  vulngate_test.go           table over finding sets x policy states; Example on the report renderer
```

- Files: `vulngate.go`, `cmd/demo/main.go`, `vulngate_test.go`.
- Implement: `Evaluate(findings, policy, now) Triage`; `Triage.Decision()`, `Triage.ExitCode()`, `Triage.Report()`; `LoadPolicy(io.Reader)` with `ErrInvalidSuppression` for a missing justification or expiry.
- Test: reachable+no-suppression blocks; reachable+valid-suppression passes as suppressed; reachable+expired-suppression blocks (fail closed); module-only passes as informational; empty passes. Assert the decision enum and the exit code.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/vulngate/cmd/demo
cd ~/go-exercises/vulngate
go mod init example.com/vulngate
go mod edit -go=1.26
```

### Why the gate is a pure function of an injected clock

The entire gate is deterministic: given the same findings, the same policy, and
the same `now`, it always produces the same triage and the same exit code. That is
why `Evaluate` takes `now time.Time` as an argument rather than calling
`time.Now()` internally. Suppression expiry is the only time-dependent rule, and
threading `now` in makes expiry testable without sleeping or mocking a clock — the
expired and non-expired cases are just two different `now` values. Production code
passes `time.Now()`; tests pass a fixed instant.

### The input contract: one finding per OSV, already reduced

`Evaluate` assumes each `Finding` it receives represents a distinct OSV, with its
`Reachable` flag already set to the maximum granularity govulncheck resolved for
that advisory. This is not cosmetic. Recall from Exercise 1 that govulncheck emits
several findings for a single OSV — one module-level, one package-level, one or
more symbol-level. If those raw findings were fed in directly, the *same* OSV would
appear as both a `Reachable: true` finding (its symbol trace) and a
`Reachable: false` finding (its module trace); the reachable one would land in
`Blocking` while the unreachable one landed in `Informational`, so a single
advisory would be reported as simultaneously blocking and merely-present. The
caller must therefore reduce the raw stream to one finding per OSV first — exactly
what `Report.Vulnerabilities()` from Exercise 1 produces — so that each OSV reaches
this gate once, carrying its highest-granularity reachability. The gate does not
re-deduplicate; keeping that responsibility in the parser is what lets `Evaluate`
stay a straight-line pure function.

### The three buckets and fail-closed suppression

`Evaluate` sorts every finding into one of three buckets:

- **Blocking** — a reachable (symbol-level) finding with no active suppression.
  These fail the build.
- **Suppressed** — a reachable finding that has a suppression whose expiry is in
  the future. Reported for visibility, but does not block, for its bounded window.
- **Informational** — a finding that is not reachable (module- or package-only).
  Reported, never blocks, because the vulnerable code is present but not called.

The fail-closed rule lives in one comparison. A suppression is active only while
`now` is not after its expiry (`!now.After(s.Expires)`). Once the expiry passes,
the lookup still finds the entry, but it is no longer active, so the finding falls
through to Blocking exactly as if no suppression existed. An allowlist entry can
therefore never silently become permanent: the day it expires, the build breaks
until someone renews it with a fresh justification or fixes the finding. That is
the property that keeps teams from disabling the gate — the escape hatch is real
but it closes on its own.

### Suppressions must be validated on load

A suppression with no justification or no expiry is not a suppression; it is a
blank cheque. `LoadPolicy` rejects both at parse time with the
`ErrInvalidSuppression` sentinel, so a malformed allowlist fails the pipeline
loudly instead of quietly accepting a vulnerability forever. The justification is
what an auditor reads six months later; the expiry is what forces the review.

Create `vulngate.go`:

```go
package vulngate

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
)

// ErrInvalidSuppression is returned when a suppression lacks a justification or
// an expiry. A suppression without both is an unbounded acceptance and is refused.
var ErrInvalidSuppression = errors.New("vulngate: invalid suppression")

// Finding is the minimal shape the gate needs: which advisory, whether the
// vulnerable symbol is actually called (reachable), and the fixing version. One
// Finding must stand for one OSV, already reduced to its maximum granularity (as
// Exercise 1's Report.Vulnerabilities produces); feeding the raw per-level
// findings in would let a single OSV land in two buckets at once.
type Finding struct {
	OSV          string
	Reachable    bool // true when resolved to symbol level (a called function)
	Module       string
	FixedVersion string
}

// Suppression is an allowlist entry: a bounded, justified acceptance of one OSV.
type Suppression struct {
	OSV           string
	Justification string
	Expires       time.Time
}

// Active reports whether the suppression is still in force at now. It fails
// closed: once now is past Expires, the suppression no longer applies.
func (s Suppression) Active(now time.Time) bool {
	return !now.After(s.Expires)
}

// Policy maps an OSV id to its suppression.
type Policy map[string]Suppression

// LoadPolicy reads a JSON array of suppressions and validates each one. A
// suppression missing a justification or an expiry is rejected with
// ErrInvalidSuppression rather than silently accepted.
func LoadPolicy(r io.Reader) (Policy, error) {
	var raw []struct {
		OSV           string    `json:"osv"`
		Justification string    `json:"justification"`
		Expires       time.Time `json:"expires"`
	}
	if err := json.NewDecoder(r).Decode(&raw); err != nil {
		return nil, fmt.Errorf("vulngate: decode policy: %w", err)
	}
	p := make(Policy, len(raw))
	for _, s := range raw {
		if strings.TrimSpace(s.Justification) == "" {
			return nil, fmt.Errorf("%w: %s missing justification", ErrInvalidSuppression, s.OSV)
		}
		if s.Expires.IsZero() {
			return nil, fmt.Errorf("%w: %s missing expiry", ErrInvalidSuppression, s.OSV)
		}
		p[s.OSV] = Suppression{OSV: s.OSV, Justification: s.Justification, Expires: s.Expires}
	}
	return p, nil
}

// Decision is the gate's verdict.
type Decision int

const (
	DecisionPass Decision = iota
	DecisionBlock
)

func (d Decision) String() string {
	if d == DecisionBlock {
		return "BLOCK"
	}
	return "PASS"
}

// Documented CI exit codes for this gate. They are the gate's own contract, not
// govulncheck's: this gate always computes its verdict from parsed findings, so
// it never inherits govulncheck's exit-0-under-json behavior.
const (
	ExitOK      = 0 // no blocking findings
	ExitBlocked = 1 // at least one reachable, unsuppressed finding
)

// SuppressedFinding pairs a suppressed finding with the entry that suppressed it,
// so the report can show why it was allowed and when the window closes.
type SuppressedFinding struct {
	Finding     Finding
	Suppression Suppression
}

// Triage is the sorted result of evaluating findings against a policy.
type Triage struct {
	Blocking      []Finding
	Suppressed    []SuppressedFinding
	Informational []Finding
}

// Evaluate sorts findings into blocking, suppressed, and informational buckets.
// A reachable finding blocks unless an active (non-expired) suppression covers it;
// a non-reachable finding is always informational. now is injected so expiry is
// deterministic in tests.
func Evaluate(findings []Finding, policy Policy, now time.Time) Triage {
	var t Triage
	for _, f := range findings {
		if !f.Reachable {
			t.Informational = append(t.Informational, f)
			continue
		}
		if s, ok := policy[f.OSV]; ok && s.Active(now) {
			t.Suppressed = append(t.Suppressed, SuppressedFinding{Finding: f, Suppression: s})
			continue
		}
		t.Blocking = append(t.Blocking, f)
	}
	slices.SortFunc(t.Blocking, func(a, b Finding) int { return cmp.Compare(a.OSV, b.OSV) })
	slices.SortFunc(t.Informational, func(a, b Finding) int { return cmp.Compare(a.OSV, b.OSV) })
	slices.SortFunc(t.Suppressed, func(a, b SuppressedFinding) int {
		return cmp.Compare(a.Finding.OSV, b.Finding.OSV)
	})
	return t
}

// Decision blocks the build when any reachable, unsuppressed finding remains.
func (t Triage) Decision() Decision {
	if len(t.Blocking) > 0 {
		return DecisionBlock
	}
	return DecisionPass
}

// ExitCode maps the decision to the documented CI exit code.
func (t Triage) ExitCode() int {
	if t.Decision() == DecisionBlock {
		return ExitBlocked
	}
	return ExitOK
}

// Report renders a triage summary suitable for a PR comment: blocking findings
// first, then suppressed (with their justification and window), then
// informational advisories. Ordering is stable for reproducible diffs.
func (t Triage) Report() string {
	var b strings.Builder
	for _, f := range t.Blocking {
		fmt.Fprintf(&b, "BLOCK %s reachable fix=%s\n", f.OSV, f.FixedVersion)
	}
	for _, s := range t.Suppressed {
		fmt.Fprintf(&b, "SUPPRESS %s until %s reason: %s\n",
			s.Finding.OSV, s.Suppression.Expires.Format("2006-01-02"), s.Suppression.Justification)
	}
	for _, f := range t.Informational {
		fmt.Fprintf(&b, "INFO %s module-only\n", f.OSV)
	}
	return b.String()
}
```

### The runnable demo

The demo loads a suppression file (embedded as a constant), evaluates a fixed set
of findings against it at a fixed `now`, prints the triage report, and exits with
the gate's code. One reachable finding is covered by an active suppression, one
reachable finding is not (so it blocks), and one module-only advisory is
informational — the three buckets in one run.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"example.com/vulngate"
)

const policyJSON = `[
  {"osv":"GO-2024-2687","justification":"upstream patch pending, tracked in JIRA-4412","expires":"2026-12-31T00:00:00Z"}
]`

func main() {
	policy, err := vulngate.LoadPolicy(strings.NewReader(policyJSON))
	if err != nil {
		log.Fatal(err)
	}

	findings := []vulngate.Finding{
		{OSV: "GO-2024-2687", Reachable: true, Module: "golang.org/x/net", FixedVersion: "v0.23.0"},
		{OSV: "GO-2025-0001", Reachable: true, Module: "example.com/lib", FixedVersion: "v1.4.0"},
		{OSV: "GO-2023-1988", Reachable: false, Module: "gopkg.in/yaml.v2"},
	}

	now := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	tr := vulngate.Evaluate(findings, policy, now)

	fmt.Print(tr.Report())
	fmt.Printf("decision: %s\n", tr.Decision())
	fmt.Printf("exit code: %d\n", tr.ExitCode())
	os.Exit(tr.ExitCode())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output (then a nonzero exit, because one reachable finding is unsuppressed):

```
BLOCK GO-2025-0001 reachable fix=v1.4.0
SUPPRESS GO-2024-2687 until 2026-12-31 reason: upstream patch pending, tracked in JIRA-4412
INFO GO-2023-1988 module-only
decision: BLOCK
exit code: 1
```

### Tests

The core test is a table over finding sets crossed with policy states: reachable
with no suppression blocks; reachable with a valid suppression passes and lands in
the suppressed bucket; the same suppression expired blocks again (fail closed);
module-only passes as informational; empty passes. Each case asserts both the
`Decision` enum and the intended exit code, and the bucket lengths so a
mis-routed finding is caught. `TestLoadPolicyValidation` asserts the
`ErrInvalidSuppression` sentinel via `errors.Is` for a missing justification. All
expiry logic uses a fixed `now`, so nothing is time-flaky.

Create `vulngate_test.go`:

```go
package vulngate

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestEvaluate(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	future := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	past := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	reachable := Finding{OSV: "GO-2024-2687", Reachable: true, FixedVersion: "v0.23.0"}
	moduleOnly := Finding{OSV: "GO-2023-1988", Reachable: false}

	tests := []struct {
		name         string
		findings     []Finding
		policy       Policy
		wantDecision Decision
		wantExit     int
		wantBlock    int
		wantSuppress int
		wantInfo     int
	}{
		{
			name:         "reachable no suppression blocks",
			findings:     []Finding{reachable},
			policy:       Policy{},
			wantDecision: DecisionBlock,
			wantExit:     ExitBlocked,
			wantBlock:    1,
		},
		{
			name:     "reachable with valid suppression passes",
			findings: []Finding{reachable},
			policy: Policy{reachable.OSV: {
				OSV: reachable.OSV, Justification: "patch pending", Expires: future,
			}},
			wantDecision: DecisionPass,
			wantExit:     ExitOK,
			wantSuppress: 1,
		},
		{
			name:     "expired suppression fails closed",
			findings: []Finding{reachable},
			policy: Policy{reachable.OSV: {
				OSV: reachable.OSV, Justification: "patch pending", Expires: past,
			}},
			wantDecision: DecisionBlock,
			wantExit:     ExitBlocked,
			wantBlock:    1,
		},
		{
			name:         "module-only is informational",
			findings:     []Finding{moduleOnly},
			policy:       Policy{},
			wantDecision: DecisionPass,
			wantExit:     ExitOK,
			wantInfo:     1,
		},
		{
			name:         "empty passes",
			findings:     nil,
			policy:       Policy{},
			wantDecision: DecisionPass,
			wantExit:     ExitOK,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tr := Evaluate(tc.findings, tc.policy, now)
			if got := tr.Decision(); got != tc.wantDecision {
				t.Errorf("Decision() = %s; want %s", got, tc.wantDecision)
			}
			if got := tr.ExitCode(); got != tc.wantExit {
				t.Errorf("ExitCode() = %d; want %d", got, tc.wantExit)
			}
			if got := len(tr.Blocking); got != tc.wantBlock {
				t.Errorf("Blocking = %d; want %d", got, tc.wantBlock)
			}
			if got := len(tr.Suppressed); got != tc.wantSuppress {
				t.Errorf("Suppressed = %d; want %d", got, tc.wantSuppress)
			}
			if got := len(tr.Informational); got != tc.wantInfo {
				t.Errorf("Informational = %d; want %d", got, tc.wantInfo)
			}
		})
	}
}

func TestLoadPolicyValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		wantErr error
	}{
		{
			name:    "valid",
			input:   `[{"osv":"GO-2024-2687","justification":"patch pending","expires":"2026-12-31T00:00:00Z"}]`,
			wantErr: nil,
		},
		{
			name:    "missing justification",
			input:   `[{"osv":"GO-2024-2687","expires":"2026-12-31T00:00:00Z"}]`,
			wantErr: ErrInvalidSuppression,
		},
		{
			name:    "missing expiry",
			input:   `[{"osv":"GO-2024-2687","justification":"patch pending"}]`,
			wantErr: ErrInvalidSuppression,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := LoadPolicy(strings.NewReader(tc.input))
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("LoadPolicy() = %v; want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("LoadPolicy() = %v; want errors.Is(_, %v)", err, tc.wantErr)
			}
		})
	}
}

func ExampleTriage_Report() {
	findings := []Finding{
		{OSV: "GO-2025-0001", Reachable: true, FixedVersion: "v1.4.0"},
		{OSV: "GO-2023-1988", Reachable: false},
	}
	now := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	tr := Evaluate(findings, Policy{}, now)
	fmt.Print(tr.Report())
	// Output:
	// BLOCK GO-2025-0001 reachable fix=v1.4.0
	// INFO GO-2023-1988 module-only
}
```

## Review

The gate is correct when the fail-closed rule holds: a reachable finding with an
expired suppression must block exactly as if the suppression were absent, which is
why `Active` compares against the injected `now` and the expired-case test asserts
`DecisionBlock`. Reachability, not severity, decides blocking: a module-only
advisory is informational because the vulnerable code is present but not called,
and blocking on it is the alert-fatigue anti-pattern that gets gates disabled.

The mistakes to avoid: do not call `time.Now()` inside `Evaluate` — inject `now`
so expiry is deterministic and the expired/active cases are two test inputs, not a
sleep. Do not accept a suppression without a justification and an expiry; a blank
allowlist entry is a permanent, unaudited acceptance, which is why `LoadPolicy`
rejects both with `ErrInvalidSuppression`. Confirm correctness by running the demo:
the unsuppressed reachable finding must appear under `BLOCK`, the covered one under
`SUPPRESS` with its window, the module-only one under `INFO`, and the process must
exit `1`.

## Resources

- [`time.Time.After`](https://pkg.go.dev/time#Time.After) — the comparison behind the fail-closed expiry check.
- [`slices.SortFunc`](https://pkg.go.dev/slices#SortFunc) and [`cmp.Compare`](https://pkg.go.dev/cmp#Compare) — stable, reproducible report ordering.
- [govulncheck-action](https://github.com/golang/govulncheck-action) — the official GitHub Action, for how a real gate wires output format and failure behavior.

---

Back to [01-parse-govulncheck-stream.md](01-parse-govulncheck-stream.md) | Next: [03-run-govulncheck-programmatically.md](03-run-govulncheck-programmatically.md)
