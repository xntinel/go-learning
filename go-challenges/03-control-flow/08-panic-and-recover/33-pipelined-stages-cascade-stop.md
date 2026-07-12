# Exercise 33: Pipelined Stages: Cascade-Stop on Panic With No Goroutine Leaks

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A three-stage processing pipeline — decode, validate, enrich, connected by
channels, each stage its own goroutine — is the standard shape for
streaming ETL in Go. Unlike a fan-out worker pool, where one bad task
should be isolated and the rest of the batch should keep going, a pipeline
failure in a middle stage usually means the whole pipeline's output for
this run is no longer trustworthy: stage 2 panicking partway through a
batch is a signal to stop the entire pipeline cleanly, not to keep pumping
items through stage 3 with stage 2 no longer validating anything. The hard
part is not catching the panic - it is making every other stage's goroutine
notice the shutdown and actually exit, instead of blocking forever trying
to send to, or receive from, a channel whose other end has already stopped
listening. This module builds `Run`, a fail-fast pipeline with a shared
cancellation signal that guarantees every stage's goroutine exits after any
one stage fails. It is fully self-contained: its own module, demo, and
tests.

## What you'll build

```text
pipeline/                   independent module: example.com/pipeline
  go.mod                     go 1.24
  pipeline.go                  Item, StageFunc, Result, Run, runGuarded
  cmd/
    demo/
      main.go                runnable demo: a clean 3-item run, then a run where stage2 panics on the first item
  pipeline_test.go              all succeed in order, cascade stop on panic, no goroutine leak, empty
```

Files: `pipeline.go`, `cmd/demo/main.go`, `pipeline_test.go`.
Implement: `Run(items []Item, stage1, stage2, stage3 StageFunc) Result` that wires source -> stage1 -> stage2 -> stage3 -> sink across unbuffered channels, each stage in its own goroutine; any stage's item panicking closes a shared `done` channel exactly once, and every channel send and receive in every stage races against `done` so the whole pipeline unwinds without a leaked goroutine.
Test: three items flowing cleanly through in order; a middle-stage panic on the very first item, asserting zero items reach the sink and exactly one error is recorded; twenty repeated cascade-stop runs followed by a bounded `runtime.NumGoroutine()` check for leaks; an empty item list.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/08-panic-and-recover/33-pipelined-stages-cascade-stop/cmd/demo
cd go-solutions/03-control-flow/08-panic-and-recover/33-pipelined-stages-cascade-stop
go mod edit -go=1.24
```

### Why every send and receive races against a shared done channel, and why the demo isolates the failure to the very first item

Each stage's goroutine runs a loop: receive one item, transform it, send it
downstream, repeat - and both the receive and the send are written as
`select` statements racing against a shared `done` channel, closed exactly
once (via `sync.Once`) by whichever stage's item panics first. This
symmetry is what makes the shutdown cascade in both directions from the
point of failure. Downstream of the failing stage, its output channel is
closed by its own `defer close(out)` the moment it returns, so the next
stage's blocked receive simply sees a closed channel and returns too,
cascading further downstream automatically - closed channels do not need
`done` at all. Upstream of the failing stage, there is no closed channel to
observe: an earlier stage might be blocked trying to *send* an item to the
now-dead stage's input channel, and nothing will ever receive it again. That
send would block forever without the `done` case in its `select` - the
`done` channel is what lets an upstream stage notice "downstream stopped
listening" and exit, in turn closing its own input channel's consumption
and unblocking whatever produced *it*.

Getting an exact, reproducible expected output out of a genuinely
concurrent cascade is harder than it looks, and it is the reason the demo's
failing run is built so the poisoning item is the very *first* one stage 2
ever receives. Because a single-goroutine stage processes and forwards
items strictly in the order it receives them, stage 2 cannot have already
forwarded anything to stage 3 by the time it panics on its first item -
there is no race about "how many items happened to sneak through before the
cascade caught up," because zero items ever had the chance to. That is a
property of *this specific demo's construction*, not a general guarantee
this pipeline makes about how many in-flight items survive a mid-batch
failure - in a longer run, a few items already past the failing stage when
it panics may or may not complete, and that is an accepted, inherent
characteristic of a fail-fast cascading shutdown, not a bug.

Create `pipeline.go`:

```go
package pipeline

import (
	"fmt"
	"sync"
)

// Item flows through the pipeline.
type Item struct {
	Value int
}

// StageFunc transforms one item. It may return an error, or - the failure
// this package defends against - panic.
type StageFunc func(Item) (Item, error)

// Result is the outcome of one pipeline run.
type Result struct {
	Processed []Item
	Errs      []error
}

// Run wires three stages, each in its own goroutine, connected by
// unbuffered channels: a source -> stage1 -> stage2 -> stage3 -> a sink.
// Every stage reads from its input channel and, for every item, isolates
// the stage function's panic with its own recover. When any stage's
// function fails - error or panic - that stage records the failure, closes
// a shared cancellation channel exactly once, and stops.
//
// Every stage's send to its output channel races against that same
// cancellation channel, so a stage blocked trying to hand an item to a
// downstream stage that has already stopped reading unblocks immediately
// instead of leaking forever; every stage's read from its input also races
// against cancellation, so an upstream stage that is still producing stops
// being read, and - via its own blocked send racing the same channel -
// stops too. The result is a clean, total shutdown of the whole pipeline
// triggered by a single failure, with no stage goroutine still running
// after Run returns.
func Run(items []Item, stage1, stage2, stage3 StageFunc) Result {
	var (
		mu     sync.Mutex
		once   sync.Once
		result Result
	)
	done := make(chan struct{})
	cancel := func() { once.Do(func() { close(done) }) }

	src := make(chan Item)
	c1 := make(chan Item)
	c2 := make(chan Item)
	c3 := make(chan Item)

	var wg sync.WaitGroup
	wg.Add(5)

	go func() {
		defer wg.Done()
		defer close(src)
		for _, it := range items {
			select {
			case src <- it:
			case <-done:
				return
			}
		}
	}()

	runStage := func(name string, in, out chan Item, fn StageFunc) {
		defer wg.Done()
		defer close(out)
		for {
			var it Item
			var ok bool
			select {
			case it, ok = <-in:
				if !ok {
					return
				}
			case <-done:
				return
			}

			transformed, err := runGuarded(name, it, fn)
			if err != nil {
				mu.Lock()
				result.Errs = append(result.Errs, err)
				mu.Unlock()
				cancel()
				return
			}

			select {
			case out <- transformed:
			case <-done:
				return
			}
		}
	}

	go runStage("stage1", src, c1, stage1)
	go runStage("stage2", c1, c2, stage2)
	go runStage("stage3", c2, c3, stage3)

	go func() {
		defer wg.Done()
		for it := range c3 {
			mu.Lock()
			result.Processed = append(result.Processed, it)
			mu.Unlock()
		}
	}()

	wg.Wait()
	return result
}

// runGuarded is the recover boundary: exactly one stage's untrusted fn call
// on exactly one item.
func runGuarded(stage string, it Item, fn StageFunc) (out Item, err error) {
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(error); ok {
				err = fmt.Errorf("%s panicked on item %d: %w", stage, it.Value, e)
				return
			}
			err = fmt.Errorf("%s panicked on item %d: %v", stage, it.Value, r)
		}
	}()
	return fn(it)
}
```

### The runnable demo

The first run is a clean three-item pipeline: double, pass through, add a
hundred. The second run reuses the same shape but replaces stage 2 with a
function that panics on an out-of-range index read - and because it is the
very first item stage 2 ever sees, zero items reach the sink.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/pipeline"
)

func main() {
	double := func(it pipeline.Item) (pipeline.Item, error) { return pipeline.Item{Value: it.Value * 2}, nil }
	identity := func(it pipeline.Item) (pipeline.Item, error) { return it, nil }
	addHundred := func(it pipeline.Item) (pipeline.Item, error) { return pipeline.Item{Value: it.Value + 100}, nil }

	fmt.Println("-- normal run, no panics --")
	normal := pipeline.Run(
		[]pipeline.Item{{Value: 1}, {Value: 2}, {Value: 3}},
		double, identity, addHundred,
	)
	fmt.Printf("processed: %d, errs: %d\n", len(normal.Processed), len(normal.Errs))
	for _, it := range normal.Processed {
		fmt.Println("  value:", it.Value)
	}

	fmt.Println("-- cascade-stop run: stage2 panics on the first item --")
	poison := func(it pipeline.Item) (pipeline.Item, error) {
		var validated []int
		return pipeline.Item{Value: validated[it.Value]}, nil // index out of range
	}
	failed := pipeline.Run(
		[]pipeline.Item{{Value: 1}, {Value: 2}, {Value: 3}},
		double, poison, addHundred,
	)
	fmt.Printf("processed: %d, errs: %d\n", len(failed.Processed), len(failed.Errs))
	for _, err := range failed.Errs {
		fmt.Println("  error:", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
-- normal run, no panics --
processed: 3, errs: 0
  value: 102
  value: 104
  value: 106
-- cascade-stop run: stage2 panics on the first item --
processed: 0, errs: 1
  error: stage2 panicked on item 2: runtime error: index out of range [2] with length 0
```

### Tests

`TestRunCascadesStopWhenMiddleStagePanics` mirrors the demo's deterministic
construction (the panic happens on the first item stage 2 ever receives)
so the assertion that zero items reach the sink is not a race.
`TestRunDoesNotLeakGoroutines` runs the cascade-stop scenario twenty times
in a row and then polls `runtime.NumGoroutine()` against a small tolerance
above the pre-test baseline, so a genuine leak fails the test instead of
silently accumulating background goroutines run after run.

Create `pipeline_test.go`:

```go
package pipeline

import (
	"runtime"
	"strings"
	"testing"
	"time"
)

func identity(it Item) (Item, error) { return it, nil }

func TestRunAllItemsSucceedInOrder(t *testing.T) {
	double := func(it Item) (Item, error) { return Item{Value: it.Value * 2}, nil }
	result := Run([]Item{{Value: 1}, {Value: 2}, {Value: 3}}, double, identity, identity)

	if len(result.Errs) != 0 {
		t.Fatalf("Errs = %v, want none", result.Errs)
	}
	want := []int{2, 4, 6}
	if len(result.Processed) != len(want) {
		t.Fatalf("Processed = %+v, want %d items", result.Processed, len(want))
	}
	for i, w := range want {
		if result.Processed[i].Value != w {
			t.Fatalf("Processed[%d].Value = %d, want %d", i, result.Processed[i].Value, w)
		}
	}
}

func TestRunCascadesStopWhenMiddleStagePanics(t *testing.T) {
	poison := func(it Item) (Item, error) {
		var v []int
		return Item{Value: v[it.Value]}, nil // index out of range: panics on the first item stage2 sees
	}
	result := Run([]Item{{Value: 1}, {Value: 2}, {Value: 3}}, identity, poison, identity)

	if len(result.Processed) != 0 {
		t.Fatalf("Processed = %+v, want empty: the poisoned first item never reaches stage3", result.Processed)
	}
	if len(result.Errs) != 1 {
		t.Fatalf("Errs = %v, want exactly 1", result.Errs)
	}
	if !strings.Contains(result.Errs[0].Error(), "stage2") || !strings.Contains(result.Errs[0].Error(), "index out of range") {
		t.Fatalf("Errs[0] = %v, want it to name stage2 and the index-out-of-range panic", result.Errs[0])
	}
}

func TestRunDoesNotLeakGoroutines(t *testing.T) {
	before := runtime.NumGoroutine()

	for i := 0; i < 20; i++ {
		poison := func(it Item) (Item, error) {
			var v []int
			return Item{Value: v[it.Value]}, nil
		}
		Run([]Item{{Value: 1}, {Value: 2}}, identity, poison, identity)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before+2 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("goroutine count = %d, want close to baseline %d (leak suspected)", runtime.NumGoroutine(), before)
}

func TestRunEmptyItems(t *testing.T) {
	result := Run(nil, identity, identity, identity)
	if len(result.Processed) != 0 || len(result.Errs) != 0 {
		t.Fatalf("result = %+v, want empty", result)
	}
}
```

## Review

`Run` is correct when every stage's blocked send or receive has a way to
notice the pipeline has failed and unblock, in both directions from the
point of failure - not just downstream, where a closed channel makes it
easy, but upstream too, where nothing but the shared `done` channel can
ever wake a stage stuck trying to hand an item to a stage that already
quit. The detail most likely to be missed in a first draft is exactly that
upstream case: it is easy to notice that closing `out` cascades downstream
automatically, and easy to forget that a stage's *send* also needs its own
`select` against `done`, without which an upstream goroutine blocks
forever the moment its immediate downstream stage exits.

## Resources

- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — the `done`-channel cancellation pattern this pipeline is built directly from.
- [Go Blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover) — the per-stage, per-item recover boundary in `runGuarded`.
- [sync.Once](https://pkg.go.dev/sync#Once) — guaranteeing the shared cancellation channel is closed exactly once no matter which stage fails first.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [32-semaphore-token-bucket-release.md](32-semaphore-token-bucket-release.md) | Next: [34-graceful-shutdown-coordinator.md](34-graceful-shutdown-coordinator.md)
