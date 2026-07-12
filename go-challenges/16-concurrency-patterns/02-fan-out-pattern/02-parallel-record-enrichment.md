# Exercise 2: Parallel Record Enrichment

A common backend job is to take a batch of records and enrich each one by calling a slower downstream service — a profile lookup, a geo-IP resolver, a pricing API. Doing it sequentially wastes the latency; doing it with one goroutine per record floods the dependency. This module fans the batch out to a bounded pool of workers that call an injected `Enricher`, returns the results in the original input order, and aggregates every per-record failure into one joined error instead of failing fast.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
enrich.go            Record, Enriched, Enricher (injected dependency), EnrichBatch
cmd/
  demo/
    main.go          enrich five records through a fake directory service, in order
enrich_test.go       order preservation, worker bound via a concurrency gauge, error aggregation, cancellation
```

- Files: `enrich.go`, `cmd/demo/main.go`, `enrich_test.go`.
- Implement: the `Enricher` interface and `EnrichBatch(ctx, enricher, records, workers) ([]Enriched, error)`.
- Test: results come back in input order, peak concurrency never exceeds `workers`, a failing record is aggregated (not fatal) while good records still enrich, and a cancelled context surfaces `context.Canceled`.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/16-concurrency-patterns/02-fan-out-pattern/02-parallel-record-enrichment/cmd/demo && cd go-solutions/16-concurrency-patterns/02-fan-out-pattern/02-parallel-record-enrichment
```

### Why an injected interface, ordered results, and joined errors

Three real-world requirements drive this design, and each one shapes the code.

The first is that the downstream service is a dependency, not a detail. `EnrichBatch` accepts an `Enricher` interface so it never imports an HTTP or gRPC client; production passes a real client, the test passes a fake that can fail on command and count its own concurrency. This is the difference between a fan-out you can test and one you can only hope works.

The second is result ordering. Fan-out destroys input order — workers finish in whatever order the scheduler and the downstream latencies dictate. Many callers need the output aligned with the input (row 3 of the result is the enrichment of row 3 of the request). The cheap fix would be to sort afterward, but there is a cheaper one: give every job its input index and have the worker write its result to `results[idx]`. Because each index is written by exactly one worker and the slice is never reallocated, distinct-index writes touch disjoint memory, so no lock is needed and the race detector stays silent. The output is ordered for free, by construction rather than by sorting.

The third is error handling. Failing fast on the first bad record throws away the work already done on the other 999 and is rarely what a batch job wants. Instead every worker writes its per-record error into `errs[idx]` — the same disjoint-index trick — and `EnrichBatch` returns `errors.Join(errs...)` at the end. `errors.Join` returns `nil` when every element is `nil`, so the happy path naturally yields no error, and the joined error is still inspectable with `errors.Is` for sentinel checks like cancellation. A record that failed keeps its zero-valued slot in `results`, so the caller can tell exactly which rows are missing.

The worker bound is the jobs channel plus a fixed number of worker goroutines: the dispatcher sends one `job` per record, and only `workers` goroutines ever read, so at most `workers` calls to `Enrich` run at once no matter how large the batch is. The dispatcher sends inside a `select` on `ctx.Done()`, so a cancelled context stops feeding new work rather than enqueueing the entire batch into a void.

Create `enrich.go`:

```go
package enrich

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// Record is one input row to be enriched.
type Record struct {
	ID   int
	Name string
}

// Enriched is the result of enriching one Record.
type Enriched struct {
	ID      int
	Name    string
	Country string
}

// Enricher is the downstream dependency that turns a Record into an Enriched.
// It is an interface so the batch processor never depends on a concrete client:
// production injects an HTTP- or gRPC-backed implementation, tests inject a fake.
type Enricher interface {
	Enrich(ctx context.Context, r Record) (Enriched, error)
}

// EnrichBatch enriches records by fanning out to enricher across at most workers
// goroutines. It returns results in the SAME order as the input, regardless of
// the order in which workers happen to finish, and it aggregates every per-record
// failure into a single joined error instead of failing fast, so one bad record
// does not discard the work done on the rest.
//
// Ordering without a sort: each job carries its input index, and a worker writes
// its result to results[idx]. Distinct indices are written by distinct goroutines
// and never overlap, so no lock guards the slice and the race detector stays
// quiet. The errors slice is filled the same way and joined at the end.
func EnrichBatch(ctx context.Context, enricher Enricher, records []Record, workers int) ([]Enriched, error) {
	if workers < 1 {
		workers = 1
	}

	results := make([]Enriched, len(records))
	errs := make([]error, len(records))

	type job struct {
		idx int
		rec Record
	}
	jobs := make(chan job)

	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			for j := range jobs {
				if err := ctx.Err(); err != nil {
					errs[j.idx] = fmt.Errorf("record %d: %w", j.rec.ID, err)
					continue
				}
				out, err := enricher.Enrich(ctx, j.rec)
				if err != nil {
					errs[j.idx] = fmt.Errorf("record %d: %w", j.rec.ID, err)
					continue
				}
				results[j.idx] = out
			}
		}()
	}

	// Dispatch on the same goroutine, honoring cancellation so a cancelled
	// context stops feeding new work instead of enqueueing the whole batch.
	for i, r := range records {
		select {
		case jobs <- job{idx: i, rec: r}:
		case <-ctx.Done():
			errs[i] = fmt.Errorf("record %d: %w", r.ID, ctx.Err())
		}
	}
	close(jobs)
	wg.Wait()

	return results, errors.Join(errs...)
}
```

The cancellation path is worth tracing because it is where ownership of `errs[i]` could go wrong. Each index is written by exactly one goroutine: if the dispatcher's `jobs <- job` send succeeds, that record is owned by whichever worker receives it, and the worker writes `results[i]` or `errs[i]`; if instead the `ctx.Done()` case wins, the record was never dispatched and the dispatcher itself writes `errs[i]`. The two cases are mutually exclusive per index, so there is never a second writer, and the race detector confirms it.

### The runnable demo

The demo wires a `directoryEnricher` — a stand-in for a slow service that maps a name to a country and fails on unknown names — and enriches five records across three workers. One record (`mystery`) has no entry, so it fails: its result slot stays zero-valued and its error joins the returned error, while the other four come back enriched and in order.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"example.com/parallel-enrichment"
)

// directoryEnricher is a stand-in for a slow downstream service: it looks up a
// country by name and fails for unknown names.
type directoryEnricher struct {
	country map[string]string
}

func (d directoryEnricher) Enrich(ctx context.Context, r enrich.Record) (enrich.Enriched, error) {
	select {
	case <-time.After(3 * time.Millisecond):
	case <-ctx.Done():
		return enrich.Enriched{}, ctx.Err()
	}
	c, ok := d.country[r.Name]
	if !ok {
		return enrich.Enriched{}, fmt.Errorf("no country for %q", r.Name)
	}
	return enrich.Enriched{ID: r.ID, Name: strings.ToUpper(r.Name), Country: c}, nil
}

func main() {
	svc := directoryEnricher{country: map[string]string{
		"ada": "GB", "alan": "GB", "grace": "US", "edsger": "NL",
	}}
	records := []enrich.Record{
		{ID: 1, Name: "ada"},
		{ID: 2, Name: "grace"},
		{ID: 3, Name: "mystery"},
		{ID: 4, Name: "edsger"},
		{ID: 5, Name: "alan"},
	}

	results, err := enrich.EnrichBatch(context.Background(), svc, records, 3)

	for _, r := range results {
		fmt.Printf("id=%d name=%-7s country=%s\n", r.ID, r.Name, r.Country)
	}
	fmt.Println("error:", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
id=1 name=ADA     country=GB
id=2 name=GRACE   country=US
id=0 name=        country=
id=4 name=EDSGER  country=NL
id=5 name=ALAN    country=GB
```
```
error: record 3: no country for "mystery"
```

The third line is the failed record, printed straight from its zero-valued slot: it makes the partial-failure behavior visible, and the joined error names exactly which record failed.

### Tests

The tests pin each requirement independently. `TestEnrichBatchPreservesOrder` checks that `got[i]` is the enrichment of `recs[i]` for a 50-record batch across 8 workers. `TestEnrichBatchRespectsWorkerBound` uses a `gaugeEnricher` that records peak concurrent calls with atomics and asserts the peak never exceeds the worker count. `TestEnrichBatchAggregatesErrors` fails two records and asserts both are named in the joined error while the good records are still enriched and the failed slots are zero. `TestEnrichBatchCancellation` cancels mid-flight and asserts `errors.Is(err, context.Canceled)`.

Create `enrich_test.go`:

```go
package enrich

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// gaugeEnricher records the peak number of concurrent Enrich calls so a test can
// assert the worker bound is respected, and fails any record whose name is in
// failNames so error aggregation can be exercised.
type gaugeEnricher struct {
	failNames map[string]bool
	inFlight  atomic.Int64
	peak      atomic.Int64
}

func (g *gaugeEnricher) Enrich(ctx context.Context, r Record) (Enriched, error) {
	n := g.inFlight.Add(1)
	for {
		old := g.peak.Load()
		if n <= old || g.peak.CompareAndSwap(old, n) {
			break
		}
	}
	defer g.inFlight.Add(-1)

	// Hold the slot briefly so concurrent workers actually overlap.
	select {
	case <-time.After(2 * time.Millisecond):
	case <-ctx.Done():
		return Enriched{}, ctx.Err()
	}

	if g.failNames[r.Name] {
		return Enriched{}, fmt.Errorf("lookup failed for %q", r.Name)
	}
	return Enriched{ID: r.ID, Name: strings.ToUpper(r.Name), Country: "XX"}, nil
}

func sampleRecords(n int) []Record {
	recs := make([]Record, n)
	for i := range recs {
		recs[i] = Record{ID: i + 1, Name: fmt.Sprintf("name-%02d", i+1)}
	}
	return recs
}

func TestEnrichBatchPreservesOrder(t *testing.T) {
	t.Parallel()

	recs := sampleRecords(50)
	g := &gaugeEnricher{}
	got, err := EnrichBatch(context.Background(), g, recs, 8)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for i, r := range recs {
		if got[i].ID != r.ID {
			t.Fatalf("position %d: got ID %d, want %d (order not preserved)", i, got[i].ID, r.ID)
		}
		if got[i].Name != strings.ToUpper(r.Name) {
			t.Fatalf("position %d: got name %q, want %q", i, got[i].Name, strings.ToUpper(r.Name))
		}
	}
}

func TestEnrichBatchRespectsWorkerBound(t *testing.T) {
	t.Parallel()

	const workers = 4
	g := &gaugeEnricher{}
	if _, err := EnrichBatch(context.Background(), g, sampleRecords(64), workers); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if peak := g.peak.Load(); peak > workers {
		t.Fatalf("peak concurrency %d exceeded worker bound %d", peak, workers)
	}
}

func TestEnrichBatchAggregatesErrors(t *testing.T) {
	t.Parallel()

	recs := sampleRecords(10)
	g := &gaugeEnricher{failNames: map[string]bool{"name-03": true, "name-07": true}}
	got, err := EnrichBatch(context.Background(), g, recs, 4)
	if err == nil {
		t.Fatal("expected a joined error, got nil")
	}
	if !strings.Contains(err.Error(), "record 3") || !strings.Contains(err.Error(), "record 7") {
		t.Fatalf("joined error must mention both failures, got: %v", err)
	}
	// The good records are still enriched: failure is aggregated, not fatal.
	if got[0].Name != "NAME-01" {
		t.Fatalf("good record was discarded: got %+v", got[0])
	}
	// A failed record keeps its zero value in the results slot.
	if got[2] != (Enriched{}) {
		t.Fatalf("failed record should be zero-valued, got %+v", got[2])
	}
}

func TestEnrichBatchCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	var done sync.WaitGroup
	done.Add(1)
	var err error
	go func() {
		defer done.Done()
		_, err = EnrichBatch(ctx, &gaugeEnricher{}, sampleRecords(1000), 4)
	}()
	time.Sleep(5 * time.Millisecond)
	cancel()
	done.Wait()

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected a context.Canceled in the joined error, got: %v", err)
	}
}
```

## Review

The design is correct when three invariants hold together. Ordering is by construction: each worker writes only `results[idx]` and `errs[idx]` for the index on its job, so the output aligns with the input without a sort and without a lock. The worker bound is real: the gauge test proves at most `workers` calls to `Enrich` overlap, which is what keeps the downstream service from being flooded. Errors are aggregated, not fatal: `errors.Join` collapses the per-record errors into one value that is `nil` on the happy path and `errors.Is`-inspectable otherwise, and the failed records are identifiable by their zero-valued result slots.

Common mistakes for this feature. The first is guarding `results` with a mutex out of reflex — unnecessary and slower here, because disjoint-index writes never alias; the lock would only matter if two workers could write the same index, which the per-job index forbids. The second is failing fast on the first error and discarding the rest of the batch, when the caller almost always wants every result it can get plus a list of what failed. The third is unbounded fan-out — one goroutine per record — which trades a latency problem for a "you just DDoSed your own dependency" problem; the fixed worker pool is the bound. The fourth is ignoring the context, so a cancelled batch keeps calling a service nobody is waiting on.

## Resources

- [`errors.Join`](https://pkg.go.dev/errors#Join) — the standard-library aggregator this module returns; note that it yields `nil` when every joined error is `nil`.
- [Go Blog: Go Concurrency Patterns — Pipelines and cancellation](https://go.dev/blog/pipelines) — the fan-out skeleton and the `done`/context cancellation this batch processor builds on.
- [Go Memory Model](https://go.dev/ref/mem) — why writes to distinct elements of a shared slice need no synchronization, and what the `WaitGroup` happens-before guarantee gives you.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-fan-out-core.md](01-fan-out-core.md) | Next: [03-distributing-writes-with-backpressure.md](03-distributing-writes-with-backpressure.md)
