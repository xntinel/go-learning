# Exercise 9: Load Config Numbers Without Losing int64 Precision

Decode `{"account_id": 9007199254740993}` into a `map[string]any` and read it back:
you get `9007199254740992`, silently off by one, because every JSON number in an
`any` becomes a `float64` and `float64` is exact only to 2^53. For a config value,
a Snowflake-style ID, or a ledger amount, that is data corruption with no error.
This module builds a config loader that decodes with `Decoder.UseNumber`, so large
integers arrive as exact `json.Number` values, plus the `,string` tag technique for
IDs transported as strings.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
confignum/                     independent module: example.com/confignum
  go.mod                       go 1.24
  config/
    config.go                  Loader (UseNumber) with Int64/Float64; Pricing json:",string"
  cmd/
    demo/
      main.go                  show float64 corruption vs UseNumber exactness
  config/config_test.go        corruption proof, exact decode, overflow error, ,string round-trip
```

Files: `config/config.go`, `cmd/demo/main.go`, `config/config_test.go`.
Implement: `Loader` over `map[string]any` decoded with `Decoder.UseNumber`, typed `Int64`/`Float64` accessors returning errors, and a `Pricing` struct with a `json:",string"` field.
Test: a large integer into plain `any` is corrupted; with `UseNumber` `Number.Int64()` is exact; `Int64()` on a fractional literal errors; a `,string` field round-trips quoted.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/confignum/config ~/go-exercises/confignum/cmd/demo
cd ~/go-exercises/confignum
go mod init example.com/confignum
go mod edit -go=1.24
```

## Why any corrupts integers, and two ways out

When `encoding/json` decodes a number into an `any` (or `map[string]any`, or
`[]any`), it has no target type to guide it, so it always chooses `float64`. A
`float64` has a 52-bit mantissa, so it represents every integer exactly only up to
2^53 = 9007199254740992. One past that, 9007199254740993, is not representable and
rounds to 9007199254740992 — and crucially there is **no error**; the corruption is
silent. Any 64-bit identifier decoded through `any` is at risk.

There are two fixes, and this module builds both.

`Decoder.UseNumber()` changes the decode of numbers-into-`any`: instead of
`float64`, each number becomes a `json.Number`, which is really a `string` holding
the original digits. No precision is lost at decode time because nothing is
converted yet. You convert on demand: `Number.Int64()` parses to an exact `int64`
(and returns an error on overflow or a non-integer), `Number.Float64()` parses to a
`float64` when you actually want floating point. The loader wraps this in typed
accessors so callers get an `int64` and a real error, never a corrupted value.

The `,string` struct tag is the transport-side fix. Appending `,string` to a
numeric field's tag (`json:"account_id,string"`) makes `encoding/json` encode that
number as a JSON **string** (`"account_id":"9007199254740993"`) and decode it back
from a string. This is how you safely send a 64-bit ID to a consumer whose native
number type is a float — JavaScript, most notably, where `JSON.parse` would
otherwise mangle it. The value stays a normal `int64` in Go; only its wire
representation is quoted.

Create `config/config.go`:

```go
// config/config.go
package config

import (
	"encoding/json"
	"fmt"
	"io"
)

// ErrMissingKey and ErrWrongType classify accessor failures.
var (
	ErrMissingKey = fmt.Errorf("config key not found")
	ErrWrongType  = fmt.Errorf("config value has wrong type")
)

// Loader holds untyped config decoded with UseNumber, so numbers are exact
// json.Number values rather than lossy float64.
type Loader struct {
	values map[string]any
}

// Load decodes JSON config, preserving numeric precision via UseNumber.
func Load(r io.Reader) (*Loader, error) {
	dec := json.NewDecoder(r)
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	return &Loader{values: m}, nil
}

// Int64 returns an exact integer, erroring on a missing key, a non-number, or a
// non-integer/overflowing value.
func (l *Loader) Int64(key string) (int64, error) {
	v, ok := l.values[key]
	if !ok {
		return 0, fmt.Errorf("%w: %q", ErrMissingKey, key)
	}
	num, ok := v.(json.Number)
	if !ok {
		return 0, fmt.Errorf("%w: %q is %T, not a number", ErrWrongType, key, v)
	}
	n, err := num.Int64()
	if err != nil {
		return 0, fmt.Errorf("%w: %q = %s: %v", ErrWrongType, key, num, err)
	}
	return n, nil
}

// Float64 returns a floating-point config value.
func (l *Loader) Float64(key string) (float64, error) {
	v, ok := l.values[key]
	if !ok {
		return 0, fmt.Errorf("%w: %q", ErrMissingKey, key)
	}
	num, ok := v.(json.Number)
	if !ok {
		return 0, fmt.Errorf("%w: %q is %T, not a number", ErrWrongType, key, v)
	}
	f, err := num.Float64()
	if err != nil {
		return 0, fmt.Errorf("%w: %q = %s: %v", ErrWrongType, key, num, err)
	}
	return f, nil
}

// Pricing transports a 64-bit account ID as a JSON string via the ,string tag,
// so a float-based consumer cannot corrupt it.
type Pricing struct {
	AccountID int64   `json:"account_id,string"`
	Rate      float64 `json:"rate"`
}
```

## The runnable demo

The demo decodes the same large integer twice — once into a plain `any` (corrupted)
and once through the `UseNumber` loader (exact) — then marshals a `Pricing` to show
the `,string` field quoted on the wire.

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"example.com/confignum/config"
)

func main() {
	const body = `{"id":9007199254740993}`

	var m map[string]any
	_ = json.Unmarshal([]byte(body), &m)
	fmt.Printf("plain any:  %d\n", int64(m["id"].(float64)))

	l, _ := config.Load(strings.NewReader(body))
	id, _ := l.Int64("id")
	fmt.Printf("UseNumber:  %d\n", id)

	b, _ := json.Marshal(config.Pricing{AccountID: 9007199254740993, Rate: 1.5})
	fmt.Printf("string tag: %s\n", b)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
plain any:  9007199254740992
UseNumber:  9007199254740993
string tag: {"account_id":"9007199254740993","rate":1.5}
```

The first line is off by one: that is the silent corruption. The second is exact.

## Tests

`TestPlainAnyCorruptsLargeInt` documents the trap: decoding into `any` yields a
`float64` that has lost the last digit. `TestUseNumberIsExact` asserts the loader
returns the exact `int64`. `TestInt64OnFractionalErrors` asserts `Int64` on a
non-integer literal returns `ErrWrongType`. `TestStringTagRoundTrip` asserts the
`,string` field is quoted on the wire and decodes back exactly.

Create `config/config_test.go`:

```go
// config/config_test.go
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestPlainAnyCorruptsLargeInt(t *testing.T) {
	t.Parallel()
	const exact = 9007199254740993 // 2^53 + 1
	var m map[string]any
	if err := json.Unmarshal([]byte(`{"id":9007199254740993}`), &m); err != nil {
		t.Fatal(err)
	}
	f, ok := m["id"].(float64)
	if !ok {
		t.Fatalf("expected float64 from plain any, got %T", m["id"])
	}
	if int64(f) == exact {
		t.Fatal("expected float64 to corrupt the value, but it was exact")
	}
	if int64(f) != 9007199254740992 {
		t.Fatalf("unexpected corrupted value %d", int64(f))
	}
}

func TestUseNumberIsExact(t *testing.T) {
	t.Parallel()
	l, err := Load(strings.NewReader(`{"id":9007199254740993}`))
	if err != nil {
		t.Fatal(err)
	}
	got, err := l.Int64("id")
	if err != nil {
		t.Fatal(err)
	}
	if got != 9007199254740993 {
		t.Fatalf("UseNumber lost precision: got %d", got)
	}
}

func TestInt64OnFractionalErrors(t *testing.T) {
	t.Parallel()
	l, err := Load(strings.NewReader(`{"rate":1.5}`))
	if err != nil {
		t.Fatal(err)
	}
	_, err = l.Int64("rate")
	if !errors.Is(err, ErrWrongType) {
		t.Fatalf("expected ErrWrongType for a fractional value, got %v", err)
	}
}

func TestMissingKeyErrors(t *testing.T) {
	t.Parallel()
	l, _ := Load(strings.NewReader(`{}`))
	_, err := l.Int64("nope")
	if !errors.Is(err, ErrMissingKey) {
		t.Fatalf("expected ErrMissingKey, got %v", err)
	}
}

func TestStringTagRoundTrip(t *testing.T) {
	t.Parallel()
	data, err := json.Marshal(Pricing{AccountID: 9007199254740993, Rate: 1.5})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"account_id":"9007199254740993"`) {
		t.Fatalf("account_id should be a quoted string: %s", data)
	}
	var back Pricing
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if back.AccountID != 9007199254740993 {
		t.Fatalf(",string round trip lost precision: %d", back.AccountID)
	}
}

func ExampleLoader_Int64() {
	l, _ := Load(strings.NewReader(`{"id":9007199254740993}`))
	id, _ := l.Int64("id")
	fmt.Println(id)
	// Output: 9007199254740993
}
```

## Review

The loader is correct when a value above 2^53 survives decode exactly through
`UseNumber` while the same value through a plain `any` is provably corrupted, and
when a non-integer literal produces a typed error instead of a truncated result.
The rule is blunt: never decode a number you care about into `any` and trust it —
use `UseNumber`/`json.Number` or a typed struct field with the right integer type.
For IDs that must cross into a float-based runtime, `,string` moves them as text so
neither side ever rounds them. These are not edge cases; they are the default
behavior of `map[string]any`, which is why untyped JSON config and 64-bit IDs are a
recurring source of production incidents.

## Resources

- [`json.Decoder.UseNumber`](https://pkg.go.dev/encoding/json#Decoder.UseNumber) — decode numbers as `json.Number` instead of `float64`.
- [`json.Number`](https://pkg.go.dev/encoding/json#Number) — `Int64`/`Float64` conversion with overflow reporting.
- [`encoding/json.Marshal`](https://pkg.go.dev/encoding/json#Marshal) — the `,string` tag option for transporting numbers as JSON strings.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [08-streaming-ndjson-ingest.md](08-streaming-ndjson-ingest.md) | Next: [10-reflect-structtag-column-mapper.md](10-reflect-structtag-column-mapper.md)
