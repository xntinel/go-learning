# Exercise 10: Ranking with slices.SortFunc and an Inline Comparator

Ranking a list by several keys at once — incidents by severity, then age, then id —
is where anonymous functions meet the sort package. `slices.SortFunc` takes a
comparator literal that must return a three-way integer, and `cmp.Or` + `cmp.Compare`
compose the multi-key logic cleanly. This module builds an alert ranker, pins down
the comparator contract, and contrasts the unstable `SortFunc` with the stable
`SortStableFunc`.

This module is fully self-contained. Nothing here imports another exercise.

## What you'll build

```text
ranking/                      module example.com/ranking
  go.mod
  ranking.go                  Alert, Severity; byRank comparator; Rank, RankStable
  ranking_test.go             multi-key order, comparator contract, stability contrast
  cmd/demo/main.go            rank a handful of alerts
```

- Files: `ranking.go`, `ranking_test.go`, `cmd/demo/main.go`.
- Implement: a `byRank` comparator (an anonymous function value) using `cmp.Or`, `cmp.Compare`, and `time.Time.Compare` — severity descending, then oldest-first, then id; `Rank` (SortFunc) and `RankStable` (SortStableFunc).
- Test: a known input yields the exact multi-key order; the comparator returns 0 for equal keys and is antisymmetric; `RankStable` preserves input order among equal-key items where `Rank`'s id tiebreak reorders them.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/ranking/cmd/demo
cd ~/go-exercises/ranking
go mod init example.com/ranking
```

### The three-way comparator contract

`slices.SortFunc(x, cmp)` sorts `x` with a comparator that returns a negative int
if `a` should sort before `b`, `0` if they are equal on the sort keys, and a
positive int if `a` should sort after `b`. The comparator must be a strict weak
ordering: it must return `0` for elements equal on the keys, and it must be
antisymmetric — `cmp(a,b)` and `cmp(b,a)` have opposite signs. A comparator that
returns only `-1`/`1` and never `0`, or that is not antisymmetric, makes equal
elements sort nondeterministically and breaks any stability assumption.

`cmp.Compare[T]` produces exactly this three-way int for ordered types, and
`cmp.Or(vals...)` returns the first non-zero of its arguments — which is precisely
how you compose a multi-key comparator: try severity first, fall through to
timestamp on a tie, then to id. `byRank` is a *function value* (an anonymous
literal assigned to a package variable), so it is both the comparator handed to
`SortFunc` and a thing a test can call directly to check the contract. Note the
severity comparison is `cmp.Compare(b.Sev, a.Sev)` — arguments reversed — to sort
*descending*: higher severity first. `time.Time.Compare` gives the three-way result
for timestamps without subtracting.

`Rank` uses `slices.SortFunc`, which is not stable; its comparator includes the id
as a final tiebreak so the result is fully deterministic anyway. `RankStable` uses
`slices.SortStableFunc` with a comparator that stops at (severity, timestamp), so
elements equal on those two keys retain their input order — the reason to pay for a
stable sort.

Create `ranking.go`:

```go
package ranking

import (
	"cmp"
	"slices"
	"time"
)

// Severity ranks an alert's urgency.
type Severity int

const (
	Info Severity = iota
	Warning
	Critical
)

func (s Severity) String() string {
	switch s {
	case Critical:
		return "CRITICAL"
	case Warning:
		return "WARNING"
	default:
		return "INFO"
	}
}

// Alert is an incident to rank.
type Alert struct {
	ID  string
	Sev Severity
	At  time.Time
}

// byRank is the three-way comparator: severity descending, then oldest first,
// then id ascending. It is an anonymous function value used by Rank and tests.
var byRank = func(a, b Alert) int {
	return cmp.Or(
		cmp.Compare(b.Sev, a.Sev), // higher severity first (arguments reversed)
		a.At.Compare(b.At),        // older first
		cmp.Compare(a.ID, b.ID),   // deterministic tiebreak
	)
}

// Rank orders alerts in place by (severity desc, age, id). Unstable, but the id
// tiebreak makes the result deterministic.
func Rank(alerts []Alert) {
	slices.SortFunc(alerts, byRank)
}

// RankStable orders alerts by (severity desc, age) with a stable sort, so alerts
// equal on those keys keep their input order.
func RankStable(alerts []Alert) {
	slices.SortStableFunc(alerts, func(a, b Alert) int {
		return cmp.Or(
			cmp.Compare(b.Sev, a.Sev),
			a.At.Compare(b.At),
		)
	})
}
```

### The runnable demo

The demo ranks three alerts and prints them in ranked order.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/ranking"
)

func main() {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	alerts := []ranking.Alert{
		{ID: "disk-full", Sev: ranking.Critical, At: base.Add(2 * time.Minute)},
		{ID: "cpu-high", Sev: ranking.Warning, At: base},
		{ID: "oom", Sev: ranking.Critical, At: base},
	}

	ranking.Rank(alerts)
	for _, a := range alerts {
		fmt.Printf("%s %s\n", a.Sev, a.ID)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
CRITICAL oom
CRITICAL disk-full
WARNING cpu-high
```

### Tests

`TestRankOrder` feeds a known mix and asserts the exact multi-key order.
`TestComparatorContract` calls `byRank` directly to confirm it returns 0 for equal
keys and is antisymmetric. `TestStabilityContrast` builds three alerts equal on
severity and timestamp and shows `RankStable` preserves their input order while
`Rank`'s id tiebreak sorts them.

Create `ranking_test.go`:

```go
package ranking

import (
	"fmt"
	"slices"
	"testing"
	"time"
)

func idList(alerts []Alert) []string {
	out := make([]string, len(alerts))
	for i, a := range alerts {
		out[i] = a.ID
	}
	return out
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	default:
		return 0
	}
}

func TestRankOrder(t *testing.T) {
	t.Parallel()
	t1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Hour)
	in := []Alert{
		{ID: "low", Sev: Info, At: t2},
		{ID: "crit-new", Sev: Critical, At: t2},
		{ID: "crit-old", Sev: Critical, At: t1},
		{ID: "warn", Sev: Warning, At: t1},
	}
	Rank(in)
	want := []string{"crit-old", "crit-new", "warn", "low"}
	if got := idList(in); !slices.Equal(got, want) {
		t.Fatalf("Rank order = %v, want %v", got, want)
	}
}

func TestComparatorContract(t *testing.T) {
	t.Parallel()
	now := time.Now()
	a := Alert{ID: "a", Sev: Critical, At: now}
	b := Alert{ID: "b", Sev: Warning, At: now.Add(time.Minute)}

	if sign(byRank(a, b)) != -sign(byRank(b, a)) {
		t.Fatalf("comparator not antisymmetric: ab=%d ba=%d", byRank(a, b), byRank(b, a))
	}
	same := Alert{ID: "a", Sev: Critical, At: now}
	if got := byRank(a, same); got != 0 {
		t.Fatalf("comparator on equal keys = %d, want 0", got)
	}
}

func TestStabilityContrast(t *testing.T) {
	t.Parallel()
	now := time.Now()
	in := []Alert{
		{ID: "c", Sev: Warning, At: now},
		{ID: "a", Sev: Warning, At: now},
		{ID: "b", Sev: Warning, At: now},
	}

	stable := slices.Clone(in)
	RankStable(stable)
	if got := idList(stable); !slices.Equal(got, []string{"c", "a", "b"}) {
		t.Fatalf("RankStable reordered equal-key items: %v, want c,a,b", got)
	}

	full := slices.Clone(in)
	Rank(full)
	if got := idList(full); !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Fatalf("Rank id-tiebreak order = %v, want a,b,c", got)
	}
}

func ExampleRank() {
	base := time.Unix(0, 0).UTC()
	alerts := []Alert{
		{ID: "b", Sev: Warning, At: base},
		{ID: "a", Sev: Critical, At: base},
	}
	Rank(alerts)
	fmt.Println(alerts[0].ID, alerts[1].ID)
	// Output: a b
}
```

## Review

The ranker is correct when a known input produces the exact ranked order and the
comparator obeys its contract. `TestComparatorContract` is the important one: a
comparator that never returns 0, or that is not antisymmetric, silently corrupts the
order, and the property test catches it. The severity comparison reverses its
arguments to sort descending; forgetting that is the most common ranking bug.
`cmp.Or` composing `cmp.Compare` and `time.Time.Compare` keeps the multi-key logic
readable and correct. Finally, `Rank` (unstable) is made deterministic by its id
tiebreak, while `RankStable` deliberately omits it and pays for a stable sort to
preserve input order among equal-key alerts — `TestStabilityContrast` shows exactly
when that distinction matters.

## Resources

- [slices.SortFunc](https://pkg.go.dev/slices#SortFunc)
- [slices.SortStableFunc](https://pkg.go.dev/slices#SortStableFunc)
- [cmp.Or](https://pkg.go.dev/cmp#Or)
- [time.Time.Compare](https://pkg.go.dev/time#Time.Compare)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [09-shared-capture-race-fanout.md](09-shared-capture-race-fanout.md) | Next: [11-validation-rule-pipeline.md](11-validation-rule-pipeline.md)
