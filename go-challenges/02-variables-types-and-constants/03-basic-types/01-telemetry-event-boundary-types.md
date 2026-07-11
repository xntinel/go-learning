# Exercise 1: Telemetry Event — Types That Encode Boundary Invariants

A telemetry event arrives at a collector as loose wire fields: a service name
string, a numeric status, a latency in milliseconds, a sample rate, a hex trace id.
This module builds the value object that only comes into existence if every one of
those fields is valid, choosing a basic type per field so the invariant is part of
the representation.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
telemetry/                 independent module: example.com/telemetry
  go.mod                   go 1.26
  event.go                 Event value object; NewEvent, ParseTraceID, ServiceNameOK
  cmd/
    demo/
      main.go              builds one event from wire inputs and prints it
  event_test.go            valid construction, rejected input table, ServiceNameOK, 404 case
```

- Files: `event.go`, `cmd/demo/main.go`, `event_test.go`.
- Implement: `NewEvent` building a validated `Event` from wire inputs, `ParseTraceID` decoding a 16-byte id from hex, `ServiceNameOK` validating the name as ASCII runes.
- Test: valid round-trip, a table of rejected inputs, a map-driven `ServiceNameOK`, and the case where status 404 is valid but not retryable.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/telemetry/cmd/demo
cd ~/go-exercises/telemetry
go mod init example.com/telemetry
```

### Why each field gets the type it does

The `Event` struct is the domain value; its field types are chosen so that a
constructed `Event` is already valid. `Status` is `uint16` because HTTP status codes
fit in 16 bits and cannot be negative — the type rejects a negative status for free,
and `NewEvent` narrows the legal range further to 100..599. `Latency` is a
`time.Duration`, not the raw `uint32` milliseconds off the wire: the boundary does
the `time.Duration(latencyMillis) * time.Millisecond` conversion exactly once, so
every consumer downstream gets a real duration and never has to remember the unit.
`SampleRate` is a `float64` because it is a ratio in `[0,1]` where approximation is
fine — but a ratio has no meaning as `NaN` or as `2.0`, so those are rejected at
construction with `math.IsNaN` and a range check. `Retryable` is a `bool` derived
from the status, not an independent input, so it cannot disagree with the status.
`TraceID` is a fixed `[16]byte`: a trace id is exactly 16 bytes of binary, and the
array type makes any other length unrepresentable — a short or long hex input fails
in `ParseTraceID` before it can fill the array.

The two text-handling functions split along the byte/rune line deliberately.
`ParseTraceID` works in raw bytes: `hex.DecodeString` yields a `[]byte`, the length
is checked against the 16-byte array, and `copy` moves the bytes in. `ServiceNameOK`
works in runes because it validates *text*: it ranges over the string (decoding
UTF-8), rejects anything above `unicode.MaxASCII`, and allows only letters, digits,
`-`, and `_`. A service name like `café` is rejected not because the bytes are
malformed but because a non-ASCII letter is not a legal service identifier — a rune
decision, not a byte one.

Create `event.go`:

```go
package telemetry

import (
	"encoding/hex"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode"
)

// Event is a validated telemetry record. Every field's type encodes a boundary
// invariant, so a constructed Event is already good.
type Event struct {
	Service    string
	Status     uint16
	Latency    time.Duration
	SampleRate float64
	Retryable  bool
	TraceID    [16]byte
}

// NewEvent converts loose wire fields into a validated Event, or returns an
// error naming the first field that fails. Latency is converted from wire
// milliseconds to a time.Duration exactly once, here at the boundary.
func NewEvent(service string, status uint16, latencyMillis uint32, sampleRate float64, traceIDHex string) (Event, error) {
	service = strings.TrimSpace(service)
	if !ServiceNameOK(service) {
		return Event{}, fmt.Errorf("invalid service name: %q", service)
	}
	if status < 100 || status > 599 {
		return Event{}, fmt.Errorf("invalid status code: %d", status)
	}
	if math.IsNaN(sampleRate) || sampleRate < 0 || sampleRate > 1 {
		return Event{}, fmt.Errorf("invalid sample rate: %v", sampleRate)
	}

	traceID, err := ParseTraceID(traceIDHex)
	if err != nil {
		return Event{}, err
	}

	return Event{
		Service:    service,
		Status:     status,
		Latency:    time.Duration(latencyMillis) * time.Millisecond,
		SampleRate: sampleRate,
		Retryable:  status >= 500,
		TraceID:    traceID,
	}, nil
}

// ParseTraceID decodes exactly 16 bytes from a hex string into a fixed array.
// It works in raw bytes: a trace id is binary, not text.
func ParseTraceID(raw string) ([16]byte, error) {
	var traceID [16]byte

	decoded, err := hex.DecodeString(raw)
	if err != nil {
		return traceID, fmt.Errorf("decode trace id: %w", err)
	}
	if len(decoded) != len(traceID) {
		return traceID, fmt.Errorf("trace id must be %d bytes, got %d", len(traceID), len(decoded))
	}
	copy(traceID[:], decoded)
	return traceID, nil
}

// ServiceNameOK reports whether service is a legal identifier: non-empty, ASCII,
// and composed only of letters, digits, '-', and '_'. It works in runes because
// it validates text.
func ServiceNameOK(service string) bool {
	if service == "" {
		return false
	}
	for _, r := range service {
		if r > unicode.MaxASCII {
			return false
		}
		if r == '-' || r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) {
			continue
		}
		return false
	}
	return true
}
```

### The runnable demo

The demo builds one event from wire-shaped inputs and prints its domain fields,
including the trace id round-tripped back to hex with `hex.EncodeToString`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"encoding/hex"
	"fmt"
	"log"

	"example.com/telemetry"
)

func main() {
	ev, err := telemetry.NewEvent(" api_v1 ", 502, 125, 0.25, "00112233445566778899aabbccddeeff")
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("service=%s status=%d latency=%s retryable=%t\n",
		ev.Service, ev.Status, ev.Latency, ev.Retryable)
	fmt.Printf("trace=%s\n", hex.EncodeToString(ev.TraceID[:]))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
service=api_v1 status=502 latency=125ms retryable=true
trace=00112233445566778899aabbccddeeff
```

### Tests

The tests force each type choice to defend an invariant. `TestNewEventBuildsValidatedEvent`
round-trips a valid event and checks the trimmed service, the millisecond-to-duration
conversion, the derived `Retryable` for a 5xx, and the hex round-trip of the trace id.
`TestNewEventRejectsInvalidInput` is a table of inputs that must each be refused: an
empty service, a non-ASCII service, an out-of-range status, a sample rate above one,
a `NaN` sample rate, and a short trace id. `TestServiceNameOK` is map-driven over the
allowed and rejected identifier shapes. `TestNewEventStatus404NotRetryable` pins the
boundary between "valid" and "retryable": a 404 is a perfectly valid event, but not a
retryable one, because `Retryable` is `status >= 500`.

Create `event_test.go`:

```go
package telemetry

import (
	"encoding/hex"
	"fmt"
	"math"
	"net/http"
	"testing"
	"time"
)

func TestNewEventBuildsValidatedEvent(t *testing.T) {
	t.Parallel()

	traceID := "00112233445566778899aabbccddeeff"
	got, err := NewEvent(" api_v1 ", http.StatusBadGateway, 125, 0.25, traceID)
	if err != nil {
		t.Fatal(err)
	}

	if got.Service != "api_v1" {
		t.Fatalf("Service = %q", got.Service)
	}
	if got.Status != http.StatusBadGateway {
		t.Fatalf("Status = %d", got.Status)
	}
	if got.Latency != 125*time.Millisecond {
		t.Fatalf("Latency = %s", got.Latency)
	}
	if got.SampleRate != 0.25 {
		t.Fatalf("SampleRate = %v", got.SampleRate)
	}
	if !got.Retryable {
		t.Fatal("Retryable should be true for 5xx")
	}
	if encoded := hex.EncodeToString(got.TraceID[:]); encoded != traceID {
		t.Fatalf("TraceID = %s", encoded)
	}
}

func TestNewEventRejectsInvalidInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		service    string
		status     uint16
		sampleRate float64
		traceIDHex string
	}{
		{name: "empty service", service: "", status: 200, sampleRate: 1, traceIDHex: "00112233445566778899aabbccddeeff"},
		{name: "unicode service", service: "café", status: 200, sampleRate: 1, traceIDHex: "00112233445566778899aabbccddeeff"},
		{name: "bad status", service: "api", status: 42, sampleRate: 1, traceIDHex: "00112233445566778899aabbccddeeff"},
		{name: "bad sample rate", service: "api", status: 200, sampleRate: 2, traceIDHex: "00112233445566778899aabbccddeeff"},
		{name: "nan sample rate", service: "api", status: 200, sampleRate: math.NaN(), traceIDHex: "00112233445566778899aabbccddeeff"},
		{name: "short trace", service: "api", status: 200, sampleRate: 1, traceIDHex: "001122"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := NewEvent(tt.service, tt.status, 10, tt.sampleRate, tt.traceIDHex)
			if err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestServiceNameOK(t *testing.T) {
	t.Parallel()

	tests := map[string]bool{
		"api":      true,
		"api_v1":   true,
		"worker-2": true,
		"":         false,
		"bad name": false,
		"café":     false,
	}

	for service, want := range tests {
		got := ServiceNameOK(service)
		if got != want {
			t.Fatalf("ServiceNameOK(%q) = %v, want %v", service, got, want)
		}
	}
}

func TestNewEventStatus404NotRetryable(t *testing.T) {
	t.Parallel()

	got, err := NewEvent("api", http.StatusNotFound, 10, 0.5, "00112233445566778899aabbccddeeff")
	if err != nil {
		t.Fatalf("404 should be a valid event: %v", err)
	}
	if got.Retryable {
		t.Fatal("Retryable should be false for 404")
	}
}

func ExampleServiceNameOK() {
	fmt.Println(ServiceNameOK("api_v1"), ServiceNameOK("bad name"))
	// Output: true false
}
```

## Review

The design is correct when a constructed `Event` cannot be invalid: `Status` cannot
be negative or outside 100..599, `SampleRate` cannot be `NaN` or outside `[0,1]`,
`TraceID` cannot be any length but 16, and `Service` cannot contain non-ASCII or
non-identifier characters. The two text functions split correctly: `ParseTraceID`
reasons in bytes because a trace id is binary, and `ServiceNameOK` reasons in runes
because a name is text — swapping them (validating the trace id as runes, or the name
by byte value) would be the classic byte/rune confusion. The `Retryable` field is
derived, not supplied, so the 404 test confirms it tracks the status rather than a
caller's claim. Run `go test -race` to confirm the value object is safe to build
concurrently.

## Resources

- [Go Specification: Types](https://go.dev/ref/spec#Types) — the numeric types and their exact widths.
- [Go Blog: Strings, bytes, runes and characters in Go](https://go.dev/blog/strings) — why `range` decodes runes and indexing yields bytes.
- [encoding/hex](https://pkg.go.dev/encoding/hex) — `DecodeString`/`EncodeToString` for the trace id round-trip.
- [math.IsNaN](https://pkg.go.dev/math#IsNaN) — guarding a `float64` sample rate.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-env-config-parser.md](02-env-config-parser.md)
