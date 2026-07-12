# Exercise 6: Loading Config from Environment with Defaults and Failures

Every backend loads configuration from the environment, applies defaults, and
rejects nonsense. Testing that is where a table meets a hard constraint:
`t.Setenv` cannot run under `t.Parallel`, so this table is serial by design — and
knowing that is part of the pattern. This module builds `LoadConfig() (Config,
error)`, drives each row's environment with `t.Setenv`, and compares the whole
result struct with `cmp.Diff`.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. It uses `github.com/google/go-cmp`; nothing here imports another exercise.

## What you'll build

```text
envconfig/                independent module: example.com/envconfig
  go.mod                  go 1.26; requires github.com/google/go-cmp
  config.go               Config, LoadConfig, sentinel errors
  cmd/
    demo/
      main.go             sets env, loads config, prints it
  config_test.go          serial table over {name,env,wantConfig,wantErr} with cmp.Diff
```

- Files: `config.go`, `cmd/demo/main.go`, `config_test.go`.
- Implement: `LoadConfig()` reading `PORT`, `TIMEOUT`, `LOG_LEVEL` with defaults (8080, 30s, `info`) and validation, returning wrapped sentinel errors.
- Test: a serial table of `{name, env map, wantConfig, wantErr}` that calls `t.Setenv` per key and asserts `(err != nil) == wantErr` and `cmp.Diff(wantConfig, got) == ""`.
- Verify: `go test -count=1 -race ./...`

Set up the module. It depends on go-cmp:

```bash
go get github.com/google/go-cmp/cmp@v0.7.0
```

### Why this table is serial, and why cmp.Diff on the whole struct

`t.Setenv(key, value)` sets an environment variable for the duration of the test
and restores it automatically on cleanup — no manual `defer os.Setenv(...)`, no
leak into other tests. The catch is that it *panics* if the test or any parent is
marked parallel, because a parallel test cannot safely own a process-global like
the environment. So this table runs serially: neither the parent test nor the
subtests call `t.Parallel`. That is not a defect to work around; it is the correct
trade-off. `t.Setenv` buys leak-free cleanup, and the price is giving up
parallelism for env-dependent cases. Recognizing which tables must be serial —
anything touching the environment, the working directory, or another global — is a
senior habit.

Hermeticity here needs care. A row that expects defaults must not be polluted by an
ambient `PORT` set in your shell. The clean fix is to always set *all three* keys
in every subtest — a row that omits a key sets it to the empty string — and have
`LoadConfig` treat an empty value the same as unset: use the default. That way each
subtest fully determines the environment the loader sees, regardless of what was
there before.

The result is compared with `cmp.Diff(tc.wantConfig, got)` over the entire `Config`
struct, not field by field. For a small struct you *could* compare fields, but
`cmp.Diff` scales to any struct, reports a readable `-want +got` diff naming the
differing field, and is the same tool the later modules use for rich domain types.
An empty diff string means equal; a non-empty diff is the failure message, so the
output tells you exactly which field was wrong without your adding a `Fatalf` per
field.

Create `config.go`:

```go
package envconfig

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

// Sentinel errors for each rejected value.
var (
	ErrInvalidPort     = errors.New("invalid PORT")
	ErrInvalidTimeout  = errors.New("invalid TIMEOUT")
	ErrInvalidLogLevel = errors.New("invalid LOG_LEVEL")
)

// Config is the fully-resolved application configuration.
type Config struct {
	Port     int
	Timeout  time.Duration
	LogLevel string
}

var validLevels = map[string]bool{"debug": true, "info": true, "warn": true, "error": true}

// LoadConfig reads PORT, TIMEOUT, and LOG_LEVEL from the environment, applying
// defaults for empty or unset values and rejecting malformed ones.
func LoadConfig() (Config, error) {
	cfg := Config{Port: 8080, Timeout: 30 * time.Second, LogLevel: "info"}

	if v := os.Getenv("PORT"); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, fmt.Errorf("%q: %w", v, ErrInvalidPort)
		}
		if p < 1 || p > 65535 {
			return Config{}, fmt.Errorf("%d out of range: %w", p, ErrInvalidPort)
		}
		cfg.Port = p
	}

	if v := os.Getenv("TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, fmt.Errorf("%q: %w", v, ErrInvalidTimeout)
		}
		if d <= 0 {
			return Config{}, fmt.Errorf("%s not positive: %w", d, ErrInvalidTimeout)
		}
		cfg.Timeout = d
	}

	if v := os.Getenv("LOG_LEVEL"); v != "" {
		if !validLevels[v] {
			return Config{}, fmt.Errorf("%q: %w", v, ErrInvalidLogLevel)
		}
		cfg.LogLevel = v
	}

	return cfg, nil
}
```

### The runnable demo

The demo sets a couple of variables, loads the config, and prints it — showing an
override for `PORT` and defaults for the rest.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"

	"example.com/envconfig"
)

func main() {
	os.Setenv("PORT", "9090")
	os.Setenv("LOG_LEVEL", "debug")

	cfg, err := envconfig.LoadConfig()
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("port=%d timeout=%s level=%s\n", cfg.Port, cfg.Timeout, cfg.LogLevel)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
port=9090 timeout=30s level=debug
```

### The tests

Each row supplies an `env` map; the subtest calls `t.Setenv` for all three keys
(missing keys become empty, which the loader reads as "use default"), then
`LoadConfig`. Neither the parent nor the subtests are parallel — `t.Setenv`
forbids it. The whole `Config` is compared with `cmp.Diff`.

Create `config_test.go`:

```go
package envconfig

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

func TestLoadConfig(t *testing.T) {
	// No t.Parallel: t.Setenv forbids it.

	envKeys := []string{"PORT", "TIMEOUT", "LOG_LEVEL"}

	tests := []struct {
		name       string
		env        map[string]string
		wantConfig Config
		wantErr    bool
	}{
		{
			name:       "all_defaults",
			env:        map[string]string{},
			wantConfig: Config{Port: 8080, Timeout: 30 * time.Second, LogLevel: "info"},
		},
		{
			name:       "all_overridden",
			env:        map[string]string{"PORT": "9090", "TIMEOUT": "5s", "LOG_LEVEL": "debug"},
			wantConfig: Config{Port: 9090, Timeout: 5 * time.Second, LogLevel: "debug"},
		},
		{
			name:       "partial_override",
			env:        map[string]string{"PORT": "3000"},
			wantConfig: Config{Port: 3000, Timeout: 30 * time.Second, LogLevel: "info"},
		},
		{
			name:    "bad_port",
			env:     map[string]string{"PORT": "not-a-number"},
			wantErr: true,
		},
		{
			name:    "port_out_of_range",
			env:     map[string]string{"PORT": "70000"},
			wantErr: true,
		},
		{
			name:    "bad_timeout",
			env:     map[string]string{"TIMEOUT": "quickly"},
			wantErr: true,
		},
		{
			name:    "bad_log_level",
			env:     map[string]string{"LOG_LEVEL": "verbose"},
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// No t.Parallel here either: t.Setenv requires a serial test.
			for _, k := range envKeys {
				t.Setenv(k, tc.env[k]) // missing key -> "" -> default
			}

			got, err := LoadConfig()
			if (err != nil) != tc.wantErr {
				t.Fatalf("LoadConfig() error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if diff := cmp.Diff(tc.wantConfig, got); diff != "" {
				t.Fatalf("LoadConfig() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
```

## Review

The loader is correct when an empty or unset variable yields the default and a
malformed one yields the matching sentinel, and the table covers all three
columns of behavior: defaults, override, rejection. The structural lesson is the
serial constraint — neither the parent nor the subtests may call `t.Parallel`,
because `t.Setenv` panics under parallelism. If you add `t.Parallel()` anywhere in
this test, it panics at run time; that is the language enforcing the trade-off, not
a bug in your setup.

Two details keep the cases honest. Setting all three keys in every subtest (missing
ones to empty) makes each row fully determine the environment, so an ambient `PORT`
in your shell cannot leak into the `all_defaults` row. And comparing the whole
struct with `cmp.Diff` rather than field-by-field means the failure message names
the wrong field for you. On the error rows, `return` immediately after asserting
`wantErr` so a `Config{}` zero value is never diffed against an expected config.

## Resources

- [testing.T.Setenv](https://pkg.go.dev/testing#T.Setenv) — sets an env var with automatic cleanup; forbidden under parallel.
- [time.ParseDuration](https://pkg.go.dev/time#ParseDuration) and [strconv.Atoi](https://pkg.go.dev/strconv#Atoi) — the parsers used here.
- [google/go-cmp](https://pkg.go.dev/github.com/google/go-cmp/cmp) — `cmp.Diff` for whole-struct comparison.

---

Back to [00-concepts.md](00-concepts.md) | Next: [07-parser-with-error-cases.md](07-parser-with-error-cases.md)
