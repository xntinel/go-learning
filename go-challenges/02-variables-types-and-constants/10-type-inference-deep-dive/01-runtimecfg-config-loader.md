# Exercise 1: Config Loader: Let Struct Fields Drive Untyped Constants

A runtime config loader is where inference bites first on any service: untyped
constant defaults, a typed `Config` struct, and a handful of parsers that each
return a different concrete type. You will build the loader so the struct field
types drive the defaults, and so every parsed override is narrowed only after its
range is validated.

## What you'll build

```text
runtimecfg/                 independent module: example.com/runtimecfg
  go.mod                    go 1.26
  config.go                 const defaults, type Config, func Load
  cmd/
    demo/
      main.go               loads defaults and an override set, prints both
  config_test.go            defaults/overrides/rejection table tests
```

Files: `config.go`, `cmd/demo/main.go`, `config_test.go`.
Implement: `Load(env map[string]string) (Config, error)` with untyped-constant
defaults flowing into typed fields, parsing overrides and range-validating them.
Test: whole-struct equality for defaults and overrides, a rejection table for bad
inputs.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/runtimecfg/cmd/demo
cd ~/go-exercises/runtimecfg
go mod init example.com/runtimecfg
go mod edit -go=1.26
```

## Why the parse type differs from the storage type

The `Config` struct fixes four boundary types: `MaxBodyBytes int64`,
`Timeout time.Duration`, `SampleRate float32`, `Workers int`. The four constant
defaults — `10 << 20`, `2 * time.Second`, `0.10`, `4` — are all *untyped*, so when
they initialize the struct they adopt the field types with no conversion. That is
the safe direction: the field is the target, the constant conforms.

The override path is where the wide-return-type rule matters. `MAX_BODY_BYTES` is
parsed with `strconv.ParseInt`, which returns `int64` — a clean match for the
field. `WORKERS` uses `strconv.Atoi`, which returns `int`. `TIMEOUT` uses
`time.ParseDuration`, which returns `time.Duration`. `SAMPLE_RATE` is the trap:
`strconv.ParseFloat(raw, 32)` returns a `float64`, *not* a `float32`, even though
you asked for 32-bit precision. The `32` only tells the parser to round to a value
representable in `float32`; the returned value is still `float64`. So you validate
the `float64` (the rate must fall in `(0, 1]`), and only then convert with
`float32(sampleRate64)` into the field. Narrowing before the check would risk
storing a value the range test never saw.

Each override is range-validated with a sentinel-free `fmt.Errorf`, and parse
failures are wrapped with `%w` so a caller can still reach the underlying
`strconv`/`time` error with `errors.Is`/`errors.As` if it wants to. Because every
`Config` field is a comparable scalar, the whole struct is comparable, which is why
the tests can assert `got != want` on the entire value.

Create `config.go`:

```go
package runtimecfg

import (
	"fmt"
	"strconv"
	"time"
)

// Untyped constant defaults. Each adopts the type of the Config field it
// initializes: 10<<20 becomes int64, 2*time.Second is already time.Duration,
// 0.10 becomes float32, 4 becomes int. None of these needs a conversion.
const (
	defaultMaxBodyBytes = 10 << 20 // 10 MiB
	defaultTimeout      = 2 * time.Second
	defaultSampleRate   = 0.10
	defaultWorkers      = 4
)

// Config holds validated runtime settings. Every field is a comparable scalar,
// so a whole-struct == comparison is meaningful in tests.
type Config struct {
	MaxBodyBytes int64
	Timeout      time.Duration
	SampleRate   float32
	Workers      int
}

// Load builds a Config from defaults, applies any overrides present in env, and
// range-validates each override. Parse failures are wrapped with %w.
func Load(env map[string]string) (Config, error) {
	cfg := Config{
		MaxBodyBytes: defaultMaxBodyBytes,
		Timeout:      defaultTimeout,
		SampleRate:   defaultSampleRate,
		Workers:      defaultWorkers,
	}

	if raw := env["MAX_BODY_BYTES"]; raw != "" {
		maxBodyBytes, err := strconv.ParseInt(raw, 10, 64) // returns int64
		if err != nil {
			return Config{}, fmt.Errorf("parse MAX_BODY_BYTES: %w", err)
		}
		if maxBodyBytes <= 0 {
			return Config{}, fmt.Errorf("MAX_BODY_BYTES must be positive, got %d", maxBodyBytes)
		}
		cfg.MaxBodyBytes = maxBodyBytes
	}

	if raw := env["TIMEOUT"]; raw != "" {
		timeout, err := time.ParseDuration(raw) // returns time.Duration
		if err != nil {
			return Config{}, fmt.Errorf("parse TIMEOUT: %w", err)
		}
		if timeout <= 0 {
			return Config{}, fmt.Errorf("TIMEOUT must be positive, got %s", timeout)
		}
		cfg.Timeout = timeout
	}

	if raw := env["SAMPLE_RATE"]; raw != "" {
		sampleRate64, err := strconv.ParseFloat(raw, 32) // returns float64, not float32
		if err != nil {
			return Config{}, fmt.Errorf("parse SAMPLE_RATE: %w", err)
		}
		if sampleRate64 <= 0 || sampleRate64 > 1 {
			return Config{}, fmt.Errorf("SAMPLE_RATE must be in (0, 1], got %v", sampleRate64)
		}
		cfg.SampleRate = float32(sampleRate64) // narrow only after validating
	}

	if raw := env["WORKERS"]; raw != "" {
		workers, err := strconv.Atoi(raw) // returns int
		if err != nil {
			return Config{}, fmt.Errorf("parse WORKERS: %w", err)
		}
		if workers <= 0 {
			return Config{}, fmt.Errorf("WORKERS must be positive, got %d", workers)
		}
		cfg.Workers = workers
	}

	return cfg, nil
}
```

## Demo

The demo loads the defaults, then an override set, and prints both so you can see
the field types serialize the way the struct declares them.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/runtimecfg"
)

func main() {
	def, err := runtimecfg.Load(nil)
	if err != nil {
		panic(err)
	}
	fmt.Printf("defaults: max_body_bytes=%d timeout=%s sample_rate=%.2f workers=%d\n",
		def.MaxBodyBytes, def.Timeout, def.SampleRate, def.Workers)

	over, err := runtimecfg.Load(map[string]string{
		"MAX_BODY_BYTES": "2048",
		"TIMEOUT":        "750ms",
		"SAMPLE_RATE":    "0.25",
		"WORKERS":        "8",
	})
	if err != nil {
		panic(err)
	}
	fmt.Printf("override: max_body_bytes=%d timeout=%s sample_rate=%.2f workers=%d\n",
		over.MaxBodyBytes, over.Timeout, over.SampleRate, over.Workers)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
defaults: max_body_bytes=10485760 timeout=2s sample_rate=0.10 workers=4
override: max_body_bytes=2048 timeout=750ms sample_rate=0.25 workers=8
```

## Tests

The default and override tests use whole-struct equality because every field is
comparable. The rejection table drives a slice of bad env maps and asserts each
returns a non-nil error; one case also checks the wrapped `strconv` error is
reachable with `errors.Is`.

Create `config_test.go`:

```go
package runtimecfg

import (
	"errors"
	"strconv"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	t.Parallel()

	got, err := Load(nil)
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		MaxBodyBytes: 10 << 20,
		Timeout:      2 * time.Second,
		SampleRate:   float32(0.10),
		Workers:      4,
	}
	if got != want {
		t.Fatalf("Load(nil) = %+v, want %+v", got, want)
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Parallel()

	got, err := Load(map[string]string{
		"MAX_BODY_BYTES": "2048",
		"TIMEOUT":        "750ms",
		"SAMPLE_RATE":    "0.25",
		"WORKERS":        "8",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		MaxBodyBytes: 2048,
		Timeout:      750 * time.Millisecond,
		SampleRate:   0.25,
		Workers:      8,
	}
	if got != want {
		t.Fatalf("Load(override) = %+v, want %+v", got, want)
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		env  map[string]string
	}{
		{"zero body", map[string]string{"MAX_BODY_BYTES": "0"}},
		{"negative body", map[string]string{"MAX_BODY_BYTES": "-1"}},
		{"nonnumeric body", map[string]string{"MAX_BODY_BYTES": "big"}},
		{"zero timeout", map[string]string{"TIMEOUT": "0s"}},
		{"bad timeout", map[string]string{"TIMEOUT": "soon"}},
		{"rate above one", map[string]string{"SAMPLE_RATE": "2"}},
		{"rate zero", map[string]string{"SAMPLE_RATE": "0"}},
		{"zero workers", map[string]string{"WORKERS": "0"}},
		{"bad workers", map[string]string{"WORKERS": "lots"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := Load(tc.env); err == nil {
				t.Fatalf("Load(%v) expected error, got nil", tc.env)
			}
		})
	}
}

func TestLoadWrapsParseError(t *testing.T) {
	t.Parallel()

	_, err := Load(map[string]string{"WORKERS": "lots"})
	if !errors.Is(err, strconv.ErrSyntax) {
		t.Fatalf("error %v does not wrap strconv.ErrSyntax", err)
	}
}
```

## Review

The loader is correct when defaults come straight from the untyped constants
through the field types with no conversion, and when every override is narrowed
only after its range is validated. The `SAMPLE_RATE` path is the one to inspect
closely: `strconv.ParseFloat(raw, 32)` yields a `float64`, the validation runs on
that `float64`, and `float32(...)` appears exactly once, after the check. If you
ever see a conversion before the range test, that is the bug this exercise exists
to prevent. The rejection table proves each field guards its own range, and the
`%w` wrap keeps the underlying `strconv` error reachable for a caller that cares.

## Resources

- [Go Specification: Constants](https://go.dev/ref/spec#Constants) — untyped constants and how they adopt a target type.
- [strconv package](https://pkg.go.dev/strconv) — exact return types of `ParseInt`, `Atoi`, `ParseFloat`.
- [time.ParseDuration](https://pkg.go.dev/time#ParseDuration) — the `time.Duration` return type.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-compile-time-type-contracts.md](02-compile-time-type-contracts.md)
