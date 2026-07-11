# Exercise 3: Schema-embedded validation with protovalidate

This is the on-the-job exercise: move request validation out of hand-written Go
and into the schema. You will declare field and message constraints with
`buf.validate`, add the `protovalidate` dependency to the module, and enforce the
exact same contract at runtime with `buf.build/go/protovalidate`, mapping a
validation failure to a Connect `InvalidArgument` error. The validation rules now
live in one place — the schema — and every language that compiles it sees them.

This module is self-contained. The Go validator and tests sit behind
`//go:build bufgen` because they import the generated types and the
`protovalidate` runtime. The offline gate proves the sources are `gofmt`-clean;
the reproducible proof is `buf dep update`, `buf generate`, and `go test -tags
bufgen`.

## What you'll build

```text
ordervalidate/                       buf module + Go module: example.com/ordervalidate
  go.mod                             go 1.26
  buf.yaml                           version v2; deps: buf.build/bufbuild/protovalidate
  buf.lock                           dependency digests (written by buf dep update)
  buf.gen.yaml                       managed mode; local go plugin
  proto/acme/order/v1/order.proto    CreateOrderRequest with buf.validate constraints
  gen/acme/order/v1/order.pb.go      generated messages (after buf generate)
  validate.go                        //go:build bufgen: Validator built once; Connect mapping
  cmd/demo/main.go                   //go:build bufgen: rejects an invalid request
  validate_test.go                   //go:build bufgen: valid + each invalid field
```

- Files: `buf.yaml`, `buf.gen.yaml`, `proto/acme/order/v1/order.proto`, `validate.go`, `cmd/demo/main.go`, `validate_test.go`.
- Implement: `buf.validate.field` constraints (`string.uuid`, `string.email`, `uint32.gte`/`lte`, `string.max_len`) plus one message-level CEL rule; a Go `Service` that builds a `protovalidate.Validator` once and maps `ValidationError` to `connect.CodeInvalidArgument`.
- Test: a table-driven test exercising a valid request and each invalid field, asserting the violated rule id and the mapped Connect code.
- Verify: `buf dep update && buf generate && go mod tidy`, then `go test -tags bufgen ./...`.

### Constraints belong in the schema

Hand-written validation drifts: every service re-implements "email must be an
email, quantity must be 1..100," and the rules diverge. `protovalidate` puts the
rules in the schema as options on the fields, so the contract is defined once and
enforced identically wherever it is compiled. Unlike the old
`protoc-gen-validate`, `protovalidate` generates no `Validate()` method — it
interprets the constraints at runtime from the compiled descriptors (the
expressions are CEL underneath), so there is nothing generated to drift and one
validator serves every message type.

The request carries five constraints. `customer_id` must be a UUID
(`string.uuid`); `email` must be a well-formed email (`string.email`); `quantity`
must be in `[1, 100]` (`uint32.gte` and `uint32.lte`); `note` is capped at 280
characters (`string.max_len`). The message-level CEL rule expresses a
cross-field invariant that no single-field rule can: a high-volume order (quantity
>= 50) must carry an email. CEL rules are the escape hatch for constraints that
span fields.

Create `proto/acme/order/v1/order.proto`:

```proto
syntax = "proto3";

package acme.order.v1;

import "buf/validate/validate.proto";

// CreateOrderRequest is validated at runtime by protovalidate against the
// constraints declared inline below.
message CreateOrderRequest {
  string customer_id = 1 [(buf.validate.field).string.uuid = true];

  string email = 2 [(buf.validate.field).string.email = true];

  uint32 quantity = 3 [
    (buf.validate.field).uint32.gte = 1,
    (buf.validate.field).uint32.lte = 100
  ];

  string note = 4 [(buf.validate.field).string.max_len = 280];

  option (buf.validate.message).cel = {
    id: "quantity_requires_email"
    message: "orders of 50 or more require an email address"
    expression: "this.quantity < 50 || this.email != ''"
  };
}
```

### Declaring the dependency

The `buf.validate` extensions come from a BSR module. Declare it under `deps` in
`buf.yaml`, then run `buf dep update` to resolve it and pin its digest in
`buf.lock` (this is the v2 command; the old `buf mod update` is gone).

Create `buf.yaml`:

```yaml
version: v2
modules:
  - path: proto
deps:
  - buf.build/bufbuild/protovalidate
lint:
  use:
    - STANDARD
breaking:
  use:
    - WIRE_JSON
```

Create `buf.gen.yaml`:

```yaml
version: v2
clean: true
managed:
  enabled: true
  override:
    - file_option: go_package_prefix
      value: example.com/ordervalidate/gen
plugins:
  - local: protoc-gen-go
    out: gen
    opt:
      - paths=source_relative
inputs:
  - directory: proto
```

Resolve the dependency and generate:

```bash
buf dep update      # writes buf.lock with the pinned digest
buf lint
buf generate
go get buf.build/go/protovalidate connectrpc.com/connect google.golang.org/protobuf
go mod tidy
```

### The validator: build once, reuse

`protovalidate.New()` returns a `protovalidate.Validator` (an interface value,
safe for concurrent use) and an error if the CEL environment fails to build. Build
it exactly once at startup and reuse it for every request — constructing a
validator per request re-parses every constraint and is wasteful. `Validate`
returns `nil` when the message satisfies every constraint, and a
`*protovalidate.ValidationError` (whose `Violations` slice names each failed rule)
otherwise. `CheckCreateOrder` maps any validation failure to a Connect
`InvalidArgument`, the correct code for a client sending malformed input; because
`connect.NewError` wraps the underlying error and `*connect.Error` implements
`Unwrap`, callers can still recover the `ValidationError` with `errors.As`.

Create `validate.go`:

```go
//go:build bufgen

// Package ordervalidate enforces the schema's buf.validate constraints at runtime.
package ordervalidate

import (
	"fmt"

	"buf.build/go/protovalidate"
	"connectrpc.com/connect"
	orderv1 "example.com/ordervalidate/gen/acme/order/v1"
)

// Service validates CreateOrder requests against the schema constraints.
type Service struct {
	validator protovalidate.Validator
}

// NewService builds the validator once; reuse the Service for the process life.
func NewService() (*Service, error) {
	v, err := protovalidate.New()
	if err != nil {
		return nil, fmt.Errorf("build validator: %w", err)
	}
	return &Service{validator: v}, nil
}

// ValidateCreateOrder returns the raw protovalidate error (a
// *protovalidate.ValidationError) when a constraint is violated, else nil.
func (s *Service) ValidateCreateOrder(req *orderv1.CreateOrderRequest) error {
	return s.validator.Validate(req)
}

// CheckCreateOrder maps a validation failure to a Connect InvalidArgument error.
func (s *Service) CheckCreateOrder(req *orderv1.CreateOrderRequest) error {
	if err := s.ValidateCreateOrder(req); err != nil {
		return connect.NewError(connect.CodeInvalidArgument, err)
	}
	return nil
}
```

### The demo

The demo builds the service once, submits a request that violates two rules (a
malformed email and a zero quantity), prints the mapped Connect code, and recovers
the underlying `ValidationError` to print the first violated rule id. `errors.As`
walks through the Connect error's `Unwrap` to reach the `ValidationError`.

Create `cmd/demo/main.go`:

```go
//go:build bufgen

package main

import (
	"errors"
	"fmt"
	"log"

	"buf.build/go/protovalidate"
	"connectrpc.com/connect"
	"example.com/ordervalidate"
	orderv1 "example.com/ordervalidate/gen/acme/order/v1"
)

func main() {
	svc, err := ordervalidate.NewService()
	if err != nil {
		log.Fatal(err)
	}

	bad := &orderv1.CreateOrderRequest{
		CustomerId: "550e8400-e29b-41d4-a716-446655440000",
		Email:      "not-an-email",
		Quantity:   0,
	}

	err = svc.CheckCreateOrder(bad)
	fmt.Println("connect code:", connect.CodeOf(err))

	var valErr *protovalidate.ValidationError
	if errors.As(err, &valErr) {
		fmt.Println("violations:", len(valErr.Violations))
		fmt.Println("first rule:", valErr.Violations[0].Proto.GetRuleId())
	}
}
```

Run it (after `buf generate`):

```bash
go run -tags bufgen ./cmd/demo
```

Expected output:

```
connect code: invalid_argument
violations: 2
first rule: string.email
```

The two violations are the malformed `email` (rule `string.email`) and the
below-minimum `quantity` (rule `uint32.gte`); protovalidate reports violations in
field-declaration order, so `string.email` is first.

### The test

The test builds one `Service` and drives a table: a fully valid request (no
error), then one request per broken rule, asserting both that a violation occurs
and which rule id fired. A second test confirms the Connect mapping yields
`CodeInvalidArgument` for a bad request and `nil` for a good one. `hasRule` scans
the `ValidationError.Violations` for the expected rule id, since protovalidate may
report multiple violations and their order is only guaranteed to follow field
declaration.

Create `validate_test.go`:

```go
//go:build bufgen

package ordervalidate

import (
	"errors"
	"testing"

	"buf.build/go/protovalidate"
	"connectrpc.com/connect"
	orderv1 "example.com/ordervalidate/gen/acme/order/v1"
)

const validUUID = "550e8400-e29b-41d4-a716-446655440000"

func validRequest() *orderv1.CreateOrderRequest {
	return &orderv1.CreateOrderRequest{
		CustomerId: validUUID,
		Email:      "buyer@example.com",
		Quantity:   3,
		Note:       "gift wrap please",
	}
}

func hasRule(err error, ruleID string) bool {
	var valErr *protovalidate.ValidationError
	if !errors.As(err, &valErr) {
		return false
	}
	for _, v := range valErr.Violations {
		if v.Proto.GetRuleId() == ruleID {
			return true
		}
	}
	return false
}

func TestValidateCreateOrder(t *testing.T) {
	t.Parallel()

	svc, err := NewService()
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	tests := []struct {
		name     string
		mutate   func(*orderv1.CreateOrderRequest)
		wantRule string // empty means the request must be valid
	}{
		{name: "valid", mutate: func(*orderv1.CreateOrderRequest) {}, wantRule: ""},
		{
			name:     "bad uuid",
			mutate:   func(r *orderv1.CreateOrderRequest) { r.CustomerId = "not-a-uuid" },
			wantRule: "string.uuid",
		},
		{
			name:     "bad email",
			mutate:   func(r *orderv1.CreateOrderRequest) { r.Email = "nope" },
			wantRule: "string.email",
		},
		{
			name:     "quantity too low",
			mutate:   func(r *orderv1.CreateOrderRequest) { r.Quantity = 0 },
			wantRule: "uint32.gte",
		},
		{
			name:     "quantity too high",
			mutate:   func(r *orderv1.CreateOrderRequest) { r.Quantity = 1000 },
			wantRule: "uint32.lte",
		},
		{
			name:     "bulk order without email",
			mutate:   func(r *orderv1.CreateOrderRequest) { r.Quantity = 80; r.Email = "" },
			wantRule: "quantity_requires_email",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := validRequest()
			tc.mutate(req)
			err := svc.ValidateCreateOrder(req)
			if tc.wantRule == "" {
				if err != nil {
					t.Fatalf("ValidateCreateOrder(valid) = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateCreateOrder = nil, want violation %q", tc.wantRule)
			}
			if !hasRule(err, tc.wantRule) {
				t.Errorf("violations do not include rule %q: %v", tc.wantRule, err)
			}
		})
	}
}

func TestCheckCreateOrderConnectCode(t *testing.T) {
	t.Parallel()

	svc, err := NewService()
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	if err := svc.CheckCreateOrder(validRequest()); err != nil {
		t.Fatalf("CheckCreateOrder(valid) = %v, want nil", err)
	}

	bad := validRequest()
	bad.Email = "nope"
	err = svc.CheckCreateOrder(bad)
	if got := connect.CodeOf(err); got != connect.CodeInvalidArgument {
		t.Errorf("CodeOf = %v, want %v", got, connect.CodeInvalidArgument)
	}
}
```

## Review

The contract is correct when a valid request passes and each broken rule produces
a violation naming the expected rule id, and when the Connect mapping turns any
violation into `CodeInvalidArgument`. The design points to hold onto: build the
`Validator` once (`protovalidate.New`) and reuse it — it is concurrency-safe and
re-creating it per request re-parses every constraint. Import the current module,
`buf.build/go/protovalidate`, not the archived `github.com/bufbuild/protovalidate-go`.
Remember the two-layer model: `buf lint`/`buf breaking` check the schema at merge
time, but they do nothing for runtime data — the constraints are advisory until
this server actually calls `Validate`, which is precisely what `CheckCreateOrder`
does at the boundary. The message-level CEL rule is what lets you express the
cross-field invariant (bulk orders need an email) that no single-field constraint
can. Because this is a bar-mode lesson, the offline gate proves only `gofmt`
cleanliness; the runtime proof is `buf dep update`, `buf generate`, and `go test
-tags bufgen ./...`.

## Resources

- [protovalidate-go (buf.build/go/protovalidate)](https://github.com/bufbuild/protovalidate-go) — the current Go runtime: `New`, `Validate`, `ValidationError`.
- [protovalidate documentation](https://protovalidate.com/) — declaring `buf.validate` field and message constraints.
- [protovalidate standard rules](https://protovalidate.com/schemas/standard-rules/) — `string.uuid`, `string.email`, numeric `gte`/`lte`, `string.max_len`.
- [connectrpc.com/connect error codes](https://connectrpc.com/docs/go/errors/) — `NewError`, `CodeInvalidArgument`, and `CodeOf`.

---

Prev: [02-breaking-change-gate.md](02-breaking-change-gate.md) | Back to [00-concepts.md](00-concepts.md) | Next: [../04-gqlgen-graphql-server/00-concepts.md](../04-gqlgen-graphql-server/00-concepts.md)
