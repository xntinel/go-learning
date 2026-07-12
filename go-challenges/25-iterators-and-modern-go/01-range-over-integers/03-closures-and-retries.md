# Exercise 3: Closures and Bounded Retries

Two patterns show the integer range form interacting with the rest of the language: building a slice of closures that each capture their own iteration value, and replacing a classic three-clause loop with `for range attempts` to drive a bounded retry. This exercise builds a `tasks` package with `Stages`, which returns a pipeline of index-tagged closures, and `Retry`, which calls a function up to `n` times and stops at the first success.

This module is fully self-contained. It has its own `go mod init`, defines every type and function it needs, and ships its own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
tasks.go             Stages (closure pipeline), Retry, ErrExhausted
cmd/
  demo/
    main.go          run a 3-stage pipeline, retry a flaky call, exhaust a failing one
tasks_test.go        distinct-closure capture, per-index check, retry success/exhaust/zero
```

- Files: `tasks.go`, `cmd/demo/main.go`, `tasks_test.go`.
- Implement: `Stages(n int) []func(string) string`, `Retry(attempts int, fn func() error) error`, and the sentinel `ErrExhausted`.
- Test: prove each stage closure captures its own index, that `Retry` returns on first success, exhausts after the last failure, and treats zero attempts as exhausted without calling `fn`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/25-iterators-and-modern-go/01-range-over-integers/03-closures-and-retries/cmd/demo && cd go-solutions/25-iterators-and-modern-go/01-range-over-integers/03-closures-and-retries
```

### Why each stage captures its own index, and why Retry drops the counter

`Stages` builds a slice of `n` functions in a `for i := range n` loop, and the body of each function refers to `i`. The behavior that makes this useful is Go 1.22's per-iteration scope: each pass through the loop gets a fresh `i`, so each closure captures a distinct value. `Stages(3)` returns three functions that tag their input with `>stage0`, `>stage1`, and `>stage2` respectively. Run them in sequence over `"input"` and you get `input>stage0>stage1>stage2`. Under the pre-1.22 rules, the same source produced three closures that all shared one `i` whose final value was `n-1`, so every stage would have tagged `>stage2` — the canonical loop-variable-capture bug. Here it is simply correct, and the test asserts the three numbers are distinct. (The history of that semantic change is the next lesson; this exercise depends on the post-1.22 behavior as a fact.)

`Retry` is the other half: a loop whose body never reads the iteration number, so it uses `for range attempts`. The job is "call `fn` up to `attempts` times, return on the first `nil`." The three-clause form `for i := 0; i < attempts; i++` would introduce an `i` that the body would never touch, which is exactly the unused index the range form removes. `Retry` keeps a `last` error so that, if every attempt fails, it can wrap the final failure with `ErrExhausted` and return it. Two edge cases shape the control flow. First, success short-circuits: the moment `fn()` returns `nil`, `Retry` returns `nil` immediately, without running the remaining attempts. Second, `attempts == 0` means the loop body never runs, so `fn` is never called and `last` stays `nil`; the function returns a bare `ErrExhausted` to signal "you asked for zero tries." Both cases are testable and both are the kind of boundary a naive retry helper gets wrong.

Create `tasks.go`:

```go
package tasks

import (
	"errors"
	"fmt"
)

// Stages builds a slice of n closures. Each one closes over its own copy of the
// loop variable i, because Go 1.22 gives the integer range variable per-iteration
// scope. The closures therefore report distinct stage numbers; before Go 1.22
// they would all have shared one variable and reported n-1.
func Stages(n int) []func(string) string {
	stages := make([]func(string) string, n)
	for i := range n {
		stages[i] = func(s string) string {
			return fmt.Sprintf("%s>stage%d", s, i)
		}
	}
	return stages
}

// ErrExhausted reports that every retry attempt failed, or that zero attempts
// were requested.
var ErrExhausted = errors.New("all attempts failed")

// Retry calls fn up to attempts times and returns nil on the first success. It
// uses `for range attempts` because only the repetition count drives the loop,
// not a hand-managed index. Zero attempts returns ErrExhausted without calling
// fn; otherwise the final failure is wrapped with ErrExhausted.
func Retry(attempts int, fn func() error) error {
	var last error
	for range attempts {
		last = fn()
		if last == nil {
			return nil
		}
	}
	if last == nil {
		return ErrExhausted
	}
	return fmt.Errorf("%w after %d attempts: %v", ErrExhausted, attempts, last)
}
```

`Stages` assigns by index into a preallocated slice; the closure literal captures `i`, and because each iteration has its own `i`, the closures do not alias. `Retry` returns `nil` the instant a call succeeds, so a function that succeeds on its second of five attempts is called exactly twice. When `last` is still `nil` after the loop, the only way that happens is `attempts <= 0`, so the bare sentinel is returned; otherwise the wrapped error preserves both the attempt count and the underlying failure while remaining matchable with `errors.Is(err, ErrExhausted)`.

### The runnable demo

The demo runs a three-stage pipeline, retries a function that fails twice then succeeds, and exhausts a function that always fails.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/tasks"
)

func main() {
	stages := tasks.Stages(3)
	out := "input"
	for _, stage := range stages {
		out = stage(out)
	}
	fmt.Println("pipeline:", out)

	// A function that fails twice, then succeeds on the third call.
	calls := 0
	flaky := func() error {
		calls++
		if calls < 3 {
			return fmt.Errorf("transient failure %d", calls)
		}
		return nil
	}
	if err := tasks.Retry(5, flaky); err != nil {
		fmt.Println("retry:", err)
	} else {
		fmt.Printf("retry: succeeded after %d calls\n", calls)
	}

	// A function that always fails, exhausting every attempt.
	err := tasks.Retry(2, func() error { return errors.New("down") })
	fmt.Println("exhausted:", errors.Is(err, tasks.ErrExhausted))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
pipeline: input>stage0>stage1>stage2
retry: succeeded after 3 calls
exhausted: true
```

### Tests

The tests pin the closure-capture behavior and every branch of `Retry`. `TestStagesAreDistinct` runs the whole pipeline and checks the composed string, which can only be correct if each stage captured its own index. `TestStagesCaptureOwnIndex` checks each stage in isolation against its expected number. `TestRetrySucceeds` proves the short-circuit (three calls, not five). `TestRetryExhausts` proves the wrapped sentinel after the last failure. `TestRetryZeroAttempts` proves `fn` is never called and `ErrExhausted` is returned.

Create `tasks_test.go`:

```go
package tasks

import (
	"errors"
	"testing"
)

func TestStagesAreDistinct(t *testing.T) {
	t.Parallel()

	stages := Stages(3)
	got := "x"
	for _, s := range stages {
		got = s(got)
	}
	want := "x>stage0>stage1>stage2"
	if got != want {
		t.Fatalf("pipeline = %q, want %q", got, want)
	}
}

func TestStagesCaptureOwnIndex(t *testing.T) {
	t.Parallel()

	stages := Stages(4)
	for i, s := range stages {
		got := s("")
		want := ">stage" + string(rune('0'+i))
		if got != want {
			t.Fatalf("stage %d returned %q, want %q", i, got, want)
		}
	}
}

func TestRetrySucceeds(t *testing.T) {
	t.Parallel()

	calls := 0
	err := Retry(5, func() error {
		calls++
		if calls < 3 {
			return errors.New("transient")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Retry error = %v", err)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
}

func TestRetryExhausts(t *testing.T) {
	t.Parallel()

	calls := 0
	err := Retry(3, func() error {
		calls++
		return errors.New("down")
	})
	if !errors.Is(err, ErrExhausted) {
		t.Fatalf("err = %v, want ErrExhausted", err)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
}

func TestRetryZeroAttempts(t *testing.T) {
	t.Parallel()

	calls := 0
	err := Retry(0, func() error { calls++; return nil })
	if !errors.Is(err, ErrExhausted) {
		t.Fatalf("err = %v, want ErrExhausted", err)
	}
	if calls != 0 {
		t.Fatalf("calls = %d, want 0", calls)
	}
}
```

## Review

The package is correct when the closures capture distinct values and the retry honors its boundaries. `Stages` relies on Go 1.22's per-iteration scope so that each `for i := range n` pass binds a fresh `i`; the pipeline test and the per-index test both fail if the closures alias a shared variable. `Retry` uses `for range attempts` because the index is never read, returns immediately on the first success, treats zero attempts as exhausted without invoking `fn`, and otherwise wraps the final failure with `ErrExhausted` so callers can match it with `errors.Is`.

The traps this code avoids: assuming closures built in a loop share one variable (true before Go 1.22, false now, and the whole reason the pipeline composes); keeping a three-clause loop in `Retry` where the counter is dead; and forgetting the `attempts == 0` boundary, where a careless implementation either returns `nil` (claiming success it never achieved) or calls `fn` once anyway. The `-race` run with all five tests passing establishes the capture behavior and every retry branch.

## Resources

- [Go Specification: For statements with range clause](https://go.dev/ref/spec#For_range) — the rules for ranging over an integer, including the per-iteration scope of the loop variable.
- [Fixing For Loops in Go 1.22](https://go.dev/blog/loopvar-preview) — the official explanation of the per-iteration loop-variable scope that makes the stage closures capture distinct values.
- [`errors` package](https://pkg.go.dev/errors) — `errors.Is` and the `%w` wrapping the `ErrExhausted` contract relies on.

---

Back to [02-grids-and-fixtures.md](02-grids-and-fixtures.md) | Next: [04-worker-pool-dispatcher.md](04-worker-pool-dispatcher.md)
