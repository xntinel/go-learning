# Exercise 20: Track failures per time interval and skip next interval on threshold breach

**Nivel: Intermedio** — validacion rapida (un test corto).

A circuit breaker in front of a flaky downstream dependency tracks failures
per fixed time window — per minute, say. Once an interval's failures cross a
threshold, retrying immediately in the very next interval is almost
guaranteed to fail the same way, so the breaker trips open: the next
interval is fast-failed without attempting any requests, and normal checking
resumes the interval after that. This module is fully self-contained: its
own `go mod init`, all code inline, its own demo and tests.

## What you'll build

```text
breaker/                    independent module: example.com/breaker
  go.mod                     go 1.24
  breaker.go                 Interval, Run
  cmd/
    demo/
      main.go                runnable demo: healthy, tripped, skipped, recovered intervals
  breaker_test.go             table test: no intervals, all healthy, one trip skips its neighbor, two trips in sequence, skip does not count failures
```

- Files: `breaker.go`, `cmd/demo/main.go`, `breaker_test.go`.
- Implement: `Run(intervals []Interval, threshold int) (results []string)`, tripping an interval once its failures exceed `threshold` and fast-failing the interval immediately after it.
- Test: no intervals, every interval healthy, a tripped interval skipping exactly its neighbor, a fresh trip occurring right after a skip ends, and a skipped interval's own failures never being counted.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/breaker/cmd/demo
cd ~/go-exercises/breaker
go mod init example.com/breaker
go mod edit -go=1.24
```

### Why the failure count needs a labeled continue

`Run` carries one piece of state across iterations of its outer loop:
`skipNext`, set the moment an interval trips. Deciding whether *this*
interval trips means counting its failures, which is itself a loop over that
interval's requests — nested one level below the intervals loop. The moment
the running failure count crosses `threshold`, there is no reason to keep
counting the rest of this interval's requests: the trip decision is already
made. A bare `continue` at that point would only skip to the next request of
the SAME interval, which is pointless once the outcome is already decided —
it would keep evaluating `if failures > threshold` on every remaining
request for no benefit. The labeled `continue intervals`, fired from inside
the per-request loop, jumps straight past the rest of this interval's
requests to the decision for the next one, which is exactly the boundary
where `skipNext` needs to be checked next.

Create `breaker.go`:

```go
package breaker

// Interval is one fixed time window's worth of request outcomes: true means
// the request succeeded, false means it failed.
type Interval struct {
	Name     string
	Requests []bool
}

// Run walks intervals in chronological order, counting failures per
// interval. The moment an interval's failures exceed threshold, the breaker
// trips: that interval is marked "tripped" and the interval immediately
// FOLLOWING it is fast-failed without even looking at its requests, since a
// breaker that just tripped should not immediately retry. The failure count
// is itself a running total built inside a nested loop over that interval's
// requests, and the moment it crosses threshold there is no reason to keep
// counting the rest of that interval's requests — a labeled continue on the
// intervals loop, fired from inside the per-request loop, moves straight to
// the decision for the NEXT interval.
func Run(intervals []Interval, threshold int) (results []string) {
	skipNext := false

intervals:
	for _, iv := range intervals {
		if skipNext {
			skipNext = false
			results = append(results, iv.Name+": skipped (breaker open)")
			continue intervals
		}

		failures := 0
		for _, ok := range iv.Requests {
			if !ok {
				failures++
			}
			if failures > threshold {
				results = append(results, iv.Name+": tripped")
				skipNext = true
				continue intervals
			}
		}
		results = append(results, iv.Name+": processed")
	}

	return results
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/breaker"
)

func main() {
	intervals := []breaker.Interval{
		{Name: "00:00", Requests: []bool{true, true, true}},
		{Name: "00:01", Requests: []bool{false, false, false, true}},
		{Name: "00:02", Requests: []bool{true, true}}, // skipped: breaker just tripped
		{Name: "00:03", Requests: []bool{true, true}},
	}

	for _, line := range breaker.Run(intervals, 2) {
		fmt.Println(line)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
00:00: processed
00:01: tripped
00:02: skipped (breaker open)
00:03: processed
```

`00:01` accumulates three failures against a threshold of two and trips
before its fourth request is even looked at. `00:02` is fast-failed purely
because the breaker just tripped — its two requests, both of which would
have succeeded, are never touched. `00:03` finds the breaker closed again
and processes normally.

### Tests

`TestRun` covers no intervals, every interval healthy, one tripped interval
skipping exactly its neighbor, a fresh trip happening again right after a
skip ends, and a skipped interval whose own failures never get the chance to
be counted.

Create `breaker_test.go`:

```go
package breaker

import (
	"slices"
	"testing"
)

func TestRun(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		intervals []Interval
		threshold int
		want      []string
	}{
		"no intervals": {
			intervals: nil,
			threshold: 1,
			want:      nil,
		},
		"all intervals healthy": {
			intervals: []Interval{
				{Name: "a", Requests: []bool{true, true}},
				{Name: "b", Requests: []bool{true}},
			},
			threshold: 1,
			want:      []string{"a: processed", "b: processed"},
		},
		"a tripped interval skips exactly the next one": {
			intervals: []Interval{
				{Name: "a", Requests: []bool{false, false, false}},
				{Name: "b", Requests: []bool{true}},
				{Name: "c", Requests: []bool{true}},
			},
			threshold: 2,
			want:      []string{"a: tripped", "b: skipped (breaker open)", "c: processed"},
		},
		"a fresh trip can happen again right after a skip ends": {
			intervals: []Interval{
				{Name: "a", Requests: []bool{false, false, false}},
				{Name: "b", Requests: []bool{true}},
				{Name: "c", Requests: []bool{false, false, false}},
				{Name: "d", Requests: []bool{true}},
				{Name: "e", Requests: []bool{true}},
			},
			threshold: 2,
			want: []string{
				"a: tripped",
				"b: skipped (breaker open)",
				"c: tripped",
				"d: skipped (breaker open)",
				"e: processed",
			},
		},
		"a skipped interval never has its own failures counted": {
			intervals: []Interval{
				{Name: "a", Requests: []bool{false, false, false}},
				{Name: "b", Requests: []bool{false, false, false, false, false}},
				{Name: "c", Requests: []bool{true}},
			},
			threshold: 2,
			want:      []string{"a: tripped", "b: skipped (breaker open)", "c: processed"},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := Run(tc.intervals, tc.threshold)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("Run = %v, want %v", got, tc.want)
			}
		})
	}
}
```

Verify:

```bash
go test -count=1 ./...
```

## Review

The breaker is correct when a tripped interval's neighbor is skipped
regardless of what that neighbor's own requests would have shown — the
"skipped interval never has its own failures counted" test proves this by
giving the skipped interval five failures that never get the chance to trip
it a second time. The bug this exercise guards against is a bare `continue`
inside the per-request failure count: it would keep re-evaluating `failures
> threshold` against every remaining request of the interval that already
tripped, which does not change the outcome but is wasted work, and reads as
if the loop had a reason to keep going when the decision was already final.

## Resources

- [Go Specification: Continue statements](https://go.dev/ref/spec#Continue_statements) — a labeled `continue` targets the named enclosing `for`.
- [Martin Fowler, CircuitBreaker](https://martinfowler.com/bliki/CircuitBreaker.html) — the pattern this breaker's trip-then-fast-fail behavior implements.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [19-event-stream-corruption-detector.md](19-event-stream-corruption-detector.md) | Next: [21-quorum-leader-election.md](21-quorum-leader-election.md)
