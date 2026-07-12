# Exercise 4: Aggregate a fan-out to N backends with multiple %w for partial failure

A health check runs several dependency probes and must report a single verdict
that a caller can reason about two ways at once: "is the service degraded?" and
"was it a timeout specifically?". This exercise builds a `CheckAll` that, on any
failure, returns one `fmt.Errorf` wrapping *both* a `ErrDegraded` context sentinel
and the aggregated cause(s) using multiple `%w`, so `errors.Is` finds either. It
also demonstrates the asymmetry that makes `errors.Unwrap` return `nil` on this
error even though `errors.Is` traverses it fully.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
health/                        independent module: example.com/health
  go.mod                       go 1.24
  health.go                    ErrDegraded, ErrTimeout; Check; CheckAll with multiple %w
  health_test.go               all-pass -> nil; one timeout -> Is finds both; Unwrap asymmetry
  cmd/
    demo/
      main.go                  three probes, one of them times out
```

- Files: `health.go`, `cmd/demo/main.go`, `health_test.go`.
- Implement: `CheckAll(checks []Check)` that runs each probe; on any failure returns `fmt.Errorf("%w: %w", ErrDegraded, errors.Join(failures...))`, naming each failing dependency.
- Test: all pass returns `nil`; one timeout makes `errors.Is(err, ErrDegraded)` and `errors.Is(err, ErrTimeout)` both true; `errors.Unwrap(err)` returns `nil` (the `Unwrap() []error` form); the message lists the failing dependency.
- Verify: `go test -count=1 -race ./...`

### One error, two reachable sentinels

Each `Check` is a named probe: a `Name` and a `Run func() error`. `CheckAll`
collects the failures, wrapping each with its dependency name so the aggregate
message says which backend failed. If nothing failed it returns `nil`. Otherwise
it builds the aggregate with a single call carrying two `%w` operands:

```go
return fmt.Errorf("%w: %w", ErrDegraded, errors.Join(failures...))
```

The first `%w` is the context sentinel `ErrDegraded` — the verdict a load balancer
or an alert rule keys on. The second `%w` is `errors.Join(failures...)`, which
itself is an `Unwrap() []error` tree of the individual named failures, each of
which wraps its underlying cause (such as `ErrTimeout`). Because a two-`%w`
`fmt.Errorf` yields an `Unwrap() []error`, both operands are reachable by the
depth-first walk that `errors.Is` performs. So `errors.Is(err, ErrDegraded)` finds
the first operand, and `errors.Is(err, ErrTimeout)` descends through the joined
failures to the cause. One value answers both questions.

The instructive asymmetry: `errors.Unwrap(err)` returns `nil` here. `errors.Unwrap`
only calls the single `Unwrap() error` method, which this multi-`%w` error does
not implement — it implements `Unwrap() []error`. A caller who tried to inspect
this error with a manual `errors.Unwrap` loop would see `nil` on the first step and
wrongly conclude there is no cause. The tree-walking `errors.Is`/`errors.As` are
the only correct way to inspect it.

Create `health.go`:

```go
package health

import (
	"errors"
	"fmt"
)

// ErrDegraded is the verdict sentinel; ErrTimeout is one possible cause.
var (
	ErrDegraded = errors.New("service degraded")
	ErrTimeout  = errors.New("dependency timeout")
)

// Check is one named dependency probe.
type Check struct {
	Name string
	Run  func() error
}

// CheckAll runs every probe. On any failure it returns a single error that wraps
// both the ErrDegraded verdict and the aggregated causes with multiple %w, so a
// caller can errors.Is the result against ErrDegraded and against a specific cause.
func CheckAll(checks []Check) error {
	var failures []error
	for _, c := range checks {
		if err := c.Run(); err != nil {
			failures = append(failures, fmt.Errorf("dependency %s: %w", c.Name, err))
		}
	}
	if len(failures) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrDegraded, errors.Join(failures...))
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/health"
)

func main() {
	checks := []health.Check{
		{Name: "postgres", Run: func() error { return nil }},
		{Name: "redis", Run: func() error { return health.ErrTimeout }},
		{Name: "s3", Run: func() error { return nil }},
	}

	err := health.CheckAll(checks)
	fmt.Printf("verdict: %v\n", err)
	fmt.Printf("is ErrDegraded=%v\n", errors.Is(err, health.ErrDegraded))
	fmt.Printf("is ErrTimeout=%v\n", errors.Is(err, health.ErrTimeout))
	fmt.Printf("errors.Unwrap == nil: %v\n", errors.Unwrap(err) == nil)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
verdict: service degraded: dependency redis: dependency timeout
is ErrDegraded=true
is ErrTimeout=true
errors.Unwrap == nil: true
```

### Tests

The core test proves both `%w` operands are reachable through the single value,
and a dedicated test pins the `errors.Unwrap` asymmetry so nobody "simplifies" the
inspection into a broken manual loop.

Create `health_test.go`:

```go
package health

import (
	"errors"
	"strings"
	"testing"
)

func TestCheckAllAllPass(t *testing.T) {
	t.Parallel()

	err := CheckAll([]Check{
		{Name: "a", Run: func() error { return nil }},
		{Name: "b", Run: func() error { return nil }},
	})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
}

func TestCheckAllPartialFailureReachesBothSentinels(t *testing.T) {
	t.Parallel()

	err := CheckAll([]Check{
		{Name: "postgres", Run: func() error { return nil }},
		{Name: "redis", Run: func() error { return ErrTimeout }},
	})

	if !errors.Is(err, ErrDegraded) {
		t.Fatalf("err = %v, want errors.Is ErrDegraded", err)
	}
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want errors.Is ErrTimeout through the tree walk", err)
	}
	if !strings.Contains(err.Error(), "redis") {
		t.Fatalf("err.Error() = %q, want the failing dependency name", err.Error())
	}
}

func TestCheckAllUnwrapAsymmetry(t *testing.T) {
	t.Parallel()

	err := CheckAll([]Check{
		{Name: "redis", Run: func() error { return ErrTimeout }},
	})

	// A multiple-%w error implements Unwrap() []error, not Unwrap() error, so
	// errors.Unwrap returns nil even though errors.Is traverses it.
	if errors.Unwrap(err) != nil {
		t.Fatal("errors.Unwrap should return nil for a multiple-%w error")
	}
	if !errors.Is(err, ErrTimeout) {
		t.Fatal("errors.Is must still find the cause through Unwrap() []error")
	}
}
```

## Review

`CheckAll` is correct when a single returned value answers both "degraded?" and
"which cause?" through `errors.Is`, and when it collapses to `nil` on all-pass.
The multiple-`%w` form is the right tool precisely because there are two things a
caller wants to key on at once — the verdict and the specific failure — and a
single-`%w` chain could carry only one of them at the top. The asymmetry test is
not busywork: the most common way to break this is for someone to "inspect the
cause" with `errors.Unwrap` in a loop, get `nil`, and add a spurious fallback
branch. Pinning `errors.Unwrap(err) == nil` alongside `errors.Is(err, ErrTimeout)
== true` documents in code that the tree-walking helpers are the only correct
inspection path for a joined or multi-wrapped error.

## Resources

- [fmt.Errorf](https://pkg.go.dev/fmt#Errorf) — multiple `%w` and `Unwrap() []error`.
- [errors.Join](https://pkg.go.dev/errors#Join) — aggregating independent failures.
- [errors package](https://pkg.go.dev/errors) — `Is`/`As` tree walk versus `Unwrap`.
- [Go 1.20 release notes: errors](https://go.dev/doc/go1.20#errors) — the introduction of multiple-error wrapping.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [03-config-loader-w-vs-v.md](03-config-loader-w-vs-v.md) | Next: [05-validation-join-accumulate.md](05-validation-join-accumulate.md)
