# Exercise 9: Convert Environment Strings Into A Typed Config Struct

Every service reads configuration from string environment variables and must
convert them into typed fields: an `int` pool size, a `bool` feature flag, a
`time.Duration` timeout, a validated enum. This is a string-to-typed boundary —
distinct from interface assertions but governed by the same representability
discipline — and the production-grade version reports *every* bad field at once
instead of failing on the first.

This module is fully self-contained: its own module, all code inline, its own
demo and tests.

## What you'll build

```text
envconfig/                   independent module: example.com/envconfig
  go.mod                     go 1.26
  config.go                  type Config; Load(lookup) (Config, error) with errors.Join
  cmd/
    demo/
      main.go                runnable demo: load a valid env and a broken one
  config_test.go             all-valid, defaults, and each invalid conversion aggregated
```

- Files: `config.go`, `cmd/demo/main.go`, `config_test.go`.
- Implement: `Load(lookup LookupFunc) (Config, error)` parsing `int`, `bool`, `time.Duration`, and a validated enum, accumulating per-field errors with `errors.Join`.
- Test: a table with a fake map-based lookup covering all-valid, missing-with-default, and each invalid conversion, asserting `errors.Join` reports every bad field at once and valid fields still parse.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/02-variables-types-and-constants/05-type-conversions-and-type-assertions/09-typed-config-from-env/cmd/demo
cd go-solutions/02-variables-types-and-constants/05-type-conversions-and-type-assertions/09-typed-config-from-env
go mod edit -go=1.26
```

### Conversion as parsing, and accumulate-don't-abort

Environment values arrive as strings, so this boundary uses the `strconv` /
`time` *parsers*, not conversions or assertions: `strconv.Atoi` for the pool
size, `strconv.ParseBool` for the flag, `time.ParseDuration` for the timeout, and
a membership check for the enum. Each parser is a representability claim in the
same sense as a numeric conversion — "this text denotes a value of the target
type" — and it can fail, which is the point.

The design choice that separates a toy loader from a real one is error handling.
Failing on the first bad variable forces an operator into a fix-restart-discover
loop: correct `POOL_SIZE`, redeploy, learn `TIMEOUT` is also wrong, repeat. The
production form parses every field, collects each failure into a slice, and
returns them together with `errors.Join`, which builds a single error whose
`Error()` lists all of them and which `errors.Is` can still match against each
constituent. The operator sees the full list of misconfigured variables in one
shot. Fields that parse correctly are still populated in the returned struct even
when a sibling failed, and a missing variable falls back to a documented default
via `os.LookupEnv`'s comma-ok result rather than erroring — absence is a valid
state, a malformed value is not.

`Load` takes a `LookupFunc` (`func(key string) (string, bool)`) instead of
calling `os.LookupEnv` directly, so the tests inject a map and the demo passes
`os.LookupEnv`. That injection is the same boundary discipline: the dependency on
the process environment is a parameter, not a hidden global.

Create `config.go`:

```go
// config.go
package envconfig

import (
	"errors"
	"fmt"
	"strconv"
	"time"
)

// LookupFunc resolves an environment key, reporting whether it was set.
type LookupFunc func(key string) (string, bool)

// Config is the typed configuration the service consumes.
type Config struct {
	PoolSize int
	Feature  bool
	Timeout  time.Duration
	Mode     string
}

var validModes = map[string]bool{"prod": true, "staging": true, "dev": true}

// Load reads and converts every configuration field, accumulating all field
// errors and returning them together via errors.Join.
func Load(lookup LookupFunc) (Config, error) {
	cfg := Config{
		PoolSize: 10,
		Feature:  false,
		Timeout:  30 * time.Second,
		Mode:     "prod",
	}
	var errs []error

	if v, ok := lookup("POOL_SIZE"); ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("POOL_SIZE: %w", err))
		} else if n < 1 {
			errs = append(errs, fmt.Errorf("POOL_SIZE: must be >= 1, got %d", n))
		} else {
			cfg.PoolSize = n
		}
	}

	if v, ok := lookup("FEATURE_X"); ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("FEATURE_X: %w", err))
		} else {
			cfg.Feature = b
		}
	}

	if v, ok := lookup("TIMEOUT"); ok {
		d, err := time.ParseDuration(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("TIMEOUT: %w", err))
		} else {
			cfg.Timeout = d
		}
	}

	if v, ok := lookup("MODE"); ok {
		if !validModes[v] {
			errs = append(errs, fmt.Errorf("MODE: %q is not one of prod/staging/dev", v))
		} else {
			cfg.Mode = v
		}
	}

	return cfg, errors.Join(errs...)
}
```

### The runnable demo

Create `cmd/demo/main.go`. It builds a map-backed lookup so the demo is
deterministic, then shows both a clean load and one that aggregates two errors:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/envconfig"
)

func mapLookup(m map[string]string) envconfig.LookupFunc {
	return func(key string) (string, bool) {
		v, ok := m[key]
		return v, ok
	}
}

func main() {
	good, err := envconfig.Load(mapLookup(map[string]string{
		"POOL_SIZE": "25",
		"FEATURE_X": "true",
		"TIMEOUT":   "5s",
		"MODE":      "staging",
	}))
	fmt.Printf("good: %+v err=%v\n", good, err)

	bad, err := envconfig.Load(mapLookup(map[string]string{
		"POOL_SIZE": "lots",
		"TIMEOUT":   "5",
		"MODE":      "prod",
	}))
	fmt.Printf("bad:  %+v\n", bad)
	fmt.Printf("errors:\n%v\n", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
good: {PoolSize:25 Feature:true Timeout:5s Mode:staging} err=<nil>
bad:  {PoolSize:10 Feature:false Timeout:30s Mode:prod}
errors:
POOL_SIZE: strconv.Atoi: parsing "lots": invalid syntax
TIMEOUT: time: missing unit in duration "5"
```

### Tests

The tests use a map-backed lookup. They assert the all-valid path, the
missing-with-default path, and that an env with multiple bad fields reports every
one through the joined error while still parsing the valid siblings.

Create `config_test.go`:

```go
// config_test.go
package envconfig

import (
	"strings"
	"testing"
	"time"
)

func lookupFrom(m map[string]string) LookupFunc {
	return func(key string) (string, bool) {
		v, ok := m[key]
		return v, ok
	}
}

func TestLoadValid(t *testing.T) {
	t.Parallel()
	cfg, err := Load(lookupFrom(map[string]string{
		"POOL_SIZE": "25",
		"FEATURE_X": "1",
		"TIMEOUT":   "2m",
		"MODE":      "dev",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := Config{PoolSize: 25, Feature: true, Timeout: 2 * time.Minute, Mode: "dev"}
	if cfg != want {
		t.Fatalf("Config = %+v, want %+v", cfg, want)
	}
}

func TestLoadDefaults(t *testing.T) {
	t.Parallel()
	cfg, err := Load(lookupFrom(map[string]string{})) // nothing set
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := Config{PoolSize: 10, Feature: false, Timeout: 30 * time.Second, Mode: "prod"}
	if cfg != want {
		t.Fatalf("defaults = %+v, want %+v", cfg, want)
	}
}

func TestLoadAggregatesErrors(t *testing.T) {
	t.Parallel()
	cfg, err := Load(lookupFrom(map[string]string{
		"POOL_SIZE": "lots",   // bad
		"FEATURE_X": "maybe",  // bad
		"TIMEOUT":   "5",      // bad (no unit)
		"MODE":      "canary", // bad enum
	}))
	if err == nil {
		t.Fatal("expected aggregated error, got nil")
	}
	msg := err.Error()
	for _, field := range []string{"POOL_SIZE", "FEATURE_X", "TIMEOUT", "MODE"} {
		if !strings.Contains(msg, field) {
			t.Errorf("aggregated error missing %s: %q", field, msg)
		}
	}
	// Valid defaults survive alongside the errors.
	if cfg.PoolSize != 10 || cfg.Mode != "prod" {
		t.Fatalf("expected untouched defaults on error, got %+v", cfg)
	}
}

func TestLoadPartialValidSurvives(t *testing.T) {
	t.Parallel()
	cfg, err := Load(lookupFrom(map[string]string{
		"POOL_SIZE": "50",   // valid, must survive
		"TIMEOUT":   "nope", // invalid
	}))
	if err == nil {
		t.Fatal("expected error for bad TIMEOUT")
	}
	if cfg.PoolSize != 50 {
		t.Fatalf("valid POOL_SIZE lost: %+v", cfg)
	}
}
```

## Review

The loader is correct when a well-formed environment yields the exact typed
struct, an empty environment yields the documented defaults with no error, and a
broken environment reports *all* bad fields through one `errors.Join` while
leaving the valid fields populated. The aggregation test is the differentiator:
it sets four bad variables and asserts every field name appears in the joined
message, which is the operator experience that turns a multi-round debugging
session into a single fix. The parsers are the string-boundary analogue of the
numeric and interface boundaries in the earlier exercises — each `ParseX` is a
representability claim that either succeeds into a typed value or fails into a
named error, and nothing downstream ever sees the raw string.

## Resources

- [os.LookupEnv](https://pkg.go.dev/os#LookupEnv) — reading an env var with a present/absent boolean.
- [strconv.ParseBool](https://pkg.go.dev/strconv#ParseBool) — the accepted boolean spellings (`1`, `t`, `true`, ...).
- [time.ParseDuration](https://pkg.go.dev/time#ParseDuration) — parsing `300ms`, `2m`, `1h30m` into a `time.Duration`.
- [errors.Join](https://pkg.go.dev/errors#Join) — combining multiple errors into one that `errors.Is` still matches.

---

Back to [00-concepts.md](00-concepts.md) | Next: [../06-type-aliases-vs-type-definitions/00-concepts.md](../06-type-aliases-vs-type-definitions/00-concepts.md)
