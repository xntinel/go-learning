# Exercise 2: Fan Out Aggregate Calls on One Shared Cancellation Signal

A dashboard endpoint rarely reads a single row; it fans out to several backends
and stitches the results together. This exercise builds that `Aggregate` step by
hand — N repo-backed lookups launched concurrently, all sharing one context — and
proves that cancelling the shared context wakes every goroutine at the same
`ctx.Done()` close, so a tight deadline fails the whole fan-out fast instead of
serially.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
fanout/                      independent module: example.com/fanout
  go.mod                     go 1.24
  fanout.go                  Repo.Get; Service.Aggregate (WaitGroup + buffered chan + close-after-wait)
  cmd/
    demo/
      main.go                aggregates two keys inside a generous budget
  fanout_test.go             both-values happy path, shared-cancel fast fail, goroutine-leak check
```

Files: `fanout.go`, `cmd/demo/main.go`, `fanout_test.go`.
Implement: a `Service.Aggregate(ctx, keys...)` that launches one goroutine per key,
each calling `repo.Get(ctx, key)` with the *shared* context, collecting through a
buffered channel with a `sync.WaitGroup` and `close`-after-`Wait`.
Test: a generous-budget path returning all values; a tight-budget path asserting
`errors.Is(DeadlineExceeded)` with elapsed well under the summed latency; a
goroutine-count check proving nothing outlives the aborted aggregate.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/fanout/cmd/demo
cd ~/go-exercises/fanout
go mod init example.com/fanout
go mod edit -go=1.24
```

### Why one shared context, not one per goroutine

The instinct to give each fanned-out goroutine "its own" context is exactly wrong
here. The whole reason to fan out under a deadline is that the *request* has a
budget, and every branch of the fan-out is spending the same budget. Hand every
goroutine the identical `ctx` and a single close of `ctx.Done()` — whether from a
timeout or an explicit cancel — releases all of them at once. If instead each
goroutine got a fresh timeout, the calls would effectively time out one after
another, and the aggregate could take far longer than the request's real budget.

The mechanics are a classic Go fan-out. Launch N goroutines, each writing its
result into a *buffered* channel sized to N so no goroutine ever blocks on the
send (even the ones whose result the receiver will never read after an early
error). A `sync.WaitGroup` counts them; a `close(out)` after `wg.Wait()` lets the
receiver's `range out` terminate cleanly instead of deadlocking. Because the
channel is buffered to capacity, every goroutine can send-and-exit even if the
aggregate is about to return an error from the first failure it reads — which is
precisely what keeps this fan-out from leaking goroutines. The goroutine-leak test
checks that invariant directly.

Under a 20ms budget against an 80ms repo delay, both goroutines block in their
`select`, both wake on the same `ctx.Done()` close at ~20ms, both send their
wrapped `DeadlineExceeded` error, and `Aggregate` returns the first one it reads —
in roughly 20ms, not 160ms. That "not 160ms" is the property the cancel test pins
down.

Create `fanout.go`:

```go
package fanout

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrNotFound is returned when a key is absent from the repository.
var ErrNotFound = errors.New("fanout: key not found")

// Repo simulates a slow, cancellation-aware backend read.
type Repo struct {
	mu    sync.RWMutex
	data  map[string]string
	delay time.Duration
}

// NewRepo returns a Repo that takes delay per read, seeded with a copy of kvs.
func NewRepo(delay time.Duration, kvs map[string]string) *Repo {
	cp := make(map[string]string, len(kvs))
	for k, v := range kvs {
		cp[k] = v
	}
	return &Repo{data: cp, delay: delay}
}

// Get races the simulated latency against ctx.Done and wraps the cause on abort.
func (r *Repo) Get(ctx context.Context, key string) (string, error) {
	select {
	case <-time.After(r.delay):
	case <-ctx.Done():
		return "", fmt.Errorf("repo.Get %q: %w", key, ctx.Err())
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	v, ok := r.data[key]
	if !ok {
		return "", fmt.Errorf("repo.Get %q: %w", key, ErrNotFound)
	}
	return v, nil
}

// Service fans out repo reads over a shared context.
type Service struct {
	repo *Repo
}

// NewService wraps repo in a Service.
func NewService(repo *Repo) *Service { return &Service{repo: repo} }

// Aggregate launches one goroutine per key, all sharing ctx, and collects the
// results. A single close of ctx.Done cancels every branch at once; the first
// error encountered is returned. The result channel is buffered to len(keys) so
// no goroutine blocks on send, even when Aggregate returns early on an error.
func (s *Service) Aggregate(ctx context.Context, keys ...string) (map[string]string, error) {
	type result struct {
		key string
		val string
		err error
	}
	out := make(chan result, len(keys))
	var wg sync.WaitGroup
	for _, key := range keys {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := s.repo.Get(ctx, key)
			out <- result{key: key, val: v, err: err}
		}()
	}
	wg.Wait()
	close(out)

	got := make(map[string]string, len(keys))
	for r := range out {
		if r.err != nil {
			return nil, fmt.Errorf("aggregate %q: %w", r.key, r.err)
		}
		got[r.key] = r.val
	}
	return got, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"sort"
	"time"

	"example.com/fanout"
)

func main() {
	repo := fanout.NewRepo(10*time.Millisecond, map[string]string{"u1": "alice", "u2": "bob"})
	svc := fanout.NewService(repo)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	got, err := svc.Aggregate(ctx, "u1", "u2")
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	keys := make([]string, 0, len(got))
	for k := range got {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("%s=%s\n", k, got[k])
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
u1=alice
u2=bob
```

### Tests

`TestAggregateReturnsBothValues` covers the generous-budget path.
`TestAggregateCancelsBothOnTimeout` is the core: an 80ms-per-read repo under a
20ms budget must return a `DeadlineExceeded` error in well under the summed 160ms,
proving both reads were cancelled together rather than run in sequence.
`TestNoGoroutineLeak` samples `runtime.NumGoroutine` before and after an aborted
aggregate, with a short settle, to confirm no fanned-out goroutine outlives the
call.

Create `fanout_test.go`:

```go
package fanout

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"testing"
	"time"
)

func TestAggregateReturnsBothValues(t *testing.T) {
	t.Parallel()

	repo := NewRepo(5*time.Millisecond, map[string]string{"u1": "alice", "u2": "bob"})
	svc := NewService(repo)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	got, err := svc.Aggregate(ctx, "u1", "u2")
	if err != nil {
		t.Fatalf("Aggregate: err = %v, want nil", err)
	}
	if got["u1"] != "alice" || got["u2"] != "bob" {
		t.Fatalf("Aggregate = %v, want u1=alice u2=bob", got)
	}
}

func TestAggregateCancelsBothOnTimeout(t *testing.T) {
	t.Parallel()

	repo := NewRepo(80*time.Millisecond, map[string]string{"u1": "alice", "u2": "bob"})
	svc := NewService(repo)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := svc.Aggregate(ctx, "u1", "u2")
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	// Both reads share one deadline: total must be near one delay, not two.
	if elapsed > 60*time.Millisecond {
		t.Fatalf("aggregate did not cancel branches together: took %v", elapsed)
	}
}

func TestNoGoroutineLeak(t *testing.T) {
	repo := NewRepo(30*time.Millisecond, map[string]string{"u1": "alice", "u2": "bob"})
	svc := NewService(repo)

	before := runtime.NumGoroutine()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	_, _ = svc.Aggregate(ctx, "u1", "u2")
	cancel()

	// Let any stragglers wake on ctx.Done and exit.
	time.Sleep(60 * time.Millisecond)

	after := runtime.NumGoroutine()
	if after > before {
		t.Fatalf("goroutine leak: before=%d after=%d", before, after)
	}
}

func ExampleService_Aggregate() {
	repo := NewRepo(time.Millisecond, map[string]string{"u1": "alice", "u2": "bob"})
	svc := NewService(repo)

	got, _ := svc.Aggregate(context.Background(), "u1", "u2")
	fmt.Println(got["u1"], got["u2"])
	// Output: alice bob
}
```

## Review

The aggregate is correct when its wall-clock cost under cancellation is bounded by
a *single* read's latency, not the sum. That is the observable signature of "one
shared cancellation signal": if `TestAggregateCancelsBothOnTimeout` ever
approaches 160ms, the branches are being cancelled serially and the fan-out has
lost its point. The buffered channel is not an optimization but a correctness
requirement — size it to the number of goroutines so that a goroutine whose result
the receiver will discard after an early error can still complete its send and
exit; a smaller buffer reintroduces the leak that `TestNoGoroutineLeak` guards
against. Run `go test -race` to catch any accidental sharing across the result
channel. The next exercise rewrites this same fan-out with `errgroup`, which
folds the WaitGroup, the channel, and the shared-cancel wiring into a few lines
and adds a concurrency limit.

## Resources

- [Go Blog: Go Concurrency Patterns: Pipelines and cancellation](https://go.dev/blog/pipelines)
- [`sync.WaitGroup`](https://pkg.go.dev/sync#WaitGroup) — counting fanned-out goroutines.
- [`context` package](https://pkg.go.dev/context) — `Context.Done` and shared cancellation.

---

Prev: [01-layered-propagation-stack.md](01-layered-propagation-stack.md) | Back to [00-concepts.md](00-concepts.md) | Next: [03-wrapped-error-chain.md](03-wrapped-error-chain.md)
