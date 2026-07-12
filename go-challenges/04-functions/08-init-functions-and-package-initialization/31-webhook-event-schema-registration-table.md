# Exercise 31: Webhook Event Types and Their Field Schemas Registered and Cross-Validated at init

**Nivel: Intermedio** — validacion rapida (un test corto).

A webhook system typically keeps two related tables: the list of event
types it declares support for, and a map from each event type to the
fields its payload must contain. Keeping these as two separate data
structures is convenient to edit, but it opens an easy mistake: adding an
event to one table and forgetting the other. This exercise cross-validates
both tables against each other at package initialization, so a declared
event with no schema, or a schema for an event nobody declared, panics
immediately instead of surfacing as a confusing validation gap at runtime.

## What you'll build

```text
webhookschema/              independent module: example.com/webhookschema
  go.mod                     module example.com/webhookschema
  webhookschema.go             declaredEvents, schemas, crossValidate, SchemaFor, ValidatePayload
  cmd/
    demo/
      main.go                  validates a good payload, a payload missing fields, and an unknown event
  webhookschema_test.go        crossValidate table (matching/missing/orphaned/duplicate) + ValidatePayload tests
```

Files: `webhookschema.go`, `cmd/demo/main.go`, `webhookschema_test.go`.
Implement: `crossValidate(declared []string, schemas map[string]EventSchema) error` checking every declared event has a schema, every schema belongs to a declared event, and no event is declared twice; `SchemaFor(eventType string) (EventSchema, bool)`; `ValidatePayload(eventType string, payload map[string]any) error` checking every required field is present.
Test: matching declared/schema sets pass; a declared event missing its schema, a schema for an undeclared event, and a duplicate declaration each return a descriptive error; `ValidatePayload` succeeds on a complete payload, names the missing field(s) on an incomplete one, and reports an unknown event type distinctly.
Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why two tables need a cross-check, not just two checks

`declaredEvents` (a slice — the canonical list of what this service emits or
accepts) and `schemas` (a map — the field requirements per event) describe
the same set of event type names from two different angles, maintained as
two separate Go literals for readability. That separation is exactly what
makes them drift: someone adds `"order.shipped"` to `declaredEvents` while
adding a webhook handler, forgets to add its schema, and every payload for
that event type silently passes validation with zero required fields
checked — or, in the other direction, someone renames an event in
`declaredEvents` but leaves the old name's schema in place, so the old
schema is now dead code nobody notices is unreachable.

`crossValidate` catches both directions in one pass: it builds the set of
declared names, checks every declared name has a matching schema entry
(catching the first drift), and checks every schema's key is a declared
name (catching the second). It also rejects a declared event listed twice,
which would otherwise mask the fact that one name is doing double duty. All
of this is static, compile-time-known data, so — as with every other
registration-table exercise in this chapter — `init()` is the right place
to run the check exactly once and panic immediately if the tables have
drifted apart, rather than discovering the gap when a real webhook payload
for the orphaned event type sails through unchecked.

Create `webhookschema.go`:

```go
// webhookschema.go
// Package webhookschema registers webhook event types alongside the field
// schema each event's payload must satisfy, cross-validating at package
// initialization that every declared event type has a schema and every
// schema belongs to a declared event type -- so a typo in either table
// panics at startup instead of silently accepting or rejecting the wrong
// payloads at runtime.
package webhookschema

import (
	"fmt"
	"sort"
)

// EventSchema lists the fields a webhook payload for one event type must
// contain.
type EventSchema struct {
	RequiredFields []string
}

// declaredEvents is the list of event types this service is expected to
// emit or accept. schemas must have exactly one entry per name here.
var declaredEvents = []string{"user.created", "user.deleted", "order.paid", "order.refunded"}

// schemas maps each declared event type to its required-field schema.
var schemas = map[string]EventSchema{
	"user.created":   {RequiredFields: []string{"user_id", "email"}},
	"user.deleted":   {RequiredFields: []string{"user_id"}},
	"order.paid":     {RequiredFields: []string{"order_id", "amount_cents"}},
	"order.refunded": {RequiredFields: []string{"order_id", "amount_cents", "reason"}},
}

func init() {
	if err := crossValidate(declaredEvents, schemas); err != nil {
		panic("webhookschema: " + err.Error())
	}
}

// crossValidate confirms declared and schemas describe exactly the same
// set of event type names: every declared event must have a schema, and
// every schema must belong to a declared event. It is extracted from init
// so tests can exercise both mismatch directions directly.
func crossValidate(declared []string, schemas map[string]EventSchema) error {
	declaredSet := make(map[string]struct{}, len(declared))
	for _, name := range declared {
		if _, dup := declaredSet[name]; dup {
			return fmt.Errorf("event %q declared more than once", name)
		}
		declaredSet[name] = struct{}{}
	}

	var missing []string
	for _, name := range declared {
		if _, ok := schemas[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("declared event(s) missing a schema: %v", missing)
	}

	var orphaned []string
	for name := range schemas {
		if _, ok := declaredSet[name]; !ok {
			orphaned = append(orphaned, name)
		}
	}
	if len(orphaned) > 0 {
		sort.Strings(orphaned)
		return fmt.Errorf("schema(s) registered for undeclared event(s): %v", orphaned)
	}

	return nil
}

// SchemaFor returns the field schema for a declared event type.
func SchemaFor(eventType string) (EventSchema, bool) {
	s, ok := schemas[eventType]
	return s, ok
}

// ValidatePayload confirms payload contains every field eventType's schema
// requires.
func ValidatePayload(eventType string, payload map[string]any) error {
	schema, ok := SchemaFor(eventType)
	if !ok {
		return fmt.Errorf("webhookschema: unknown event type %q", eventType)
	}
	var missing []string
	for _, field := range schema.RequiredFields {
		if _, ok := payload[field]; !ok {
			missing = append(missing, field)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("webhookschema: payload for %q missing field(s): %v", eventType, missing)
	}
	return nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/webhookschema"
)

func main() {
	err := webhookschema.ValidatePayload("order.paid", map[string]any{
		"order_id":     "ord_1",
		"amount_cents": 1999,
	})
	fmt.Println("order.paid valid payload:", err)

	err = webhookschema.ValidatePayload("order.refunded", map[string]any{
		"order_id": "ord_1",
	})
	fmt.Println("order.refunded missing fields:", err)

	err = webhookschema.ValidatePayload("shipment.created", map[string]any{})
	fmt.Println("unknown event type:", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
order.paid valid payload: <nil>
order.refunded missing fields: webhookschema: payload for "order.refunded" missing field(s): [amount_cents reason]
unknown event type: webhookschema: unknown event type "shipment.created"
```

### Tests

Create `webhookschema_test.go`:

```go
// webhookschema_test.go
package webhookschema

import (
	"strings"
	"testing"
)

func TestCrossValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		declared []string
		schemas  map[string]EventSchema
		wantErr  string
	}{
		{
			name:     "matching sets",
			declared: []string{"a", "b"},
			schemas:  map[string]EventSchema{"a": {}, "b": {}},
		},
		{
			name:     "missing schema",
			declared: []string{"a", "b"},
			schemas:  map[string]EventSchema{"a": {}},
			wantErr:  "missing a schema",
		},
		{
			name:     "orphaned schema",
			declared: []string{"a"},
			schemas:  map[string]EventSchema{"a": {}, "b": {}},
			wantErr:  "undeclared event",
		},
		{
			name:     "declared twice",
			declared: []string{"a", "a"},
			schemas:  map[string]EventSchema{"a": {}},
			wantErr:  "more than once",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := crossValidate(tc.declared, tc.schemas)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestValidatePayload(t *testing.T) {
	t.Parallel()

	if err := ValidatePayload("order.paid", map[string]any{"order_id": "o1", "amount_cents": 100}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	err := ValidatePayload("order.paid", map[string]any{"order_id": "o1"})
	if err == nil || !strings.Contains(err.Error(), "amount_cents") {
		t.Fatalf("err = %v, want mentioning missing field amount_cents", err)
	}

	err = ValidatePayload("nonexistent.event", map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "unknown event type") {
		t.Fatalf("err = %v, want unknown event type error", err)
	}
}

func TestPackageSchemaRegisteredAtInit(t *testing.T) {
	t.Parallel()

	s, ok := SchemaFor("user.created")
	if !ok {
		t.Fatal("SchemaFor(\"user.created\") ok = false")
	}
	if len(s.RequiredFields) != 2 {
		t.Fatalf("user.created RequiredFields = %v, want 2 fields", s.RequiredFields)
	}

	if _, ok := SchemaFor("nonexistent"); ok {
		t.Fatal("SchemaFor(\"nonexistent\") ok = true, want false")
	}
}
```

## Review

`crossValidate` is correct when it catches all three ways the two tables
can drift apart: a declared event with no matching schema, a schema keyed
by a name that was never declared, and the same event name declared more
than once (which would otherwise mask which of the two declarations is
"real"). `ValidatePayload` builds on a schema table that is already known
to be internally consistent — `TestPackageSchemaRegisteredAtInit` confirms
the package's own production tables passed `crossValidate` at init, or the
test binary would never have started — and correctly distinguishes an
unknown event type from a known one with missing required fields, naming
every missing field rather than stopping at the first.

The mistake to avoid is validating only one direction: checking that every
declared event has a schema catches half the drift, but a schema silently
registered for a typo'd or removed event name — dead configuration that
nobody notices — needs the other direction of the check, which is exactly
what `crossValidate`'s second pass over `schemas`'s keys exists to catch.

## Resources

- [Go spec: Package initialization](https://go.dev/ref/spec#Package_initialization) — why the cross-check between the two tables belongs in `init()`.
- [Svix — Webhook event types](https://docs.svix.com/receiving/verifying-payloads/why) — a real webhook platform's event-type and payload-schema conventions.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [30-circuit-breaker-thresholds-from-config.md](30-circuit-breaker-thresholds-from-config.md) | Next: [32-multi-tenant-routing-table-validator.md](32-multi-tenant-routing-table-validator.md)
