# Exercise 2: Applying Defaults When the Config Pointer Is Nil

The idiomatic way to accept configuration is a pointer whose nil value and whose
zero fields both mean "use the default". This module builds a `Load` that
normalizes an HTTP-server config once at the boundary so the rest of the program
can treat the result as fully populated and never nil-check a timeout again.

This module is fully self-contained.

## What you'll build

```text
srvconfig/                independent module: example.com/srvconfig
  go.mod                  go 1.24
  config.go               type ServerConfig; Load(cfg *ServerConfig) *ServerConfig using cmp.Or
  cmd/
    demo/
      main.go             runnable demo: nil, partial, and complete inputs
  config_test.go          table tests for nil/partial/complete + non-mutation
```

Files: `config.go`, `cmd/demo/main.go`, `config_test.go`.
Implement: `Load(cfg *ServerConfig) *ServerConfig` that treats nil and zero fields as "default" via `cmp.Or`, returning a fresh fully-populated non-nil config.
Test: nil input yields all defaults; a partial config keeps its set fields and fills the rest; a complete config comes back unchanged; the returned pointer is never nil and is distinct from the input so mutating it does not touch the caller's value.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/srvconfig/cmd/demo
cd ~/go-exercises/srvconfig
go mod init example.com/srvconfig
go mod edit -go=1.24
```

### Normalize once, not everywhere

Config flows into a program from flags, env, and files, all of which routinely
leave a field unset. If every consumer of the config writes `timeout :=
cfg.ReadTimeout; if timeout == 0 { timeout = 15 * time.Second }`, the default is
duplicated at every site and drifts. The boundary pattern is to run one `Load`
that accepts a possibly-nil, possibly-partial `*ServerConfig` and returns a fresh
config with every field populated. Downstream code takes the result and never
checks nil or zero again.

`cmp.Or` (Go 1.22+) is built for exactly the per-field "first non-zero" choice:
`cmp.Or(cfg.Addr, defaultAddr)` yields `cfg.Addr` when it is set and
`defaultAddr` when it is the zero string. Because `time.Duration` and `int64` are
comparable, the same call works for every field, replacing an if-ladder.

Two design points make `Load` safe to trust. It handles nil by substituting an
empty config, so a nil pointer flows into the same per-field logic as an all-zero
one — nil and "sent nothing" are treated identically, which is the intended
meaning here. And it returns a *new* pointer rather than mutating the argument in
place, so the caller's original value (often a zero struct) is never touched and
the returned config is independently owned.

Create `config.go`:

```go
package srvconfig

import (
	"cmp"
	"time"
)

// Defaults for an HTTP server. Exported so callers and tests can reference them.
const (
	DefaultAddr         = ":8080"
	DefaultReadTimeout  = 15 * time.Second
	DefaultWriteTimeout = 15 * time.Second
	DefaultMaxBodyBytes = 1 << 20 // 1 MiB
)

// ServerConfig configures an HTTP server. A nil *ServerConfig or any zero field
// means "use the default"; Load resolves both.
type ServerConfig struct {
	Addr         string
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	MaxBodyBytes int64
}

// Load returns a fully-populated config: it substitutes defaults for a nil
// pointer and for every zero field, and always returns a fresh non-nil pointer
// distinct from the input. Downstream code can use the result without any
// further nil or zero checks.
func Load(cfg *ServerConfig) *ServerConfig {
	if cfg == nil {
		cfg = &ServerConfig{}
	}
	return &ServerConfig{
		Addr:         cmp.Or(cfg.Addr, DefaultAddr),
		ReadTimeout:  cmp.Or(cfg.ReadTimeout, DefaultReadTimeout),
		WriteTimeout: cmp.Or(cfg.WriteTimeout, DefaultWriteTimeout),
		MaxBodyBytes: cmp.Or(cfg.MaxBodyBytes, DefaultMaxBodyBytes),
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/srvconfig"
)

func main() {
	// A nil pointer: entirely defaulted.
	full := srvconfig.Load(nil)
	fmt.Printf("nil -> addr=%s read=%s\n", full.Addr, full.ReadTimeout)

	// A partial config: keep Addr, default the rest.
	partial := srvconfig.Load(&srvconfig.ServerConfig{Addr: ":9000"})
	fmt.Printf("partial -> addr=%s read=%s max=%d\n",
		partial.Addr, partial.ReadTimeout, partial.MaxBodyBytes)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
nil -> addr=:8080 read=15s
partial -> addr=:9000 read=15s max=1048576
```

### Tests

The table drives the three shapes of input, and a dedicated test proves the
non-mutation property: `Load` returns a pointer different from a non-nil argument
and never nil.

Create `config_test.go`:

```go
package srvconfig

import (
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   *ServerConfig
		want ServerConfig
	}{
		{
			name: "nil yields all defaults",
			in:   nil,
			want: ServerConfig{DefaultAddr, DefaultReadTimeout, DefaultWriteTimeout, DefaultMaxBodyBytes},
		},
		{
			name: "empty yields all defaults",
			in:   &ServerConfig{},
			want: ServerConfig{DefaultAddr, DefaultReadTimeout, DefaultWriteTimeout, DefaultMaxBodyBytes},
		},
		{
			name: "partial keeps set fields",
			in:   &ServerConfig{Addr: ":9000", ReadTimeout: 3 * time.Second},
			want: ServerConfig{":9000", 3 * time.Second, DefaultWriteTimeout, DefaultMaxBodyBytes},
		},
		{
			name: "complete is unchanged",
			in:   &ServerConfig{":1", time.Second, 2 * time.Second, 42},
			want: ServerConfig{":1", time.Second, 2 * time.Second, 42},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := Load(tt.in)
			if got == nil {
				t.Fatal("Load returned nil")
			}
			if *got != tt.want {
				t.Fatalf("Load = %+v, want %+v", *got, tt.want)
			}
		})
	}
}

func TestLoadReturnsFreshPointer(t *testing.T) {
	t.Parallel()

	in := &ServerConfig{Addr: ":9000"}
	got := Load(in)
	if got == in {
		t.Fatal("Load returned the same pointer; it must return a fresh copy")
	}
	// Mutating the result must not touch the caller's value.
	got.Addr = ":changed"
	if in.Addr != ":9000" {
		t.Fatalf("input mutated: Addr = %q", in.Addr)
	}
}

func TestLoadNilDoesNotMutateZeroValue(t *testing.T) {
	t.Parallel()

	var zero ServerConfig
	_ = Load(&zero)
	if (zero != ServerConfig{}) {
		t.Fatalf("Load mutated the caller's zero value: %+v", zero)
	}
}
```

## Review

`Load` is correct when it is total: for any input — nil, empty, partial, or
complete — it returns a non-nil config with every field either the caller's set
value or the exported default, and it never mutates the argument. The table pins
the field logic; `TestLoadReturnsFreshPointer` pins that the result is
independently owned. If a future edit switches `cmp.Or` for in-place assignment
on the argument, `TestLoadReturnsFreshPointer` fails immediately.

The trap avoided here is the scattered default: had defaulting lived at each call
site, one site would eventually forget it and ship a zero timeout to production.
Concentrating it in one `Load` at the boundary is the fix.

## Resources

- [cmp.Or](https://pkg.go.dev/cmp#Or) — returns the first non-zero argument.
- [Go Blog: Go 1.22 is released](https://go.dev/blog/go1.22) — introduces `cmp.Or` among other additions.
- [Effective Go: Pointers vs. Values](https://go.dev/doc/effective_go#pointers_vs_values) — passing a pointer to allow "nil means default".

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-nil-safe-cache-nil-receiver.md](01-nil-safe-cache-nil-receiver.md) | Next: [03-patch-handler-pointer-optional-fields.md](03-patch-handler-pointer-optional-fields.md)
