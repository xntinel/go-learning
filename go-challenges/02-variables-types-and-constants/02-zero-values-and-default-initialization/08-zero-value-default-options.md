# Exercise 8: An Options Struct Where The Zero Value Is A Sensible Default

A well-designed client should be configurable with `New(Config{})` and produce
safe production behavior — no ceremony, no half-configured client. This exercise
builds a `Config` whose zero value normalizes to documented defaults through an
internal `withDefaults` step, using `cmp.Or` as the fill-the-default operator,
and is explicit about the one footgun: a caller-supplied `0` is treated as
"omitted", not "unlimited".

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
clientcfg/                 independent module: example.com/clientcfg
  go.mod
  clientcfg.go             Config, withDefaults, Client, accessors
  cmd/
    demo/
      main.go              builds a client from Config{} and from explicit values
  clientcfg_test.go        zero normalizes to defaults; explicit passes; clamps
```

Files: `clientcfg.go`, `cmd/demo/main.go`, `clientcfg_test.go`.
Implement: a `Config{Timeout, MaxRetries, BaseBackoff}` and a `withDefaults` that fills zero `Timeout`/`BaseBackoff` from documented defaults via `cmp.Or` and clamps negative `MaxRetries` to `0`; a `Client` built from the normalized config with exported accessors.
Test: a zero `Config` normalizes to the documented defaults; explicit values pass through; invalid/negative values are clamped; the documented "explicit 0 -> default" policy holds.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/02-variables-types-and-constants/02-zero-values-and-default-initialization/08-zero-value-default-options/cmd/demo
cd go-solutions/02-variables-types-and-constants/02-zero-values-and-default-initialization/08-zero-value-default-options
```

## Why cmp.Or, and the explicit-zero policy

`cmp.Or` (Go 1.22+) returns its first non-zero argument, which makes it the exact
tool for "use the caller's value, or the default if they left it zero":
`cmp.Or(c.Timeout, defaultTimeout)` yields the caller's timeout when they set one
and `defaultTimeout` when the field is still its zero value. Applied field by
field in `withDefaults`, it turns a zero `Config` into a fully-specified one and
leaves any explicitly-set field untouched. This is the zero-value-defaults
pattern: the type's zero value is not merely valid, it is the *recommended*
default configuration.

The clamp is the other half. `cmp.Or` treats "non-zero" as "keep", so a *negative*
`MaxRetries` (which is non-zero) would pass through unchanged — and a negative
retry count is nonsense. `withDefaults` therefore clamps it explicitly with
`max(c.MaxRetries, 0)`. The lesson: `cmp.Or` fills *absence*, it does not
*validate*; ranges and invariants still need their own guard.

The footgun to document, loudly, is the ambiguity this pattern bakes in: because
a caller-supplied `0` timeout is indistinguishable from an omitted one, both
become `defaultTimeout`. That is fine — and ergonomic — *if* your documented
policy is "0 means use the default". It is a disaster if a caller reasonably
expects `Timeout: 0` to mean "no timeout / unlimited", because they will silently
get 30 seconds instead. When `0` must carry a distinct meaning like "unlimited",
zero-value-defaults is the wrong pattern and you need a `*time.Duration` (nil =
omitted, non-nil zero = unlimited) or a sentinel. Here the policy is stated and
tested: `0` means default.

Create `clientcfg.go`:

```go
package clientcfg

import (
	"cmp"
	"time"
)

// Documented defaults. Policy: a zero Timeout or BaseBackoff means "use the
// default", NOT "unlimited"/"no wait". A negative MaxRetries is clamped to 0.
const (
	defaultTimeout     = 30 * time.Second
	defaultBaseBackoff = 100 * time.Millisecond
)

// Config configures a Client. Its zero value normalizes to the documented
// production defaults, so New(Config{}) is a valid, safe client.
type Config struct {
	Timeout     time.Duration
	MaxRetries  int
	BaseBackoff time.Duration
}

// withDefaults returns a copy of c with absent fields filled and invalid fields
// clamped.
func (c Config) withDefaults() Config {
	c.Timeout = cmp.Or(c.Timeout, defaultTimeout)
	c.BaseBackoff = cmp.Or(c.BaseBackoff, defaultBaseBackoff)
	c.MaxRetries = max(c.MaxRetries, 0)
	return c
}

// Client is configured once from a normalized Config.
type Client struct {
	cfg Config
}

// New builds a Client, normalizing cfg to defaults.
func New(cfg Config) *Client {
	return &Client{cfg: cfg.withDefaults()}
}

// Timeout reports the effective request timeout.
func (c *Client) Timeout() time.Duration { return c.cfg.Timeout }

// MaxRetries reports the effective retry count.
func (c *Client) MaxRetries() int { return c.cfg.MaxRetries }

// BaseBackoff reports the effective base backoff.
func (c *Client) BaseBackoff() time.Duration { return c.cfg.BaseBackoff }
```

## The runnable demo

The demo builds one client from the zero `Config` (all defaults) and one with
explicit values including a negative retry count (clamped), and prints the
effective settings.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/clientcfg"
)

func main() {
	def := clientcfg.New(clientcfg.Config{})
	fmt.Printf("defaults: timeout=%s retries=%d backoff=%s\n",
		def.Timeout(), def.MaxRetries(), def.BaseBackoff())

	custom := clientcfg.New(clientcfg.Config{
		Timeout:    5 * time.Second,
		MaxRetries: -3, // invalid, clamped to 0
	})
	fmt.Printf("custom:   timeout=%s retries=%d backoff=%s\n",
		custom.Timeout(), custom.MaxRetries(), custom.BaseBackoff())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
defaults: timeout=30s retries=0 backoff=100ms
custom:   timeout=5s retries=0 backoff=100ms
```

## Tests

`TestWithDefaults` is a table over four cases: a zero config fills every default;
explicit values pass through; a negative retry count clamps to `0`; and an
explicit `0` timeout resolves to the default per the documented policy — the case
that pins the footgun.

Create `clientcfg_test.go`:

```go
package clientcfg

import (
	"testing"
	"time"
)

func TestWithDefaults(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   Config
		want Config
	}{
		{
			name: "zero config fills all defaults",
			in:   Config{},
			want: Config{Timeout: defaultTimeout, MaxRetries: 0, BaseBackoff: defaultBaseBackoff},
		},
		{
			name: "explicit values pass through",
			in:   Config{Timeout: 5 * time.Second, MaxRetries: 3, BaseBackoff: time.Second},
			want: Config{Timeout: 5 * time.Second, MaxRetries: 3, BaseBackoff: time.Second},
		},
		{
			name: "negative retries clamp to zero",
			in:   Config{MaxRetries: -3},
			want: Config{Timeout: defaultTimeout, MaxRetries: 0, BaseBackoff: defaultBaseBackoff},
		},
		{
			name: "explicit zero timeout resolves to default (documented policy)",
			in:   Config{Timeout: 0, MaxRetries: 2},
			want: Config{Timeout: defaultTimeout, MaxRetries: 2, BaseBackoff: defaultBaseBackoff},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.in.withDefaults(); got != tt.want {
				t.Fatalf("withDefaults(%+v) = %+v, want %+v", tt.in, got, tt.want)
			}
		})
	}
}

func TestNewNormalizes(t *testing.T) {
	t.Parallel()

	c := New(Config{})
	if c.Timeout() != defaultTimeout {
		t.Fatalf("Timeout() = %s, want %s", c.Timeout(), defaultTimeout)
	}
	if c.MaxRetries() != 0 {
		t.Fatalf("MaxRetries() = %d, want 0", c.MaxRetries())
	}
	if c.BaseBackoff() != defaultBaseBackoff {
		t.Fatalf("BaseBackoff() = %s, want %s", c.BaseBackoff(), defaultBaseBackoff)
	}
}
```

## Review

The config is correct when `New(Config{})` yields the documented defaults and any
explicitly set field survives normalization untouched. `cmp.Or` handles the fill
cleanly, but remember it fills *absence*, not correctness — the negative-retries
clamp shows that a non-zero-but-invalid value slips past `cmp.Or` and needs its
own guard. The pattern's defining risk is the ambiguity between an explicit `0`
and an omitted field: it is only safe because the policy "0 means default" is
documented and tested here. If a field ever needs `0` to mean something distinct
(unlimited, disabled), switch that field to a pointer or a sentinel rather than
bending `cmp.Or` around it. Contrast this with functional options
(`New(WithTimeout(...))`), which make "not provided" structurally distinct at the
cost of more machinery; zero-value-defaults trades that away for ergonomics.

## Resources

- [`cmp.Or`](https://pkg.go.dev/cmp#Or) — returns the first non-zero argument.
- [Effective Go: composite literals](https://go.dev/doc/effective_go#composite_literals) — the zero-value-useful design principle.
- [`time.Duration`](https://pkg.go.dev/time#Duration) — the timeout/backoff type and its formatting.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [07-sync-once-lazy-singleton.md](07-sync-once-lazy-singleton.md) | Next: [09-comparable-struct-cache-key.md](09-comparable-struct-cache-key.md)
