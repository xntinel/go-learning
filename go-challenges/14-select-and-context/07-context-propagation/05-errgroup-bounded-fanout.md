# Exercise 5: Bounded Parallel Enrichment with errgroup and Shared Cancellation

The hand-rolled fan-out in Exercise 2 works, but real batch enrichment adds two
requirements it did not handle: cap the number of simultaneous outbound calls so
you do not stampede a downstream, and stop the whole batch the instant one call
fails. This exercise rewrites the fan-out with `golang.org/x/sync/errgroup`, which
folds the WaitGroup, the shared-cancel wiring, and the first-error propagation into
a few lines, and adds `SetLimit` to bound concurrency.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. It requires the external module `golang.org/x/sync`.

## What you'll build

```text
enrich/                      independent module: example.com/enrich
  go.mod                     go 1.24; require golang.org/x/sync
  enrich.go                  Enrich(ctx, ids, limit, fetch): errgroup.WithContext + SetLimit,
                             results into a preallocated slice indexed by position
  cmd/
    demo/
      main.go                enriches six IDs with a concurrency cap of two
  enrich_test.go             collects-all, first-error-cancels-rest, respects-limit (atomic gauge)
```

Files: `enrich.go`, `cmd/demo/main.go`, `enrich_test.go`.
Implement: `Enrich(ctx, ids, limit, fetch)` using `errgroup.WithContext(ctx)`,
`Group.SetLimit(limit)`, one `Group.Go` per ID writing into `results[i]`, and
`Group.Wait()`.
Test: the success path fills the slice in order; injecting one failure makes
`Wait` return that error and leaves some calls never started; peak concurrency
never exceeds the limit, measured with an atomic gauge.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/enrich/cmd/demo
cd ~/go-exercises/enrich
go mod init example.com/enrich
go mod edit -go=1.24
go get golang.org/x/sync/errgroup
```

### What errgroup buys you over the hand-rolled version

`errgroup.WithContext(parent)` returns a `*Group` and a derived `ctx`. Three
behaviors matter. First, the derived `ctx` is cancelled the moment the first
`Go`-registered function returns a non-nil error (or when `Wait` returns) — so if
you hand that `ctx` to every `fetch`, the first failure automatically cancels all
the still-running calls, which is the shared-cancellation property you wrote by
hand in Exercise 2, now for free. Second, `Wait` returns the *first* non-nil error
and blocks until every goroutine has finished, so there is no channel to size and
no `close`-after-`Wait` dance. Third, `SetLimit(n)` bounds the number of in-flight
goroutines: `Go` blocks until a slot is free, so a batch of ten thousand IDs
against a downstream that tolerates twenty concurrent connections runs twenty at a
time instead of opening ten thousand sockets.

Two correctness details the code must get right. Results go into a *preallocated
slice indexed by position* (`results[i]`), never a shared map — each goroutine
writes a distinct index, so there is no data race and no lock. And the loop
variable: on Go 1.22+ each `for i := range ids` iteration has its own `i`, so
capturing `i` and `ids[i]` inside the closure is safe; on older toolchains you
would need an explicit copy. Writing `for i := range ids` with index-based writes
is the idiom that is correct on every modern toolchain and sidesteps the classic
"all goroutines see the last value" bug.

Create `enrich.go`:

```go
package enrich

import (
	"context"
	"fmt"

	"golang.org/x/sync/errgroup"
)

// Fetcher hydrates a single ID from a downstream, respecting ctx.
type Fetcher func(ctx context.Context, id int) (string, error)

// Enrich hydrates every id concurrently, capping in-flight calls at limit (<=0
// means unlimited). Results are written into a preallocated slice indexed by
// position, so there is no shared-map race. The first fetch error cancels the
// shared context, so the remaining calls observe ctx.Done and stop, and Enrich
// returns that error.
func Enrich(ctx context.Context, ids []int, limit int, fetch Fetcher) ([]string, error) {
	results := make([]string, len(ids))
	g, ctx := errgroup.WithContext(ctx)
	if limit > 0 {
		g.SetLimit(limit)
	}
	for i := range ids {
		g.Go(func() error {
			v, err := fetch(ctx, ids[i])
			if err != nil {
				return fmt.Errorf("enrich id=%d: %w", ids[i], err)
			}
			results[i] = v
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

The demo enriches six IDs with a cap of two, so the work proceeds in three waves.
A trivial `fetch` returns `user-<id>` after a short sleep.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"example.com/enrich"
)

func main() {
	fetch := func(ctx context.Context, id int) (string, error) {
		select {
		case <-time.After(5 * time.Millisecond):
			return fmt.Sprintf("user-%d", id), nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	names, err := enrich.Enrich(ctx, []int{1, 2, 3, 4, 5, 6}, 2, fetch)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println(strings.Join(names, ","))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
user-1,user-2,user-3,user-4,user-5,user-6
```

### Tests

`TestErrgroupCollectsAll` checks the ordered slice on the success path.
`TestErrgroupFirstErrorCancelsRest` injects a failure on one ID and asserts `Wait`
surfaces it *and* that, because the shared context cancelled early, some fetches
returned before doing any work (started < N). `TestErrgroupRespectsLimit` uses an
atomic gauge to record peak concurrency and asserts it never exceeds the limit.

Create `enrich_test.go`:

```go
package enrich

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

var errBoom = errors.New("downstream boom")

func TestErrgroupCollectsAll(t *testing.T) {
	t.Parallel()

	fetch := func(ctx context.Context, id int) (string, error) {
		return "u" + itoa(id), nil
	}
	got, err := Enrich(context.Background(), []int{1, 2, 3}, 2, fetch)
	if err != nil {
		t.Fatalf("Enrich: err = %v, want nil", err)
	}
	want := []string{"u1", "u2", "u3"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestErrgroupFirstErrorCancelsRest(t *testing.T) {
	t.Parallel()

	var started int64
	fetch := func(ctx context.Context, id int) (string, error) {
		// Respect cancellation before doing any work.
		if err := ctx.Err(); err != nil {
			return "", err
		}
		atomic.AddInt64(&started, 1)
		if id == 0 {
			return "", errBoom // fail fast, cancelling the shared context
		}
		select {
		case <-time.After(50 * time.Millisecond):
			return "ok", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}

	ids := []int{0, 1, 2, 3, 4, 5} // id 0 fails immediately
	_, err := Enrich(context.Background(), ids, 2, fetch)
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want errBoom", err)
	}
	if n := atomic.LoadInt64(&started); n >= int64(len(ids)) {
		t.Fatalf("all %d fetches started; expected the shared cancel to skip some (started=%d)", len(ids), n)
	}
}

func TestErrgroupRespectsLimit(t *testing.T) {
	t.Parallel()

	const limit = 3
	var inFlight, peak int64
	fetch := func(ctx context.Context, id int) (string, error) {
		n := atomic.AddInt64(&inFlight, 1)
		for {
			p := atomic.LoadInt64(&peak)
			if n <= p || atomic.CompareAndSwapInt64(&peak, p, n) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		atomic.AddInt64(&inFlight, -1)
		return "ok", nil
	}

	ids := make([]int, 20)
	for i := range ids {
		ids[i] = i
	}
	if _, err := Enrich(context.Background(), ids, limit, fetch); err != nil {
		t.Fatalf("Enrich: err = %v, want nil", err)
	}
	if p := atomic.LoadInt64(&peak); p > limit {
		t.Fatalf("peak concurrency = %d, want <= %d", p, limit)
	}
}

func ExampleEnrich() {
	fetch := func(ctx context.Context, id int) (string, error) {
		return fmt.Sprintf("user-%d", id), nil
	}

	names, _ := Enrich(context.Background(), []int{1, 2, 3}, 2, fetch)
	fmt.Println(names)
	// Output: [user-1 user-2 user-3]
}

// itoa is a tiny local helper so the test needs no extra imports.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
```

## Review

The rewrite is correct when it reproduces the shared-cancellation behavior of the
hand-rolled fan-out with less code and adds a real concurrency bound. The three
tests map to the three guarantees: `Wait` returns the first error,
first-error-cancels-the-rest (so `started < N`), and `SetLimit` actually caps peak
concurrency. The subtle part is the results slice — writing into `results[i]` from
each goroutine is race-free precisely because the indices are disjoint; swapping it
for a shared `map[int]string` would fail `-race` immediately, which is why the
concepts insist on index-based writes. Run `go test -race`; a green race build is
the proof that the position-indexed writes and the atomic gauges are correct.

## Resources

- [`golang.org/x/sync/errgroup`](https://pkg.go.dev/golang.org/x/sync/errgroup) — `WithContext`, `Go`, `SetLimit`, `Wait`.
- [Go Blog: Go Concurrency Patterns: Pipelines and cancellation](https://go.dev/blog/pipelines) — the fan-out and cancellation model errgroup formalizes.
- [Go 1.22 loop variable scoping](https://go.dev/blog/loopvar-preview) — why `for i := range ids` is safe to capture.

---

Prev: [04-cancellation-cause-classification.md](04-cancellation-cause-classification.md) | Back to [00-concepts.md](00-concepts.md) | Next: [06-request-scoped-values.md](06-request-scoped-values.md)
