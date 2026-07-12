# Exercise 10: Round-trip fixture — canonical JSON marshal and unmarshal stability

A versioned API or event struct has a wire contract: it must marshal to a canonical, stable JSON shape, and that same JSON must unmarshal back to the identical struct. Testing only one direction lets schema drift slip through the other. This module pins both directions of a payment event against a canonical golden fixture.

## What you'll build

```text
roundtrip/                    independent module: example.com/roundtrip
  go.mod                      go 1.26 (requires github.com/google/go-cmp)
  event.go                    PaymentEvent; CanonicalJSON() using json.Indent
  cmd/
    demo/
      main.go                 prints the canonical JSON of a sample event
  event_test.go               marshal==golden; unmarshal(golden)==struct; full round-trip
  testdata/
    event.golden.json         the canonical indented JSON
```

Files: `event.go`, `cmd/demo/main.go`, `event_test.go`, `testdata/event.golden.json`.
Implement: `PaymentEvent` and `CanonicalJSON` producing indented, canonical JSON via `json.Marshal` + `json.Indent`.
Test: assert `CanonicalJSON()` equals the golden (`bytes.Equal`); assert `json.Unmarshal(golden)` equals the sample struct (`cmp.Diff`); and assert a full struct to JSON to struct round-trip.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/07-test-fixtures-and-testdata/10-roundtrip-canonical-fixture/cmd/demo go-solutions/12-testing-ecosystem/07-test-fixtures-and-testdata/10-roundtrip-canonical-fixture/testdata
cd go-solutions/12-testing-ecosystem/07-test-fixtures-and-testdata/10-roundtrip-canonical-fixture
go get github.com/google/go-cmp/cmp
```

### Why both directions matter

A wire contract has two halves and a golden test that checks only one half is half a test. The marshal direction pins the *shape* the struct produces: field names, field order, indentation, how a slice renders. The unmarshal direction pins that the same JSON *decodes back* into the struct you expect. Drift can hide in either. Rename a `json:"amount_cents"` tag to `json:"amount"` and re-run only a marshal test with `-update`, and the golden happily changes to match — the test stays green while every existing consumer of `amount_cents` breaks. Test only unmarshal, and a struct whose marshal output silently reordered or dropped a field goes unnoticed because decoding is lenient about field order and missing fields. Asserting both directions against one committed golden is the cheapest guard against silent JSON-tag and schema drift: a tag change breaks at least one direction, and the break shows up in the diff a reviewer reads.

The canonicalization matters because "the JSON" must be a single, stable byte sequence to compare against a file. `json.Marshal` produces compact JSON with fields in struct-declaration order; running that through `json.Indent` yields a deterministic pretty-printed form — the same algorithm `MarshalIndent` uses, shown here in its two-step form to make the "canonical" step explicit. Struct field order is the canonical field order, so declaring fields in a stable sequence is part of the contract, not an accident of formatting.

The comparison tools split by direction. The marshal assertion is a byte comparison (`bytes.Equal`) against the golden file — bytes are the contract. The unmarshal assertion is a structural comparison (`cmp.Diff`) between the decoded value and the expected struct, which yields a readable field-level diff when a tag drifts, far more useful than a boolean `reflect.DeepEqual`.

Create `event.go`:

```go
package roundtrip

import (
	"bytes"
	"encoding/json"
)

// PaymentEvent is a versioned payment event with a canonical wire form.
type PaymentEvent struct {
	Version  int      `json:"version"`
	EventID  string   `json:"event_id"`
	Amount   int64    `json:"amount_cents"`
	Currency string   `json:"currency"`
	Tags     []string `json:"tags"`
}

// CanonicalJSON renders the event as canonical, indented JSON: compact-marshal
// for stable field order, then json.Indent for a deterministic pretty form.
func (e PaymentEvent) CanonicalJSON() ([]byte, error) {
	raw, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
```

The golden is the canonical wire form of the sample event — a reviewer reads it as the contract.

Create `testdata/event.golden.json`:

```json
{
  "version": 2,
  "event_id": "evt_123",
  "amount_cents": 4999,
  "currency": "USD",
  "tags": [
    "retail",
    "priority"
  ]
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/roundtrip"
)

func main() {
	e := roundtrip.PaymentEvent{
		Version:  2,
		EventID:  "evt_123",
		Amount:   4999,
		Currency: "USD",
		Tags:     []string{"retail", "priority"},
	}
	b, err := e.CanonicalJSON()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(string(b))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
{
  "version": 2,
  "event_id": "evt_123",
  "amount_cents": 4999,
  "currency": "USD",
  "tags": [
    "retail",
    "priority"
  ]
}
```

### The test

Three tests cover the contract. The marshal test byte-compares `CanonicalJSON()` to the golden. The unmarshal test decodes the golden and `cmp.Diff`s it against the sample struct. The round-trip test closes the loop: struct to canonical JSON to struct, asserting the value survives unchanged.

Create `event_test.go`:

```go
package roundtrip

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
)

var sample = PaymentEvent{
	Version:  2,
	EventID:  "evt_123",
	Amount:   4999,
	Currency: "USD",
	Tags:     []string{"retail", "priority"},
}

func goldenJSON(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "event.golden.json"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	return b
}

func TestMarshalMatchesGolden(t *testing.T) {
	t.Parallel()

	got, err := sample.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	if want := goldenJSON(t); !bytes.Equal(bytes.TrimSpace(got), bytes.TrimSpace(want)) {
		t.Fatalf("canonical JSON drift:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestUnmarshalGoldenMatchesStruct(t *testing.T) {
	t.Parallel()

	var got PaymentEvent
	if err := json.Unmarshal(goldenJSON(t), &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if diff := cmp.Diff(sample, got); diff != "" {
		t.Fatalf("golden did not decode to sample (-sample +got):\n%s", diff)
	}
}

func TestStructJSONStructRoundtrips(t *testing.T) {
	t.Parallel()

	raw, err := sample.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	var back PaymentEvent
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if diff := cmp.Diff(sample, back); diff != "" {
		t.Fatalf("round-trip mismatch (-sample +back):\n%s", diff)
	}
}

func ExamplePaymentEvent_roundtrip() {
	e := PaymentEvent{Version: 1, EventID: "e", Currency: "EUR"}
	b, _ := e.CanonicalJSON()
	var back PaymentEvent
	_ = json.Unmarshal(b, &back)
	fmt.Println(back.Version, back.EventID, back.Currency)
	// Output: 1 e EUR
}
```

## Review

The contract holds when the struct marshals byte-for-byte to the golden and the golden decodes back to the identical struct. The failure this exercise guards against is the one-directional test that misses drift in the untested direction: a renamed or dropped `json` tag can pass a marshal-only test (regenerate the golden and it "matches") or an unmarshal-only test (decoding tolerates missing fields), yet still break real consumers. Assert both directions against one committed golden, use `bytes.Equal` for the shape and `cmp.Diff` for the value, and keep struct field order deliberate — it is the canonical field order that the golden encodes.

## Resources

- [encoding/json: Indent](https://pkg.go.dev/encoding/json#Indent) — deterministic pretty-printing of compact JSON.
- [encoding/json: Marshal and Unmarshal](https://pkg.go.dev/encoding/json#Marshal) — the two directions of the wire contract.
- [github.com/google/go-cmp/cmp: Diff](https://pkg.go.dev/github.com/google/go-cmp/cmp#Diff) — readable structural comparison for the decode direction.

---

Back to [09-per-case-directories.md](09-per-case-directories.md) | Next: [../08-mocking-with-interfaces/00-concepts.md](../08-mocking-with-interfaces/00-concepts.md)
