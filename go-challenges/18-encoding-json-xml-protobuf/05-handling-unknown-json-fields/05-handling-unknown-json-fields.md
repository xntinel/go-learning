# 5. Handling Unknown JSON Fields

Unknown JSON fields are a policy decision. Ignoring them gives forward compatibility; rejecting them protects strict contracts. This lesson builds an `eventjson` library that uses `json.RawMessage`, `Decoder.DisallowUnknownFields`, and wrapped sentinel errors to make that policy explicit.

## Concepts

### Default Decoding Is Permissive

`json.Unmarshal` ignores object keys that do not match exported struct fields. That behavior is useful when a client wants only part of a large response, but it is dangerous for request validation because misspelled fields silently disappear.

### RawMessage Defers the Hard Choice

`json.RawMessage` stores raw encoded JSON bytes. Decode a small envelope first, inspect its `type`, then decode `payload` into the matching struct. This keeps the discriminator logic typed without forcing every payload into `map[string]any`.

### Strict Decoding Requires a Decoder

`DisallowUnknownFields` is a method on `*json.Decoder`, not a package-level `Unmarshal` option. Wrap strict decode failures with a sentinel so callers can use `errors.Is(err, ErrStrictDecode)` while still seeing the underlying field name in the message.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/18-encoding-json-xml-protobuf/05-handling-unknown-json-fields/05-handling-unknown-json-fields/cmd/demo
cd go-solutions/18-encoding-json-xml-protobuf/05-handling-unknown-json-fields/05-handling-unknown-json-fields
go mod edit -go=1.26
```

### Exercise 1: Decode a Typed Envelope

Create `event.go`:

```go
package eventjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

var (
	ErrUnknownEventType = errors.New("unknown event type")
	ErrStrictDecode     = errors.New("strict payload decode")
)

type Envelope struct {
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp"`
	Payload   json.RawMessage `json:"payload"`
}

type Event struct {
	kind      string
	timestamp string
	summary   string
}

func (e Event) Kind() string      { return e.kind }
func (e Event) Timestamp() string { return e.timestamp }
func (e Event) Summary() string   { return e.summary }

type UserCreated struct {
	UserID string `json:"user_id"`
	Email  string `json:"email"`
}

type OrderPlaced struct {
	OrderID string   `json:"order_id"`
	Amount  float64  `json:"amount"`
	Items   []string `json:"items"`
}

type SystemAlert struct {
	Level   string `json:"level"`
	Message string `json:"message"`
}

func DecodeEvent(data []byte) (Event, error) {
	var env Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return Event{}, err
	}

	switch env.Type {
	case "user.created":
		var payload UserCreated
		if err := decodeStrict(env.Payload, &payload); err != nil {
			return Event{}, fmt.Errorf("%w: %v", ErrStrictDecode, err)
		}
		return Event{kind: env.Type, timestamp: env.Timestamp, summary: fmt.Sprintf("user %s created with email %s", payload.UserID, payload.Email)}, nil
	case "order.placed":
		var payload OrderPlaced
		if err := decodeStrict(env.Payload, &payload); err != nil {
			return Event{}, fmt.Errorf("%w: %v", ErrStrictDecode, err)
		}
		return Event{kind: env.Type, timestamp: env.Timestamp, summary: fmt.Sprintf("order %s placed for %.2f", payload.OrderID, payload.Amount)}, nil
	case "system.alert":
		var payload SystemAlert
		if err := decodeStrict(env.Payload, &payload); err != nil {
			return Event{}, fmt.Errorf("%w: %v", ErrStrictDecode, err)
		}
		return Event{kind: env.Type, timestamp: env.Timestamp, summary: fmt.Sprintf("%s alert: %s", payload.Level, payload.Message)}, nil
	default:
		return Event{}, fmt.Errorf("%w: %s", ErrUnknownEventType, env.Type)
	}
}

func decodeStrict(data []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}
```

### Exercise 2: Test Strict and Polymorphic Behavior

Create `event_test.go`:

```go
package eventjson

import (
	"errors"
	"fmt"
	"testing"
)

func TestDecodeEvent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    string
		wantErr error
	}{
		{name: "user created", input: `{"type":"user.created","timestamp":"2026-03-13T10:00:00Z","payload":{"user_id":"u1","email":"a@example.com"}}`, want: "user u1 created with email a@example.com"},
		{name: "order placed", input: `{"type":"order.placed","timestamp":"2026-03-13T10:05:00Z","payload":{"order_id":"o1","amount":59.99,"items":["widget"]}}`, want: "order o1 placed for 59.99"},
		{name: "system alert", input: `{"type":"system.alert","timestamp":"2026-03-13T10:10:00Z","payload":{"level":"warning","message":"high memory"}}`, want: "warning alert: high memory"},
		{name: "unknown event", input: `{"type":"unknown.event","timestamp":"2026-03-13T10:15:00Z","payload":{"foo":"bar"}}`, wantErr: ErrUnknownEventType},
		{name: "unknown payload field", input: `{"type":"user.created","timestamp":"2026-03-13T10:00:00Z","payload":{"user_id":"u1","email":"a@example.com","role":"admin"}}`, wantErr: ErrStrictDecode},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := DecodeEvent([]byte(tt.input))
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("DecodeEvent() error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("DecodeEvent() error = %v", err)
			}
			if got.Summary() != tt.want {
				t.Fatalf("Summary() = %q, want %q", got.Summary(), tt.want)
			}
		})
	}
}

func ExampleDecodeEvent() {
	event, _ := DecodeEvent([]byte(`{"type":"user.created","timestamp":"2026-03-13T10:00:00Z","payload":{"user_id":"u1","email":"a@example.com"}}`))
	fmt.Println(event.Kind() + ": " + event.Summary())
	// Output:
	// user.created: user u1 created with email a@example.com
}
```

Your turn: add a strict test for an `order.placed` payload with an unexpected `coupon` field.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/eventjson"
)

func main() {
	data := []byte(`{"type":"order.placed","timestamp":"2026-03-13T10:05:00Z","payload":{"order_id":"o1","amount":59.99,"items":["widget","gadget"]}}`)
	event, err := eventjson.DecodeEvent(data)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%s at %s: %s\n", event.Kind(), event.Timestamp(), event.Summary())
}
```

## Common Mistakes

- Wrong: using `json.Unmarshal` for strict request validation. What happens: misspelled fields are ignored. Fix: use `json.Decoder` with `DisallowUnknownFields`.
- Wrong: decoding every payload into `map[string]any`. What happens: validation moves into fragile type assertions. Fix: use `json.RawMessage` and typed payload structs.
- Wrong: returning only `fmt.Errorf("bad payload")`. What happens: callers cannot classify the error. Fix: wrap a sentinel with `%w`.

## Verification

From `~/go-exercises/eventjson`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

All commands must pass. Add at least one test of your own before considering the lesson complete.

## Summary

- Unknown JSON fields are ignored by default.
- `json.RawMessage` supports two-pass decoding for polymorphic payloads.
- `Decoder.DisallowUnknownFields` turns strictness into an explicit policy.
- Wrapped sentinels preserve both machine-checkable error classes and human-readable causes.

## What's Next

Next: [JSON Patch and Merge](../06-json-patch-merge/06-json-patch-merge.md).

## Resources

- [encoding/json package documentation](https://pkg.go.dev/encoding/json)
- [json.RawMessage documentation](https://pkg.go.dev/encoding/json#RawMessage)
- [Decoder.DisallowUnknownFields documentation](https://pkg.go.dev/encoding/json#Decoder.DisallowUnknownFields)
- [Go blog: JSON and Go](https://go.dev/blog/json)
