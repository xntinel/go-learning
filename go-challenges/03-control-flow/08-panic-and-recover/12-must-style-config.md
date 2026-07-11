# Exercise 12: Must-Style Config Constructors: Fail Fast at Startup, Recover Only in the Try Wrapper

**Nivel: Intermedio** — validacion rapida (un test corto).

`regexp.MustCompile` and `template.Must` share a pattern: panic on a
programmer/operator mistake that should never reach production, because there
is no sane way to keep running with a broken regular expression or a broken
config. This module builds that pattern for startup configuration —
`MustParseConfig`, meant to be called once from `main` and never recovered —
alongside `TryParseConfig`, the one narrow place that *does* recover it, for a
genuinely different caller: something validating a candidate config (a
hot-reload request) that needs an error back instead of a crashed process.

## What you'll build

```text
startupconfig/              independent module: example.com/startupconfig
  go.mod                    go 1.24
  config.go                 Config, ConfigError, ParseConfig, MustParseConfig, TryParseConfig
  config_test.go             valid/invalid parse, Must panics, Try converts the panic to an error
```

Files: `config.go`, `config_test.go`.
Implement: `ParseConfig(env map[string]string) (Config, error)`; `MustParseConfig(env map[string]string) Config` that panics with the `*ConfigError` on failure; `TryParseConfig(env map[string]string) (Config, error)` that recovers only an error-shaped panic and re-panics anything else.
Test: one table-driven test runs `ParseConfig` and `TryParseConfig` through the same five cases (valid, missing host, missing port, non-numeric port, zero port), since both share the `(Config, error)` signature and must agree; a second table test drives `MustParseConfig` through a valid and an invalid case, asserting it returns cleanly on the one and panics with an error value on the other.
Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/startupconfig
cd ~/go-exercises/startupconfig
go mod init example.com/startupconfig
go mod edit -go=1.24
```

Create `config.go`:

```go
package startupconfig

import (
	"fmt"
	"strconv"
)

// Config is the validated shape every caller actually wants to work with.
type Config struct {
	Host string
	Port int
}

// ParseConfig validates raw environment values and returns a normal error on
// any problem. This is the only function in the package that ever produces a
// *ConfigError as a return value instead of a panic.
func ParseConfig(env map[string]string) (Config, error) {
	host := env["HOST"]
	if host == "" {
		return Config{}, &ConfigError{Field: "HOST", Reason: "must not be empty"}
	}
	portStr, ok := env["PORT"]
	if !ok {
		return Config{}, &ConfigError{Field: "PORT", Reason: "missing"}
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 {
		return Config{}, &ConfigError{Field: "PORT", Reason: fmt.Sprintf("invalid value %q", portStr)}
	}
	return Config{Host: host, Port: port}, nil
}

// ConfigError reports which field failed validation and why.
type ConfigError struct {
	Field  string
	Reason string
}

func (e *ConfigError) Error() string {
	return fmt.Sprintf("startupconfig: %s: %s", e.Field, e.Reason)
}

// MustParseConfig is the Must-pattern constructor: called once, from main,
// before the server starts serving anything. A broken deployment config is a
// programmer/operator error the process should never start with — panicking
// here is deliberate, matching regexp.MustCompile and template.Must. It is
// NOT meant to be recovered anywhere in normal request-serving code; the
// three legitimate recover boundaries (request, goroutine, package edge) do
// not include "swallow a broken startup config and limp along."
func MustParseConfig(env map[string]string) Config {
	cfg, err := ParseConfig(env)
	if err != nil {
		panic(err)
	}
	return cfg
}

// TryParseConfig exists for exactly one different need: validating a
// candidate config (e.g. from a hot-reload request) where the caller needs an
// error back, not a crashed process. It is the one legitimate place that
// recovers a Must panic, and it does so narrowly: only an error value coming
// from MustParseConfig's own panic(err) is converted back to a return; any
// other panic is a real bug and must keep propagating.
func TryParseConfig(env map[string]string) (cfg Config, err error) {
	defer func() {
		if r := recover(); r != nil {
			e, ok := r.(error)
			if !ok {
				panic(r) // not our own panic(err) shape: a real bug, let it crash
			}
			err = e
		}
	}()
	return MustParseConfig(env), nil
}
```

Create `config_test.go`:

```go
package startupconfig

import (
	"errors"
	"testing"
)

func validEnv() map[string]string {
	return map[string]string{"HOST": "0.0.0.0", "PORT": "8080"}
}

// TestParseAndTry drives ParseConfig and TryParseConfig through the same
// table: both share the (Config, error) signature, and TryParseConfig's job
// is precisely to make MustParseConfig behave like ParseConfig for a caller
// that cannot tolerate a panic.
func TestParseAndTry(t *testing.T) {
	fns := map[string]func(map[string]string) (Config, error){
		"ParseConfig":    ParseConfig,
		"TryParseConfig": TryParseConfig,
	}
	cases := []struct {
		name      string
		env       map[string]string
		wantErr   bool
		wantField string
	}{
		{name: "valid", env: validEnv()},
		{name: "missing host", env: map[string]string{"PORT": "8080"}, wantErr: true, wantField: "HOST"},
		{name: "missing port", env: map[string]string{"HOST": "x"}, wantErr: true, wantField: "PORT"},
		{name: "non-numeric port", env: map[string]string{"HOST": "x", "PORT": "abc"}, wantErr: true, wantField: "PORT"},
		{name: "zero port", env: map[string]string{"HOST": "x", "PORT": "0"}, wantErr: true, wantField: "PORT"},
	}

	for fnName, fn := range fns {
		for _, c := range cases {
			t.Run(fnName+"/"+c.name, func(t *testing.T) {
				cfg, err := fn(c.env)
				if c.wantErr {
					if err == nil {
						t.Fatal("err = nil, want a *ConfigError")
					}
					var ce *ConfigError
					if !errors.As(err, &ce) {
						t.Fatalf("err is %T, want *ConfigError", err)
					}
					if ce.Field != c.wantField {
						t.Fatalf("ConfigError.Field = %q, want %q", ce.Field, c.wantField)
					}
					if cfg != (Config{}) {
						t.Fatalf("cfg = %+v, want zero value on error", cfg)
					}
					return
				}
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				if cfg.Host != "0.0.0.0" || cfg.Port != 8080 {
					t.Fatalf("cfg = %+v, want Host=0.0.0.0 Port=8080", cfg)
				}
			})
		}
	}
}

// TestMustParseConfig covers the one behavior TryParseConfig deliberately
// does not have: MustParseConfig panics on invalid input instead of
// returning an error, matching regexp.MustCompile's fail-fast contract.
func TestMustParseConfig(t *testing.T) {
	tests := []struct {
		name      string
		env       map[string]string
		wantPanic bool
	}{
		{name: "valid input returns cleanly", env: validEnv()},
		{name: "invalid input panics", env: map[string]string{"HOST": "x", "PORT": "not-a-number"}, wantPanic: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				r := recover()
				if tt.wantPanic && r == nil {
					t.Fatal("MustParseConfig did not panic on invalid config")
				}
				if tt.wantPanic {
					if _, ok := r.(error); !ok {
						t.Fatalf("panic value = %#v (%T), want an error", r, r)
					}
				} else if r != nil {
					t.Fatalf("unexpected panic: %v", r)
				}
			}()
			cfg := MustParseConfig(tt.env)
			if !tt.wantPanic && cfg.Port != 8080 {
				t.Fatalf("cfg.Port = %d, want 8080", cfg.Port)
			}
		})
	}
}
```

## Review

The design is correct when `MustParseConfig` is used exactly once, in `main`,
and nowhere pretends to recover it as ordinary error handling — a broken
deployment config failing fast, loudly, at process start is the entire point,
just like an invalid regular expression failing fast via
`regexp.MustCompile`. `TryParseConfig` is not a general-purpose safety net
around `MustParseConfig`; it exists for one different caller with one
different need (validating a candidate config without crashing a running
process), and it stays narrow by only converting a panic whose value is
exactly the error `MustParseConfig` itself panicked with — the `ok` check on
the type assertion re-panics anything else, so a genuine bug inside
`ParseConfig` (a nil map dereference, say) still crashes instead of being
misreported as "your config is invalid." Notice `TryParseConfig` never
duplicates `ParseConfig`'s validation logic; it composes `MustParseConfig`
instead, so the two entry points can never drift out of sync.

## Resources

- [regexp.MustCompile](https://pkg.go.dev/regexp#MustCompile) — the canonical Must-pattern in the standard library.
- [Go Blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover) — the mechanism `TryParseConfig`'s recover relies on.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [11-template-render-guard.md](11-template-render-guard.md) | Next: [13-json-path-extractor.md](13-json-path-extractor.md)
