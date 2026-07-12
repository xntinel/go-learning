# Exercise 10: Dispatch a Queue Payload by Its Decoded Shape

**Nivel: Intermedio** — validacion rapida (un test corto).

A queue consumer receives message bodies from producers on different versions
that disagree on the wire format: some send a bare JSON object, some a batch
array, some a plain string reference key, some a legacy numeric id. The
consumer must classify a decoded `any` payload by its shape alone, not by an
explicit envelope field, and route it to single, batch, or reference handling.

## What you'll build

```text
qpayload/                   independent module: example.com/qpayload
  go.mod                    go 1.24
  qpayload.go               Dispatch(payload any) (Result, error)
  qpayload_test.go          one table test over every decoded shape
```

- Implement: `Dispatch(payload any) (Result, error)`, classifying `nil`,
  `map[string]any`, `[]any`, `string`, and `json.Number` into a `Result{Kind,
  Count}`.
- Test: one table covering each shape plus two rejected cases — an empty
  reference string and a non-numeric legacy id.

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/04-type-switch/10-message-queue-payload-dispatcher
cd go-solutions/03-control-flow/04-type-switch/10-message-queue-payload-dispatcher
go mod edit -go=1.24
```

Create `qpayload.go`:

```go
package qpayload

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ErrUnsupportedPayload is the sentinel for a queue payload that cannot be
// dispatched.
var ErrUnsupportedPayload = errors.New("unsupported payload")

// Kind classifies how a decoded queue payload must be processed.
type Kind int

const (
	KindEmpty Kind = iota
	KindSingle
	KindBatch
	KindReference
)

// Result is the dispatch decision for one message body.
type Result struct {
	Kind  Kind
	Count int
}

// Dispatch inspects a message body already decoded into any (via
// json.Decoder with UseNumber) and classifies it by its decoded shape, not by
// an explicit envelope field. Producers on different versions emit a bare
// JSON object for a single job, a JSON array for a batch, a string for a
// reference key pointing at an out-of-band blob, or a legacy bare numeric id.
func Dispatch(payload any) (Result, error) {
	switch p := payload.(type) {
	case nil:
		return Result{Kind: KindEmpty}, nil
	case map[string]any:
		return Result{Kind: KindSingle, Count: 1}, nil
	case []any:
		return Result{Kind: KindBatch, Count: len(p)}, nil
	case string:
		if p == "" {
			return Result{}, fmt.Errorf("%w: empty reference key", ErrUnsupportedPayload)
		}
		return Result{Kind: KindReference, Count: 1}, nil
	case json.Number:
		if _, err := p.Int64(); err != nil {
			return Result{}, fmt.Errorf("%w: legacy id %q: %v", ErrUnsupportedPayload, p, err)
		}
		return Result{Kind: KindReference, Count: 1}, nil
	default:
		return Result{}, fmt.Errorf("%w: cannot dispatch %T", ErrUnsupportedPayload, payload)
	}
}
```

Create `qpayload_test.go`:

```go
package qpayload

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestDispatch(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		payload   any
		wantKind  Kind
		wantCount int
		wantErr   bool
	}{
		{"empty", nil, KindEmpty, 0, false},
		{"single object", map[string]any{"order_id": "o-1"}, KindSingle, 1, false},
		{"batch of three", []any{"a", "b", "c"}, KindBatch, 3, false},
		{"reference key", "s3://bucket/key.json", KindReference, 1, false},
		{"empty reference key", "", KindEmpty, 0, true},
		{"legacy numeric id", json.Number("42"), KindReference, 1, false},
		{"legacy id not numeric", json.Number("abc"), KindEmpty, 0, true},
		{"unsupported type", 3.14, KindEmpty, 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := Dispatch(tt.payload)
			if tt.wantErr {
				if !errors.Is(err, ErrUnsupportedPayload) {
					t.Fatalf("Dispatch(%v) err = %v, want ErrUnsupportedPayload", tt.payload, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Dispatch(%v) unexpected error: %v", tt.payload, err)
			}
			if got.Kind != tt.wantKind || got.Count != tt.wantCount {
				t.Fatalf("Dispatch(%v) = %+v, want Kind=%v Count=%d", tt.payload, got, tt.wantKind, tt.wantCount)
			}
		})
	}
}
```

Verify: `go test -count=1 ./...`

## Review

The switch dispatches on the decoded shape itself — `map[string]any` versus
`[]any` versus `string` versus `json.Number` — rather than on a tag field a
producer might forget to set. `json.Number` gets its own case ahead of
`default` so a legacy bare id is validated as numeric instead of silently
becoming a reference string; without `UseNumber` on the decoder it would
arrive as `float64` and need the same treatment. The `default` branch names
the unexpected type so a fifth producer format fails loudly instead of being
silently absorbed.

## Resources

- [A Tour of Go: Type switches](https://go.dev/tour/methods/16)
- [encoding/json.Number](https://pkg.go.dev/encoding/json#Number)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [09-any-numeric-normalizer.md](09-any-numeric-normalizer.md) | Next: [11-cache-value-decoder.md](11-cache-value-decoder.md)
