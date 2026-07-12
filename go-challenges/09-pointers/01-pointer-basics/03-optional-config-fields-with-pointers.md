# Exercise 3: Config loader — distinguish "field absent" from "field set to zero"

This is the canonical real-world reason pointer fields exist. A JSON config
overlay must tell "the operator did not set `max_conns`, use the default" apart
from "the operator explicitly set `max_conns` to 0". A plain `int` cannot; a
`*int` can, because `nil` means absent and a pointer-to-`0` means explicit zero.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
configmerge/               independent module: example.com/configmerge
  go.mod                   module example.com/configmerge
  config.go                Overlay{MaxConns *int, Timeout *string, TLSEnabled *bool}; Config; Parse; Apply
  cmd/
    demo/
      main.go              parses three payloads, prints the merged result
  config_test.go           {}, {"max_conns":0}, {"max_conns":50}, {"max_conns":null}
```

- Files: `config.go`, `cmd/demo/main.go`, `config_test.go`.
- Implement: an `Overlay` with pointer fields, `Parse([]byte) (Overlay, error)`, and `Apply(defaults Config) Config` that merges the overlay over defaults.
- Test: `{}` leaves all defaults; `{"max_conns":0}` overrides to `0`; `{"max_conns":50}` overrides to `50`; `{"max_conns":null}` leaves the pointer `nil` (default wins).
- Verify: `go test -count=1 -race ./...`

### Absent, zero, and null are three different things

`Config` is the resolved, fully-populated configuration the service runs on: plain
value fields, no pointers. `Overlay` is the operator's partial input: every field
is a pointer, so `nil` encodes "not provided". `Parse` unmarshals JSON into an
`Overlay`. The JSON semantics do the work:

- `{}` — the key `max_conns` is absent, so `json.Unmarshal` never touches the
  field; starting from a zero `Overlay`, `MaxConns` stays `nil`.
- `{"max_conns": 50}` — a present value; `json.Unmarshal` allocates a new `int`,
  stores `50`, and points `MaxConns` at it.
- `{"max_conns": 0}` — also a present value; `MaxConns` becomes a non-nil pointer
  to `0`. This is the case a plain `int` cannot represent: it looks identical to
  the zero value of an untouched field.
- `{"max_conns": null}` — JSON `null` explicitly sets the pointer to `nil`.

`Apply` walks each overlay field: if the pointer is non-nil, its pointed-to value
overrides the default; if it is `nil`, the default is kept. Because a non-nil
pointer to `0` is not `nil`, an explicit `0` overrides — that is the entire point.
If `MaxConns` were a plain `int`, `Apply` would have to guess whether `0` meant
"unset" or "explicitly zero", and every choice it made would be wrong for some
operator.

Create `config.go`:

```go
package configmerge

import "encoding/json"

// Config is the resolved configuration the service runs on. Plain value fields:
// by the time it is built every field has a concrete value.
type Config struct {
	MaxConns   int
	Timeout    string
	TLSEnabled bool
}

// Overlay is an operator's partial input. Every field is a pointer so that nil
// means "not provided" and a non-nil pointer to the zero value means
// "explicitly set to zero".
type Overlay struct {
	MaxConns   *int    `json:"max_conns"`
	Timeout    *string `json:"timeout"`
	TLSEnabled *bool   `json:"tls_enabled"`
}

// Parse unmarshals a JSON overlay. Absent keys leave the corresponding pointer
// nil; a JSON null sets it to nil; a present value allocates and fills it.
func Parse(data []byte) (Overlay, error) {
	var o Overlay
	if err := json.Unmarshal(data, &o); err != nil {
		return Overlay{}, err
	}
	return o, nil
}

// Apply merges the overlay over defaults: each non-nil overlay field overrides
// the default; each nil field leaves the default in place. A non-nil pointer to
// zero (0, "", false) overrides, which a plain value field could not express.
func (o Overlay) Apply(defaults Config) Config {
	out := defaults
	if o.MaxConns != nil {
		out.MaxConns = *o.MaxConns
	}
	if o.Timeout != nil {
		out.Timeout = *o.Timeout
	}
	if o.TLSEnabled != nil {
		out.TLSEnabled = *o.TLSEnabled
	}
	return out
}
```

### The runnable demo

The demo shows the decisive contrast: an empty overlay keeps `max_conns=100`, but
`{"max_conns":0}` drives it to `0` — an override a value field would silently drop.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/configmerge"
)

func main() {
	defaults := configmerge.Config{MaxConns: 100, Timeout: "30s", TLSEnabled: true}

	for _, payload := range []string{`{}`, `{"max_conns":0}`, `{"max_conns":50}`} {
		o, err := configmerge.Parse([]byte(payload))
		if err != nil {
			fmt.Println("parse error:", err)
			return
		}
		merged := o.Apply(defaults)
		fmt.Printf("%-18s -> MaxConns=%d Timeout=%s TLS=%v\n",
			payload, merged.MaxConns, merged.Timeout, merged.TLSEnabled)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
{}                 -> MaxConns=100 Timeout=30s TLS=true
{"max_conns":0}    -> MaxConns=0 Timeout=30s TLS=true
{"max_conns":50}   -> MaxConns=50 Timeout=30s TLS=true
```

### Tests

The suite feeds the four payloads and asserts the merged result. The load-bearing
assertion is that `{}` and `{"max_conns":0}` produce *different* merged configs
(`100` vs `0`) — the behavior a value field cannot deliver. `TestNullSetsPointerNil`
asserts a JSON `null` leaves `MaxConns` `nil`, so the default wins just as an absent
key would (the merged value is the default, but the mechanism is `null -> nil`).

Create `config_test.go`:

```go
package configmerge

import (
	"fmt"
	"testing"
)

func defaults() Config {
	return Config{MaxConns: 100, Timeout: "30s", TLSEnabled: true}
}

func TestAbsentVersusExplicitZeroDiffer(t *testing.T) {
	t.Parallel()

	absent, err := Parse([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	explicitZero, err := Parse([]byte(`{"max_conns":0}`))
	if err != nil {
		t.Fatal(err)
	}

	a := absent.Apply(defaults())
	z := explicitZero.Apply(defaults())

	if a.MaxConns != 100 {
		t.Fatalf("absent MaxConns = %d, want 100 (default wins)", a.MaxConns)
	}
	if z.MaxConns != 0 {
		t.Fatalf("explicit-zero MaxConns = %d, want 0 (override wins)", z.MaxConns)
	}
	if a.MaxConns == z.MaxConns {
		t.Fatal("absent and explicit-zero merged to the same value; pointer field failed to distinguish them")
	}
}

func TestPresentValueOverrides(t *testing.T) {
	t.Parallel()

	o, err := Parse([]byte(`{"max_conns":50,"timeout":"","tls_enabled":false}`))
	if err != nil {
		t.Fatal(err)
	}
	got := o.Apply(defaults())

	if got.MaxConns != 50 {
		t.Fatalf("MaxConns = %d, want 50", got.MaxConns)
	}
	if got.Timeout != "" {
		t.Fatalf("Timeout = %q, want empty (explicit override)", got.Timeout)
	}
	if got.TLSEnabled != false {
		t.Fatalf("TLSEnabled = %v, want false (explicit override)", got.TLSEnabled)
	}
}

func TestNullSetsPointerNil(t *testing.T) {
	t.Parallel()

	o, err := Parse([]byte(`{"max_conns":null}`))
	if err != nil {
		t.Fatal(err)
	}
	if o.MaxConns != nil {
		t.Fatalf("MaxConns pointer = %v, want nil after JSON null", o.MaxConns)
	}
	if got := o.Apply(defaults()); got.MaxConns != 100 {
		t.Fatalf("MaxConns = %d, want 100 (null leaves default)", got.MaxConns)
	}
}

func Example() {
	absent, _ := Parse([]byte(`{}`))
	zero, _ := Parse([]byte(`{"max_conns":0}`))
	d := Config{MaxConns: 100}
	fmt.Println(absent.Apply(d).MaxConns, zero.Apply(d).MaxConns)
	// Output: 100 0
}
```

## Review

The design is correct when `{}` and `{"max_conns":0}` merge to different configs;
if they collapse to the same value you have used a value field where a pointer field
was required, and an operator's explicit `0` is being clobbered by the default. The
three JSON states map cleanly onto the pointer: absent key leaves it untouched (stays
`nil` from the zero struct), `null` sets it `nil`, and any present value — including
`0`, `""`, `false` — allocates a non-nil pointer that `Apply` treats as an override.
Do not "simplify" `Overlay` to value fields to avoid the pointers; that erases the
one distinction the type exists to carry. The `json` struct tags are load-bearing:
they map `max_conns` to `MaxConns`, and dropping them would silently stop the
override from binding.

## Resources

- [JSON and Go](https://go.dev/blog/json) — how `encoding/json` treats pointer fields, `null`, and absent keys.
- [`encoding/json.Unmarshal`](https://pkg.go.dev/encoding/json#Unmarshal) — allocation of pointer fields and null handling.
- [Effective Go: Pointers vs. Values](https://go.dev/doc/effective_go#pointers_vs_values)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-distinct-pointer-type-contract.md](02-distinct-pointer-type-contract.md) | Next: [04-repository-lookup-nil-return.md](04-repository-lookup-nil-return.md)
