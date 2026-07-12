# Exercise 1: Run One Background Task and Block Until It Finishes

The smallest useful goroutine primitive is a function that launches exactly one
piece of background work and returns only after that work has fully completed.
The real framing is a cache-prewarm step: before an HTTP handler starts serving,
a warm-up must finish, and the caller must be able to depend on that.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
prewarm/                     independent module: example.com/prewarm
  go.mod
  fanout.go                  FanOut(work func()) — launch one goroutine, join it
  cmd/
    demo/
      main.go                prewarm a cache before "serving"
  fanout_test.go             ran-exactly-once + waited-for-completion, under -race
```

Files: `fanout.go`, `cmd/demo/main.go`, `fanout_test.go`.
Implement: `FanOut(work func())` that runs `work` in one goroutine and returns only after it completes.
Test: an `atomic.Int64` proves `work` ran exactly once; a `done` channel closed inside `work`, read with a non-blocking `select` after `FanOut` returns, proves the function waited.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/13-goroutines-and-channels/01-your-first-goroutine/01-fanout-single-background-task/cmd/demo
cd go-solutions/13-goroutines-and-channels/01-your-first-goroutine/01-fanout-single-background-task
```

### Why waiting is the whole point

`FanOut` is deliberately trivial to write and easy to get subtly wrong. The
temptation is to think "I launched the goroutine, my job is done" and return.
That is the vanishing-work bug: if the caller is `main` and it returns, the
process exits and the goroutine is killed before `work` runs. Even when the
caller is a longer-lived function, returning early means the caller cannot depend
on the side effect having happened — the cache is not actually warm yet.

The fix is a join. `sync.WaitGroup` is the counter for exactly this: `Add(1)`
before the `go` statement (so `Wait` cannot observe a premature zero), a
`defer wg.Done()` as the first line of the goroutine (so the counter is
decremented no matter how `work` exits, including a panic), and `wg.Wait()` after
launching. When `FanOut` returns, `work` has provably completed. Note that this
particular helper is synchronous — it blocks its caller — which is correct for a
prewarm step whose completion is a precondition for serving. Fire-and-forget is a
different tool for a different job.

Create `fanout.go`:

```go
package fanout

import "sync"

// FanOut runs work in a new goroutine and returns only after work has fully
// completed. It is the join-before-return contract in its smallest form: a
// caller that depends on work's side effect (a warmed cache, a persisted
// record) can rely on it having happened once FanOut returns.
func FanOut(work func()) {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		work()
	}()
	wg.Wait()
}
```

### The runnable demo

The demo models a cache prewarm: a handler must not serve until the cache is
populated. `FanOut` runs the warm-up in the background and blocks until it is
done, so the "serving" line is only reached with a warm cache.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync/atomic"

	"example.com/prewarm" // package name is fanout
)

func main() {
	var warmed atomic.Bool

	fanout.FanOut(func() {
		// Expensive warm-up: load hot keys, compile templates, dial pools.
		warmed.Store(true)
	})

	if warmed.Load() {
		fmt.Println("cache warmed; ready to serve")
	} else {
		fmt.Println("BUG: serving with a cold cache")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
cache warmed; ready to serve
```

### Tests

`TestFanOutRunsTheWorkExactlyOnce` increments an `atomic.Int64` inside `work` and
asserts the count is exactly one — the goroutine ran, and it ran once.
`TestFanOutWaitsForCompletion` closes a `done` channel inside `work` and then, in
the goroutine that called `FanOut`, does a *non-blocking* `select`: if the channel
is already closed the function waited; if the `default` arm is taken, `FanOut`
returned before `work` finished. The `Example` pins a deterministic output.

Create `fanout_test.go`:

```go
package fanout

import (
	"fmt"
	"sync/atomic"
	"testing"
)

func TestFanOutRunsTheWorkExactlyOnce(t *testing.T) {
	t.Parallel()

	var counter atomic.Int64
	FanOut(func() {
		counter.Add(1)
	})
	if got := counter.Load(); got != 1 {
		t.Fatalf("counter = %d, want 1", got)
	}
}

func TestFanOutWaitsForCompletion(t *testing.T) {
	t.Parallel()

	done := make(chan struct{})
	FanOut(func() {
		close(done)
	})

	select {
	case <-done:
		// work finished before FanOut returned: correct.
	default:
		t.Fatal("FanOut returned before the work finished")
	}
}

func TestFanOutPropagatesClosureState(t *testing.T) {
	t.Parallel()

	var result atomic.Int64
	FanOut(func() {
		result.Store(42)
	})
	if got := result.Load(); got != 42 {
		t.Fatalf("result = %d, want 42", got)
	}
}

func ExampleFanOut() {
	var n atomic.Int64
	FanOut(func() {
		n.Add(1)
	})
	fmt.Println(n.Load())
	// Output: 1
}
```

## Review

`FanOut` is correct when it is a true barrier: after it returns, `work` has run
and run once. The two properties the tests pin are independent and both matter —
"ran once" (the `atomic.Int64` equals one) and "we waited" (the `done` channel is
already closed at the non-blocking `select`). The most common way to break this is
to drop `wg.Wait()`, which turns the barrier into a fire-and-forget launch; the
completion test then flakes or fails because `FanOut` races ahead of `work`.
Keep `wg.Add(1)` before the `go` statement and `defer wg.Done()` first inside the
goroutine, and run `go test -race` so any accidental unsynchronized read of shared
state in `work` is caught.

## Resources

- [Go Language Specification: Go statements](https://go.dev/ref/spec#Go_statements)
- [sync.WaitGroup](https://pkg.go.dev/sync#WaitGroup)
- [Effective Go: Goroutines](https://go.dev/doc/effective_go#goroutines)

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-fanout-n-workers-loopvar.md](02-fanout-n-workers-loopvar.md)
