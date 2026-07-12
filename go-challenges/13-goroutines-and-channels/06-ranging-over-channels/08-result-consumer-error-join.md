# Exercise 8: Consume Result Values and Aggregate Partial Failures

When parallel workers each return a value or an error, the aggregating consumer
must not stop at the first failure — it collects the successes it can and reports
every failure together. This exercise ranges a channel of `Result{Value, Err}` and
uses `errors.Join` to hand the caller both the partial results and one aggregated
error.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
resultconsumer/             independent module: example.com/resultconsumer
  go.mod                    go 1.26
  consumer.go               type Result[T]; Consume(ch) ([]T, error) via errors.Join
  cmd/
    demo/
      main.go               mixed success/failure stream aggregated
  consumer_test.go          mixed, all-success nil err, all-failure, nils discarded
```

Files: `consumer.go`, `cmd/demo/main.go`, `consumer_test.go`.
Implement: `Result[T any]` with `Value` and `Err`, and `Consume[T any](ch <-chan Result[T]) ([]T, error)` that collects successes into a slice and combines all failures with `errors.Join`.
Test: a mixed stream returns the collected successes plus a joined error that `errors.Is`-matches each injected sentinel; an all-success stream returns a nil error; an all-failure stream returns a joined error and no values; nil errors are discarded so the combined error reflects only real failures.
Verify: `go test -count=1 -race ./...`

### Why errors.Join is the right aggregator

A fan-in-with-errors consumer faces a design choice: on the first error, do you
abort or keep going? For batch work — validating a thousand records, fanning a
request to many backends — aborting throws away good results and hides all but one
failure. The senior default is to drain the whole stream, collect every success,
and combine every error into one. `errors.Join(errs ...error)` is built for
exactly this: it returns a single error wrapping all the non-nil arguments, and
its `Error()` string lists each on its own line. Crucially, the joined error still
`errors.Is`-matches every sentinel it wraps, so a caller can test the aggregate for
any specific failure — `errors.Is(err, ErrTimeout)` is true if any worker timed
out, even though the error also wraps three others.

Two properties of `errors.Join` make the consumer simple. First, it discards nil
arguments: passing a mix of nils and real errors yields an error that wraps only
the real ones. Second, if every argument is nil (or there are none), it returns
nil. So the consumer can branch on `r.Err != nil` to split values from errors, but
even a naive "collect all `r.Err`" approach would be correct because `Join` filters
the nils itself. The exercise branches explicitly for clarity: a `Result` with a
nil `Err` is a success carrying a `Value`; a non-nil `Err` is a failure.

The signature returns `([]T, error)`: partial results *and* the aggregate. That is
the whole point — the caller gets the 997 records that validated and one error
naming the 3 that did not. Returning only the error would discard the successes;
returning only the values would hide the failures.

Create `consumer.go`:

```go
package resultconsumer

import "errors"

// Result is one worker's outcome: a Value on success, or a non-nil Err on failure.
type Result[T any] struct {
	Value T
	Err   error
}

// Ok builds a success result.
func Ok[T any](v T) Result[T] { return Result[T]{Value: v} }

// Fail builds a failure result.
func Fail[T any](err error) Result[T] { return Result[T]{Err: err} }

// Consume drains ch, collecting the values of successful results and joining the
// errors of failed ones. It returns the partial successes and a single aggregated
// error (nil if every result succeeded).
func Consume[T any](ch <-chan Result[T]) ([]T, error) {
	var values []T
	var errs []error
	for r := range ch {
		if r.Err != nil {
			errs = append(errs, r.Err)
			continue
		}
		values = append(values, r.Value)
	}
	return values, errors.Join(errs...)
}
```

### The runnable demo

The demo feeds a mixed stream — two successes and two failures with distinct
sentinel errors — and prints the collected values and the joined error, whose
string lists both failures.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/resultconsumer"
)

var (
	errTimeout  = errors.New("timeout")
	errNotFound = errors.New("not found")
)

func main() {
	ch := make(chan resultconsumer.Result[string], 4)
	ch <- resultconsumer.Ok("alice")
	ch <- resultconsumer.Fail[string](errTimeout)
	ch <- resultconsumer.Ok("bob")
	ch <- resultconsumer.Fail[string](errNotFound)
	close(ch)

	values, err := resultconsumer.Consume(ch)
	fmt.Printf("values: %v\n", values)
	fmt.Printf("timeout seen: %v\n", errors.Is(err, errTimeout))
	fmt.Printf("error: %v\n", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
values: [alice bob]
timeout seen: true
error: timeout
not found
```

### Tests

`TestMixedStream` feeds successes and failures and asserts both the collected
values and that the joined error `errors.Is`-matches each sentinel.
`TestAllSuccessNilError` asserts a stream with no failures returns a nil error.
`TestAllFailure` asserts an all-error stream returns no values and an error that
matches every sentinel. `TestNilErrorsDiscarded` mixes success results (nil `Err`)
with one failure and asserts the aggregate matches only the real failure — proving
`Join` drops the nils.

Create `consumer_test.go`:

```go
package resultconsumer

import (
	"errors"
	"fmt"
	"testing"
)

var (
	errA = errors.New("failure A")
	errB = errors.New("failure B")
)

func stream[T any](results ...Result[T]) <-chan Result[T] {
	ch := make(chan Result[T], len(results))
	for _, r := range results {
		ch <- r
	}
	close(ch)
	return ch
}

func TestMixedStream(t *testing.T) {
	t.Parallel()
	ch := stream(Ok(1), Fail[int](errA), Ok(2), Fail[int](errB))

	values, err := Consume(ch)
	if len(values) != 2 || values[0] != 1 || values[1] != 2 {
		t.Fatalf("values = %v, want [1 2]", values)
	}
	if !errors.Is(err, errA) || !errors.Is(err, errB) {
		t.Fatalf("joined err = %v, want to match both errA and errB", err)
	}
}

func TestAllSuccessNilError(t *testing.T) {
	t.Parallel()
	values, err := Consume(stream(Ok("x"), Ok("y"), Ok("z")))
	if err != nil {
		t.Fatalf("err = %v, want nil for all-success stream", err)
	}
	if len(values) != 3 {
		t.Fatalf("values = %v, want 3", values)
	}
}

func TestAllFailure(t *testing.T) {
	t.Parallel()
	values, err := Consume(stream(Fail[int](errA), Fail[int](errB)))
	if len(values) != 0 {
		t.Fatalf("values = %v, want none", values)
	}
	if !errors.Is(err, errA) || !errors.Is(err, errB) {
		t.Fatalf("joined err = %v, want to match errA and errB", err)
	}
}

func TestNilErrorsDiscarded(t *testing.T) {
	t.Parallel()
	// Two successes (nil Err) and one failure: Join must reflect only the failure.
	_, err := Consume(stream(Ok(1), Ok(2), Fail[int](errA)))
	if !errors.Is(err, errA) {
		t.Fatalf("err = %v, want to match errA", err)
	}
	if errors.Is(err, errB) {
		t.Fatalf("err = %v, unexpectedly matches errB", err)
	}
}

func ExampleConsume() {
	values, err := Consume(stream(Ok(10), Fail[int](errA), Ok(20)))
	fmt.Println(values, errors.Is(err, errA))
	// Output: [10 20] true
}
```

## Review

The consumer is correct when it never stops early, returns every success, and
returns one error that matches every real failure and none of the phantom ones.
The `errors.Is` assertions are the right tool: the joined error is opaque as a
string but transparent to `Is`, so a caller can still branch on a specific
sentinel. `TestNilErrorsDiscarded` pins the `Join` semantics that keep the API
clean — a success contributes a value and no error, and the aggregate reflects
only genuine failures. Returning `([]T, error)` rather than bailing on the first
error is the design decision that makes this a partial-failure aggregator instead
of a fail-fast one.

## Resources

- [pkg.go.dev: errors.Join](https://pkg.go.dev/errors#Join) — wraps multiple errors, discards nils, returns nil if all are nil.
- [pkg.go.dev: errors.Is](https://pkg.go.dev/errors#Is) — matches a target sentinel through a joined/wrapped error.
- [Go Blog: Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — wrapping, `%w`, and the `Is`/`As` model.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [07-pipeline-stage.md](07-pipeline-stage.md) | Next: [09-dedup-idempotent-consumer.md](09-dedup-idempotent-consumer.md)
