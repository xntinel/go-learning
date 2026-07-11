# Exercise 4: Fan-Out to a Downstream Service

A production fan-out launches one goroutine per work item to call a downstream dependency and then aggregates the responses. This exercise builds that pattern against an injected service, writes each goroutine's result into its own slot of a pre-sized slice, and uses the race detector to prove every item is handled with its own value and that the aggregation is correct.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
fanout.go            Service, Response, FanOut, Aggregate, ErrEmptyBatch
cmd/
  demo/
    main.go          a fake squaring service, fan out over five ids, print rollup
fanout_test.go       race-clean fan-out over a recording service + error + empty cases
example_test.go      ExampleFanOut with a verified // Output block
```

- Files: `fanout.go`, `cmd/demo/main.go`, `fanout_test.go`, `example_test.go`.
- Implement: `FanOut(Service, []int) ([]Response, error)` that launches one goroutine per id, each capturing its own iteration index and id, calls `Service.Call`, writes into its own results slot, and joins per-item errors; plus `Aggregate([]Response) int`, the `Service` interface, the `Response` type, and the sentinel `ErrEmptyBatch`.
- Test: assert results stay aligned with the input order, that a recording service sees each id exactly once, that a failing service surfaces a wrapped error, and that an empty batch returns `ErrEmptyBatch` — all under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p loop-fanout-service/cmd/demo && cd loop-fanout-service
go mod init example.com/loop-fanout-service
```

### Why the result slot, not a shared accumulator, is the architecture

The naive way to collect fan-out results is to append to a shared slice or write to a shared map from inside each goroutine. That is a data race on the slice header or the map, and it is unrelated to the loop variable — it would race even with a perfect per-iteration capture. The disciplined pattern is to pre-size a results slice to `len(ids)` and have each goroutine write only to `results[i]`, where `i` is *its* iteration index. Distinct indices into an existing backing array are distinct memory locations, so no two goroutines ever touch the same word; there is no shared mutable accumulator, hence no race and no mutex. The slice header itself is only read concurrently (never grown), which is safe. This is the canonical Go answer to "fan out and collect in order," and it depends on Go 1.22 to be correct: each goroutine must capture its own `i` and `id`, or every goroutine would index with the loop's final `i` and clobber the same last slot.

That dependency is the whole point of placing this exercise in the loop-variable chapter. Before 1.22, `for i, id := range ids { go func() { results[i] = svc.Call(id) }() }` captured a single shared `i` and a single shared `id`; the goroutines would race to read both, and the writes would pile into whichever slot `i` happened to hold when each goroutine ran — undefined, not merely "the last slot." The fix used to be `i, id := i, id` on the first line of the loop body. Under `go 1.26` the per-iteration variables are created for you, so the copy-free form is correct, and the `-race` test in this module is the proof: if the capture were wrong, the recording service would still be called N times, but the results would not line up with the inputs and the detector would likely flag the concurrent index writes.

`FanOut` makes the per-iteration capture observable by keeping results positional: `results[i]` must equal the response for `ids[i]`. Because the demo's service squares its input and squares of distinct ids are distinct, an aligned `[1 4 9 16 25]` for `[1 2 3 4 5]` is direct evidence each goroutine carried its own id into the call. Errors are collected the same way — one `errs[i]` slot per item — and folded with `errors.Join`, which returns `nil` when every slot is `nil` and otherwise a single error wrapping all the failures, so a partial failure is reported without losing which items failed.

Create `fanout.go`:

```go
package loopvar

import (
	"errors"
	"fmt"
	"sync"
)

// ErrEmptyBatch is returned when there is nothing to fan out over.
var ErrEmptyBatch = errors.New("batch must contain at least one id")

// Response is what the downstream service returns for one request.
type Response struct {
	ID    int
	Value int
}

// Service is the downstream dependency the fan-out calls once per item. It is
// an interface so tests can inject a recording or failing fake in place of a
// real network client.
type Service interface {
	Call(id int) (Response, error)
}

// FanOut launches one goroutine per id, each capturing its own iteration index
// and id, calls svc.Call, and writes the response into its own slot of the
// results slice. Distinct indices into a pre-sized slice are distinct memory
// locations, so the fan-out needs no mutex and is race-free under go test
// -race; positional results stay aligned with the input order. Per-item errors
// are collected into matching slots and folded with errors.Join.
func FanOut(svc Service, ids []int) ([]Response, error) {
	if len(ids) == 0 {
		return nil, ErrEmptyBatch
	}

	results := make([]Response, len(ids))
	errs := make([]error, len(ids))
	var wg sync.WaitGroup

	for i, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := svc.Call(id)
			if err != nil {
				errs[i] = fmt.Errorf("call id %d: %w", id, err)
				return
			}
			results[i] = resp
		}()
	}
	wg.Wait()

	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return results, nil
}

// Aggregate rolls the responses up into a single figure by summing values.
func Aggregate(responses []Response) int {
	total := 0
	for _, r := range responses {
		total += r.Value
	}
	return total
}
```

### The runnable demo

The demo injects a fake service that squares each id, fans out over five integers, and prints both the positional values and the aggregate. Because the results are positional rather than collected in completion order, the output is deterministic despite the goroutines finishing in an unpredictable order, so it is safe to pin in an Expected-output block.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/loop-fanout-service"
)

// squaringService is a stand-in for a real downstream dependency.
type squaringService struct{}

func (squaringService) Call(id int) (loopvar.Response, error) {
	return loopvar.Response{ID: id, Value: id * id}, nil
}

func main() {
	responses, err := loopvar.FanOut(squaringService{}, []int{1, 2, 3, 4, 5})
	if err != nil {
		fmt.Println("fan out error:", err)
		return
	}

	values := make([]int, len(responses))
	for i, r := range responses {
		values[i] = r.Value
	}
	fmt.Println("values:", values)
	fmt.Println("total:", loopvar.Aggregate(responses))
}
```

Run it:

```bash
go run -race ./cmd/demo
```

Expected output:

```
values: [1 4 9 16 25]
total: 55
```

The `-race` flag is the point: each goroutine writes only `results[i]`, so the program is race-clean, and the positional values prove each call carried its own id.

### Tests

The tests run under `-race`, which proves the per-slot writes never collide. `TestFanOutCallsEachIdOnce` injects a recording service whose mutex-guarded counter shows each id was called exactly once and that the positional results match each id's square. `TestFanOutSurfacesError` injects a service that fails one id and checks the joined error names it. `TestFanOutEmpty` checks the sentinel. The parallel subtests in `TestFanOutAlignment` capture `tc` directly, with no `tc := tc` copy, relying on the per-iteration scope this chapter is about.

Create `fanout_test.go`:

```go
package loopvar

import (
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// recordingService is a concurrency-safe fake that records how many times each
// id was called, so a test can prove the fan-out handled every item once.
type recordingService struct {
	mu   sync.Mutex
	seen map[int]int
}

func (s *recordingService) Call(id int) (Response, error) {
	s.mu.Lock()
	s.seen[id]++
	s.mu.Unlock()
	return Response{ID: id, Value: id * id}, nil
}

func TestFanOutCallsEachIdOnce(t *testing.T) {
	t.Parallel()

	svc := &recordingService{seen: make(map[int]int)}
	ids := []int{2, 3, 5, 7}

	got, err := FanOut(svc, ids)
	if err != nil {
		t.Fatal(err)
	}

	for i, id := range ids {
		if got[i].ID != id || got[i].Value != id*id {
			t.Fatalf("result %d = %+v, want id %d value %d", i, got[i], id, id*id)
		}
		if svc.seen[id] != 1 {
			t.Fatalf("id %d called %d times, want 1", id, svc.seen[id])
		}
	}
	if len(svc.seen) != len(ids) {
		t.Fatalf("distinct ids seen = %d, want %d", len(svc.seen), len(ids))
	}
}

func TestFanOutAlignment(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		ids  []int
		want []int
	}{
		{name: "single", ids: []int{4}, want: []int{16}},
		{name: "ordered", ids: []int{1, 2, 3}, want: []int{1, 4, 9}},
		{name: "unordered", ids: []int{3, 1, 2}, want: []int{9, 1, 4}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			svc := &recordingService{seen: make(map[int]int)}
			got, err := FanOut(svc, tc.ids)
			if err != nil {
				t.Fatal(err)
			}
			values := make([]int, len(got))
			for i, r := range got {
				values[i] = r.Value
			}
			if !reflect.DeepEqual(values, tc.want) {
				t.Fatalf("values = %v, want %v", values, tc.want)
			}
		})
	}
}

// failingService fails the single configured id and serves the rest.
type failingService struct {
	badID int
}

func (s failingService) Call(id int) (Response, error) {
	if id == s.badID {
		return Response{}, errors.New("downstream unavailable")
	}
	return Response{ID: id, Value: id}, nil
}

func TestFanOutSurfacesError(t *testing.T) {
	t.Parallel()

	_, err := FanOut(failingService{badID: 3}, []int{1, 2, 3, 4})
	if err == nil {
		t.Fatal("FanOut error = nil, want an error for id 3")
	}
	if !strings.Contains(err.Error(), "call id 3") {
		t.Fatalf("error %q does not mention the failing id", err.Error())
	}
}

func TestFanOutEmpty(t *testing.T) {
	t.Parallel()

	if _, err := FanOut(failingService{}, nil); !errors.Is(err, ErrEmptyBatch) {
		t.Fatalf("FanOut(nil) error = %v, want ErrEmptyBatch", err)
	}
}

func TestAggregateSumsValues(t *testing.T) {
	t.Parallel()

	got := Aggregate([]Response{{Value: 1}, {Value: 4}, {Value: 9}})
	if got != 14 {
		t.Fatalf("Aggregate = %d, want 14", got)
	}
}
```

Create `example_test.go`:

```go
package loopvar

import "fmt"

type doublingService struct{}

func (doublingService) Call(id int) (Response, error) {
	return Response{ID: id, Value: id * 2}, nil
}

func ExampleFanOut() {
	responses, _ := FanOut(doublingService{}, []int{1, 2, 3, 4})
	fmt.Println(Aggregate(responses))
	// Output: 20
}
```

## Review

The fan-out is correct when, run under `go test -race`, it reports no data race, the positional results stay aligned with the inputs, and the recording service shows each id called exactly once. The race-freedom rests entirely on two facts: each goroutine writes only its own `results[i]`, and Go 1.22 gives each iteration its own `i` and `id` so those indices are distinct. Remove either property — share one accumulator, or compile the copy-free capture in a `go 1.21` module — and the detector flags it. The positional-collection design is what makes the result deterministic without sorting; it also generalizes to heterogeneous results where sorting would not help.

The mistake to avoid is appending to a shared slice or writing a shared map from the goroutines "because it is faster than pre-sizing." That reintroduces a race that has nothing to do with the loop variable and everything to do with concurrent mutation of one structure; pre-size and index instead. The second mistake is reasoning about the old loop bug as "every result lands in the last slot." A data race has no defined outcome, so the only honest description of the pre-1.22 behavior is "undefined." The `i, id := i, id` copy is unnecessary in a `go 1.26` module, and the parallel-subtest test exists to show the copy-free form is now safe.

## Resources

- [Fixing For Loops in Go 1.22](https://go.dev/blog/loopvar-preview) — why per-iteration scope makes goroutine fan-outs over a loop correct.
- [errors.Join](https://pkg.go.dev/errors#Join) — how joined errors aggregate multiple per-item failures into one value, skipping nils.
- [Data Race Detector](https://go.dev/doc/articles/race_detector) — what `-race` instruments and what a reported race means.
- [Go FAQ: What happens with closures running as goroutines?](https://go.dev/doc/faq#closures_and_goroutines) — the official note on capturing loop variables in goroutines under the 1.22 behavior.

---

Back to [03-pointer-capture-strategies.md](03-pointer-capture-strategies.md) | Next: [05-parallel-batch-fetch-with-bounded-concurrency.md](05-parallel-batch-fetch-with-bounded-concurrency.md)
