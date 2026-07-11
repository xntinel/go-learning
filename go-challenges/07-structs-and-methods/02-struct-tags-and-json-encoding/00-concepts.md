# Struct Tags and JSON Encoding for Production APIs — Concepts

Struct tags are the contract boundary between a Go service's internal types and
the wire format that clients, other services, and datastores depend on. At the
level of "add a `json` tag" they look trivial. At senior level the interesting
problems are the failure modes that leak into production: a request handler that
silently accepts fields the API never declared (mass assignment, spec drift); a
`PATCH` that cannot tell "field absent" from "field set to null" from "field set
to its zero value", so it clobbers stored data; a secret that ships in an API
response because the field was exported and nobody tagged it; a large integer ID
corrupted into a `float64` because it was decoded into `any`; an audit timestamp
that never disappears from the payload because a zero `time.Time` is not
"empty"; and a handler that `Unmarshal`s a whole request body into memory when it
should be streaming. This file is the conceptual foundation. Read it once and you
have everything the ten independent exercises need.

## The model

### A struct tag is an opaque string that consumers interpret

A struct tag is a string literal that follows a field declaration. The compiler
stores it verbatim and never interprets it; it is `reflect.StructTag`, a
conventionally space-separated list of `key:"value"` pairs:

```go
type Response struct {
	ID    string `json:"id"`
	Email string `json:"email,omitempty" validate:"email"`
	Token string `json:"-"`
}
```

Consumers — `encoding/json`, database mappers, validators — read the tag at
runtime through `reflect.StructTag.Get(key)` or `reflect.StructTag.Lookup(key)`.
`Lookup` returns `(value, ok)` and is strictly more informative than `Get`: it
distinguishes "the `json` key is absent" from "the `json` key is present but
empty" (`json:""`), a distinction that matters when a library treats an explicit
empty tag differently from no tag at all. The grammar is fixed: keys are
space-separated, each value is a double-quoted Go string, and the value's own
internal grammar (a name followed by comma-separated options such as
`json:"email,omitempty"`) is defined by the consuming package, not by the
language. That is why hand-splitting tags with ad-hoc `strings.Split` is a bug
waiting to happen: use `reflect.StructTag` to get the well-defined outer parse,
then split the one value you own.

### Field visibility is a hard rule, not a tag option

`encoding/json` only sees exported (capitalized) fields. An unexported field is
invisible to marshaling regardless of any tag it carries; an exported field is
serialized by default. This has a direct security consequence: redacting a secret
is a deliberate design decision, never something that happens by accident. If a
field is exported and untagged, it is on the wire. You suppress it either with
`json:"-"` (the field disappears entirely) or with a custom `MarshalJSON` that
emits a placeholder like `"[REDACTED]"` (the field stays, but the value is
scrubbed). "I forgot to tag the password field" is a production incident, not a
style nit.

### omitempty and omitzero are defined in different universes

`omitempty` is defined in *JSON* terms: it omits a field whose value is `false`,
`0`, `""`, `nil`, or an empty slice, map, or array. It knows nothing about the Go
type's own notion of "unset". `omitzero` (added in Go 1.24) is defined in *Go*
terms: it omits a field whose value is the zero value for its type, and if the
type has an `IsZero() bool` method, it calls that. The two diverge exactly where
it hurts:

- A zero `time.Time` is **not** empty in JSON terms, so `omitempty` keeps it and
  you get `"completed_at":"0001-01-01T00:00:00Z"` on the wire — an audit
  timestamp that never disappears. `omitzero` calls `time.Time.IsZero()`, sees
  true, and drops the field.
- A non-nil but empty slice is empty in JSON terms (`omitempty` drops it) but is
  not the zero value in Go terms (a non-nil slice header), so `omitzero` keeps
  it as `[]`.

You can specify both (`json:"x,omitempty,omitzero"`); the field is omitted if
either rule applies. For an optional `time.Time` or an optional nested struct,
reach for `omitzero` or a pointer, never bare `omitempty`.

### Optionality on the wire is a three-state problem

"Optional" is not one state, it is three: the field can be **absent**, present as
**null**, or present with a **zero value**. On a `PATCH` these are three distinct
intents — leave unchanged, clear, set to zero. A value field (`string`, `bool`,
`int`) collapses all three into one indistinguishable zero value, so a handler
that uses value fields with `omitempty` cannot tell "the client did not mention
this field" from "the client wants it set to false", and will silently clobber
stored data. Preserving the distinction requires a field that has more than one
"empty" representation: a pointer (`*string` is `nil` when absent, non-nil
pointing at `""` when explicitly empty) or a `json.RawMessage` (which is `nil`
when absent, `[]byte("null")` when explicitly null, and the literal bytes
otherwise). Tri-state fields are mandatory for correct `PATCH`/merge semantics.

### The default decoder is lenient by design

`json.Unmarshal` and a default `json.Decoder` are permissive: unknown fields in
the input are silently ignored, key matching is case-insensitive
(`{"ID":...}` fills a field tagged `json:"id"`), and when a key appears more than
once the last occurrence wins. Leniency is convenient for forward compatibility
but dangerous on an inbound request: it is exactly the door through which mass
assignment and undetected client/spec drift walk in. `Decoder.DisallowUnknownFields`
turns an unknown key into an error, and checking that a second `Decode` returns
`io.EOF` rejects trailing garbage or a smuggled second object. Case-insensitive
matching and last-key-wins also mean a proxy and the app can disagree about which
value "counts", which is a request-smuggling surface to be aware of.

### A type can own its wire representation

Implement `json.Marshaler` (`MarshalJSON() ([]byte, error)`) and
`json.Unmarshaler` (`UnmarshalJSON([]byte) error`) and the type controls exactly
how it appears in JSON. This is how you keep money in integer minor units instead
of a lossy `float64`, transport an ID as a string, or scrub a secret. Implement
`encoding.TextMarshaler`/`TextUnmarshaler` in addition and the type becomes usable
as a JSON **map key** (JSON object keys must be strings) and reusable across other
encoders (`encoding/xml`, database drivers). Two traps live here. First, receiver
mismatch: a `MarshalJSON` on a pointer receiver is not in the method set of a
non-addressable value, so marshaling the value skips your method and falls back to
the default — define the method on the receiver that matches how the type is used,
and be consistent. Second, infinite recursion: calling `json.Marshal(v)` on the
same type *inside* its own `MarshalJSON` re-enters the method forever; break the
cycle by marshaling a local alias type (`type alias T`) that does not carry the
method set.

### Numbers decoded into `any` are float64

When JSON is decoded into `any` / `map[string]any`, every number becomes a
`float64`. `float64` has 52 bits of mantissa, so it represents integers exactly
only up to 2^53. A 64-bit ID like `9007199254740993` decoded into `any` comes
back as `9007199254740992` — silently off by one, with no error. `Decoder.UseNumber`
changes this: numbers decode into `json.Number`, which is a string, and its
`Int64()` / `Float64()` methods convert on demand and report overflow as an error.
The `,string` tag is the complementary transport trick: it encodes a numeric field
as a JSON **string** (`"id":"9007199254740993"`) so it survives JavaScript and
other consumers whose native number type is a float.

### Unmarshal buffers; Decoder streams

`json.Unmarshal` reads its entire input into memory before parsing. For a large
body, an NDJSON feed, or a JSON array of unknown length, that is unbounded memory.
`json.Decoder` streams: call `Decode` in a loop until it returns `io.EOF` to
process one record at a time, and for a single top-level JSON array read the
opening `[` with `Token`, loop while `More()` reports another element, then read
the closing `]`. Pair it with `json.Encoder`, whose `Encode` writes one value plus
a newline, to emit results incrementally. Memory stays bounded by the size of a
single record rather than the whole stream.

### RawMessage defers decoding for polymorphic payloads

`json.RawMessage` is a `[]byte` that `Unmarshal` fills with the raw, still-encoded
bytes of a subtree and `Marshal` writes back verbatim. It lets you decode in two
phases: read a discriminator field now, then decode the deferred `Data` into the
concrete type chosen by the discriminator later. This is the canonical pattern for
webhooks, message-bus envelopes (Kafka/SQS/NATS), and discriminated-union APIs
where a single fixed struct cannot describe every payload.

### Output bytes are not a stable contract

`Encoder.SetEscapeHTML(false)`, `Encoder.SetIndent`, and `MarshalIndent` control
the exact output bytes, but the point is the inverse: you must never assert JSON
equality by comparing raw strings. Go does emit object keys in struct-field order,
but that is an implementation detail across versions and does not hold for a
`map`, and HTML escaping (`<`, `>`, `&`) is on by default. Assert on parsed
structure, or on the presence of specific `"key":value` substrings, never on a
whole-document `==`.

## Common Mistakes

### Assuming omitempty drops a zero time.Time or nested struct

Wrong: tagging an optional `CompletedAt time.Time` with `omitempty` and expecting
it to vanish when unset. It does not — `omitempty` is a JSON-emptiness test and a
zero `time.Time` is not JSON-empty, so you ship `"0001-01-01T00:00:00Z"`.

Fix: use `omitzero` (Go 1.24), which calls `IsZero()`, or make the field a
`*time.Time` and leave it `nil`.

### A value field with omitempty on a PATCH

Wrong: `Active bool json:"active,omitempty"` in a patch struct. Now "field absent"
and "set to false" both decode to `false`, and applying the patch cannot tell them
apart — it silently clobbers a stored `true`.

Fix: use `*bool` or `json.RawMessage` so absent, null, and false are three
distinguishable states.

### Leaving a secret field exported and untagged

Wrong: an exported `Password`/`APIKey` field with no tag. It serializes straight
into API responses and structured logs.

Fix: `json:"-"` to drop it, or a redacting `MarshalJSON` that emits a placeholder
so the field's presence is visible but the value never is.

### Trusting the default lenient decoder on inbound requests

Wrong: `json.NewDecoder(r.Body).Decode(&req)` with defaults, which ignores unknown
fields and accepts trailing garbage — the mass-assignment door.

Fix: `dec.DisallowUnknownFields()` and a second `Decode` that must return
`io.EOF`.

### Decoding numbers into any and trusting the result

Wrong: `json.Unmarshal(b, &m)` into `map[string]any` and reading an ID as a
number. Any integer above 2^53 is corrupted by `float64` with no error.

Fix: `Decoder.UseNumber()` and `json.Number.Int64()`, or decode into a typed
struct with the right integer type, or transport the ID with `,string`.

### Comparing marshaled JSON with ==

Wrong: `if string(data) == `{"a":1,"b":2}``. HTML escaping and map key order make
this brittle across inputs and versions.

Fix: parse and compare structurally, or assert on specific `"key":value`
substrings.

### Unmarshaling an entire large or streaming body

Wrong: reading a multi-megabyte or open-ended NDJSON feed with a single
`json.Unmarshal`, buffering all of it.

Fix: stream with `json.Decoder.Decode` in a loop to `io.EOF`, bounding memory to
one record.

### MarshalJSON receiver mismatch or infinite recursion

Wrong: a `MarshalJSON` on `*T` while the value is marshaled as `T` (method never
called), or `json.Marshal(t)` inside `T.MarshalJSON` (infinite recursion).

Fix: match the receiver to how the value is used, and inside a custom marshaler
convert to a local alias type (`type alias T`) that lacks the method before
delegating to `json.Marshal`.

### Hand-parsing struct tags

Wrong: splitting the raw tag string with ad-hoc `strings.Split` and missing the
absent-vs-empty distinction or the option list after the comma.

Fix: `reflect.StructTag.Lookup`/`Get` for the outer `key:"value"` parse, then
`strings.Cut` on the one value you own to separate name from options.

### Forgetting case-insensitive and last-key-wins matching

Wrong: assuming `{"ID":...}` will not fill a `json:"id"` field, or that a
duplicate key errors. It does fill it, and duplicates keep the last value.

Fix: know the rules; validate explicitly and reject unknown/duplicate structure
where a security boundary depends on it.

Next: [01-api-response-tags-and-omitempty.md](01-api-response-tags-and-omitempty.md)
