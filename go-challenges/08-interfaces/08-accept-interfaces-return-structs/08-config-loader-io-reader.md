# Exercise 8: Load Config From An io.Reader, Return A Concrete *Config

A config loader is the rule in its purest form: accept the standard `io.Reader`
interface for the source, return a concrete `*Config` for the result. Reading through
`io.Reader` decouples loading from the filesystem, so the whole path — decode, defaults,
validation — is tested with a `strings.NewReader`, no file I/O anywhere.

This module is fully self-contained. It has its own `go mod init`, defines every
type it needs, and ships its own demo and tests. Nothing here imports any other
exercise.

## What you'll build

```text
configloader/               independent module: example.com/configloader
  go.mod                    go 1.26
  config.go                 Config; ErrInvalidConfig; MissingFieldError; LoadConfig(io.Reader) (*Config, error)
  cmd/
    demo/
      main.go               loads config from an in-memory reader and prints it
  config_test.go            valid input + defaults; truncated -> wrapped error; missing field -> typed error
```

Files: `config.go`, `cmd/demo/main.go`, `config_test.go`.
Implement: `LoadConfig(r io.Reader) (*Config, error)` that JSON-decodes, applies defaults, validates required fields, and returns `*Config`.
Test: valid JSON yields the parsed fields and applied defaults; truncated input yields a decode error matched with `errors.Is(err, ErrInvalidConfig)`; a missing required field yields a `*MissingFieldError` extracted with `errors.As`. No file I/O.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### Why io.Reader in, *Config out

`LoadConfig` accepts `io.Reader`, the narrowest possible source abstraction: one
method, `Read`. A test hands it a `strings.NewReader` or a `bytes.Buffer`; production
hands it an `*os.File`; a remote loader hands it an HTTP response body. The loader
neither knows nor cares — it just decodes bytes. Had it accepted a filename string and
opened the file itself, every test would need a temp file and the loader would be welded
to the filesystem. Accepting the interface is what makes the three failure paths —
malformed bytes, truncated stream, missing field — testable in memory.

It returns `*Config`, the concrete type, so callers read fields directly
(`cfg.MaxConns`) and the struct can grow fields without an interface to widen. Two error
shapes model the two failure kinds. A stream that will not decode wraps the underlying
error behind the `ErrInvalidConfig` sentinel with `%w`, so callers can branch with
`errors.Is(err, ErrInvalidConfig)` while still unwrapping to the concrete
`json`/`io` error if they want detail. A structurally valid document that omits a
required field returns a typed `*MissingFieldError` naming the field, extracted with
`errors.As` — a typed error, not a sentinel, because the field name is data the caller
may want. Defaults are applied after a successful decode and before validation, so an
omitted optional field takes its default while an omitted required field still fails.

Create `config.go`:

```go
package configloader

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// ErrInvalidConfig wraps any failure to decode the config stream.
var ErrInvalidConfig = errors.New("configloader: invalid config")

// MissingFieldError is a typed error naming a required field that was omitted.
type MissingFieldError struct {
	Field string
}

func (e *MissingFieldError) Error() string {
	return fmt.Sprintf("configloader: missing required field %q", e.Field)
}

// Config is the concrete result type. Callers read its fields directly.
type Config struct {
	ServiceName string `json:"service_name"`
	ListenAddr  string `json:"listen_addr"`
	MaxConns    int    `json:"max_conns"`
	TimeoutSecs int    `json:"timeout_secs"`
}

const (
	defaultListenAddr  = ":8080"
	defaultMaxConns    = 100
	defaultTimeoutSecs = 30
)

// LoadConfig reads JSON from r, applies defaults, validates required fields, and
// returns *Config. It accepts io.Reader so the source is substitutable.
func LoadConfig(r io.Reader) (*Config, error) {
	var cfg Config
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}

	// Defaults for omitted optional fields.
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = defaultListenAddr
	}
	if cfg.MaxConns == 0 {
		cfg.MaxConns = defaultMaxConns
	}
	if cfg.TimeoutSecs == 0 {
		cfg.TimeoutSecs = defaultTimeoutSecs
	}

	// Required-field validation.
	if cfg.ServiceName == "" {
		return nil, &MissingFieldError{Field: "service_name"}
	}

	return &cfg, nil
}
```

### The runnable demo

The demo loads config from an in-memory `strings.NewReader` — the same code path a file
would take — and prints the parsed service name alongside a field that fell back to its
default.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"strings"

	"example.com/configloader"
)

func main() {
	src := strings.NewReader(`{"service_name":"orders","max_conns":250}`)

	cfg, err := configloader.LoadConfig(src)
	if err != nil {
		fmt.Println("load:", err)
		return
	}
	fmt.Printf("service=%s addr=%s max_conns=%d timeout=%ds\n",
		cfg.ServiceName, cfg.ListenAddr, cfg.MaxConns, cfg.TimeoutSecs)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
service=orders addr=:8080 max_conns=250 timeout=30s
```

The `addr` and `timeout` are defaults; `max_conns` is the parsed value.

### Tests

The tests feed readers, never files. One asserts a valid document parses and applies
defaults; one feeds a truncated stream and matches `errors.Is(err, ErrInvalidConfig)`,
then unwraps to confirm the underlying `io.ErrUnexpectedEOF` is still reachable; one
omits the required field and extracts the `*MissingFieldError` with `errors.As`.

Create `config_test.go`:

```go
package configloader

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func TestLoadConfigAppliesDefaults(t *testing.T) {
	t.Parallel()
	cfg, err := LoadConfig(strings.NewReader(`{"service_name":"orders"}`))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.ServiceName != "orders" {
		t.Fatalf("ServiceName = %q, want orders", cfg.ServiceName)
	}
	if cfg.ListenAddr != ":8080" {
		t.Fatalf("ListenAddr = %q, want default :8080", cfg.ListenAddr)
	}
	if cfg.MaxConns != 100 {
		t.Fatalf("MaxConns = %d, want default 100", cfg.MaxConns)
	}
	if cfg.TimeoutSecs != 30 {
		t.Fatalf("TimeoutSecs = %d, want default 30", cfg.TimeoutSecs)
	}
}

func TestLoadConfigParsesExplicitValues(t *testing.T) {
	t.Parallel()
	in := `{"service_name":"orders","listen_addr":":9000","max_conns":5,"timeout_secs":2}`
	cfg, err := LoadConfig(strings.NewReader(in))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.ListenAddr != ":9000" || cfg.MaxConns != 5 || cfg.TimeoutSecs != 2 {
		t.Fatalf("cfg = %+v, want explicit values honored", cfg)
	}
}

func TestLoadConfigTruncatedInput(t *testing.T) {
	t.Parallel()
	_, err := LoadConfig(strings.NewReader(`{"service_name":"orders"`)) // no closing brace
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("err = %v, want wrapped ErrInvalidConfig", err)
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v, want underlying io.ErrUnexpectedEOF reachable", err)
	}
}

func TestLoadConfigMissingRequiredField(t *testing.T) {
	t.Parallel()
	_, err := LoadConfig(strings.NewReader(`{"max_conns":5}`))
	var mfe *MissingFieldError
	if !errors.As(err, &mfe) {
		t.Fatalf("err = %v, want *MissingFieldError", err)
	}
	if mfe.Field != "service_name" {
		t.Fatalf("Field = %q, want service_name", mfe.Field)
	}
}

func TestLoadConfigRejectsUnknownField(t *testing.T) {
	t.Parallel()
	_, err := LoadConfig(strings.NewReader(`{"service_name":"orders","bogus":true}`))
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("err = %v, want wrapped ErrInvalidConfig for unknown field", err)
	}
}
```

## Review

The loader is correct when a valid document parses, omitted optional fields take their
defaults, and omitted required fields fail — all driven through an `io.Reader` with no
file touched. The two error shapes are the lesson: a decode failure is wrapped behind
the `ErrInvalidConfig` sentinel with `%w` so callers branch with `errors.Is` yet can
still unwrap to the concrete `io`/`json` error, while a missing required field is a
typed `*MissingFieldError` carrying the field name, pulled out with `errors.As`.
Accepting `io.Reader` rather than a filename is what makes every one of these paths a
fast in-memory test; returning `*Config` rather than an interface lets callers read
fields directly and lets the struct grow.

## Resources

- [`io.Reader`](https://pkg.go.dev/io#Reader) — the one-method source interface the loader accepts.
- [`encoding/json.Decoder`](https://pkg.go.dev/encoding/json#Decoder) — streaming decode and `DisallowUnknownFields`.
- [Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — `%w` wrapping, `errors.Is`, and `errors.As`.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [07-http-handler-service-interface.md](07-http-handler-service-interface.md) | Next: [09-observability-decorator.md](09-observability-decorator.md)
