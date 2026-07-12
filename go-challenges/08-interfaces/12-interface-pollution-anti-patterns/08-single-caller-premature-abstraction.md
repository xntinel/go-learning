# Exercise 8: Delete the Interface — One Implementation, One Caller

A `Loader` interface with exactly one implementation and exactly one caller is
pure indirection. This module refactors that config-loading path to the concrete
`*FileLoader`, deletes the interface, and then adds a new method
(`WithReloadTimeout`) that a new test uses — a change that in the interface
version would have forced editing the interface and every consumer, but here
touches only the concrete type.

## What you'll build

```text
configload/                 independent module: example.com/configload
  go.mod                    go 1.26
  config.go                 Config; Parse; ErrInvalidConfig
  loader.go                 concrete *FileLoader; NewFileLoader; Load; WithReloadTimeout
  cmd/
    demo/
      main.go               writes a temp config, loads it, prints fields
  loader_test.go            load valid; load missing -> error; the new-method test
```

- Files: `config.go`, `loader.go`, `cmd/demo/main.go`, `loader_test.go`.
- Implement: a concrete `*FileLoader` with `Load(path)`; then ADD `WithReloadTimeout(d)` returning `*FileLoader`, with no interface to edit.
- Test: load a valid config file and a missing file (error via `errors.Is(err, os.ErrNotExist)`); a new test that exercises `WithReloadTimeout`, demonstrating the concrete type extends without touching any interface.
- Verify: `go test -count=1 -race ./...`

### Why the interface had to go, and what deleting it buys

Picture the "before": a `Loader` interface with one method, `Load(path) (Config, error)`,
implemented by exactly one type, `fileLoader`, and called from exactly one place —
the server's startup path. That interface decouples nothing, because there is
nothing to swap in. It costs the reader a hop (go-to-definition on `Load` lands on
a signature, not the code that reads the file) and it costs the maintainer a
second surface. It is a speculative abstraction, justified by "we might load
config from a database or etcd someday." YAGNI: the day rarely comes, and when it
does the real second loader rarely matches the guessed one-method shape.

Deleting it is the refactor. `NewFileLoader` returns the concrete `*FileLoader`;
the single caller uses it directly; the interface is gone. Nothing is lost,
because the concrete type is fully testable on its own — the test below loads a
real file from `t.TempDir()` and asserts the parsed `Config`, and loads a missing
path and asserts the error, with no interface anywhere.

The payoff shows up when requirements grow. Suppose you now need a reload timeout.
On the concrete type you just add a method: `WithReloadTimeout(d) *FileLoader`,
and a new test calls it. Nothing else changes. In the interface version, adding a
method means editing the `Loader` interface, which breaks every implementation and
every fake until they add the method too — and any consumer that only wanted
`Load` is now coupled to a method it does not use. The concrete type extends
freely; the interface would have made the same extension a breaking change. That
asymmetry is the whole reason "return structs" beats "return interfaces" for types
with a single implementation.

Create `config.go`:

```go
package configload

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ErrInvalidConfig wraps a config that parses but is semantically invalid.
var ErrInvalidConfig = errors.New("configload: invalid config")

// Config is the loaded application config.
type Config struct {
	Name string `json:"name"`
	Port int    `json:"port"`
}

// Parse decodes JSON config bytes and validates them.
func Parse(data []byte) (Config, error) {
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return Config{}, fmt.Errorf("configload: parse: %w", err)
	}
	if c.Name == "" {
		return Config{}, fmt.Errorf("%w: name is required", ErrInvalidConfig)
	}
	if c.Port <= 0 || c.Port > 65535 {
		return Config{}, fmt.Errorf("%w: port %d out of range", ErrInvalidConfig, c.Port)
	}
	return c, nil
}
```

Create `loader.go`:

```go
package configload

import (
	"fmt"
	"os"
	"time"
)

// FileLoader loads config from a file. It is a concrete type with no interface:
// one implementation, one caller, so an interface would be pure indirection.
type FileLoader struct {
	reloadTimeout time.Duration
}

// NewFileLoader returns a concrete *FileLoader (return structs, not interfaces).
func NewFileLoader() *FileLoader {
	return &FileLoader{}
}

// Load reads and parses the config file at path.
func (l *FileLoader) Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("configload: read %s: %w", path, err)
	}
	return Parse(data)
}

// WithReloadTimeout is the NEW method added after the interface was deleted. On
// the concrete type this is a non-breaking addition; behind an interface it would
// have forced editing the interface and every implementation and fake.
func (l *FileLoader) WithReloadTimeout(d time.Duration) *FileLoader {
	l.reloadTimeout = d
	return l
}

// ReloadTimeout exposes the configured timeout (0 if unset).
func (l *FileLoader) ReloadTimeout() time.Duration {
	return l.reloadTimeout
}
```

### The runnable demo

The demo writes a temp config file, loads it with the concrete loader, and prints
the parsed fields and the reload timeout set through the new fluent method.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"example.com/configload"
)

func main() {
	dir, err := os.MkdirTemp("", "configload-demo")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)

	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"name":"api","port":8080}`), 0o600); err != nil {
		panic(err)
	}

	loader := configload.NewFileLoader().WithReloadTimeout(5 * time.Second)
	cfg, err := loader.Load(path)
	if err != nil {
		panic(err)
	}

	fmt.Printf("name=%s port=%d\n", cfg.Name, cfg.Port)
	fmt.Printf("reload timeout=%s\n", loader.ReloadTimeout())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
name=api port=8080
reload timeout=5s
```

### Tests

`TestLoadValid` writes a config into `t.TempDir()` and asserts the parsed fields —
the concrete loader tested directly, no interface, no mock. `TestLoadMissingFile`
loads a path that does not exist and asserts `errors.Is(err, os.ErrNotExist)`.
`TestWithReloadTimeout` is the new-method test: it would have required editing a
`Loader` interface in the before version, but here it just calls the new method
on the concrete type. `ExampleParse` pins the parse output so `go test` verifies
the snippet too.

Create `loader_test.go`:

```go
package configload

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadValid(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"name":"api","port":8080}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg, err := NewFileLoader().Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Name != "api" || cfg.Port != 8080 {
		t.Fatalf("cfg = %+v, want {api 8080}", cfg)
	}
}

func TestLoadMissingFile(t *testing.T) {
	t.Parallel()

	_, err := NewFileLoader().Load(filepath.Join(t.TempDir(), "absent.json"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Load(absent) err = %v, want errors.Is(_, os.ErrNotExist)", err)
	}
}

func TestLoadInvalidConfig(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(path, []byte(`{"name":"","port":0}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err := NewFileLoader().Load(path)
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("Load(bad) err = %v, want errors.Is(_, ErrInvalidConfig)", err)
	}
}

// TestWithReloadTimeout is the new-method test. In the interface version this
// method would not exist without editing the Loader interface; on the concrete
// type it is a free addition.
func TestWithReloadTimeout(t *testing.T) {
	t.Parallel()

	loader := NewFileLoader().WithReloadTimeout(3 * time.Second)
	if got := loader.ReloadTimeout(); got != 3*time.Second {
		t.Fatalf("ReloadTimeout = %s, want 3s", got)
	}
}

// ExampleParse shows the pure parse/validate step returning a Config; the
// // Output line is auto-verified by `go test`.
func ExampleParse() {
	cfg, _ := Parse([]byte(`{"name":"api","port":8080}`))
	fmt.Printf("%s %d\n", cfg.Name, cfg.Port)
	// Output: api 8080
}
```

## Review

The refactor deleted a `Loader` interface that had one implementation and one
caller, and the code got simpler with no loss of testability — the concrete
`*FileLoader` is exercised directly against real files. The new-method test is the
argument's payoff: `WithReloadTimeout` was added to the concrete type without
touching any interface, whereas the interface version would have propagated the
edit to every implementer and faker and coupled every `Load`-only consumer to a
method it does not call. The rule is "do not define an interface before it is
used" — before there is a second real implementation or a boundary you must fake.
When one of those finally arrives, define the interface then, narrow, in the
consumer that needs it.

## Resources

- [Go Code Review Comments — Interfaces](https://go.dev/wiki/CodeReviewComments#interfaces) — do not define interfaces before they are used; return concrete types.
- [Martin Fowler — Yagni](https://martinfowler.com/bliki/Yagni.html) — the cost of speculative abstraction.
- [os.ReadFile](https://pkg.go.dev/os#ReadFile) and [errors.Is](https://pkg.go.dev/errors#Is) — the file read and the `os.ErrNotExist` match the test asserts.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [07-typed-nil-interface-pitfall.md](07-typed-nil-interface-pitfall.md) | Next: [09-generics-over-boxed-interface.md](09-generics-over-boxed-interface.md)
