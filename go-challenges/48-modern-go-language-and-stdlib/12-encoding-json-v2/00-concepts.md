# Streaming JSON with encoding/json/v2 and jsontext — Concepts

`encoding/json` v1 has served Go for over a decade, and every senior backend team
eventually hits its walls at the same three places: it decodes a whole document
into memory before you can reject it (a multi-gigabyte ingest or an unbounded
event stream becomes an out-of-memory lever), it silently tolerates payloads that
a strict reader would refuse (duplicate object members, invalid UTF-8,
case-insensitive field matching), and its `MarshalJSON`/`UnmarshalJSON` interfaces
hand you a `[]byte`, which forces buffering and rules out true streaming. Go's
answer is a two-package redesign shipped behind an experiment: `encoding/json/v2`
for the Go-to-JSON *semantics*, sitting on top of `encoding/json/jsontext` for the
JSON *syntax*. This file is the conceptual foundation for the three independent
exercises that follow; read it once and you have the model you need to reason
through streaming ingest, token-layer rewriting, and a deliberate v1 compatibility
shim.

Everything here is gated behind `GOEXPERIMENT=jsonv2` and the
`//go:build goexperiment.jsonv2` build constraint. That is not incidental — it is
part of the skill. The job is to evaluate and prototype a migration under the
experiment, gate the code behind the constraint, and test your existing v1
behavior under v2 to catch drift *before* it ships.

## Concepts

### Two layers, one engine

The split is the key mental model. `jsontext` understands JSON *syntax* and
nothing about Go: it reads and writes JSON as a stream of tokens (`{`, `}`, `[`,
`]`, a string, a number, `true`, `false`, `null`) or as raw `jsontext.Value`
byte slices. By default it enforces RFC 7493 ("I-JSON"): valid UTF-8, no
duplicate object member names. `json/v2` understands *semantics*: it maps Go
values to and from JSON, resolving struct tags, custom marshalers, and options —
and it delegates every byte of I/O to a `jsontext.Encoder` or `jsontext.Decoder`.

This separation is why streaming, exact error offsets, and token rewriting are
possible in v2 and were not in v1. Because the semantic layer drives a syntactic
tokenizer rather than a monolithic reflect-and-buffer routine, you can drop down
to the token layer whenever you want to transform bytes without ever building Go
values, and you can drive the semantic layer one value at a time.

### Six entry points on three axes

`json/v2` exposes marshal and unmarshal along three I/O axes:

```
Marshal(in any, ...Options) ([]byte, error)          Unmarshal([]byte, any, ...Options) error         // in-memory
MarshalWrite(io.Writer, any, ...Options) error       UnmarshalRead(io.Reader, any, ...Options) error  // whole stream
MarshalEncode(*jsontext.Encoder, any, ...Options)    UnmarshalDecode(*jsontext.Decoder, any, ...Options) // one value
```

`MarshalWrite`/`UnmarshalRead` still consume the entire reader or produce the
entire value; they just avoid the intermediate `[]byte`. The true streaming
primitive is `UnmarshalDecode`: it decodes *exactly one* JSON value per call from
a shared `jsontext.Decoder` rather than draining to `io.EOF`. That single property
is what makes bounded-memory ingest of a huge array or an endless event stream
possible: you loop, decoding one element into a reused destination, and you never
hold more than one element in memory. `MarshalEncode` is its mirror for writing a
value into a shared encoder.

### Changed defaults are the whole point — and the whole risk

v2 tightens four defaults that were v1 footguns:

- Duplicate object member names are rejected. In v1, `{"role":"user","role":"admin"}`
  silently kept the last value. When your Go service and an upstream proxy
  disagree about which duplicate wins, that is a parser-differential — a
  request-smuggling and authorization-bypass vector.
- Invalid UTF-8 is rejected instead of being silently replaced, which stops a
  class of data-corruption and smuggling bugs.
- Field names match case-*sensitively*. In v1, `{"Password":"..."}` would bind to
  a field tagged `json:"password"` that you never meant to expose to that casing.
- A nil slice marshals as `[]` and a nil map as `{}`, not `null`.

Each change closes a real hole, and each can break an integration that depended on
the old leniency. The senior discipline is: know every changed default, and know
the one option that reverts it (see below), so you can re-enable leniency
*deliberately at a trust boundary* rather than discovering it by accident in
production.

### omitzero versus omitempty

v2 adds the `omitzero` struct-tag option. `omitzero` omits a field when it equals
its type's zero value (or when the field's type has an `IsZero() bool` method that
returns true). This is precise: `Port int json:"port,omitzero"` omits `port` only
when it is exactly `0`. v1's `omitempty` used a fuzzy notion of "empty" that also
dropped `false`, `0`, and present-but-empty collections, which produced a
long-standing class of "why did my zero value disappear from the wire" bugs.
`omitempty` still exists in v2 with refined semantics, but for config and API
structs `omitzero` is almost always what you actually want.

### Options are values, not global state

Every knob is an `Options` value produced by a constructor function, and
`json.JoinOptions(...)` composes them (later options win over earlier ones). There
is no global mode switch. This is what lets you keep two named bundles side by
side — a strict internal bundle and a lenient edge bundle — instead of sprinkling
booleans through call sites:

```
strict := json.JoinOptions(json.RejectUnknownMembers(true))
compat := json.JoinOptions(
    json.MatchCaseInsensitiveNames(true),
    json.FormatNilSliceAsNull(true),
    jsontext.AllowDuplicateNames(true),
)
```

Options set on a `jsontext.Decoder`/`Encoder` at construction are inherited by any
`UnmarshalDecode`/`MarshalEncode` that runs against it, so you configure the
leniency of a boundary once, where the decoder is created.

### Streaming custom marshalers

v1's `MarshalJSON() ([]byte, error)` forces a type to allocate and return a
buffer, which defeats streaming. v2 adds `MarshalerTo` and `UnmarshalerFrom`:

```
MarshalJSONTo(*jsontext.Encoder) error
UnmarshalJSONFrom(*jsontext.Decoder) error
```

A type that implements these writes or reads directly against the shared token
stream — no intermediate `[]byte`. For types you do not own, the caller-side
`json.MarshalFunc`/`MarshalToFunc`/`UnmarshalFunc`/`UnmarshalFromFunc`, registered
through `json.WithMarshalers`/`WithUnmarshalers`, attach behavior for a Go type
without modifying it. `MarshalToFunc[T](func(*jsontext.Encoder, T) error)` is the
streaming form.

### The format struct tag

The `format` tag is new and load-bearing. It moves representation decisions out of
hand-written marshaler methods and into declarative tags: `json:",format:RFC3339"`
or `format:unix` for `time.Time`, `format:base64`/`format:hex` for `[]byte`, and
duration formats. It is not available in v1. Declaring the wire format on the
field is clearer and less error-prone than a bespoke `MarshalJSON` per type.

### An error model with a location

Syntax problems surface as `*jsontext.SyntacticError`; semantic problems as
`*json.SemanticError`. Both carry a `ByteOffset` (int64) and a `JSONPointer`
(`jsontext.Pointer`, an RFC 6901 pointer) to the exact failing location, and both
`Unwrap` to a cause you can match with `errors.Is`. For a pipeline service this
turns "invalid JSON somewhere in a 40 MB body" into an actionable, loggable
coordinate: `at byte 1048576, pointer /events/8123/amount`. Because a streaming
decoder tracks the full stack, the pointer is relative to the whole document even
when you decode one element at a time.

### Streaming is the availability story

Reading a request body with `Unmarshal(io.ReadAll(body), ...)`, or even
`UnmarshalRead(body, ...)`, buys the entire body into memory before you can
validate it. A large or hostile payload then becomes an out-of-memory lever with
no early rejection. `jsontext.NewDecoder(body)` plus a `UnmarshalDecode` loop lets
you validate framing, enforce an element-count or size budget, and reject early —
with the byte offset already in hand for the log line. That is the difference
between "a big body is slow" and "a big body is an availability incident".

### Experimental status is a first-class concern

The packages exist only under `GOEXPERIMENT=jsonv2` (a compile-time setting), you
gate code that imports them with `//go:build goexperiment.jsonv2`, and they are
exempt from the Go 1 compatibility promise: the API can still change. Critically,
v1 is being re-implemented on top of v2, so the recommended migration technique is
to build and test your *existing* services under the experiment and watch for
behavior drift — a test that passed under v1 and now fails under v2 is exactly the
duplicate-key or case-sensitivity change surfacing before a customer finds it.

## Common Mistakes

### Concluding the package "does not exist"

Building or running the examples without `GOEXPERIMENT=jsonv2` (or without the
`//go:build goexperiment.jsonv2` tag on the files) makes the import fail and the
files vanish from the build. The packages are compile-time gated; set the
experiment before you decide they are missing.

### Assuming v2 is a drop-in replacement for v1

The duplicate-key, invalid-UTF-8, and case-sensitivity changes mean payloads that
v1 accepted now error. Migrating blindly breaks lenient integrations. Migrate by
running under the experiment, observing which inputs now fail, and opting back in
to leniency with an explicit option *at the specific boundary* that needs it.

### Buffering the whole stream and defeating the memory win

Calling `Unmarshal` on a whole large array and then ranging over it, or calling
`io.ReadAll` first, throws away the reason to use v2 for ingest. The streaming
loop is `UnmarshalDecode` against a shared `jsontext.Decoder`, one value per call,
into a reused destination.

### Re-enabling leniency globally to make tests pass

Reaching for `MatchCaseInsensitiveNames` or `AllowDuplicateNames` across the whole
program to quiet a failing test silently reintroduces the exact vulnerabilities v2
fixed. Scope leniency to the one external boundary that requires it and keep the
internal path strict.

### Confusing the token layer with the value layer

`ReadToken` returns one token; `ReadValue` returns one complete value (a scalar,
or an entire object/array subtree). Calling `ReadValue` when you meant to walk
token by token skips the structure you wanted to inspect; calling `ReadToken`
where you meant to grab a whole subtree leaves you hand-balancing braces. Remember
that object member *names* are themselves string tokens, so a naive "replace every
string token at this pointer" also rewrites keys.

### Expecting omitzero to behave like omitempty

They have different rules. `omitzero` drops a field only when it equals the zero
value; `omitempty` uses a fuzzier "empty" test. Picking the wrong one silently
changes the wire output — continuing to use `omitempty` to drop zero scalars is a
common source of surprise.

### Expecting streaming from a []byte-returning marshaler

A custom `MarshalJSON() ([]byte, error)` still allocates an intermediate buffer.
For true streaming you must implement `MarshalerTo`/`UnmarshalerFrom` on the type,
or register a `MarshalToFunc`/`UnmarshalFromFunc` for it.

### Ignoring the nil-slice and nil-map default change

v2 emits `[]` and `{}` where v1 emitted `null`. A consumer that branches on
`field === null` breaks. If the wire contract genuinely requires `null`, revert
deliberately with `FormatNilSliceAsNull`/`FormatNilMapAsNull` rather than being
surprised.

### Passing options to the wrong layer

`jsontext.WithIndent` affects encoding at the syntactic layer, not what
`json.Marshal` decides to emit semantically; `json.RejectUnknownMembers` is
meaningless on a pure `jsontext` token copy that never builds a Go value. Apply
each option at the layer that owns the operation.

### Treating a jsontext.Value as a parsed structure

`jsontext.Value` is a `[]byte` of raw, possibly un-normalized JSON — not a tree.
If you need a canonical form, call its `Canonicalize`, `Compact`, or `Indent`
method, or decode it; do not compare two `Value`s byte-for-byte and expect
semantic equality.

Next: [01-streaming-decode-large-arrays.md](01-streaming-decode-large-arrays.md)
