# Exercise 9: Value Copy vs Shared Pointer: the Mutable-Defaults Bug

Deriving a per-request config from a template has a right way and a wrong way.
Copying the composite-literal value gives each request an isolated config; sharing
`&defaultConfig` and mutating it corrupts global state for every request. And even
the value copy is only shallow — a slice or map field still aliases the template's
backing store until you clone it. This exercise builds all three paths and pins
each with a test.

This module is fully self-contained: its own module, demo, and tests.

## What you'll build

```text
configcopy/                   independent module: example.com/configcopy
  go.mod                      go 1.26
  config.go                   Config (with a slice field), defaultConfig, DeriveCopy, DeriveShared, Clone
  cmd/
    demo/
      main.go                 runnable demo: derive both ways, show template integrity
  config_test.go              value-copy isolates + shared-pointer corrupts + shallow-copy aliases + Clone deep-copies
```

Files: `config.go`, `cmd/demo/main.go`, `config_test.go`.
Implement: a `defaultConfig` template, `DeriveCopy` (value copy, isolated),
`DeriveShared` (shared pointer, corrupts — documented), and `Clone` (deep copy of
the slice field).
Test: the value-copy derive leaves the template unchanged; the shared-pointer path
demonstrably corrupts the template; a shallow copy still aliases the slice field;
`Clone` breaks the aliasing.
Verify: `go test -count=1 -race ./...`

### Why the copy is isolated but shallow

`defaultConfig` is a composite-literal template holding the sane defaults. To
derive a per-request config you have two candidate moves. Assigning the value —
`cfg := defaultConfig` — copies the struct: `cfg` is an independent `Config`, and
setting `cfg.MaxRetries = 5` changes only `cfg`, leaving the template untouched.
That is the correct per-request isolation. The wrong move is taking the template's
address and sharing it — `cfg := &defaultConfig` — and then mutating through the
pointer: now every request that "derives" a config is writing to the one shared
template, so a `cfg.MaxRetries = 5` in one request is visible to all of them. That
is a classic mutable-global corruption bug, and it is silent because it compiles
and usually "works" until two requests race or read each other's writes.

But value copy is *shallow*, and that is the subtle second half. A `Config` that
contains a slice field (say `AllowedHosts []string`) copies the slice *header*
when the struct is copied, and that header still points at the *same backing
array* as the template. So `cfg := defaultConfig; cfg.AllowedHosts[0] = "evil"`
mutates the template's slice too, because both headers share one array. "Copy the
struct" is not "deep copy." When a request needs to mutate a reference field in
isolation, it must clone that field explicitly — `slices.Clone(cfg.AllowedHosts)`
— which allocates a fresh backing array. `Clone` here does exactly that, and the
tests prove both that the naive shallow copy aliases and that `Clone` breaks the
aliasing.

Create `config.go`:

```go
package configcopy

import "slices"

// Config is a per-request configuration. It has a scalar field (MaxRetries) and a
// reference field (AllowedHosts) so the shallow-vs-deep copy distinction is real.
type Config struct {
	MaxRetries   int
	Timeout      int // seconds
	AllowedHosts []string
}

// defaultConfig is the template every request derives from. Deriving must not
// mutate it.
var defaultConfig = Config{
	MaxRetries:   3,
	Timeout:      30,
	AllowedHosts: []string{"localhost", "127.0.0.1"},
}

// DeriveCopy returns an isolated per-request config by copying the value template.
// Scalar mutations on the result do not touch defaultConfig.
func DeriveCopy() Config {
	return defaultConfig // struct value copy
}

// DeriveShared returns a pointer to the shared template. This is the BUG path:
// mutating through the returned pointer corrupts defaultConfig for every caller.
// It exists so a test can demonstrate the corruption.
func DeriveShared() *Config {
	return &defaultConfig
}

// Clone returns a deep copy of c: an independent struct whose AllowedHosts is a
// fresh backing array, so mutating the clone's slice cannot touch the original.
func Clone(c Config) Config {
	c.AllowedHosts = slices.Clone(c.AllowedHosts) // break the shared backing array
	return c
}

// Template exposes a read-only view of the current template for tests and callers
// that must not mutate it. It returns a shallow copy.
func Template() Config {
	return defaultConfig
}
```

### The runnable demo

The demo derives a config by value, mutates a scalar, and shows the template
unchanged; then it shows a shallow copy still aliasing the slice, and `Clone`
fixing it.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/configcopy"
)

func main() {
	// Value copy: scalar mutation is isolated.
	cfg := configcopy.DeriveCopy()
	cfg.MaxRetries = 10
	fmt.Printf("derived retries=%d, template retries=%d\n",
		cfg.MaxRetries, configcopy.Template().MaxRetries)

	// Shallow copy still aliases the slice backing array.
	shallow := configcopy.DeriveCopy()
	shallow.AllowedHosts[0] = "shallow-mutation"
	fmt.Printf("template host[0] after shallow write=%q\n",
		configcopy.Template().AllowedHosts[0])

	// Clone gives an independent slice.
	fresh := configcopy.Clone(configcopy.Template())
	fresh.AllowedHosts[0] = "cloned-mutation"
	fmt.Printf("template host[0] after clone write=%q\n",
		configcopy.Template().AllowedHosts[0])
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
derived retries=10, template retries=3
template host[0] after shallow write="shallow-mutation"
template host[0] after clone write="shallow-mutation"
```

The template's `MaxRetries` stayed 3 (scalar copy is isolated); its `host[0]` was
corrupted by the shallow slice write and then *not* corrupted again by the clone
write, which is the whole point.

### Tests

The tests reset the template between cases so they can run in any order, then pin
each property: value copy isolates scalars, the shared pointer corrupts, a shallow
copy aliases the slice, and `Clone` breaks the aliasing.

Create `config_test.go`:

```go
package configcopy

import (
	"fmt"
	"testing"
)

// reset restores the template to a known state so tests are order-independent.
func reset() {
	defaultConfig = Config{
		MaxRetries:   3,
		Timeout:      30,
		AllowedHosts: []string{"localhost", "127.0.0.1"},
	}
}

func TestValueCopyIsolatesScalars(t *testing.T) {
	reset()

	cfg := DeriveCopy()
	cfg.MaxRetries = 99
	cfg.Timeout = 1

	if defaultConfig.MaxRetries != 3 {
		t.Errorf("template MaxRetries = %d, want unchanged 3", defaultConfig.MaxRetries)
	}
	if defaultConfig.Timeout != 30 {
		t.Errorf("template Timeout = %d, want unchanged 30", defaultConfig.Timeout)
	}
}

func TestSharedPointerCorruptsTemplate(t *testing.T) {
	reset()

	// The BUG path: DeriveShared hands out the template's address.
	shared := DeriveShared()
	shared.MaxRetries = 99

	if defaultConfig.MaxRetries != 99 {
		t.Fatalf("template MaxRetries = %d; shared-pointer mutation should have corrupted it to 99",
			defaultConfig.MaxRetries)
	}
}

func TestShallowCopyAliasesSlice(t *testing.T) {
	reset()

	cfg := DeriveCopy() // value copy, but AllowedHosts header still aliases
	cfg.AllowedHosts[0] = "corrupted"

	if defaultConfig.AllowedHosts[0] != "corrupted" {
		t.Fatalf("template host[0] = %q; shallow copy should alias the backing array",
			defaultConfig.AllowedHosts[0])
	}
}

func TestCloneBreaksAliasing(t *testing.T) {
	reset()

	fresh := Clone(Template())
	fresh.AllowedHosts[0] = "isolated"

	if defaultConfig.AllowedHosts[0] != "localhost" {
		t.Fatalf("template host[0] = %q; Clone should have isolated the slice",
			defaultConfig.AllowedHosts[0])
	}
}

// ExampleClone shows that Clone breaks slice aliasing. It uses a local Config
// rather than the package template so it does not depend on global state.
func ExampleClone() {
	original := Config{MaxRetries: 3, AllowedHosts: []string{"localhost"}}
	clone := Clone(original)
	clone.AllowedHosts[0] = "changed"
	fmt.Println(original.AllowedHosts[0], clone.AllowedHosts[0])
	// Output:
	// localhost changed
}
```

## Review

Each test pins one leg of the copy-semantics story, and together they are the
lesson. `TestValueCopyIsolatesScalars` proves the right derive path works for
scalars; `TestSharedPointerCorruptsTemplate` documents the bug by *asserting the
corruption happens*, which is how you make a silent failure visible and regression-
proof; `TestShallowCopyAliasesSlice` proves that even the correct value copy still
shares the slice's backing array; and `TestCloneBreaksAliasing` proves the fix.
The mental model to carry away: assigning a struct copies its top-level fields by
value (isolation for scalars) but shares every slice, map, and pointer field
(aliasing for references), so "isolate this config" means "copy the value *and*
clone every reference field you will mutate." The tests `reset()` the template
first so they do not run in parallel — they mutate shared package state on purpose,
which is exactly the hazard the lesson is about.

## Resources

- [Go Specification: Assignments](https://go.dev/ref/spec#Assignments) — struct assignment copies fields; reference fields share backing storage.
- [slices.Clone](https://pkg.go.dev/slices#Clone) — the standard shallow-clone of a slice's backing array.
- [Go blog: Slices intro](https://go.dev/blog/slices-intro) — slice headers and shared backing arrays.

---

Back to [00-concepts.md](00-concepts.md) | Next: [10-atomic-pointer-config-reload.md](10-atomic-pointer-config-reload.md)
