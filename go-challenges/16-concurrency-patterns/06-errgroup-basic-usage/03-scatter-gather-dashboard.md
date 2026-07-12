# Exercise 3: Scatter-Gather a Dashboard

A dashboard page is the canonical scatter-gather: it is assembled from a dozen independent back-ends — CPU stats, recent orders, a billing summary, a feature-flag service — and the page is only as fast as the slowest one if you fetch them serially. This exercise builds `Assemble`, which fetches every widget concurrently with `errgroup`, bounds the fan-out with `SetLimit`, returns the widgets in request order with no sorting step, and fails the whole page fast the moment any one back-end errors.

This module is fully self-contained: its own `go mod init`, a direct `errgroup` import, its own demo and tests. The gate fetches `errgroup`; locally run `go mod tidy` once.

## What you'll build

```text
dashboard.go           Widget, WidgetFunc, Source, Assemble(ctx, maxParallel, sources...)
cmd/
  demo/
    main.go            assemble a four-widget dashboard, then show a failing widget aborting the page
dashboard_test.go      request-order preservation, fail-fast with sibling cancellation, bounded concurrency
```

- Files: `dashboard.go`, `cmd/demo/main.go`, `dashboard_test.go`.
- Implement: `Assemble(ctx context.Context, maxParallel int, sources ...Source) ([]Widget, error)` over `errgroup.WithContext` + `SetLimit`, writing each result into its own index.
- Test: results come back in source order regardless of fetch latency; one failure cancels the in-flight siblings and returns the wrapped error with a nil slice; `SetLimit` caps the observed concurrency.
- Verify: `go test -race ./...`

Set up the module:

```bash
go get golang.org/x/sync/errgroup
```

### Why this is the shape every aggregating endpoint wants

An endpoint that builds a composite response has four requirements that pull against each other, and `Assemble` satisfies all four at once. It must be concurrent, because serial fetches make the page's latency the sum of the parts instead of the max. It must be bounded, because an unbounded fan-out across a burst of requests opens thousands of sockets and can topple the very back-ends it depends on. It must preserve order, because the caller wants `widgets[0]` to be the CPU panel it asked for first, not whichever back-end happened to answer first. And it must fail fast, because if the billing service is down there is no point waiting four seconds for the slow analytics widget before returning the error to the user.

Concurrency and fail-fast come straight from `errgroup.WithContext`: each source runs under `g.Go`, and the first one to return a non-nil error cancels the shared context, so every other source that is threading that context into its fetch stops early. Bounding comes from `g.SetLimit(maxParallel)`, called before any `g.Go`; once `maxParallel` goroutines are active, the next `g.Go` call blocks until a slot frees, so the loop naturally throttles the fan-out without a hand-built semaphore. A non-positive `maxParallel` skips the limit and runs unbounded.

Order preservation is the part people reach for `sync.Mutex` and a post-hoc sort to solve, and it needs neither. The trick is that the result slice is pre-sized to `len(sources)` before any goroutine starts, and the goroutine launched for source `i` writes only `widgets[i]`. Distinct indices are distinct memory, so there is no shared write and no mutex; the slice is already in request order because position, not completion time, decides where a result lands. The only synchronization needed is the happens-before edge that `g.Wait` provides: every goroutine writes its index before returning, `Wait` returns only after all goroutines have returned, so reading the whole slice after `Wait` sees every write. Reading it before `Wait`, or sharing an index, is the data race that `-race` exists to catch.

One detail in the failure path matters for the API contract: when `g.Wait` returns an error, `Assemble` returns `nil, err` rather than the half-filled slice. A partially-assembled dashboard is worse than none — the caller cannot tell which panels are real and which are zero values — so a failed assembly yields no widgets at all.

Create `dashboard.go`:

```go
package dashboard

import (
	"context"
	"fmt"

	"golang.org/x/sync/errgroup"
)

// Widget is one rendered panel of a dashboard.
type Widget struct {
	Name    string
	Content string
}

// WidgetFunc fetches a single widget's content. It must honor ctx so that a
// sibling's failure can cancel it instead of letting it run to completion.
type WidgetFunc func(ctx context.Context) (string, error)

// Source pairs a widget's name with the function that renders it.
type Source struct {
	Name string
	Fn   WidgetFunc
}

// Assemble fetches every source concurrently and returns the widgets in the
// same order as sources. If any source fails, the context handed to the
// in-flight sources is cancelled and Assemble returns the first error wrapped
// with the failing widget's name, along with a nil slice — a half-built
// dashboard is never returned. maxParallel bounds the number of in-flight
// fetches; a value <= 0 means unbounded.
func Assemble(ctx context.Context, maxParallel int, sources ...Source) ([]Widget, error) {
	widgets := make([]Widget, len(sources))
	g, ctx := errgroup.WithContext(ctx)
	if maxParallel > 0 {
		g.SetLimit(maxParallel)
	}
	for i, src := range sources {
		g.Go(func() error {
			content, err := src.Fn(ctx)
			if err != nil {
				return fmt.Errorf("widget %q: %w", src.Name, err)
			}
			widgets[i] = Widget{Name: src.Name, Content: content}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return widgets, nil
}
```

### The runnable demo

The demo assembles a four-widget dashboard with a concurrency cap of three, prints the panels in request order, then runs a second assembly where one widget's back-end is down and shows the whole page aborting with the wrapped error.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"

	"example.com/dashboard"
)

func static(content string) dashboard.WidgetFunc {
	return func(ctx context.Context) (string, error) {
		return content, nil
	}
}

func main() {
	sources := []dashboard.Source{
		{Name: "cpu", Fn: static("73%")},
		{Name: "memory", Fn: static("5.2 GB")},
		{Name: "disk", Fn: static("41%")},
		{Name: "network", Fn: static("12 Mbps")},
	}
	widgets, err := dashboard.Assemble(context.Background(), 3, sources...)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println("=== dashboard ===")
	for _, w := range widgets {
		fmt.Printf("%s: %s\n", w.Name, w.Content)
	}

	failing := []dashboard.Source{
		{Name: "cpu", Fn: static("73%")},
		{Name: "disk", Fn: func(ctx context.Context) (string, error) {
			return "", errors.New("disk offline")
		}},
	}
	fmt.Println("=== with a failing widget ===")
	if _, err := dashboard.Assemble(context.Background(), 3, failing...); err != nil {
		fmt.Println("error:", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
=== dashboard ===
cpu: 73%
memory: 5.2 GB
disk: 41%
network: 12 Mbps
=== with a failing widget ===
error: widget "disk": disk offline
```

The four panels print in the order they were requested, not the order they were fetched. The second assembly returns the failing widget's name wrapped around its cause, and the dashboard slice is discarded.

### Tests

`TestAssembleOrder` gives three sources delays in reverse, so completion order is the opposite of request order, and asserts the returned slice is still in request order — proving the indexed-write design, not luck, is what orders the result. `TestAssembleFailFast` runs a fast failer and a slow cooperative sibling and asserts the result is the wrapped sentinel, the returned slice is nil, and the sibling was actually cancelled rather than running its full two seconds. `TestAssembleBoundedConcurrency` launches six sources with `maxParallel = 2`, has each fetch bump an atomic in-flight counter and record the high-water mark, and asserts the observed peak never exceeded the limit while all six still completed.

Create `dashboard_test.go`:

```go
package dashboard

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

func TestAssembleOrder(t *testing.T) {
	t.Parallel()

	mk := func(name, content string, d time.Duration) Source {
		return Source{Name: name, Fn: func(ctx context.Context) (string, error) {
			select {
			case <-time.After(d):
				return content, nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}}
	}
	sources := []Source{
		mk("a", "AAA", 30*time.Millisecond),
		mk("b", "BBB", 20*time.Millisecond),
		mk("c", "CCC", 10*time.Millisecond),
	}
	widgets, err := Assemble(context.Background(), 0, sources...)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	want := []Widget{{"a", "AAA"}, {"b", "BBB"}, {"c", "CCC"}}
	for i := range want {
		if widgets[i] != want[i] {
			t.Fatalf("widgets[%d] = %+v, want %+v", i, widgets[i], want[i])
		}
	}
}

func TestAssembleFailFast(t *testing.T) {
	t.Parallel()

	offline := errors.New("disk offline")
	cancelled := make(chan struct{})

	slow := Source{Name: "slow", Fn: func(ctx context.Context) (string, error) {
		select {
		case <-time.After(2 * time.Second):
			return "", errors.New("slow widget was not cancelled in time")
		case <-ctx.Done():
			close(cancelled)
			return "", ctx.Err()
		}
	}}
	bad := Source{Name: "disk", Fn: func(ctx context.Context) (string, error) {
		return "", offline
	}}

	widgets, err := Assemble(context.Background(), 0, slow, bad)
	if !errors.Is(err, offline) {
		t.Fatalf("Assemble err = %v, want wrapping %v", err, offline)
	}
	if widgets != nil {
		t.Fatalf("widgets = %v, want nil on error", widgets)
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("slow widget was not cancelled when a sibling failed")
	}
}

func TestAssembleBoundedConcurrency(t *testing.T) {
	t.Parallel()

	const limit = 2
	const n = 6
	var active, peak atomic.Int32

	sources := make([]Source, n)
	for i := range sources {
		sources[i] = Source{Name: fmt.Sprintf("w%d", i), Fn: func(ctx context.Context) (string, error) {
			cur := active.Add(1)
			for {
				old := peak.Load()
				if cur <= old || peak.CompareAndSwap(old, cur) {
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
			active.Add(-1)
			return "ok", nil
		}}
	}

	widgets, err := Assemble(context.Background(), limit, sources...)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if len(widgets) != n {
		t.Fatalf("got %d widgets, want %d", len(widgets), n)
	}
	if got := peak.Load(); got > limit {
		t.Fatalf("observed %d concurrent fetches, want <= %d", got, limit)
	}
}
```

## Review

`Assemble` is correct when it is concurrent, ordered, bounded, and fail-fast all at once, and the three tests pin each property under `-race`. The order test is the load-bearing one: by giving later sources shorter delays it guarantees completion order differs from request order, so a green result can only come from the indexed-write design, not from accidental in-order completion. The fail-fast test confirms two things a careless implementation gets wrong — that the error is wrapped with the failing widget's identity so the caller knows which panel broke, and that the half-built slice is discarded rather than returned. The bounded test confirms `SetLimit` actually throttles rather than being decorative. The mistakes to avoid are the ones the concepts file names: sharing a slice index instead of pre-sizing and writing one index per goroutine (a race), reading the slice before `Wait` (a race), and assuming `errgroup` will stop a fetch that ignores the context (it will not — every `WidgetFunc` here threads `ctx` into its `select`).

## Resources

- [`golang.org/x/sync/errgroup`](https://pkg.go.dev/golang.org/x/sync/errgroup) — `WithContext`, `Go`, `Wait`, and especially `SetLimit`, the bounded-fan-out primitive this exercise leans on.
- [Go blog: Go Concurrency Patterns — Context](https://go.dev/blog/context) — propagating cancellation and deadlines across a request that fans out to several back-ends.
- [Google API Design Guide: errors](https://google.aip.dev/193) — why an aggregating endpoint should surface which dependency failed rather than a bare error, which is what the wrapped `%w` message provides.

---

Back to [02-group-internals.md](02-group-internals.md) | Next: [04-parallel-validation.md](04-parallel-validation.md)
