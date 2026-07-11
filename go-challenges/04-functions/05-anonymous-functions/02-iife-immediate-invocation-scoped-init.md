# Exercise 2: Immediately-Invoked Function Literal for Startup Config Assembly

At process startup you often need to turn a raw config slice into a validated,
immutable lookup structure — the set of allowed CORS origins, the enabled
feature-flag keys — and you want the temporary builder locals to vanish once the
value is built. An immediately-invoked function literal does exactly that: it runs
inline, scopes its locals out of the surrounding block, and hands back only the
finished value. This module builds a CORS-origin allowlist that way and contrasts
the IIFE with a plain helper and with the panic-at-init variant.

This module is fully self-contained. Nothing here imports another exercise.

## What you'll build

```text
originset/                    module example.com/originset
  go.mod
  config.go                   Config; New (helper), MustNew (IIFE+panic); Allows, Origins, OriginSet
  config_test.go              exact-keys, source-independence, defensive-copy, panic-on-invalid
  cmd/demo/main.go            build an allowlist and probe it
```

- Files: `config.go`, `config_test.go`, `cmd/demo/main.go`.
- Implement: `buildOriginSet` returning a validated `map[string]struct{}` or an error; `New` (helper style) returning `(*Config, error)`; `MustNew` assembling the field via an IIFE that panics on an invalid allowlist; `Allows`, `Origins`, `OriginSet`.
- Test: the set has exactly the expected keys; mutating the source slice leaves it unchanged; `OriginSet` returns an independent copy; an invalid or duplicate entry makes `MustNew` panic.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/originset/cmd/demo
cd ~/go-exercises/originset
go mod init example.com/originset
```

### IIFE for value, helper for error, IIFE for panic

Three ways to assemble the set sit side by side so you can see when each fits.

`buildOriginSet` is a plain helper: it validates each origin, rejects blanks,
malformed entries, and duplicates, and returns either the finished
`map[string]struct{}` or an error. Prefer a helper when the caller should *handle*
the failure — `New` wraps it and returns `(*Config, error)`.

`MustNew` is the startup path where an invalid allowlist is not a recoverable
condition but a deployment bug that must crash the process loudly and immediately.
Its `origins` field is assigned by an immediately-invoked function literal:

```go
origins: func() map[string]struct{} {
	set, err := buildOriginSet(raw)
	if err != nil {
		panic(err)
	}
	return set
}(),
```

The trailing `()` runs the literal at the point of definition and yields its
result straight into the struct field. The `set` and `err` locals live only inside
the literal — they never enter the enclosing scope, so there is no half-built map
or stale `err` sitting around after construction. This is the canonical
`must := func() T { ... }()` idiom: turn an unrecoverable init error into a panic
at startup rather than a zero value that fails mysteriously under load.

The set uses the `map[string]struct{}` idiom: `struct{}` occupies zero bytes, so
the map is a pure membership set. `Allows` is a single map probe. `Origins`
returns a sorted slice (a fresh allocation) and `OriginSet` returns
`maps.Clone(c.origins)` — both defensive copies, so a caller cannot reach in and
mutate the immutable allowlist the rest of the process relies on.

Create `config.go`:

```go
package originset

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// ErrInvalidOrigin marks a rejected allowlist entry.
var ErrInvalidOrigin = errors.New("invalid origin")

// Config holds an immutable set of permitted CORS origins.
type Config struct {
	origins map[string]struct{}
}

// buildOriginSet validates raw and returns a membership set, or an error on a
// blank, malformed, or duplicate entry. This is the helper (error-returning)
// style, for callers that want to handle failure.
func buildOriginSet(raw []string) (map[string]struct{}, error) {
	set := make(map[string]struct{}, len(raw))
	for _, o := range raw {
		o = strings.TrimSpace(o)
		if o == "" || !strings.Contains(o, "://") {
			return nil, fmt.Errorf("%w: %q", ErrInvalidOrigin, o)
		}
		if _, dup := set[o]; dup {
			return nil, fmt.Errorf("%w: duplicate %q", ErrInvalidOrigin, o)
		}
		set[o] = struct{}{}
	}
	return set, nil
}

// New validates raw and returns a Config or an error (helper style).
func New(raw []string) (*Config, error) {
	set, err := buildOriginSet(raw)
	if err != nil {
		return nil, err
	}
	return &Config{origins: set}, nil
}

// MustNew assembles the allowlist via an IIFE that panics on an invalid entry,
// so a bad deployment config crashes at startup. The builder locals never escape
// the literal.
func MustNew(raw []string) *Config {
	return &Config{
		origins: func() map[string]struct{} {
			set, err := buildOriginSet(raw)
			if err != nil {
				panic(err)
			}
			return set
		}(),
	}
}

// Allows reports whether origin is in the allowlist.
func (c *Config) Allows(origin string) bool {
	_, ok := c.origins[origin]
	return ok
}

// Origins returns the allowlist as a sorted slice (a defensive copy).
func (c *Config) Origins() []string {
	return slices.Sorted(maps.Keys(c.origins))
}

// OriginSet returns an independent copy of the underlying set, so a caller
// cannot mutate the Config's internal state.
func (c *Config) OriginSet() map[string]struct{} {
	return maps.Clone(c.origins)
}
```

### The runnable demo

The demo builds an allowlist with `MustNew`, prints the sorted origins, and probes
two lookups. All output is deterministic because `Origins` sorts.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/originset"
)

func main() {
	cfg := originset.MustNew([]string{
		"https://app.example.com",
		"https://admin.example.com",
	})

	fmt.Println("allowlist:", cfg.Origins())
	fmt.Println("app allowed:  ", cfg.Allows("https://app.example.com"))
	fmt.Println("evil allowed: ", cfg.Allows("https://evil.example.com"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
allowlist: [https://admin.example.com https://app.example.com]
app allowed:   true
evil allowed:  false
```

### Tests

The tests pin the three properties that matter. `TestExactKeys` asserts the set
holds precisely the expected origins and nothing else. `TestSourceIndependence`
mutates the *source slice* after construction and asserts the set is unchanged —
the builder copied the values in, so the config does not alias its input.
`TestDefensiveCopy` mutates the map returned by `OriginSet` and asserts the
internal set is untouched. `TestMustNewPanicsOnInvalid` recovers the documented
panic and confirms it wraps `ErrInvalidOrigin`.

Create `config_test.go`:

```go
package originset

import (
	"errors"
	"fmt"
	"slices"
	"testing"
)

func TestExactKeys(t *testing.T) {
	t.Parallel()
	cfg, err := New([]string{"https://a.example", "https://b.example"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	want := []string{"https://a.example", "https://b.example"}
	if got := cfg.Origins(); !slices.Equal(got, want) {
		t.Fatalf("Origins() = %v, want %v", got, want)
	}
	if cfg.Allows("https://c.example") {
		t.Fatal("Allows returned true for an origin not in the set")
	}
}

func TestSourceIndependence(t *testing.T) {
	t.Parallel()
	src := []string{"https://a.example", "https://b.example"}
	cfg := MustNew(src)

	src[0] = "https://hijacked.example" // mutate the source after building

	if !cfg.Allows("https://a.example") {
		t.Fatal("config lost the original origin after source mutation")
	}
	if cfg.Allows("https://hijacked.example") {
		t.Fatal("config aliased the mutated source slice")
	}
}

func TestDefensiveCopy(t *testing.T) {
	t.Parallel()
	cfg := MustNew([]string{"https://a.example"})

	copySet := cfg.OriginSet()
	copySet["https://injected.example"] = struct{}{}

	if cfg.Allows("https://injected.example") {
		t.Fatal("mutating OriginSet()'s copy changed the internal set")
	}
}

func TestMustNewPanicsOnInvalid(t *testing.T) {
	t.Parallel()
	cases := map[string][]string{
		"malformed": {"not-a-url"},
		"blank":     {"   "},
		"duplicate": {"https://a.example", "https://a.example"},
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				r := recover()
				if r == nil {
					t.Fatal("MustNew did not panic on invalid input")
				}
				err, ok := r.(error)
				if !ok || !errors.Is(err, ErrInvalidOrigin) {
					t.Fatalf("panic value = %v, want error wrapping ErrInvalidOrigin", r)
				}
			}()
			MustNew(raw)
		})
	}
}

func ExampleConfig_Allows() {
	cfg := MustNew([]string{"https://app.example.com"})
	fmt.Println(cfg.Allows("https://app.example.com"), cfg.Allows("https://evil.example.com"))
	// Output: true false
}
```

## Review

The IIFE earns its place here for exactly two reasons: it scopes the `set`/`err`
builder locals out of the constructor, and it lets `MustNew` fold "validate, then
panic on failure" into a single field initializer. If those locals were not
transient — if a caller needed to inspect the error — you would use the `New`
helper instead; the module ships both so the contrast is explicit. The correctness
proof is that the set contains exactly the expected keys, that mutating the source
slice or the `OriginSet` copy cannot reach the internal state (the values were
copied in, and `maps.Clone` returns an independent map), and that an invalid
allowlist panics with an error wrapping `ErrInvalidOrigin` rather than silently
admitting the wrong origins. Do not overuse the IIFE — when there are no locals to
hide and no inline error to handle, a plain assignment is clearer.

## Resources

- [Go Language Specification: Function literals](https://go.dev/ref/spec#Function_literals)
- [maps.Clone](https://pkg.go.dev/maps#Clone)
- [slices.Sorted](https://pkg.go.dev/slices#Sorted)
- [Effective Go: the empty struct](https://go.dev/doc/effective_go)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-worker-pool-goroutine-literals.md](01-worker-pool-goroutine-literals.md) | Next: [03-deferred-closure-named-return-observability.md](03-deferred-closure-named-return-observability.md)
