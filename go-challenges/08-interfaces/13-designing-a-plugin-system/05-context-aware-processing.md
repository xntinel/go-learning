# Exercise 5: Context-Aware Process with Cancellation and Deadlines

Upgrade the `Process` signature to take a `context.Context` so the host can bound
and cancel plugin work. The registry wraps each call in `context.WithTimeout`, and
plugins must actually observe `ctx.Done()` — a context that is passed but ignored
gives false safety.

This module is fully self-contained. It has its own `go mod init`, defines every
type it needs, and ships its own demo and tests. Nothing here imports any other
exercise.

## What you'll build

```text
ctxplugin/                independent module: example.com/ctxplugin
  go.mod                  go 1.25
  registry.go             Plugin.Process(ctx, input); Registry.Run wraps WithTimeout
  cmd/
    demo/
      main.go             a fast plugin succeeds, a slow one hits the deadline
  registry_test.go        DeadlineExceeded and Canceled assertions with t.Context()
```

- Files: `registry.go`, `cmd/demo/main.go`, `registry_test.go`.
- Implement: `Process(ctx context.Context, input string) (string, error)`; a `Registry` whose `Run` derives a per-call `context.WithTimeout` and cancels it; plugins that select on `ctx.Done()`.
- Test: a slow plugin yields `context.DeadlineExceeded` when the per-call timeout elapses and `context.Canceled` when the parent context is cancelled; a fast plugin returns before the deadline. Parent is `t.Context()`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.25
```

### The host bounds, the plugin observes

`Process(input string)` gives the host no lever over a slow plugin. If a plugin
blocks for a minute, the request that called it blocks for a minute. Threading a
`context.Context` through — `Process(ctx, input)` — gives the host two powers: it
can attach a deadline so one plugin cannot exceed a request's latency budget, and
it can cancel all in-flight plugin work when the request is abandoned or the server
shuts down.

The registry supplies the bound. `Run` derives a per-call context with
`context.WithTimeout(parent, r.timeout)` and, critically, `defer cancel()` to
release the timer whether the plugin returns early or late — a `WithTimeout` whose
`cancel` is never called leaks the timer until it fires. The derived context is
passed to `Process`.

But the bound is inert unless the plugin cooperates. A plugin that accepts `ctx`
and then does a blocking operation without watching `ctx.Done()` will run to
completion regardless of the deadline — the context fired, but nothing was
listening. So a well-behaved plugin selects on `ctx.Done()` at every point it
might block and returns `ctx.Err()` when it fires. `ctx.Err()` reports *why* the
context ended: `context.DeadlineExceeded` if the timeout elapsed,
`context.Canceled` if a parent cancel propagated. Both are sentinel errors the
caller asserts with `errors.Is`.

Create `registry.go`:

```go
package ctxplugin

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned by Run for an unknown plugin name.
var ErrNotFound = errors.New("plugin not found")

// Plugin processes input under a context. Implementations must observe
// ctx.Done() at any blocking point and return ctx.Err() when it fires.
type Plugin interface {
	Name() string
	Process(ctx context.Context, input string) (string, error)
}

// Registry runs plugins by name, bounding each call with a per-call timeout.
type Registry struct {
	timeout time.Duration
	plugins map[string]Plugin
}

// NewRegistry returns a registry that bounds every Run with timeout.
func NewRegistry(timeout time.Duration) *Registry {
	return &Registry{timeout: timeout, plugins: make(map[string]Plugin)}
}

// Register stores p under p.Name().
func (r *Registry) Register(p Plugin) {
	r.plugins[p.Name()] = p
}

// Run processes input through the named plugin under a context derived from
// parent with the registry's timeout. It returns ctx.Err()
// (context.DeadlineExceeded or context.Canceled) when the bound fires and the
// plugin honors it.
func (r *Registry) Run(parent context.Context, name, input string) (string, error) {
	p, ok := r.plugins[name]
	if !ok {
		return "", ErrNotFound
	}
	ctx, cancel := context.WithTimeout(parent, r.timeout)
	defer cancel()
	return p.Process(ctx, input)
}
```

### Two plugins: one fast, one slow

The `fast` plugin returns immediately and never blocks, so it always beats the
deadline. The `slow` plugin simulates real blocking work — it waits on a timer
that outlasts any test timeout — but does so inside a `select` that also watches
`ctx.Done()`, so it returns `ctx.Err()` the instant the context ends instead of
running to completion.

Create these in `registry.go` too. Append to `registry.go`:

```go
// fast returns immediately; it never blocks and so never trips the deadline.
type fast struct{}

func (fast) Name() string { return "fast" }

func (fast) Process(_ context.Context, input string) (string, error) {
	return "fast:" + input, nil
}

// slow blocks on a long timer but honors cancellation: it returns ctx.Err() the
// moment the context is done, rather than waiting out the timer.
type slow struct {
	work time.Duration
}

func (s slow) Name() string { return "slow" }

func (s slow) Process(ctx context.Context, input string) (string, error) {
	select {
	case <-time.After(s.work):
		return "slow:" + input, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}
```

### The runnable demo

The demo runs the fast plugin (succeeds well within the bound) and the slow plugin
(whose work outlasts the bound, so it returns the deadline error). It uses a real,
short timeout so you can watch the deadline fire against the wall clock.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"example.com/ctxplugin"
)

func main() {
	r := ctxplugin.NewRegistry(50 * time.Millisecond)
	r.Register(ctxplugin.NewFast())
	r.Register(ctxplugin.NewSlow(time.Hour)) // work far exceeds the 50ms bound

	out, err := r.Run(context.Background(), "fast", "hi")
	fmt.Printf("fast: out=%q err=%v\n", out, err)

	_, err = r.Run(context.Background(), "slow", "hi")
	fmt.Printf("slow: deadline exceeded = %v\n", errors.Is(err, context.DeadlineExceeded))
}
```

For the demo to build the plugins it needs exported constructors, since `fast` and
`slow` are unexported. Append to `registry.go`:

```go
// NewFast returns the fast sample plugin.
func NewFast() Plugin { return fast{} }

// NewSlow returns the slow sample plugin whose Process blocks for work unless
// the context is cancelled first.
func NewSlow(work time.Duration) Plugin { return slow{work: work} }
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
fast: out="fast:hi" err=<nil>
slow: deadline exceeded = true
```

### Tests

`TestFastPluginBeatsDeadline` asserts the fast plugin returns a value and no
error. `TestSlowPluginHitsDeadline` gives the registry a short timeout and a slow
plugin whose work never completes, then asserts `errors.Is(err, context.DeadlineExceeded)`.
`TestParentCancelPropagates` cancels the parent context before `Run` and asserts
`errors.Is(err, context.Canceled)` — a cancelled parent propagates through the
derived timeout context. The parent context in each test is `t.Context()`.

Create `registry_test.go`:

```go
package ctxplugin

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestFastPluginBeatsDeadline(t *testing.T) {
	t.Parallel()

	r := NewRegistry(time.Second)
	r.Register(fast{})

	out, err := r.Run(t.Context(), "fast", "x")
	if err != nil {
		t.Fatalf("Run(fast) err = %v, want nil", err)
	}
	if out != "fast:x" {
		t.Fatalf("Run(fast) = %q, want fast:x", out)
	}
}

func TestSlowPluginHitsDeadline(t *testing.T) {
	t.Parallel()

	r := NewRegistry(20 * time.Millisecond)
	r.Register(slow{work: time.Hour}) // never completes within the bound

	_, err := r.Run(t.Context(), "slow", "x")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run(slow) err = %v, want context.DeadlineExceeded", err)
	}
}

func TestParentCancelPropagates(t *testing.T) {
	t.Parallel()

	r := NewRegistry(time.Hour) // huge timeout; cancellation must win the race
	r.Register(slow{work: time.Hour})

	ctx, cancel := context.WithCancel(t.Context())
	cancel() // parent already cancelled before the call

	_, err := r.Run(ctx, "slow", "x")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run(slow) err = %v, want context.Canceled", err)
	}
}

func TestRunUnknownReturnsNotFound(t *testing.T) {
	t.Parallel()

	r := NewRegistry(time.Second)
	if _, err := r.Run(t.Context(), "missing", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Run(missing) err = %v, want ErrNotFound", err)
	}
}
```

## Review

The context is only real safety when the plugin observes it. The slow-plugin tests
prove exactly that: with a `select` on `ctx.Done()`, `Run` returns
`context.DeadlineExceeded` at the bound and `context.Canceled` on a cancelled
parent — instantly, without waiting out the plugin's own timer. Remove the
`ctx.Done()` case and both tests hang until the (hour-long) work timer fires, which
is the false-safety failure mode this module is about. Two implementation details
carry weight: `defer cancel()` in `Run` releases the timeout timer on every path
(dropping it leaks the timer), and asserting with `errors.Is` rather than `==` is
required because the context sentinels can be wrapped as they propagate. Use
`t.Context()` as the parent so the test's own lifetime bounds any stray goroutine.

## Resources

- [context.WithTimeout](https://pkg.go.dev/context#WithTimeout) — deriving a bounded context and why `cancel` must always be called.
- [context package](https://pkg.go.dev/context) — `DeadlineExceeded`, `Canceled`, and `ctx.Err()` semantics.
- [Go Blog: Contexts](https://go.dev/blog/context) — propagating cancellation and deadlines across API boundaries.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-constructor-factory-registration.md](04-constructor-factory-registration.md) | Next: [06-ordered-shutdown-error-aggregation.md](06-ordered-shutdown-error-aggregation.md)
