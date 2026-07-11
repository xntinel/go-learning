# Exercise 2: A CI breaking-change gate over the schema

Guard the order schema against incompatible edits. You will pick the right
breaking-change category for a Connect/JSON API, wire `buf breaking` into CI so it
fails closed, and walk a safe evolution (append a field, delete one the right way)
next to an unsafe one (change a type, delete without reserving, rename) so you can
see exactly which rule fires and why. A Go round-trip demo proves that the *safe*
evolution really is wire-compatible.

This module is self-contained. The `.proto` edits and `buf` commands are the core
of the exercise; the Go demo and test (behind `//go:build bufgen`) prove the
wire-compatibility claim against the generated types. The offline gate checks that
the Go sources are `gofmt`-clean; the `buf breaking` outcomes are the reproducible
proof of the gate.

## What you'll build

```text
ordergate/                           buf module + Go module: example.com/ordergate
  go.mod                             go 1.26
  buf.yaml                           version v2; breaking use WIRE_JSON
  buf.gen.yaml                       managed mode; local go plugin
  proto/acme/order/v1/order.proto    baseline schema (the "main" version)
  scripts/breaking-check.sh          runs buf breaking against main; fails closed
  .github/workflows/buf.yml          CI step invoking the same check
  gen/acme/order/v1/order.pb.go      generated messages (after buf generate)
  wire.go                            //go:build bufgen: marshal/unmarshal helpers
  cmd/demo/main.go                   //go:build bufgen: old bytes -> new message
  wire_test.go                       //go:build bufgen: round-trip compatibility
```

- Files: `buf.yaml`, `proto/acme/order/v1/order.proto`, `scripts/breaking-check.sh`, `.github/workflows/buf.yml`, `wire.go`, `cmd/demo/main.go`, `wire_test.go`.
- Implement: `breaking.use: [WIRE_JSON]`; a safe evolution and an unsafe evolution of `order.proto`; a `bufgen`-tagged Go helper that marshals an order and unmarshals it into the evolved message.
- Test: a table-driven round-trip test asserting old fields survive and an appended field defaults to zero.
- Verify: `buf breaking --against '.git#branch=main'` (empty for the safe diff, one line per violation for the unsafe one), then `go test -tags bufgen ./...`.

### Choosing the category for a Connect/JSON API

`buf breaking` compares the working tree to a baseline and fails on incompatible
edits, but only for the rules in the categories you enable. The order API is
served over Connect, which speaks both protobuf binary and JSON, so field *names*
are part of the contract, not just tag numbers. That makes `WIRE_JSON` the correct
compatibility floor: it catches binary breaks (type changes) and JSON breaks
(renames) while still allowing source-level refactors that `FILE`/`PACKAGE` would
block. `WIRE` alone would miss the rename; `FILE` would over-constrain. Set it in
`buf.yaml`.

Create `buf.yaml`:

```yaml
version: v2
modules:
  - path: proto
    name: buf.build/acme/order
lint:
  use:
    - STANDARD
breaking:
  use:
    - WIRE_JSON
  ignore_unstable_packages: true
```

`ignore_unstable_packages: true` exempts `v1alpha1`/`v1beta1` packages while the
stable `acme.order.v1` package stays locked — pre-1.0 you iterate freely, and the
moment a package is `v1` the gate protects it.

### The baseline schema (what lives on main)

This is the version `buf breaking --against '.git#branch=main'` compares against.
It carries a `note` field at tag 5 that later evolutions will remove, so the
delete-safely vs delete-unsafely contrast is concrete.

Create `proto/acme/order/v1/order.proto`:

```proto
syntax = "proto3";

package acme.order.v1;

enum OrderStatus {
  ORDER_STATUS_UNSPECIFIED = 0;
  ORDER_STATUS_PENDING = 1;
  ORDER_STATUS_SHIPPED = 2;
  ORDER_STATUS_DELIVERED = 3;
  ORDER_STATUS_CANCELLED = 4;
}

message Order {
  string id = 1;
  string customer_id = 2;
  uint32 quantity = 3;
  OrderStatus status = 4;
  string note = 5;
}
```

### The SAFE evolution

Two safe edits: append a brand-new field at a fresh tag number, and delete `note`
the right way by reserving both its number and its name. Reserving is what makes
the delete safe — the compiler will reject any future attempt to re-introduce
number 5 or the name `note`, so a later edit cannot silently rebind the number to a
different type. Under `WIRE_JSON`, appending a field and deleting a name-reserved
field are both compatible, so `buf breaking` passes.

This is the SAFE version of `proto/acme/order/v1/order.proto` (edit the file to
this on your feature branch):

```proto
syntax = "proto3";

package acme.order.v1;

enum OrderStatus {
  ORDER_STATUS_UNSPECIFIED = 0;
  ORDER_STATUS_PENDING = 1;
  ORDER_STATUS_SHIPPED = 2;
  ORDER_STATUS_DELIVERED = 3;
  ORDER_STATUS_CANCELLED = 4;
}

message Order {
  string id = 1;
  string customer_id = 2;
  uint32 quantity = 3;
  OrderStatus status = 4;

  reserved 5;
  reserved "note";

  uint32 priority = 6;
}
```

Against `main`, `buf breaking` prints nothing and exits 0:

```text
$ buf breaking --against '.git#branch=main'
$ echo $?
0
```

### The UNSAFE evolution

Now the wrong way, three violations at once, each a distinct failure mode:

1. `quantity` changes type `uint32` to `string` — a binary and JSON break.
2. `customer_id` is renamed to `buyer_id` at the same tag 2 — the wire binary is
   unaffected but every JSON key changes, so JSON clients break.
3. `note` (tag 5) is deleted with no `reserved` — a future edit can now rebind 5
   to a different type and mis-decode every stored payload that still carries a 5.

This is the UNSAFE version of `proto/acme/order/v1/order.proto` (do NOT keep this;
it exists to see the gate fire):

```proto
syntax = "proto3";

package acme.order.v1;

enum OrderStatus {
  ORDER_STATUS_UNSPECIFIED = 0;
  ORDER_STATUS_PENDING = 1;
  ORDER_STATUS_SHIPPED = 2;
  ORDER_STATUS_DELIVERED = 3;
  ORDER_STATUS_CANCELLED = 4;
}

message Order {
  string id = 1;
  string buyer_id = 2;
  string quantity = 3;
  OrderStatus status = 4;
}
```

Against `main`, `buf breaking` fails with one line per violation, in the
`path:line:col:message` form buf uses. The exact rule that fires is named in the
message, and each is a `WIRE_JSON` rule:

```text
$ buf breaking --against '.git#branch=main'
proto/acme/order/v1/order.proto:13:1:Previously present field "5" with name "note" on message "Order" was deleted without reserving the name "note".
proto/acme/order/v1/order.proto:15:3:Field "2" on message "Order" changed name from "customer_id" to "buyer_id".
proto/acme/order/v1/order.proto:16:3:Field "3" with name "quantity" on message "Order" changed type from "uint32" to "string".
$ echo $?
100
```

buf prints one annotation per violation, sorted by file then ascending line and
column, so the rules appear in this order: `FIELD_NO_DELETE_UNLESS_NAME_RESERVED`
(the un-reserved delete at 13:1), `FIELD_SAME_NAME` (the rename at 15:3), and
`FIELD_WIRE_JSON_COMPATIBLE_TYPE` (the type change at 16:3). A nonzero exit is
what "fail closed" depends on.

### The CI wiring

The gate must fail the build on any violation. A thin script keeps the exact
invocation in one place, and the workflow calls it. `buf breaking` exits nonzero
on any violation, so the script needs no extra logic to fail — but running under
`set -euo pipefail` makes the intent explicit and catches setup errors too.

Create `scripts/breaking-check.sh`:

```bash
#!/usr/bin/env bash
set -euo pipefail

# Compare the working tree against the schema as it exists on main (the merge
# base). buf exits nonzero on any WIRE_JSON-incompatible edit, failing the build.
buf breaking --against '.git#branch=main'
```

Create `.github/workflows/buf.yml`:

```yaml
name: buf
on:
  pull_request:
    paths:
      - "proto/**"
      - "buf.yaml"
jobs:
  breaking:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          fetch-depth: 0 # buf needs history to resolve .git#branch=main
      - uses: bufbuild/buf-action@v1
        with:
          breaking: true
          breaking_against: ".git#branch=main"
```

### Proving the safe evolution is wire-compatible

The breaking gate asserts compatibility statically; a Go round-trip demonstrates
it dynamically. The helper marshals an `Order` as an *old* client would (no
`priority` field, populated `note` reserved away — here we simulate the old shape
by setting only the surviving fields) and unmarshals the bytes into the evolved
`Order` type. Old fields survive and the appended `priority` decodes to its zero
value, which is exactly what "appending a field is safe" means on the wire.

The Go here uses the standard `google.golang.org/protobuf/proto` package for
`Marshal`/`Unmarshal`, and the generated `Order` from the SAFE schema. It is
behind `//go:build bufgen` because it needs the generated `gen` package.

Create `wire.go`:

```go
//go:build bufgen

// Package ordergate demonstrates that the safe schema evolution round-trips.
package ordergate

import (
	"fmt"

	orderv1 "example.com/ordergate/gen/acme/order/v1"
	"google.golang.org/protobuf/proto"
)

// RoundTrip marshals src and unmarshals the bytes into a fresh Order, modeling
// an old producer and a new consumer of the same wire payload.
func RoundTrip(src *orderv1.Order) (*orderv1.Order, error) {
	wire, err := proto.Marshal(src)
	if err != nil {
		return nil, fmt.Errorf("marshal order: %w", err)
	}
	dst := &orderv1.Order{}
	if err := proto.Unmarshal(wire, dst); err != nil {
		return nil, fmt.Errorf("unmarshal order: %w", err)
	}
	return dst, nil
}
```

Create `cmd/demo/main.go`:

```go
//go:build bufgen

package main

import (
	"fmt"
	"log"

	"example.com/ordergate"
	orderv1 "example.com/ordergate/gen/acme/order/v1"
)

func main() {
	// An order produced before the "priority" field existed.
	old := &orderv1.Order{
		Id:         "ord-7",
		CustomerId: "cust-9",
		Quantity:   2,
		Status:     orderv1.OrderStatus_ORDER_STATUS_SHIPPED,
	}

	got, err := ordergate.RoundTrip(old)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("id=%s customer=%s qty=%d status=%s priority=%d\n",
		got.GetId(), got.GetCustomerId(), got.GetQuantity(),
		got.GetStatus(), got.GetPriority())
}
```

Run it (after `buf generate` on the SAFE schema):

```bash
go run -tags bufgen ./cmd/demo
```

Expected output:

```
id=ord-7 customer=cust-9 qty=2 status=ORDER_STATUS_SHIPPED priority=0
```

`priority=0` is the whole point: the new consumer reads old bytes and the
appended field is simply absent (zero), so the evolution is transparent.

### The test

The test asserts the round-trip preserves every field an old payload carried and
that a set `priority` also survives a round-trip within the new schema — covering
both "old to new" and "new to new."

Create `wire_test.go`:

```go
//go:build bufgen

package ordergate

import (
	"testing"

	orderv1 "example.com/ordergate/gen/acme/order/v1"
)

func TestRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   *orderv1.Order
	}{
		{
			name: "old payload without priority",
			in: &orderv1.Order{
				Id:         "ord-1",
				CustomerId: "cust-1",
				Quantity:   4,
				Status:     orderv1.OrderStatus_ORDER_STATUS_PENDING,
			},
		},
		{
			name: "new payload with priority",
			in: &orderv1.Order{
				Id:         "ord-2",
				CustomerId: "cust-2",
				Quantity:   9,
				Status:     orderv1.OrderStatus_ORDER_STATUS_DELIVERED,
				Priority:   5,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := RoundTrip(tc.in)
			if err != nil {
				t.Fatalf("RoundTrip: %v", err)
			}
			if got.GetId() != tc.in.GetId() {
				t.Errorf("Id = %q, want %q", got.GetId(), tc.in.GetId())
			}
			if got.GetCustomerId() != tc.in.GetCustomerId() {
				t.Errorf("CustomerId = %q, want %q", got.GetCustomerId(), tc.in.GetCustomerId())
			}
			if got.GetQuantity() != tc.in.GetQuantity() {
				t.Errorf("Quantity = %d, want %d", got.GetQuantity(), tc.in.GetQuantity())
			}
			if got.GetPriority() != tc.in.GetPriority() {
				t.Errorf("Priority = %d, want %d", got.GetPriority(), tc.in.GetPriority())
			}
		})
	}
}
```

## Review

The gate is correct when the safe diff produces empty `buf breaking` output and a
zero exit, and the unsafe diff produces one line per violation and a nonzero exit
(buf uses exit code 100 for detected breakages). The most consequential decision
is the category: `WIRE_JSON` is the floor for a Connect/JSON API because it is the
only tier that catches the `customer_id` to `buyer_id` rename — under `WIRE` that
rename is invisible and would ship a JSON-breaking change green. The most
dangerous edit the gate blocks is the un-reserved delete of tag 5: without
`reserved 5; reserved "note";`, a later change can rebind 5 to a new type and
silently corrupt every stored payload. The Go round-trip is the dynamic
counterpart to the static check: it shows the appended `priority` field decoding to
zero from old bytes, which is what makes appending safe. This is a bar-mode
lesson, so the offline gate only proves the Go sources are `gofmt`-clean; the
compatibility proof is the documented `buf breaking` outcomes plus the
`bufgen`-tagged round-trip.

## Resources

- [buf breaking-change rules and categories](https://buf.build/docs/breaking/rules/) — which rules live in FILE, PACKAGE, WIRE_JSON, and WIRE.
- [buf breaking overview](https://buf.build/docs/breaking/overview/) — comparing against `.git#branch=main` and exit behavior.
- [bufbuild/buf-action](https://github.com/bufbuild/buf-action) — the GitHub Action that runs lint and breaking checks in CI.
- [Protobuf: updating a message type](https://protobuf.dev/programming-guides/proto3/#updating) — the wire-compatibility rules the gate enforces.

---

Prev: [01-schema-module-and-codegen.md](01-schema-module-and-codegen.md) | Back to [00-concepts.md](00-concepts.md) | Next: [03-protovalidate-constraints.md](03-protovalidate-constraints.md)
