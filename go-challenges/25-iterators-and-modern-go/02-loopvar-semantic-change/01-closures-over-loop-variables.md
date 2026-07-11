# Exercise 1: Closures Over Loop Variables

The clearest way to see Go 1.22's per-iteration scope is to build a slice of closures inside a loop and call them after the loop ends. Under the old per-loop semantics they would all return the final value; under the new semantics each returns its own iteration's value, with no defensive copy. This exercise builds that proof for both the range form and the three-clause `for` form.

This module is fully self-contained. It begins with its own `go mod init`, defines every function it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
closures.go          NameClosures, IndexClosures, generic Evaluate, ErrEmptyInput
cmd/
  demo/
    main.go          build closures in a loop, call them after the loop, print results
closures_test.go     assert each closure returns its own per-iteration value
example_test.go      ExampleNameClosures with a verified // Output block
```

- Files: `closures.go`, `cmd/demo/main.go`, `closures_test.go`, `example_test.go`.
- Implement: `NameClosures([]string) ([]func() string, error)`, `IndexClosures(int) ([]func() int, error)`, and the generic `Evaluate[T any]([]func() T) []T`, plus the sentinel `ErrEmptyInput`.
- Test: build closures for three names and four indices, evaluate them after the loop, and assert the full ordered result rather than any single value.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p loop-closures/cmd/demo && cd loop-closures
go mod init example.com/loop-closures
```

### Why the direct capture is now correct

A closure created inside a loop captures the loop variable by reference. The whole question is how many distinct variables the loop creates. Under the pre-1.22 per-loop rule there was exactly one `name` for the entire range loop, so every closure captured the same reference; when they were finally called after the loop, they all read the last element. The fix programmers reached for was a shadowing redeclaration, `name := name`, as the first line of the body, which created a fresh block-scoped variable per iteration for the closure to capture.

Under Go 1.22 the loop variable is *already* fresh per iteration, so the direct form `func() string { return name }` captures a different `name` each time and the shadow assignment is redundant. The exercise deliberately writes the closures with no `name := name` line to make the point: in a module declaring `go 1.26`, the body you would naively write is the correct one.

The three-clause form `for i := 0; i < n; i++` gets the same treatment, which surprises people who assume the change only touched `range`. Each iteration receives its own `i`, seeded from the previous iteration's value, run through the condition and post-statement, and copied forward. `IndexClosures` captures that per-iteration `i`, so the returned closures yield `0, 1, 2, 3` rather than four copies of `n`.

`Evaluate` is generic over the closure's return type so one helper drives both demonstrations. It calls each function in order and collects the results, which is what lets the tests assert the entire ordered sequence — the property that actually distinguishes per-iteration scope from per-loop scope. Checking only that a result is non-empty would pass under both the buggy and the fixed semantics; checking the full ordered slice is what pins the behavior.

Create `closures.go`:

```go
package loopvar

import (
	"errors"
	"fmt"
)

// ErrEmptyInput is returned by a builder given nothing to iterate over.
var ErrEmptyInput = errors.New("input must not be empty")

// NameClosures returns one closure per name, each capturing that iteration's
// value. In a module declaring go 1.22 or later the loop variable is
// per-iteration scoped, so each closure returns its own name and no
// "name := name" shadow assignment is needed.
func NameClosures(names []string) ([]func() string, error) {
	if len(names) == 0 {
		return nil, fmt.Errorf("name closures: %w", ErrEmptyInput)
	}

	funcs := make([]func() string, 0, len(names))
	for _, name := range names {
		funcs = append(funcs, func() string {
			return name
		})
	}
	return funcs, nil
}

// IndexClosures returns one closure per index using the three-clause for form,
// proving the per-iteration rule also applies to for i := 0; i < n; i++ and not
// only to range loops.
func IndexClosures(n int) ([]func() int, error) {
	if n <= 0 {
		return nil, fmt.Errorf("index closures: %w", ErrEmptyInput)
	}

	funcs := make([]func() int, 0, n)
	for i := 0; i < n; i++ {
		funcs = append(funcs, func() int {
			return i
		})
	}
	return funcs, nil
}

// Evaluate calls every closure once, in order, and collects the results.
func Evaluate[T any](funcs []func() T) []T {
	values := make([]T, 0, len(funcs))
	for _, fn := range funcs {
		values = append(values, fn())
	}
	return values
}
```

### The runnable demo

The demo builds both kinds of closure inside their loops and calls them only after the loops have finished, which is exactly the timing that exposed the old bug. Under per-iteration scope each closure has kept its own value.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/loop-closures"
)

func main() {
	names, err := loopvar.NameClosures([]string{"alpha", "beta", "gamma"})
	if err != nil {
		fmt.Println("name closures error:", err)
		return
	}
	fmt.Println(loopvar.Evaluate(names))

	idx, err := loopvar.IndexClosures(4)
	if err != nil {
		fmt.Println("index closures error:", err)
		return
	}
	fmt.Println(loopvar.Evaluate(idx))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
[alpha beta gamma]
[0 1 2 3]
```

Under the old semantics the first line would have printed `[gamma gamma gamma]` and the second `[4 4 4 4]`.

### Tests

The tests assert the full ordered result of evaluating the closures after the loop, which is the property per-iteration scope guarantees. `TestEmptyInputs` checks that both builders wrap `ErrEmptyInput` with `%w` so callers can match with `errors.Is`.

Create `closures_test.go`:

```go
package loopvar

import (
	"errors"
	"reflect"
	"testing"
)

func TestNameClosuresCaptureEachIteration(t *testing.T) {
	t.Parallel()

	funcs, err := NameClosures([]string{"alpha", "beta", "gamma"})
	if err != nil {
		t.Fatal(err)
	}
	got := Evaluate(funcs)
	want := []string{"alpha", "beta", "gamma"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Evaluate = %v, want %v", got, want)
	}
}

func TestIndexClosuresCaptureEachIteration(t *testing.T) {
	t.Parallel()

	funcs, err := IndexClosures(4)
	if err != nil {
		t.Fatal(err)
	}
	got := Evaluate(funcs)
	want := []int{0, 1, 2, 3}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Evaluate = %v, want %v", got, want)
	}
}

func TestEmptyInputs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		call func() error
	}{
		{name: "names", call: func() error { _, err := NameClosures(nil); return err }},
		{name: "index", call: func() error { _, err := IndexClosures(0); return err }},
	}

	for _, tt := range tests {
		if err := tt.call(); !errors.Is(err, ErrEmptyInput) {
			t.Fatalf("%s error = %v, want ErrEmptyInput", tt.name, err)
		}
	}
}
```

Create `example_test.go`:

```go
package loopvar

import "fmt"

func ExampleNameClosures() {
	funcs, _ := NameClosures([]string{"alpha", "beta", "gamma"})
	fmt.Println(Evaluate(funcs))
	// Output: [alpha beta gamma]
}
```

## Review

The behavior is correct when the closures, called after their loops complete, return the per-iteration values `[alpha beta gamma]` and `[0 1 2 3]` rather than three or four copies of the final value. Confirm the bodies contain no `name := name` or `i := i` shadow line — in a `go 1.26` module the direct capture is already correct, and adding the shadow only implies to a reader that the variable is still shared. Note that the test asserts the entire ordered slice, not a single element or a non-empty check, because only the full sequence distinguishes per-iteration from per-loop scope; a weaker assertion would pass under the old buggy semantics too.

The common mistake here is the reflex to copy the loop variable defensively. It is harmless to behavior but it is dead code in current Go, and it is the single most frequent fossil left over from pre-1.22 tutorials, especially the `tc := tc` line in `t.Parallel()` subtests. The second mistake is assuming the change only touched `range`; `IndexClosures` exists to show the three-clause form gets fresh per-iteration variables too.

## Resources

- [Fixing For Loops in Go 1.22](https://go.dev/blog/loopvar-preview) — the official explanation of the per-iteration change, the bugs it fixes, and why it was safe to make.
- [Go Wiki: LoopvarExperiment](https://go.dev/wiki/LoopvarExperiment) — the experiment that shipped the change, including how the language version in `go.mod` selects the behavior.
- [Go Spec: For statements](https://go.dev/ref/spec#For_statements) — the normative rules for `for` loops and how iteration variables are declared.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-goroutines-in-loops.md](02-goroutines-in-loops.md)
