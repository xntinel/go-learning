# Exercise 5: Typed Getters over a `map[string]any` Config Tree

Config loaded from YAML, JSON, or environment overlays arrives as `map[string]any`
where the same logical integer might be an `int`, an `int64`, a `float64`, a
`json.Number`, or a numeric `string` depending on the source. This module builds
coercing getters — `GetString`, `GetInt`, `GetDuration` — that normalize across the
numeric zoo and return a wrapped sentinel error instead of panicking.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
dynconfig/                 independent module: example.com/dynconfig
  go.mod                   go 1.26
  dynconfig.go             Config over map[string]any; GetString/GetInt/GetDuration + sentinels
  cmd/
    demo/
      main.go              runnable demo: read mixed-typed config keys
  dynconfig_test.go        numeric zoo, fractional-float rejection, duration parse, errors.Is
```

- Files: `dynconfig.go`, `cmd/demo/main.go`, `dynconfig_test.go`.
- Implement: a `Config` wrapping `map[string]any` with `GetString(key, def)`, `GetInt(key, def)`, and `GetDuration(key, def)` that coerce across `int`/`int64`/`float64`/`json.Number`/`string` and report wrapped sentinel errors.
- Test: `GetInt` succeeds for the whole numeric zoo, rejects a fractional float and a non-numeric string, and returns the default on a missing key; `GetDuration` parses `"30s"` and rejects garbage; errors are matchable with `errors.Is`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### Why coercion, and why a default plus an error

The same config key means the same thing regardless of which loader produced the
tree. YAML gives you an `int`; JSON gives you a `float64` (or `json.Number` under
`UseNumber`); an environment overlay gives you a `string`. A getter that only
accepts one of those rejects valid config depending on the loader — a brittle,
maddening bug. So `GetInt` type-switches over the whole numeric zoo and normalizes
to `int64`. The one place it must be strict is a `float64` that is not a whole
number: `3.5` is not an integer, and silently truncating it to `3` hides a real
config error, so `GetInt` rejects it. A numeric `string` is parsed with
`strconv.ParseInt`; a `json.Number` goes through its `Int64()`.

Each getter takes a default and returns `(value, error)`. The default is what the
caller uses when the key is absent or malformed; the error explains why, and — the
senior detail — it wraps a package sentinel so a caller can distinguish "key was
missing" (`ErrKeyNotFound`, often fine to fall back on) from "key was present but
the wrong type" (`ErrWrongType`, usually a config bug worth failing startup over)
via `errors.Is`. Nothing panics: a bad config value returns an error the caller
routes, never a crash at boot.

Create `dynconfig.go`:

```go
package dynconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Sentinel errors let callers distinguish a missing key from a present-but-wrong
// value with errors.Is.
var (
	ErrKeyNotFound = errors.New("dynconfig: key not found")
	ErrWrongType   = errors.New("dynconfig: value has wrong type")
)

// Config wraps a config tree decoded from YAML/JSON/env into map[string]any.
type Config struct {
	m map[string]any
}

// New wraps m. A nil map is treated as empty.
func New(m map[string]any) *Config {
	if m == nil {
		m = map[string]any{}
	}
	return &Config{m: m}
}

// GetString returns the string at key, or def and an error if absent or not a
// string.
func (c *Config) GetString(key, def string) (string, error) {
	v, ok := c.m[key]
	if !ok {
		return def, fmt.Errorf("%q: %w", key, ErrKeyNotFound)
	}
	s, ok := v.(string)
	if !ok {
		return def, fmt.Errorf("%q holds %T: %w", key, v, ErrWrongType)
	}
	return s, nil
}

// GetInt returns the value at key coerced to int64, accepting int, int64, float64
// (whole numbers only), json.Number, and numeric string. It returns def and a
// wrapped error for a missing key, a fractional float, or an unparseable value.
func (c *Config) GetInt(key string, def int64) (int64, error) {
	v, ok := c.m[key]
	if !ok {
		return def, fmt.Errorf("%q: %w", key, ErrKeyNotFound)
	}
	switch x := v.(type) {
	case int:
		return int64(x), nil
	case int64:
		return x, nil
	case float64:
		if x != float64(int64(x)) {
			return def, fmt.Errorf("%q is fractional (%v): %w", key, x, ErrWrongType)
		}
		return int64(x), nil
	case json.Number:
		n, err := x.Int64()
		if err != nil {
			return def, fmt.Errorf("%q json.Number %q: %w", key, x, ErrWrongType)
		}
		return n, nil
	case string:
		n, err := strconv.ParseInt(x, 10, 64)
		if err != nil {
			return def, fmt.Errorf("%q string %q: %w", key, x, ErrWrongType)
		}
		return n, nil
	default:
		return def, fmt.Errorf("%q holds %T: %w", key, v, ErrWrongType)
	}
}

// GetDuration returns the value at key as a time.Duration. It accepts a duration
// string ("30s") or an integer count of nanoseconds via the numeric zoo. It returns
// def and a wrapped error for a missing key or an unparseable value.
func (c *Config) GetDuration(key string, def time.Duration) (time.Duration, error) {
	v, ok := c.m[key]
	if !ok {
		return def, fmt.Errorf("%q: %w", key, ErrKeyNotFound)
	}
	if s, ok := v.(string); ok {
		d, err := time.ParseDuration(s)
		if err != nil {
			return def, fmt.Errorf("%q duration %q: %w", key, s, ErrWrongType)
		}
		return d, nil
	}
	// Fall back to an integer count of nanoseconds via GetInt's coercion.
	n, err := c.GetInt(key, int64(def))
	if err != nil {
		return def, err
	}
	return time.Duration(n), nil
}
```

### The runnable demo

The demo builds a config tree whose values arrive as several different dynamic
types — mimicking a merged YAML/JSON/env overlay — and reads them through the typed
getters.

Create `cmd/demo/main.go`:

```go
package main

import (
	"encoding/json"
	"fmt"
	"time"

	"example.com/dynconfig"
)

func main() {
	cfg := dynconfig.New(map[string]any{
		"host":         "db.internal",
		"port":         float64(5432),      // from a JSON loader
		"max_conns":    json.Number("100"), // from a UseNumber loader
		"retries":      "3",                // from an env overlay
		"idle_timeout": "30s",
	})

	host, _ := cfg.GetString("host", "localhost")
	port, _ := cfg.GetInt("port", 5432)
	conns, _ := cfg.GetInt("max_conns", 10)
	retries, _ := cfg.GetInt("retries", 0)
	idle, _ := cfg.GetDuration("idle_timeout", time.Minute)

	fmt.Printf("host=%s port=%d max_conns=%d retries=%d idle=%s\n",
		host, port, conns, retries, idle)

	if _, err := cfg.GetInt("host", 0); err != nil {
		fmt.Println("GetInt(host):", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
host=db.internal port=5432 max_conns=100 retries=3 idle=30s
GetInt(host): "host" holds string: dynconfig: value has wrong type
```

### Tests

`TestGetIntNumericZoo` walks every dynamic type `GetInt` must accept and asserts the
normalized `int64`. `TestGetIntRejects` covers the failure paths — a fractional
float, a non-numeric string, a wrong type, and a missing key — asserting the default
is returned and the error is matchable with `errors.Is` against the right sentinel.
`TestGetDuration` parses `"30s"` and rejects garbage. The `errors.Is` assertions are
the point: a caller must be able to tell "missing" from "wrong type".

Create `dynconfig_test.go`:

```go
package dynconfig

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestGetIntNumericZoo(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		val  any
		want int64
	}{
		{"int", int(42), 42},
		{"int64", int64(42), 42},
		{"whole float64", float64(42), 42},
		{"json.Number", json.Number("42"), 42},
		{"numeric string", "42", 42},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := New(map[string]any{"k": tc.val})
			got, err := c.GetInt("k", -1)
			if err != nil {
				t.Fatalf("GetInt(%v): %v", tc.val, err)
			}
			if got != tc.want {
				t.Fatalf("GetInt(%v) = %d, want %d", tc.val, got, tc.want)
			}
		})
	}
}

func TestGetIntRejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cfg     map[string]any
		key     string
		wantErr error
	}{
		{"fractional float", map[string]any{"k": 3.5}, "k", ErrWrongType},
		{"non-numeric string", map[string]any{"k": "abc"}, "k", ErrWrongType},
		{"wrong type", map[string]any{"k": true}, "k", ErrWrongType},
		{"missing key", map[string]any{}, "k", ErrKeyNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := New(tc.cfg)
			got, err := c.GetInt(tc.key, 99)
			if got != 99 {
				t.Fatalf("GetInt returned %d, want default 99", got)
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("GetInt error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestGetDuration(t *testing.T) {
	t.Parallel()

	c := New(map[string]any{"good": "30s", "bad": "xyz"})

	d, err := c.GetDuration("good", time.Minute)
	if err != nil {
		t.Fatalf("GetDuration(good): %v", err)
	}
	if d != 30*time.Second {
		t.Fatalf("GetDuration(good) = %s, want 30s", d)
	}

	def := time.Minute
	d, err = c.GetDuration("bad", def)
	if !errors.Is(err, ErrWrongType) {
		t.Fatalf("GetDuration(bad) error = %v, want ErrWrongType", err)
	}
	if d != def {
		t.Fatalf("GetDuration(bad) = %s, want default %s", d, def)
	}
}

func TestGetString(t *testing.T) {
	t.Parallel()

	c := New(map[string]any{"s": "hi", "n": 5})

	if got, err := c.GetString("s", "def"); err != nil || got != "hi" {
		t.Fatalf("GetString(s) = %q,%v; want hi,nil", got, err)
	}
	if got, err := c.GetString("n", "def"); !errors.Is(err, ErrWrongType) || got != "def" {
		t.Fatalf("GetString(n) = %q,%v; want def,ErrWrongType", got, err)
	}
	if got, err := c.GetString("missing", "def"); !errors.Is(err, ErrKeyNotFound) || got != "def" {
		t.Fatalf("GetString(missing) = %q,%v; want def,ErrKeyNotFound", got, err)
	}
}
```

## Review

The getters are correct when they accept every dynamic type a real loader can
produce for a value and reject only what is genuinely wrong: a fractional float for
an integer key, an unparseable string, a type that makes no sense. The default-plus-
wrapped-error shape is what makes them usable at startup — `TestGetIntRejects` proves
the default comes back and `errors.Is` distinguishes a missing key from a wrong type,
so a caller can choose to fall back on the former and fail fast on the latter. The
mistake this module prevents is a getter that asserts `v.(int)` and panics the moment
a JSON loader hands it a `float64`, or one that silently truncates `3.5` to `3` and
buries a config error. Run `go test -race` to confirm the whole zoo and every failure
path behave.

## Resources

- [`strconv.ParseInt`](https://pkg.go.dev/strconv#ParseInt) — parsing a numeric string to int64.
- [`time.ParseDuration`](https://pkg.go.dev/time#ParseDuration) — parsing "30s" and friends.
- [`errors.Is`](https://pkg.go.dev/errors#Is) — matching a wrapped sentinel so callers can branch on the cause.

---

Back to [00-concepts.md](00-concepts.md) | Next: [06-structured-log-fields.md](06-structured-log-fields.md)
