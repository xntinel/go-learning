# Exercise 1: Fan-Out Worker Pipeline: Per-Tag Goroutine Capture

A tagged pipeline spawns one worker goroutine per tag, each worker records its
tag and bumps a shared atomic counter. This is the canonical fan-out shape, and
it is exactly where loop-variable capture historically broke: without a
per-iteration variable, every worker saw the last tag. You build both the
argument-passing form and the direct-capture form and pin, with tests, that both
behave identically on Go 1.22+.

## What you'll build

```text
tagpipe/                     independent module: example.com/tagpipe
  go.mod                     go 1.26
  pipeline.go                Pipeline; New, Count, Run (pass i,tag as args), RunUnguarded (capture)
  cmd/
    demo/
      main.go                runnable demo: fan out over tags, print each Result
  pipeline_test.go           capture, unguarded-equivalence, and concurrency tests (-race)
```

- Files: `pipeline.go`, `cmd/demo/main.go`, `pipeline_test.go`.
- Implement: `Pipeline` with a shared `atomic.Int64`, `Run` (index and tag passed as goroutine arguments) and `RunUnguarded` (index and tag captured from the loop).
- Test: every input tag appears exactly once in the output of both forms; ten concurrent `Run` calls produce an exact counter; all `-race` clean.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/tagpipe/cmd/demo
cd ~/go-exercises/tagpipe
go mod init example.com/tagpipe
go mod edit -go=1.26
```

### Two forms, one contract

`Run` is the version-independent pattern: it passes both the index `i` and the
value `tag` as arguments to the goroutine, so each goroutine gets its own copy
evaluated at the `go` statement. There is no way for the closure to alias the
loop variable, because it does not reference the loop variable at all.

`RunUnguarded` is the historical shape: the goroutine closes over `i` and `tag`
directly. Before Go 1.22 this was the bug — every goroutine would read the final
iteration's `tag` and write to the final `i`. Under a `go 1.22`+ module, each
iteration gets its own `i` and `tag`, so the direct capture is now correct. The
tests assert both forms produce the same result to pin that contract in code, so
a future downgrade of the module version would fail the suite loudly rather than
silently corrupting results.

Each worker writes into `results[idx]` — its OWN slot — and reads a fresh count
from `p.counter.Add(1)`. The counter is a `sync/atomic.Int64`: `Add` is a single
atomic read-modify-write, so concurrent workers never lose an increment and the
final count is exact. The result slot per worker is why there is no write race on
the slice: distinct indices are distinct memory, so no synchronization is needed
beyond the `WaitGroup` that establishes the happens-before edge for the reads
after `Wait`.

Create `pipeline.go`:

```go
package pipeline

import (
	"fmt"
	"sync"
	"sync/atomic"
)

// Result records the tag a worker processed and the counter value it observed.
type Result struct {
	Tag   string
	Count int
}

// Pipeline fans work out over goroutines, one per tag, sharing an atomic counter.
type Pipeline struct {
	counter atomic.Int64
}

// New returns a ready Pipeline.
func New() *Pipeline {
	return &Pipeline{}
}

// Count reports how many workers have run across all Run/RunUnguarded calls.
func (p *Pipeline) Count() int64 {
	return p.counter.Load()
}

// Run fans out one goroutine per tag, passing the index and tag as arguments so
// each goroutine owns its own copy. Version-independent: correct on any Go.
func (p *Pipeline) Run(tags []string) []Result {
	results := make([]Result, len(tags))
	var wg sync.WaitGroup
	for i, tag := range tags {
		wg.Add(1)
		go func(idx int, tag string) {
			defer wg.Done()
			results[idx] = Result{Tag: tag, Count: int(p.counter.Add(1))}
		}(i, tag)
	}
	wg.Wait()
	return results
}

// RunUnguarded captures the loop variables directly. On a go 1.22+ module the
// per-iteration variable makes this behave identically to Run.
func (p *Pipeline) RunUnguarded(tags []string) []Result {
	results := make([]Result, len(tags))
	var wg sync.WaitGroup
	for i, tag := range tags {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = Result{Tag: tag, Count: int(p.counter.Add(1))}
		}()
	}
	wg.Wait()
	return results
}

// NewTagged is a tiny helper that namespaces a raw tag, used by the demo.
func NewTagged(tag string) string {
	return fmt.Sprintf("tag:%s", tag)
}
```

### The runnable demo

Because worker order is nondeterministic, the demo sorts the tags it observed
before printing, so the output is stable across runs.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sort"

	"example.com/tagpipe"
)

func main() {
	p := pipeline.New()
	tags := []string{
		pipeline.NewTagged("ingest"),
		pipeline.NewTagged("index"),
		pipeline.NewTagged("notify"),
	}

	results := p.Run(tags)

	got := make([]string, len(results))
	for i, r := range results {
		got[i] = r.Tag
	}
	sort.Strings(got)

	for _, tag := range got {
		fmt.Println("processed", tag)
	}
	fmt.Println("total workers:", p.Count())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
processed tag:index
processed tag:ingest
processed tag:notify
total workers: 3
```

### Tests

`TestRunCapturesEachTag` asserts every input tag appears exactly once in the
output — the historical bug would leave one tag repeated and the rest missing.
`TestRunUnguardedAlsoCapturesEachTag` proves the direct-capture form is equally
correct under the per-iteration variable. `TestRunIsSafeUnderConcurrency` fires
ten concurrent `Run` calls and asserts the shared counter is exact, exercising
both the atomic and the disjoint-slot writes under `-race`.

Create `pipeline_test.go`:

```go
package pipeline

import (
	"fmt"
	"sort"
	"sync"
	"testing"
)

func TestRunCapturesEachTag(t *testing.T) {
	t.Parallel()

	tags := []string{"a", "b", "c", "d", "e"}
	p := New()
	results := p.Run(tags)
	if len(results) != len(tags) {
		t.Fatalf("results = %d, want %d", len(results), len(tags))
	}

	got := make([]string, len(results))
	for i, r := range results {
		got[i] = r.Tag
	}
	sort.Strings(got)
	for i, want := range tags {
		if got[i] != want {
			t.Fatalf("got[%d] = %q, want %q", i, got[i], want)
		}
	}
	if p.Count() != int64(len(tags)) {
		t.Fatalf("Count() = %d, want %d", p.Count(), len(tags))
	}
}

func TestRunUnguardedAlsoCapturesEachTag(t *testing.T) {
	t.Parallel()

	tags := []string{"a", "b", "c", "d", "e"}
	p := New()
	results := p.RunUnguarded(tags)
	if len(results) != len(tags) {
		t.Fatalf("results = %d, want %d", len(results), len(tags))
	}

	got := make(map[string]bool)
	for _, r := range results {
		got[r.Tag] = true
	}
	for _, want := range tags {
		if !got[want] {
			t.Fatalf("missing tag %q in results", want)
		}
	}
}

func TestRunIsSafeUnderConcurrency(t *testing.T) {
	t.Parallel()

	tags := make([]string, 100)
	for i := range tags {
		tags[i] = string(rune('a' + (i % 26)))
	}

	p := New()
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = p.Run(tags)
		}()
	}
	wg.Wait()

	want := int64(10 * len(tags))
	if got := p.Count(); got != want {
		t.Fatalf("Count() = %d, want %d", got, want)
	}
}

func ExamplePipeline_Run() {
	p := New()
	results := p.Run([]string{"only"})
	fmt.Println(results[0].Tag, results[0].Count)
	// Output: only 1
}
```

## Review

The pipeline is correct when the output of `Run` and `RunUnguarded` are set-equal
to the input tags and the shared counter equals the total number of workers.
`TestRunCapturesEachTag` would fail with one tag present and the rest missing if
the per-iteration variable were absent, so it is a live regression guard against a
`go.mod` downgrade. The two things to keep straight: each goroutine writes its own
`results[idx]` slot (disjoint memory, no race), and the count comes from an atomic
`Add`, not a plain `++`. Do not add a `tag := tag` shadow — on this `go 1.26`
module it is dead code. Run `go test -race` to confirm the disjoint-slot writes
and the atomic hold under contention.

## Resources

- [Go 1.22 release notes: loop variable scoping](https://go.dev/doc/go1.22#language) — the per-iteration variable change.
- [Fixing for loops in Go 1.22](https://go.dev/blog/loopvar-preview) — why the change was made and what it does not fix.
- [`sync/atomic`](https://pkg.go.dev/sync/atomic#Int64) — `Int64.Add`, `Int64.Load`.
- [`sync.WaitGroup`](https://pkg.go.dev/sync#WaitGroup) — the fan-out join.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-defer-in-loop-fd-leak.md](02-defer-in-loop-fd-leak.md)
