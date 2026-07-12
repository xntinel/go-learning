# Exercise 5: Scatter-Gather — Collect N Worker Results with First-Error Semantics

A query coordinator fans a request out to N backends and gathers their replies into
one aggregate, aborting the instant any backend fails. The gather side is a
`select` loop over a results channel and an errors channel, terminated by a counter
— no `default`, no busy-spin, no fixed number of `case`s per backend. This module
builds `Collect`.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. It imports no other exercise.

## What you'll build

```text
scattergather/                  module example.com/scattergather
  go.mod                        go 1.26
  scattergather.go              Collect(n, results, errs) ([]Result, error)
  cmd/
    demo/
      main.go                   fan out 5 shard queries, gather, print
  scattergather_test.go         all-succeed, first-error-wins, exact-count termination
```

Files: `scattergather.go`, `cmd/demo/main.go`, `scattergather_test.go`.
Implement: `Collect(n int, results <-chan Result, errs <-chan error) ([]Result, error)` — gather exactly n results, or return on the first error.
Test: n results aggregate in any order; one interleaved error wins and aborts early; the loop stops after exactly n and never blocks for an (n+1)th.
Verify: `go test -count=1 -race ./...`

## The counter is the terminator

The naive urge is to write one `case` per backend, or to poll with a `default`.
Both are wrong. The backend count is a runtime value, and a `default` turns the
gather into a busy-spin that pegs a core while it waits for the next reply. The
right shape is a single two-case `select` — one case for a success on `results`,
one for a failure on `errs` — wrapped in a loop whose termination condition is a
*count*, not a channel state:

```go
for len(collected) < n {
	select {
	case r := <-results:
		collected = append(collected, r)
	case err := <-errs:
		return collected, err // first error aborts the gather
	}
}
```

The loop blocks on the `select` between receives, so it consumes no CPU while
waiting; it wakes once per reply. Each success appends and advances the counter
toward `n`; the loop exits precisely when it has `n` results. A failure short-
circuits: the first value seen on `errs` is returned immediately, along with
whatever partial results were gathered so far, and the remaining in-flight backends
are simply abandoned (in a real coordinator you would also cancel them — that is
the hedged-read pattern in the next module).

Because the terminator is `len(collected) == n` and not "the channel is closed,"
`Collect` never waits for an (n+1)th value it does not need and never spins on a
drained channel. It reads exactly as many times as it must and returns. The errors
are wrapped with `%w` at the producer so the caller can match a sentinel with
`errors.Is` — the test relies on that.

Create `scattergather.go`:

```go
package scattergather

// Result is one backend's successful reply.
type Result struct {
	Shard int
	Value string
}

// Collect gathers exactly n successful results from the results channel, or
// returns the first error observed on the errs channel. It blocks on a select
// between the two channels and terminates on a count, so it neither busy-spins
// nor waits for more results than requested. On error it returns the partial
// results gathered so far alongside the error.
func Collect(n int, results <-chan Result, errs <-chan error) ([]Result, error) {
	collected := make([]Result, 0, n)
	for len(collected) < n {
		select {
		case r := <-results:
			collected = append(collected, r)
		case err := <-errs:
			return collected, err
		}
	}
	return collected, nil
}
```

## The runnable demo

The demo fans five shard queries out as goroutines, each sending its result, and
gathers all five. Output order is nondeterministic, so it sorts before printing.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sort"

	"example.com/scattergather"
)

func main() {
	const n = 5
	results := make(chan scattergather.Result, n)
	errs := make(chan error, n)

	for i := range n {
		go func() {
			results <- scattergather.Result{Shard: i, Value: fmt.Sprintf("rows=%d", i*10)}
		}()
	}

	got, err := scattergather.Collect(n, results, errs)
	if err != nil {
		fmt.Println("gather failed:", err)
		return
	}

	sort.Slice(got, func(i, j int) bool { return got[i].Shard < got[j].Shard })
	fmt.Printf("gathered %d results:\n", len(got))
	for _, r := range got {
		fmt.Printf("shard %d -> %s\n", r.Shard, r.Value)
	}
}
```

Run with `go run ./cmd/demo`.

Expected output:

```
gathered 5 results:
shard 0 -> rows=0
shard 1 -> rows=10
shard 2 -> rows=20
shard 3 -> rows=30
shard 4 -> rows=40
```

## Tests

`TestCollectAllSucceed` fires n concurrent producers and asserts the aggregate has
all n results as a multiset, order-independent. `TestCollectFirstErrorWins`
interleaves one failure among the successes and asserts `Collect` returns that
error — matched against a sentinel with `errors.Is` — and stops early with fewer
than n results. `TestCollectExactCount` makes more than n results available and
asserts `Collect` returns after exactly n, within a time budget, proving it does
not block waiting for an unneeded (n+1)th value.

Create `scattergather_test.go`:

```go
package scattergather

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

var errShardFailed = errors.New("shard query failed")

func TestCollectAllSucceed(t *testing.T) {
	t.Parallel()

	const n = 6
	results := make(chan Result, n)
	errs := make(chan error, n)
	for i := range n {
		go func() { results <- Result{Shard: i, Value: "ok"} }()
	}

	got, err := Collect(n, results, errs)
	if err != nil {
		t.Fatalf("Collect: unexpected error %v", err)
	}
	if len(got) != n {
		t.Fatalf("Collect: got %d results, want %d", len(got), n)
	}
	seen := map[int]bool{}
	for _, r := range got {
		if seen[r.Shard] {
			t.Fatalf("shard %d appeared twice", r.Shard)
		}
		seen[r.Shard] = true
	}
	if len(seen) != n {
		t.Fatalf("distinct shards = %d, want %d", len(seen), n)
	}
}

func TestCollectFirstErrorWins(t *testing.T) {
	t.Parallel()

	const n = 5
	results := make(chan Result, n)
	errs := make(chan error, 1)

	// Two successes then a failure; the failure must abort before n succeed.
	results <- Result{Shard: 0, Value: "ok"}
	results <- Result{Shard: 1, Value: "ok"}
	errs <- fmt.Errorf("shard 2: %w", errShardFailed)

	got, err := Collect(n, results, errs)
	if err == nil {
		t.Fatal("Collect: err = nil, want the shard failure")
	}
	if !errors.Is(err, errShardFailed) {
		t.Fatalf("Collect: err = %v, want wrapping %v", err, errShardFailed)
	}
	if len(got) >= n {
		t.Fatalf("Collect returned %d results; must abort before %d on error", len(got), n)
	}
}

func TestCollectExactCount(t *testing.T) {
	t.Parallel()

	const n = 3
	// More than n results are available; Collect must stop after exactly n.
	results := make(chan Result, n+2)
	errs := make(chan error, 1)
	for i := range n + 2 {
		results <- Result{Shard: i, Value: "ok"}
	}

	done := make(chan []Result, 1)
	go func() {
		got, _ := Collect(n, results, errs)
		done <- got
	}()

	select {
	case got := <-done:
		if len(got) != n {
			t.Fatalf("Collect returned %d results, want exactly %d", len(got), n)
		}
	case <-time.After(time.Second):
		t.Fatal("Collect blocked; it waited for more than n results")
	}
	// Exactly n consumed; the surplus remains buffered.
	if left := len(results); left != 2 {
		t.Fatalf("results left buffered = %d, want 2 (Collect over-consumed)", left)
	}
}
```

## Review

`Collect` is correct when it returns exactly n results on the happy path, returns
the first error the instant it appears (carrying the partial aggregate), and
terminates on the counter rather than on channel state — so it neither spins nor
waits for a value it does not need. `TestCollectExactCount`'s check that two
surplus results remain buffered is the guard against an over-consuming loop. The
error path leans on `%w` + `errors.Is`: the coordinator returns a wrapped sentinel,
and the caller matches it without string comparison. When you later want to cancel
the abandoned backends instead of just dropping them, that is the shared-stop-
channel pattern in Exercise 6.

## Resources

- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — fan-out/fan-in and bounded gather.
- [errors.Is and %w wrapping](https://pkg.go.dev/errors#Is) — the first-error matching this module's test asserts.
- [Go Specification: Select statements](https://go.dev/ref/spec#Select_statements) — receive arbitration between the results and errors channels.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-done-channel-worker.md](04-done-channel-worker.md) | Next: [06-hedged-replica-read.md](06-hedged-replica-read.md)
