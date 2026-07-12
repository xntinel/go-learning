# Exercise 21: OpenTelemetry Span Attribute Enricher

**Nivel: Intermedio** — validacion rapida (un test corto).

A traced request accumulates span attributes over its lifetime: the HTTP
method is known at the start, the status code only after the response is
written, a retry count only if a retry happened at all. Rather than threading
a mutable attribute map through every layer, each layer calls one function —
`Enrich(base, extra...)` — that merges its own attributes into whatever the
caller built so far, overwriting any attribute that already exists under the
same key.

## What you'll build

```text
spanattrs/                  independent module: example.com/spanattrs
  go.mod                    go 1.24
  spanattrs.go              package spanattrs; type Attr; String/Int/Bool; Enrich(base []Attr, extra ...Attr) []Attr
  cmd/
    demo/
      main.go               runnable demo: base attrs enriched with a status code and a retry count
  spanattrs_test.go         table tests: append new keys in order, overwrite in place, zero extras, no mutation of base
```

- Files: `spanattrs.go`, `cmd/demo/main.go`, `spanattrs_test.go`.
- Implement: `type Attr struct{ Key string; Value any }`, constructors `String`, `Int`, `Bool`, and `Enrich(base []Attr, extra ...Attr) []Attr` that overwrites same-key attributes in place and appends new ones in argument order.
- Test: a new key from `extra` is appended after `base` in the order given; an `extra` key matching a `base` key overwrites the value but keeps its original position; zero extras returns a slice equivalent to `base`; `Enrich` never mutates `base`.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/03-variadic-functions/21-span-attribute-enricher-otel/cmd/demo
cd go-solutions/04-functions/03-variadic-functions/21-span-attribute-enricher-otel
go mod edit -go=1.24
```

### Why overwrite-in-place instead of append-and-dedup-later

The signature `Enrich(base []Attr, extra ...Attr) []Attr` puts the required,
already-known attributes as a plain parameter and the variable, later-known
ones as the variadic tail — the same "required first, optional/repeated
last" split as the query encoder in exercise 19. What makes this one
different is the merge semantics: a real span's attributes are keyed, so
`Enrich` builds an `index map[string]int` from `base`'s keys to their
position, and for each `extra` attribute either overwrites `out[index[key]]`
in place (same key) or appends a new entry and records its position (new
key). The alternative — just appending everything and deduplicating with a
`for i := len(out)-1; i >= 0; i--` scan afterward, keeping the *last* match —
would work but costs an extra full pass and an easy-to-get-wrong "which
occurrence wins" decision; doing the overwrite as you go means there is only
ever one attribute per key in `out` at any point, no cleanup pass needed.

`Enrich` copies `base` into a freshly allocated slice before touching
anything (`out := make([]Attr, len(base), len(base)+len(extra)); copy(out,
base)`), so passing the same `base` slice into several independent
`Enrich` calls from different code paths never lets one caller's extras leak
into another's. This is the same aliasing discipline as the cache-key
builder in exercise 1, applied to a slice of structs instead of strings.

Create `spanattrs.go`:

```go
// spanattrs.go
package spanattrs

// Attr is a single OpenTelemetry-style span attribute: a key and a typed
// value. This package models the shape independently of any tracing SDK so
// the exercise stays dependency-free.
type Attr struct {
	Key   string
	Value any
}

// String returns a string-valued Attr.
func String(key, value string) Attr { return Attr{Key: key, Value: value} }

// Int returns an int-valued Attr.
func Int(key string, value int) Attr { return Attr{Key: key, Value: value} }

// Bool returns a bool-valued Attr.
func Bool(key string, value bool) Attr { return Attr{Key: key, Value: value} }

// Enrich merges base span attributes with any number of extra attributes
// collected later in a request's lifecycle (a retry count, a final status
// code, and so on). An extra attribute sharing a key with one already in
// base overwrites its value in place, preserving the position of the first
// occurrence; a new key is appended in the order given. Enrich never
// mutates base — it always returns a fresh slice.
func Enrich(base []Attr, extra ...Attr) []Attr {
	out := make([]Attr, len(base), len(base)+len(extra))
	copy(out, base)

	index := make(map[string]int, len(out))
	for i, a := range out {
		index[a.Key] = i
	}

	for _, a := range extra {
		if i, ok := index[a.Key]; ok {
			out[i] = a
			continue
		}
		index[a.Key] = len(out)
		out = append(out, a)
	}
	return out
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/spanattrs"
)

func main() {
	base := []spanattrs.Attr{
		spanattrs.String("http.method", "GET"),
		spanattrs.Int("http.status_code", 0),
	}

	enriched := spanattrs.Enrich(base,
		spanattrs.Int("http.status_code", 200),
		spanattrs.Int("retry.count", 2),
	)

	for _, a := range enriched {
		fmt.Printf("%s=%v\n", a.Key, a.Value)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
http.method=GET
http.status_code=200
retry.count=2
```

### Tests

`TestEnrichOverwritesInPlacePreservingPosition` is the one that captures the
subtle contract: `http.status_code` starts at position 1 in `base` with
value `0`, and after enriching with the real status code it must still be at
position 1 — not moved to the end — with the new value.

Create `spanattrs_test.go`:

```go
// spanattrs_test.go
package spanattrs

import (
	"reflect"
	"testing"
)

func TestEnrichAppendsNewKeysInOrder(t *testing.T) {
	t.Parallel()

	base := []Attr{String("http.method", "GET")}
	got := Enrich(base, Int("retry.count", 1), Bool("cache.hit", false))

	want := []Attr{
		String("http.method", "GET"),
		Int("retry.count", 1),
		Bool("cache.hit", false),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Enrich = %+v, want %+v", got, want)
	}
}

func TestEnrichOverwritesInPlacePreservingPosition(t *testing.T) {
	t.Parallel()

	base := []Attr{
		String("http.method", "GET"),
		Int("http.status_code", 0),
	}
	got := Enrich(base, Int("http.status_code", 200))

	want := []Attr{
		String("http.method", "GET"),
		Int("http.status_code", 200),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Enrich = %+v, want %+v", got, want)
	}
}

func TestEnrichWithNoExtrasReturnsAnEquivalentCopy(t *testing.T) {
	t.Parallel()

	base := []Attr{String("http.method", "GET")}
	got := Enrich(base)

	if !reflect.DeepEqual(got, base) {
		t.Fatalf("Enrich(base) = %+v, want %+v", got, base)
	}
}

func TestEnrichDoesNotMutateBase(t *testing.T) {
	t.Parallel()

	base := []Attr{
		String("http.method", "GET"),
		Int("http.status_code", 0),
	}
	original := make([]Attr, len(base))
	copy(original, base)

	_ = Enrich(base, Int("http.status_code", 200), String("retry.count", "2"))

	if !reflect.DeepEqual(base, original) {
		t.Fatalf("Enrich mutated base: got %+v, want %+v", base, original)
	}
}
```

## Review

`Enrich` is correct when every attribute key appears exactly once in the
result, a new key is appended in the order it was given, an existing key's
value is replaced without disturbing its position, and `base` itself is
never modified regardless of how many times `Enrich` is called with it. The
senior point is that "merge with overwrite" is a keyed operation, not a
positional one — reaching for a `map[string]int` index instead of nested
loops or post-hoc deduplication keeps the merge at O(len(base)+len(extra))
instead of O(len(base) × len(extra)). The mistake to avoid is appending
`extra` onto `base`'s own backing array in place (e.g. `append(base,
extra...)` directly) — if `base` has spare capacity, that silently
overwrites memory a different goroutine or a different span might still be
reading.

## Resources

- [OpenTelemetry: Attribute and Limits](https://opentelemetry.io/docs/specs/otel/common/#attribute) — the key/value attribute model this exercise mirrors.
- [Go Spec: Passing arguments to `...` parameters](https://go.dev/ref/spec#Passing_arguments_to_..._parameters)
- [`maps` package](https://pkg.go.dev/maps) — for building keyed indexes like the one `Enrich` uses.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [20-where-predicate-combinator-and-or.md](20-where-predicate-combinator-and-or.md) | Next: [22-dns-resolver-fallback-chain.md](22-dns-resolver-fallback-chain.md)
