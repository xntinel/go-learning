# Exercise 9: Preserve Request Order Across a Fan-Out Worker Pool

A batch API contract usually says response `i` corresponds to request `i`. But
fan-out throws ordering away: independent workers finish in nondeterministic order,
so results arrive scrambled. This exercise keeps the throughput of a worker pool and
restores the ordering by tagging each job with its index and reassembling the
results into a pre-sized slice — the standard fix when a batch endpoint must return
answers in input order.

This module is self-contained: its own module, a `batch` package, a demo, and tests.
Nothing here imports another exercise.

## What you'll build

```text
batch/                       independent module: example.com/batch
  go.mod                     go 1.26
  batch.go                   type Req, Resp; RunOrdered, RunUnordered
  cmd/demo/main.go           runnable demo: ordered vs unordered on the same input
  batch_test.go              order preserved, all processed, empty/single, unordered contrast
```

- Files: `batch.go`, `cmd/demo/main.go`, `batch_test.go`.
- Implement: `RunOrdered(inputs []Req, workers int) []Resp` that fans out but returns results in input order, and `RunUnordered` for contrast.
- Test: results are in input order under non-uniform work; all inputs processed; empty and single-element inputs; unordered variant processes all but does not guarantee order.
- Verify: `go test -count=1 -race ./...`

### Carry the index through the pipeline

The unordered pool (Exercise 1) appends results as they arrive, so `out[0]` is
whichever job finished first — useless when the caller expects `out[i]` to answer
`inputs[i]`. Sorting afterward needs a stable key, and the input order may not be
derivable from the values. The robust fix carries the position explicitly:

1. The feeder sends a `job{index, req}` for each input, so every unit of work knows
   where it belongs. The `jobs` channel is unbuffered — the feeder is paced by the
   workers.
2. Each worker emits an `indexed{index, resp}` carrying the same position back
   through the `results` channel.
3. The collector allocates `out := make([]Resp, len(inputs))` up front and writes
   each result to `out[res.index]`. Because every index is distinct and every slot
   is written exactly once, no locking is needed on `out` — the writes never collide,
   and the pre-sizing means no `append` reordering.

The result is input-ordered output with full fan-out concurrency. The
`WaitGroup`-then-`close(results)` ownership discipline is unchanged from the plain
pool; only the payloads gained an index and the collector gained a pre-sized slice.

`RunUnordered` is the contrast: same fan-out, but it appends results in arrival
order, so the output order is whatever the scheduler produced. It processes every
input correctly — it just does not preserve position.

Create `batch.go`:

```go
package batch

import (
	"sync"
	"time"
)

// Req is one request in a batch.
type Req struct {
	ID int
	N  int
}

// Resp is one response, carrying the request ID it answers.
type Resp struct {
	ID      int
	Doubled int
}

func process(r Req) Resp {
	// Non-uniform work: smaller N finishes sooner, so completion order differs
	// from input order. This is what makes ordering non-trivial.
	time.Sleep(time.Duration(r.N%5) * time.Millisecond)
	return Resp{ID: r.ID, Doubled: r.N * 2}
}

type job struct {
	index int
	req   Req
}

type indexed struct {
	index int
	resp  Resp
}

// RunOrdered fans inputs out to workers and returns responses in input order:
// out[i] answers inputs[i].
func RunOrdered(inputs []Req, workers int) []Resp {
	if workers < 1 {
		workers = 1
	}
	jobs := make(chan job)
	results := make(chan indexed)
	var wg sync.WaitGroup

	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for jb := range jobs {
				results <- indexed{index: jb.index, resp: process(jb.req)}
			}
		}()
	}

	go func() {
		for i, r := range inputs {
			jobs <- job{index: i, req: r}
		}
		close(jobs)
	}()

	go func() {
		wg.Wait()
		close(results)
	}()

	out := make([]Resp, len(inputs))
	for res := range results {
		out[res.index] = res.resp
	}
	return out
}

// RunUnordered fans inputs out and appends results in arrival order. Every input
// is processed, but the output order is nondeterministic.
func RunUnordered(inputs []Req, workers int) []Resp {
	if workers < 1 {
		workers = 1
	}
	jobs := make(chan Req)
	results := make(chan Resp)
	var wg sync.WaitGroup

	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := range jobs {
				results <- process(r)
			}
		}()
	}

	go func() {
		for _, r := range inputs {
			jobs <- r
		}
		close(jobs)
	}()

	go func() {
		wg.Wait()
		close(results)
	}()

	var out []Resp
	for res := range results {
		out = append(out, res)
	}
	return out
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/batch"
)

func main() {
	inputs := []batch.Req{
		{ID: 100, N: 4},
		{ID: 101, N: 3},
		{ID: 102, N: 2},
		{ID: 103, N: 1},
	}

	ordered := batch.RunOrdered(inputs, 4)
	var ids []int
	for _, r := range ordered {
		ids = append(ids, r.ID)
	}
	fmt.Println("ordered IDs:", ids)
	fmt.Println("out[0] answers input[0]:", ordered[0].ID == inputs[0].ID)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
ordered IDs: [100 101 102 103]
out[0] answers input[0]: true
```

### Tests

`TestResultsAreInInputOrder` uses inputs with descending `N` so the first jobs sleep
longest and finish last; if the output is still in input order, the index
reassembly works. It asserts `out[i].ID == inputs[i].ID` and the doubled value for
every position. `TestAllInputsProcessed` checks the set of answered IDs.
`TestEmptyAndSingle` covers the degenerate sizes. `TestUnorderedProcessesAll` shows
the contrast: `RunUnordered` answers every input but makes no order promise — the
test asserts set equality, not positional equality. Running with `-count=10` shakes
out any ordering flakiness.

Create `batch_test.go`:

```go
package batch

import (
	"testing"
)

func descending(n int) []Req {
	inputs := make([]Req, n)
	for i := range inputs {
		inputs[i] = Req{ID: 1000 + i, N: n - i} // first has the largest N
	}
	return inputs
}

func TestResultsAreInInputOrder(t *testing.T) {
	t.Parallel()

	inputs := descending(20)
	out := RunOrdered(inputs, 5)
	if len(out) != len(inputs) {
		t.Fatalf("got %d results, want %d", len(out), len(inputs))
	}
	for i, in := range inputs {
		if out[i].ID != in.ID {
			t.Fatalf("out[%d].ID = %d, want %d (order not preserved)", i, out[i].ID, in.ID)
		}
		if out[i].Doubled != in.N*2 {
			t.Fatalf("out[%d].Doubled = %d, want %d", i, out[i].Doubled, in.N*2)
		}
	}
}

func TestAllInputsProcessed(t *testing.T) {
	t.Parallel()

	inputs := descending(30)
	out := RunOrdered(inputs, 4)
	seen := make(map[int]bool, len(out))
	for _, r := range out {
		seen[r.ID] = true
	}
	for _, in := range inputs {
		if !seen[in.ID] {
			t.Fatalf("input %d was not processed", in.ID)
		}
	}
}

func TestEmptyAndSingle(t *testing.T) {
	t.Parallel()

	if out := RunOrdered(nil, 3); len(out) != 0 {
		t.Fatalf("empty input produced %d results, want 0", len(out))
	}
	one := []Req{{ID: 7, N: 21}}
	out := RunOrdered(one, 3)
	if len(out) != 1 || out[0].ID != 7 || out[0].Doubled != 42 {
		t.Fatalf("single input = %+v, want one {ID:7 Doubled:42}", out)
	}
}

func TestUnorderedProcessesAll(t *testing.T) {
	t.Parallel()

	inputs := descending(20)
	out := RunUnordered(inputs, 5)
	if len(out) != len(inputs) {
		t.Fatalf("got %d results, want %d", len(out), len(inputs))
	}
	seen := make(map[int]bool, len(out))
	for _, r := range out {
		seen[r.ID] = true
	}
	for _, in := range inputs {
		if !seen[in.ID] {
			t.Fatalf("input %d was not processed by RunUnordered", in.ID)
		}
	}
}
```

## Review

`RunOrdered` is correct when `out[i]` always answers `inputs[i]`, no matter which
worker finished when. The index carried on each job and result, plus a pre-sized
`out` slice written at `res.index`, is what guarantees it — each slot is written
exactly once by a distinct index, so there is no collision and no lock. `-count=10`
under `-race` is the honest check: if any run scrambles the order, the index
plumbing is wrong. `RunUnordered` exists to make the difference concrete: it
processes everything but returns arrival order, which is why appending fan-out
results and expecting input order is the bug this exercise fixes. When throughput
and a positional contract both matter, carry the index; do not rely on completion
order.

## Resources

- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — fan-out/fan-in and why arrival order is nondeterministic.
- [Effective Go: Channels](https://go.dev/doc/effective_go#channels) — the worker-pool topology this builds on.
- [`sync.WaitGroup`](https://pkg.go.dev/sync#WaitGroup) — closing the results channel exactly once after all workers finish.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [08-graceful-drain-on-shutdown.md](08-graceful-drain-on-shutdown.md) | Next: [10-connection-registry-command-channel.md](10-connection-registry-command-channel.md)
