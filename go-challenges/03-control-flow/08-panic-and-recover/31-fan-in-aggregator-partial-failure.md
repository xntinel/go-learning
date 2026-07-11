# Exercise 31: Fan-In Aggregator: Partial Failure When One Source Panics

**Nivel: Intermedio** — validacion rapida (un test corto).

A report-generation job that pulls from several database shards, or a
dashboard that combines several partner APIs into one feed, is a classic
fan-in: N independent sources contribute records concurrently into one
combined result. One shard being down or returning a malformed response
that panics a naive parser must not silently corrupt or truncate the
records the *other* shards successfully contributed - the aggregator needs
to keep the good data and separately report exactly which sources failed,
so an operator can distinguish "this report is missing shard-b's orders"
from "this report has no data at all." This module builds `Aggregate`,
which fans out to every source concurrently and isolates each source's
panic to its own goroutine. It is fully self-contained: its own module,
demo, and tests.

## What you'll build

```text
faninagg/                   independent module: example.com/faninagg
  go.mod                     go 1.24
  faninagg.go                  Source, Result, Aggregate, runSource
  cmd/
    demo/
      main.go                runnable demo: 3 shards, one panics on an index out of range
  faninagg_test.go              surviving sources' items collected, ordinary error also tracked, empty
```

Files: `faninagg.go`, `cmd/demo/main.go`, `faninagg_test.go`.
Implement: `Aggregate(sources []Source) Result` that launches one goroutine per source, isolates each source's `Fetch` panic in `runSource`, merges every successful source's items into a sorted `Items` slice, and records every failure - error or panic - in `SourceErrs` keyed by source name.
Test: three sources where one panics, asserting the other two sources' items are both present and sorted, and that exactly one `SourceErrs` entry names the panicking source; a source returning a plain error is tracked the same way; an empty source list.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/fan-in-aggregator-partial-failure/cmd/demo
cd ~/go-exercises/fan-in-aggregator-partial-failure
go mod init example.com/faninagg
go mod edit -go=1.24
```

### Why the combined items are sorted before returning, not left in fetch order

Every source's `Fetch` runs concurrently in its own goroutine, appending
its items to a shared `Result.Items` slice under a mutex once it finishes.
Which source finishes first - and therefore whose items land first in the
slice - depends entirely on how long each source's `Fetch` happens to take,
which is not something `Aggregate`'s caller controls or should have to
reason about. If `Aggregate` returned `Items` in raw finish order, the exact
same call with the exact same sources could produce a different-looking
(though set-equal) result on every run, which makes both a demo's expected
output and a test's assertions dependent on scheduling luck. Sorting
`Items` once, after every source has finished, converts "probably the same
order most of the time" into "always the same order" - a small final step
that turns a flaky-by-construction result into a reproducible one, at
negligible cost for report-sized record counts.

The recover boundary lives in `runSource`, once per source goroutine: a
malformed response that trips an unguarded index or type assertion inside
one source's parsing logic panics only that source's own goroutine, and
`Aggregate` converts it into that source's `SourceErrs` entry exactly as if
it had returned an ordinary error. The mutex guarding `Result` is held only
for the brief append/error-record step, never around the `Fetch` call
itself, so slow sources never block fast ones from finishing and reporting
their own results.

Create `faninagg.go`:

```go
package faninagg

import (
	"fmt"
	"sort"
	"sync"
)

// Source is one upstream feed (a database shard, a partner API, a queue
// partition) that contributes records to the aggregate.
type Source struct {
	Name  string
	Fetch func() ([]string, error)
}

// Result is what Aggregate produced from one run.
type Result struct {
	Items      []string         // combined records from every source that succeeded
	SourceErrs map[string]error // source name -> error, for every source that failed or panicked
}

// Aggregate fans out to every source concurrently, one goroutine per
// source. A source panicking - a malformed partner response, an index
// computed from a field that was not actually present - must not stop the
// other sources from contributing their records: each source's Fetch runs
// under its own recover boundary in its own goroutine, so a panic in one
// source can never unwind into a sibling source's goroutine, and can never
// drop items a source that finished first already contributed.
//
// The combined Items are sorted before being returned so the result is
// reproducible regardless of which source's goroutine happened to finish
// first - callers should never have to guess an interleaving order that
// depends on the scheduler.
func Aggregate(sources []Source) Result {
	var wg sync.WaitGroup
	var mu sync.Mutex
	result := Result{SourceErrs: make(map[string]error)}

	for _, s := range sources {
		s := s
		wg.Add(1)
		go func() {
			defer wg.Done()
			items, err := runSource(s)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				result.SourceErrs[s.Name] = err
				return
			}
			result.Items = append(result.Items, items...)
		}()
	}
	wg.Wait()

	sort.Strings(result.Items)
	return result
}

// runSource is the recover boundary: exactly one source's untrusted Fetch.
func runSource(s Source) (items []string, err error) {
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(error); ok {
				err = fmt.Errorf("source %q panicked: %w", s.Name, e)
				return
			}
			err = fmt.Errorf("source %q panicked: %v", s.Name, r)
		}
	}()
	return s.Fetch()
}
```

### The runnable demo

Three shards contribute orders; `shard-b` panics on an index-out-of-range
read from a nil slice.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sort"

	"example.com/faninagg"
)

func main() {
	sources := []faninagg.Source{
		{Name: "shard-a", Fetch: func() ([]string, error) {
			return []string{"order-1", "order-2"}, nil
		}},
		{Name: "shard-b", Fetch: func() ([]string, error) {
			var rows []string
			return []string{rows[0]}, nil // index out of range
		}},
		{Name: "shard-c", Fetch: func() ([]string, error) {
			return []string{"order-5"}, nil
		}},
	}

	result := faninagg.Aggregate(sources)

	fmt.Println("items:", result.Items)

	names := make([]string, 0, len(result.SourceErrs))
	for name := range result.SourceErrs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Printf("source %s failed: %v\n", name, result.SourceErrs[name])
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
items: [order-1 order-2 order-5]
source shard-b failed: source "shard-b" panicked: runtime error: index out of range [0] with length 0
```

### Tests

`TestAggregateCollectsFromSurvivingSources` panics one of three sources and
asserts the surviving two sources' items are both present, sorted, and
that `SourceErrs` names exactly the panicking source.

Create `faninagg_test.go`:

```go
package faninagg

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestAggregateCollectsFromSurvivingSources(t *testing.T) {
	sources := []Source{
		{Name: "a", Fetch: func() ([]string, error) { return []string{"x1"}, nil }},
		{Name: "b", Fetch: func() ([]string, error) { panic(errors.New("shard down")) }},
		{Name: "c", Fetch: func() ([]string, error) { return []string{"x2"}, nil }},
	}

	result := Aggregate(sources)

	if !reflect.DeepEqual(result.Items, []string{"x1", "x2"}) {
		t.Fatalf("Items = %v, want [x1 x2]", result.Items)
	}
	if len(result.SourceErrs) != 1 {
		t.Fatalf("SourceErrs = %v, want exactly 1 entry", result.SourceErrs)
	}
	if err, ok := result.SourceErrs["b"]; !ok || !strings.Contains(err.Error(), "shard down") {
		t.Fatalf("SourceErrs[b] = %v, want a panic wrapping shard down", err)
	}
}

func TestAggregateOrdinaryErrorAlsoTracked(t *testing.T) {
	sources := []Source{
		{Name: "a", Fetch: func() ([]string, error) { return []string{"x1"}, nil }},
		{Name: "b", Fetch: func() ([]string, error) { return nil, errors.New("timeout") }},
	}
	result := Aggregate(sources)
	if len(result.Items) != 1 || result.Items[0] != "x1" {
		t.Fatalf("Items = %v, want [x1]", result.Items)
	}
	if result.SourceErrs["b"] == nil || result.SourceErrs["b"].Error() != "timeout" {
		t.Fatalf("SourceErrs[b] = %v, want timeout", result.SourceErrs["b"])
	}
}

func TestAggregateEmptySources(t *testing.T) {
	result := Aggregate(nil)
	if len(result.Items) != 0 || len(result.SourceErrs) != 0 {
		t.Fatalf("result = %+v, want empty", result)
	}
}
```

## Review

`Aggregate` is correct when one source's panic can only ever cost the
caller that one source's contribution, never anyone else's, and when the
combined result is reproducible rather than order-dependent on goroutine
scheduling. The mutex's scope is worth double-checking in review: it must
cover only the shared-state update (the append or the map write), never the
`Fetch` call itself - holding it around `Fetch` would serialize every
source behind the slowest one and defeat the entire point of fanning out
concurrently in the first place.

## Resources

- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — the fan-in half of the fan-out/fan-in pattern this module implements.
- [Go Blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover) — the per-source recover boundary in `runSource`.
- [sync.WaitGroup](https://pkg.go.dev/sync#WaitGroup) — coordinating the wait for every concurrently-fetching source.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [30-distributed-tracing-child-spans.md](30-distributed-tracing-child-spans.md) | Next: [32-semaphore-token-bucket-release.md](32-semaphore-token-bucket-release.md)
