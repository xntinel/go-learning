# Exercise 8: Process-Global Config: t.Setenv, t.Chdir, and the Parallel Panic

A twelve-factor config loader reads overrides from environment variables and
resolves a relative config path against the working directory — both process
globals. `t.Setenv` and `t.Chdir` mutate and auto-restore that global state, but
they *panic* under `t.Parallel()`. This exercise builds the loader, tests the
global path serially, and shows the dependency-injected variant that scales.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports any other exercise.

## What you'll build

```text
appconfig/                   independent module: example.com/appconfig
  go.mod                     go 1.24 (t.Chdir needs it)
  config.go                  Load (reads env + cwd) and LoadFrom (injected, hermetic)
  cmd/
    demo/
      main.go                runnable demo: LoadFrom with an explicit env map and dir
  config_test.go             serial t.Setenv/t.Chdir tests + a parallel LoadFrom test
```

- Files: `config.go`, `cmd/demo/main.go`, `config_test.go`.
- Implement: `Load(configFile)` reading `DB_DSN`/`PORT` from the environment and resolving `configFile` against the cwd; `LoadFrom(env, baseDir, configFile)` that touches no globals.
- Test: serial tests using `t.Setenv` and `t.Chdir` into a `t.TempDir`; a `t.Parallel()` test against `LoadFrom` to show the injected path scales.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why the env/cwd test must stay serial

`t.Setenv` and `t.Chdir` are both implemented on top of `Cleanup`: each mutates a
process-global (an environment variable, the working directory), records the prior
value, and registers a cleanup that restores it at test end. That restoration is
automatic and reliable — but the state they touch is shared by the *entire
process*. If two tests ran in parallel and one changed `DB_DSN` while the other
read it, the second would observe a value it never set. To prevent that class of
heisenbug, the runtime *panics* the moment `t.Setenv` or `t.Chdir` is called on a
test that has called `t.Parallel()` (or has a parallel ancestor). It is a hard
constraint, not a lint: adding `t.Parallel()` to `TestLoadReadsEnvOverride` below
turns it from passing into a panic.

That constraint is exactly why senior code pushes the globals to the edge. `Load`
is the thin adapter that reads `os.Getenv` and `os.Getwd` once and immediately
delegates to `LoadFrom(env, baseDir, configFile)`, which takes the environment and
base directory as *parameters* and touches no globals at all. `LoadFrom` is pure
with respect to process state, so its tests can run `t.Parallel()` freely. The
serial `t.Setenv`/`t.Chdir` tests cover the adapter seam; the parallel `LoadFrom`
tests cover the logic and scale with the rest of the suite. This is the same
dependency-injection move that makes any global-touching code testable.

`t.Chdir` pairs naturally with `t.TempDir`: change into a fresh temp directory that
holds a written config file, load a *relative* path, and assert it resolved
against the cwd — then, after the subtest returns, assert the cwd was restored,
because `t.Chdir` registered that restoration as its own cleanup.

Create `config.go`:

```go
package appconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	defaultDSN  = "postgres://localhost:5432/app"
	defaultPort = 8080
)

// Config holds resolved service settings.
type Config struct {
	DSN         string
	Port        int
	ServiceName string
}

// Load resolves configuration from the process environment and a config file
// found relative to the current working directory. It touches process globals
// (env, cwd), so it is NOT safe under t.Parallel.
func Load(configFile string) (Config, error) {
	env := map[string]string{
		"DB_DSN": os.Getenv("DB_DSN"),
		"PORT":   os.Getenv("PORT"),
	}
	wd, err := os.Getwd()
	if err != nil {
		return Config{}, fmt.Errorf("appconfig: getwd: %w", err)
	}
	return LoadFrom(env, wd, configFile)
}

// LoadFrom resolves configuration from an explicit env map and base directory.
// It reads no process globals, so it is safe to call from parallel tests.
func LoadFrom(env map[string]string, baseDir, configFile string) (Config, error) {
	cfg := Config{DSN: defaultDSN, Port: defaultPort}
	if v := env["DB_DSN"]; v != "" {
		cfg.DSN = v
	}
	if v := env["PORT"]; v != "" {
		p, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, fmt.Errorf("appconfig: invalid PORT %q: %w", v, err)
		}
		cfg.Port = p
	}
	path := configFile
	if !filepath.IsAbs(path) {
		path = filepath.Join(baseDir, configFile)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("appconfig: read config: %w", err)
	}
	cfg.ServiceName = strings.TrimSpace(string(data))
	return cfg, nil
}
```

### The runnable demo

The demo uses `LoadFrom` with an explicit env map and a temp directory, so it
touches no process globals and prints the resolved config.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	"example.com/appconfig"
)

func main() {
	dir, err := os.MkdirTemp("", "appconfig-demo")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)

	if err := os.WriteFile(filepath.Join(dir, "service.conf"), []byte("checkout"), 0o600); err != nil {
		log.Fatal(err)
	}

	cfg, err := appconfig.LoadFrom(
		map[string]string{"DB_DSN": "postgres://db/checkout", "PORT": "9000"},
		dir, "service.conf",
	)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("service=%s dsn=%s port=%d\n", cfg.ServiceName, cfg.DSN, cfg.Port)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
service=checkout dsn=postgres://db/checkout port=9000
```

### The tests

`TestLoadReadsEnvOverride` and `TestLoadResolvesRelativeToCwd` are serial: they
use `t.Setenv` and `t.Chdir`, which would panic under `t.Parallel()`. The second
test runs the `t.Chdir` inside a subtest and, after it returns, asserts the cwd
was restored. `TestLoadFromInjectedIsHermetic` marks itself parallel and exercises
the injected `LoadFrom`, demonstrating the path that scales. A missing-file test
matches `os.ErrNotExist` through the `%w` wrap.

Create `config_test.go`:

```go
package appconfig

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// writeConfig writes a service.conf holding name into dir.
func writeConfig(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "service.conf"), []byte(name), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

func TestLoadReadsEnvOverride(t *testing.T) {
	// No t.Parallel(): t.Setenv and t.Chdir mutate process globals. Adding
	// t.Parallel() here makes the runtime panic.
	dir := t.TempDir()
	writeConfig(t, dir, "orders-api")
	t.Chdir(dir)
	t.Setenv("DB_DSN", "postgres://db:5432/orders")
	t.Setenv("PORT", "9090")

	cfg, err := Load("service.conf")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DSN != "postgres://db:5432/orders" {
		t.Errorf("DSN = %q, want the override", cfg.DSN)
	}
	if cfg.Port != 9090 {
		t.Errorf("Port = %d, want 9090", cfg.Port)
	}
	if cfg.ServiceName != "orders-api" {
		t.Errorf("ServiceName = %q, want orders-api", cfg.ServiceName)
	}
}

func TestLoadUsesDefaults(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, "svc")
	t.Chdir(dir)
	// Clear the env explicitly so an inherited value cannot leak in.
	t.Setenv("DB_DSN", "")
	t.Setenv("PORT", "")

	cfg, err := Load("service.conf")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DSN != defaultDSN {
		t.Errorf("DSN = %q, want default %q", cfg.DSN, defaultDSN)
	}
	if cfg.Port != defaultPort {
		t.Errorf("Port = %d, want default %d", cfg.Port, defaultPort)
	}
}

func TestLoadResolvesRelativeToCwd(t *testing.T) {
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Run("inside tempdir", func(t *testing.T) {
		dir := t.TempDir()
		writeConfig(t, dir, "billing")
		t.Chdir(dir)
		t.Setenv("DB_DSN", "")
		t.Setenv("PORT", "")

		cfg, err := Load("service.conf")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.ServiceName != "billing" {
			t.Errorf("ServiceName = %q, want billing", cfg.ServiceName)
		}
	})
	// t.Chdir registered a cleanup that restored the cwd when the subtest ended.
	now, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if now != orig {
		t.Errorf("cwd = %q after subtest, want restored %q", now, orig)
	}
}

func TestLoadFromInjectedIsHermetic(t *testing.T) {
	t.Parallel() // safe: LoadFrom touches no process globals
	dir := t.TempDir()
	writeConfig(t, dir, "payments")

	cfg, err := LoadFrom(
		map[string]string{"DB_DSN": "postgres://x/y", "PORT": "7000"},
		dir, "service.conf",
	)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if cfg.ServiceName != "payments" || cfg.Port != 7000 {
		t.Errorf("got %+v, want payments on :7000", cfg)
	}
}

func TestLoadFromMissingConfig(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_, err := LoadFrom(map[string]string{}, dir, "absent.conf")
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v, want os.ErrNotExist", err)
	}
}

func TestLoadFromInvalidPort(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeConfig(t, dir, "svc")
	_, err := LoadFrom(map[string]string{"PORT": "notanumber"}, dir, "service.conf")
	if err == nil {
		t.Fatal("LoadFrom accepted a non-numeric PORT")
	}
}

func ExampleLoadFrom() {
	dir, _ := os.MkdirTemp("", "cfg")
	defer os.RemoveAll(dir)
	_ = os.WriteFile(filepath.Join(dir, "service.conf"), []byte("demo-svc"), 0o600)

	cfg, _ := LoadFrom(map[string]string{"PORT": "8443"}, dir, "service.conf")
	fmt.Printf("%s on :%d\n", cfg.ServiceName, cfg.Port)
	// Output: demo-svc on :8443
}
```

## Review

The loader is correct when `Load` reads globals only at the adapter seam and
delegates all logic to the pure `LoadFrom`. The proof the split matters is in the
test shapes: the `t.Setenv`/`t.Chdir` tests must stay serial (adding `t.Parallel()`
panics), while `TestLoadFromInjectedIsHermetic` runs parallel precisely because it
injects the env and base dir. The mistakes to avoid: do not parallelize a test that
touches env or cwd — restructure it to inject the dependency instead; do not lean
on an inherited environment (clear `DB_DSN`/`PORT` with `t.Setenv("", ...)` when
asserting defaults, so a CI machine's env cannot leak in); and let `t.Chdir` and
`t.TempDir` own their teardown rather than a manual `os.Chdir`-back or
`os.RemoveAll`. Run `go test -race` to confirm the parallel `LoadFrom` tests never
touch shared state.

## Resources

- [`testing.T.Setenv`](https://pkg.go.dev/testing#T.Setenv) — sets an env var for the test and restores it via Cleanup; panics under parallel.
- [`testing.T.Chdir`](https://pkg.go.dev/testing#T.Chdir) — the Go 1.24 working-directory switch, restored via Cleanup; panics under parallel.
- [`testing.T.TempDir`](https://pkg.go.dev/testing#T.TempDir) — the unique, auto-removed directory this test writes its config into.
- [`errors.Is`](https://pkg.go.dev/errors#Is) — matching `os.ErrNotExist` through the `%w` wrap.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [07-tb-shared-fixture.md](07-tb-shared-fixture.md) | Next: [09-leak-guard-lifo-invariant.md](09-leak-guard-lifo-invariant.md)
