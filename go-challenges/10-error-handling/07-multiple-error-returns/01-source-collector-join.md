# Exercise 1: Aggregate Multi-Source Failures With errors.Join

A health checker that fans out to N upstream probes must not hide four failures
behind the first one. This exercise builds the core aggregation pattern: run a
slice of named sources, wrap each failure with its source name, and return one
`errors.Join` of all of them — a single error value the caller can still walk with
`errors.Is` to find each specific failure.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
collector/                 independent module: example.com/collector
  go.mod                   go 1.26
  collector.go             ErrSourceA/B/C sentinels; Source, Collector; Collect() error
  cmd/
    demo/
      main.go              runs three sources (one failing) and prints the aggregate
  collector_test.go        table tests: all-ok, all-fail, partial; -race
```

- Files: `collector.go`, `cmd/demo/main.go`, `collector_test.go`.
- Implement: a `Collector` holding `[]Source`, whose `Collect()` runs each source, wraps any error with `fmt.Errorf("source %q: %w", name, err)`, and returns `errors.Join(errs...)` (nil when all succeed).
- Test: all-success returns nil; all-fail is `errors.Is` each sentinel; partial failure is `errors.Is` only the failing sentinel and not the healthy ones.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/collector/cmd/demo
cd ~/go-exercises/collector
go mod init example.com/collector
```

### Why aggregate instead of returning the first error

The obvious loop returns on the first failing source. That is fail-fast, and for a
health checker it is wrong: the caller asked "which of my dependencies are down?"
and a fail-fast loop answers with exactly one name, forcing another probe cycle to
discover the next. Independent probes call for fail-complete — run all of them,
collect every failure, and hand back the full census in one value.

`errors.Join` is built for exactly this. You accumulate wrapped errors into a
`[]error` and pass it through `errors.Join(errs...)`. Two of its properties make
the code clean: it discards nil inputs, so you only append the failures; and it
returns nil when every input is nil, so the all-healthy case needs no special-case
branch — an empty (or all-nil) slice through `Join` is nil.

The wrapping is what makes the aggregate useful. `fmt.Errorf("source %q: %w", name, err)`
stamps the source name onto the error for the log *and*, thanks to `%w`, keeps the
underlying sentinel reachable by `errors.Is`. So `errors.Is(agg, ErrSourceB)` still
returns true through both the `fmt.Errorf` wrapper and the `Join` wrapper. That is
the wrap-then-join pattern: greppable message, machine-inspectable tree.

Create `collector.go`:

```go
package collector

import (
	"errors"
	"fmt"
)

// Sentinel errors each source can return. Callers match them with errors.Is,
// never with ==, because the collector double-wraps them (fmt.Errorf + Join).
var (
	ErrSourceA = errors.New("source A failed")
	ErrSourceB = errors.New("source B failed")
	ErrSourceC = errors.New("source C failed")
)

// Source is one named probe: a health check, an upstream fetch, a dependency ping.
type Source struct {
	Name string
	Run  func() error
}

// Collector fans out to a set of independent sources and aggregates their failures.
type Collector struct {
	Sources []Source
}

// Collect runs every source, wraps each failure with its source name, and returns
// errors.Join of the failures. It returns nil when every source succeeds.
func (c *Collector) Collect() error {
	var errs []error
	for _, s := range c.Sources {
		if err := s.Run(); err != nil {
			errs = append(errs, fmt.Errorf("source %q: %w", s.Name, err))
		}
	}
	return errors.Join(errs...)
}
```

### The runnable demo

The demo wires three sources where the middle one fails, then prints the aggregate
and probes it with `errors.Is` to show the sentinel survives the double wrapping.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/collector"
)

func main() {
	c := &collector.Collector{
		Sources: []collector.Source{
			{Name: "billing", Run: func() error { return nil }},
			{Name: "inventory", Run: func() error { return collector.ErrSourceB }},
			{Name: "shipping", Run: func() error { return nil }},
		},
	}

	err := c.Collect()
	if err == nil {
		fmt.Println("all sources healthy")
		return
	}

	fmt.Println("aggregate error:")
	fmt.Println(err)
	fmt.Println("is ErrSourceB:", errors.Is(err, collector.ErrSourceB))
	fmt.Println("is ErrSourceA:", errors.Is(err, collector.ErrSourceA))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
aggregate error:
source "inventory": source B failed
is ErrSourceB: true
is ErrSourceA: false
```

### Tests

The tests pin the three cases that matter. All-success must return untyped nil (not
a non-nil empty aggregate). All-fail must be `errors.Is` each of the three
sentinels through the wrapping. Partial failure must be `errors.Is` the one failing
sentinel and must *not* be `errors.Is` the healthy ones — proving the aggregate
carries exactly the failures that happened. The table drives a set of sources per
case and asserts on which sentinels are present.

Create `collector_test.go`:

```go
package collector

import (
	"errors"
	"fmt"
	"testing"
)

func src(name string, err error) Source {
	return Source{Name: name, Run: func() error { return err }}
}

func TestCollect(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		sources []Source
		wantNil bool
		present []error // sentinels that must be errors.Is
		absent  []error // sentinels that must NOT be errors.Is
	}{
		{
			name:    "all success",
			sources: []Source{src("a", nil), src("b", nil)},
			wantNil: true,
		},
		{
			name:    "all fail",
			sources: []Source{src("a", ErrSourceA), src("b", ErrSourceB), src("c", ErrSourceC)},
			present: []error{ErrSourceA, ErrSourceB, ErrSourceC},
		},
		{
			name:    "partial failure",
			sources: []Source{src("a", nil), src("b", ErrSourceB), src("c", nil)},
			present: []error{ErrSourceB},
			absent:  []error{ErrSourceA, ErrSourceC},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := &Collector{Sources: tt.sources}
			err := c.Collect()

			if tt.wantNil {
				if err != nil {
					t.Fatalf("Collect() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("Collect() = nil, want error")
			}
			for _, s := range tt.present {
				if !errors.Is(err, s) {
					t.Errorf("errors.Is(err, %v) = false, want true", s)
				}
			}
			for _, s := range tt.absent {
				if errors.Is(err, s) {
					t.Errorf("errors.Is(err, %v) = true, want false", s)
				}
			}
		})
	}
}

func Example() {
	c := &Collector{Sources: []Source{
		src("cache", ErrSourceA),
	}}
	fmt.Println(c.Collect())
	// Output: source "cache": source A failed
}
```

## Review

`Collect` is correct when the happy path returns untyped nil and every failure is
recoverable by `errors.Is` through both the `fmt.Errorf` and `Join` wrappers. The
partial-failure case is the load-bearing one: it proves the aggregate contains
exactly the sentinels that failed and nothing else, which is only true because
`errors.Join` skips the nil returns of the healthy sources. Do not return the first
error from the loop — that hides the rest and defeats the purpose. Do not compare
with `==`; the double wrapping makes identity comparison always fail, which is why
the tests use `errors.Is`. Run with `-race`; the sources here are synchronous, but
the habit matters once you fan them out concurrently (Exercise 9).

## Resources

- [errors.Join](https://pkg.go.dev/errors#Join) — nil-skipping, all-nil-is-nil, and the multi-line `Error()`.
- [Go Blog: Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — the `%w` verb and `errors.Is`/`errors.As`.
- [Go 1.20 release notes: errors](https://go.dev/doc/go1.20#errors) — multi-error wrapping and the tree walk.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-joined-error-message-shape.md](02-joined-error-message-shape.md)
