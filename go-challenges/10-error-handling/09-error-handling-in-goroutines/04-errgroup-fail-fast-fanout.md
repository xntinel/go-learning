# Exercise 4: Fan Out to N Upstreams with errgroup, Cancelling Siblings on the First Error

When a request needs data from three replicas and all three must answer, letting
the two healthy ones keep working after the third has already failed is wasted
effort and wasted latency. This is the fail-fast fan-out, and `errgroup.WithContext`
is the production tool for it: the first worker to return non-nil cancels a derived
context, the siblings observe `ctx.Done()` and abort, and `Wait` returns that first
error. This module replaces the hand-rolled runner with errgroup and proves the
siblings actually get cancelled.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
fanout/                      independent module: example.com/fanout
  go.mod                     go 1.26; requires golang.org/x/sync
  fanout.go                  Upstream; FetchAll (errgroup.WithContext, fail-fast)
  cmd/
    demo/
      main.go                runnable demo: three upstreams, one fails, siblings cancel
  fanout_test.go             tests: first error via Wait, sibling observed cancellation, deadline-bounded
```

Files: `fanout.go`, `cmd/demo/main.go`, `fanout_test.go`.
Implement: `FetchAll(ctx, upstreams)` using `errgroup.WithContext` so each upstream runs in a `g.Go`, and the first non-nil return cancels the group's context; `Wait` returns that error.
Test: one upstream returns `errBoom`; `Wait`'s error `Is` `errBoom`; a slow sibling records (via atomic flag) that its `ctx.Err()` became `context.Canceled`; a blocking stub returns promptly under a test deadline.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
go get golang.org/x/sync/errgroup
```

### What errgroup.WithContext actually does

`g, ctx := errgroup.WithContext(parent)` returns a `*errgroup.Group` and a context
derived from `parent`. That derived `ctx` is cancelled at the first of two events:
any func passed to `g.Go` returns a non-nil error, or `g.Wait` returns. The first
non-nil error is remembered and returned by `g.Wait`; later errors are dropped
(fail-fast keeps the *first* cause). This is the entire mechanism that turns N
independent goroutines into a coordinated group — and the crucial caveat from the
concepts file applies: errgroup cancels the *context*, it does not stop your
workers. A worker that ignores `ctx.Done()` runs to completion regardless.
Fail-fast only works because each worker's blocking call is context-aware.

`FetchAll` models querying N upstreams where all must succeed. Each upstream's
`Query` receives the group's `ctx`; a well-behaved upstream selects on
`ctx.Done()` (or passes `ctx` to its HTTP/DB call) so that when a sibling fails and
the context is cancelled, it aborts instead of finishing needless work. `FetchAll`
returns the collected results and `g.Wait`'s error. On the happy path all
upstreams succeed, `Wait` returns nil, and every result is present. On the sad
path the first failure cancels the rest and its error comes back, possibly before
the slow siblings would have finished on their own.

One subtlety about the result slice: each worker writes to its own pre-assigned
index `results[i]`, never appending to a shared slice. Distinct indices of a slice
are independent memory, so concurrent writes to `results[0]`, `results[1]`, ...
need no mutex — a standard errgroup idiom that keeps the fast path lock-free. Only
the successful workers write; a cancelled worker leaves its slot as the zero
value, which the caller ignores because `Wait` returned an error.

Create `fanout.go`:

```go
package fanout

import (
	"context"

	"golang.org/x/sync/errgroup"
)

// Upstream is a named data source. Query must honor ctx so it can abort when a
// sibling fails and the group's context is cancelled.
type Upstream struct {
	Name  string
	Query func(ctx context.Context) (string, error)
}

// FetchAll queries every upstream concurrently with fail-fast semantics: the
// first Query to return a non-nil error cancels the shared context, the other
// upstreams observe ctx.Done and abort, and the first error is returned. On
// success every result is present, indexed to match upstreams.
func FetchAll(ctx context.Context, upstreams []Upstream) ([]string, error) {
	results := make([]string, len(upstreams))
	g, ctx := errgroup.WithContext(ctx)
	for i, u := range upstreams {
		g.Go(func() error {
			v, err := u.Query(ctx)
			if err != nil {
				return err
			}
			results[i] = v // distinct index: no mutex needed
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return results, nil
}
```

### The runnable demo

The demo queries three upstreams. Two return quickly; the third fails immediately.
The failure cancels the group, so `FetchAll` returns the error. A slow sibling that
watches `ctx.Done()` reports it was cancelled rather than running its full delay.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"example.com/fanout"
)

func main() {
	upstreams := []fanout.Upstream{
		{Name: "replica-a", Query: func(ctx context.Context) (string, error) {
			return "a-ok", nil
		}},
		{Name: "replica-b", Query: func(ctx context.Context) (string, error) {
			// Slow: would take a second, but aborts when the context is cancelled.
			select {
			case <-time.After(time.Second):
				return "b-ok", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}},
		{Name: "replica-c", Query: func(ctx context.Context) (string, error) {
			return "", errors.New("replica-c: connection refused")
		}},
	}

	start := time.Now()
	_, err := fanout.FetchAll(context.Background(), upstreams)
	fmt.Printf("FetchAll error: %v\n", err)
	fmt.Printf("returned fast (well under the 1s slow sibling): %t\n", time.Since(start) < 500*time.Millisecond)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
FetchAll error: replica-c: connection refused
returned fast (well under the 1s slow sibling): true
```

### Tests

`TestFailFastReturnsFirstError` asserts `Wait`'s error `Is` the injected sentinel.
`TestSiblingObservesCancellation` is the important one: a slow sibling records via
an `atomic.Bool` that its `ctx.Err()` became `context.Canceled`, proving the
failure actually propagated to and aborted the other worker — not that it merely
finished on its own. `TestBlockingUpstreamReturnsPromptly` guards against the leak
where a worker blocks forever: a stub that only unblocks on `ctx.Done()` must let
`FetchAll` return well within a test deadline. `TestAllSucceed` pins the happy
path. Everything runs under `-race`.

Create `fanout_test.go`:

```go
package fanout

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

var errBoom = errors.New("boom")

func TestAllSucceed(t *testing.T) {
	t.Parallel()
	ups := []Upstream{
		{Name: "a", Query: func(ctx context.Context) (string, error) { return "1", nil }},
		{Name: "b", Query: func(ctx context.Context) (string, error) { return "2", nil }},
	}
	got, err := FetchAll(context.Background(), ups)
	if err != nil {
		t.Fatalf("FetchAll() error = %v, want nil", err)
	}
	if len(got) != 2 || got[0] != "1" || got[1] != "2" {
		t.Fatalf("FetchAll() = %v, want [1 2]", got)
	}
}

func TestFailFastReturnsFirstError(t *testing.T) {
	t.Parallel()
	ups := []Upstream{
		{Name: "ok", Query: func(ctx context.Context) (string, error) { return "x", nil }},
		{Name: "bad", Query: func(ctx context.Context) (string, error) { return "", errBoom }},
	}
	_, err := FetchAll(context.Background(), ups)
	if !errors.Is(err, errBoom) {
		t.Fatalf("FetchAll() error = %v, want errBoom", err)
	}
}

func TestSiblingObservesCancellation(t *testing.T) {
	t.Parallel()
	var cancelled atomic.Bool
	ups := []Upstream{
		{Name: "fails", Query: func(ctx context.Context) (string, error) {
			return "", errBoom
		}},
		{Name: "slow", Query: func(ctx context.Context) (string, error) {
			select {
			case <-time.After(10 * time.Second):
				return "never", nil
			case <-ctx.Done():
				cancelled.Store(errors.Is(ctx.Err(), context.Canceled))
				return "", ctx.Err()
			}
		}},
	}
	_, err := FetchAll(context.Background(), ups)
	if !errors.Is(err, errBoom) {
		t.Fatalf("FetchAll() error = %v, want errBoom", err)
	}
	if !cancelled.Load() {
		t.Fatal("slow sibling was not cancelled; the group did not propagate the failure")
	}
}

func TestBlockingUpstreamReturnsPromptly(t *testing.T) {
	t.Parallel()
	ups := []Upstream{
		{Name: "fails", Query: func(ctx context.Context) (string, error) { return "", errBoom }},
		{Name: "blocks", Query: func(ctx context.Context) (string, error) {
			<-ctx.Done() // unblocks only when the group cancels
			return "", ctx.Err()
		}},
	}
	done := make(chan struct{})
	go func() {
		_, _ = FetchAll(context.Background(), ups)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("FetchAll did not return; a blocking upstream leaked")
	}
}
```

## Review

The fan-out is correct when the first failure both surfaces and cancels: `Wait`
returns the first error (`errors.Is` against the sentinel), and a sibling that
watches `ctx.Done()` records `context.Canceled` — proving the group propagated the
abort rather than the sibling merely finishing. The design fact that makes it work
is that `errgroup.WithContext` cancels the context on the first non-nil return; the
design fact that makes it *matter* is that each worker honors that context. Drop
the `ctx.Done()` branch from an upstream and it runs to completion no matter what
its siblings do — the classic "errgroup does not stop the work" trap. The
lock-free `results[i]` writes are safe only because the indices are distinct; never
`append` to a shared slice from these workers. Run `go test -race` and
`go vet ./...` to confirm.

## Resources

- [`golang.org/x/sync/errgroup`](https://pkg.go.dev/golang.org/x/sync/errgroup) — `WithContext`, `Go`, `Wait`, and the fail-fast contract.
- [`context.Context`](https://pkg.go.dev/context) — `Done`, `Err`, and cancellation propagation.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — why workers must honor `ctx.Done()`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [05-bounded-worker-pool-errgroup.md](05-bounded-worker-pool-errgroup.md)
