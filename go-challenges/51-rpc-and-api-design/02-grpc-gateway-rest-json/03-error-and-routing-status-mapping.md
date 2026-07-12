# Exercise 3: Map gRPC status codes to HTTP with a stable error envelope

A public REST API needs a predictable error surface: the right HTTP status *and* a stable,
machine-readable body. This exercise installs a custom error handler that maps gRPC status
codes to HTTP status via `runtime.HTTPStatusFromCode` and serializes one JSON envelope, plus
a routing error handler so gateway-level failures (unknown path, wrong method) speak the
same shape.

## What you'll build

```text
errmap/                          module: example.com/errmap
  go.mod
  proto/order/v1/order.proto     annotated OrderService (go_package -> example.com/errmap/...)
  gen/order/v1/                  generated from the proto (orderpb: messages + Register*Handler* funcs)
  errors.go                      Envelope + custom ErrorHandlerFunc + RoutingErrorHandlerFunc
  cmd/demo/main.go               NotFound RPC, unknown path, wrong method -> one envelope
  errors_test.go                 code->status table (404/400/403/401) + routing 404/405
```

Files: `proto/order/v1/order.proto`, `errors.go`, `cmd/demo/main.go`, `errors_test.go`.
Implement: `NewGateway` installing `runtime.WithErrorHandler` and `runtime.WithRoutingErrorHandler`, both writing a stable `Envelope`.
Test: drive the mux with a fake that returns `status.Error(codes.X, ...)`, asserting the HTTP status equals `runtime.HTTPStatusFromCode(codes.X)` and the envelope carries the code; separately assert routing errors yield 404/405 in the same envelope.
Verify: `buf generate && go test -count=1 -race ./...` (bar mode: builds only after codegen and with the modules present).

Set up the module:

```bash
mkdir -p go-solutions/51-rpc-and-api-design/02-grpc-gateway-rest-json/03-error-and-routing-status-mapping/proto/order/v1 go-solutions/51-rpc-and-api-design/02-grpc-gateway-rest-json/03-error-and-routing-status-mapping/cmd/demo
cd go-solutions/51-rpc-and-api-design/02-grpc-gateway-rest-json/03-error-and-routing-status-mapping
go get github.com/grpc-ecosystem/grpc-gateway/v2/runtime
go get google.golang.org/grpc google.golang.org/protobuf
```

### The proto this module generates from

This module is standalone, so it carries its own copy of the annotated `OrderService`
proto — identical to Exercise 1 except for the `go_package` option, which must name *this*
module's import path (`example.com/errmap/gen/order/v1;orderpb`). Generating with
`buf generate` (using `protoc-gen-go`, `protoc-gen-go-grpc`, and `protoc-gen-grpc-gateway`)
produces the `orderpb` package in `gen/order/v1` that the code below imports. The annotation
grammar is explained in Exercise 1; here the proto exists so this module regenerates without
reaching back to another one.

Create `proto/order/v1/order.proto`:

```proto
syntax = "proto3";

package order.v1;

import "google/api/annotations.proto";
import "google/protobuf/empty.proto";
import "google/protobuf/field_mask.proto";

option go_package = "example.com/errmap/gen/order/v1;orderpb";

service OrderService {
  rpc GetOrder(GetOrderRequest) returns (Order) {
    option (google.api.http) = {
      get: "/v1/orders/{id}"
      additional_bindings {get: "/orders/{id}"}
    };
  }

  rpc ListOrders(ListOrdersRequest) returns (ListOrdersResponse) {
    option (google.api.http) = {get: "/v1/orders"};
  }

  rpc CreateOrder(CreateOrderRequest) returns (Order) {
    option (google.api.http) = {
      post: "/v1/orders"
      body: "*"
    };
  }

  rpc UpdateOrder(UpdateOrderRequest) returns (Order) {
    option (google.api.http) = {
      patch: "/v1/orders/{order.id}"
      body: "order"
    };
  }

  rpc DeleteOrder(DeleteOrderRequest) returns (google.protobuf.Empty) {
    option (google.api.http) = {delete: "/v1/orders/{id}"};
  }
}

message Order {
  string id = 1;
  string customer = 2;
  int64 amount_cents = 3;
  string state = 4;
}

message GetOrderRequest {
  string id = 1;
}

message ListOrdersRequest {
  int32 page_size = 1;
  string state = 2;
}

message ListOrdersResponse {
  repeated Order orders = 1;
}

message CreateOrderRequest {
  string customer = 1;
  int64 amount_cents = 2;
}

message UpdateOrderRequest {
  Order order = 1;
  google.protobuf.FieldMask update_mask = 2;
}

message DeleteOrderRequest {
  string id = 1;
}
```

### Why HTTP status alone is not enough

gRPC has a richer error model than HTTP: a `codes.Code`, a message, and optional typed
details. HTTP has a status line and whatever body you write. If you leak only the HTTP
status, clients cannot distinguish the many failures that all map to `400`, and if you leak
grpc-gateway's default error body, its shape is not one you have promised. So you do two
things deliberately: map the code to HTTP status with the *canonical* function
`runtime.HTTPStatusFromCode` (never a hand-rolled table that drifts), and serialize a stable
`Envelope` carrying the numeric code, the string code, the message, and any details. Both the
RPC path and the routing path write that one envelope.

`runtime.HTTPStatusFromCode` is the authority for the mapping: `NotFound → 404`,
`InvalidArgument → 400`, `PermissionDenied → 403`, `Unauthenticated → 401`,
`AlreadyExists → 409`, `Unavailable → 503`, and so on. `status.FromError` pulls the
`*status.Status` out of the error; when the error is not a gRPC status it returns a status
with `codes.Unknown` wrapping the message, so the handler always has something coherent to
serialize.

Create `errors.go`:

```go
package errmap

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	orderpb "example.com/errmap/gen/order/v1"
)

// Envelope is the stable JSON error shape every client can depend on, used for both
// RPC-level and gateway-level failures.
type Envelope struct {
	Code    int      `json:"code"`   // numeric gRPC code
	Status  string   `json:"status"` // string gRPC code, e.g. "NotFound"
	Message string   `json:"message"`
	Details []string `json:"details,omitempty"`
}

// NewGateway registers the in-process OrderService with a custom error handler and a
// routing error handler so every error leaves as the same envelope.
func NewGateway(ctx context.Context, srv orderpb.OrderServiceServer) (*runtime.ServeMux, error) {
	mux := runtime.NewServeMux(
		runtime.WithErrorHandler(errorHandler),
		runtime.WithRoutingErrorHandler(routingErrorHandler),
	)
	if err := orderpb.RegisterOrderServiceHandlerServer(ctx, mux, srv); err != nil {
		return nil, err
	}
	return mux, nil
}

func writeEnvelope(w http.ResponseWriter, m runtime.Marshaler, httpStatus int, env Envelope) {
	w.Header().Set("Content-Type", m.ContentType(env))
	w.WriteHeader(httpStatus)
	_ = json.NewEncoder(w).Encode(env)
}

// errorHandler maps an RPC-level gRPC status to its canonical HTTP status and writes the
// stable envelope instead of grpc-gateway's default error body.
func errorHandler(_ context.Context, _ *runtime.ServeMux, m runtime.Marshaler, w http.ResponseWriter, _ *http.Request, err error) {
	st, _ := status.FromError(err)
	env := Envelope{
		Code:    int(st.Code()),
		Status:  st.Code().String(),
		Message: st.Message(),
	}
	for _, d := range st.Details() {
		env.Details = append(env.Details, fmt.Sprintf("%v", d))
	}
	writeEnvelope(w, m, runtime.HTTPStatusFromCode(st.Code()), env)
}

// routingErrorHandler turns gateway-level failures (unknown path, wrong method) into the
// same envelope, translating the HTTP status back to a gRPC code first.
func routingErrorHandler(_ context.Context, _ *runtime.ServeMux, m runtime.Marshaler, w http.ResponseWriter, _ *http.Request, httpStatus int) {
	code := codes.Unknown
	switch httpStatus {
	case http.StatusNotFound:
		code = codes.NotFound
	case http.StatusMethodNotAllowed:
		code = codes.Unimplemented
	case http.StatusBadRequest:
		code = codes.InvalidArgument
	}
	writeEnvelope(w, m, httpStatus, Envelope{
		Code:    int(code),
		Status:  code.String(),
		Message: http.StatusText(httpStatus),
	})
}
```

### The runnable demo

The demo shows all three error paths through one gateway: an RPC that returns
`codes.NotFound`, an unknown path, and a wrong method against a known path. Each answers with
the same envelope shape, so a client parses errors one way.

Create `cmd/demo/main.go`:

```go
package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"

	"example.com/errmap"
	orderpb "example.com/errmap/gen/order/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type memServer struct {
	orderpb.UnimplementedOrderServiceServer
}

func (memServer) GetOrder(_ context.Context, _ *orderpb.GetOrderRequest) (*orderpb.Order, error) {
	return nil, status.Error(codes.NotFound, "order not found")
}

func main() {
	mux, err := errmap.NewGateway(context.Background(), memServer{})
	if err != nil {
		log.Fatal(err)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()

	do := func(method, path string) {
		req, _ := http.NewRequest(method, srv.URL+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			log.Fatal(err)
		}
		out, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		fmt.Printf("%s %s -> %d %s\n", method, path, resp.StatusCode, bytes.TrimSpace(out))
	}

	do(http.MethodGet, "/v1/orders/missing")
	do(http.MethodGet, "/nope")
	do(http.MethodPost, "/v1/orders/o-1")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
GET /v1/orders/missing -> 404 {"code":5,"status":"NotFound","message":"order not found"}
GET /nope -> 404 {"code":5,"status":"NotFound","message":"Not Found"}
POST /v1/orders/o-1 -> 405 {"code":12,"status":"Unimplemented","message":"Method Not Allowed"}
```

### Tests

The tests lock the public error contract. `TestRPCErrorMapping` drives a fake that returns a
different `codes.Code` per id and asserts the HTTP status equals `runtime.HTTPStatusFromCode`
for that code and the envelope carries the numeric and string code — so a future edit to the
handler cannot silently change the mapping. `TestRoutingErrors` hits an unknown path and a
wrong method and asserts `404`/`405` arrive in the *same* envelope, so clients never see two
error shapes.

Create `errors_test.go`:

```go
package errmap

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	orderpb "example.com/errmap/gen/order/v1"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type errServer struct {
	orderpb.UnimplementedOrderServiceServer
}

func (errServer) GetOrder(_ context.Context, req *orderpb.GetOrderRequest) (*orderpb.Order, error) {
	switch req.GetId() {
	case "missing":
		return nil, status.Error(codes.NotFound, "order not found")
	case "bad":
		return nil, status.Error(codes.InvalidArgument, "invalid id")
	case "forbidden":
		return nil, status.Error(codes.PermissionDenied, "no access")
	case "anon":
		return nil, status.Error(codes.Unauthenticated, "login required")
	default:
		return &orderpb.Order{Id: req.GetId()}, nil
	}
}

func newServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux, err := NewGateway(context.Background(), errServer{})
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func decode(t *testing.T, r *http.Response) Envelope {
	t.Helper()
	var env Envelope
	if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	r.Body.Close()
	return env
}

func TestRPCErrorMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		id         string
		code       codes.Code
		wantStatus string
	}{
		{name: "not found", id: "missing", code: codes.NotFound, wantStatus: "NotFound"},
		{name: "invalid argument", id: "bad", code: codes.InvalidArgument, wantStatus: "InvalidArgument"},
		{name: "permission denied", id: "forbidden", code: codes.PermissionDenied, wantStatus: "PermissionDenied"},
		{name: "unauthenticated", id: "anon", code: codes.Unauthenticated, wantStatus: "Unauthenticated"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newServer(t)
			resp, err := http.Get(srv.URL + "/v1/orders/" + tt.id)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			wantHTTP := runtime.HTTPStatusFromCode(tt.code)
			if resp.StatusCode != wantHTTP {
				t.Fatalf("status = %d; want %d (canonical for %s)", resp.StatusCode, wantHTTP, tt.code)
			}
			env := decode(t, resp)
			if env.Code != int(tt.code) {
				t.Fatalf("envelope code = %d; want %d", env.Code, int(tt.code))
			}
			if env.Status != tt.wantStatus {
				t.Fatalf("envelope status = %q; want %q", env.Status, tt.wantStatus)
			}
		})
	}
}

func TestRoutingErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		method   string
		path     string
		wantHTTP int
	}{
		{name: "unknown path", method: http.MethodGet, path: "/nope", wantHTTP: http.StatusNotFound},
		{name: "wrong method", method: http.MethodPost, path: "/v1/orders/o-1", wantHTTP: http.StatusMethodNotAllowed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newServer(t)
			req, err := http.NewRequest(tt.method, srv.URL+tt.path, nil)
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("do: %v", err)
			}
			if resp.StatusCode != tt.wantHTTP {
				t.Fatalf("status = %d; want %d", resp.StatusCode, tt.wantHTTP)
			}
			env := decode(t, resp)
			if env.Message == "" {
				t.Fatal("routing error envelope has empty message")
			}
		})
	}
}

func ExampleNewGateway() {
	mux, _ := NewGateway(context.Background(), errServer{})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, _ := http.Get(srv.URL + "/v1/orders/missing")
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	fmt.Printf("%d %s\n", resp.StatusCode, bytes.TrimSpace(out))
	// Output: 404 {"code":5,"status":"NotFound","message":"order not found"}
}
```

## Review

The error surface is correct when both failure paths produce one envelope and the status is
always the canonical mapping. `TestRPCErrorMapping` proves the code→status table by comparing
each response against `runtime.HTTPStatusFromCode(tt.code)` rather than a hard-coded number,
so the test tracks the canonical mapping instead of duplicating it. `TestRoutingErrors`
proves an unknown path and a wrong method yield `404`/`405` in the same shape. If a client
suddenly sees a different body for a 404, the routing error handler is missing; if a status
is wrong, someone hand-rolled the mapping instead of calling `HTTPStatusFromCode`.

The mistakes to avoid: shipping without a custom error handler, so clients depend on
grpc-gateway's default shape; forgetting `WithRoutingErrorHandler`, so gateway-level and
RPC-level errors diverge; and hand-writing the code→status table, which drifts from the
canonical one the moment a new code matters. As with the rest of this lesson, this is bar
mode: the code depends on the generated `orderpb` package and the grpc-gateway/grpc modules,
so it compiles only after `buf generate`, not in the offline gate.

## Resources

- [grpc-gateway runtime: HTTPStatusFromCode and error handlers](https://pkg.go.dev/github.com/grpc-ecosystem/grpc-gateway/v2/runtime#HTTPStatusFromCode) — the canonical code→status mapping and the `WithErrorHandler`/`WithRoutingErrorHandler` options.
- [grpc-gateway: Customizing error handling](https://grpc-ecosystem.github.io/grpc-gateway/docs/mapping/customizing_your_gateway/#error-handler) — installing a custom error handler and routing error handler.
- [google.golang.org/grpc/status](https://pkg.go.dev/google.golang.org/grpc/status) — `status.FromError`, `Status.Code`, `Status.Message`, `Status.Details`.
- [google.golang.org/grpc/codes](https://pkg.go.dev/google.golang.org/grpc/codes) — the canonical gRPC codes and their numeric values.

---

Back to [02-json-contract-and-metadata-bridge.md](02-json-contract-and-metadata-bridge.md) | Next: [../03-protobuf-buf-workflow/00-concepts.md](../03-protobuf-buf-workflow/00-concepts.md)
