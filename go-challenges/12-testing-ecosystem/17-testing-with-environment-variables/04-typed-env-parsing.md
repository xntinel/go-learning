# Exercise 4: Parse Durations, Bools, And Bounded Ints From Env Into A Typed Config

Environment variables are strings; a config struct holds `time.Duration`, `int`,
and `bool`. This exercise builds the typed parsing layer a real service needs —
an HTTP timeout, a bounded connection-pool size, and a feature flag — with each
failure wrapped so it names the offending key and value.

## What you'll build

```text
typedconfig/               independent module: example.com/typedconfig
  go.mod                   go directive supplied by the gate
  config.go                Config{HTTPTimeout,MaxConns,FeatureXEnabled}; Load(); parse sentinels
  cmd/
    demo/
      main.go              runnable demo: parse a valid env set, print typed values
  config_test.go           per-field valid/invalid tables; error-message assertions
```

Files: `config.go`, `cmd/demo/main.go`, `config_test.go`.
Implement: `Load()` parsing `HTTP_TIMEOUT` (duration), `MAX_CONNS` (bounded int), `FEATURE_X` (bool), wrapping `ErrInvalidDuration`/`ErrInvalidInt`/`ErrOutOfRange`/`ErrInvalidBool`.
Test: per-field valid/invalid tables via `t.Setenv`; assert exact `time.Duration`, the truthy/falsy `ParseBool` set, range rejection, and that messages name the key.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/17-testing-with-environment-variables/04-typed-env-parsing/cmd/demo
cd go-solutions/12-testing-ecosystem/17-testing-with-environment-variables/04-typed-env-parsing
```

## Typed parsing, and why the error message is half the job

Each field uses the right stdlib parser: `time.ParseDuration` for `HTTP_TIMEOUT`,
`strconv.Atoi` plus a range check for `MAX_CONNS`, and `strconv.ParseBool` for
`FEATURE_X`. The parsing itself is a one-liner each; the value of the code is in
the failure paths.

`time.ParseDuration` requires a unit. `HTTP_TIMEOUT=1500ms` and `HTTP_TIMEOUT=2s`
parse; `HTTP_TIMEOUT=30` does *not*, because a bare number has no unit. This is
one of the most common config mistakes in Go services, and it is only debuggable
quickly if the error says `HTTP_TIMEOUT="30": invalid duration` rather than a bare
`time: missing unit in duration "30"`. So every parse failure is wrapped with
`fmt.Errorf("KEY=%q: %w", value, sentinel)`: the operator sees which variable and
what value, and the caller can still match the sentinel with `errors.Is`.

`strconv.ParseBool` accepts a fixed truthy set — `1, t, T, TRUE, true, True` — and
the matching falsy set, and rejects everything else. That means `FEATURE_X=yes`
or `FEATURE_X=on` is an *error*, not a silent false; catching it at startup beats
shipping with a feature you thought you had enabled. `MAX_CONNS` parses as an int
and is then range-checked against a sane `[1, 512]` window, because a pool size of
`0` or `100000` is a real deployment error `strconv.Atoi` is happy to accept.

Create `config.go`:

```go
package typedconfig

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

// Parse-failure sentinels, one per failure mode.
var (
	ErrInvalidDuration = errors.New("invalid duration")
	ErrInvalidInt      = errors.New("invalid integer")
	ErrOutOfRange      = errors.New("value out of range")
	ErrInvalidBool     = errors.New("invalid boolean")
)

const (
	minConns = 1
	maxConns = 512
)

// Config holds the typed service configuration.
type Config struct {
	HTTPTimeout     time.Duration
	MaxConns        int
	FeatureXEnabled bool
}

// Load parses HTTP_TIMEOUT, MAX_CONNS, and FEATURE_X into typed fields. Every
// parse error is wrapped with the offending key and value.
func Load() (Config, error) {
	var cfg Config

	timeoutStr := os.Getenv("HTTP_TIMEOUT")
	d, err := time.ParseDuration(timeoutStr)
	if err != nil {
		return Config{}, fmt.Errorf("HTTP_TIMEOUT=%q: %w", timeoutStr, ErrInvalidDuration)
	}
	cfg.HTTPTimeout = d

	connsStr := os.Getenv("MAX_CONNS")
	n, err := strconv.Atoi(connsStr)
	if err != nil {
		return Config{}, fmt.Errorf("MAX_CONNS=%q: %w", connsStr, ErrInvalidInt)
	}
	if n < minConns || n > maxConns {
		return Config{}, fmt.Errorf("MAX_CONNS=%d not in [%d,%d]: %w", n, minConns, maxConns, ErrOutOfRange)
	}
	cfg.MaxConns = n

	featStr := os.Getenv("FEATURE_X")
	b, err := strconv.ParseBool(featStr)
	if err != nil {
		return Config{}, fmt.Errorf("FEATURE_X=%q: %w", featStr, ErrInvalidBool)
	}
	cfg.FeatureXEnabled = b

	return cfg, nil
}
```

## The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"

	"example.com/typedconfig"
)

func main() {
	os.Setenv("HTTP_TIMEOUT", "1500ms")
	os.Setenv("MAX_CONNS", "64")
	os.Setenv("FEATURE_X", "true")

	cfg, err := typedconfig.Load()
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("timeout=%s max_conns=%d feature_x=%t\n",
		cfg.HTTPTimeout, cfg.MaxConns, cfg.FeatureXEnabled)

	os.Setenv("HTTP_TIMEOUT", "30") // missing unit
	if _, err := typedconfig.Load(); err != nil {
		fmt.Println("error:", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
timeout=1.5s max_conns=64 feature_x=true
error: HTTP_TIMEOUT="30": invalid duration
```

## Tests

Each field gets a valid/invalid table. The duration table asserts the exact
`time.Duration` value (so `1500ms` becomes `1500 * time.Millisecond`) and rejects
the unit-less `30`. The bool table walks the `ParseBool` truthy and falsy sets and
rejects `yes`. The int table rejects both non-numeric and out-of-range values. The
invalid cases also assert the error message names the key with `strings.Contains`,
so a regression that drops the key from the message is caught.

Create `config_test.go`:

```go
package typedconfig

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// setEnv holds all three variables at valid defaults, then applies overrides,
// so a per-field table can vary one variable at a time.
func setEnv(t *testing.T, overrides map[string]string) {
	t.Helper()
	base := map[string]string{
		"HTTP_TIMEOUT": "1s",
		"MAX_CONNS":    "10",
		"FEATURE_X":    "false",
	}
	for k, v := range overrides {
		base[k] = v
	}
	for k, v := range base {
		t.Setenv(k, v)
	}
}

func TestDurationParsing(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    time.Duration
		wantErr bool
	}{
		{name: "milliseconds", value: "1500ms", want: 1500 * time.Millisecond},
		{name: "seconds", value: "2s", want: 2 * time.Second},
		{name: "composite", value: "1m30s", want: 90 * time.Second},
		{name: "missing unit", value: "30", wantErr: true},
		{name: "garbage", value: "soon", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setEnv(t, map[string]string{"HTTP_TIMEOUT": tt.value})
			cfg, err := Load()
			if tt.wantErr {
				if !errors.Is(err, ErrInvalidDuration) {
					t.Fatalf("err = %v, want ErrInvalidDuration", err)
				}
				if !strings.Contains(err.Error(), "HTTP_TIMEOUT") {
					t.Fatalf("error %q does not name the key HTTP_TIMEOUT", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.HTTPTimeout != tt.want {
				t.Fatalf("HTTPTimeout = %v, want %v", cfg.HTTPTimeout, tt.want)
			}
		})
	}
}

func TestBoolParsing(t *testing.T) {
	truthy := []string{"1", "t", "T", "TRUE", "true", "True"}
	falsy := []string{"0", "f", "F", "FALSE", "false", "False"}
	for _, v := range truthy {
		t.Run("true/"+v, func(t *testing.T) {
			setEnv(t, map[string]string{"FEATURE_X": v})
			cfg, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if !cfg.FeatureXEnabled {
				t.Fatalf("FEATURE_X=%q parsed as false, want true", v)
			}
		})
	}
	for _, v := range falsy {
		t.Run("false/"+v, func(t *testing.T) {
			setEnv(t, map[string]string{"FEATURE_X": v})
			cfg, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.FeatureXEnabled {
				t.Fatalf("FEATURE_X=%q parsed as true, want false", v)
			}
		})
	}
	t.Run("yes is not a bool", func(t *testing.T) {
		setEnv(t, map[string]string{"FEATURE_X": "yes"})
		_, err := Load()
		if !errors.Is(err, ErrInvalidBool) {
			t.Fatalf("err = %v, want ErrInvalidBool", err)
		}
	})
}

func TestMaxConnsParsing(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    int
		wantErr error
	}{
		{name: "in range", value: "64", want: 64},
		{name: "lower bound", value: "1", want: 1},
		{name: "upper bound", value: "512", want: 512},
		{name: "zero rejected", value: "0", wantErr: ErrOutOfRange},
		{name: "too large", value: "100000", wantErr: ErrOutOfRange},
		{name: "non-numeric", value: "lots", wantErr: ErrInvalidInt},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setEnv(t, map[string]string{"MAX_CONNS": tt.value})
			cfg, err := Load()
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want errors.Is %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.MaxConns != tt.want {
				t.Fatalf("MaxConns = %d, want %d", cfg.MaxConns, tt.want)
			}
		})
	}
}

func ExampleLoad() {
	os.Setenv("HTTP_TIMEOUT", "750ms")
	os.Setenv("MAX_CONNS", "32")
	os.Setenv("FEATURE_X", "1")

	cfg, _ := Load()
	fmt.Printf("%s %d %t\n", cfg.HTTPTimeout, cfg.MaxConns, cfg.FeatureXEnabled)
	// Output: 750ms 32 true
}
```

## Review

The parsing is correct when each stdlib parser is matched to its field and every
failure is both matchable (`errors.Is` against the field's sentinel) and
actionable (the message names the key and value). The two traps this exercise
pins are the unit-less duration — `30` fails, `30s` succeeds — and the strict
`ParseBool` set, where `yes`/`on` are errors rather than truthy. The range check
on `MAX_CONNS` is the reminder that "parses as an int" is not the same as "is a
valid value"; a config layer that skips the bound ships a pool size of `0` to
production.

## Resources

- [time.ParseDuration](https://pkg.go.dev/time#ParseDuration) — requires a unit; `30` fails, `30s` parses.
- [strconv.ParseBool](https://pkg.go.dev/strconv#ParseBool) — the exact accepted truthy/falsy set.
- [strconv.Atoi](https://pkg.go.dev/strconv#Atoi) — integer parsing before the range check.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-required-vs-optional-lookupenv.md](03-required-vs-optional-lookupenv.md) | Next: [05-accumulated-validation-errors.md](05-accumulated-validation-errors.md)
