# Exercise 7: Config Loader: Shadowed err Leaves Zero-Value Config

An environment config loader that reads variables, parses ints and durations, and
aggregates every failure. The trap is a nested-scope shadow: reusing
`cfg, err := parse()` inside an inner block declares fresh variables, silently
discarding the partially built outer `cfg` or returning a stale `nil` error. The
fix is to declare result variables once and reassign fields, returning the zero
`Config` on any failure.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
appconfig/                      module: example.com/appconfig
  go.mod
  config.go                     Config, Load, sentinel errors, errors.Join aggregation
  cmd/
    demo/
      main.go                   loads from os.LookupEnv; shows a valid and an invalid case
  config_test.go                valid load, invalid int returns zero Config, aggregated errors
```

- Files: `config.go`, `cmd/demo/main.go`, `config_test.go`.
- Implement: `Load(lookup)` that parses `APP_ENV`, `APP_PORT`, `APP_TIMEOUT`, aggregating failures with `errors.Join` and returning the zero `Config` on any error.
- Test: valid env yields a fully populated `Config`; a malformed int returns a wrapped `ErrInvalidInt` and the zero `Config`; multiple bad fields aggregate so `errors.Is` matches each.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/02-variables-types-and-constants/09-blank-identifier-and-shadowing/07-config-loader-shadow/cmd/demo
cd go-solutions/02-variables-types-and-constants/09-blank-identifier-and-shadowing/07-config-loader-shadow
```

### The shadow that leaks a half-built config

The tempting but wrong shape reuses `:=` inside a nested block:

```go
// Wrong: the inner cfg and err shadow the outer ones.
func Load(lookup Lookup) (Config, error) {
	var cfg Config
	var err error
	if v, ok := lookup("APP_PORT"); ok {
		if cfg, err := parsePort(cfg, v); err != nil { // new inner cfg, err
			return cfg, err
		}
		// the assignment to the inner cfg is lost here; the outer cfg is unchanged
	}
	return cfg, err // outer err is still nil even if a later step failed
}
```

Two bugs hide in that block. First, `cfg, err := parsePort(...)` declares a fresh
`cfg` scoped to the `if`; the successfully parsed port is written into the inner
`cfg` and thrown away when the block ends, so the outer `cfg` never gets it.
Second, because the outer `err` is never assigned, `return cfg, err` at the bottom
can return `nil` even after a real failure elsewhere.

The correct loader declares the results once and assigns *fields* of `cfg`
directly (no same-named inner variable can shadow a field), collects each failure
in a slice, and — critically — returns the zero `Config` when anything failed, so
a caller never receives a half-populated struct that looks valid. Aggregation uses
`errors.Join`, which produces an error that `errors.Is` matches against every
joined sentinel.

Create `config.go`:

```go
package config

import (
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Lookup resolves an env var, reporting presence (os.LookupEnv has this shape).
type Lookup func(key string) (value string, ok bool)

// Config is the fully parsed application configuration.
type Config struct {
	Env     string
	Port    int
	Timeout time.Duration
}

// Sentinel errors, wrapped with %w so errors.Is matches each.
var (
	ErrMissingEnv      = errors.New("required env var missing")
	ErrInvalidInt      = errors.New("invalid integer")
	ErrInvalidDuration = errors.New("invalid duration")
)

// Load reads and validates configuration. On any failure it returns the zero
// Config and a joined error, never a partially built struct.
func Load(lookup Lookup) (Config, error) {
	var (
		cfg  Config
		errs []error
	)

	if v, ok := lookup("APP_ENV"); ok {
		cfg.Env = v
	} else {
		errs = append(errs, fmt.Errorf("APP_ENV: %w", ErrMissingEnv))
	}

	if v, ok := lookup("APP_PORT"); ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("APP_PORT %q: %w", v, ErrInvalidInt))
		} else {
			cfg.Port = n
		}
	} else {
		errs = append(errs, fmt.Errorf("APP_PORT: %w", ErrMissingEnv))
	}

	if v, ok := lookup("APP_TIMEOUT"); ok {
		d, err := time.ParseDuration(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("APP_TIMEOUT %q: %w", v, ErrInvalidDuration))
		} else {
			cfg.Timeout = d
		}
	} else {
		errs = append(errs, fmt.Errorf("APP_TIMEOUT: %w", ErrMissingEnv))
	}

	if len(errs) > 0 {
		return Config{}, errors.Join(errs...)
	}
	return cfg, nil
}
```

### The runnable demo

The demo sets env vars and loads through `os.LookupEnv` (whose signature is
exactly `Lookup`), then breaks one value to show the wrapped parse error.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"

	"example.com/appconfig"
)

func main() {
	os.Setenv("APP_ENV", "prod")
	os.Setenv("APP_PORT", "8080")
	os.Setenv("APP_TIMEOUT", "30s")

	cfg, err := config.Load(os.LookupEnv)
	if err != nil {
		fmt.Println("config error:", err)
		return
	}
	fmt.Printf("env=%s port=%d timeout=%s\n", cfg.Env, cfg.Port, cfg.Timeout)

	os.Setenv("APP_PORT", "eighty")
	if _, err := config.Load(os.LookupEnv); err != nil {
		fmt.Println("config error:", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
env=prod port=8080 timeout=30s
config error: APP_PORT "eighty": invalid integer
```

### Tests

`TestLoadInvalidPortReturnsZero` proves the loader does not leak a half-built
struct: the returned `Config` must be the zero value, and the error must wrap
`ErrInvalidInt`. `TestLoadAggregates` proves `errors.Join` matches every sentinel.

Create `config_test.go`:

```go
package config

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func mapLookup(m map[string]string) Lookup {
	return func(key string) (string, bool) {
		v, ok := m[key]
		return v, ok
	}
}

func TestLoadValid(t *testing.T) {
	t.Parallel()

	cfg, err := Load(mapLookup(map[string]string{
		"APP_ENV":     "staging",
		"APP_PORT":    "9090",
		"APP_TIMEOUT": "15s",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := Config{Env: "staging", Port: 9090, Timeout: 15 * time.Second}
	if cfg != want {
		t.Fatalf("Config = %+v, want %+v", cfg, want)
	}
}

func TestLoadInvalidPortReturnsZero(t *testing.T) {
	t.Parallel()

	cfg, err := Load(mapLookup(map[string]string{
		"APP_ENV":     "prod",
		"APP_PORT":    "eighty",
		"APP_TIMEOUT": "30s",
	}))
	if !errors.Is(err, ErrInvalidInt) {
		t.Fatalf("error = %v, want ErrInvalidInt", err)
	}
	if cfg != (Config{}) {
		t.Fatalf("Config = %+v, want zero value (no half-built leak)", cfg)
	}
}

func TestLoadAggregates(t *testing.T) {
	t.Parallel()

	_, err := Load(mapLookup(map[string]string{
		"APP_PORT":    "eighty",
		"APP_TIMEOUT": "notaduration",
	}))
	if !errors.Is(err, ErrMissingEnv) {
		t.Fatal("missing APP_ENV not reported")
	}
	if !errors.Is(err, ErrInvalidInt) {
		t.Fatal("invalid port not reported")
	}
	if !errors.Is(err, ErrInvalidDuration) {
		t.Fatal("invalid timeout not reported")
	}
}

func ExampleLoad() {
	cfg, _ := Load(mapLookup(map[string]string{
		"APP_ENV":     "dev",
		"APP_PORT":    "3000",
		"APP_TIMEOUT": "5s",
	}))
	fmt.Printf("%s %d %s\n", cfg.Env, cfg.Port, cfg.Timeout)
	// Output: dev 3000 5s
}
```

## Review

The loader is correct when a failure never leaks a partial `Config` and never
returns a stale `nil` error. `TestLoadInvalidPortReturnsZero` fails if the code
returns the half-built struct; `TestLoadAggregates` fails if any failure is
dropped. The structural discipline is to declare `cfg` and `errs` once and write
fields, so no nested `:=` can shadow them, and to return `Config{}` on error. Never
reuse `cfg, err := ...` inside a nested block when the outer `cfg` still matters.

## Resources

- [`errors.Join`](https://pkg.go.dev/errors#Join) — aggregating multiple errors so `errors.Is` matches each.
- [`os.LookupEnv`](https://pkg.go.dev/os#LookupEnv) — presence-aware env reads.
- [`strconv.Atoi`](https://pkg.go.dev/strconv#Atoi) and [`time.ParseDuration`](https://pkg.go.dev/time#ParseDuration) — the parsers used here.

---

Back to [00-concepts.md](00-concepts.md) | Next: [08-retryable-error-assertion.md](08-retryable-error-assertion.md)
