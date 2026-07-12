# Exercise 2: Config Loader — Parsing Env Vars With Explicit Widths and Range Errors

Every service starts by turning a handful of environment strings into typed config.
This is the boundary where `"70000"` must not become a silently-wrapped port and
`"abc"` must not become a zero — where `strconv`'s structured errors and the right
`bitSize` are the difference between rejecting bad config and booting on it.

This module is fully self-contained: its own `go mod init`, all code inline, its own
demo and tests. Nothing here imports another exercise.

## What you'll build

```text
config/                    independent module: example.com/config
  go.mod                   go 1.26
  config.go                Config struct; Load; ConfigError; typed field parsers
  cmd/
    demo/
      main.go              loads a good config and classifies a bad one
  config_test.go           valid load, ErrRange, ErrSyntax, missing-key-by-name
```

- Files: `config.go`, `cmd/demo/main.go`, `config_test.go`.
- Implement: `Load(lookup)` reading `PORT` (uint16), `MAX_CONNS` (bounded int), `SHUTDOWN_TIMEOUT` (float seconds), `SAMPLE_RATE` (ratio) into a `Config`, returning a `*ConfigError` that names the offending key and preserves `strconv`'s `ErrRange`/`ErrSyntax`.
- Test: valid values populate the struct; `PORT="70000"` classifies as `ErrRange`; `PORT="abc"` classifies as `ErrSyntax`; a missing required key is reported by name.
- Verify: `go test -count=1 -race ./...`

### Why bitSize and error classification are the whole point

The naive loader calls `strconv.Atoi` (which is `ParseInt(s, 10, 0)` — full `int`
width) and stores the result in whatever field. That is wrong twice over. First,
`PORT="70000"` parses to a valid `int` `70000`, and only the later `uint16(70000)`
conversion wraps it to `4464` — a silent, plausible-looking port that will bind the
wrong socket. Passing `bitSize=16` to `ParseUint` instead makes 70000 an *error* at
parse time, because it does not fit a 16-bit unsigned field. The `bitSize` argument
is not cosmetic; it is where the field's real range lives.

Second, `strconv` returns a `*strconv.NumError` that wraps one of two sentinels:
`strconv.ErrSyntax` when the text is not a number, and `strconv.ErrRange` when the
number does not fit the `bitSize`. On `ErrRange` the function also returns the
max-magnitude value for the width — so a loader that ignores the error accepts a
clamped `65535` as if the operator had typed it. The loader must classify with
`errors.Is` and reject, never read the returned number on a range error. This module
wraps each parse error in a `*ConfigError` that carries the offending key, and
because `ConfigError.Unwrap` chains down to the `NumError` and then to the sentinel,
`errors.Is(err, strconv.ErrRange)` still works through the wrapper.

Create `config.go`:

```go
package config

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"
)

// ErrMissing marks a required key that was not present.
var ErrMissing = errors.New("required key not set")

// ErrInvalid marks a value that parsed but failed a domain constraint.
var ErrInvalid = errors.New("invalid value")

// Config holds the typed settings a service needs at startup.
type Config struct {
	Port            uint16
	MaxConns        int
	ShutdownTimeout time.Duration
	SampleRate      float64
}

// ConfigError names the offending key and wraps the underlying cause, so
// errors.Is(err, strconv.ErrRange) and errors.As(err, *ConfigError) both work.
type ConfigError struct {
	Key string
	Err error
}

func (e *ConfigError) Error() string { return fmt.Sprintf("config %s: %v", e.Key, e.Err) }
func (e *ConfigError) Unwrap() error { return e.Err }

// LookupFunc mirrors os.LookupEnv: it returns the value and whether it was set.
// Injecting it lets tests supply a map instead of touching the real environment.
type LookupFunc func(key string) (string, bool)

// Load reads and validates the service configuration. PORT is required; the
// other keys fall back to documented defaults when unset.
func Load(lookup LookupFunc) (Config, error) {
	var cfg Config

	raw, ok := lookup("PORT")
	if !ok {
		return Config{}, &ConfigError{Key: "PORT", Err: ErrMissing}
	}
	port, err := strconv.ParseUint(raw, 10, 16)
	if err != nil {
		return Config{}, &ConfigError{Key: "PORT", Err: err}
	}
	cfg.Port = uint16(port)

	cfg.MaxConns = 100
	if raw, ok := lookup("MAX_CONNS"); ok {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return Config{}, &ConfigError{Key: "MAX_CONNS", Err: err}
		}
		if n < 1 || n > 1<<20 {
			return Config{}, &ConfigError{Key: "MAX_CONNS", Err: fmt.Errorf("%w: %d not in [1,%d]", ErrInvalid, n, 1<<20)}
		}
		cfg.MaxConns = int(n)
	}

	cfg.ShutdownTimeout = 10 * time.Second
	if raw, ok := lookup("SHUTDOWN_TIMEOUT"); ok {
		secs, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return Config{}, &ConfigError{Key: "SHUTDOWN_TIMEOUT", Err: err}
		}
		if math.IsNaN(secs) || math.IsInf(secs, 0) || secs < 0 {
			return Config{}, &ConfigError{Key: "SHUTDOWN_TIMEOUT", Err: fmt.Errorf("%w: %v seconds", ErrInvalid, secs)}
		}
		cfg.ShutdownTimeout = time.Duration(secs * float64(time.Second))
	}

	cfg.SampleRate = 1.0
	if raw, ok := lookup("SAMPLE_RATE"); ok {
		r, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return Config{}, &ConfigError{Key: "SAMPLE_RATE", Err: err}
		}
		if math.IsNaN(r) || r < 0 || r > 1 {
			return Config{}, &ConfigError{Key: "SAMPLE_RATE", Err: fmt.Errorf("%w: %v not in [0,1]", ErrInvalid, r)}
		}
		cfg.SampleRate = r
	}

	return cfg, nil
}
```

### The runnable demo

The demo loads a valid config from a map, then feeds a bad `PORT` and shows the
error is classified as a range error — not accepted as a wrapped `4464`. It uses
`os.LookupEnv` only in a comment to make the mapping to real env explicit; the demo
itself injects a map so the output is deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"strconv"

	"example.com/config"
)

func fromMap(m map[string]string) config.LookupFunc {
	return func(key string) (string, bool) {
		v, ok := m[key]
		return v, ok
	}
}

func main() {
	// In production the lookup would be os.LookupEnv.
	good := fromMap(map[string]string{
		"PORT":             "8443",
		"MAX_CONNS":        "1024",
		"SHUTDOWN_TIMEOUT": "5",
		"SAMPLE_RATE":      "0.1",
	})
	cfg, err := config.Load(good)
	if err != nil {
		fmt.Println("load failed:", err)
		return
	}
	fmt.Printf("port=%d maxConns=%d shutdown=%s sampleRate=%v\n",
		cfg.Port, cfg.MaxConns, cfg.ShutdownTimeout, cfg.SampleRate)

	bad := fromMap(map[string]string{"PORT": "70000"})
	_, err = config.Load(bad)
	fmt.Printf("PORT=70000 rejected: range=%t\n", errors.Is(err, strconv.ErrRange))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
port=8443 maxConns=1024 shutdown=5s sampleRate=0.1
PORT=70000 rejected: range=true
```

### Tests

The tests inject a map-backed lookup so they never touch the real environment. The
valid case checks each field is populated, including the float-seconds to
`time.Duration` conversion and the defaults for unset optional keys. The two
`PORT` failures are the heart of the lesson: `"70000"` must classify as
`strconv.ErrRange` (proving `bitSize=16` rejected it rather than wrapping to 4464),
and `"abc"` must classify as `strconv.ErrSyntax`. The missing-key case uses
`errors.As` to recover the `*ConfigError` and assert it names `PORT`.

Create `config_test.go`:

```go
package config

import (
	"errors"
	"strconv"
	"testing"
	"time"
)

func mapLookup(m map[string]string) LookupFunc {
	return func(key string) (string, bool) {
		v, ok := m[key]
		return v, ok
	}
}

func TestLoadValid(t *testing.T) {
	t.Parallel()

	cfg, err := Load(mapLookup(map[string]string{
		"PORT":             "8443",
		"MAX_CONNS":        "1024",
		"SHUTDOWN_TIMEOUT": "5",
		"SAMPLE_RATE":      "0.1",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 8443 {
		t.Errorf("Port = %d, want 8443", cfg.Port)
	}
	if cfg.MaxConns != 1024 {
		t.Errorf("MaxConns = %d, want 1024", cfg.MaxConns)
	}
	if cfg.ShutdownTimeout != 5*time.Second {
		t.Errorf("ShutdownTimeout = %s, want 5s", cfg.ShutdownTimeout)
	}
	if cfg.SampleRate != 0.1 {
		t.Errorf("SampleRate = %v, want 0.1", cfg.SampleRate)
	}
}

func TestLoadDefaultsForUnsetOptional(t *testing.T) {
	t.Parallel()

	cfg, err := Load(mapLookup(map[string]string{"PORT": "80"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxConns != 100 || cfg.ShutdownTimeout != 10*time.Second || cfg.SampleRate != 1.0 {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
}

func TestLoadClassifiesErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		env    map[string]string
		target error
	}{
		{name: "port out of range", env: map[string]string{"PORT": "70000"}, target: strconv.ErrRange},
		{name: "port not a number", env: map[string]string{"PORT": "abc"}, target: strconv.ErrSyntax},
		{name: "missing port", env: map[string]string{}, target: ErrMissing},
		{name: "sample rate too high", env: map[string]string{"PORT": "80", "SAMPLE_RATE": "2"}, target: ErrInvalid},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := Load(mapLookup(tt.env))
			if !errors.Is(err, tt.target) {
				t.Fatalf("Load error = %v, want Is %v", err, tt.target)
			}
		})
	}
}

func TestLoadNamesOffendingKey(t *testing.T) {
	t.Parallel()

	_, err := Load(mapLookup(map[string]string{}))
	var cerr *ConfigError
	if !errors.As(err, &cerr) {
		t.Fatalf("error is not *ConfigError: %v", err)
	}
	if cerr.Key != "PORT" {
		t.Fatalf("Key = %q, want PORT", cerr.Key)
	}
}
```

## Review

The loader is correct when a value that does not fit its field is an error, not a
wrapped number. The decisive checks are the two `PORT` cases: if `"70000"` ever
produces a `Config` with `Port == 4464`, the `bitSize` argument was dropped or the
error was ignored. Classification through the `*ConfigError` wrapper works only
because `Unwrap` chains to the `NumError` and its sentinel — remove the `Unwrap`
method and every `errors.Is` in the tests fails, which is the fast way to confirm the
wrapping is real. Injecting `LookupFunc` instead of calling `os.LookupEnv` keeps the
tests hermetic and parallel-safe; in production you pass `os.LookupEnv` directly.

## Resources

- [strconv package](https://pkg.go.dev/strconv) — `ParseUint`/`ParseInt`/`ParseFloat`, `NumError`, `ErrRange`, `ErrSyntax`.
- [errors package](https://pkg.go.dev/errors) — `Is` and `As` traversal through `Unwrap`.
- [os.LookupEnv](https://pkg.go.dev/os#LookupEnv) — the real lookup the injected `LookupFunc` mirrors.

---

Back to [00-concepts.md](00-concepts.md) | Next: [03-money-minor-units.md](03-money-minor-units.md)
