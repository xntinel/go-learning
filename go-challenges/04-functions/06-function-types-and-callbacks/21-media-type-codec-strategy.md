# Exercise 21: Media Type Codec Selection via Strategy Callbacks

**Nivel: Intermedio** — validacion rapida (un test corto).

An HTTP API that speaks more than one wire format — JSON for browsers, a
compact binary format for internal services — picks which one to use per
request by reading the `Accept` header. This module builds that negotiation
as a `Registry` of `Codec` strategies, each one an `Encoder`/`Decoder`
callback pair, so adding a third format never touches the negotiation logic.

## What you'll build

```text
mediacodec/                 independent module: example.com/media-type-codec-strategy
  go.mod                     go 1.24
  mediacodec.go                type Encoder, type Decoder, type Codec, JSONCodec, GobCodec, type Registry, func NewRegistry, (Registry) Select, (Registry) Negotiate
  cmd/
    demo/
      main.go                  runnable demo: negotiate three different Accept headers, round trip each
  mediacodec_test.go           table test: each codec's round trip, exact select, Accept-list negotiation, wildcard default, no-match error
```

Files: `mediacodec.go`, `cmd/demo/main.go`, `mediacodec_test.go`.
Implement: `type Encoder func(v any) ([]byte, error)`, `type Decoder func(data []byte, v any) error`, a `Codec` struct pairing a `ContentType` with both, `JSONCodec()` over `encoding/json`, `GobCodec()` over `encoding/gob`, and a `Registry` with `NewRegistry`, `Select` (exact match), and `Negotiate` (Accept-header preference list, `"*/*"` falling back to the first-registered codec).
Test: JSON round trip, gob round trip, `Select` hit/miss, `Negotiate` picking the first supported entry across three different Accept headers, `Negotiate("*/*")` returning the same deterministic default across repeated calls, and `Negotiate` erroring when nothing in the list is supported.
Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/media-type-codec-strategy/cmd/demo
cd ~/go-exercises/media-type-codec-strategy
go mod init example.com/media-type-codec-strategy
go mod edit -go=1.24
```

### Why `Registry` stores an order alongside the map, not just the map

`Encoder`/`Decoder` hide the real difference between formats — `json.Marshal`
takes a value and returns bytes, `gob.NewEncoder(w).Encode` wants a writer —
behind one shape a negotiator can call without knowing which library is
underneath. That part is the ordinary strategy pattern. The subtler design
point is what `Negotiate` does for a wildcard `Accept: */*`: a client that
accepts anything still gets one specific response, and that choice has to be
the same every time or two identical requests could get two different wire
formats. The obvious implementation — range over a `map[string]Codec` and
return whatever comes out first — is exactly wrong, because Go deliberately
randomizes map iteration order per run. `Registry` keeps a parallel `order
[]string` recording registration order, so `"*/*"` always resolves to
`order[0]`: the first codec the caller registered, every time, in every
process.

Create `mediacodec.go`:

```go
// Package mediacodec selects an encoding strategy (JSON, gob, ...) based on
// HTTP content-type negotiation, adapting each format behind one function
// type pair.
package mediacodec

import (
	"bytes"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"strings"
)

// Encoder serializes v into bytes for one media type.
type Encoder func(v any) ([]byte, error)

// Decoder deserializes data into v for one media type.
type Decoder func(data []byte, v any) error

// Codec pairs a media type with its Encoder/Decoder strategy.
type Codec struct {
	ContentType string
	Encode      Encoder
	Decode      Decoder
}

// JSONCodec adapts encoding/json into the Codec shape.
func JSONCodec() Codec {
	return Codec{
		ContentType: "application/json",
		Encode: func(v any) ([]byte, error) {
			return json.Marshal(v)
		},
		Decode: func(data []byte, v any) error {
			return json.Unmarshal(data, v)
		},
	}
}

// GobCodec adapts encoding/gob into the Codec shape. It stands in for a
// compact binary wire format (the role protobuf or msgpack would play in a
// real service) using only the standard library.
func GobCodec() Codec {
	return Codec{
		ContentType: "application/x-gob",
		Encode: func(v any) ([]byte, error) {
			var buf bytes.Buffer
			if err := gob.NewEncoder(&buf).Encode(v); err != nil {
				return nil, fmt.Errorf("gob encode: %w", err)
			}
			return buf.Bytes(), nil
		},
		Decode: func(data []byte, v any) error {
			if err := gob.NewDecoder(bytes.NewReader(data)).Decode(v); err != nil {
				return fmt.Errorf("gob decode: %w", err)
			}
			return nil
		},
	}
}

// Registry selects a Codec by media type, and negotiates one from an HTTP
// Accept header's ordered preference list. byType is the lookup table;
// order preserves registration order so "*/*" has one deterministic,
// server-chosen default instead of depending on Go's randomized map
// iteration.
type Registry struct {
	byType map[string]Codec
	order  []string
}

// NewRegistry builds a Registry from codecs, keyed by ContentType. The
// first codec passed in becomes the server's default for a wildcard
// ("*/*") Accept header.
func NewRegistry(codecs ...Codec) Registry {
	r := Registry{byType: make(map[string]Codec, len(codecs))}
	for _, c := range codecs {
		r.byType[c.ContentType] = c
		r.order = append(r.order, c.ContentType)
	}
	return r
}

// Select looks up a Codec by an exact media type, as from a Content-Type
// request header.
func (r Registry) Select(contentType string) (Codec, error) {
	c, ok := r.byType[contentType]
	if !ok {
		return Codec{}, fmt.Errorf("mediacodec: unsupported content type %q", contentType)
	}
	return c, nil
}

// Negotiate picks a Codec from an HTTP Accept header, which lists media
// types in the client's order of preference (any ";q=" weight is ignored;
// entries are tried left to right). A "*/*" entry matches the server's
// first-registered default codec.
func (r Registry) Negotiate(accept string) (Codec, error) {
	for _, part := range strings.Split(accept, ",") {
		mediaType := strings.TrimSpace(strings.SplitN(part, ";", 2)[0])
		if mediaType == "*/*" {
			if len(r.order) == 0 {
				continue
			}
			return r.byType[r.order[0]], nil
		}
		if c, ok := r.byType[mediaType]; ok {
			return c, nil
		}
	}
	return Codec{}, fmt.Errorf("mediacodec: no codec satisfies Accept: %q", accept)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/media-type-codec-strategy"
)

type payload struct {
	ID   int
	Name string
}

func main() {
	registry := mediacodec.NewRegistry(mediacodec.JSONCodec(), mediacodec.GobCodec())

	for _, accept := range []string{"application/x-gob", "text/plain, application/json", "*/*"} {
		codec, err := registry.Negotiate(accept)
		if err != nil {
			fmt.Printf("Accept %q: error: %v\n", accept, err)
			continue
		}

		in := payload{ID: 7, Name: "widget"}
		encoded, err := codec.Encode(in)
		if err != nil {
			fmt.Println("encode error:", err)
			continue
		}
		var out payload
		if err := codec.Decode(encoded, &out); err != nil {
			fmt.Println("decode error:", err)
			continue
		}
		fmt.Printf("Accept %q -> %s: roundtrip=%v\n", accept, codec.ContentType, out == in)
	}

	if _, err := registry.Select("application/xml"); err != nil {
		fmt.Println("select xml:", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
Accept "application/x-gob" -> application/x-gob: roundtrip=true
Accept "text/plain, application/json" -> application/json: roundtrip=true
Accept "*/*" -> application/json: roundtrip=true
select xml: mediacodec: unsupported content type "application/xml"
```

### Tests

Create `mediacodec_test.go`:

```go
package mediacodec

import "testing"

type widget struct {
	ID   int
	Name string
}

func TestJSONCodecRoundTrip(t *testing.T) {
	t.Parallel()
	c := JSONCodec()
	in := widget{ID: 1, Name: "gizmo"}
	encoded, err := c.Encode(in)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	var out widget
	if err := c.Decode(encoded, &out); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if out != in {
		t.Fatalf("roundtrip = %+v, want %+v", out, in)
	}
}

func TestGobCodecRoundTrip(t *testing.T) {
	t.Parallel()
	c := GobCodec()
	in := widget{ID: 2, Name: "sprocket"}
	encoded, err := c.Encode(in)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	var out widget
	if err := c.Decode(encoded, &out); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if out != in {
		t.Fatalf("roundtrip = %+v, want %+v", out, in)
	}
}

func TestRegistrySelectExactMatch(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(JSONCodec(), GobCodec())
	c, err := reg.Select("application/json")
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if c.ContentType != "application/json" {
		t.Fatalf("ContentType = %q, want application/json", c.ContentType)
	}
	if _, err := reg.Select("application/xml"); err == nil {
		t.Fatal("expected error selecting an unregistered content type")
	}
}

func TestNegotiatePicksFirstSupportedEntry(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(JSONCodec(), GobCodec())

	tests := []struct {
		accept string
		want   string
	}{
		{"application/x-gob", "application/x-gob"},
		{"text/plain, application/json", "application/json"},
		{"text/plain, application/x-gob, application/json", "application/x-gob"},
	}
	for _, tc := range tests {
		c, err := reg.Negotiate(tc.accept)
		if err != nil {
			t.Fatalf("Negotiate(%q): %v", tc.accept, err)
		}
		if c.ContentType != tc.want {
			t.Errorf("Negotiate(%q) = %q, want %q", tc.accept, c.ContentType, tc.want)
		}
	}
}

func TestNegotiateWildcardPicksDeterministicDefault(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(JSONCodec(), GobCodec())
	for i := 0; i < 5; i++ {
		c, err := reg.Negotiate("*/*")
		if err != nil {
			t.Fatalf("Negotiate(*/*): %v", err)
		}
		if c.ContentType != "application/json" {
			t.Fatalf("Negotiate(*/*) = %q, want the first-registered codec application/json", c.ContentType)
		}
	}
}

func TestNegotiateReturnsErrorWhenNothingMatches(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(JSONCodec())
	if _, err := reg.Negotiate("application/xml, text/plain"); err == nil {
		t.Fatal("expected error when no Accept entry is supported")
	}
}
```

## Review

`Negotiate` walks the client's Accept list left to right and returns the
first entry the `Registry` actually has a `Codec` for — `TestNegotiatePicks
FirstSupportedEntry` pins that "first supported," not "first listed," wins.
The wildcard test is the one worth re-reading if `Negotiate` is ever
rewritten: it runs the same lookup five times specifically because a map-
only implementation would pass it *some* of the time (map iteration order
is randomized per process, not per call, so a flaky version might still look
stable within one `go test` run) and only a repeated, explicit assertion
across a real `Registry` construction reliably catches the bug. Keeping
`order` next to `byType` is what makes the wildcard path a plain slice
index instead of a race with `math/rand` hiding inside the runtime.

## Resources

- [encoding/json](https://pkg.go.dev/encoding/json)
- [encoding/gob](https://pkg.go.dev/encoding/gob)
- [RFC 9110: HTTP Semantics, Accept header field](https://www.rfc-editor.org/rfc/rfc9110#field.accept)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [20-feature-rule-evaluator-callback.md](20-feature-rule-evaluator-callback.md) | Next: [22-sql-query-filter-builder-callback.md](22-sql-query-filter-builder-callback.md)
