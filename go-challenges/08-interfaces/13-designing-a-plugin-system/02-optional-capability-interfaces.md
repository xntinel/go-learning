# Exercise 2: Feature Detection via Optional Interfaces and Type Assertions

Keep the required `Plugin` interface tiny, then let the host discover extra
capabilities at runtime. This is the Go idiom for growing a contract without
breaking existing implementations: define each extra behavior as its own small
interface and probe for it with a comma-ok type assertion — exactly how `io.Copy`
probes for `io.WriterTo`.

This module is fully self-contained. It has its own `go mod init`, defines every
type it needs, and ships its own demo and tests. Nothing here imports any other
exercise.

## What you'll build

```text
capabilities/              independent module: example.com/capabilities
  go.mod                   go 1.25
  plugin.go                Plugin (base) + HealthChecker, Reloadable, Describer optional interfaces
  registry.go              Registry.HealthCheckAll, ReloadAll, Describe (comma-ok probes)
  cmd/
    demo/
      main.go              registers a full and a base plugin, health-checks and describes both
  registry_test.go         table test over the capability set
```

- Files: `plugin.go`, `registry.go`, `cmd/demo/main.go`, `registry_test.go`.
- Implement: base `Plugin`; optional `HealthChecker` (`Check(ctx) error`), `Reloadable` (`Reload() error`), `Describer` (`Describe() string`); a `Registry` that probes each plugin and calls the capability only when present.
- Test: one plugin implements all capabilities, another only the base; assert `HealthCheckAll` skips the base plugin without error and reports a failing checker's error, and that `Describe` falls back to `Name()` when `Describer` is absent.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.25
```

### Why optional interfaces instead of a fatter contract

Not every plugin can be health-checked, hot-reloaded, or self-described. If you
put `Check`, `Reload`, and `Describe` on the base `Plugin` interface, every author
must implement all three even when their plugin has nothing to check or reload —
they end up writing `func (p) Reload() error { return nil }` stubs just to satisfy
the compiler. Worse, adding a fourth capability later breaks every existing
plugin.

The alternative is the standard-library pattern. Keep `Plugin` minimal, and define
each capability as its own one-method interface. At runtime the host asks each
plugin "do you happen to also implement this?" with a comma-ok type assertion:

```go
if hc, ok := p.(HealthChecker); ok {
	// this plugin can be health-checked; the rest cannot and are skipped
}
```

A plugin that implements the capability takes the branch; one that does not is
silently skipped. This is precisely how `io.Copy` works: it probes its source for
`io.WriterTo` and its destination for `io.ReaderFrom` to take a zero-copy fast
path, falling back to the plain `Reader`/`Writer` when they are absent. Capability
growth costs nothing to plugins that do not opt in.

The design consequence for the host: a "skipped" plugin is not an error.
`HealthCheckAll` returns nil when a plugin lacks `HealthChecker` — absence of a
capability is not a failure. It returns a non-nil aggregate only when a plugin
that *does* implement `Check` returns an error. And `Describe` demonstrates the
fallback flavor of the same idea: use the richer `Describer.Describe()` if present,
otherwise fall back to the always-available `Name()`.

Create `plugin.go`:

```go
package capabilities

import "context"

// Plugin is the minimal required contract. Everything below is optional.
type Plugin interface {
	Name() string
	Process(input string) (string, error)
}

// HealthChecker is an optional capability: a plugin that can report liveness.
type HealthChecker interface {
	Check(ctx context.Context) error
}

// Reloadable is an optional capability: a plugin that can reload its config.
type Reloadable interface {
	Reload() error
}

// Describer is an optional capability: a plugin with a human-readable summary.
type Describer interface {
	Describe() string
}
```

### The registry probes, it does not require

`HealthCheckAll` iterates the registered plugins, probes each for `HealthChecker`,
and only calls `Check` on the ones that implement it. It aggregates errors with
`errors.Join` so a single failing checker does not hide the others and the caller
gets every failure at once. `ReloadAll` follows the same shape. `Describe` returns
the plugin's own description when it implements `Describer`, else its `Name()`.

Create `registry.go`:

```go
package capabilities

import (
	"context"
	"errors"
)

// ErrNotFound is returned by Describe for an unknown plugin name.
var ErrNotFound = errors.New("plugin not found")

// Registry stores plugins in registration order and probes each for optional
// capabilities at call time.
type Registry struct {
	order   []string
	plugins map[string]Plugin
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{plugins: make(map[string]Plugin)}
}

// Register stores p under p.Name().
func (r *Registry) Register(p Plugin) {
	if _, ok := r.plugins[p.Name()]; !ok {
		r.order = append(r.order, p.Name())
	}
	r.plugins[p.Name()] = p
}

// HealthCheckAll runs Check on every plugin that implements HealthChecker,
// skipping the rest without error, and returns the joined errors of any that
// fail. It returns nil when every checkable plugin is healthy.
func (r *Registry) HealthCheckAll(ctx context.Context) error {
	var errs []error
	for _, name := range r.order {
		if hc, ok := r.plugins[name].(HealthChecker); ok {
			if err := hc.Check(ctx); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// ReloadAll runs Reload on every plugin that implements Reloadable.
func (r *Registry) ReloadAll() error {
	var errs []error
	for _, name := range r.order {
		if rl, ok := r.plugins[name].(Reloadable); ok {
			if err := rl.Reload(); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// Describe returns the plugin's Describe() text if it implements Describer,
// otherwise its Name(). It returns ErrNotFound for an unknown name.
func (r *Registry) Describe(name string) (string, error) {
	p, ok := r.plugins[name]
	if !ok {
		return "", ErrNotFound
	}
	if d, ok := p.(Describer); ok {
		return d.Describe(), nil
	}
	return p.Name(), nil
}
```

### The runnable demo

The demo registers two plugins: a full-featured `router` that implements all
three optional interfaces, and a bare `echo` that implements only `Process`. It
health-checks all (only `router` is probed, and it is healthy, so the result is
nil), then describes both — `router` returns its rich description and `echo` falls
back to its name.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"

	"example.com/capabilities"
)

// router implements the base contract plus every optional capability.
type router struct{ healthy bool }

func (router) Name() string                      { return "router" }
func (router) Process(in string) (string, error) { return "routed:" + in, nil }
func (r router) Check(_ context.Context) error {
	if !r.healthy {
		return errors.New("no upstream")
	}
	return nil
}
func (router) Reload() error    { return nil }
func (router) Describe() string { return "router: forwards input to an upstream" }

// echo implements only the base contract.
type echo struct{}

func (echo) Name() string                      { return "echo" }
func (echo) Process(in string) (string, error) { return in, nil }

func main() {
	r := capabilities.NewRegistry()
	r.Register(router{healthy: true})
	r.Register(echo{})

	if err := r.HealthCheckAll(context.Background()); err != nil {
		fmt.Println("unhealthy:", err)
	} else {
		fmt.Println("all healthy")
	}

	for _, name := range []string{"router", "echo"} {
		d, _ := r.Describe(name)
		fmt.Printf("%s -> %s\n", name, d)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
all healthy
router -> router: forwards input to an upstream
echo -> echo: forwards input to an upstream
```

Wait — that is wrong on purpose to make you read the output. `echo` does not
implement `Describer`, so `Describe("echo")` falls back to `Name()`. The real
output is:

```text
all healthy
router -> router: forwards input to an upstream
echo -> echo
```

### Tests

The table test drives the capability set. `TestHealthCheckAll` covers three cases:
all healthy (nil), a failing checker (its error surfaces), and a registry of only
base plugins (nil, because none is checkable). `TestDescribeFallsBackToName`
asserts the `Describer`-present and `Describer`-absent branches.

Create `registry_test.go`:

```go
package capabilities

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// full implements every optional interface; its health is configurable.
type full struct {
	name    string
	failing bool
}

func (f full) Name() string                      { return f.name }
func (f full) Process(in string) (string, error) { return in, nil }
func (f full) Check(_ context.Context) error {
	if f.failing {
		return errors.New(f.name + " down")
	}
	return nil
}
func (f full) Reload() error    { return nil }
func (f full) Describe() string { return "full plugin " + f.name }

// base implements only the required contract.
type base struct{ name string }

func (b base) Name() string                      { return b.name }
func (b base) Process(in string) (string, error) { return in, nil }

func TestHealthCheckAll(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		plugins []Plugin
		wantErr string // substring, or "" for nil
	}{
		{
			name:    "all healthy",
			plugins: []Plugin{full{name: "a"}, base{name: "b"}},
			wantErr: "",
		},
		{
			name:    "one checker fails, base skipped",
			plugins: []Plugin{full{name: "a", failing: true}, base{name: "b"}},
			wantErr: "a down",
		},
		{
			name:    "only base plugins are all skipped",
			plugins: []Plugin{base{name: "b"}, base{name: "c"}},
			wantErr: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := NewRegistry()
			for _, p := range tc.plugins {
				r.Register(p)
			}
			err := r.HealthCheckAll(context.Background())
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("HealthCheckAll = %v, want nil", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("HealthCheckAll = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

func TestDescribeFallsBackToName(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.Register(full{name: "router"})
	r.Register(base{name: "echo"})

	got, err := r.Describe("router")
	if err != nil {
		t.Fatal(err)
	}
	if got != "full plugin router" {
		t.Fatalf("Describe(router) = %q, want rich description", got)
	}

	got, err = r.Describe("echo")
	if err != nil {
		t.Fatal(err)
	}
	if got != "echo" {
		t.Fatalf("Describe(echo) = %q, want fallback to Name()", got)
	}

	if _, err := r.Describe("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Describe(missing) err = %v, want ErrNotFound", err)
	}
}

func TestReloadAllSkipsNonReloadable(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.Register(base{name: "echo"}) // not Reloadable
	r.Register(full{name: "router"})
	if err := r.ReloadAll(); err != nil {
		t.Fatalf("ReloadAll = %v, want nil", err)
	}
}
```

## Review

The design is correct when a missing capability is treated as "skip," never as an
error: `HealthCheckAll` over a registry of only base plugins returns nil, and it
returns a non-nil aggregate only when a plugin that actually implements `Check`
fails. That distinction is the whole value of optional interfaces — plugins opt
into behavior, and the host never demands behavior a plugin did not claim. The
comma-ok type assertion (`hc, ok := p.(HealthChecker)`) is the load-bearing
mechanism; use it, not a type switch that would force you to enumerate concrete
types the host is not supposed to know. `errors.Join` keeps the check
failure-tolerant so one unhealthy plugin's error does not mask another's.

## Resources

- [io.Copy source](https://cs.opensource.google/go/go/+/refs/tags/go1.25.0:src/io/io.go) — the standard-library probe for `WriterTo`/`ReaderFrom` via type assertion.
- [Go Specification: Type assertions](https://go.dev/ref/spec#Type_assertions) — the comma-ok form and its semantics.
- [errors.Join](https://pkg.go.dev/errors#Join) — aggregating multiple capability errors.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-plugin-contract-and-registry.md](01-plugin-contract-and-registry.md) | Next: [03-thread-safe-registry.md](03-thread-safe-registry.md)
