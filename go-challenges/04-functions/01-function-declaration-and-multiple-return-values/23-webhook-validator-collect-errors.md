# Exercise 23: Webhook Payload Schema Validator

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A webhook sender fixing a broken integration needs the *complete* list of
what is wrong with their payload, not one violation per HTTP round trip.
This exercise builds `Validate(payload) (valid bool, errs []string)`,
which checks every field regardless of what already failed, collecting
every violation into one response instead of returning on the first
error.

This module is fully self-contained: its own `go mod init`, all code
inline, its own demo and tests.

## What you'll build

```text
webhookvalidate/              independent module: example.com/webhook-validator-collect-errors
  go.mod                      go 1.24
  webhookvalidate.go           package webhookvalidate; Validator; Validate(payload) (valid,errs)
  cmd/
    demo/
      main.go                   a well-formed payload and one broken in four ways at once
  webhookvalidate_test.go       well-formed case; "collects everything" case; one subtest per individual rule
```

- Files: `webhookvalidate.go`, `cmd/demo/main.go`, `webhookvalidate_test.go`.
- Implement: `(*Validator).Validate(payload map[string]any) (valid bool, errs []string)` checking `event` (known type, via comma-ok assertion), `id` (string with a required prefix), `timestamp` (RFC3339), and `amount_cents` (non-negative number) — every rule evaluated regardless of earlier failures.
- Test: a payload violating all four rules at once returns all four messages in one call, not just the first; individual rule violations (unknown event, bad id prefix, non-numeric amount, negative amount) are each present in `errs` when triggered alone.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Return on first error is the wrong shape here

A validator built the way an early-return function normally is —
`if !ok { return false, err }` at the first failing check — forces the
sender through one bug-fix-resubmit cycle per violation. If a payload has
a bad `id` *and* a missing `amount_cents`, an early-return validator tells
them about the `id` first, they fix it, resubmit, and only then find out
about `amount_cents`. Each rule here is checked unconditionally and
appends to a shared `errs` slice instead:

```go
event, ok := payload["event"].(string)
switch {
case !ok || event == "":
	errs = append(errs, "event: missing or not a string")
case !v.allowedEvents[event]:
	errs = append(errs, fmt.Sprintf("event: unknown event type %q", event))
}

id, ok := payload["id"].(string)
switch {
case !ok || id == "":
	errs = append(errs, "id: missing or not a string")
case !strings.HasPrefix(id, "evt_"):
	errs = append(errs, fmt.Sprintf("id: must start with %q, got %q", "evt_", id))
}
// ... timestamp, amount_cents checked the same unconditional way
return len(errs) == 0, errs
```

Every field access still goes through a comma-ok type assertion
(`payload["event"].(string)`) rather than a bare one, because `payload` is
`map[string]any` straight off a JSON decode — a field the sender got wrong
could be a number, a nested object, or simply absent, and none of those
should panic the validator that exists specifically to catch sender
mistakes.

Create `webhookvalidate.go`:

```go
package webhookvalidate

import (
	"fmt"
	"strings"
	"time"
)

// Validator checks decoded webhook payloads (already JSON-unmarshalled into
// map[string]any) against a fixed schema.
type Validator struct {
	allowedEvents map[string]bool
}

// NewValidator builds a Validator accepting exactly the given event type
// names.
func NewValidator(allowedEvents ...string) *Validator {
	set := make(map[string]bool, len(allowedEvents))
	for _, e := range allowedEvents {
		set[e] = true
	}
	return &Validator{allowedEvents: set}
}

// Validate checks payload against every rule and collects every violation
// found, rather than returning on the first one. A webhook sender fixing
// its integration needs the full list in one response — reporting only the
// first problem means it fixes that, resubmits, and immediately hits the
// second one, one round trip per bug instead of one round trip total.
func (v *Validator) Validate(payload map[string]any) (valid bool, errs []string) {
	event, ok := payload["event"].(string)
	switch {
	case !ok || event == "":
		errs = append(errs, "event: missing or not a string")
	case !v.allowedEvents[event]:
		errs = append(errs, fmt.Sprintf("event: unknown event type %q", event))
	}

	id, ok := payload["id"].(string)
	switch {
	case !ok || id == "":
		errs = append(errs, "id: missing or not a string")
	case !strings.HasPrefix(id, "evt_"):
		errs = append(errs, fmt.Sprintf("id: must start with %q, got %q", "evt_", id))
	}

	ts, ok := payload["timestamp"].(string)
	if !ok {
		errs = append(errs, "timestamp: missing or not a string")
	} else if _, err := time.Parse(time.RFC3339, ts); err != nil {
		errs = append(errs, fmt.Sprintf("timestamp: not RFC3339: %v", err))
	}

	amountRaw, ok := payload["amount_cents"]
	if !ok {
		errs = append(errs, "amount_cents: missing")
	} else if amount, ok := amountRaw.(float64); !ok {
		errs = append(errs, "amount_cents: not a number")
	} else if amount < 0 {
		errs = append(errs, fmt.Sprintf("amount_cents: must be >= 0, got %v", amount))
	}

	return len(errs) == 0, errs
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/webhook-validator-collect-errors"
)

func main() {
	validator := webhookvalidate.NewValidator("payment.created", "payment.refunded")

	good := map[string]any{
		"event":        "payment.created",
		"id":           "evt_123",
		"timestamp":    "2030-01-01T00:00:00Z",
		"amount_cents": float64(4999),
	}
	valid, errs := validator.Validate(good)
	fmt.Printf("well-formed payload: valid=%t errs=%v\n", valid, errs)

	broken := map[string]any{
		"event":     "payment.disputed", // not in the allowed set
		"id":        "wrong-prefix",     // missing "evt_" prefix
		"timestamp": "not-a-timestamp",  // not RFC3339
		// amount_cents omitted entirely
	}
	valid, errs = validator.Validate(broken)
	fmt.Printf("broken payload: valid=%t\n", valid)
	for _, e := range errs {
		fmt.Println("  -", e)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
well-formed payload: valid=true errs=[]
broken payload: valid=false
  - event: unknown event type "payment.disputed"
  - id: must start with "evt_", got "wrong-prefix"
  - timestamp: not RFC3339: parsing time "not-a-timestamp" as "2006-01-02T15:04:05Z07:00": cannot parse "not-a-timestamp" as "2006"
  - amount_cents: missing
```

### Tests

Create `webhookvalidate_test.go`:

```go
package webhookvalidate

import "testing"

func TestValidateWellFormedPayload(t *testing.T) {
	t.Parallel()
	v := NewValidator("payment.created")

	valid, errs := v.Validate(map[string]any{
		"event":        "payment.created",
		"id":           "evt_1",
		"timestamp":    "2030-01-01T00:00:00Z",
		"amount_cents": float64(100),
	})
	if !valid {
		t.Fatalf("valid = false, errs = %v", errs)
	}
	if len(errs) != 0 {
		t.Fatalf("errs = %v, want empty", errs)
	}
}

func TestValidateCollectsEveryViolation(t *testing.T) {
	t.Parallel()
	v := NewValidator("payment.created")

	// Every field is wrong or missing; the validator must not stop at the
	// first one — all four violations must be present in one pass.
	valid, errs := v.Validate(map[string]any{
		"event":     "payment.disputed",
		"id":        "wrong-prefix",
		"timestamp": "not-a-timestamp",
	})
	if valid {
		t.Fatal("valid = true, want false")
	}
	if len(errs) != 4 {
		t.Fatalf("errs = %v, want 4 violations (event, id, timestamp, amount_cents)", errs)
	}
}

func TestValidateIndividualRules(t *testing.T) {
	t.Parallel()
	v := NewValidator("payment.created")
	base := func() map[string]any {
		return map[string]any{
			"event":        "payment.created",
			"id":           "evt_1",
			"timestamp":    "2030-01-01T00:00:00Z",
			"amount_cents": float64(100),
		}
	}

	cases := []struct {
		name    string
		mutate  func(map[string]any)
		wantErr string
	}{
		{"unknown event", func(p map[string]any) { p["event"] = "payment.disputed" }, `event: unknown event type "payment.disputed"`},
		{"bad id prefix", func(p map[string]any) { p["id"] = "no-prefix" }, `id: must start with "evt_", got "no-prefix"`},
		{"amount not a number", func(p map[string]any) { p["amount_cents"] = "100" }, "amount_cents: not a number"},
		{"negative amount", func(p map[string]any) { p["amount_cents"] = float64(-5) }, "amount_cents: must be >= 0, got -5"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			payload := base()
			tc.mutate(payload)

			valid, errs := v.Validate(payload)
			if valid {
				t.Fatal("valid = true, want false")
			}
			found := false
			for _, e := range errs {
				if e == tc.wantErr {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("errs = %v, want it to contain %q", errs, tc.wantErr)
			}
		})
	}
}
```

## Review

`Validate` is correct when every rule runs regardless of the others' outcome
and `errs` ends up with one entry per genuine violation, in the same fixed
field order every time (this validator never ranges over `payload`'s keys
for its checks, so there is no map-iteration-order nondeterminism to guard
against). `TestValidateCollectsEveryViolation` is the load-bearing test —
it is the one an early-return implementation would fail, since it asserts
on `len(errs) == 4`, not just `valid == false`.

The mistake to avoid is an `if err := checkX(); err != nil { return false,
[]string{err.Error()} }` chain that looks almost identical to this
function but silently reverts to first-error-wins the moment someone
"simplifies" one of the `switch` blocks back into an early return.

## Resources

- [Go spec: type assertions](https://go.dev/ref/spec#Type_assertions) — the comma-ok form guarding every field read from the decoded `map[string]any`.
- [time.Parse and the reference time](https://pkg.go.dev/time#Parse) — validating `timestamp` against `time.RFC3339`.
- [RFC 3339: Date and Time on the Internet](https://www.rfc-editor.org/rfc/rfc3339) — the timestamp format webhook payloads in this exercise are expected to use.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [22-cache-lookup-with-age.md](22-cache-lookup-with-age.md) | Next: [24-feature-flag-resolve-audited.md](24-feature-flag-resolve-audited.md)
