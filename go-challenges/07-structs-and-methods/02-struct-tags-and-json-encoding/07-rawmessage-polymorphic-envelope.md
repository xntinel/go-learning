# Exercise 7: Decode a Discriminated Event Envelope With json.RawMessage

A webhook receiver, a Kafka consumer, an SQS worker — all face the same problem: a
single stream carries messages of many shapes, and you cannot decode them into one
fixed struct. The standard answer is a discriminated envelope: read a `type` field
first, then decode the payload into the concrete struct that `type` selects. This
module builds that two-phase decoder using `json.RawMessage` to defer the payload
until the discriminator is known.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
eventbus/                      independent module: example.com/eventbus
  go.mod                       go 1.24
  events/
    envelope.go                Envelope{Type, Data RawMessage}; registry; Encode/Decode
  cmd/
    demo/
      main.go                  encode two event types, decode and route each
  events/envelope_test.go      routing, unknown type, deferred phase-2 failure, byte round-trip
```

Files: `events/envelope.go`, `cmd/demo/main.go`, `events/envelope_test.go`.
Implement: `Envelope{Type string; Data json.RawMessage}`, a `registry` of `type -> factory`, `Encode(type, payload)`, and `Decode(bytes) (type, any, error)` with a sentinel `ErrUnknownType`.
Test: two types route to the correct concrete struct; unknown type errors; a known type with malformed `Data` passes phase one but fails phase two; an envelope round-trips with `Data` preserved byte-for-byte.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/07-structs-and-methods/02-struct-tags-and-json-encoding/07-rawmessage-polymorphic-envelope/events go-solutions/07-structs-and-methods/02-struct-tags-and-json-encoding/07-rawmessage-polymorphic-envelope/cmd/demo
cd go-solutions/07-structs-and-methods/02-struct-tags-and-json-encoding/07-rawmessage-polymorphic-envelope
go mod edit -go=1.24
```

## Two phases, deferred by RawMessage

The envelope is `{"type":"...","data":{...}}`. The trick is that `Data` is declared
as `json.RawMessage`, which is a `[]byte` that `Unmarshal` fills with the payload's
*still-encoded* bytes instead of trying to parse it. So phase one — unmarshaling
the outer envelope — reads and validates the `type` discriminator and captures the
`data` subtree verbatim, without needing to know what shape `data` has. This is why
a message with a valid envelope but a malformed payload still parses in phase one:
`RawMessage` does not look inside.

Phase two happens only after `type` tells you which concrete struct to build. A
`registry` maps each type string to a factory function that returns a fresh pointer
to the right struct (`func() any { return &UserCreated{} }`). `Decode` looks up the
factory, calls it, and unmarshals the captured `Data` bytes into that concrete
value. An unrecognized `type` never reaches phase two — it returns `ErrUnknownType`
so the caller can dead-letter or reject the message.

Deferring with `RawMessage` (rather than decoding into `map[string]any` and
re-marshaling) has two payoffs. It preserves the payload bytes exactly, so
`Encode` of a re-wrapped envelope reproduces the original `data` byte-for-byte
(important for signature verification on webhooks). And it decodes the payload
directly into a typed struct with no intermediate `any`, so numbers keep their
declared types instead of degrading to `float64`.

Create `events/envelope.go`:

```go
// events/envelope.go
package events

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ErrUnknownType is returned for an envelope whose Type has no registered factory.
var ErrUnknownType = errors.New("unknown event type")

// Envelope is the discriminated wire form: a Type tag plus a deferred Data
// payload captured as raw bytes.
type Envelope struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// UserCreated and OrderPlaced are two concrete payload shapes.
type UserCreated struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

type OrderPlaced struct {
	OrderID     string `json:"order_id"`
	AmountCents int64  `json:"amount_cents"`
}

// registry maps a type discriminator to a factory for its concrete payload.
var registry = map[string]func() any{
	"user.created": func() any { return &UserCreated{} },
	"order.placed": func() any { return &OrderPlaced{} },
}

// Encode wraps a payload in an envelope with the given type.
func Encode(eventType string, payload any) ([]byte, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal payload: %w", err)
	}
	return json.Marshal(Envelope{Type: eventType, Data: data})
}

// Decode reads the discriminator (phase one), then decodes the deferred payload
// into the concrete type it selects (phase two).
func Decode(b []byte) (string, any, error) {
	var e Envelope
	if err := json.Unmarshal(b, &e); err != nil {
		return "", nil, fmt.Errorf("decode envelope: %w", err)
	}
	factory, ok := registry[e.Type]
	if !ok {
		return e.Type, nil, fmt.Errorf("%w: %q", ErrUnknownType, e.Type)
	}
	payload := factory()
	if err := json.Unmarshal(e.Data, payload); err != nil {
		return e.Type, nil, fmt.Errorf("decode %s payload: %w", e.Type, err)
	}
	return e.Type, payload, nil
}
```

## The runnable demo

The demo encodes one event of each type, decodes each back, and routes on the
concrete type with a type switch.

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/eventbus/events"
)

func route(b []byte) {
	typ, payload, err := events.Decode(b)
	if err != nil {
		fmt.Printf("error: %v\n", err)
		return
	}
	switch p := payload.(type) {
	case *events.UserCreated:
		fmt.Printf("%s -> user %s <%s>\n", typ, p.ID, p.Email)
	case *events.OrderPlaced:
		fmt.Printf("%s -> order %s for %d cents\n", typ, p.OrderID, p.AmountCents)
	}
}

func main() {
	uc, _ := events.Encode("user.created", events.UserCreated{ID: "u1", Email: "a@x.com"})
	route(uc)

	op, _ := events.Encode("order.placed", events.OrderPlaced{OrderID: "o9", AmountCents: 4200})
	route(op)

	route([]byte(`{"type":"widget.exploded","data":{}}`))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
user.created -> user u1 <a@x.com>
order.placed -> order o9 for 4200 cents
error: unknown event type: "widget.exploded"
```

## Tests

`TestRoutesEachType` decodes both event types and asserts each becomes the correct
concrete struct with the right fields. `TestUnknownTypeErrors` asserts an
unregistered type returns `ErrUnknownType`. `TestPhaseOneSucceedsPhaseTwoFails`
feeds a known type with a malformed `Data` (a number where a string is expected):
it asserts the envelope alone parses (phase one) but `Decode` fails (phase two).
`TestRoundTripPreservesData` asserts `Data` survives an encode/decode cycle
byte-for-byte.

Create `events/envelope_test.go`:

```go
// events/envelope_test.go
package events

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

func TestRoutesEachType(t *testing.T) {
	t.Parallel()
	uc, _ := Encode("user.created", UserCreated{ID: "u1", Email: "a@x.com"})
	typ, payload, err := Decode(uc)
	if err != nil {
		t.Fatal(err)
	}
	u, ok := payload.(*UserCreated)
	if typ != "user.created" || !ok || u.ID != "u1" || u.Email != "a@x.com" {
		t.Fatalf("bad user route: %s %#v", typ, payload)
	}

	op, _ := Encode("order.placed", OrderPlaced{OrderID: "o9", AmountCents: 4200})
	typ, payload, err = Decode(op)
	if err != nil {
		t.Fatal(err)
	}
	o, ok := payload.(*OrderPlaced)
	if typ != "order.placed" || !ok || o.OrderID != "o9" || o.AmountCents != 4200 {
		t.Fatalf("bad order route: %s %#v", typ, payload)
	}
}

func TestUnknownTypeErrors(t *testing.T) {
	t.Parallel()
	_, _, err := Decode([]byte(`{"type":"widget.exploded","data":{}}`))
	if !errors.Is(err, ErrUnknownType) {
		t.Fatalf("expected ErrUnknownType, got %v", err)
	}
}

func TestPhaseOneSucceedsPhaseTwoFails(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"type":"user.created","data":{"id":"u1","email":12345}}`)

	// Phase one: the envelope parses even though the payload is malformed,
	// because RawMessage defers decoding of Data.
	var e Envelope
	if err := json.Unmarshal(raw, &e); err != nil {
		t.Fatalf("phase one should succeed, got %v", err)
	}
	if e.Type != "user.created" || len(e.Data) == 0 {
		t.Fatalf("phase one did not capture envelope: %#v", e)
	}

	// Phase two: decoding the payload into the concrete type fails.
	if _, _, err := Decode(raw); err == nil {
		t.Fatal("phase two should fail on malformed payload")
	}
}

func TestRoundTripPreservesData(t *testing.T) {
	t.Parallel()
	original := json.RawMessage(`{"id":"u1","email":"a@x.com"}`)
	wire, err := json.Marshal(Envelope{Type: "user.created", Data: original})
	if err != nil {
		t.Fatal(err)
	}
	var back Envelope
	if err := json.Unmarshal(wire, &back); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, back.Data) {
		t.Fatalf("Data not preserved: %s vs %s", original, back.Data)
	}
}

func ExampleDecode() {
	uc, _ := Encode("user.created", UserCreated{ID: "u1", Email: "a@x.com"})
	typ, payload, _ := Decode(uc)
	u := payload.(*UserCreated)
	fmt.Printf("%s %s\n", typ, u.Email)
	// Output: user.created a@x.com
}
```

## Review

The decoder is correct when each registered type routes to its concrete struct,
an unregistered type is rejected with `ErrUnknownType`, and a syntactically valid
envelope wrapping a malformed payload fails only in phase two — the observable proof
that `RawMessage` deferred the payload rather than parsing it eagerly. This
two-phase shape is the backbone of every real message router; the registry makes
adding a new event type a one-line change, and preserving `Data` byte-for-byte is
what lets you verify a webhook signature over the exact payload the sender signed.

## Resources

- [`json.RawMessage`](https://pkg.go.dev/encoding/json#RawMessage) — deferring decoding to a second phase.
- [`encoding/json.Unmarshal`](https://pkg.go.dev/encoding/json#Unmarshal) — decoding into a concrete type chosen at runtime.
- [Go blog: JSON and Go](https://go.dev/blog/json) — decoding arbitrary and polymorphic JSON.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [06-secret-redaction-via-marshaljson.md](06-secret-redaction-via-marshaljson.md) | Next: [08-streaming-ndjson-ingest.md](08-streaming-ndjson-ingest.md)
