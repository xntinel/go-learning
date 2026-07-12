# Exercise 7: Custom Generators and Minimized Counterexamples for a Config Validator

A service-config validator is a pile of interdependent rules: TLS needs both a
cert and a key, `MaxConns` must be positive, the read timeout must not exceed the
total, the mode must be one of an allowed set, and `https` implies TLS. The hard
part of property-testing it is generation — you need inputs that are *interesting*
(they trip the validator's branches) without being so malformed that every one is
trivially invalid. This exercise is about building generators with `Custom`,
`SampledFrom`, `Ptr`, and `Filter`, and about reading rapid's minimized
counterexample to localize a validator bug fast.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports any other exercise.

## What you'll build

```text
svccfg/                     independent module: example.com/svccfg
  go.mod                    go 1.26, requires pgregory.net/rapid
  svccfg.go                 type Config, TLS; sentinel errors; Validate(Config) error
  cmd/
    demo/
      main.go               runnable demo: validate a good and a bad config
  svccfg_test.go            rapid property: Validate accepts iff an independent spec holds
```

Files: `svccfg.go`, `cmd/demo/main.go`, `svccfg_test.go`.
Implement: a `Config` with interdependent fields, package-level sentinel errors, and `Validate` returning a wrapped sentinel on each violation.
Test: a `rapid.Custom[Config]` generator mixing valid and invalid field values; a property asserting `Validate(c)==nil` if and only if an independent `spec(c)` predicate holds; a `Filter`-based generator of only-valid configs; sentinel assertions with `errors.Is`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/24-property-based-testing/07-custom-generators-and-shrinking/cmd/demo
cd go-solutions/12-testing-ecosystem/24-property-based-testing/07-custom-generators-and-shrinking
go mod edit -go=1.26
go get pgregory.net/rapid@latest
```

### The independent spec, and generating interesting-but-structured inputs

The property here is subtle: `Validate` should return `nil` exactly when the config
is valid. To test that, you need a *second, independent* statement of what "valid"
means — a `spec(c) bool` predicate written from the requirements, not copied from
`Validate`'s code. The property asserts `(Validate(c) == nil) == spec(c)` for every
generated config. If `Validate` accepts something `spec` rejects, or rejects
something `spec` accepts, the property fails and rapid shrinks to the minimal config
that exhibits the disagreement. The danger, called out in the concepts file, is
making `spec` a copy of `Validate` — then they agree by construction and the
property can never fail. Here `spec` is written as a separate flat predicate, so a
divergence in either direction is caught. (In practice `Validate` and `spec` will
look similar because the rules are simple; the discipline is to derive both from
the requirements independently, and in real code `spec` is often the documented
invariant from the design doc.)

Generation is the craft. A generator that only ever produced valid configs would
never exercise the reject branches; one that produced uniformly random bytes would
almost never produce a config valid enough to reach the deeper rules. The generator
below hits the sweet spot with `Custom` composing per-field generators:
`rapid.IntRange(-2, 100)` for `MaxConns` so both the `<= 0` and positive branches
fire; timeouts in `[0, 10]` seconds so zero, ordered, and inverted pairs all occur;
`rapid.SampledFrom` over `{"http", "https", "grpc", "ftp"}` so both allowed and
disallowed modes appear; and `rapid.Ptr(genTLS(), true)` so TLS is sometimes nil
and sometimes present with a mix of empty and non-empty paths. This deliberate
mixture is what makes the property meaningful — every branch of `Validate` is
reached by some generated input.

`Validate` returns a package-level sentinel wrapped with `%w` for each violation,
so callers can branch on `errors.Is`. That is the production idiom: a config loader
that can say "this failed because of TLS" needs a typed error, not a string.

Create `svccfg.go`:

```go
package svccfg

import (
	"errors"
	"fmt"
	"time"
)

// Sentinel errors, one per validation rule, wrapped with %w so callers can branch
// with errors.Is.
var (
	ErrMaxConns    = errors.New("max_conns must be positive")
	ErrTimeouts    = errors.New("timeouts must be positive and read <= total")
	ErrMode        = errors.New("mode must be one of http, https, grpc")
	ErrTLSFiles    = errors.New("tls requires both cert and key paths")
	ErrTLSRequired = errors.New("https mode requires tls")
)

// TLS holds the paths to a certificate and its private key.
type TLS struct {
	CertPath string
	KeyPath  string
}

// Config is a service configuration with interdependent fields.
type Config struct {
	MaxConns     int
	ReadTimeout  time.Duration
	TotalTimeout time.Duration
	Mode         string
	TLS          *TLS
}

// Validate returns nil for a valid config, or a wrapped sentinel for the first rule
// violated.
func Validate(c Config) error {
	if c.MaxConns <= 0 {
		return fmt.Errorf("max_conns=%d: %w", c.MaxConns, ErrMaxConns)
	}
	if c.ReadTimeout <= 0 || c.TotalTimeout <= 0 || c.ReadTimeout > c.TotalTimeout {
		return fmt.Errorf("read=%s total=%s: %w", c.ReadTimeout, c.TotalTimeout, ErrTimeouts)
	}
	switch c.Mode {
	case "http", "https", "grpc":
	default:
		return fmt.Errorf("mode=%q: %w", c.Mode, ErrMode)
	}
	if c.TLS != nil && (c.TLS.CertPath == "" || c.TLS.KeyPath == "") {
		return fmt.Errorf("%w", ErrTLSFiles)
	}
	if c.Mode == "https" && c.TLS == nil {
		return fmt.Errorf("%w", ErrTLSRequired)
	}
	return nil
}
```

### The runnable demo

The demo validates one well-formed config and one that requests `https` without
TLS, showing the sentinel surfacing through `errors.Is`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"time"

	"example.com/svccfg"
)

func main() {
	good := svccfg.Config{
		MaxConns:     100,
		ReadTimeout:  2 * time.Second,
		TotalTimeout: 5 * time.Second,
		Mode:         "http",
	}
	fmt.Println("good:", svccfg.Validate(good))

	bad := svccfg.Config{
		MaxConns:     100,
		ReadTimeout:  2 * time.Second,
		TotalTimeout: 5 * time.Second,
		Mode:         "https", // requires TLS, but TLS is nil
	}
	err := svccfg.Validate(bad)
	fmt.Println("bad:", err)
	fmt.Println("is ErrTLSRequired:", errors.Is(err, svccfg.ErrTLSRequired))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
good: <nil>
bad: https mode requires tls
is ErrTLSRequired: true
```

### The property tests

The first property is the IFF: `Validate` accepts exactly when `spec` says the
config is valid. The second uses `genConfig().Filter(spec)` to produce a stream of
*only-valid* configs and asserts `Validate` accepts every one — a targeted check of
the accept path. Note the caution: `Filter` discards generated values that fail the
predicate, so a filter that rejects almost everything starves the generator; here
`spec` accepts a healthy fraction of the mixed stream, so the filter is cheap, but
if the base generator were mostly-invalid you would build validity into `Custom`
instead of filtering. The comment in `genConfig` also points at
`gen.Example(seed...)`, rapid's tool for eyeballing a generator during development.

Create `svccfg_test.go`:

```go
package svccfg

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"pgregory.net/rapid"
)

// spec is an INDEPENDENT statement of validity, written from the requirements —
// not copied from Validate. The property asserts the two agree.
func spec(c Config) bool {
	if c.MaxConns <= 0 {
		return false
	}
	if c.ReadTimeout <= 0 || c.TotalTimeout <= 0 || c.ReadTimeout > c.TotalTimeout {
		return false
	}
	switch c.Mode {
	case "http", "https", "grpc":
	default:
		return false
	}
	if c.TLS != nil && (c.TLS.CertPath == "" || c.TLS.KeyPath == "") {
		return false
	}
	if c.Mode == "https" && c.TLS == nil {
		return false
	}
	return true
}

func genTLS() *rapid.Generator[TLS] {
	return rapid.Custom(func(t *rapid.T) TLS {
		return TLS{
			CertPath: rapid.SampledFrom([]string{"", "/etc/cert.pem"}).Draw(t, "cert"),
			KeyPath:  rapid.SampledFrom([]string{"", "/etc/key.pem"}).Draw(t, "key"),
		}
	})
}

func genConfig() *rapid.Generator[Config] {
	// During development, gen.Example() prints a sample: genConfig().Example(0).
	return rapid.Custom(func(t *rapid.T) Config {
		return Config{
			MaxConns:     rapid.IntRange(-2, 100).Draw(t, "maxconns"),
			ReadTimeout:  time.Duration(rapid.IntRange(0, 10).Draw(t, "read")) * time.Second,
			TotalTimeout: time.Duration(rapid.IntRange(0, 10).Draw(t, "total")) * time.Second,
			Mode:         rapid.SampledFrom([]string{"http", "https", "grpc", "ftp"}).Draw(t, "mode"),
			TLS:          rapid.Ptr(genTLS(), true).Draw(t, "tls"),
		}
	})
}

func TestValidateMatchesSpec(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		c := genConfig().Draw(t, "config")
		if (Validate(c) == nil) != spec(c) {
			t.Fatalf("Validate=%v disagrees with spec=%v for %+v", Validate(c), spec(c), c)
		}
	})
}

func TestValidateAcceptsValid(t *testing.T) {
	t.Parallel()
	valid := genConfig().Filter(spec)
	rapid.Check(t, func(t *rapid.T) {
		c := valid.Draw(t, "config")
		if err := Validate(c); err != nil {
			t.Fatalf("Validate rejected a valid config %+v: %v", c, err)
		}
	})
}

func TestSentinels(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  Config
		want error
	}{
		{"maxconns", Config{MaxConns: 0, ReadTimeout: time.Second, TotalTimeout: time.Second, Mode: "http"}, ErrMaxConns},
		{"timeouts", Config{MaxConns: 1, ReadTimeout: 5 * time.Second, TotalTimeout: time.Second, Mode: "http"}, ErrTimeouts},
		{"mode", Config{MaxConns: 1, ReadTimeout: time.Second, TotalTimeout: time.Second, Mode: "ftp"}, ErrMode},
		{"tlsfiles", Config{MaxConns: 1, ReadTimeout: time.Second, TotalTimeout: time.Second, Mode: "http", TLS: &TLS{}}, ErrTLSFiles},
		{"tlsrequired", Config{MaxConns: 1, ReadTimeout: time.Second, TotalTimeout: time.Second, Mode: "https"}, ErrTLSRequired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := Validate(tc.cfg); !errors.Is(err, tc.want) {
				t.Fatalf("Validate = %v, want errors.Is %v", err, tc.want)
			}
		})
	}
}

func ExampleValidate() {
	err := Validate(Config{MaxConns: -1})
	fmt.Println(errors.Is(err, ErrMaxConns))
	// Output: true
}
```

## Review

The validator is correct when `Validate(c) == nil` agrees with the independent
`spec(c)` for every generated config, and when each rule surfaces its own sentinel
through `errors.Is`. The mixed generator is what makes the IFF property meaningful:
by drawing `MaxConns` across zero, timeouts across inverted pairs, modes across the
allowed set plus a rogue value, and TLS across nil and half-populated, every branch
of `Validate` is exercised, and any gap — a rule that accepts what it should reject —
shrinks to the minimal config that exposes it.

The mistakes to avoid are the generation traps. First, do not write `spec` by
copying `Validate`: identical logic agrees by construction and tests nothing —
derive both from the requirements so a divergence is possible. Second, do not filter
a mostly-invalid stream down to validity; `Filter` that rejects almost everything
burns the generation budget and rapid eventually gives up — build validity into
`Custom` (compose already-valid fields) and reserve `Filter` for the rare last-mile
constraint. Third, wrap sentinels with `%w`, not `%v` or string concatenation, or
`errors.Is` cannot see through the wrap and callers lose the ability to branch on
the specific failure. Run `go test -race`; the validator is pure.

## Resources

- [`pgregory.net/rapid`](https://pkg.go.dev/pgregory.net/rapid) — `Custom`, `SampledFrom`, `Ptr`, `Filter`, and `Generator.Example`.
- [`errors`](https://pkg.go.dev/errors) — `errors.Is` and error wrapping with `%w`.
- [Go blog: Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — sentinel errors and `%w` wrapping, the idiom `Validate` uses.

---

Back to [06-stateful-lru-cache-model.md](06-stateful-lru-cache-model.md) | Next: [08-reproducibility-and-fuzz-bridge.md](08-reproducibility-and-fuzz-bridge.md)
