# Exercise 9: Defensive deref — safely walk a chain of optional pointers

Reading `cfg.TLS.Cert.Path` where `TLS` and `Cert` are optional `*struct` fields is
a nil-panic waiting to happen: any link in the chain may be `nil`. This module
builds a safe accessor that returns `(value, ok)` instead of panicking, and a
`recover`-based test that pins exactly what an unguarded deref does.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
nilchain/                  independent module: example.com/nilchain
  go.mod                   module example.com/nilchain
  config.go                Config{TLS *TLSConfig}; TLSConfig{Cert *CertConfig}; CertConfig{Path}; CertPath (safe)
  cmd/
    demo/
      main.go              reads a full config and a partial one, prints (value, ok)
  config_test.go           full returns (path,true); nil link returns ("",false); naive deref panics
```

- Files: `config.go`, `cmd/demo/main.go`, `config_test.go`.
- Implement: `CertPath(cfg *Config) (string, bool)` that guards each optional pointer before dereferencing the next.
- Test: a fully-populated config returns `(path, true)`; a config with a nil `Cert` returns `("", false)` without panicking; a `defer`/`recover` test asserts the naive deref path panics.
- Verify: `go test -count=1 -race ./...`

### Guard each link before stepping to the next

`Config` has an optional `*TLSConfig`; `TLSConfig` has an optional `*CertConfig`;
`CertConfig` has a `Path`. A partially-populated config — a common shape for
decoded JSON or a feature that is simply off — may have `TLS == nil` (TLS disabled)
or `TLS != nil` but `Cert == nil` (TLS on, no client cert). Writing
`cfg.TLS.Cert.Path` reads three dereferences in one expression; if `cfg.TLS` is
`nil`, the very first `.Cert` access panics with a runtime nil-pointer-dereference,
and in a handler that is a crash, not a handled "no cert configured".

`CertPath` walks the chain defensively, returning `(value, ok)`:

```go
if cfg == nil || cfg.TLS == nil || cfg.TLS.Cert == nil {
	return "", false
}
return cfg.TLS.Cert.Path, true
```

Go's `||` short-circuits left to right, so each guard runs only after the previous
pointer is known non-nil — `cfg.TLS.Cert` is evaluated only once `cfg.TLS != nil`
has passed. The `(string, bool)` return is the comma-ok pattern applied to a
pointer chain: the caller learns "present" vs "absent" without ever risking a
panic. This is the same shape you use for any deeply-nested optional config or a
partially-populated API response.

Create `config.go`:

```go
package nilchain

// CertConfig is the leaf of the optional chain.
type CertConfig struct {
	Path string
}

// TLSConfig optionally carries a client certificate.
type TLSConfig struct {
	Cert *CertConfig
}

// Config optionally enables TLS.
type Config struct {
	TLS *TLSConfig
}

// CertPath safely reads cfg.TLS.Cert.Path, guarding each optional pointer. It
// returns ("", false) if any link in the chain is nil, and never panics.
func CertPath(cfg *Config) (string, bool) {
	if cfg == nil || cfg.TLS == nil || cfg.TLS.Cert == nil {
		return "", false
	}
	return cfg.TLS.Cert.Path, true
}

// naiveCertPath is the unguarded version, kept only so the test can prove it
// panics on a partial config. Production code must not do this.
func naiveCertPath(cfg *Config) string {
	return cfg.TLS.Cert.Path
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/nilchain"
)

func main() {
	full := &nilchain.Config{TLS: &nilchain.TLSConfig{Cert: &nilchain.CertConfig{Path: "/etc/tls/cert.pem"}}}
	if path, ok := nilchain.CertPath(full); ok {
		fmt.Println("full config cert:", path)
	}

	partial := &nilchain.Config{TLS: &nilchain.TLSConfig{}} // Cert is nil
	if _, ok := nilchain.CertPath(partial); !ok {
		fmt.Println("partial config: no cert, handled without panic")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
full config cert: /etc/tls/cert.pem
partial config: no cert, handled without panic
```

### Tests

`TestFullConfigReturnsPath` asserts a fully-populated config returns `(path, true)`.
`TestNilLinkReturnsFalse` asserts both `TLS == nil` and `Cert == nil` return `("",
false)` without panicking. `TestNaiveDerefPanics` uses `defer`/`recover` to assert
the unguarded path panics on a partial config, pinning why `CertPath` guards.

Create `config_test.go`:

```go
package nilchain

import (
	"fmt"
	"testing"
)

func TestFullConfigReturnsPath(t *testing.T) {
	t.Parallel()

	cfg := &Config{TLS: &TLSConfig{Cert: &CertConfig{Path: "/p"}}}
	if path, ok := CertPath(cfg); !ok || path != "/p" {
		t.Fatalf("CertPath = %q,%v; want /p,true", path, ok)
	}
}

func TestNilLinkReturnsFalse(t *testing.T) {
	t.Parallel()

	cases := map[string]*Config{
		"nil config": nil,
		"nil TLS":    {},
		"nil Cert":   {TLS: &TLSConfig{}},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if path, ok := CertPath(cfg); ok || path != "" {
				t.Fatalf("CertPath = %q,%v; want \"\",false", path, ok)
			}
		})
	}
}

func TestNaiveDerefPanics(t *testing.T) {
	t.Parallel()

	panicked := func() (p bool) {
		defer func() {
			if recover() != nil {
				p = true
			}
		}()
		_ = naiveCertPath(&Config{}) // TLS is nil -> panic on .Cert
		return false
	}()

	if !panicked {
		t.Fatal("naive deref of a partial config should panic; the guard in CertPath is load-bearing")
	}
}

func Example() {
	cfg := &Config{TLS: &TLSConfig{Cert: &CertConfig{Path: "/etc/tls/cert.pem"}}}
	path, ok := CertPath(cfg)
	fmt.Println(path, ok)
	// Output: /etc/tls/cert.pem true
}
```

## Review

The accessor is correct when it returns `(path, true)` only for a fully-populated
chain and `("", false)` for any missing link, and never panics. The `||`
short-circuit is what makes the single guard safe: `cfg.TLS.Cert` is evaluated only
after `cfg.TLS != nil` is established, so the guard never itself triggers the panic
it is preventing. `TestNaiveDerefPanics` is the exercise's point — it shows the
unguarded one-liner crashing on a partial config, which is exactly the production
incident (a nil-deref in a hot handler) the comma-ok accessor removes. For deeper
chains the same pattern scales: one guard expression, one `return zero, false`. Run
`go test -race`.

## Resources

- [Go Language Specification: Address operators and indirection](https://go.dev/ref/spec#Address_operators) — dereferencing a nil pointer panics.
- [`recover`](https://go.dev/ref/spec#Handling_panics) — catching the panic in the test harness.
- [Effective Go: Errors and recover](https://go.dev/doc/effective_go#recover)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [08-range-copy-address-trap.md](08-range-copy-address-trap.md) | Next: [10-reset-on-flush-swap.md](10-reset-on-flush-swap.md)
