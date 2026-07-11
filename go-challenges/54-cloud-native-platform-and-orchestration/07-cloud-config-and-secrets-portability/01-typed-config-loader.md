# Exercise 1: A Typed, Portable Config Loader over runtimevar

Configuration arrives as raw bytes no matter where it lives. This exercise builds
a `ConfigLoader` that turns those bytes into a strongly-typed `*Config`, reads the
current value under a bounded context, and reports readiness — all as
backend-independent concerns solved once. The hermetic backing here is
`constantvar` (in-memory), so the identical code runs unchanged against `file://`
or a cloud parameter store by swapping only the URL and opener.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
typedconfig/                 independent module: example.com/typedconfig
  go.mod                     require gocloud.dev
  config.go                  type Config; ConfigLoader; NewConfigLoader, NewConfigDecoder, Load, Ready, Close
  cmd/
    demo/
      main.go                opens a constant:// URL with a typed decoder and prints the config
  config_test.go             hermetic constantvar tests: decode, wrong type, malformed, cancelled ctx, readiness
```

Files: `config.go`, `cmd/demo/main.go`, `config_test.go`.
Implement: a `Config` struct plus a `ConfigLoader` over `*runtimevar.Variable` with `Load(ctx) (*Config, error)`, `Ready() error`, and `Close() error`, and a `NewConfigDecoder()` that decodes JSON bytes into `*Config`.
Test: hermetic `constantvar`-backed tests asserting a typed value with a non-zero `UpdateTime`, a wrong-decoder type error, a malformed-JSON decode error, that a cancelled context still returns an existing good value, and that `Ready` is nil once a value exists.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/typedconfig/cmd/demo
cd ~/go-exercises/typedconfig
go mod init example.com/typedconfig
go get gocloud.dev@latest
```

### Why the loader depends on a Variable, not a cloud

The `ConfigLoader` holds a `*runtimevar.Variable` and nothing else. That is the
port. It never mentions AWS, GCP, a file path, or a URL scheme; whoever constructs
the `Variable` decides the backend. In a test that is `constantvar.NewBytes`; in
production it is `runtimevar.OpenVariable(ctx, os.Getenv("CONFIG_URL"))`. Because
the loader depends only on the interface, the decode-bytes-to-struct logic, the
timeout handling, and the readiness check are written once and reused across every
environment.

The bridge from raw bytes to a typed struct is the decoder. `NewConfigDecoder`
returns `runtimevar.NewDecoder(&Config{}, runtimevar.JSONDecode)`: the prototype
`&Config{}` fixes the value's dynamic type as `*Config`, and `JSONDecode`
unmarshals the bytes into a fresh `Config` on every good snapshot. `Load` reads
`Latest`, then type-asserts `snap.Value` to `*Config`. The assertion must match
the prototype — assert to `*Config`, never `Config` — and a failed assertion is
reported as `ErrUnexpectedType` rather than allowed to panic. That defends against
wiring a loader over a variable that was decoded with the wrong decoder.

`Load` uses `Latest`, which is the correct per-request method: it is
concurrency-safe and returns the most recent good value. Its context behavior is
worth internalizing. If a good value already exists, `Latest` returns it even when
the context is already Done; only when no good value has ever arrived and the
context is Done does it return the latest error. So a cancelled context does not
corrupt a running service that already has config — it just prevents blocking on a
cold start. `Ready` delegates to `CheckHealth`, which is non-blocking and returns
`nil` exactly when a good value is available; that is the method a readiness probe
must call, never `Latest`.

Create `config.go`:

```go
package config

import (
	"context"
	"errors"
	"fmt"

	"gocloud.dev/runtimevar"
)

// ErrUnexpectedType is returned when the variable's snapshot value is not the
// *Config produced by NewConfigDecoder (for example, the variable was decoded
// with the wrong decoder).
var ErrUnexpectedType = errors.New("config: unexpected snapshot type")

// Config is the strongly-typed application configuration decoded from the raw
// bytes a runtimevar backend delivers.
type Config struct {
	ServiceName  string          `json:"service_name"`
	MaxConns     int             `json:"max_conns"`
	FeatureFlags map[string]bool `json:"feature_flags"`
}

// NewConfigDecoder returns a decoder that unmarshals JSON bytes into a *Config.
// The prototype &Config{} fixes the snapshot value's dynamic type as *Config.
func NewConfigDecoder() *runtimevar.Decoder {
	return runtimevar.NewDecoder(&Config{}, runtimevar.JSONDecode)
}

// ConfigLoader reads typed configuration from any runtimevar.Variable. It
// depends on the port, not on a concrete cloud backend.
type ConfigLoader struct {
	v *runtimevar.Variable
}

// NewConfigLoader wraps an already-opened Variable. The caller chose the backend
// (constantvar, filevar, a cloud parameter store) by how it built v.
func NewConfigLoader(v *runtimevar.Variable) *ConfigLoader {
	return &ConfigLoader{v: v}
}

// Load returns the latest good configuration. It is safe for concurrent,
// per-request use. If a good value exists it is returned even when ctx is Done;
// on a cold start with a Done ctx it returns the latest error instead of blocking.
func (l *ConfigLoader) Load(ctx context.Context) (*Config, error) {
	snap, err := l.v.Latest(ctx)
	if err != nil {
		return nil, fmt.Errorf("load latest config: %w", err)
	}
	cfg, ok := snap.Value.(*Config)
	if !ok {
		return nil, fmt.Errorf("%w: got %T", ErrUnexpectedType, snap.Value)
	}
	return cfg, nil
}

// Ready reports whether configuration is available without blocking. It maps
// onto a readiness probe: nil means a good value exists. Do not call Load in a
// probe, because Load blocks until the first good value on a cold start.
func (l *ConfigLoader) Ready() error {
	return l.v.CheckHealth()
}

// Close releases the backing variable's polling goroutine or watcher.
func (l *ConfigLoader) Close() error {
	return l.v.Close()
}
```

### The runnable demo

The demo shows the full portability story with a `constant://` URL. It registers
the typed decoder on a fresh `URLMux` (the zero value has no schemes, so there is
no conflict with the default mux), then opens `constant://?val=<json>`. Swapping
that URL for `file:///etc/app/config.json` or
`awsparamstore://myapp/config?decoder=json` — and registering the matching driver
opener — changes only this wiring, not the `ConfigLoader`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"time"

	"example.com/typedconfig"

	"gocloud.dev/runtimevar"
	"gocloud.dev/runtimevar/constantvar"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Register a typed decoder for the constant:// scheme on a private mux.
	var mux runtimevar.URLMux
	mux.RegisterVariable("constant", &constantvar.URLOpener{Decoder: config.NewConfigDecoder()})

	raw := `{"service_name":"orders","max_conns":64,"feature_flags":{"new_checkout":true,"legacy_api":false}}`
	v, err := mux.OpenVariable(ctx, "constant://?val="+url.QueryEscape(raw))
	if err != nil {
		log.Fatalf("open variable: %v", err)
	}

	loader := config.NewConfigLoader(v)
	defer loader.Close()

	cfg, err := loader.Load(ctx)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	fmt.Printf("service: %s\n", cfg.ServiceName)
	fmt.Printf("max connections: %d\n", cfg.MaxConns)
	fmt.Printf("flag new_checkout: %v\n", cfg.FeatureFlags["new_checkout"])
	fmt.Printf("ready: %v\n", loader.Ready() == nil)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
service: orders
max connections: 64
flag new_checkout: true
ready: true
```

### Tests

The tests are hermetic: `constantvar.NewBytes` builds a `Variable` from a byte
slice and a decoder with no I/O, so the same loader code is exercised without a
cloud. `TestLoadValid` asserts the typed fields and a non-zero `UpdateTime` (read
from the underlying variable, reachable because the test is in the same package).
`TestLoadWrongType` wires the loader over a `StringDecoder` (whose value is
`*string`) and asserts `Load` returns `ErrUnexpectedType` via `errors.Is` rather
than panicking. `TestLoadMalformed` feeds invalid JSON and asserts the decode
error surfaces from `Load`; because a permanently-bad value never becomes good and
`Latest` blocks until one does, that test bounds its context so the call returns
the decode error instead of hanging. `TestCancelledContextGoodValue` proves the
`Latest` rule: with a good value present, an already-cancelled context still
returns the config. `TestReady` asserts `Ready` is nil once a value has loaded.

Create `config_test.go`:

```go
package config

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"gocloud.dev/runtimevar"
	"gocloud.dev/runtimevar/constantvar"
)

const validJSON = `{"service_name":"orders","max_conns":64,"feature_flags":{"new_checkout":true}}`

func TestLoadValid(t *testing.T) {
	t.Parallel()
	v := constantvar.NewBytes([]byte(validJSON), NewConfigDecoder())
	loader := NewConfigLoader(v)
	t.Cleanup(func() { loader.Close() })

	cfg, err := loader.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ServiceName != "orders" {
		t.Errorf("ServiceName = %q, want %q", cfg.ServiceName, "orders")
	}
	if cfg.MaxConns != 64 {
		t.Errorf("MaxConns = %d, want 64", cfg.MaxConns)
	}
	if !cfg.FeatureFlags["new_checkout"] {
		t.Errorf("FeatureFlags[new_checkout] = false, want true")
	}

	snap, err := v.Latest(context.Background())
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if snap.UpdateTime.IsZero() {
		t.Error("Snapshot.UpdateTime is zero, want a real timestamp")
	}
}

func TestLoadWrongType(t *testing.T) {
	t.Parallel()
	// StringDecoder yields *string, not *Config, so the assertion must fail cleanly.
	v := constantvar.NewBytes([]byte("plain text"), runtimevar.StringDecoder)
	loader := NewConfigLoader(v)
	t.Cleanup(func() { loader.Close() })

	_, err := loader.Load(context.Background())
	if !errors.Is(err, ErrUnexpectedType) {
		t.Fatalf("Load error = %v, want ErrUnexpectedType", err)
	}
}

func TestLoadMalformed(t *testing.T) {
	t.Parallel()
	v := constantvar.NewBytes([]byte("{not valid json"), NewConfigDecoder())
	loader := NewConfigLoader(v)
	t.Cleanup(func() { loader.Close() })

	// A permanently-bad value never becomes "good", so Latest would block forever
	// on a background context; bound it so it returns the decode error instead.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := loader.Load(ctx); err == nil {
		t.Fatal("Load succeeded on malformed JSON, want a decode error")
	}
}

func TestCancelledContextGoodValue(t *testing.T) {
	t.Parallel()
	v := constantvar.NewBytes([]byte(validJSON), NewConfigDecoder())
	loader := NewConfigLoader(v)
	t.Cleanup(func() { loader.Close() })

	// Prime a good value, then read it back with an already-cancelled context.
	if _, err := loader.Load(context.Background()); err != nil {
		t.Fatalf("prime Load: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	cfg, err := loader.Load(ctx)
	if err != nil {
		t.Fatalf("Load with cancelled ctx returned error %v; a good value should win", err)
	}
	if cfg.ServiceName != "orders" {
		t.Errorf("ServiceName = %q, want %q", cfg.ServiceName, "orders")
	}
}

func TestReady(t *testing.T) {
	t.Parallel()
	v := constantvar.NewBytes([]byte(validJSON), NewConfigDecoder())
	loader := NewConfigLoader(v)
	t.Cleanup(func() { loader.Close() })

	if _, err := loader.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := loader.Ready(); err != nil {
		t.Fatalf("Ready = %v, want nil after a good value loaded", err)
	}
}

func Example() {
	v := constantvar.NewBytes([]byte(validJSON), NewConfigDecoder())
	loader := NewConfigLoader(v)
	defer loader.Close()

	cfg, _ := loader.Load(context.Background())
	fmt.Println(cfg.ServiceName, cfg.MaxConns)
	// Output: orders 64
}
```

## Review

The loader is correct when the type of `Snapshot.Value` always matches the
prototype passed to `NewDecoder`: `&Config{}` gives `*Config`, so `Load` asserts
`*Config` and treats a mismatch as `ErrUnexpectedType` rather than panicking.
Confirm with `TestLoadWrongType`, which deliberately wires a `*string`-producing
decoder and checks that the failure is a returned error, not a crash.

The mistakes to avoid are the ones the tests pin down. Do not call `Latest` inside
a readiness probe — it blocks until the first good value, which is why `Ready`
calls the non-blocking `CheckHealth` instead. Do not assume a cancelled context
means "no config": `TestCancelledContextGoodValue` shows that an existing good
value is returned regardless, and only a cold start with a Done context fails.
Remember to `Close` the loader; the hermetic `constantvar` backend has little to
release, but the same code over a cloud driver leaks a polling goroutine if you
skip it. Run `go test -count=1 -race ./...` to confirm the whole set.

## Resources

- [`gocloud.dev/runtimevar`](https://pkg.go.dev/gocloud.dev/runtimevar) — `OpenVariable`, `Variable.Latest`/`CheckHealth`, `NewDecoder`, `JSONDecode`.
- [`gocloud.dev/runtimevar/constantvar`](https://pkg.go.dev/gocloud.dev/runtimevar/constantvar) — the in-memory backend and the `constant://` URL scheme used by the demo.
- [Go CDK: runtime configuration how-to](https://gocloud.dev/howto/runtimevar/) — opening variables by URL and choosing a decoder.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-hot-reload-config-watcher.md](02-hot-reload-config-watcher.md)
