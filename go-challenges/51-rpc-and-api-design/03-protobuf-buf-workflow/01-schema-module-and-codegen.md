# Exercise 1: A versioned buf module with reproducible codegen

Build a self-contained `buf` v2 schema module for an order API and wire up
reproducible Go codegen with managed mode. The deliverable is a module that lints
clean under `STANDARD`, builds to a valid image, and generates Go that a small
program compiles against — the everyday shape of an API contract in a real repo.

This module is fully self-contained: its own `go mod init`, its own schema, its
own generated-code consumer, and its own tests. Nothing here imports another
exercise. Because codegen needs the `buf` toolchain and its plugins, the Go
consumer sits behind a `//go:build bufgen` constraint; the offline gate proves the
sources are `gofmt`-clean, and the `buf` commands below are the reproducible proof
of the codegen loop.

## What you'll build

```text
orderschema/                         buf module + Go module: example.com/orderschema
  go.mod                             go 1.26
  buf.yaml                           version v2; module at proto; lint STANDARD; breaking FILE
  buf.gen.yaml                       version v2; clean; managed mode; local go + connect-go plugins
  proto/
    acme/order/v1/order.proto        package acme.order.v1: OrderService, Order, OrderStatus enum
  gen/                               buf generate output (not committed by hand)
    acme/order/v1/order.pb.go        generated messages + OrderStatus
    acme/order/v1/orderv1connect/    generated Connect stubs
  order.go                           //go:build bufgen: builds generated types (proves compile)
  cmd/demo/main.go                   //go:build bufgen: constructs a request/response
  order_test.go                      //go:build bufgen: asserts generated types + service name
```

- Files: `buf.yaml`, `buf.gen.yaml`, `proto/acme/order/v1/order.proto`, `order.go`, `cmd/demo/main.go`, `order_test.go`.
- Implement: a versioned `acme.order.v1` package with an `OrderService`, an `Order` message, and an `OrderStatus` enum whose zero value is `ORDER_STATUS_UNSPECIFIED`; a v2 `buf.yaml` and a managed-mode `buf.gen.yaml`.
- Test: after `buf generate`, a `bufgen`-tagged test that constructs the generated request/response types and asserts the generated Connect service name.
- Verify: `buf format -w && buf lint && buf build` (all clean), then `buf generate && go build -tags bufgen ./... && go test -tags bufgen ./...`.

Set up the module:

```bash
mkdir -p go-solutions/51-rpc-and-api-design/03-protobuf-buf-workflow/01-schema-module-and-codegen/proto/acme/order/v1 go-solutions/51-rpc-and-api-design/03-protobuf-buf-workflow/01-schema-module-and-codegen/cmd/demo
cd go-solutions/51-rpc-and-api-design/03-protobuf-buf-workflow/01-schema-module-and-codegen
```

### The schema: designing acme.order.v1 to pass STANDARD

Every rule in the `STANDARD` lint category is a design decision made for you, so
the schema is written to satisfy them from the first line. The package is
`acme.order.v1` — the `v1` suffix (`PACKAGE_VERSION_SUFFIX`) makes the version
part of the type identity so a future `v2` can coexist, and the directory
`acme/order/v1/` matches the package (`PACKAGE_DIRECTORY_MATCH`). The enum's zero
value is `ORDER_STATUS_UNSPECIFIED = 0` (`ENUM_ZERO_VALUE_SUFFIX`) so an unset
status is distinguishable from a real state on the wire; real states start at 1.
The service is `OrderService` (`SERVICE_SUFFIX`), and each RPC has its own unique
request and response messages (`RPC_REQUEST_RESPONSE_UNIQUE`) so adding a field to
one RPC never perturbs another. Note there is no `go_package` option in the file:
managed mode supplies it, keeping the schema free of any consumer's import path.

Create `proto/acme/order/v1/order.proto`:

```proto
syntax = "proto3";

package acme.order.v1;

// OrderStatus enumerates the lifecycle states of an order. The zero value is
// UNSPECIFIED so an unset status is distinguishable from a real state.
enum OrderStatus {
  ORDER_STATUS_UNSPECIFIED = 0;
  ORDER_STATUS_PENDING = 1;
  ORDER_STATUS_SHIPPED = 2;
  ORDER_STATUS_DELIVERED = 3;
  ORDER_STATUS_CANCELLED = 4;
}

// Order is the core resource of the API.
message Order {
  string id = 1;
  string customer_id = 2;
  uint32 quantity = 3;
  OrderStatus status = 4;
}

message GetOrderRequest {
  string id = 1;
}

message GetOrderResponse {
  Order order = 1;
}

message CreateOrderRequest {
  string customer_id = 1;
  uint32 quantity = 2;
}

message CreateOrderResponse {
  Order order = 1;
}

// OrderService is the API surface. Each RPC has unique request/response types.
service OrderService {
  rpc GetOrder(GetOrderRequest) returns (GetOrderResponse);
  rpc CreateOrder(CreateOrderRequest) returns (CreateOrderResponse);
}
```

### buf.yaml: the module boundary and its policies

The v2 `buf.yaml` is a single file at the module root. `modules:` declares one
module rooted at `proto` with an optional BSR `name`. `lint.use: [STANDARD]`
selects the recommended category; the explicit `enum_zero_value_suffix` and
`service_suffix` keys document the conventions the schema already follows (they
match the defaults, and stating them is a form of executable documentation).
`breaking.use: [FILE]` is set here as the starting point; Exercise 2 revisits that
choice and explains why a Go-only backend usually wants `PACKAGE` and a JSON API
wants `WIRE_JSON`.

Create `buf.yaml`:

```yaml
version: v2
modules:
  - path: proto
    name: buf.build/acme/order
lint:
  use:
    - STANDARD
  enum_zero_value_suffix: _UNSPECIFIED
  service_suffix: Service
breaking:
  use:
    - FILE
```

### buf.gen.yaml: managed mode and reproducible codegen

`buf.gen.yaml` describes codegen declaratively. `clean: true` wipes the `gen`
directory before each run so a renamed or deleted message cannot leave an orphan
behind — generation is deterministic. Managed mode is enabled with a single
`go_package_prefix` override of `example.com/orderschema/gen`: `buf` computes each
file's `go_package` from that prefix plus its proto path, so
`acme/order/v1/order.proto` generates into `example.com/orderschema/gen/acme/order/v1`
without any `go_package` option in the schema. Two local plugins run:
`protoc-gen-go` for the message types and `protoc-gen-connect-go` for the Connect
service stubs, both with `paths=source_relative` so the output tree mirrors the
proto tree under `gen`. `inputs` points codegen at the `proto` directory.

Create `buf.gen.yaml`:

```yaml
version: v2
clean: true
managed:
  enabled: true
  override:
    - file_option: go_package_prefix
      value: example.com/orderschema/gen
plugins:
  - local: protoc-gen-go
    out: gen
    opt:
      - paths=source_relative
  - local: protoc-gen-connect-go
    out: gen
    opt:
      - paths=source_relative
inputs:
  - directory: proto
```

The local plugins must be on `PATH`. Install them once:

```bash
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
go install connectrpc.com/connect/cmd/protoc-gen-connect-go@latest
```

### The full loop

Run the loop in order. `buf format -w` rewrites the `.proto` files to canonical
form in place; `buf lint` must print nothing (clean); `buf build` compiles the
module into a validated in-memory image; `buf generate` writes the Go into `gen`.

```bash
buf format -w
buf lint
buf build
buf generate
go mod tidy
```

### The generated-code consumer

With `gen` populated, a small package proves the generated types compile and are
usable. It is behind `//go:build bufgen` so the offline gate — which has no `gen`
directory — skips it while still checking its formatting. `NewPendingOrder`
constructs an `Order` using the generated struct fields and the generated enum
constant `OrderStatus_ORDER_STATUS_PENDING`; the generated getters (`GetId`,
`GetStatus`, ...) are nil-safe accessors that `protoc-gen-go` emits for every
field.

Create `order.go`:

```go
//go:build bufgen

// Package orderschema demonstrates consuming the generated acme.order.v1 types.
package orderschema

import (
	orderv1 "example.com/orderschema/gen/acme/order/v1"
)

// NewPendingOrder builds a pending Order from the generated types, proving the
// buf-generated package compiles and is usable from ordinary Go.
func NewPendingOrder(id, customerID string, quantity uint32) *orderv1.Order {
	return &orderv1.Order{
		Id:         id,
		CustomerId: customerID,
		Quantity:   quantity,
		Status:     orderv1.OrderStatus_ORDER_STATUS_PENDING,
	}
}
```

### The demo

The demo constructs a `CreateOrderRequest`, derives a `CreateOrderResponse`
carrying a pending `Order`, and prints it. It touches only exported, generated API
plus the exported `NewPendingOrder`, exactly as a service handler would when
turning a request into a response.

Create `cmd/demo/main.go`:

```go
//go:build bufgen

package main

import (
	"fmt"

	"example.com/orderschema"
	orderv1 "example.com/orderschema/gen/acme/order/v1"
)

func main() {
	req := &orderv1.CreateOrderRequest{
		CustomerId: "cust-42",
		Quantity:   3,
	}

	order := orderschema.NewPendingOrder("ord-1001", req.GetCustomerId(), req.GetQuantity())
	resp := &orderv1.CreateOrderResponse{Order: order}

	fmt.Printf("created %s for %s x%d status=%s\n",
		resp.GetOrder().GetId(),
		resp.GetOrder().GetCustomerId(),
		resp.GetOrder().GetQuantity(),
		resp.GetOrder().GetStatus(),
	)
}
```

Run it (after `buf generate`):

```bash
go run -tags bufgen ./cmd/demo
```

Expected output:

```
created ord-1001 for cust-42 x3 status=ORDER_STATUS_PENDING
```

The status prints as `ORDER_STATUS_PENDING` because `protoc-gen-go` gives every
enum a `String()` method returning the proto value name, which `%s` invokes.

### The test

The test constructs the generated request/response types and asserts their
round-tripped field values, and it references the generated Connect package to
assert the service name constant `orderv1connect.OrderServiceName`, which
`protoc-gen-connect-go` emits as the fully qualified `acme.order.v1.OrderService`.
That single constant is proof that both plugins ran and that the package path
managed mode computed is the one the code imports. The test is `bufgen`-tagged for
the same reason as the consumer.

Create `order_test.go`:

```go
//go:build bufgen

package orderschema

import (
	"testing"

	orderv1 "example.com/orderschema/gen/acme/order/v1"
	"example.com/orderschema/gen/acme/order/v1/orderv1connect"
)

func TestNewPendingOrder(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		id         string
		customerID string
		quantity   uint32
	}{
		{name: "single", id: "ord-1", customerID: "cust-1", quantity: 1},
		{name: "bulk", id: "ord-2", customerID: "cust-2", quantity: 250},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := NewPendingOrder(tc.id, tc.customerID, tc.quantity)
			if got.GetId() != tc.id {
				t.Errorf("Id = %q, want %q", got.GetId(), tc.id)
			}
			if got.GetCustomerId() != tc.customerID {
				t.Errorf("CustomerId = %q, want %q", got.GetCustomerId(), tc.customerID)
			}
			if got.GetQuantity() != tc.quantity {
				t.Errorf("Quantity = %d, want %d", got.GetQuantity(), tc.quantity)
			}
			if got.GetStatus() != orderv1.OrderStatus_ORDER_STATUS_PENDING {
				t.Errorf("Status = %v, want ORDER_STATUS_PENDING", got.GetStatus())
			}
		})
	}
}

func TestGeneratedServiceName(t *testing.T) {
	t.Parallel()
	const want = "acme.order.v1.OrderService"
	if orderv1connect.OrderServiceName != want {
		t.Errorf("OrderServiceName = %q, want %q", orderv1connect.OrderServiceName, want)
	}
}
```

## Review

The module is correct when `buf lint` prints nothing, `buf build` succeeds, and a
freshly generated `gen` tree compiles under `go build -tags bufgen ./...`. The
subtle checks are the ones lint enforces: drop the `v1` from the package and
`PACKAGE_VERSION_SUFFIX` fails; set the enum zero value to a real state and
`ENUM_ZERO_VALUE_SUFFIX` fails; share one message across two RPCs and
`RPC_REQUEST_RESPONSE_UNIQUE` fails. The most common structural mistake is adding
a `go_package` option to the `.proto` to "fix" an import path — that reintroduces
the drift managed mode exists to prevent; the fix is always the
`go_package_prefix` override in `buf.gen.yaml`. The second is running `buf
generate` without `clean: true` and then wondering why a message you deleted still
compiles: the orphaned generated file is still on disk. Because this is a bar-mode
lesson, the offline gate proves only that the sources are `gofmt`-clean; the codegen
proof is the documented `buf` loop, and the `bufgen`-tagged consumer is what turns
"it generated" into "it compiles and is usable."

## Resources

- [buf.yaml v2 configuration reference](https://buf.build/docs/configuration/v2/buf-yaml/) — the module list, lint, and breaking keys used here.
- [buf lint rules and categories](https://buf.build/docs/lint/rules/) — what MINIMAL, BASIC, and STANDARD each enforce.
- [buf generate managed mode](https://buf.build/docs/generate/managed-mode/) — the `go_package_prefix` override and plugin configuration.
- [protoc-gen-connect-go](https://connectrpc.com/docs/go/getting-started/) — the Connect Go plugin and the generated service constants.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-breaking-change-gate.md](02-breaking-change-gate.md)
