# Exercise 2: Decode a real webhook payload loaded from testdata/

Inbound webhook and event bodies are large, realistic JSON. Embedding one as a giant string literal in a test buries the assertion under noise. The canonical home for a realistic input is a file under `testdata/`, loaded with `os.ReadFile`. This module parses and validates an `order.created` webhook from fixture files.

## What you'll build

```text
webhook/                      independent module: example.com/webhook
  go.mod                      go 1.26
  event.go                    ParseOrderCreated([]byte); ErrInvalidPayload sentinel
  cmd/
    demo/
      main.go                 parses an inline payload and prints a summary
  event_test.go               loads testdata/*.json via os.ReadFile; asserts parse + domain error
  testdata/
    order_created.json        a valid payload
    malformed.json            valid JSON that violates the domain contract
```

Files: `event.go`, `cmd/demo/main.go`, `event_test.go`, `testdata/order_created.json`, `testdata/malformed.json`.
Implement: `ParseOrderCreated` decoding strict JSON (`DisallowUnknownFields`) into a typed struct and validating it, wrapping `ErrInvalidPayload` with `%w` on any violation.
Test: load each fixture with `os.ReadFile(filepath.Join("testdata", name))`, assert the valid one parses and the malformed one returns `ErrInvalidPayload`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/webhook/cmd/demo ~/go-exercises/webhook/testdata
cd ~/go-exercises/webhook
go mod init example.com/webhook
```

### Why testdata/ and why the path is stable

`testdata/` is a reserved convention in the go command. `go build`, `go vet`, and package listing all ignore any directory named `testdata`, so it is the one place you can park realistic inputs, golden outputs, and seed data without the toolchain trying to compile or lint them. Put your reference payloads there and they stay out of the build entirely while remaining right next to the package that uses them.

The second guarantee is what makes the relative path work: a package's tests always run with the working directory set to that package's source directory. It does not matter whether you invoke `go test ./...` from the repository root or from three directories up — when `event_test.go` runs, its cwd is the `webhook/` package directory, so `filepath.Join("testdata", name)` resolves against that directory every time. That is why fixture-loading tests do not need absolute paths or runtime path discovery; the relative path is deterministic. (Note this is a test-time guarantee. A compiled binary run in production has no such promise, which is why the demo below uses an inline payload rather than reading the file.)

Two engineering details carry their weight here. Use `filepath.Join` rather than string concatenation with a `/`, so the path is correct on every platform and rooted at `testdata`. And treat an `os.ReadFile` failure as a fatal test setup error with `t.Fatalf` — a missing fixture is not a failed assertion about the code, it is a broken test, and continuing would produce a confusing nil-slice decode error instead of naming the real problem.

The decoder is strict. `json.Decoder.DisallowUnknownFields` rejects a body carrying a field the struct does not declare, which catches a producer that added a field you have not accounted for. Every rejection — bad JSON, unknown field, or a domain-rule violation — is wrapped over the single `ErrInvalidPayload` sentinel with `%w`, so a caller checks one thing with `errors.Is` and still gets a descriptive message.

Create `event.go`:

```go
package webhook

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrInvalidPayload wraps every reason an order.created body is rejected.
var ErrInvalidPayload = errors.New("invalid webhook payload")

// LineItem is a single ordered SKU and its quantity.
type LineItem struct {
	SKU      string `json:"sku"`
	Quantity int    `json:"quantity"`
}

// OrderCreated is the decoded order.created webhook body.
type OrderCreated struct {
	Event       string     `json:"event"`
	OrderID     string     `json:"order_id"`
	Currency    string     `json:"currency"`
	AmountCents int64      `json:"amount_cents"`
	Items       []LineItem `json:"items"`
}

// ParseOrderCreated strictly decodes and validates an order.created body.
func ParseOrderCreated(data []byte) (OrderCreated, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var ev OrderCreated
	if err := dec.Decode(&ev); err != nil {
		return OrderCreated{}, fmt.Errorf("%w: decode: %v", ErrInvalidPayload, err)
	}
	if err := ev.validate(); err != nil {
		return OrderCreated{}, err
	}
	return ev, nil
}

func (e OrderCreated) validate() error {
	switch {
	case e.Event != "order.created":
		return fmt.Errorf("%w: event = %q", ErrInvalidPayload, e.Event)
	case e.OrderID == "":
		return fmt.Errorf("%w: missing order_id", ErrInvalidPayload)
	case len(e.Currency) != 3:
		return fmt.Errorf("%w: currency = %q", ErrInvalidPayload, e.Currency)
	case e.AmountCents <= 0:
		return fmt.Errorf("%w: amount_cents = %d", ErrInvalidPayload, e.AmountCents)
	case len(e.Items) == 0:
		return fmt.Errorf("%w: no line items", ErrInvalidPayload)
	}
	return nil
}
```

Now the fixtures. The valid one is a well-formed, contract-satisfying order.

Create `testdata/order_created.json`:

```json
{
  "event": "order.created",
  "order_id": "ord_01H8X",
  "currency": "USD",
  "amount_cents": 4999,
  "items": [
    {"sku": "SKU-1", "quantity": 2},
    {"sku": "SKU-2", "quantity": 1}
  ]
}
```

The malformed one is deliberately valid JSON that violates the domain contract: a two-letter currency, a non-positive amount, and no items. It parses cleanly, then fails `validate`, proving the domain error path — not a JSON syntax error.

Create `testdata/malformed.json`:

```json
{
  "event": "order.created",
  "order_id": "ord_02",
  "currency": "US",
  "amount_cents": -10,
  "items": []
}
```

### The runnable demo

The demo decodes an inline payload (independent of the working directory) and prints a one-line summary.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/webhook"
)

func main() {
	body := []byte(`{
		"event": "order.created",
		"order_id": "ord_01H8X",
		"currency": "USD",
		"amount_cents": 4999,
		"items": [{"sku": "SKU-1", "quantity": 2}, {"sku": "SKU-2", "quantity": 1}]
	}`)

	ev, err := webhook.ParseOrderCreated(body)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%s %d %s across %d items\n", ev.OrderID, ev.AmountCents, ev.Currency, len(ev.Items))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
ord_01H8X 4999 USD across 2 items
```

### The test

`readFixture` centralizes the load-and-fatal-on-error logic and is marked `t.Helper` so a failure points at the calling test line, not at the helper. The valid fixture asserts the decoded fields; the malformed fixture asserts the wrapped sentinel with `errors.Is`; a third case feeds an inline body with an extra field to prove `DisallowUnknownFields` is active.

Create `event_test.go`:

```go
package webhook

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

func TestParseValidFixture(t *testing.T) {
	t.Parallel()

	got, err := ParseOrderCreated(readFixture(t, "order_created.json"))
	if err != nil {
		t.Fatalf("ParseOrderCreated: %v", err)
	}
	if got.OrderID != "ord_01H8X" {
		t.Errorf("OrderID = %q, want ord_01H8X", got.OrderID)
	}
	if got.AmountCents != 4999 {
		t.Errorf("AmountCents = %d, want 4999", got.AmountCents)
	}
	if len(got.Items) != 2 {
		t.Errorf("len(Items) = %d, want 2", len(got.Items))
	}
}

func TestParseMalformedFixture(t *testing.T) {
	t.Parallel()

	_, err := ParseOrderCreated(readFixture(t, "malformed.json"))
	if !errors.Is(err, ErrInvalidPayload) {
		t.Fatalf("err = %v, want ErrInvalidPayload", err)
	}
}

func TestParseRejectsUnknownField(t *testing.T) {
	t.Parallel()

	body := []byte(`{"event":"order.created","order_id":"x","currency":"USD","amount_cents":1,"items":[{"sku":"a","quantity":1}],"extra":true}`)
	if _, err := ParseOrderCreated(body); !errors.Is(err, ErrInvalidPayload) {
		t.Fatalf("err = %v, want ErrInvalidPayload for unknown field", err)
	}
}
```

## Review

The loader is correct when the valid fixture round-trips into the expected struct and every contract violation surfaces as `ErrInvalidPayload`. The habits this exercise builds are the ones that keep fixture tests maintainable: put the realistic input in `testdata/` instead of a wall-of-string literal, load it with `filepath.Join("testdata", name)` so the path is platform-correct and rooted, and rely on the cwd-is-package-dir guarantee rather than hunting for the file at runtime. Fatal on a read error so a missing fixture is diagnosed as broken test setup, not misread as a logic bug. Assert the domain failure with `errors.Is` against the wrapped sentinel, not by string-matching the message.

## Resources

- [cmd/go: package lists and patterns](https://pkg.go.dev/cmd/go#hdr-Package_lists_and_patterns) — why a `testdata` directory is ignored by the toolchain.
- [os.ReadFile](https://pkg.go.dev/os#ReadFile) — reading a whole fixture file into memory.
- [encoding/json: Decoder.DisallowUnknownFields](https://pkg.go.dev/encoding/json#Decoder.DisallowUnknownFields) — strict decoding that rejects unexpected fields.

---

Back to [01-render-golden-string.md](01-render-golden-string.md) | Next: [03-golden-update-flag.md](03-golden-update-flag.md)
