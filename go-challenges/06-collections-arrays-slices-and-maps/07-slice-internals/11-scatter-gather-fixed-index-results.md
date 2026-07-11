# Exercise 11: Scatter-Gather Results Into Disjoint Slice Indices

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

An API gateway implementing GraphQL federation, or a batch endpoint that
fans one client request out into several parallel upstream RPCs, has one
hard requirement that is easy to lose under concurrency: whatever order the
client asked for its sub-requests in, the response must come back in that
same order, no matter which upstream happens to answer first. The obvious
implementation launches one goroutine per upstream call and collects
whatever comes back into a shared slice with a mutex around `append`. That
compiles, it passes a single-request smoke test, and it is subtly wrong: the
order elements land in the result depends on which goroutine wins the race
for the lock, which is completion order, not request order. Under load, with
upstreams whose latency varies request to request, the client sees its
sub-responses shuffled on some fraction of requests -- a bug that shows up
in production exactly because latency varies there and never varies in a
laptop-run test.

The fix does not need a lock at all. `make([]T, n)` before any goroutine
starts creates `n` addressable slots up front; each goroutine is given its
own index and writes only to `results[i]`. Two different goroutines writing
to two different indices of the same slice never touch the same memory, so
there is nothing to race on -- the memory safety that a mutex would provide
comes for free from the indices being disjoint. This is the lesson's central
fact about slices in a new light: a slice header is shared, not copied, so
every goroutine's `results[i] = ...` writes through the same three-word
header into the one backing array, and that sharing is exactly what makes
the lock-free write safe, not something a lock protects you from.

This module builds `scatter`, a small generic `Gather` function you take
into any fan-out gateway: it preallocates the result slots, writes each
goroutine's answer into its own index, and returns results in request order
regardless of arrival order. The mutex-guarded append never appears in the
package; it is isolated in the test file as the contrast the tests pin
against.

The trap here is specifically that the mutex-guarded version *looks* like
the careful, correct one -- it is the version a reviewer reaches for when
told "make this concurrent-safe," and the Go race detector has nothing to
say against it, because it genuinely never has two goroutines touch the
same memory without synchronization. `-race` catches missing
synchronization; it does not catch a synchronization strategy that
accidentally scrambles logical ordering while staying perfectly memory-safe.
That is exactly why this bug survives into production: every tool built to
catch concurrency bugs passes it, and the only thing that catches it is a
test that forces a specific, adversarial completion order and checks the
result against request order directly.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
scatter/                 module example.com/scatter
  go.mod                 go 1.24
  scatter.go             Gather[T any](n int, work func(i int) T) []T
  scatter_test.go        table, struct-typed work, call-count guard, the
                         reverse-completion order pin, mutex-append contrast,
                         ExampleGather
```

- Files: `scatter.go`, `scatter_test.go`.
- Implement: `Gather[T any](n int, work func(i int) T) []T`, running `work(i)` concurrently for every `i` in `[0,n)`, one goroutine per index, writing directly into a preallocated `results[i]`; `n <= 0` returns an empty, non-nil slice without invoking `work`.
- Test: the basic table (zero, negative, one, several); `Gather` with a struct-typed `T`; every index invoked exactly once; a deterministic reverse-completion schedule built from chained channels (no `time.Sleep`) proving `Gather` still returns request order; the same schedule through an unexported mutex-guarded `gatherNaiveAppend`, proving it returns completion order instead; and `ExampleGather` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

`Gather` takes no context and no cancellation: it is deliberately the
smallest useful primitive, one call per fan-out, leaving timeout and
cancellation policy to the caller's `work` closures, which are free to check
a `context.Context` captured from their enclosing scope. That keeps
`Gather` itself free of any policy decision about what "the request failed"
should mean for a particular gateway.

Set up the module:

```bash
mkdir -p ~/go-exercises/scatter
cd ~/go-exercises/scatter
go mod init example.com/scatter
go mod edit -go=1.24
```

### Disjoint writes need no lock; a shared append needs one and still loses order

`append` mutates a shared piece of state: the slice's length, and
potentially the backing array it points to when it grows. Two goroutines
calling `append` on the same slice variable concurrently is a data race by
definition -- both read and write the same length field -- so the naive
fan-out reaches for a mutex:

```go
var mu sync.Mutex
results := make([]T, 0, n)
go func() {
    v := work(i)
    mu.Lock()
    results = append(results, v) // serializes on completion order
    mu.Unlock()
}()
```

This is memory-safe. It is also useless for ordering: whichever goroutine
acquires the lock first is whichever one finished first, and `append` places
its value at whatever index that happens to be. The client's sub-request
order and the gateway's response order have nothing to do with each other
anymore.

`results[i] = work(i)` sidesteps the whole problem, because indexed
assignment to *different* indices of the same slice is not a data race at
all -- there is no shared field being read and written, only disjoint memory
being written once each. Preallocating with `make([]T, n)` before any
goroutine starts is what makes every index already addressable: there is no
`append`, no growth, no shared length counter for two goroutines to
contend on. Contrast that with launching the goroutines against a slice
built with `append` as each one finishes: `append` reads and potentially
writes the slice's own length field, so even a single unguarded `append`
call from two goroutines is a race regardless of whether their target
indices would have been disjoint -- the race is on the bookkeeping `append`
does before it ever gets to writing an element, not on the elements
themselves.

Create `scatter.go`:

```go
// Package scatter fans a unit of work out across goroutines, one per index,
// and gathers the results back into a single slice in original request
// order -- regardless of which goroutine finishes first -- by writing each
// result directly into its own reserved slot instead of collecting them
// through a mutex-guarded append.
package scatter

import "sync"

// Gather runs work(i) concurrently for every i in [0,n), one goroutine per
// index, and returns a slice of length n where result[i] holds the value
// work(i) returned.
//
// Gather blocks until every call to work has returned. n <= 0 returns an
// empty, non-nil slice without invoking work.
//
// The result slice is preallocated with make([]T, n) before any goroutine
// starts, so every index already has a reserved, addressable slot. Each
// goroutine writes to exactly one index, results[i]; concurrent writes to
// different indices of the same slice never touch the same memory, so no
// goroutine needs a lock to record its result -- disjoint indices are what
// make that safe, not the absence of shared state. The slice header itself
// is shared, not copied: every goroutine's results[i] = ... writes through
// the same three-word header into the one backing array.
//
// If work panics, Gather does not recover the panic: it propagates like any
// unrecovered panic running in a goroutine, which crashes the process. A
// work function whose failures must not crash the caller should recover
// internally and encode the failure in T.
func Gather[T any](n int, work func(i int) T) []T {
	if n <= 0 {
		return []T{}
	}

	results := make([]T, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			results[i] = work(i)
		}(i)
	}
	wg.Wait()
	return results
}
```

### Using it

Call `Gather` with the number of sub-requests and a function that performs
one of them given its index -- typically closing over a slice of upstream
clients or request payloads and returning a parsed response. `Gather`
blocks until every call to `work` has returned, so the caller gets a plain
synchronous-looking function that happens to run its work concurrently
underneath. A GraphQL federation gateway, for instance, would call
`Gather(len(subQueries), func(i int) SubResponse { return
resolvers[i].Resolve(ctx, subQueries[i]) })` and get back a slice whose
order matches the client's original field order exactly, with no
bookkeeping of its own required to restore that order afterward -- the
ordering is a property of how the result was collected, not something
stitched back together as a separate pass.

`ExampleGather` is this module's runnable demonstration: `go test` executes
it and compares its output against the `// Output:` block, so the ordering
guarantee shown here is checked on every run, not just asserted in prose.

```go
func ExampleGather() {
	upstream := func(i int) string {
		return fmt.Sprintf("resp-%d", i)
	}
	results := Gather(4, upstream)
	fmt.Println(results)
	// Output:
	// [resp-0 resp-1 resp-2 resp-3]
}
```

Because `Gather`'s result order never depends on completion order, this
output is reproducible regardless of how the Go scheduler happens to
interleave the four goroutines on a given run.

### Tests

`TestGatherTable` covers the basic shape: zero and negative `n` both yield
an empty, non-nil slice; one and several elements land at their expected
indices. `TestGatherWithStructType` checks the generic parameter with a
struct rather than a scalar, since a real gateway gathers structured
responses, not integers. `TestGatherInvokesWorkExactlyOncePerIndex` guards
against an off-by-one in the launch loop by counting calls per index with
`atomic.Int32` and requiring every counter to land at exactly one.

`chainedReverseCompletion` is the mechanism that makes the ordering claim
testable at all without `time.Sleep`: it wires `n` channels so that
`work(i)` can only return after `work(i+1)` already has, forcing strict
reverse-completion order deterministically through channel handoffs alone.
`TestGatherPreservesRequestOrderUnderReverseCompletion` runs `Gather` under
that exact schedule and requires request order regardless.
`TestNaiveAppendReturnsCompletionOrderNotRequestOrder` is the heart of the
module: the same deterministic schedule through the unexported
`gatherNaiveAppend` -- never part of the package's exported surface --
produces completion order instead, pinning the defect the mutex-guarded
`append` ships. Both ordering tests deliberately reuse the identical
`chainedReverseCompletion` schedule rather than each inventing their own,
so the contrast is a true apples-to-apples comparison: the only thing that
differs between the two tests is which collection strategy processes the
same sequence of arrivals, which is exactly the variable this module is
about.

Create `scatter_test.go`:

```go
package scatter

import (
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
)

// gatherNaiveAppend is the antipattern this module warns about: it collects
// results with a mutex-guarded append instead of writing to disjoint
// indices. It never corrupts memory -- the mutex serializes every append --
// but the order elements land in results depends on which goroutine
// acquires the lock first, i.e. completion order, not request order.
func gatherNaiveAppend[T any](n int, work func(i int) T) []T {
	if n <= 0 {
		return []T{}
	}
	var mu sync.Mutex
	results := make([]T, 0, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			v := work(i)
			mu.Lock()
			results = append(results, v)
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	return results
}

func TestGatherTable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		n    int
		want []int
	}{
		{name: "zero", n: 0, want: []int{}},
		{name: "negative clamps to empty", n: -3, want: []int{}},
		{name: "one", n: 1, want: []int{0}},
		{name: "five", n: 5, want: []int{0, 1, 2, 3, 4}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := Gather(tc.n, func(i int) int { return i })
			if got == nil {
				t.Fatalf("Gather(%d) returned nil, want non-nil", tc.n)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("Gather(%d) = %v, want %v", tc.n, got, tc.want)
			}
		})
	}
}

// upstreamResponse is a stand-in for a federated GraphQL or batch-endpoint
// response, used to check that Gather works for a struct type T and not
// just for scalars.
type upstreamResponse struct {
	Status int
	Body   string
}

func TestGatherWithStructType(t *testing.T) {
	t.Parallel()

	got := Gather(3, func(i int) upstreamResponse {
		return upstreamResponse{Status: 200 + i, Body: fmt.Sprintf("body-%d", i)}
	})
	want := []upstreamResponse{
		{Status: 200, Body: "body-0"},
		{Status: 201, Body: "body-1"},
		{Status: 202, Body: "body-2"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Gather(struct) = %+v, want %+v", got, want)
	}
}

// TestGatherInvokesWorkExactlyOncePerIndex guards against an off-by-one in
// the goroutine loop launching too few, too many, or duplicate calls: each
// index's call counter must land at exactly 1.
func TestGatherInvokesWorkExactlyOncePerIndex(t *testing.T) {
	t.Parallel()

	const n = 50
	var calls [n]atomic.Int32
	Gather(n, func(i int) int {
		calls[i].Add(1)
		return i
	})
	for i := range calls {
		if got := calls[i].Load(); got != 1 {
			t.Fatalf("work(%d) was called %d times, want 1", i, got)
		}
	}
}

// chainedReverseCompletion builds n channels wired so that work(i) can only
// finish after work(i+1) already has: index n-1 finishes first, index 0
// finishes last. That makes completion order fully deterministic (reverse of
// request order) using channel handoffs alone -- no time.Sleep anywhere.
func chainedReverseCompletion(n int) func(i int) int {
	done := make([]chan struct{}, n)
	for i := range done {
		done[i] = make(chan struct{})
	}
	return func(i int) int {
		if i < n-1 {
			<-done[i+1]
		}
		close(done[i])
		return i
	}
}

// TestGatherPreservesRequestOrderUnderReverseCompletion is the heart of the
// module. Every goroutine finishes in strict reverse order (n-1 first, 0
// last), forced deterministically by chainedReverseCompletion. Gather must
// still return [0, 1, ..., n-1]: request order, because each result lands in
// its own reserved index regardless of when it arrives.
func TestGatherPreservesRequestOrderUnderReverseCompletion(t *testing.T) {
	t.Parallel()

	const n = 6
	got := Gather(n, chainedReverseCompletion(n))
	want := []int{0, 1, 2, 3, 4, 5}
	if !slices.Equal(got, want) {
		t.Fatalf("Gather result = %v, want %v (request order)", got, want)
	}
}

// TestNaiveAppendReturnsCompletionOrderNotRequestOrder pins the defect: under
// the identical deterministic reverse-completion schedule, the mutex-guarded
// append records results in the order goroutines finish -- reversed from the
// request order Gather guarantees.
func TestNaiveAppendReturnsCompletionOrderNotRequestOrder(t *testing.T) {
	t.Parallel()

	const n = 6
	got := gatherNaiveAppend(n, chainedReverseCompletion(n))
	want := []int{5, 4, 3, 2, 1, 0}
	if !slices.Equal(got, want) {
		t.Fatalf("gatherNaiveAppend result = %v, want %v (completion order)", got, want)
	}

	requestOrder := []int{0, 1, 2, 3, 4, 5}
	if slices.Equal(got, requestOrder) {
		t.Fatal("gatherNaiveAppend accidentally preserved request order; the contrast needs it not to")
	}
}

// ExampleGather is the runnable demonstration of this module: it fans four
// units of work out and gathers them back in request order. Gather's result
// order does not depend on goroutine scheduling, so this output is
// reproducible on every run.
func ExampleGather() {
	upstream := func(i int) string {
		return fmt.Sprintf("resp-%d", i)
	}
	results := Gather(4, upstream)
	fmt.Println(results)
	// Output:
	// [resp-0 resp-1 resp-2 resp-3]
}
```

## Review

`Gather` is correct when its result order matches request order regardless
of upstream latency -- `TestGatherPreservesRequestOrderUnderReverseCompletion`
pins exactly that, under a schedule engineered so completion order is the
opposite of request order. The mechanism is preallocating `results` with
`make([]T, n)` before launching any goroutine, so every index is already an
addressable slot that exactly one goroutine ever writes to; disjoint writes
to the same backing array need no lock, because there is no shared field
being contended on, only separate memory being written once each. The trap
this avoids is the mutex-guarded `append`, isolated in the test file as
`gatherNaiveAppend`: it is memory-safe but returns completion order, a bug
that a test run on an idle machine will not reproduce and a production
gateway under real, variable upstream latency will. The panic-propagation
contract on `Gather` is deliberately narrow rather than defensive: a work
function whose upstream call can fail is expected to encode that failure in
`T` and recover any panic itself, because a `Gather` that silently swallowed
panics would turn a crash a caller needed to see into a partially-filled
result slice that looks like success. Run `go test -count=1 -race ./...` to
confirm the table, the struct-typed case, the per-index call count, the
ordering guarantee under a forced reverse schedule, and the contrast against
the naive collector.

## Resources

- [Go Spec: Appending to and copying slices](https://go.dev/ref/spec#Appending_and_copying_slices) — why concurrent `append` on a shared slice is a data race.
- [The Go Memory Model](https://go.dev/ref/mem) — the happens-before guarantees `sync.WaitGroup` and channel operations provide, which this module's tests rely on for determinism.
- [`sync.WaitGroup`](https://pkg.go.dev/sync#WaitGroup) — the primitive `Gather` uses to block until every goroutine has written its result.
- [Go Race Detector](https://go.dev/doc/articles/race_detector) — why disjoint-index writes pass `-race` cleanly while a shared, unguarded `append` would not.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [10-correlation-id-capacity-pinning.md](10-correlation-id-capacity-pinning.md) | Next: [12-sliding-window-front-truncation-compaction.md](12-sliding-window-front-truncation-compaction.md)
