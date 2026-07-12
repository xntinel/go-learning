# Exercise 8: A Config Loader That Matches fs.ErrNotExist

A missing config file is often not an error — it means "use defaults". But a
config file you *cannot read* (bad permissions) or that is *corrupt* absolutely
is. This exercise builds a loader that tells those cases apart with
`errors.Is(err, fs.ErrNotExist)`, and demonstrates why `==` fails here: `os.ReadFile`
returns a wrapped `*fs.PathError`, not the bare sentinel.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
config/                       independent module: example.com/config
  go.mod                      go 1.26
  config.go                   Config; Default; Load (missing->defaults, else surface); parse; ErrCorrupt
  cmd/
    demo/
      main.go                 missing/loaded/corrupt paths
  config_test.go              missing->defaults, permission-not-swallowed, PathError resolves ErrNotExist
```

- Files: `config.go`, `cmd/demo/main.go`, `config_test.go`.
- Implement: `Load(path)` that returns `Default()` when `errors.Is(err, fs.ErrNotExist)`, surfaces every other read/parse error, and parses a tiny `key = value` format.
- Test: a nonexistent path returns defaults with no error; a `0000`-perm file surfaces an error not swallowed as not-exist (skip if root); `os.ReadFile` of a missing file yields a `*fs.PathError` that `errors.Is` still resolves to `fs.ErrNotExist`.
- Verify: `go test -count=1 -race ./...`

### Missing is fine; unreadable is not

The loader has to draw a precise line. "File does not exist" is a legitimate
first-run state: return the defaults and carry on. But every *other* failure
must surface, because silently defaulting on a permission error or a corrupt file
hides a real misconfiguration and ships wrong behavior to production. The
temptation is to write `if err != nil { return Default(), nil }` — which
swallows *all* errors as "missing" and is exactly the bug.

The correct predicate is `errors.Is(err, fs.ErrNotExist)`, and understanding why
`==` cannot work here is the lesson. `os.ReadFile` does not return the bare
`fs.ErrNotExist` sentinel; on a missing file it returns a `*fs.PathError`
(carrying the op, the path, and an underlying errno) whose chain *contains*
`fs.ErrNotExist`. So `err == fs.ErrNotExist` is `false`, while
`errors.Is(err, fs.ErrNotExist)` walks to the wrapped sentinel and returns
`true`. Two more facts worth internalizing: `os.ErrNotExist` and `fs.ErrNotExist`
are the *same* value (the `os` name is an alias), so matching either works; and a
permission error is a *different* underlying errno, so `errors.Is(err, fs.ErrNotExist)`
correctly returns `false` for it and the loader surfaces it instead of defaulting.

A corrupt file (unparseable) is surfaced through a domain sentinel `ErrCorrupt`,
keeping parse failures distinct from I/O failures.

Create `config.go`:

```go
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
)

// ErrCorrupt marks a config file that exists and is readable but cannot be
// parsed. It is distinct from any I/O error.
var ErrCorrupt = errors.New("corrupt config")

type Config struct {
	MaxConns int
	Debug    bool
}

// Default is the configuration used when no file is present.
func Default() Config { return Config{MaxConns: 10, Debug: false} }

// Load reads a config file. A missing file is not an error: it returns Default().
// Every other failure (permission, corruption) is surfaced. os.ReadFile wraps a
// *fs.PathError, so == against fs.ErrNotExist fails and errors.Is is mandatory.
// os.ErrNotExist and fs.ErrNotExist are the same sentinel value.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Default(), nil
		}
		return Config{}, fmt.Errorf("load config %s: %w", path, err)
	}
	cfg, err := parse(string(data))
	if err != nil {
		return Config{}, fmt.Errorf("load config %s: %w", path, err)
	}
	return cfg, nil
}

func parse(s string) (Config, error) {
	cfg := Default()
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return Config{}, fmt.Errorf("%w: line %q", ErrCorrupt, line)
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch k {
		case "max_conns":
			n, err := strconv.Atoi(v)
			if err != nil {
				return Config{}, fmt.Errorf("%w: max_conns %q", ErrCorrupt, v)
			}
			cfg.MaxConns = n
		case "debug":
			cfg.Debug = v == "true"
		default:
			return Config{}, fmt.Errorf("%w: unknown key %q", ErrCorrupt, k)
		}
	}
	return cfg, nil
}
```

### The runnable demo

The demo hits three paths in a temp directory: a missing file (defaults), a valid
file (parsed), and a corrupt file (surfaced as `ErrCorrupt`, and *not* as
not-exist).

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"example.com/config"
)

func main() {
	dir, err := os.MkdirTemp("", "cfg")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)

	// No file: defaults, no error.
	cfg, err := config.Load(filepath.Join(dir, "missing.conf"))
	fmt.Printf("missing -> %+v err=%v\n", cfg, err)

	// A real file.
	path := filepath.Join(dir, "app.conf")
	if err := os.WriteFile(path, []byte("max_conns = 25\ndebug = true\n"), 0o644); err != nil {
		panic(err)
	}
	cfg, err = config.Load(path)
	fmt.Printf("loaded  -> %+v err=%v\n", cfg, err)

	// A corrupt file: surfaced, not swallowed as not-exist.
	bad := filepath.Join(dir, "bad.conf")
	if err := os.WriteFile(bad, []byte("max_conns = not-a-number\n"), 0o644); err != nil {
		panic(err)
	}
	_, err = config.Load(bad)
	fmt.Printf("corrupt -> corrupt=%v not-exist=%v\n",
		errors.Is(err, config.ErrCorrupt), errors.Is(err, fs.ErrNotExist))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
missing -> {MaxConns:10 Debug:false} err=<nil>
loaded  -> {MaxConns:25 Debug:true} err=<nil>
corrupt -> corrupt=true not-exist=false
```

### Tests

`TestMissingFileUsesDefaults` proves a nonexistent path returns defaults with no
error. `TestPermissionErrorNotSwallowed` writes a `0000` file and proves the
error surfaces and is *not* classified as not-exist (skipped under root, where
permission bits are ignored). `TestPathErrorResolvesNotExist` is the mechanism
proof: `os.ReadFile` of a missing file returns a `*fs.PathError` that `==` cannot
match but `errors.Is` resolves, and `os.ErrNotExist == fs.ErrNotExist`.

Create `config_test.go`:

```go
package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestMissingFileUsesDefaults(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "nope.conf")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if cfg != Default() {
		t.Fatalf("cfg = %+v, want defaults %+v", cfg, Default())
	}
}

func TestValidFileParsed(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "app.conf")
	if err := os.WriteFile(path, []byte("# comment\nmax_conns = 50\ndebug = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MaxConns != 50 || !cfg.Debug {
		t.Fatalf("cfg = %+v, want {50 true}", cfg)
	}
}

func TestCorruptFileSurfaced(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "bad.conf")
	if err := os.WriteFile(path, []byte("max_conns = not-a-number\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("want ErrCorrupt, got %v", err)
	}
	if errors.Is(err, fs.ErrNotExist) {
		t.Fatal("a corrupt file must not be classified as not-exist")
	}
}

func TestPermissionErrorNotSwallowed(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits are not enforced")
	}
	path := filepath.Join(t.TempDir(), "secret.conf")
	if err := os.WriteFile(path, []byte("debug = true\n"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })

	_, err := Load(path)
	if err == nil {
		t.Fatal("an unreadable file must surface an error, not defaults")
	}
	if errors.Is(err, fs.ErrNotExist) {
		t.Fatal("a permission error must not be classified as not-exist")
	}
}

func TestPathErrorResolvesNotExist(t *testing.T) {
	t.Parallel()

	_, err := os.ReadFile(filepath.Join(t.TempDir(), "ghost"))
	var pe *fs.PathError
	if !errors.As(err, &pe) {
		t.Fatalf("want *fs.PathError, got %T", err)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("errors.Is should resolve fs.ErrNotExist through *fs.PathError")
	}
	if err == fs.ErrNotExist {
		t.Fatal("== against fs.ErrNotExist should fail for a *fs.PathError")
	}
	if os.ErrNotExist != fs.ErrNotExist {
		t.Fatal("os.ErrNotExist and fs.ErrNotExist must be the same value")
	}
}
```

## Review

The loader is correct when only a genuine not-exist returns defaults and every
other error — permission, corruption — surfaces. `errors.Is(err, fs.ErrNotExist)`
is mandatory because `os.ReadFile` returns a wrapped `*fs.PathError`, which
`TestPathErrorResolvesNotExist` proves `==` cannot match. The bug to avoid is the
blanket `if err != nil { return Default(), nil }` that treats an unreadable or
corrupt file as if it were absent, silently shipping wrong config. Match the
specific sentinel; let everything else through.

## Resources

- [`io/fs` variables](https://pkg.go.dev/io/fs#pkg-variables) — `fs.ErrNotExist` and friends.
- [`fs.PathError`](https://pkg.go.dev/io/fs#PathError) — the wrapper `os.ReadFile` returns.
- [`os.ReadFile`](https://pkg.go.dev/os#ReadFile) and [`os.ErrNotExist`](https://pkg.go.dev/os#pkg-variables) — the alias relationship with `fs.ErrNotExist`.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [07-join-validation-pipeline.md](07-join-validation-pipeline.md) | Next: [09-idempotency-guard-typed-vs-sentinel.md](09-idempotency-guard-typed-vs-sentinel.md)
