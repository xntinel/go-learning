# Exercise 7: ApplyDefaults: A First Test Over a Struct

Every service fills in config defaults at startup: the operator sets what they
care about, and the loader supplies sane values for the rest. Testing that means
comparing whole structs, which introduces `reflect.DeepEqual` and the `%#v` verb
as first-test tools.

## What you'll build

```text
configdefaults/            independent module: example.com/configdefaults
  go.mod
  config.go                type ServerConfig; func ApplyDefaults(ServerConfig) ServerConfig
  config_test.go           TestApplyDefaults (DeepEqual + no-overwrite), ExampleApplyDefaults
  cmd/
    demo/
      main.go              applies defaults to a zero config and a partial one
```

- Files: `config.go`, `config_test.go`, `cmd/demo/main.go`.
- Implement: `ApplyDefaults(cfg ServerConfig) ServerConfig` filling zero fields — `Port=8080`, `Timeout=30s`, `MaxConns=100`.
- Test: a zero config yields the fully-populated result (`reflect.DeepEqual`, `%#v` on mismatch); an explicitly-set field is not overwritten.
- Verify: `gofmt -l .`, `go vet ./...`, `go test -count=1 -race ./...`.

### Zero-value defaulting, and comparing structs

Go's zero value does the heavy lifting here. A freshly-decoded `ServerConfig` has
`0` for any int the operator omitted and `0` for an omitted `Timeout` (a
`Duration` is an `int64`). `ApplyDefaults` treats zero as "unset" and substitutes
the default, using `cmp.Or` field by field: `cmp.Or(cfg.Port, 8080)` returns the
operator's port if non-zero, otherwise 8080. The function takes the config by
value and returns a modified copy, so it has no side effects on the caller's
struct — a pure transform, which is what makes it testable without setup.

The one semantic wrinkle to state plainly: zero-means-default cannot distinguish
"operator wants port 0" from "operator omitted port". For a server port that is
fine — port 0 is not a value anyone sets deliberately. When a real zero *is* a
valid explicit choice, you need a pointer field or an `explicitly set` bit; that
is a config-loading concern beyond this first test, but worth knowing the limit of
the idiom.

Testing a struct-valued function means you cannot use `==` on anything with
unexported fields, and even for this all-exported struct, asserting field by field
is noisy. `reflect.DeepEqual(got, want)` compares the whole value structurally in
one call. When it fails, the message prints both structs with `%#v`, which renders
`configdefaults.ServerConfig{Port:8080, Timeout:30000000000, MaxConns:100}` —
type name and every field — so the diff is fully legible in CI. The second
assertion confirms the no-overwrite contract: a config that already sets `Port`
must keep it, proving `cmp.Or` substitutes only for the zero value.

Create `config.go`:

```go
package configdefaults

import (
	"cmp"
	"time"
)

// ServerConfig holds the tunables a service reads at startup. A zero field means
// "unset" and is filled by ApplyDefaults.
type ServerConfig struct {
	Port     int
	Timeout  time.Duration
	MaxConns int
}

// ApplyDefaults returns a copy of cfg with each zero-valued field replaced by its
// default. Explicitly-set (non-zero) fields are left untouched.
func ApplyDefaults(cfg ServerConfig) ServerConfig {
	cfg.Port = cmp.Or(cfg.Port, 8080)
	cfg.Timeout = cmp.Or(cfg.Timeout, 30*time.Second)
	cfg.MaxConns = cmp.Or(cfg.MaxConns, 100)
	return cfg
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/configdefaults"
)

func main() {
	zero := configdefaults.ApplyDefaults(configdefaults.ServerConfig{})
	fmt.Printf("from zero:    %#v\n", zero)

	partial := configdefaults.ApplyDefaults(configdefaults.ServerConfig{Port: 9000})
	fmt.Printf("from partial: %#v\n", partial)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
from zero:    configdefaults.ServerConfig{Port:8080, Timeout:30000000000, MaxConns:100}
from partial: configdefaults.ServerConfig{Port:9000, Timeout:30000000000, MaxConns:100}
```

### The tests

Create `config_test.go`:

```go
package configdefaults

import (
	"fmt"
	"reflect"
	"testing"
	"time"
)

func TestApplyDefaults(t *testing.T) {
	t.Parallel()

	// A zero config is fully populated. DeepEqual compares the whole struct;
	// %#v prints type and fields so the diff is legible.
	got := ApplyDefaults(ServerConfig{})
	want := ServerConfig{Port: 8080, Timeout: 30 * time.Second, MaxConns: 100}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ApplyDefaults(zero) = %#v, want %#v", got, want)
	}

	// An explicitly-set field is not overwritten.
	got = ApplyDefaults(ServerConfig{Port: 9000})
	if got.Port != 9000 {
		t.Errorf("ApplyDefaults overwrote an explicit Port: got %d, want 9000", got.Port)
	}
	if got.MaxConns != 100 {
		t.Errorf("ApplyDefaults did not default MaxConns: got %d, want 100", got.MaxConns)
	}
}

func ExampleApplyDefaults() {
	cfg := ApplyDefaults(ServerConfig{})
	fmt.Println(cfg.Port, cfg.Timeout, cfg.MaxConns)
	// Output: 8080 30s 100
}
```

## Review

The defaulting is correct when every zero field is filled and every non-zero
field survives untouched — the two halves of the contract. `reflect.DeepEqual`
compares the populated struct in one call, and `%#v` in the failure message is
what makes a struct diff readable: it names the type and each field rather than
printing an opaque `{8080 30000000000 100}`. Remember the idiom's limit: zero
means default, so a legitimately-zero field would be silently overridden — a
pointer or a set-flag is the escape hatch when that matters. Gate with
`gofmt -l .`, `go vet ./...`, and `go test -count=1 -race ./...`.

## Resources

- [reflect.DeepEqual](https://pkg.go.dev/reflect#DeepEqual) — structural comparison of composite values.
- [cmp.Or](https://pkg.go.dev/cmp#Or) — the value-or-default idiom used per field.
- [fmt package](https://pkg.go.dev/fmt) — the `%#v` Go-syntax representation verb.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [06-deterministic-backoff-duration.md](06-deterministic-backoff-duration.md) | Next: [08-humanize-bytes.md](08-humanize-bytes.md)
