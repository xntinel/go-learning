# Exercise 1: The Plugin Contract and a Lifecycle-Managing Registry

This is the foundational artifact the whole chapter builds on: a `Plugin`
interface that defines the contract, and a `Registry` that manages every plugin's
lifecycle — registering with rollback on a failed `Init`, running by name,
rejecting duplicates, and shutting every plugin down. Two sample plugins and the
full test suite come with it.

This module is fully self-contained. It has its own `go mod init`, defines every
type it needs, and ships its own demo and tests. Nothing here imports any other
exercise.

## What you'll build

```text
pluginsys/                 independent module: example.com/pluginsys
  go.mod                   go 1.25
  plugin.go                Plugin interface; Registry (Register/Run/Shutdown); Upper, Reverser plugins
  cmd/
    demo/
      main.go              registers Upper, runs it, prints HELLO WORLD, shuts down
  plugin_test.go           registry lifecycle tests, -race concurrency
```

- Files: `plugin.go`, `cmd/demo/main.go`, `plugin_test.go`.
- Implement: a `Plugin` interface (`Name`/`Init`/`Process`/`Shutdown`) and a `Registry` with `Register` (rollback on failed `Init`), `Run` (`ErrNotFound`), duplicate rejection (`ErrAlreadyRegistered`), and `Shutdown` over all plugins; plus the `Upper` and `Reverser` sample plugins.
- Test: registers-and-runs, rejects duplicate, returns not-found, shuts down all, rejects a plugin with a failing `Init` and does not store it, and holds multiple plugins.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/pluginsys/cmd/demo
cd ~/go-exercises/pluginsys
go mod init example.com/pluginsys
go mod edit -go=1.25
```

### The contract and the four-method lifecycle

The `Plugin` interface is the contract the host owns. Four methods, no more:
`Name` gives the plugin its registry identity, `Init` runs once at load,
`Process` runs per input, and `Shutdown` runs once at unload. Keeping the required
set this small is the single most important design decision — every method here is
a method every future plugin author must write, so the later modules add
capabilities through *optional* interfaces rather than by widening this one.

The `Registry` holds a `map[string]Plugin` and enforces the lifecycle. The
critical detail is the ordering inside `Register`: it checks for a duplicate name
first, then calls `Init`, and only stores the plugin in the map when `Init`
returns nil. If `Init` fails, the plugin is never stored — there is no rollback to
undo because nothing was written. That is the whole point: a plugin becomes
reachable through `Run` only after it has successfully initialized, so `Process`
can never see a half-initialized plugin. Registering the plugin before `Init`, the
common mistake, would leave a broken plugin in the map that panics on first use.

`Run` looks the plugin up by name and returns `ErrNotFound` for an unknown name
rather than panicking on the nil zero value. `Shutdown` walks every registered
plugin and calls its `Shutdown`. Both sentinel errors are package-level and are
asserted in tests with `errors.Is`, which is the contract callers rely on.

Create `plugin.go`:

```go
package pluginsys

import "errors"

// ErrAlreadyRegistered is returned by Register when a plugin with the same Name
// is already in the registry.
var ErrAlreadyRegistered = errors.New("plugin already registered")

// ErrNotFound is returned by Run when no plugin with the given name exists.
var ErrNotFound = errors.New("plugin not found")

// Plugin is the contract the host defines and every plugin implements. The
// required set is deliberately tiny; capabilities beyond it live in optional
// interfaces discovered at runtime.
type Plugin interface {
	Name() string
	Init() error
	Process(input string) (string, error)
	Shutdown() error
}

// Registry manages plugin lifecycles: it registers plugins (after a successful
// Init), runs them by name, and shuts them all down.
type Registry struct {
	plugins map[string]Plugin
}

// NewRegistry returns an empty registry ready to accept plugins.
func NewRegistry() *Registry {
	return &Registry{plugins: make(map[string]Plugin)}
}

// Register initializes p and stores it under p.Name(). It rejects a duplicate
// name with ErrAlreadyRegistered and, if Init fails, returns the Init error
// WITHOUT storing the plugin, so a half-initialized plugin is never reachable.
func (r *Registry) Register(p Plugin) error {
	if _, ok := r.plugins[p.Name()]; ok {
		return ErrAlreadyRegistered
	}
	if err := p.Init(); err != nil {
		return err
	}
	r.plugins[p.Name()] = p
	return nil
}

// Run processes input through the named plugin, or returns ErrNotFound.
func (r *Registry) Run(name, input string) (string, error) {
	p, ok := r.plugins[name]
	if !ok {
		return "", ErrNotFound
	}
	return p.Process(input)
}

// Shutdown calls Shutdown on every registered plugin.
func (r *Registry) Shutdown() error {
	for _, p := range r.plugins {
		if err := p.Shutdown(); err != nil {
			return err
		}
	}
	return nil
}

// Upper is a sample plugin that uppercases ASCII input.
type Upper struct{}

func (Upper) Name() string { return "upper" }

func (Upper) Init() error { return nil }

func (Upper) Process(input string) (string, error) {
	out := []byte(input)
	for i, b := range out {
		if b >= 'a' && b <= 'z' {
			out[i] = b - 32
		}
	}
	return string(out), nil
}

func (Upper) Shutdown() error { return nil }

// Reverser is a second sample plugin that reverses its input by rune.
type Reverser struct{}

func (Reverser) Name() string { return "reverser" }

func (Reverser) Init() error { return nil }

func (Reverser) Process(input string) (string, error) {
	rs := []rune(input)
	for i, j := 0, len(rs)-1; i < j; i, j = i+1, j-1 {
		rs[i], rs[j] = rs[j], rs[i]
	}
	return string(rs), nil
}

func (Reverser) Shutdown() error { return nil }
```

### The runnable demo

The demo wires the smallest realistic flow: build a registry, register the
`Upper` plugin, run it through the registry by name, print the result, and shut
down. It touches only exported API because `cmd/demo` is its own `package main`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/pluginsys"
)

func main() {
	r := pluginsys.NewRegistry()
	if err := r.Register(pluginsys.Upper{}); err != nil {
		log.Fatal(err)
	}
	out, err := r.Run("upper", "hello world")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(out)
	if err := r.Shutdown(); err != nil {
		log.Fatal(err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
HELLO WORLD
```

### Tests

The suite pins every lifecycle property. `TestRegistryRegistersAndRunsPlugin` is
the happy path. `TestRegistryRejectsDuplicate` asserts `ErrAlreadyRegistered` via
`errors.Is`. `TestRegistryReturnsNotFoundForUnknown` asserts `ErrNotFound`.
`TestRegistryCallsShutdownOnAll` proves shutdown is clean.
`TestRegistryRejectsPluginWithFailingInit` proves the rollback: a plugin whose
`Init` fails is neither reported as success nor left in the map — the test reaches
into the unexported `plugins` field (same-package test) to prove it was never
stored. `TestRegistryHandlesMultiplePlugins` registers two differently-named
plugins and checks each output, pinning the "holds multiple plugins" contract.

Create `plugin_test.go`:

```go
package pluginsys

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

func TestRegistryRegistersAndRunsPlugin(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	if err := r.Register(Upper{}); err != nil {
		t.Fatal(err)
	}
	out, err := r.Run("upper", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if out != "HELLO" {
		t.Fatalf("out = %q, want HELLO", out)
	}
}

func TestRegistryRejectsDuplicate(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	if err := r.Register(Upper{}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(Upper{}); !errors.Is(err, ErrAlreadyRegistered) {
		t.Fatalf("err = %v, want ErrAlreadyRegistered", err)
	}
}

func TestRegistryReturnsNotFoundForUnknown(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	if _, err := r.Run("missing", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestRegistryCallsShutdownOnAll(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	if err := r.Register(Upper{}); err != nil {
		t.Fatal(err)
	}
	if err := r.Shutdown(); err != nil {
		t.Fatal(err)
	}
}

type failingInit struct {
	name string
}

func (f failingInit) Name() string                   { return f.name }
func (f failingInit) Init() error                    { return errors.New("init failed") }
func (f failingInit) Process(string) (string, error) { return "", nil }
func (f failingInit) Shutdown() error                { return nil }

func TestRegistryRejectsPluginWithFailingInit(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	if err := r.Register(failingInit{name: "broken"}); err == nil {
		t.Fatal("expected error for failing init")
	}
	if _, ok := r.plugins["broken"]; ok {
		t.Fatal("plugin should not be stored after failing init")
	}
}

func TestRegistryHandlesMultiplePlugins(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	if err := r.Register(Upper{}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(Reverser{}); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name, in, want string
	}{
		{"upper", "abc", "ABC"},
		{"reverser", "abc", "cba"},
	}
	for _, tc := range cases {
		out, err := r.Run(tc.name, tc.in)
		if err != nil {
			t.Fatalf("Run(%q): %v", tc.name, err)
		}
		if out != tc.want {
			t.Fatalf("Run(%q, %q) = %q, want %q", tc.name, tc.in, out, tc.want)
		}
	}
}

func TestRegistryConcurrentRun(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	if err := r.Register(Upper{}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := r.Run("upper", "go"); err != nil {
				t.Errorf("Run: %v", err)
			}
		}()
	}
	wg.Wait()
}

func Example() {
	r := NewRegistry()
	_ = r.Register(Upper{})
	out, _ := r.Run("upper", "hello")
	fmt.Println(out)
	// Output: HELLO
}
```

The concurrent test runs `Run` from 100 goroutines against a registry that is
only read after setup; it exists to prove the read path is race-free under
`-race`. (Concurrent *mutation* — registering while running — is the subject of
the thread-safe registry module.)

## Review

The registry is correct when a plugin is reachable through `Run` exactly when it
was registered *and* its `Init` returned nil, and never otherwise. The rollback
test is the one that catches the most dangerous bug: storing a plugin before
`Init` succeeds leaves a broken plugin that panics on first `Process`; the fix,
and the property the test asserts, is that `Register` writes the map only on the
success path. Duplicate rejection and not-found are asserted with `errors.Is`
against package-level sentinels, which is the interface callers depend on — never
compare these with `==` once they might be wrapped. Keep the `Plugin` interface at
four methods; every capability the later modules add arrives through a separate
optional interface, not by growing this contract.

## Resources

- [Go Specification: Interface types](https://go.dev/ref/spec#Interface_types) — structural satisfaction, the mechanism that lets any type plug in.
- [Effective Go: Interfaces and other types](https://go.dev/doc/effective_go#interfaces) — idiomatic small interfaces.
- [errors package](https://pkg.go.dev/errors) — `errors.New`, `errors.Is` for sentinel assertions.
- [hashicorp/go-plugin](https://github.com/hashicorp/go-plugin) — a production plugin architecture built on the same contract idea.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-optional-capability-interfaces.md](02-optional-capability-interfaces.md)
