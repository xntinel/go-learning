# Exercise 32: Validate Nested Protobuf Messages Against Schema

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Protobuf schemas are routinely self-referential: a `Comment` message with
a repeated `replies` field of type `Comment` is exactly how a threaded
comment section, a file tree, or an org chart gets modeled in a `.proto`
file. Validating a decoded message against that schema is a clean
recurrence — check this message's own fields, then recurse into each
nested or repeated message field using the same validator — and the
recursive schema is not a mistake to design around, it is the whole point
of the feature. The risk it carries is that whoever sends the wire payload
controls how deep the recursion actually goes: a legitimate schema with a
`Comment → replies → Comment` cycle in its *type definition* is completely
normal, but a payload that actually nests a few hundred replies deep is
not a real comment thread, it is a validator being tested for how easily
it falls over.

This module is fully self-contained: its own `go mod init`, the schema
types and validator inline, its own demo and tests.

## What you'll build

```text
protoval/                     independent module: example.com/protoval
  go.mod                         go 1.24
  protoval.go                     type MessageSchema/FieldSchema; Validate (recursive, depth-guarded)
  protoval_test.go                well-formed message, missing required field, wrong type, bad repeated field/element, invalid maxDepth, deep reply chain
  cmd/
    demo/
      main.go                     a self-referential Comment schema validates a real tree, then rejects a 200-deep reply chain
```

- Files: `protoval.go`, `cmd/demo/main.go`, `protoval_test.go`.
- Implement: `FieldSchema{Name string; Type FieldType; Required bool; Message *MessageSchema}`, `MessageSchema{Name string; Fields []FieldSchema}`, and `Validate(schema *MessageSchema, msg map[string]any, maxDepth int) error` recursing through `validateMessage`/`validateField`.
- Test: a well-formed nested message; a missing required field; a wrong scalar type; a repeated field that is not a list; a repeated element that is not a message; an invalid `maxDepth`; a 200-deep reply chain against a self-referential schema, rejected via `errors.Is(err, ErrMaxDepthExceeded)`.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/protoval/cmd/demo
cd ~/go-exercises/protoval
go mod init example.com/protoval
go mod edit -go=1.24
```

### A self-referential schema is legitimate; a self-referential payload depth is not

`comment.Fields` in the demo includes a field whose `Message` points back
at `comment` itself — Go allows this because `comment` is a `*MessageSchema`
allocated first, with its `Fields` slice assigned afterward, so the
pointer cycle is in the *schema*, not in any one message value. That cycle
is completely static: it describes a type, not a specific piece of data,
and it is exactly why real protobuf definitions use it for tree-shaped
data. `validateMessage` never walks the schema in a loop looking for
cycles, because a cyclic type is not the problem; a payload that actually
uses that type recursively hundreds of times is. The single `depth >
maxDepth` check at the top of `validateMessage` is the only defense
needed: it does not care whether the schema could in principle recurse
forever, only whether *this particular payload* has recursed past the
configured bound, and it fires before that payload's next nested message
is read at all.

Two more choices are worth noticing. First, an absent field is not itself
invalid — proto3 semantics treat every field as optional by default —
so only `Required` fields are checked for presence; everything else is
validated only if it is actually there. Second, scalar values are checked
by Go's dynamic type (`val.(string)`, `val.(float64)`, `val.(bool)`)
against what the field claims to be, mirroring how a message decoded via
`protojson`-style JSON mapping would actually arrive: numbers as
`float64`, nested messages as `map[string]any`, repeated fields as
`[]any`.

Create `protoval.go`:

```go
// Package protoval validates a decoded protobuf-style message (as
// represented after protojson-style decoding: nested messages as
// map[string]any, repeated fields as []any) against a MessageSchema,
// recursively. Protobuf schemas are routinely self-referential -- a
// "Comment" message with a repeated "replies" field of its own type is
// exactly how a comment thread is modeled -- which means an attacker who
// controls the wire payload can nest replies far deeper than any real
// comment thread would, purely to exhaust the validator's stack or
// runtime. Validate enforces a maximum nesting depth so that payload is
// rejected cleanly instead.
package protoval

import (
	"errors"
	"fmt"
)

// ErrMaxDepthExceeded is returned when a message nests deeper than the
// configured maximum.
var ErrMaxDepthExceeded = errors.New("protoval: message nesting exceeds maximum depth")

// FieldType names the kind of value a field holds.
type FieldType int

const (
	FieldString FieldType = iota
	FieldNumber
	FieldBool
	FieldMessage
	FieldRepeatedMessage
)

// FieldSchema describes one field of a message schema. Message must be set
// when Type is FieldMessage or FieldRepeatedMessage; it may point back at
// the enclosing MessageSchema itself for a self-referential type.
type FieldSchema struct {
	Name     string
	Type     FieldType
	Required bool
	Message  *MessageSchema
}

// MessageSchema describes a protobuf-style message: a name plus its
// fields.
type MessageSchema struct {
	Name   string
	Fields []FieldSchema
}

// Validate checks msg against schema, recursively validating nested and
// repeated message fields, and enforcing maxDepth (schema's own message is
// depth 1).
func Validate(schema *MessageSchema, msg map[string]any, maxDepth int) error {
	if maxDepth < 1 {
		return fmt.Errorf("protoval: maxDepth must be >= 1, got %d", maxDepth)
	}
	return validateMessage(schema, msg, 1, maxDepth)
}

func validateMessage(schema *MessageSchema, msg map[string]any, depth, maxDepth int) error {
	if depth > maxDepth {
		return fmt.Errorf("%w: message %s at depth %d", ErrMaxDepthExceeded, schema.Name, depth)
	}

	for _, f := range schema.Fields {
		val, present := msg[f.Name]
		if !present {
			if f.Required {
				return fmt.Errorf("protoval: %s.%s: required field missing", schema.Name, f.Name)
			}
			continue
		}
		if err := validateField(schema, f, val, depth, maxDepth); err != nil {
			return err
		}
	}
	return nil
}

func validateField(schema *MessageSchema, f FieldSchema, val any, depth, maxDepth int) error {
	switch f.Type {
	case FieldString:
		if _, ok := val.(string); !ok {
			return fmt.Errorf("protoval: %s.%s: want string, got %T", schema.Name, f.Name, val)
		}
	case FieldNumber:
		if _, ok := val.(float64); !ok {
			return fmt.Errorf("protoval: %s.%s: want number, got %T", schema.Name, f.Name, val)
		}
	case FieldBool:
		if _, ok := val.(bool); !ok {
			return fmt.Errorf("protoval: %s.%s: want bool, got %T", schema.Name, f.Name, val)
		}
	case FieldMessage:
		nested, ok := val.(map[string]any)
		if !ok {
			return fmt.Errorf("protoval: %s.%s: want message, got %T", schema.Name, f.Name, val)
		}
		if err := validateMessage(f.Message, nested, depth+1, maxDepth); err != nil {
			return err
		}
	case FieldRepeatedMessage:
		list, ok := val.([]any)
		if !ok {
			return fmt.Errorf("protoval: %s.%s: want repeated message, got %T", schema.Name, f.Name, val)
		}
		for i, elem := range list {
			item, ok := elem.(map[string]any)
			if !ok {
				return fmt.Errorf("protoval: %s.%s[%d]: want message, got %T", schema.Name, f.Name, i, elem)
			}
			if err := validateMessage(f.Message, item, depth+1, maxDepth); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("protoval: %s.%s: unknown field type %v", schema.Name, f.Name, f.Type)
	}
	return nil
}
```

### The runnable demo

The demo defines a self-referential `Comment` schema, validates a
realistic two-level comment tree, then builds a 200-level reply chain
simulating an attacker-controlled payload and shows it rejected.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/protoval"
)

func main() {
	// A self-referential schema: a Comment can have replies, each of
	// which is itself a Comment. Real .proto files define exactly this
	// shape for threaded comments, file trees, org charts, and the like.
	comment := &protoval.MessageSchema{Name: "Comment"}
	comment.Fields = []protoval.FieldSchema{
		{Name: "author", Type: protoval.FieldString, Required: true},
		{Name: "text", Type: protoval.FieldString, Required: true},
		{Name: "replies", Type: protoval.FieldRepeatedMessage, Message: comment},
	}

	good := map[string]any{
		"author": "alice",
		"text":   "great post",
		"replies": []any{
			map[string]any{"author": "bob", "text": "agreed", "replies": []any{}},
		},
	}
	if err := protoval.Validate(comment, good, 5); err != nil {
		panic(err)
	}
	fmt.Println("well-formed comment tree: valid")

	// Simulate an attacker-controlled payload: a chain of 200 nested
	// replies, each with exactly one child, far deeper than any real
	// thread and far past the configured limit.
	var bomb map[string]any
	for i := 0; i < 200; i++ {
		next := map[string]any{"author": "x", "text": "y"}
		if bomb != nil {
			next["replies"] = []any{bomb}
		}
		bomb = next
	}
	err := protoval.Validate(comment, bomb, 50)
	fmt.Println("200-deep reply chain result:", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
well-formed comment tree: valid
200-deep reply chain result: protoval: message nesting exceeds maximum depth: message Comment at depth 51
```

### Tests

`TestValidateAcceptsWellFormedMessage` checks the happy path.
`TestValidateRejectsMissingRequiredField`, `TestValidateRejectsWrongFieldType`,
`TestValidateRejectsNonListRepeatedField`, and
`TestValidateRejectsBadRepeatedElement` form the table of malformed-shape
cases. `TestValidateRejectsInvalidMaxDepth` covers input validation.
`TestValidateRejectsDeepReplyChain` is the test this exercise exists for:
against the same self-referential schema the demo uses, a 200-level
payload must be rejected via `ErrMaxDepthExceeded`, not by exhausting the
validator's own recursion.

Create `protoval_test.go`:

```go
package protoval

import (
	"errors"
	"testing"
)

func commentSchema() *MessageSchema {
	comment := &MessageSchema{Name: "Comment"}
	comment.Fields = []FieldSchema{
		{Name: "author", Type: FieldString, Required: true},
		{Name: "text", Type: FieldString, Required: true},
		{Name: "score", Type: FieldNumber},
		{Name: "replies", Type: FieldRepeatedMessage, Message: comment},
	}
	return comment
}

func TestValidateAcceptsWellFormedMessage(t *testing.T) {
	t.Parallel()

	msg := map[string]any{
		"author": "alice",
		"text":   "great post",
		"score":  float64(5),
		"replies": []any{
			map[string]any{"author": "bob", "text": "agreed"},
		},
	}
	if err := Validate(commentSchema(), msg, 5); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestValidateRejectsMissingRequiredField(t *testing.T) {
	t.Parallel()

	msg := map[string]any{"text": "no author here"}
	if err := Validate(commentSchema(), msg, 5); err == nil {
		t.Fatal("expected error for missing required field")
	}
}

func TestValidateRejectsWrongFieldType(t *testing.T) {
	t.Parallel()

	msg := map[string]any{"author": "alice", "text": "hi", "score": "not-a-number"}
	if err := Validate(commentSchema(), msg, 5); err == nil {
		t.Fatal("expected error for wrong field type")
	}
}

func TestValidateRejectsNonListRepeatedField(t *testing.T) {
	t.Parallel()

	msg := map[string]any{"author": "alice", "text": "hi", "replies": "not-a-list"}
	if err := Validate(commentSchema(), msg, 5); err == nil {
		t.Fatal("expected error for non-list repeated field")
	}
}

func TestValidateRejectsBadRepeatedElement(t *testing.T) {
	t.Parallel()

	msg := map[string]any{
		"author":  "alice",
		"text":    "hi",
		"replies": []any{"not-a-message"},
	}
	if err := Validate(commentSchema(), msg, 5); err == nil {
		t.Fatal("expected error for a repeated element that is not a message")
	}
}

func TestValidateRejectsInvalidMaxDepth(t *testing.T) {
	t.Parallel()

	msg := map[string]any{"author": "a", "text": "b"}
	if err := Validate(commentSchema(), msg, 0); err == nil {
		t.Fatal("expected error for maxDepth < 1")
	}
}

// TestValidateRejectsDeepReplyChain is the test this exercise exists for:
// a self-referential schema fed a payload nested far deeper than any real
// comment thread must be rejected via the depth guard, not by exhausting
// the validator's own call stack.
func TestValidateRejectsDeepReplyChain(t *testing.T) {
	t.Parallel()

	var bomb map[string]any
	for i := 0; i < 200; i++ {
		next := map[string]any{"author": "x", "text": "y"}
		if bomb != nil {
			next["replies"] = []any{bomb}
		}
		bomb = next
	}

	err := Validate(commentSchema(), bomb, 50)
	if !errors.Is(err, ErrMaxDepthExceeded) {
		t.Fatalf("Validate() error = %v, want %v", err, ErrMaxDepthExceeded)
	}
}
```

## Review

`Validate` is correct when it accepts any message shape that matches its
schema within the depth budget, and rejects both structurally malformed
messages (missing required fields, wrong types, malformed repeated
fields) and over-deep ones at the exact level the budget was crossed.
`TestValidateRejectsDeepReplyChain` is the test that would fail — with a
stack-overflow panic instead of a clean error — on a version of this
exercise that treats the schema's self-reference as something to special-case
or forbid, rather than simply bounding the recursion depth of whatever
payload arrives. The mistake this exercise targets is conflating "the
schema type is recursive" with "recursion is unsafe here"; the schema
being self-referential is not the risk, an unbounded *payload* is, and
the fix is the same one-line depth check this package already needs for
any deeply nested message, self-referential schema or not.

## Resources

- [Protocol Buffers: Language Guide (proto3), message and repeated fields](https://protobuf.dev/programming-guides/proto3/)
- [protobuf's JSON mapping (protojson): how messages become JSON-like values](https://protobuf.dev/programming-guides/json/)
- [Go Specification: Function declarations](https://go.dev/ref/spec#Function_declarations)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [31-invoice-tree-line-item-aggregation.md](31-invoice-tree-line-item-aggregation.md) | Next: [33-database-schema-fk-cycle-detection.md](33-database-schema-fk-cycle-detection.md)
