# Exercise 2: Control the JSON contract and bridge headers to gRPC metadata

The gateway's `protojson` options are not an implementation detail — they *are* your
public JSON contract. This exercise configures them deliberately (snake_case field names,
a chosen policy on zero-valued fields, tolerance for unknown incoming fields) and then
wires the header/metadata bridge so `Authorization` reaches the gRPC handler and a
handler-set status code and `Location` surface back on the HTTP response.

## What you'll build

```text
contract/                        module: example.com/contract
  go.mod
  proto/order/v1/order.proto     annotated OrderService (go_package -> example.com/contract/...)
  gen/order/v1/                  generated from the proto (orderpb: messages + Register*Handler* funcs)
  contract.go                    NewGateway: JSONPb marshaler + header matcher + metadata + forward-response
  cmd/demo/main.go               POST an order, observe snake_case body, 201, and Location
  contract_test.go               snake_case, EmitUnpopulated toggle, auth->metadata, SetHeader->HTTP
```

Files: `proto/order/v1/order.proto`, `contract.go`, `cmd/demo/main.go`, `contract_test.go`.
Implement: `NewGateway` installing `runtime.JSONPb` via `WithMarshalerOption`, plus `WithIncomingHeaderMatcher`, `WithMetadata`, and `WithForwardResponseOption`.
Test: assert responses use snake_case, that `EmitUnpopulated` toggles zero-field presence, that an `Authorization` header arrives as gRPC metadata inside the handler, and that a handler's `grpc.SetHeader` value surfaces on the HTTP response.
Verify: `buf generate && go test -count=1 -race ./...` (bar mode: builds only after codegen and with the modules present).

Set up the module:

```bash
mkdir -p go-solutions/51-rpc-and-api-design/02-grpc-gateway-rest-json/02-json-contract-and-metadata-bridge/proto/order/v1 go-solutions/51-rpc-and-api-design/02-grpc-gateway-rest-json/02-json-contract-and-metadata-bridge/cmd/demo
cd go-solutions/51-rpc-and-api-design/02-grpc-gateway-rest-json/02-json-contract-and-metadata-bridge
go get github.com/grpc-ecosystem/grpc-gateway/v2/runtime
go get google.golang.org/grpc google.golang.org/protobuf
```

### The proto this module generates from

This module is standalone, so it carries its own copy of the annotated `OrderService`
proto — identical to Exercise 1 except for the `go_package` option, which must name *this*
module's import path (`example.com/contract/gen/order/v1;orderpb`). Generating with
`buf generate` (using `protoc-gen-go`, `protoc-gen-go-grpc`, and `protoc-gen-grpc-gateway`)
produces the `orderpb` package in `gen/order/v1` that the code below imports. The annotation
grammar is explained in Exercise 1; here the proto exists so you never have to reach back to
another module to regenerate.

Create `proto/order/v1/order.proto`:

```proto
syntax = "proto3";

package order.v1;

import "google/api/annotations.proto";
import "google/protobuf/empty.proto";
import "google/protobuf/field_mask.proto";

option go_package = "example.com/contract/gen/order/v1;orderpb";

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

### The marshaler is the contract

grpc-gateway serializes messages with `protojson`, and you register your chosen options by
building a `runtime.JSONPb` and passing it to `runtime.WithMarshalerOption(runtime.MIMEWildcard, ...)`.
`MIMEWildcard` is the constant `"*"` — it means "use this marshaler for every content type",
which is what you want for a JSON-only public API. `runtime.JSONPb` embeds
`protojson.MarshalOptions` and `protojson.UnmarshalOptions` as anonymous fields, so the
struct literal keys them by their type names:

- `UseProtoNames: true` keeps the proto snake_case field names (`amount_cents`) instead of
  protojson's default lowerCamelCase (`amountCents`). Once clients read snake_case, this is
  a locked-in contract decision.
- `EmitUnpopulated` decides whether zero-valued fields appear at all. This exercise threads
  it through `Options` so a test can prove both behaviors; in production you pick one and
  pin it, because flipping it is a breaking change for clients that distinguish absent from
  zero.
- `DiscardUnknown: true` on unmarshal makes the gateway tolerate unknown incoming fields
  rather than 400 on them — the right call for forward-compatible clients.

### The header/metadata bridge

HTTP headers and gRPC metadata are separate namespaces; by default only a small whitelist
crosses. Three hooks control the boundary:

- `WithIncomingHeaderMatcher` maps HTTP request headers to gRPC metadata keys. The matcher
  is called per header with the canonical header name; return `(metadataKey, true)` to
  forward it, or fall through to `runtime.DefaultHeaderMatcher` for the built-in whitelist.
  Forwarding *everything* would leak internal headers, so name exactly what the backend may
  see — here `Authorization` and `X-Request-Id`.
- `WithMetadata` injects derived values into the outgoing metadata regardless of the
  request. We attach the matched path pattern via `runtime.HTTPPathPattern`, which is the
  kind of context a backend uses for routing-aware logging.
- `WithForwardResponseOption` runs after the handler and can copy handler-set metadata onto
  the HTTP response. `runtime.ServerMetadataFromContext` returns the `ServerMetadata` whose
  `HeaderMD` holds what the handler set with `grpc.SetHeader`. The canonical uses: promote an
  `x-http-code` value to the real HTTP status (so a create can answer `201`), and promote a
  `location` value to a proper `Location` header.

Create `contract.go`:

```go
package contract

import (
	"context"
	"net/http"
	"strconv"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	orderpb "example.com/contract/gen/order/v1"
)

// Options exposes the public JSON contract knobs a test needs to flip.
type Options struct {
	EmitUnpopulated bool
}

// NewGateway builds an in-process OrderService gateway with a deliberate JSON contract
// and a header/metadata bridge.
func NewGateway(ctx context.Context, srv orderpb.OrderServiceServer, opt Options) (*runtime.ServeMux, error) {
	mux := runtime.NewServeMux(
		runtime.WithMarshalerOption(runtime.MIMEWildcard, &runtime.JSONPb{
			MarshalOptions: protojson.MarshalOptions{
				UseProtoNames:   true,
				EmitUnpopulated: opt.EmitUnpopulated,
			},
			UnmarshalOptions: protojson.UnmarshalOptions{
				DiscardUnknown: true,
			},
		}),
		runtime.WithIncomingHeaderMatcher(incomingHeaderMatcher),
		runtime.WithMetadata(annotateMetadata),
		runtime.WithForwardResponseOption(forwardResponse),
	)
	if err := orderpb.RegisterOrderServiceHandlerServer(ctx, mux, srv); err != nil {
		return nil, err
	}
	return mux, nil
}

// incomingHeaderMatcher forwards only the headers the backend is allowed to see; the
// default matcher handles the built-in whitelist for everything else.
func incomingHeaderMatcher(key string) (string, bool) {
	switch key {
	case "Authorization":
		return "authorization", true
	case "X-Request-Id":
		return "x-request-id", true
	default:
		return runtime.DefaultHeaderMatcher(key)
	}
}

// annotateMetadata injects derived context into every outgoing gRPC call.
func annotateMetadata(ctx context.Context, _ *http.Request) metadata.MD {
	md := metadata.MD{}
	if pattern, ok := runtime.HTTPPathPattern(ctx); ok {
		md.Set("http-path-pattern", pattern)
	}
	return md
}

// forwardResponse promotes handler-set metadata onto the HTTP response: an x-http-code
// becomes the status code, a location becomes a Location header.
func forwardResponse(ctx context.Context, w http.ResponseWriter, _ proto.Message) error {
	md, ok := runtime.ServerMetadataFromContext(ctx)
	if !ok {
		return nil
	}
	if loc := md.HeaderMD.Get("location"); len(loc) > 0 {
		w.Header().Set("Location", loc[0])
	}
	if code := md.HeaderMD.Get("x-http-code"); len(code) > 0 {
		n, err := strconv.Atoi(code[0])
		if err != nil {
			return err
		}
		delete(md.HeaderMD, "x-http-code")
		w.WriteHeader(n)
	}
	return nil
}
```

### The runnable demo

The demo wires a small in-memory server whose `CreateOrder` sets `x-http-code: 201` and a
`location` via `grpc.SetHeader`, then POSTs an order and prints the status, the `Location`
header, and the response body. Because `UseProtoNames` is on, the body is snake_case; note
`amount_cents` is a 64-bit integer, which protojson encodes as a JSON string. protojson's
single-line encoder also inserts a nondeterministic space after some commas (an anti-byte-
stability measure), so the demo compacts the body with `json.Compact` before printing to keep
the output stable across runs.

Create `cmd/demo/main.go`:

```go
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"

	"example.com/contract"
	orderpb "example.com/contract/gen/order/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

type memServer struct {
	orderpb.UnimplementedOrderServiceServer
}

func (memServer) CreateOrder(ctx context.Context, req *orderpb.CreateOrderRequest) (*orderpb.Order, error) {
	_ = grpc.SetHeader(ctx, metadata.Pairs("x-http-code", "201", "location", "/v1/orders/o-new"))
	return &orderpb.Order{
		Id:          "o-new",
		Customer:    req.GetCustomer(),
		AmountCents: req.GetAmountCents(),
		State:       "OPEN",
	}, nil
}

func main() {
	mux, err := contract.NewGateway(context.Background(), memServer{}, contract.Options{})
	if err != nil {
		log.Fatal(err)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()

	body := `{"customer":"bob","amount_cents":"999"}`
	resp, err := http.Post(srv.URL+"/v1/orders", "application/json", bytes.NewBufferString(body))
	if err != nil {
		log.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	// protojson may insert a nondeterministic space after commas; compact it so
	// the printed body is stable across runs.
	var out bytes.Buffer
	if err := json.Compact(&out, raw); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("POST /v1/orders -> %d\n", resp.StatusCode)
	fmt.Printf("Location: %s\n", resp.Header.Get("Location"))
	fmt.Printf("body: %s\n", out.Bytes())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
POST /v1/orders -> 201
Location: /v1/orders/o-new
body: {"id":"o-new","customer":"bob","amount_cents":"999","state":"OPEN"}
```

### Tests

The tests lock the public contract against silent regressions. `TestSnakeCaseAndCreatedStatus`
proves `UseProtoNames` produces `amount_cents` (not `amountCents`), that the auth header
reaches the handler as metadata, and that `grpc.SetHeader` surfaces as a `201` plus a
`Location` header. `TestEmitUnpopulated` builds one gateway with the flag off and one with it
on and asserts the same zero-valued field is absent in the first and present in the second —
exactly the behavior clients depend on.

Create `contract_test.go`:

```go
package contract_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"example.com/contract"
	orderpb "example.com/contract/gen/order/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

type fakeServer struct {
	orderpb.UnimplementedOrderServiceServer

	mu      sync.Mutex
	gotAuth string
}

func (s *fakeServer) CreateOrder(ctx context.Context, req *orderpb.CreateOrderRequest) (*orderpb.Order, error) {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if v := md.Get("authorization"); len(v) > 0 {
			s.mu.Lock()
			s.gotAuth = v[0]
			s.mu.Unlock()
		}
	}
	_ = grpc.SetHeader(ctx, metadata.Pairs("x-http-code", "201", "location", "/v1/orders/o-new"))
	return &orderpb.Order{Id: "o-new", Customer: req.GetCustomer(), State: "OPEN"}, nil
}

func (s *fakeServer) GetOrder(_ context.Context, req *orderpb.GetOrderRequest) (*orderpb.Order, error) {
	// Only id is set; customer, amount_cents, and state stay zero so EmitUnpopulated
	// is observable.
	return &orderpb.Order{Id: req.GetId()}, nil
}

func TestSnakeCaseAndCreatedStatus(t *testing.T) {
	t.Parallel()
	fake := &fakeServer{}
	mux, err := contract.NewGateway(context.Background(), fake, contract.Options{})
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()

	body := `{"customer":"bob","amount_cents":"999"}`
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/orders", bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer t0ken")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d; want 201 (from x-http-code via forward-response)", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/v1/orders/o-new" {
		t.Fatalf("Location = %q; want /v1/orders/o-new", got)
	}
	if !strings.Contains(string(out), `"amount_cents"`) {
		t.Fatalf("body %s lacks snake_case amount_cents (UseProtoNames not applied)", out)
	}
	if strings.Contains(string(out), `"amountCents"`) {
		t.Fatalf("body %s used camelCase; UseProtoNames should force snake_case", out)
	}
	fake.mu.Lock()
	gotAuth := fake.gotAuth
	fake.mu.Unlock()
	if gotAuth != "Bearer t0ken" {
		t.Fatalf("handler saw authorization = %q; want Bearer t0ken", gotAuth)
	}
}

func TestEmitUnpopulated(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		emit      bool
		wantState bool // whether the zero-valued "state" field should be present
	}{
		{name: "omitted when off", emit: false, wantState: false},
		{name: "present when on", emit: true, wantState: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mux, err := contract.NewGateway(context.Background(), &fakeServer{}, contract.Options{EmitUnpopulated: tt.emit})
			if err != nil {
				t.Fatalf("NewGateway: %v", err)
			}
			srv := httptest.NewServer(mux)
			defer srv.Close()

			resp, err := http.Get(srv.URL + "/v1/orders/o-1")
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			out, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			var m map[string]any
			if err := json.Unmarshal(out, &m); err != nil {
				t.Fatalf("unmarshal %s: %v", out, err)
			}
			_, gotState := m["state"]
			if gotState != tt.wantState {
				t.Fatalf("state present = %v; want %v (body %s)", gotState, tt.wantState, out)
			}
		})
	}
}

func ExampleNewGateway() {
	mux, _ := contract.NewGateway(context.Background(), &fakeServer{}, contract.Options{})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, _ := http.Get(srv.URL + "/v1/orders/o-1")
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	fmt.Println(string(out))
	// Output: {"id":"o-1"}
}
```

## Review

The contract is correct when the wire shape is exactly what you chose and does not shift
under you. `TestSnakeCaseAndCreatedStatus` proves three couplings at once: `UseProtoNames`
forces snake_case, the incoming-header matcher delivers `Authorization` to the handler as
metadata, and the forward-response option turns the handler's `x-http-code`/`location` into a
real `201` and a `Location` header. `TestEmitUnpopulated` proves the zero-field policy is a
switch you own. If snake_case flips to camelCase, `UseProtoNames` was dropped; if the auth
header never arrives, the matcher did not forward it (or forwarded the wrong casing); if the
status stays `200`, the forward-response option or `grpc.SetHeader` is missing.

The mistakes to avoid: leaving `protojson` at defaults and shipping camelCase by accident;
flipping `EmitUnpopulated` after clients depend on the current shape; and forwarding *all*
headers instead of a named set, which leaks internal headers into the backend. Remember this
is bar mode — the code depends on the generated `orderpb` package and the grpc-gateway/grpc
modules, so it compiles only after `buf generate`, not in the offline gate.

## Resources

- [grpc-gateway: Customizing your gateway](https://grpc-ecosystem.github.io/grpc-gateway/docs/mapping/customizing_your_gateway/) — marshaler options, header matchers, and the forward-response hook.
- [protojson.MarshalOptions](https://pkg.go.dev/google.golang.org/protobuf/encoding/protojson#MarshalOptions) — `UseProtoNames`, `EmitUnpopulated`, and how protojson encodes 64-bit ints.
- [grpc-gateway runtime: JSONPb and ServeMux options](https://pkg.go.dev/github.com/grpc-ecosystem/grpc-gateway/v2/runtime) — `WithMarshalerOption`, `MIMEWildcard`, `WithIncomingHeaderMatcher`, `WithMetadata`, `WithForwardResponseOption`, `ServerMetadataFromContext`.
- [grpc.SetHeader](https://pkg.go.dev/google.golang.org/grpc#SetHeader) — how a handler sets header metadata the gateway can read back.

---

Back to [01-annotated-gateway-transcoding.md](01-annotated-gateway-transcoding.md) | Next: [03-error-and-routing-status-mapping.md](03-error-and-routing-status-mapping.md)
