# Exercise 3: Layered config resolution with nil-means-inherit

Production services resolve configuration from several sources: compiled defaults,
a config file, then environment overrides, each layer winning over the ones below.
The clean way to model "this layer did not specify this setting" versus "this layer
explicitly set it (perhaps to zero)" is a struct of pointer fields, where nil
means inherit and a non-nil pointer means override. This exercise builds that
resolver and uses `new(expr)` — including on function results — to construct the
override layers without temporaries.

This module is fully self-contained: its own `go mod init`, all types inline, its
own demo and tests.

## What you'll build

```text
configmerge/                 independent module: example.com/configmerge
  go.mod                     go 1.26
  config.go                  Config (concrete); Layer (pointer fields); Merge;
                             Resolve; LayerFromEnv; ErrBadEnv
  cmd/
    demo/
      main.go                resolves config from a simulated env map
  config_test.go             precedence tests; Resolve; malformed-env; Example
```

- Files: `config.go`, `cmd/demo/main.go`, `config_test.go`.
- Implement: a pointer-field `Layer`, `Merge(base, over Layer) Layer` folding one layer onto another, `Resolve(defaults Config, layers ...Layer) Config` collapsing the stack into a concrete config, and `LayerFromEnv(lookup) (Layer, error)` parsing env values into a layer with `new(expr)`.
- Test: assert `Merge` precedence (higher layer wins, including override-to-zero beating a non-zero lower value; nil leaves the lower value intact), assert `Resolve` produces the expected concrete config, and assert the malformed-env path with `errors.Is`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/48-modern-go-language-and-stdlib/15-new-expr-initialized-allocation/03-config-overlay-merge/cmd/demo
cd go-solutions/48-modern-go-language-and-stdlib/15-new-expr-initialized-allocation/03-config-overlay-merge
go mod edit -go=1.26
```

### The two shapes: concrete config and override layer

`Config` is the effective, fully-resolved configuration the application reads —
every field is a concrete value. `Layer` is an *override*: the same fields, but as
pointers, so each field carries the tri-state nil / pointer-to-zero /
pointer-to-value. A nil field in a layer means "this source said nothing, inherit
from below"; a pointer to `false` means "this source explicitly turned it off",
which must beat a lower layer that had it on.

`Merge(base, over)` folds one layer onto another: for each field, if the higher
(`over`) layer set it, it wins; otherwise the lower value survives. `Resolve`
starts from a concrete `Config` of defaults, folds every layer left-to-right into
one merged `Layer`, then collapses that onto the defaults — copying a field only
when its pointer is non-nil. The collapse checks `nil`, never zero: a pointer to
`0` is an explicit override and must be applied. This is exactly why you cannot
use `cmp.Or` here — `cmp.Or` returns the first *non-zero* argument, so it would
discard an explicit-zero override and reintroduce the ambiguity the pointers
exist to remove.

### new(expr) on function results

`LayerFromEnv` reads env values and constructs a `Layer` from the parsed results.
Building a layer inline is where `new(expr)` shines, including on values produced
by a function call — a case that had no `&` form at all, since a call result is
not addressable:

```go
p := &clamp(raw, 1, 100)   // compile error: cannot address a call result
p := new(clamp(raw, 1, 100)) // fine: allocates, initializes, returns *int
```

When the value comes from a fallible parse (`strconv.Atoi` returns a value and an
error), you must handle the error first, so the pointer is taken from the parsed
variable; but for pure helpers, `new(f(...))` puts the result straight into the
layer.

Create `config.go`:

```go
package configmerge

import (
	"errors"
	"fmt"
	"strconv"
)

// ErrBadEnv is returned (wrapped) when an environment value fails to parse.
var ErrBadEnv = errors.New("invalid environment value")

// Config is the effective, fully-resolved configuration. Every field is concrete.
type Config struct {
	MaxConns int
	Timeout  int // seconds
	Verbose  bool
	Endpoint string
}

// Layer is one override source. A nil field means "unset at this layer, inherit
// from below"; a non-nil field is an explicit override, even to a zero value.
type Layer struct {
	MaxConns *int    `json:"max_conns,omitempty"`
	Timeout  *int    `json:"timeout,omitempty"`
	Verbose  *bool   `json:"verbose,omitempty"`
	Endpoint *string `json:"endpoint,omitempty"`
}

// Merge folds over onto base: for each field the higher layer (over) wins when it
// set the field, otherwise base's value survives. It checks nil, not zero, so an
// explicit pointer-to-zero override is preserved.
func Merge(base, over Layer) Layer {
	if over.MaxConns != nil {
		base.MaxConns = over.MaxConns
	}
	if over.Timeout != nil {
		base.Timeout = over.Timeout
	}
	if over.Verbose != nil {
		base.Verbose = over.Verbose
	}
	if over.Endpoint != nil {
		base.Endpoint = over.Endpoint
	}
	return base
}

// Resolve folds the layers left-to-right (later layers win) and collapses the
// result onto defaults, producing a concrete Config. A field left nil across all
// layers keeps its default; a field set to zero at some layer overrides to zero.
func Resolve(defaults Config, layers ...Layer) Config {
	var merged Layer
	for _, l := range layers {
		merged = Merge(merged, l)
	}

	out := defaults
	if merged.MaxConns != nil {
		out.MaxConns = *merged.MaxConns
	}
	if merged.Timeout != nil {
		out.Timeout = *merged.Timeout
	}
	if merged.Verbose != nil {
		out.Verbose = *merged.Verbose
	}
	if merged.Endpoint != nil {
		out.Endpoint = *merged.Endpoint
	}
	return out
}

// LayerFromEnv builds a Layer from a lookup function (os.LookupEnv in
// production). LookupEnv distinguishes an unset variable from one set to empty,
// so a set-but-empty endpoint becomes an explicit override, not an inherit.
func LayerFromEnv(lookup func(string) (string, bool)) (Layer, error) {
	var l Layer

	if v, ok := lookup("APP_MAX_CONNS"); ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Layer{}, fmt.Errorf("%w: APP_MAX_CONNS=%q: %v", ErrBadEnv, v, err)
		}
		l.MaxConns = new(n)
	}
	if v, ok := lookup("APP_TIMEOUT"); ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Layer{}, fmt.Errorf("%w: APP_TIMEOUT=%q: %v", ErrBadEnv, v, err)
		}
		l.Timeout = new(n)
	}
	if v, ok := lookup("APP_VERBOSE"); ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return Layer{}, fmt.Errorf("%w: APP_VERBOSE=%q: %v", ErrBadEnv, v, err)
		}
		l.Verbose = new(b)
	}
	if v, ok := lookup("APP_ENDPOINT"); ok {
		l.Endpoint = new(v) // new(expr) straight from the looked-up string
	}
	return l, nil
}
```

### The runnable demo

The demo resolves a config from compiled defaults, a simulated file layer, and a
simulated environment map. The env map turns verbosity off explicitly (an
override-to-zero) and raises the connection limit; the timeout is set only by the
file layer and the endpoint only by the defaults.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/configmerge"
)

func main() {
	defaults := configmerge.Config{
		MaxConns: 10,
		Timeout:  30,
		Verbose:  true,
		Endpoint: "https://prod.example.com",
	}

	// Simulated config file: raise the timeout only.
	file := configmerge.Layer{Timeout: new(60)}

	// Simulated environment: a map-backed lookup instead of os.LookupEnv.
	env := map[string]string{
		"APP_MAX_CONNS": "100",
		"APP_VERBOSE":   "false", // explicit override to zero
	}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }

	envLayer, err := configmerge.LayerFromEnv(lookup)
	if err != nil {
		panic(err)
	}

	got := configmerge.Resolve(defaults, file, envLayer)
	fmt.Printf("conns=%d timeout=%d verbose=%v endpoint=%s\n",
		got.MaxConns, got.Timeout, got.Verbose, got.Endpoint)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
conns=100 timeout=60 verbose=false endpoint=https://prod.example.com
```

`MaxConns` came from the env layer (100 beat the default 10), `Timeout` from the
file layer (60), `Verbose` was explicitly turned off by the env layer (false beat
the default true), and `Endpoint` was set by no layer so it inherited the default.

### Tests

The table constructs `defaults` plus a stack of layers and asserts `Resolve`
produces the expected concrete `Config`, covering the three cases that matter:
higher layer wins, override-to-zero beats a non-zero lower value, and a field left
nil everywhere keeps its default. `TestLayerFromEnv_Bad` asserts the wrapped
sentinel with `errors.Is`. `TestNewOnCallResult` proves `new(expr)` works on a
non-addressable call result. The `Example` shows an env layer overriding a value
to false. The last test is the "your turn" case: a layer whose only override is a
pointer-to-zero must survive all the way through `Resolve`.

Create `config_test.go`:

```go
package configmerge

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func TestResolve(t *testing.T) {
	t.Parallel()

	defaults := Config{MaxConns: 10, Timeout: 30, Verbose: true, Endpoint: "default"}

	tests := []struct {
		name   string
		layers []Layer
		want   Config
	}{
		{
			name:   "no layers keeps defaults",
			layers: nil,
			want:   defaults,
		},
		{
			name:   "higher layer wins",
			layers: []Layer{{MaxConns: new(20)}, {MaxConns: new(50)}},
			want:   Config{MaxConns: 50, Timeout: 30, Verbose: true, Endpoint: "default"},
		},
		{
			name:   "override to zero beats non-zero default",
			layers: []Layer{{Verbose: new(false), MaxConns: new(0)}},
			want:   Config{MaxConns: 0, Timeout: 30, Verbose: false, Endpoint: "default"},
		},
		{
			name:   "nil field inherits lower layer",
			layers: []Layer{{Timeout: new(45)}, {MaxConns: new(99)}},
			want:   Config{MaxConns: 99, Timeout: 45, Verbose: true, Endpoint: "default"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := Resolve(defaults, tt.layers...)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Resolve = %+v; want %+v", got, tt.want)
			}
		})
	}
}

func TestLayerFromEnv(t *testing.T) {
	t.Parallel()

	env := map[string]string{
		"APP_MAX_CONNS": "42",
		"APP_ENDPOINT":  "", // set but empty: an explicit override, not inherit
	}
	l, err := LayerFromEnv(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	if err != nil {
		t.Fatalf("LayerFromEnv error: %v", err)
	}
	if l.MaxConns == nil || *l.MaxConns != 42 {
		t.Errorf("MaxConns = %v; want *42", l.MaxConns)
	}
	if l.Endpoint == nil || *l.Endpoint != "" {
		t.Errorf("Endpoint = %v; want *\"\" (explicit empty)", l.Endpoint)
	}
	if l.Timeout != nil {
		t.Errorf("Timeout = %v; want nil (unset)", l.Timeout)
	}
}

func TestLayerFromEnv_Bad(t *testing.T) {
	t.Parallel()

	env := map[string]string{"APP_MAX_CONNS": "not-a-number"}
	_, err := LayerFromEnv(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	if !errors.Is(err, ErrBadEnv) {
		t.Fatalf("LayerFromEnv error = %v; want ErrBadEnv", err)
	}
}

// clampConns is a pure helper used to show new(expr) on a call result.
func clampConns(v int) int {
	if v < 1 {
		return 1
	}
	if v > 100 {
		return 100
	}
	return v
}

// TestNewOnCallResult shows new(expr) working on a non-addressable call result:
// &clampConns(500) is illegal, new(clampConns(500)) is not.
func TestNewOnCallResult(t *testing.T) {
	t.Parallel()

	layer := Layer{MaxConns: new(clampConns(500))}
	if layer.MaxConns == nil || *layer.MaxConns != 100 {
		t.Fatalf("MaxConns = %v; want *100", layer.MaxConns)
	}
}

// TestPointerToZeroSurvivesResolve is the "your turn" case: a layer whose only
// override is a pointer to zero must be retained through Resolve.
func TestPointerToZeroSurvivesResolve(t *testing.T) {
	t.Parallel()

	defaults := Config{MaxConns: 10, Timeout: 30}
	got := Resolve(defaults, Layer{MaxConns: new(0)})
	if got.MaxConns != 0 {
		t.Fatalf("MaxConns = %d; want 0 (explicit zero override)", got.MaxConns)
	}
	if got.Timeout != 30 {
		t.Fatalf("Timeout = %d; want 30 (inherited)", got.Timeout)
	}
}

func ExampleResolve() {
	defaults := Config{MaxConns: 10, Verbose: true, Endpoint: "prod"}
	file := Layer{MaxConns: new(50)}
	env := Layer{Verbose: new(false)} // explicit override to zero
	got := Resolve(defaults, file, env)
	fmt.Printf("conns=%d verbose=%v endpoint=%s\n", got.MaxConns, got.Verbose, got.Endpoint)
	// Output: conns=50 verbose=false endpoint=prod
}
```

## Review

The resolver is correct when a field is copied onto the effective config exactly
when some layer set its pointer, so a higher layer beats a lower one, an explicit
zero beats a non-zero default, and a field nil across all layers keeps its
default. The trap that this lesson exists to prevent is collapsing the layers with
`cmp.Or` or "first non-zero wins" logic: that treats a pointer-to-zero as unset
and silently drops an explicit override, which is why `Resolve` checks `nil` and
the dedicated test proves a `new(0)` layer survives. Use `os.LookupEnv` (not
`os.Getenv`) so a variable set to the empty string is an explicit override rather
than an inherit — the `LayerFromEnv` test pins that distinction. Confirm with
`go test -count=1 -race ./...`: the `Resolve` table covers precedence, the env
tests cover parsing and the wrapped sentinel, and `TestNewOnCallResult` shows
`new(expr)` on a value no `&` could address.

## Resources

- [Chris Siebenmann — Go's builtin new() will take an expression in Go 1.26](https://utcc.utoronto.ca/~cks/space/blog/programming/GoNewWithExpression) — the addressability motivation.
- [`strconv` package](https://pkg.go.dev/strconv) — `Atoi` and `ParseBool` for env parsing.
- [`os.LookupEnv`](https://pkg.go.dev/os#LookupEnv) — distinguishing unset from empty.
- [`cmp.Or`](https://pkg.go.dev/cmp#Or) — first non-zero value, and why it is wrong for collapsing tri-state pointers.

---

Back to [02-cloud-sdk-request-builder.md](02-cloud-sdk-request-builder.md) | Next: [../16-go-fix-inline-modernization/00-concepts.md](../16-go-fix-inline-modernization/00-concepts.md)
