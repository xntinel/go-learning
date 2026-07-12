# Exercise 18: Parallel Health Check Results with Deterministic Ranking

**Nivel: Intermedio** — validacion rapida (un test corto).

A service-mesh health dashboard fans health checks out to many services
concurrently and fans the results back in on a single channel; several
checks for the same service can even land close together if a probe is
retried. This module ranges that results channel into a map keyed by service
name — keeping only the freshest result per service — then ranks the
collected results by `(status, freshness)` so the worst-off services always
appear first, regardless of which order the channel happened to deliver
results in. The module is fully self-contained: its own `go mod init`, no
external dependencies.

## What you'll build

```text
healthagg/                  independent module: example.com/health-check-aggregator
  go.mod                    go 1.24
  healthagg.go              type Result, Status; Collect(<-chan Result) []Result
  cmd/
    demo/
      main.go               runnable demo: four results, one service checked twice
  healthagg_test.go         table test: freshest-wins + ranking + empty channel
```

- Files: `healthagg.go`, `cmd/demo/main.go`, `healthagg_test.go`.
- Implement: `Collect(results <-chan Result) []Result` accumulating into a
  `map[string]Result` by service name, then flattening and sorting by
  `(Status desc, Timestamp desc, Service asc)`.
- Test: one table with two results for the same service (proving freshest
  wins) plus distinct services requiring a status-based rank, and an
  empty-channel case.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Two ranges, two different jobs: dedupe by recency, then rank by severity

`Collect`'s `for r := range results` loop does one thing per iteration: if
this service has not been seen yet, or this result is newer than the one
already stored, replace it. That single `After` comparison is what makes a
duplicate check for the same service resolve to its latest state instead of
either the first-seen state (stale) or an arbitrary one (nondeterministic).
It is the same "map as scratch, decide what wins" idiom as an LRU or a
last-write-wins cache, just keyed by arrival freshness instead of an
explicit version number.

The second range — flattening `byService` into a slice — is where "which
order came off the channel" would otherwise leak into the result, since map
iteration order is randomized per run. `sort.Slice` imposes the real
contract: worse status must sort before better status (`Status` is defined
so a larger value is worse, so `>` puts the worst first), and for two
services in the same status, the one with the more recent timestamp is the
more actionable signal, so it sorts first. `Service` breaks any remaining tie
so the output is fully deterministic, not just "deterministic in practice
for this test data."

Create `healthagg.go`:

```go
package healthagg

import (
	"sort"
	"time"
)

// Status is a service's health as reported by one check, ordered from least
// to most severe so a higher value always means "worse."
type Status int

const (
	StatusHealthy Status = iota
	StatusDegraded
	StatusDown
)

// Result is one health check outcome for one service.
type Result struct {
	Service   string
	Status    Status
	Timestamp time.Time
}

// Collect drains results until the channel is closed, keeping only the
// freshest result per service (a service can be checked more than once
// concurrently, and only its latest state matters), then returns a
// deterministic ranking: worst status first, and within equal status the
// freshest result first.
func Collect(results <-chan Result) []Result {
	byService := make(map[string]Result)

	for r := range results {
		cur, ok := byService[r.Service]
		if !ok || r.Timestamp.After(cur.Timestamp) {
			byService[r.Service] = r
		}
	}

	out := make([]Result, 0, len(byService))
	for _, r := range byService {
		out = append(out, r)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Status != out[j].Status {
			return out[i].Status > out[j].Status // worst first
		}
		if !out[i].Timestamp.Equal(out[j].Timestamp) {
			return out[i].Timestamp.After(out[j].Timestamp) // freshest first
		}
		return out[i].Service < out[j].Service // final deterministic tiebreak
	})

	return out
}
```

### The runnable demo

The demo sends four results on a buffered channel — `billing` is checked
twice, first `Down` then `Degraded` a moment later — and prints the ranked
result.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/health-check-aggregator"
)

func main() {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	ch := make(chan healthagg.Result, 4)
	ch <- healthagg.Result{Service: "auth", Status: healthagg.StatusHealthy, Timestamp: base}
	ch <- healthagg.Result{Service: "billing", Status: healthagg.StatusDown, Timestamp: base.Add(1 * time.Second)}
	ch <- healthagg.Result{Service: "search", Status: healthagg.StatusDegraded, Timestamp: base.Add(2 * time.Second)}
	ch <- healthagg.Result{Service: "billing", Status: healthagg.StatusDegraded, Timestamp: base.Add(3 * time.Second)}
	close(ch)

	for _, r := range healthagg.Collect(ch) {
		fmt.Printf("%s status=%d\n", r.Service, r.Status)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
billing status=1
search status=1
auth status=0
```

`billing`'s second check (`Degraded`, the fresher one) is what wins over its
first check (`Down`) — freshness beats first-seen order here, on purpose.

### Tests

The table exercises the freshest-wins dedup for a repeated service alongside
a status-based rank across distinct services, plus an empty-channel case
that must return an empty (non-nil) slice.

Create `healthagg_test.go`:

```go
package healthagg

import (
	"reflect"
	"testing"
	"time"
)

func TestCollect(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		results []Result
		want    []Result
	}{
		{
			name:    "empty channel",
			results: nil,
			want:    []Result{},
		},
		{
			name: "worst status first, freshest wins per service, then freshest tiebreak",
			results: []Result{
				{Service: "auth", Status: StatusHealthy, Timestamp: base},
				{Service: "billing", Status: StatusDown, Timestamp: base.Add(1 * time.Second)},
				{Service: "search", Status: StatusDegraded, Timestamp: base.Add(2 * time.Second)},
				{Service: "billing", Status: StatusDegraded, Timestamp: base.Add(3 * time.Second)},
			},
			want: []Result{
				{Service: "billing", Status: StatusDegraded, Timestamp: base.Add(3 * time.Second)},
				{Service: "search", Status: StatusDegraded, Timestamp: base.Add(2 * time.Second)},
				{Service: "auth", Status: StatusHealthy, Timestamp: base},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ch := make(chan Result, len(tc.results))
			for _, r := range tc.results {
				ch <- r
			}
			close(ch)

			got := Collect(ch)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Collect() = %+v, want %+v", got, tc.want)
			}
		})
	}
}
```

Run it:

```bash
go test -count=1 ./...
```

## Review

`Collect` is correct when every service appears exactly once, holding its
most recently observed status, and the output order depends only on
`(status, freshness, service name)` — never on the order results happened to
arrive on the channel. The bug this design avoids is the naive "last result
in the map wins," which silently depends on channel receive order (itself
dependent on goroutine scheduling); comparing `Timestamp` explicitly instead
of trusting arrival order is what makes the result reproducible across runs.

## Resources

- [Go Specification: For statements (range over channel)](https://go.dev/ref/spec#For_range)
- [package sort (Slice)](https://pkg.go.dev/sort#Slice)
- [Google SRE Workbook: Monitoring Distributed Systems](https://sre.google/workbook/monitoring/) — why rank-by-severity dashboards matter operationally.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [17-log-error-aggregator-by-type.md](17-log-error-aggregator-by-type.md) | Next: [19-ordered-event-resequencer.md](19-ordered-event-resequencer.md)
