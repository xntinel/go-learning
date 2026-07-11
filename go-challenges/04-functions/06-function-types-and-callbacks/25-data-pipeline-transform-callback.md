# Exercise 25: Data Transformation Pipeline with Chainable Callbacks

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye concurrencia).

A streaming ingestion pipeline normalizes each record through several
steps — trim, lowercase, validate, truncate — and when one step fails on a
particular record, the pipeline needs a policy for what happens next:
substitute a default and keep going, or propagate the failure. This module
builds that as chainable `Transform` callbacks with a `Recover` hook, plus a
worker-pool runner that applies one pipeline across many items concurrently
without losing input order.

## What you'll build

```text
pipeline/                   independent module: example.com/data-pipeline-transform-callback
  go.mod                     go 1.24
  pipeline.go                  type Transform, type Recover, func Chain, func WithRecover, func RunConcurrent
  cmd/
    demo/
      main.go                  runnable demo: a normalize-with-recovery pipeline run sequentially, then concurrently
  pipeline_test.go             table test: chain ordering and short-circuit, recover substitute/decline/skip-on-success, concurrent order preservation, per-item errors, single-worker determinism (-race)
```

Files: `pipeline.go`, `cmd/demo/main.go`, `pipeline_test.go`.
Implement: `type Transform[T any] func(input T) (T, error)`, `type Recover[T any] func(input T, err error) (T, bool)`, `func Chain[T any](transforms ...Transform[T]) Transform[T]`, `func WithRecover[T any](t Transform[T], recover Recover[T]) Transform[T]`, and `func RunConcurrent[T any](items []T, transform Transform[T], workers int) ([]T, []error)`.
Test: `Chain` applying transforms in order, `Chain` stopping before a later transform once an earlier one fails, `WithRecover` substituting a value on failure, `WithRecover` propagating the original error when the hook declines, `WithRecover` never invoking the hook on success, `RunConcurrent` preserving input order regardless of goroutine completion order, `RunConcurrent` capturing a per-item error without corrupting the others' results, and a single-worker run staying trivially deterministic.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/data-pipeline-transform-callback/cmd/demo
cd ~/go-exercises/data-pipeline-transform-callback
go mod init example.com/data-pipeline-transform-callback
go mod edit -go=1.24
```

### Why `RunConcurrent` needs no mutex around its shared slices

`Chain` and `WithRecover` are ordinary composition: `Chain` threads a value
through a slice of `Transform`s and stops at the first error, `WithRecover`
wraps one `Transform` so a failure gets a second opinion from a `Recover`
callback before the caller sees an error at all. The part worth studying
closely is `RunConcurrent`. Every worker goroutine writes into `results[i]`
and `errs[i]` for the one index `i` it just pulled off the `jobs` channel —
and since two different indices are two different memory addresses, no two
goroutines ever touch the same slice element, which is precisely the
condition under which concurrent writes to a slice need *no* mutex at all.
The only state actually shared between goroutines is the `jobs` channel
itself, and a channel is already safe for concurrent send/receive by
construction. That is also why the result stays ordered exactly like
`items` no matter which goroutine happens to finish first: order here comes
from each result being written to a fixed, pre-assigned slot, not from the
order work happened to complete in.

Create `pipeline.go`:

```go
// Package pipeline builds data transformation pipelines from small,
// chainable Transform callbacks, with an error-recovery hook and a
// concurrent runner for applying one pipeline across many items.
package pipeline

import "sync"

// Transform maps one value of type T to another, or reports an error.
type Transform[T any] func(input T) (T, error)

// Recover is called when a Transform fails. It receives the input that
// failed and the error, and returns a replacement value plus whether the
// pipeline should continue with that value (true) or abort with the
// original error (false).
type Recover[T any] func(input T, err error) (T, bool)

// Chain composes transforms into one Transform that runs them in order,
// stopping at the first error.
func Chain[T any](transforms ...Transform[T]) Transform[T] {
	return func(input T) (T, error) {
		current := input
		for _, t := range transforms {
			out, err := t(current)
			if err != nil {
				return current, err
			}
			current = out
		}
		return current, nil
	}
}

// WithRecover wraps t so that a failure is offered to recover; if recover
// reports true, the pipeline continues with the recovered value instead
// of failing.
func WithRecover[T any](t Transform[T], recover Recover[T]) Transform[T] {
	return func(input T) (T, error) {
		out, err := t(input)
		if err == nil {
			return out, nil
		}
		recovered, ok := recover(input, err)
		if !ok {
			return input, err
		}
		return recovered, nil
	}
}

// RunConcurrent applies transform to every item using workers goroutines,
// and returns a result slice and an error slice, both indexed exactly
// like items regardless of goroutine completion order.
//
// Each worker only ever writes to results[i] and errs[i] for the single
// index i it just pulled from the shared jobs channel, so no two
// goroutines ever write the same slice element; the jobs channel is the
// only state actually shared between goroutines, and channels are safe
// for concurrent use without an explicit mutex.
func RunConcurrent[T any](items []T, transform Transform[T], workers int) ([]T, []error) {
	if workers < 1 {
		workers = 1
	}
	results := make([]T, len(items))
	errs := make([]error, len(items))

	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				out, err := transform(items[i])
				results[i] = out
				errs[i] = err
			}
		}()
	}

	for i := range items {
		jobs <- i
	}
	close(jobs)
	wg.Wait()

	return results, errs
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"strings"

	"example.com/data-pipeline-transform-callback"
)

func trim(s string) (string, error) {
	return strings.TrimSpace(s), nil
}

func lower(s string) (string, error) {
	return strings.ToLower(s), nil
}

func requireNonEmpty(s string) (string, error) {
	if s == "" {
		return s, errors.New("empty after trim")
	}
	return s, nil
}

func truncate10(s string) (string, error) {
	if len(s) > 10 {
		return s[:10], nil
	}
	return s, nil
}

func main() {
	normalize := pipeline.Chain(trim, lower, requireNonEmpty, truncate10)
	withDefault := pipeline.WithRecover(normalize, func(input string, err error) (string, bool) {
		return "unknown", true
	})

	inputs := []string{"  Hello World  ", "   ", "ALREADYLOWERCASEBUTLONG"}
	for _, in := range inputs {
		out, err := withDefault(in)
		fmt.Printf("%q -> %q (err=%v)\n", in, out, err)
	}

	results, errs := pipeline.RunConcurrent(inputs, withDefault, 3)
	for i, r := range results {
		fmt.Printf("concurrent[%d] = %q (err=%v)\n", i, r, errs[i])
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
"  Hello World  " -> "hello worl" (err=<nil>)
"   " -> "unknown" (err=<nil>)
"ALREADYLOWERCASEBUTLONG" -> "alreadylow" (err=<nil>)
concurrent[0] = "hello worl" (err=<nil>)
concurrent[1] = "unknown" (err=<nil>)
concurrent[2] = "alreadylow" (err=<nil>)
```

### Tests

Create `pipeline_test.go`:

```go
package pipeline

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func trimNoErr(s string) (string, error)  { return strings.TrimSpace(s), nil }
func upperNoErr(s string) (string, error) { return strings.ToUpper(s), nil }
func addExclaim(s string) (string, error) { return s + "!", nil }

func failIfContains(bad string) Transform[string] {
	return func(s string) (string, error) {
		if strings.Contains(s, bad) {
			return s, fmt.Errorf("contains %q", bad)
		}
		return s, nil
	}
}

func TestChainAppliesTransformsInOrder(t *testing.T) {
	t.Parallel()
	chain := Chain(trimNoErr, upperNoErr, addExclaim)
	got, err := chain("  hi  ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "HI!" {
		t.Fatalf("got %q, want %q", got, "HI!")
	}
}

func TestChainStopsAtFirstError(t *testing.T) {
	t.Parallel()
	var thirdCalled bool
	chain := Chain(
		upperNoErr,
		failIfContains("BAD"),
		func(s string) (string, error) {
			thirdCalled = true
			return s, nil
		},
	)
	_, err := chain("this is bad")
	if err == nil {
		t.Fatal("expected an error from the second transform")
	}
	if thirdCalled {
		t.Fatal("transform after the failing one should not run")
	}
}

func TestWithRecoverSubstitutesOnFailure(t *testing.T) {
	t.Parallel()
	risky := Chain(failIfContains("bad"))
	recovered := WithRecover(risky, func(input string, err error) (string, bool) {
		return "safe-default", true
	})

	got, err := recovered("this is bad")
	if err != nil {
		t.Fatalf("recovered pipeline should not return an error, got %v", err)
	}
	if got != "safe-default" {
		t.Fatalf("got %q, want safe-default", got)
	}
}

func TestWithRecoverCanDeclineAndPropagate(t *testing.T) {
	t.Parallel()
	risky := Chain(failIfContains("bad"))
	noRecover := WithRecover(risky, func(input string, err error) (string, bool) {
		return input, false
	})

	_, err := noRecover("this is bad")
	if err == nil {
		t.Fatal("expected the original error to propagate when recover declines")
	}
}

func TestWithRecoverPassesThroughOnSuccess(t *testing.T) {
	t.Parallel()
	calledRecover := false
	safe := WithRecover(upperNoErr, func(input string, err error) (string, bool) {
		calledRecover = true
		return input, true
	})
	got, err := safe("ok")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "OK" {
		t.Fatalf("got %q, want OK", got)
	}
	if calledRecover {
		t.Fatal("recover should not be called when the transform succeeds")
	}
}

func TestRunConcurrentPreservesOrderRegardlessOfCompletionOrder(t *testing.T) {
	t.Parallel()
	items := []int{1, 2, 3, 4, 5, 6, 7, 8}
	double := Transform[int](func(n int) (int, error) { return n * 2, nil })

	results, errs := RunConcurrent(items, double, 4)
	for i, item := range items {
		if errs[i] != nil {
			t.Fatalf("errs[%d] = %v, want nil", i, errs[i])
		}
		if results[i] != item*2 {
			t.Fatalf("results[%d] = %d, want %d", i, results[i], item*2)
		}
	}
}

func TestRunConcurrentCapturesPerItemErrors(t *testing.T) {
	t.Parallel()
	items := []int{1, 2, 3, 4}
	failOnEven := Transform[int](func(n int) (int, error) {
		if n%2 == 0 {
			return 0, errors.New("even not allowed")
		}
		return n, nil
	})

	results, errs := RunConcurrent(items, failOnEven, 2)
	for i, item := range items {
		wantErr := item%2 == 0
		if (errs[i] != nil) != wantErr {
			t.Errorf("index %d: err = %v, wantErr = %v", i, errs[i], wantErr)
		}
		if !wantErr && results[i] != item {
			t.Errorf("index %d: results = %d, want %d", i, results[i], item)
		}
	}
}

func TestRunConcurrentWithSingleWorkerIsDeterministic(t *testing.T) {
	t.Parallel()
	items := []int{5, 4, 3, 2, 1}
	square := Transform[int](func(n int) (int, error) { return n * n, nil })

	results, _ := RunConcurrent(items, square, 1)
	want := []int{25, 16, 9, 4, 1}
	for i := range want {
		if results[i] != want[i] {
			t.Fatalf("results[%d] = %d, want %d", i, results[i], want[i])
		}
	}
}
```

## Review

`Chain` never knows how many transforms it has or what they do — it only
reacts to the first non-nil error, which `TestChainStopsAtFirstError`
confirms by proving a transform placed after a failing one never runs at
all. `WithRecover` treats "the underlying transform failed" as a decision
point, not a dead end: `TestWithRecoverSubstitutesOnFailure` and
`TestWithRecoverCanDeclineAndPropagate` are the same hook answering two
different ways, and `TestWithRecoverPassesThroughOnSuccess` guards the
case that is easy to get backwards — calling `recover` even when nothing
failed, which would make error recovery observable (and potentially
expensive) on the successful path too. `TestRunConcurrentPreservesOrder
RegardlessOfCompletionOrder` is the test that actually matters for the
"streaming pipeline" framing: run it under `-race` and it also proves the
disjoint-index write pattern documented on `RunConcurrent` has no data race
to catch, because there genuinely isn't one.

## Resources

- [Go Specification: Function types](https://go.dev/ref/spec#Function_types)
- [sync.WaitGroup](https://pkg.go.dev/sync#WaitGroup)
- [Go blog: Go Concurrency Patterns: Pipelines and cancellation](https://go.dev/blog/pipelines)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [24-auth-validator-callback-chain.md](24-auth-validator-callback-chain.md) | Next: [26-database-transaction-hook-callback.md](26-database-transaction-hook-callback.md)
