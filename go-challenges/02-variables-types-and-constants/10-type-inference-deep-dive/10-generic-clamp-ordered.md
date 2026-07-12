# Exercise 10: Clamp Worker-Pool Sizes With A Generic cmp.Ordered Helper

Bounding a configured value to a sane range is a one-liner you write in every
service: a worker count to `[1, NumCPU]`, a timeout to a floor and ceiling. You will
write a single generic `Clamp` over `cmp.Ordered` that works for ints, durations,
and any ordered type, with its type parameter inferred from the call site and no
explicit type argument in sight.

## What you'll build

```text
clamp/                      independent module: example.com/clamp
  go.mod                    go 1.26
  clamp.go                  Clamp[T cmp.Ordered]; ClampWorkers; ClampTimeout
  cmd/
    demo/
      main.go               clamps a worker count and a timeout
  clamp_test.go             int and time.Duration tables, inference pins, lo>hi
  clamp_example_test.go     Example with // Output
```

Files: `clamp.go`, `cmd/demo/main.go`, `clamp_test.go`, `clamp_example_test.go`.
Implement: `func Clamp[T cmp.Ordered](v, lo, hi T) T`, plus `ClampWorkers` and
`ClampTimeout` wrappers.
Test: `int` and `time.Duration` instantiations, inference pins
`var _ int = Clamp(v, 1, n)` and `var _ time.Duration = Clamp(d, lo, hi)`, and a
deterministic `lo > hi` case.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

## One function, every ordered type, inferred at the call site

`Clamp[T cmp.Ordered](v, lo, hi T) T` bounds `v` to `[lo, hi]`. The constraint
`cmp.Ordered` (Go 1.21) is the set of all types that support the `<` operators:
every integer and float kind, `string`, and any *defined* type over them — which is
why the same `Clamp` bounds an `int` worker count and a `time.Duration` timeout with
no overloads. The body is `max(lo, min(v, hi))` using the `min`/`max` builtins,
which are themselves defined for any ordered operands.

The inference story is the point. At a call site `Clamp(v, 1, n)` the compiler
deduces `T` from the arguments: `v` and `n` are `int`, so `T = int`, and the untyped
constant `1` conforms to it — you never write `Clamp[int](...)`. Mixing an untyped
constant with typed arguments is fine (the constant bends to `T`); mixing two
*typed* arguments of *different* types is not — `Clamp(d, 1, n)` with `d` a
`time.Duration` and `n` an `int` fails inference, because there is no single `T`
that is both. That failure is a feature: it stops you from clamping a duration
against an integer bound by accident.

There is one design decision to make explicit: what does `Clamp` do when `lo > hi`,
a contradictory range? `max(lo, min(v, hi))` handles it *deterministically* —
`min(v, hi)` is at most `hi`, which is below `lo`, so `max(lo, ...)` returns `lo`.
The function never panics or returns an out-of-range value; it resolves the
contradiction by favoring `lo`. The test pins that behavior so a future refactor
cannot change it silently.

Create `clamp.go`:

```go
package clamp

import (
	"cmp"
	"runtime"
	"time"
)

// Clamp bounds v to the inclusive range [lo, hi]. T is inferred from the call
// site. If lo > hi (a contradictory range), Clamp deterministically returns lo.
func Clamp[T cmp.Ordered](v, lo, hi T) T {
	return max(lo, min(v, hi))
}

// ClampWorkers bounds a configured worker count to [1, runtime.NumCPU()]. T is
// inferred as int; the untyped constant 1 conforms to it.
func ClampWorkers(configured int) int {
	return Clamp(configured, 1, runtime.NumCPU())
}

// ClampTimeout bounds a timeout to a sane [min, max] range. T is inferred as
// time.Duration.
func ClampTimeout(d, lo, hi time.Duration) time.Duration {
	return Clamp(d, lo, hi)
}
```

## Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/clamp"
)

func main() {
	// A configured worker count of 1000 is clamped to the CPU count; 0 is
	// clamped up to 1. NumCPU varies by machine, so clamp against a fixed hi.
	fmt.Printf("clamp 1000 to [1,8] = %d\n", clamp.Clamp(1000, 1, 8))
	fmt.Printf("clamp 0 to [1,8]    = %d\n", clamp.Clamp(0, 1, 8))
	fmt.Printf("clamp 4 to [1,8]    = %d\n", clamp.Clamp(4, 1, 8))

	to := clamp.ClampTimeout(90*time.Second, time.Second, 30*time.Second)
	fmt.Printf("clamp 90s to [1s,30s] = %s\n", to)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
clamp 1000 to [1,8] = 8
clamp 0 to [1,8]    = 1
clamp 4 to [1,8]    = 4
clamp 90s to [1s,30s] = 30s
```

## Tests

The tables exercise `Clamp` over both `int` and `time.Duration`, the pins prove `T`
is inferred correctly at each call, and `TestClampInvertedRange` locks in the
`lo > hi` behavior.

Create `clamp_test.go`:

```go
package clamp

import (
	"testing"
	"time"
)

func TestClampInt(t *testing.T) {
	t.Parallel()

	n := 8
	var _ int = Clamp(4, 1, n) // pin: T inferred as int

	cases := []struct {
		v, lo, hi, want int
	}{
		{4, 1, 8, 4},
		{0, 1, 8, 1},
		{1000, 1, 8, 8},
		{1, 1, 8, 1},
		{8, 1, 8, 8},
	}
	for _, c := range cases {
		if got := Clamp(c.v, c.lo, c.hi); got != c.want {
			t.Errorf("Clamp(%d,%d,%d) = %d, want %d", c.v, c.lo, c.hi, got, c.want)
		}
	}
}

func TestClampDuration(t *testing.T) {
	t.Parallel()

	lo, hi := time.Second, 30*time.Second
	var _ time.Duration = Clamp(5*time.Second, lo, hi) // pin: T inferred as time.Duration

	cases := []struct {
		v, want time.Duration
	}{
		{5 * time.Second, 5 * time.Second},
		{90 * time.Second, 30 * time.Second},
		{time.Millisecond, time.Second},
	}
	for _, c := range cases {
		if got := Clamp(c.v, lo, hi); got != c.want {
			t.Errorf("Clamp(%s) = %s, want %s", c.v, got, c.want)
		}
	}
}

func TestClampInvertedRange(t *testing.T) {
	t.Parallel()

	// lo > hi is contradictory; Clamp resolves it deterministically to lo.
	if got := Clamp(5, 10, 1); got != 10 {
		t.Fatalf("Clamp(5,10,1) = %d, want 10 (lo wins on inverted range)", got)
	}
}

func TestClampWorkers(t *testing.T) {
	t.Parallel()

	if got := ClampWorkers(0); got < 1 {
		t.Fatalf("ClampWorkers(0) = %d, want >= 1", got)
	}
	if got := ClampWorkers(1 << 20); got < 1 {
		t.Fatalf("ClampWorkers(large) = %d, want >= 1", got)
	}
}
```

Create `clamp_example_test.go`:

```go
package clamp

import "fmt"

func ExampleClamp() {
	fmt.Println(Clamp(1000, 1, 8))
	fmt.Println(Clamp(0, 1, 8))
	fmt.Println(Clamp("m", "a", "z"))
	// Output:
	// 8
	// 1
	// m
}
```

## Review

`Clamp` is correct when it bounds any `cmp.Ordered` value with `max(lo, min(v, hi))`
and lets the compiler infer `T` at each call. The inference rules are the lesson:
`Clamp(v, 1, n)` deduces `int` and the untyped `1` conforms, while `Clamp(d, 1, n)`
mixing a `time.Duration` and an `int` fails to compile — a guard against clamping
one quantity against a bound of a different type. The `lo > hi` case is resolved
deterministically to `lo`; that is a defined behavior the test pins, not an
accident. The `string` line in the example is there to make one thing vivid:
`cmp.Ordered` spans strings too, so the exact same function orders text.

## Resources

- [cmp.Ordered](https://pkg.go.dev/cmp#Ordered) — the constraint of all ordered types.
- [Go Specification: min and max builtins](https://go.dev/ref/spec#Min_and_max) — how `min`/`max` work over ordered operands.
- [The Go Blog: type inference](https://go.dev/blog/type-inference) — deducing type parameters from arguments.

---

Back to [09-json-number-float64-boundary.md](09-json-number-float64-boundary.md) | Next: [../../03-control-flow/01-if-else-and-init-statements/01-if-else-and-init-statements.md](../../03-control-flow/01-if-else-and-init-statements/01-if-else-and-init-statements.md)
