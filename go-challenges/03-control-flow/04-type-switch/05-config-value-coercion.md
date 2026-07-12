# Exercise 5: Coerce Untyped Config Values into Typed Settings

A config layer merges values from YAML, JSON, and environment overrides. The same
logical setting arrives as a `json.Number`, a `string`, a `float64`, or a native
`int` depending on the source. The loader must coerce each into a strongly typed
setting with a bounds check before it is committed. The type switch is the
coercion engine.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
configcoerce/                independent module: example.com/configcoerce
  go.mod                     go 1.26
  coerce.go                  CoerceInt, CoerceBool, CoerceDuration over any (type switch + bounds)
  cmd/
    demo/
      main.go                coerces a mixed-source settings map into typed values
  coerce_test.go             every int representation, out-of-range/precision, duration, unsupported type
```

- Files: `coerce.go`, `cmd/demo/main.go`, `coerce_test.go`.
- Implement: `CoerceInt(v any) (int64, error)`, `CoerceBool(v any) (bool, error)`,
  `CoerceDuration(v any) (time.Duration, error)` coercing across `string`, `bool`,
  `json.Number`, `int`, `int64`, `float64`, with range and precision validation.
- Test: every source representation of an int coerces to the same value;
  out-of-range and fractional `float64` are rejected; a duration string `"30s"`
  and an integer-seconds fallback both parse; an unsupported type returns a typed
  error naming the source `%T`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/04-type-switch/05-config-value-coercion/cmd/demo
cd go-solutions/03-control-flow/04-type-switch/05-config-value-coercion
```

## One setting, many wire types

The reason this coercion is necessary is that decoders disagree about number
types. A YAML/JSON decoder that used `UseNumber` yields `json.Number`; one that
did not yields `float64`; an environment variable is always a `string`; a
programmatic default is a native `int`. All four can mean the port `8080`. The
coercer's job is to accept every faithful representation and reject the lossy
ones — a `float64` of `8080.5` is not a valid port, and a `json.Number` beyond
`int64` range is an overflow, not a value.

`CoerceInt` type-switches over the representations. The interesting branch is
`float64`: JSON has no integer type, so a whole number may arrive as a `float64`,
which is fine — but only if it has no fractional part and is within `int64`
range. The guard is `v != math.Trunc(v)` for the fractional check and an explicit
range comparison against `math.MinInt64`/`math.MaxInt64` before the conversion.
Skipping either guard is a silent-corruption bug: `int64(8080.9)` truncates to
`8080` and `int64(1e19)` overflows to a garbage value.

`CoerceDuration` accepts the two real forms a duration takes in config: a Go
duration string like `"30s"` (parsed by `time.ParseDuration`), and a bare number
meaning seconds (the common integer-seconds fallback). `CoerceBool` accepts a
native `bool` and the string forms `strconv.ParseBool` understands.

Create `coerce.go`:

```go
package configcoerce

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"
)

// ErrCoerce is the sentinel for every coercion failure; callers match it with
// errors.Is and read the wrapped detail for the offending type or value.
var ErrCoerce = errors.New("coerce")

// CoerceInt turns a config value from any supported source into an int64.
func CoerceInt(v any) (int64, error) {
	switch n := v.(type) {
	case int:
		return int64(n), nil
	case int64:
		return n, nil
	case float64:
		if n != math.Trunc(n) {
			return 0, fmt.Errorf("%w: %v has a fractional part", ErrCoerce, n)
		}
		if n < math.MinInt64 || n >= math.MaxInt64 {
			return 0, fmt.Errorf("%w: %v out of int64 range", ErrCoerce, n)
		}
		return int64(n), nil
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0, fmt.Errorf("%w: %q not an integer: %v", ErrCoerce, n.String(), err)
		}
		return i, nil
	case string:
		i, err := strconv.ParseInt(n, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%w: %q not an integer: %v", ErrCoerce, n, err)
		}
		return i, nil
	default:
		return 0, fmt.Errorf("%w: cannot coerce %T to int", ErrCoerce, v)
	}
}

// CoerceBool turns a config value into a bool.
func CoerceBool(v any) (bool, error) {
	switch b := v.(type) {
	case bool:
		return b, nil
	case string:
		parsed, err := strconv.ParseBool(b)
		if err != nil {
			return false, fmt.Errorf("%w: %q not a bool: %v", ErrCoerce, b, err)
		}
		return parsed, nil
	default:
		return false, fmt.Errorf("%w: cannot coerce %T to bool", ErrCoerce, v)
	}
}

// CoerceDuration turns a config value into a time.Duration. A string is parsed
// by time.ParseDuration; a bare number is interpreted as seconds.
func CoerceDuration(v any) (time.Duration, error) {
	switch d := v.(type) {
	case time.Duration:
		return d, nil
	case string:
		parsed, err := time.ParseDuration(d)
		if err != nil {
			return 0, fmt.Errorf("%w: %q not a duration: %v", ErrCoerce, d, err)
		}
		return parsed, nil
	case int, int64, float64, json.Number:
		secs, err := CoerceInt(v)
		if err != nil {
			return 0, err
		}
		return time.Duration(secs) * time.Second, nil
	default:
		return 0, fmt.Errorf("%w: cannot coerce %T to duration", ErrCoerce, v)
	}
}
```

## The runnable demo

The demo mimics a merged settings map where each value arrived from a different
source, then coerces each to its typed setting.

Create `cmd/demo/main.go`:

```go
package main

import (
	"encoding/json"
	"fmt"

	"example.com/configcoerce"
)

func main() {
	settings := map[string]any{
		"port":     json.Number("8080"), // from a UseNumber JSON decoder
		"max_conn": "100",               // from an env override
		"debug":    true,                // from a native default
		"timeout":  "30s",               // a duration string
	}

	port, _ := configcoerce.CoerceInt(settings["port"])
	maxConn, _ := configcoerce.CoerceInt(settings["max_conn"])
	debug, _ := configcoerce.CoerceBool(settings["debug"])
	timeout, _ := configcoerce.CoerceDuration(settings["timeout"])

	fmt.Printf("port=%d max_conn=%d debug=%t timeout=%s\n", port, maxConn, debug, timeout)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
port=8080 max_conn=100 debug=true timeout=30s
```

## Tests

The parity test proves that `json.Number("8080")`, the string `"8080"`, the
`float64` `8080.0`, and the native `int` `8080` all coerce to the same `int64`.
The rejection tests prove a fractional `float64` and an out-of-range value are
refused. The duration test covers both the `"30s"` string and the integer-seconds
fallback. The unsupported test asserts the error is `ErrCoerce` and names the
offending `%T` (here `map[string]int`).

Create `coerce_test.go`:

```go
package configcoerce

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCoerceIntParity(t *testing.T) {
	t.Parallel()
	sources := []any{json.Number("8080"), "8080", 8080.0, 8080, int64(8080)}
	for _, src := range sources {
		got, err := CoerceInt(src)
		if err != nil {
			t.Fatalf("CoerceInt(%#v): %v", src, err)
		}
		if got != 8080 {
			t.Errorf("CoerceInt(%#v) = %d, want 8080", src, got)
		}
	}
}

func TestCoerceIntRejects(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		v    any
	}{
		{"fractional float", 8080.5},
		{"non-numeric string", "not-a-number"},
		{"bad json.Number", json.Number("1.5")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := CoerceInt(tc.v); !errors.Is(err, ErrCoerce) {
				t.Fatalf("CoerceInt(%v) err = %v, want ErrCoerce", tc.v, err)
			}
		})
	}
}

func TestCoerceDuration(t *testing.T) {
	t.Parallel()
	got, err := CoerceDuration("30s")
	if err != nil || got != 30*time.Second {
		t.Fatalf("CoerceDuration(30s) = %s, %v", got, err)
	}
	got, err = CoerceDuration(int64(45)) // integer-seconds fallback
	if err != nil || got != 45*time.Second {
		t.Fatalf("CoerceDuration(45) = %s, %v", got, err)
	}
}

func TestCoerceUnsupportedNamesType(t *testing.T) {
	t.Parallel()
	_, err := CoerceInt(map[string]int{"a": 1})
	if !errors.Is(err, ErrCoerce) {
		t.Fatalf("err = %v, want ErrCoerce", err)
	}
	if !strings.Contains(err.Error(), "map[string]int") {
		t.Fatalf("err = %q, want it to name the source type", err)
	}
}
```

## Review

The coercer is correct when every faithful representation of a value maps to the
same typed result and every lossy one is refused with an `ErrCoerce` that names
what went wrong. The two silent-corruption bugs it guards against are converting a
`float64` to `int64` without the fractional check (`8080.9` becomes `8080`) and
without the range check (`1e19` overflows to nonsense). Keep both guards on the
`float64` branch. The `default` branch naming the `%T` is what turns "config
didn't apply" from a mystery into a one-line diagnosis, so always include it.

## Resources

- [encoding/json.Number](https://pkg.go.dev/encoding/json#Number)
- [strconv.ParseInt / ParseBool](https://pkg.go.dev/strconv#ParseInt)
- [time.ParseDuration](https://pkg.go.dev/time#ParseDuration)
- [math.Trunc](https://pkg.go.dev/math#Trunc)

---

Prev: [04-error-retry-classifier.md](04-error-retry-classifier.md) | Up: [00-concepts.md](00-concepts.md) | Next: [06-slog-attr-encoder.md](06-slog-attr-encoder.md)
