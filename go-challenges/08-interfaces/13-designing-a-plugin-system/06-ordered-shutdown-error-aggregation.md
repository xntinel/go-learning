# Exercise 6: Graceful Shutdown: Reverse Order and Aggregated Errors

Production shutdown tears plugins down in reverse registration order, bounds each
`Shutdown` with a context, and does not stop at the first failure. Every error is
collected with `errors.Join` so the operator sees all of them — the difference
between a clean SIGTERM and a hung process that hides half its failures.

This module is fully self-contained. It has its own `go mod init`, defines every
type it needs, and ships its own demo and tests. Nothing here imports any other
exercise.

## What you'll build

```text
gshutdown/                independent module: example.com/gshutdown
  go.mod                  go 1.25
  registry.go             Registry tracks order; Shutdown reverse-iterates, joins errors
  cmd/
    demo/
      main.go             register A,B,C; shut down; observe reverse order
  registry_test.go        reverse-order, aggregation, and all-succeed tests
```

- Files: `registry.go`, `cmd/demo/main.go`, `registry_test.go`.
- Implement: a `Registry` that records registration order and a `Shutdown(ctx)` that iterates in reverse, bounds each plugin's `Shutdown` with a per-plugin context, continues past failures, and returns `errors.Join` of every error (nil when all succeed).
- Test: register A,B,C and assert the shutdown order is C,B,A; make B and C fail and assert the joined error satisfies `errors.Is` for both sentinels while A still ran; assert nil when all succeed.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/08-interfaces/13-designing-a-plugin-system/06-ordered-shutdown-error-aggregation/cmd/demo
cd go-solutions/08-interfaces/13-designing-a-plugin-system/06-ordered-shutdown-error-aggregation
go mod edit -go=1.25
```

### Why reverse order, and why not stop at the first error

Plugins registered later often depend on ones registered earlier — a metrics
exporter registered after the database wants the database still alive while it
flushes. So teardown is last-in-first-out: reverse the registration order and shut
each down in turn. This mirrors how `defer` unwinds and how any dependency graph
is torn down safely.

The second property is failure tolerance. A naive `Shutdown` returns as soon as one
plugin errors — and then the plugins after it in the teardown order never get
their `Shutdown` called at all, leaking exactly the resources shutdown exists to
release. Correct shutdown runs every plugin's `Shutdown` regardless of earlier
failures and collects the errors. `errors.Join(errs...)` is built for this: it
returns a single error that wraps all the non-nil ones, whose `Error()`
newline-joins their messages, and against which `errors.Is` reports true for *any*
of the joined sentinels. The caller gets the complete failure picture in one
value.

Each `Shutdown` is bounded by its own context so one plugin that hangs cannot block
the whole process from exiting. In this module we derive a per-plugin
`context.WithTimeout` from the shutdown context; a plugin that respects it returns
promptly, and even one that ignores it can be surfaced as a timeout by a real host
(here we keep the plugin cooperative and focus on ordering and aggregation).

The registry records order in a slice appended on `Register`, separate from the
map, so reverse iteration is `order[len-1] ... order[0]`. `slices.Clone` +
`slices.Reverse` gives a reversed copy without mutating the stored order.

Create `registry.go`:

```go
package gshutdown

import (
	"context"
	"errors"
	"slices"
	"time"
)

// Plugin can be shut down under a context.
type Plugin interface {
	Name() string
	Shutdown(ctx context.Context) error
}

// Registry records plugins in registration order so Shutdown can reverse it.
type Registry struct {
	order          []string
	plugins        map[string]Plugin
	perPluginLimit time.Duration
}

// NewRegistry returns a registry that bounds each plugin's Shutdown with limit.
func NewRegistry(limit time.Duration) *Registry {
	return &Registry{plugins: make(map[string]Plugin), perPluginLimit: limit}
}

// Register stores p and remembers its position in registration order.
func (r *Registry) Register(p Plugin) {
	if _, ok := r.plugins[p.Name()]; !ok {
		r.order = append(r.order, p.Name())
	}
	r.plugins[p.Name()] = p
}

// Shutdown tears every plugin down in reverse registration order, bounding each
// with a per-plugin context, continuing past failures, and returning the joined
// errors of all that failed (nil when every plugin shuts down cleanly).
func (r *Registry) Shutdown(ctx context.Context) error {
	rev := slices.Clone(r.order)
	slices.Reverse(rev)

	var errs []error
	for _, name := range rev {
		pctx, cancel := context.WithTimeout(ctx, r.perPluginLimit)
		err := r.plugins[name].Shutdown(pctx)
		cancel()
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
```

### The runnable demo

The demo registers three plugins that each record the moment they are shut down
into a shared order log, so the reverse teardown is visible. Two of them return
errors, and the demo prints the joined error to show all failures surface at once.
Give the registry a real per-plugin budget (not `0`, which would make each
`context.WithTimeout` expire immediately).

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"example.com/gshutdown"
)

type recorder struct {
	name string
	log  *[]string
	fail error
}

func (p *recorder) Name() string { return p.name }

func (p *recorder) Shutdown(_ context.Context) error {
	*p.log = append(*p.log, p.name)
	return p.fail
}

func main() {
	var order []string
	r := gshutdown.NewRegistry(2 * time.Second)

	r.Register(&recorder{name: "A", log: &order})
	r.Register(&recorder{name: "B", log: &order, fail: errors.New("B flush failed")})
	r.Register(&recorder{name: "C", log: &order, fail: errors.New("C close failed")})

	err := r.Shutdown(context.Background())
	fmt.Println("order:", order)
	fmt.Println("err:", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
order: [C B A]
err: C close failed
B flush failed
```

The order log is `[C B A]` — reverse of registration — and both failures appear in
the joined error, C's before B's because teardown reached C first. A ran even
though C and B failed.

### Tests

`TestShutdownReverseOrder` registers A,B,C and asserts the recorded teardown order
is C,B,A. `TestShutdownAggregatesErrors` makes B and C fail with distinct sentinel
errors and asserts the joined result satisfies `errors.Is` for both, and that A
(which succeeds) still ran. `TestShutdownAllSucceedIsNil` asserts a clean shutdown
returns nil.

Create `registry_test.go`:

```go
package gshutdown

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

var (
	errB = errors.New("B failed")
	errC = errors.New("C failed")
)

type recorder struct {
	name string
	log  *[]string
	fail error
}

func (p *recorder) Name() string { return p.name }

func (p *recorder) Shutdown(_ context.Context) error {
	*p.log = append(*p.log, p.name)
	return p.fail
}

func TestShutdownReverseOrder(t *testing.T) {
	t.Parallel()

	var log []string
	r := NewRegistry(time.Second)
	r.Register(&recorder{name: "A", log: &log})
	r.Register(&recorder{name: "B", log: &log})
	r.Register(&recorder{name: "C", log: &log})

	if err := r.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown err = %v, want nil", err)
	}
	if want := []string{"C", "B", "A"}; !slices.Equal(log, want) {
		t.Fatalf("shutdown order = %v, want %v", log, want)
	}
}

func TestShutdownAggregatesErrors(t *testing.T) {
	t.Parallel()

	var log []string
	r := NewRegistry(time.Second)
	r.Register(&recorder{name: "A", log: &log})
	r.Register(&recorder{name: "B", log: &log, fail: errB})
	r.Register(&recorder{name: "C", log: &log, fail: errC})

	err := r.Shutdown(context.Background())
	if !errors.Is(err, errB) {
		t.Fatalf("joined err %v does not wrap errB", err)
	}
	if !errors.Is(err, errC) {
		t.Fatalf("joined err %v does not wrap errC", err)
	}
	if !slices.Contains(log, "A") {
		t.Fatalf("A did not run despite B/C failing: %v", log)
	}
}

func TestShutdownAllSucceedIsNil(t *testing.T) {
	t.Parallel()

	var log []string
	r := NewRegistry(time.Second)
	r.Register(&recorder{name: "A", log: &log})
	r.Register(&recorder{name: "B", log: &log})

	if err := r.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown err = %v, want nil", err)
	}
}
```

## Review

Correct shutdown has three properties, each pinned by a test: reverse order
(C,B,A), failure tolerance (A runs even when C and B fail), and complete
aggregation (the returned error satisfies `errors.Is` for every failing plugin).
The bug this guards against is the early return — stopping at the first failing
`Shutdown` skips every plugin after it in the teardown order, leaking the
resources shutdown was supposed to release. `errors.Join` is the right tool: it
returns nil when the slice is all-nil (so the clean case is nil automatically) and
a single wrapping error otherwise. Bound each plugin's `Shutdown` with its own
context so one hung plugin cannot block the process from exiting.

## Resources

- [errors.Join](https://pkg.go.dev/errors#Join) — aggregating multiple shutdown errors into one that `errors.Is` can match against each.
- [slices.Reverse](https://pkg.go.dev/slices#Reverse) — reversing the registration order for teardown.
- [context.WithTimeout](https://pkg.go.dev/context#WithTimeout) — bounding each plugin's shutdown so one hang cannot block exit.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [05-context-aware-processing.md](05-context-aware-processing.md) | Next: [07-decorator-middleware-plugins.md](07-decorator-middleware-plugins.md)
