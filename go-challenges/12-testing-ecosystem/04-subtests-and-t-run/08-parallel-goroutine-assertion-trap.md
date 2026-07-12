# Exercise 8: Assertions From Spawned Goroutines: The t.Fatal Trap

Testing a concurrent worker means spawning goroutines and checking what they
produced. The trap that silently corrupts these tests is calling `t.Fatal` from
inside a worker goroutine: it does not stop the test, so a "failed" assertion is
swallowed and the test can report PASS. This exercise builds a small concurrent
map-worker and tests it the correct way — outcomes flow back to the test goroutine,
which does the asserting.

This module is fully self-contained: its own `go mod init`, worker, demo, and
tests. Nothing here imports any other exercise.

## What you'll build

```text
workerpool/                 independent module: example.com/workerpool
  go.mod                    go 1.26
  pool.go                   type Result; func Map[T](ctx, inputs, fn) []Result
  cmd/
    demo/
      main.go               runnable demo: map a parse over inputs concurrently
  pool_test.go              channel-marshalled assertions on the test goroutine; -race
```

- Files: `pool.go`, `cmd/demo/main.go`, `pool_test.go`.
- Implement: `Map[T any](ctx, inputs []T, fn func(ctx, T) (int, error)) []Result`
  running `fn` over each input concurrently, results in input order.
- Test: workers send outcomes over a channel; the test goroutine ranges over them
  and calls `t.Fatalf` — never from a spawned goroutine.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/04-subtests-and-t-run/08-parallel-goroutine-assertion-trap/cmd/demo
cd go-solutions/12-testing-ecosystem/04-subtests-and-t-run/08-parallel-goroutine-assertion-trap
```

### Why t.Fatal in a goroutine is a silent bug

`t.Fatal`, `t.FailNow`, `t.SkipNow`, and `t.Parallel` are documented as callable
only from the goroutine running the test or subtest. The reason is their
mechanism: `FailNow` (which `Fatal` calls) marks the test failed and then invokes
`runtime.Goexit`, which unwinds the *current* goroutine. Called from a worker
goroutine, it unwinds the *worker*, not the test — so the worker stops, the test
keeps running, and because the worker exited quietly the test can go on to report
PASS. Your assertion "failed" and nothing happened. There is a secondary hazard —
touching `t` from another goroutine after the test function has returned is a data
race — but the primary one is the swallowed failure.

The fix is structural and always the same shape: workers must *marshal* their
outcomes back to the test goroutine, and the test goroutine does the asserting. A
buffered channel is the simplest carrier; a `sync.WaitGroup` plus an
index-addressed results slice works when you want ordered results; and
`golang.org/x/sync/errgroup` packages the "run N funcs, collect the first error"
pattern (its `Wait` returns that error to the caller's goroutine, where you assert
it). All three keep every `t.Fatal` on the test goroutine. Note that `t.Log`,
`t.Error`, and `t.Errorf` *are* safe to call from other goroutines — they only
record — but they do not stop anything, so they are not a substitute for asserting
a precondition.

`Map` itself writes each result to its own index, so the workers never touch
shared mutable state and `-race` stays clean without a lock.

Create `pool.go`:

```go
package workerpool

import (
	"context"
	"sync"
)

// Result pairs an input's position with its computed value or error.
type Result struct {
	Index int
	Value int
	Err   error
}

// Map runs fn over each input concurrently and returns the results in input
// order. Each goroutine writes only its own slice slot, so no synchronization of
// the results is needed. fn must itself be safe for concurrent use.
func Map[T any](ctx context.Context, inputs []T, fn func(context.Context, T) (int, error)) []Result {
	results := make([]Result, len(inputs))
	var wg sync.WaitGroup
	for i, in := range inputs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := fn(ctx, in)
			results[i] = Result{Index: i, Value: v, Err: err}
		}()
	}
	wg.Wait()
	return results
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"strconv"

	"example.com/workerpool"
)

func main() {
	inputs := []string{"10", "20", "x"}
	results := workerpool.Map(context.Background(), inputs, func(_ context.Context, s string) (int, error) {
		return strconv.Atoi(s)
	})
	for _, r := range results {
		if r.Err != nil {
			fmt.Printf("[%d] %q: error\n", r.Index, inputs[r.Index])
		} else {
			fmt.Printf("[%d] %q -> %d\n", r.Index, inputs[r.Index], r.Value)
		}
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
[0] "10" -> 10
[1] "20" -> 20
[2] "x": error
```

### The wrong way (do not do this)

This is the trap, shown as illustration only — it is not part of the built module:

```text
// BROKEN: t.Fatal on a worker goroutine unwinds the worker, not the test.
for _, in := range inputs {
	go func() {
		n, err := strconv.Atoi(in)
		if err != nil {
			t.Fatalf("parse %q: %v", in, err) // runs runtime.Goexit HERE; test keeps going
		}
		use(n)
	}()
}
// ... the test may now report PASS even though a parse failed.
```

### Tests

`TestConcurrentAssertion` spawns workers that send their outcome over a buffered
channel and never touch `t`; a separate closer goroutine closes the channel once
all workers are done; and the *test goroutine* ranges over the channel and calls
`t.Fatalf`. `TestMap` exercises the exported worker and asserts ordered results.
Both run under `-race`.

Create `pool_test.go`:

```go
package workerpool

import (
	"context"
	"strconv"
	"sync"
	"testing"
)

func TestConcurrentAssertion(t *testing.T) {
	t.Parallel()

	inputs := []string{"3", "14", "159"}
	type outcome struct {
		in  string
		got int
		err error
	}
	ch := make(chan outcome, len(inputs))

	var wg sync.WaitGroup
	for _, in := range inputs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := strconv.Atoi(in)
			// Correct: do NOT call t.Fatal here. Marshal the outcome back and
			// let the test goroutine assert on it.
			ch <- outcome{in: in, got: n, err: err}
		}()
	}
	go func() { wg.Wait(); close(ch) }()

	seen := make(map[string]int)
	for o := range ch { // ranges on the test goroutine — t.Fatalf is legal here
		if o.err != nil {
			t.Fatalf("parse %q: %v", o.in, o.err)
		}
		seen[o.in] = o.got
	}
	if len(seen) != len(inputs) {
		t.Fatalf("collected %d outcomes, want %d", len(seen), len(inputs))
	}
}

func TestMap(t *testing.T) {
	t.Parallel()

	inputs := []string{"1", "2", "3"}
	results := Map(t.Context(), inputs, func(_ context.Context, s string) (int, error) {
		return strconv.Atoi(s)
	})
	if len(results) != len(inputs) {
		t.Fatalf("got %d results, want %d", len(results), len(inputs))
	}
	for i, r := range results {
		if r.Index != i {
			t.Fatalf("results[%d].Index = %d, want %d", i, r.Index, i)
		}
		if r.Err != nil {
			t.Fatalf("results[%d].Err = %v, want nil", i, r.Err)
		}
		if want := i + 1; r.Value != want {
			t.Fatalf("results[%d].Value = %d, want %d", i, r.Value, want)
		}
	}
}
```

## Review

The correct concurrent-test shape is a one-way flow: workers *produce* outcomes
onto a channel (or an index-addressed slice), and the single test goroutine
*consumes* them and asserts. Every `t.Fatalf` in the tests above runs on the test
goroutine — inside the `range ch` loop or the sequential `TestMap` body — never
inside a `go func`. The illustrative broken block shows the failure mode: a
`t.Fatal` on a worker unwinds that worker via `runtime.Goexit` and the test can
still report PASS. `t.Log`/`t.Error` are safe from a goroutine but only record,
never stop, so they are not a substitute for a real precondition assertion. Run
`go test -race` to confirm both the worker's slice writes and the channel handoff
are free of data races.

## Resources

- [testing.T.Fatal — pkg.go.dev](https://pkg.go.dev/testing#T.Fatal)
- [testing — "must be called from the goroutine running the test"](https://pkg.go.dev/testing#T.FailNow)
- [golang.org/x/sync/errgroup — pkg.go.dev](https://pkg.go.dev/golang.org/x/sync/errgroup)

---

Back to [00-concepts.md](00-concepts.md) | Next: [09-dynamic-golden-subtests.md](09-dynamic-golden-subtests.md)
