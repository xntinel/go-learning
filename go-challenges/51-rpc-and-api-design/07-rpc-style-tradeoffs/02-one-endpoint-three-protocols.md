# Exercise 2: One Endpoint, Three Protocols: Connect, gRPC, and gRPC-Web

The central claim about Connect is that one handler is simultaneously gRPC-fast for
services and curl/browser-friendly for humans, with no proxy. This exercise proves
it: a single `OrderService` served once, then called by a Connect client, a gRPC
client, and a raw HTTP/1.1 JSON POST, all hitting the same address and getting the
same answer.

This module is fully self-contained. It begins with its own `go mod init`, and
ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
multiproto/                  independent module: example.com/multiproto
  go.mod                     go 1.26
  protocols.go               H2CProtocols; NewH2CServer (h2c config, pure stdlib)
  store.go                   Order; Store; ErrOrderNotFound (pure)
  order.proto               OrderService.GetOrder schema (illustrative)
  service_online.go          //go:build online — OrderServer + NewMux (connectrpc.com/connect)
  cmd/
    demo/
      main.go                runnable pure demo: show the h2c config and a store lookup
  multiproto_test.go         offline protocols + store tests; ExampleH2CProtocols
  connect_online_test.go     //go:build online — one server, three protocols; CodeNotFound mapping
```

- Files: `protocols.go`, `store.go`, `order.proto`, `service_online.go`, `cmd/demo/main.go`, `multiproto_test.go`, `connect_online_test.go`.
- Implement: `H2CProtocols`/`NewH2CServer` configuring HTTP/1.1 + cleartext HTTP/2, a concurrency-safe `Store` returning `ErrOrderNotFound`, and the online `OrderServer` mapping that sentinel to `connect.CodeNotFound` plus a `NewMux` that mounts the protocol-agnostic handler.
- Test: offline tests that the protocol set enables both HTTP/1.1 and h2c and that the store's miss wraps `ErrOrderNotFound` (asserted with `errors.Is`), plus an `Example`; the online test calls one server with a Connect client, a gRPC client, and a JSON POST and asserts they agree, and that a missing order yields `CodeNotFound`.
- Verify: `go test -count=1 -race ./...` (offline core); the multi-protocol test builds and runs with `-tags online` after generating the Connect code.

This is a mode=bar lesson: the h2c configuration and the store gate cleanly as pure
stdlib, while the Connect handler and clients need generated code and the external
`connectrpc.com/connect` module, so they live behind `//go:build online`. Set up
the module:

```bash
go mod edit -go=1.26
```

### The h2c configuration, and why both switches matter

gRPC needs HTTP/2. In local development and behind a TLS-terminating mesh you serve
cleartext, so you need HTTP/2 without TLS — h2c. As of Go 1.24 you configure this
on the standard server through `http.Server.Protocols`, a `*http.Protocols` you
must initialize before use. `H2CProtocols` turns on both HTTP/1.1 and unencrypted
HTTP/2: the first keeps curl and browser JSON POSTs working over HTTP/1.1, the
second lets a gRPC client connect over cleartext h2. This is the exact foot-gun
from the concepts file — enable only `SetUnencryptedHTTP2` and every non-gRPC
client silently breaks; forget to initialize the pointer and you panic — so it is
worth isolating and unit-testing on its own.

Create `protocols.go`:

```go
package multiproto

import "net/http"

// H2CProtocols returns an http.Protocols enabling HTTP/1.1 and cleartext
// (unencrypted) HTTP/2. Both are required: HTTP/1.1 keeps curl and browser JSON
// POSTs working, while unencrypted HTTP/2 (h2c) lets a gRPC client speak to the
// same address without TLS. Enabling only one silently breaks the other clients.
func H2CProtocols() *http.Protocols {
	p := new(http.Protocols)
	p.SetHTTP1(true)
	p.SetUnencryptedHTTP2(true)
	return p
}

// NewH2CServer builds an *http.Server for addr serving h with the h2c-capable
// protocol set. It does not call ListenAndServe; the caller owns the lifecycle.
func NewH2CServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:      addr,
		Handler:   h,
		Protocols: H2CProtocols(),
	}
}
```

### The domain store, independent of any wire type

The business logic behind the RPC is a plain in-memory repository that knows
nothing about Connect, gRPC, or JSON. A miss wraps `ErrOrderNotFound` with `%w`, so
the transport layer can branch on `errors.Is` and translate it to whatever the wire
protocol calls "not found". Keeping the domain type free of wire types is what lets
the same `Store` sit behind three protocols.

Create `store.go`:

```go
package multiproto

import (
	"errors"
	"fmt"
	"sync"
)

// ErrOrderNotFound is returned when a lookup misses. The Connect adapter maps it
// to connect.CodeNotFound so every protocol surfaces the same not-found error.
var ErrOrderNotFound = errors.New("order not found")

// Order is the domain entity returned by the service, independent of any wire type.
type Order struct {
	ID         string
	CustomerID int64
	Status     string
}

// Store is a concurrency-safe in-memory order repository.
type Store struct {
	mu     sync.RWMutex
	orders map[string]Order
}

func NewStore() *Store {
	return &Store{orders: make(map[string]Order)}
}

func (s *Store) Put(o Order) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.orders[o.ID] = o
}

// Get returns the order for id or wraps ErrOrderNotFound.
func (s *Store) Get(id string) (Order, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	o, ok := s.orders[id]
	if !ok {
		return Order{}, fmt.Errorf("get order %q: %w", id, ErrOrderNotFound)
	}
	return o, nil
}
```

### The service schema and handler (online)

The schema is a single unary method. What matters for this exercise is not the
schema's richness but the fact that the *handler* generated from it is
protocol-agnostic: register it once on a mux and it serves the Connect protocol,
gRPC, and gRPC-Web by inspecting each request. Only clients choose a protocol.

This is the illustrative schema; it is a `proto` block, not assembled Go:

```proto
syntax = "proto3";
package order.v1;
option go_package = "example.com/multiproto/gen/order/v1;orderv1";

message Order {
  string id = 1;
  int64 customer_id = 2;
  string status = 3;
}

message GetOrderRequest {
  string id = 1;
}

message GetOrderResponse {
  Order order = 1;
}

service OrderService {
  rpc GetOrder(GetOrderRequest) returns (GetOrderResponse);
}
```

Generate the Connect and protobuf code (once, with the plugins installed):

```bash
buf generate
# or: protoc with protoc-gen-go and protoc-gen-connect-go
```

`OrderServer` implements the generated `OrderServiceHandler`, delegating to the
`Store` and translating `ErrOrderNotFound` into `connect.NewError(connect.
CodeNotFound, ...)`. `NewMux` mounts it with `mux.Handle(path, handler)` from the
generated `(string, http.Handler)` pair. Because it imports the external Connect
module and the generated packages, the file is behind `//go:build online`.

Create `service_online.go`:

```go
//go:build online

// This file holds the Connect service. It is excluded from the default build
// because it imports connectrpc.com/connect and the generated order/v1 packages
// (produced by buf/protoc). Build and test it with -tags online after codegen.
// The h2c config and the store are pure stdlib and tested offline.
package multiproto

import (
	"context"
	"errors"
	"net/http"

	"connectrpc.com/connect"
	orderv1 "example.com/multiproto/gen/order/v1"
	"example.com/multiproto/gen/order/v1/orderv1connect"
)

// OrderServer implements the generated OrderServiceHandler backed by the Store.
type OrderServer struct {
	store *Store
}

func NewOrderServer(s *Store) *OrderServer { return &OrderServer{store: s} }

// GetOrder returns the requested order, mapping ErrOrderNotFound to a Connect
// CodeNotFound error so every protocol (Connect, gRPC, gRPC-Web) reports the same
// not-found status. Any other error becomes CodeInternal.
func (s *OrderServer) GetOrder(
	ctx context.Context,
	req *connect.Request[orderv1.GetOrderRequest],
) (*connect.Response[orderv1.GetOrderResponse], error) {
	o, err := s.store.Get(req.Msg.GetId())
	if err != nil {
		if errors.Is(err, ErrOrderNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&orderv1.GetOrderResponse{
		Order: &orderv1.Order{
			Id:         o.ID,
			CustomerId: o.CustomerID,
			Status:     o.Status,
		},
	}), nil
}

// NewMux mounts the OrderService on a ServeMux. The returned handler is
// protocol-agnostic: it serves the Connect protocol, gRPC, and gRPC-Web from this
// one path, so clients pick the protocol, not the server.
func NewMux(s *Store) *http.ServeMux {
	mux := http.NewServeMux()
	path, handler := orderv1connect.NewOrderServiceHandler(NewOrderServer(s))
	mux.Handle(path, handler)
	return mux
}

// NewServer wires the mux onto an h2c-capable server for cleartext gRPC plus
// HTTP/1.1 JSON in local development.
func NewServer(addr string, s *Store) *http.Server {
	return NewH2CServer(addr, NewMux(s))
}
```

### The runnable demo

The demo stays in the offline core: it prints the h2c protocol switches and does a
store lookup (hit and miss), so it runs with no codegen and no external module.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/multiproto"
)

func main() {
	p := multiproto.H2CProtocols()
	fmt.Printf("http1=%v unencrypted_h2=%v\n", p.HTTP1(), p.UnencryptedHTTP2())

	st := multiproto.NewStore()
	st.Put(multiproto.Order{ID: "ord-1", CustomerID: 42, Status: "CONFIRMED"})

	o, err := st.Get("ord-1")
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("order %s customer=%d status=%s\n", o.ID, o.CustomerID, o.Status)

	if _, err := st.Get("missing"); err != nil {
		fmt.Println("lookup miss:", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
http1=true unencrypted_h2=true
order ord-1 customer=42 status=CONFIRMED
lookup miss: get order "missing": order not found
```

### Tests

The offline tests pin the two things that carry bugs: the protocol set must enable
both HTTP/1.1 and h2c (the foot-gun), and the store miss must wrap
`ErrOrderNotFound`. `ExampleH2CProtocols` locks the configuration output.

Create `multiproto_test.go`:

```go
package multiproto

import (
	"errors"
	"fmt"
	"testing"
)

func TestH2CProtocols(t *testing.T) {
	t.Parallel()
	p := H2CProtocols()
	if !p.HTTP1() {
		t.Error("HTTP1 not enabled: curl and browser JSON POSTs would break")
	}
	if !p.UnencryptedHTTP2() {
		t.Error("UnencryptedHTTP2 not enabled: cleartext gRPC (h2c) would break")
	}
}

func TestStoreGet(t *testing.T) {
	t.Parallel()
	st := NewStore()
	st.Put(Order{ID: "ord-1", CustomerID: 7, Status: "NEW"})

	tests := []struct {
		name    string
		id      string
		wantErr error
		wantCID int64
	}{
		{name: "hit", id: "ord-1", wantCID: 7},
		{name: "miss", id: "nope", wantErr: ErrOrderNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			o, err := st.Get(tc.id)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Get err = %v, want errors.Is %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if o.CustomerID != tc.wantCID {
				t.Fatalf("CustomerID = %d, want %d", o.CustomerID, tc.wantCID)
			}
		})
	}
}

func ExampleH2CProtocols() {
	p := H2CProtocols()
	fmt.Printf("http1=%v h2c=%v\n", p.HTTP1(), p.UnencryptedHTTP2())
	// Output: http1=true h2c=true
}
```

The online test is the proof of the whole exercise. It starts one server serving
the mux, then calls it three ways. The Connect-protocol client and the
`connect.WithGRPC()` client hit the same server and must return the same customer
id — one handler, two protocols. Then a raw `http.Post` of a JSON body to the
procedure path with `Content-Type: application/json` exercises the curl/HTTP-1.1
reach and must return 200 with the id in the body. Finally a missing order must map
to `connect.CodeNotFound`.

The test uses `httptest.NewUnstartedServer` with `EnableHTTP2` and `StartTLS`, so
HTTP/2 is negotiated over TLS and the gRPC client works; in production you would use
the h2c server from `NewServer` for cleartext. `srv.Client()` trusts the test
certificate.

Create `connect_online_test.go`:

```go
//go:build online

package multiproto

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"
	orderv1 "example.com/multiproto/gen/order/v1"
	"example.com/multiproto/gen/order/v1/orderv1connect"
)

func TestOneEndpointThreeProtocols(t *testing.T) {
	store := NewStore()
	store.Put(Order{ID: "ord-1", CustomerID: 42, Status: "CONFIRMED"})

	srv := httptest.NewUnstartedServer(NewMux(store))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	// 1) Default Connect-protocol client.
	connectClient := orderv1connect.NewOrderServiceClient(srv.Client(), srv.URL)
	got1, err := connectClient.GetOrder(context.Background(),
		connect.NewRequest(&orderv1.GetOrderRequest{Id: "ord-1"}))
	if err != nil {
		t.Fatalf("connect GetOrder: %v", err)
	}

	// 2) gRPC client against the SAME server, same address.
	grpcClient := orderv1connect.NewOrderServiceClient(srv.Client(), srv.URL, connect.WithGRPC())
	got2, err := grpcClient.GetOrder(context.Background(),
		connect.NewRequest(&orderv1.GetOrderRequest{Id: "ord-1"}))
	if err != nil {
		t.Fatalf("grpc GetOrder: %v", err)
	}
	if got1.Msg.GetOrder().GetCustomerId() != got2.Msg.GetOrder().GetCustomerId() {
		t.Fatalf("connect and grpc disagreed: %d vs %d",
			got1.Msg.GetOrder().GetCustomerId(), got2.Msg.GetOrder().GetCustomerId())
	}

	// 3) Plain HTTP JSON POST via the Connect protocol (the curl/human path).
	resp, err := srv.Client().Post(
		srv.URL+"/order.v1.OrderService/GetOrder",
		"application/json",
		strings.NewReader(`{"id":"ord-1"}`),
	)
	if err != nil {
		t.Fatalf("json POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("json POST status = %d, want 200", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), "42") {
		t.Fatalf("json body missing customer id: %s", raw)
	}
}

func TestNotFoundMapsToConnectCode(t *testing.T) {
	srv := httptest.NewUnstartedServer(NewMux(NewStore()))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	client := orderv1connect.NewOrderServiceClient(srv.Client(), srv.URL)
	_, err := client.GetOrder(context.Background(),
		connect.NewRequest(&orderv1.GetOrderRequest{Id: "missing"}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("code = %v, want NotFound", connect.CodeOf(err))
	}
}
```

## Review

The exercise is correct when one running handler answers all three callers
identically. The mistake it defends against most directly is the h2c
misconfiguration: `TestH2CProtocols` fails if you forget `SetHTTP1(true)` (which
would break the JSON POST) or `SetUnencryptedHTTP2(true)` (which would break the
gRPC client), and the pointer must be initialized with `new(http.Protocols)` before
you touch it. The second is expecting a browser or curl to speak raw gRPC; the JSON
POST works only because the *handler* also speaks the Connect protocol, which is
plain HTTP. The third is leaking wire types into the domain: the `Store` returns
`ErrOrderNotFound`, and the mapping to `connect.CodeNotFound` happens once, at the
edge, in `GetOrder`.

Confirm the offline core with `go test -race ./...`: the protocol test and the
store test must pass and `ExampleH2CProtocols` must print `http1=true h2c=true`. To
prove the multi-protocol claim, run `buf generate`, add the module requirements,
and `go test -tags online ./...`; a passing `TestOneEndpointThreeProtocols` is the
demonstration that the Connect handler is genuinely callable by a gRPC client and a
plain JSON POST at the same time.

## Resources

- [Connect for Go — deployment](https://connectrpc.com/docs/go/deployment/) — serving the Connect protocol, gRPC, and gRPC-Web from one handler, including h2c.
- [`connectrpc.com/connect`](https://pkg.go.dev/connectrpc.com/connect) — `NewRequest`, `NewResponse`, `WithGRPC`, `WithGRPCWeb`, `NewError`, `CodeOf`, `CodeNotFound`.
- [`net/http#Protocols`](https://pkg.go.dev/net/http#Protocols) — `SetHTTP1`, `SetHTTP2`, `SetUnencryptedHTTP2`, and the `Server.Protocols` field.
- [Connect Go getting started](https://connectrpc.com/docs/go/getting-started/) — the generated handler/client shapes and mounting on a `ServeMux`.

---

Back to [01-wire-format-cost-model.md](01-wire-format-cost-model.md) | Next: [03-streaming-decision-sse-vs-ndjson-vs-grpc.md](03-streaming-decision-sse-vs-ndjson-vs-grpc.md)
