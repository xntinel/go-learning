# Exercise 8: A Config-Loader Test Helper Using t.Setenv (and Why It's Serial)

Config loaded from the environment is standard for a twelve-factor backend, and
testing it means setting env vars, loading, and restoring the prior environment.
`t.Setenv` does the set-and-restore for you — but it *panics* if the test is
parallel, because the environment is process-global. This module builds a
config-loader helper on `t.Setenv` and encodes the hard constraint that any test
using it is inherently serial.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
envconfig/                   independent module: example.com/envconfig
  go.mod                     go 1.26
  config.go                  Config; Load() from APP_* env; ErrMissing/ErrMalformed
  cmd/
    demo/
      main.go                sets env, loads, prints the config
  config_test.go             loadConfig(t, env) helper via t.Setenv; success and error subtests (serial)
```

- Files: `config.go`, `cmd/demo/main.go`, `config_test.go`.
- Implement: `Load()` reading `APP_HOST`, `APP_PORT`, `APP_DEBUG`, returning a typed `Config` or a wrapped `ErrMissing`/`ErrMalformed`.
- Test: `loadConfig(t, env)` applies `t.Setenv` for each key then calls `Load`; subtests assert parsed values and error cases; none use `t.Parallel` (documented as the serial contract).
- Verify: `go test -count=1 -race ./...`

### Why the helper is inherently serial

`t.Setenv(k, v)` calls `os.Setenv` and registers a `Cleanup` to restore the prior
value when the test ends — exactly the isolation a config test wants. But the
environment is a single process-global table shared by every goroutine, so the
`testing` package forbids `t.Setenv` in a parallel test: it "cannot be used in
parallel tests or tests with parallel ancestors," and *panics* at runtime if you
try. That is not a limitation to work around; it is a correctness guarantee. If two
parallel tests each set `APP_PORT` to different values, neither can trust what
`os.Getenv` returns.

So a helper built on `t.Setenv` carries a contract: **any test that calls it must
not call `t.Parallel()`, and neither may any ancestor.** A senior documents that
on the helper rather than letting a teammate discover it through a mysterious
mid-run panic. The trade-off is deliberate: you accept serial execution of these
tests in exchange for real environment isolation with automatic restore. (When you
need parallel config tests, the alternative is to make `Load` take an explicit
lookup function — `Load(getenv func(string) string)` — instead of reading the
global environment; that is the design that decouples config from process state.)

Create `config.go`:

```go
package envconfig

import (
	"errors"
	"fmt"
	"os"
	"strconv"
)

// Sentinels callers branch on with errors.Is.
var (
	ErrMissing   = errors.New("missing required variable")
	ErrMalformed = errors.New("malformed variable")
)

// Config is the strongly-typed application configuration.
type Config struct {
	Host  string
	Port  int
	Debug bool
}

// Load reads Config from APP_HOST (required), APP_PORT (required int), and
// APP_DEBUG (optional bool, default false).
func Load() (Config, error) {
	host, ok := os.LookupEnv("APP_HOST")
	if !ok || host == "" {
		return Config{}, fmt.Errorf("APP_HOST: %w", ErrMissing)
	}

	portStr, ok := os.LookupEnv("APP_PORT")
	if !ok || portStr == "" {
		return Config{}, fmt.Errorf("APP_PORT: %w", ErrMissing)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return Config{}, fmt.Errorf("APP_PORT %q: %w", portStr, ErrMalformed)
	}

	debug := false
	if v, ok := os.LookupEnv("APP_DEBUG"); ok && v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return Config{}, fmt.Errorf("APP_DEBUG %q: %w", v, ErrMalformed)
		}
		debug = b
	}

	return Config{Host: host, Port: port, Debug: debug}, nil
}
```

### The runnable demo

The demo sets the env directly with `os.Setenv` (no `*testing.T` outside a test),
loads, and prints the typed result.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"

	"example.com/envconfig"
)

func main() {
	os.Setenv("APP_HOST", "api.internal")
	os.Setenv("APP_PORT", "8443")
	os.Setenv("APP_DEBUG", "true")

	cfg, err := envconfig.Load()
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("host=%s port=%d debug=%v\n", cfg.Host, cfg.Port, cfg.Debug)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
host=api.internal port=8443 debug=true
```

### The tests

`loadConfig(t, env)` is the helper: it applies `t.Setenv` for each key, then calls
`Load` and returns its result — so each subtest declares only the environment it
cares about. Note what is *absent*: no `t.Parallel()` anywhere in this file. That
is the serial contract the `t.Setenv` foundation forces. The success subtest asserts
parsed values; the error subtests assert `errors.Is` against `ErrMissing` (absent
required var) and `ErrMalformed` (non-numeric port, non-bool debug).

Create `config_test.go`:

```go
package envconfig

import (
	"errors"
	"testing"
)

// loadConfig applies env via t.Setenv, then loads. It must NOT be used in a
// parallel test: t.Setenv panics under t.Parallel because the environment is
// process-global. Every test in this file is therefore serial by construction.
func loadConfig(t *testing.T, env map[string]string) (Config, error) {
	t.Helper()
	for k, v := range env {
		t.Setenv(k, v)
	}
	return Load()
}

func TestLoadValid(t *testing.T) {
	cfg, err := loadConfig(t, map[string]string{
		"APP_HOST":  "db.internal",
		"APP_PORT":  "5432",
		"APP_DEBUG": "true",
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Host != "db.internal" || cfg.Port != 5432 || !cfg.Debug {
		t.Fatalf("Config = %+v, want {db.internal 5432 true}", cfg)
	}
}

func TestLoadDefaultsDebugFalse(t *testing.T) {
	cfg, err := loadConfig(t, map[string]string{
		"APP_HOST": "cache.internal",
		"APP_PORT": "6379",
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Debug {
		t.Fatal("Debug = true, want false when APP_DEBUG unset")
	}
}

func TestLoadErrors(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want error
	}{
		{"missing host", map[string]string{"APP_PORT": "80"}, ErrMissing},
		{"missing port", map[string]string{"APP_HOST": "h"}, ErrMissing},
		{"bad port", map[string]string{"APP_HOST": "h", "APP_PORT": "eighty"}, ErrMalformed},
		{"bad debug", map[string]string{"APP_HOST": "h", "APP_PORT": "80", "APP_DEBUG": "maybe"}, ErrMalformed},
	}
	for _, c := range cases {
		// t.Run subtests, but NOT parallel: t.Setenv forbids it.
		t.Run(c.name, func(t *testing.T) {
			_, err := loadConfig(t, c.env)
			if !errors.Is(err, c.want) {
				t.Fatalf("Load err = %v, want %v", err, c.want)
			}
		})
	}
}
```

## Review

The loader is correct when a required missing var yields `errors.Is(err,
ErrMissing)`, a non-numeric port or non-bool debug yields `errors.Is(err,
ErrMalformed)`, and an unset `APP_DEBUG` defaults to `false`. The helper is correct
when it uses `t.Setenv` (so restoration is automatic and per-test) and no test in
the file is parallel — adding `t.Parallel()` to any of them would panic at runtime,
which is the constraint the helper encodes. Confirm isolation by observing that
`TestLoadErrors`'s `missing host` case does not see `APP_HOST` from a prior case:
`t.Setenv`'s cleanup restores the environment between subtests. Run
`go test -race -count=1`; the tests are serial by design, and that is the correct
trade for process-global state. To recover parallelism, refactor `Load` to accept
an injected `getenv func(string) string`.

## Resources

- [testing.T.Setenv](https://pkg.go.dev/testing#T.Setenv) — set-and-restore, and the parallel-test prohibition.
- [os.LookupEnv](https://pkg.go.dev/os#LookupEnv) — distinguishing an unset var from an empty one.
- [strconv.Atoi / ParseBool](https://pkg.go.dev/strconv#Atoi) — typed parsing with error returns.

---

Back to [00-concepts.md](00-concepts.md) | Next: [09-generic-case-runner.md](09-generic-case-runner.md)
