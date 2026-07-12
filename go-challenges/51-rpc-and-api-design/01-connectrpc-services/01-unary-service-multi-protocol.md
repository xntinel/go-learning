# Exercise 1: A Connect unary service on one mux — gRPC and curl over one port

This exercise builds a `UserService` with two unary RPCs against a
`buf`-generated handler interface, mounts it on a standard `http.ServeMux`, and
serves it so the same port answers a Go Connect client over HTTP and a `curl`
POST of JSON over HTTP/1.1.

This module is self-contained: its own `go mod init`, its own generated code
(described below), its own demo, and its own tests. Because it depends on the
`connectrpc.com/connect` module and on `buf`-generated code, the Go files live
behind a `//go:build connect` tag and the offline gate does not compile them;
this is a bar-mode lesson.

## What you'll build

```text
usersvc/                     independent module: example.com/usersvc
  go.mod                     requires connectrpc.com/connect, google.golang.org/protobuf
  proto/user/v1/user.proto   the schema: User, GetUser, CreateUser
  buf.gen.yaml               buf codegen config (protoc-gen-go + protoc-gen-connect-go)
  gen/user/v1/               GENERATED (described, not hand-written): user.pb.go, userv1connect/
  service.go                 //go:build connect: type Service; GetUser, CreateUser; in-memory store
  cmd/
    demo/
      main.go                //go:build connect: serve on one port, call it with a Connect client
  service_test.go            //go:build connect: table-driven direct-call handler tests + Example
```

- Files: `service.go`, `cmd/demo/main.go`, `service_test.go` (plus the schema and generated code).
- Implement: a `Service` satisfying the generated `UserServiceHandler` interface — `GetUser` and `CreateUser` over an in-memory map, taking `*connect.Request[T]` and returning `*connect.Response[T]`.
- Test: call each method directly with a hand-built `*connect.Request[T]` (no network) and assert `res.Msg` fields; assert `connect.CodeOf(err)` for a missing user.
- Verify: `go test -tags connect ./...` (needs the modules fetched and `buf generate` run).

### Set up the module and generate from the schema

The schema is the source of truth; you never hand-write the wire types or the
handler interface. Define the service, then let `buf` generate the Go.

```bash
mkdir -p go-solutions/51-rpc-and-api-design/01-connectrpc-services/01-unary-service-multi-protocol/proto/user/v1 go-solutions/51-rpc-and-api-design/01-connectrpc-services/01-unary-service-multi-protocol/cmd/demo
cd go-solutions/51-rpc-and-api-design/01-connectrpc-services/01-unary-service-multi-protocol
go mod edit -go=1.26
go get connectrpc.com/connect@latest
go get google.golang.org/protobuf@latest
```

Create `proto/user/v1/user.proto`. The `go_package` option controls the import
path and package name of the generated code — the part after `;` is the Go
package name:

```proto
syntax = "proto3";

package user.v1;

option go_package = "example.com/usersvc/gen/user/v1;userv1";

message User {
  string id = 1;
  string name = 2;
  string email = 3;
}

message GetUserRequest {
  string id = 1;
}

message GetUserResponse {
  User user = 1;
}

message CreateUserRequest {
  string name = 1;
  string email = 2;
}

message CreateUserResponse {
  User user = 1;
}

service UserService {
  rpc GetUser(GetUserRequest) returns (GetUserResponse);
  rpc CreateUser(CreateUserRequest) returns (CreateUserResponse);
}
```

Create `buf.gen.yaml` to drive the two plugins — `protoc-gen-go` for the message
types and `protoc-gen-connect-go` for the handler and client:

```yaml
version: v2
plugins:
  - remote: buf.build/protocolbuffers/go
    out: gen
    opt: paths=source_relative
  - remote: buf.build/connectrpc/go
    out: gen
    opt: paths=source_relative
```

Then generate:

```bash
buf generate proto
```

### What the generated code gives you (described, not hand-written)

`buf generate` writes `gen/user/v1/user.pb.go` (the `userv1` message types:
`User`, `GetUserRequest`, and so on) and
`gen/user/v1/userv1connect/user.connect.go`. That second package is the boundary
you code against. It contains, in shape:

```go
// generated in package userv1connect (illustrative — do not hand-write)
type UserServiceHandler interface {
	GetUser(context.Context, *connect.Request[userv1.GetUserRequest]) (*connect.Response[userv1.GetUserResponse], error)
	CreateUser(context.Context, *connect.Request[userv1.CreateUserRequest]) (*connect.Response[userv1.CreateUserResponse], error)
}

func NewUserServiceHandler(svc UserServiceHandler, opts ...connect.HandlerOption) (string, http.Handler)

type UserServiceClient interface {
	GetUser(context.Context, *connect.Request[userv1.GetUserRequest]) (*connect.Response[userv1.GetUserResponse], error)
	CreateUser(context.Context, *connect.Request[userv1.CreateUserRequest]) (*connect.Response[userv1.CreateUserResponse], error)
}

func NewUserServiceClient(httpClient connect.HTTPClient, baseURL string, opts ...connect.ClientOption) UserServiceClient
```

Your job is to implement `UserServiceHandler`. Note that each method takes a
`*connect.Request[T]` and returns a `*connect.Response[T]` — not the bare
message. The real payload is `req.Msg` (a `*userv1.GetUserRequest`), and you build
the return with `connect.NewResponse(&userv1.GetUserResponse{...})`. The envelope
around the message is what carries headers and trailers; Exercise 2 uses that
channel for auth and metadata.

### Implement the service

The store is an in-memory map behind a `sync.RWMutex` — a stand-in for the
repository a real service would hold. `GetUser` returns `CodeNotFound` for a
missing id (the code, not a bare error, is what a client keys retry logic on),
and `CreateUser` rejects an empty name with `CodeInvalidArgument` and assigns a
monotonic id.

Create `service.go`:

```go
//go:build connect

package usersvc

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"connectrpc.com/connect"
	userv1 "example.com/usersvc/gen/user/v1"
)

// Service is an in-memory implementation of the generated UserServiceHandler
// interface. The same value serves the Connect protocol, gRPC, and gRPC-Web.
type Service struct {
	mu    sync.RWMutex
	users map[string]*userv1.User
	seq   int
}

// NewService returns an empty, ready-to-serve UserService.
func NewService() *Service {
	return &Service{users: make(map[string]*userv1.User)}
}

// GetUser returns the stored user or a CodeNotFound error. The payload is
// req.Msg; the response is wrapped so headers and trailers can ride along.
func (s *Service) GetUser(
	ctx context.Context,
	req *connect.Request[userv1.GetUserRequest],
) (*connect.Response[userv1.GetUserResponse], error) {
	s.mu.RLock()
	u, ok := s.users[req.Msg.GetId()]
	s.mu.RUnlock()
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound,
			fmt.Errorf("user %q not found", req.Msg.GetId()))
	}
	return connect.NewResponse(&userv1.GetUserResponse{User: u}), nil
}

// CreateUser stores a new user with a server-assigned id. An empty name is a
// client error, reported as CodeInvalidArgument so callers do not retry it.
func (s *Service) CreateUser(
	ctx context.Context,
	req *connect.Request[userv1.CreateUserRequest],
) (*connect.Response[userv1.CreateUserResponse], error) {
	if req.Msg.GetName() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("name is required"))
	}
	s.mu.Lock()
	s.seq++
	id := fmt.Sprintf("u%d", s.seq)
	u := &userv1.User{Id: id, Name: req.Msg.GetName(), Email: req.Msg.GetEmail()}
	s.users[id] = u
	s.mu.Unlock()
	return connect.NewResponse(&userv1.CreateUserResponse{User: u}), nil
}
```

### The runnable demo

The demo proves the one-port claim from Go's side: it builds the handler, mounts
it on a plain `http.ServeMux`, and serves it with `http.Protocols` set for both
HTTP/1.1 and unencrypted HTTP/2, so the same listener answers gRPC and JSON. It
then drives the service with a real Connect client over that port.

Create `cmd/demo/main.go`:

```go
//go:build connect

package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"connectrpc.com/connect"
	"example.com/usersvc"
	userv1 "example.com/usersvc/gen/user/v1"
	"example.com/usersvc/gen/user/v1/userv1connect"
)

func main() {
	mux := http.NewServeMux()
	path, handler := userv1connect.NewUserServiceHandler(usersvc.NewService())
	mux.Handle(path, handler)

	// One listener, both transports: HTTP/1.1 for curl/JSON, unencrypted
	// HTTP/2 for gRPC and streaming.
	proto := new(http.Protocols)
	proto.SetHTTP1(true)
	proto.SetUnencryptedHTTP2(true)
	srv := &http.Server{Addr: "127.0.0.1:8080", Handler: mux, Protocols: proto}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()
	defer srv.Close()
	time.Sleep(50 * time.Millisecond) // let the listener bind

	client := userv1connect.NewUserServiceClient(http.DefaultClient, "http://127.0.0.1:8080")
	ctx := context.Background()

	created, err := client.CreateUser(ctx, connect.NewRequest(&userv1.CreateUserRequest{
		Name:  "Ada Lovelace",
		Email: "ada@example.com",
	}))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("created user %s: %s\n", created.Msg.GetUser().GetId(), created.Msg.GetUser().GetName())

	fetched, err := client.GetUser(ctx, connect.NewRequest(&userv1.GetUserRequest{
		Id: created.Msg.GetUser().GetId(),
	}))
	if err != nil {
		log.Fatal(err)
	}
	u := fetched.Msg.GetUser()
	fmt.Printf("fetched user %s: %s <%s>\n", u.GetId(), u.GetName(), u.GetEmail())
}
```

Run it:

```bash
go run -tags connect ./cmd/demo
```

Expected output:

```
created user u1: Ada Lovelace
fetched user u1: Ada Lovelace <ada@example.com>
```

The demo above exits as soon as its two client calls return, at which point the
deferred `srv.Close()` shuts the listener down, so it is not left running for an
external `curl`. The point of the demo is the Go client round-trip. To hit the
server from `curl` you would keep it alive — replace the client calls with a
block such as `select {}` or wait on a signal so the process does not return.

The wire shape is worth seeing regardless of who is running the server. A Connect
unary call over HTTP/1.1 is a plain POST to `/<package>.<Service>/<Method>` with
a JSON body — no REST gateway, no separate port, the same handler decoding JSON
instead of the binary Connect framing. Against a server that already holds a user
with id `u1`, this request:

```bash
curl --header "Content-Type: application/json" \
  --data '{"id":"u1"}' \
  http://127.0.0.1:8080/user.v1.UserService/GetUser
```

illustrates the Connect JSON response envelope:

```json
{"user":{"id":"u1","name":"Ada Lovelace","email":"ada@example.com"}}
```

That JSON is the Connect protocol's own encoding of `GetUserResponse`, produced by
the same handler that answers the Go client over HTTP/2 — not a REST translation
layer. (The store is per-process and starts empty, so a fresh server would return
`CodeNotFound` for `u1` until something creates it.)

### Tests

The fast, dependency-free way to test handler logic is to call the method
directly with a hand-built request: `connect.NewRequest(msg)` produces the
envelope, you call `svc.GetUser(ctx, req)`, and you assert on `res.Msg`. No server
and no client are involved, so the test is a pure unit test of the business logic.
The `CodeOf` assertion checks the wire contract — that a missing user surfaces as
`CodeNotFound`, not `CodeUnknown`.

Create `service_test.go`:

```go
//go:build connect

package usersvc

import (
	"context"
	"fmt"
	"testing"

	"connectrpc.com/connect"
	userv1 "example.com/usersvc/gen/user/v1"
)

func TestCreateThenGet(t *testing.T) {
	t.Parallel()
	svc := NewService()
	ctx := context.Background()

	created, err := svc.CreateUser(ctx, connect.NewRequest(&userv1.CreateUserRequest{
		Name:  "Ada Lovelace",
		Email: "ada@example.com",
	}))
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	id := created.Msg.GetUser().GetId()
	if id == "" {
		t.Fatal("CreateUser returned an empty id")
	}

	got, err := svc.GetUser(ctx, connect.NewRequest(&userv1.GetUserRequest{Id: id}))
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if name := got.Msg.GetUser().GetName(); name != "Ada Lovelace" {
		t.Errorf("GetUser name = %q, want %q", name, "Ada Lovelace")
	}
}

func TestHandlerErrors(t *testing.T) {
	t.Parallel()
	svc := NewService()
	ctx := context.Background()

	tests := []struct {
		name string
		call func() error
		want connect.Code
	}{
		{
			name: "missing user is not found",
			call: func() error {
				_, err := svc.GetUser(ctx, connect.NewRequest(&userv1.GetUserRequest{Id: "nope"}))
				return err
			},
			want: connect.CodeNotFound,
		},
		{
			name: "empty name is invalid argument",
			call: func() error {
				_, err := svc.CreateUser(ctx, connect.NewRequest(&userv1.CreateUserRequest{Name: ""}))
				return err
			},
			want: connect.CodeInvalidArgument,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.call()
			if got := connect.CodeOf(err); got != tc.want {
				t.Fatalf("code = %v, want %v (err: %v)", got, tc.want, err)
			}
		})
	}
}

func Example() {
	svc := NewService()
	ctx := context.Background()

	created, _ := svc.CreateUser(ctx, connect.NewRequest(&userv1.CreateUserRequest{
		Name: "Grace Hopper",
	}))
	got, _ := svc.GetUser(ctx, connect.NewRequest(&userv1.GetUserRequest{
		Id: created.Msg.GetUser().GetId(),
	}))
	fmt.Println(got.Msg.GetUser().GetName())
	// Output: Grace Hopper
}
```

## Review

The service is correct when its behavior is a pure function of `req.Msg` and the
store: `GetUser` returns the stored `User` or `CodeNotFound`, `CreateUser` rejects
an empty name with `CodeInvalidArgument` and otherwise assigns a fresh id and
returns the created `User`. The proof is `TestCreateThenGet` (round-trip through
the store) and `TestHandlerErrors` (the codes on the failure paths), both calling
the methods directly with no network.

The mistakes to avoid are structural. Do not read the payload anywhere but
`req.Msg`, and do not return the bare `*userv1.GetUserResponse` — wrap it with
`connect.NewResponse` so headers and trailers have a home, which Exercise 2 relies
on. On the serving side, set both `SetHTTP1(true)` and `SetUnencryptedHTTP2(true)`
(or wrap with `h2c` on pre-1.24 Go): with only HTTP/1.1 the `curl` path works and
every gRPC and streaming client silently breaks. This is a bar-mode lesson, so the
offline gate cannot fetch `connectrpc.com/connect` or run `buf generate`; confirm
correctness by generating the code and running `go test -tags connect ./...` in an
environment with network access.

## Resources

- [Connect for Go — Getting Started](https://connectrpc.com/docs/go/getting-started/) — the handler/client shape, mounting on a mux, `http.Protocols`, and the `curl` invocation.
- [`connectrpc.com/connect` reference](https://pkg.go.dev/connectrpc.com/connect) — `NewRequest`, `NewResponse`, `Request[T].Msg`, `NewUserServiceHandler`/`Client` conventions.
- [`net/http.Protocols`](https://pkg.go.dev/net/http#Protocols) — `SetHTTP1` and `SetUnencryptedHTTP2` for HTTP/1.1 + cleartext HTTP/2 on one server.
- [buf — generate](https://buf.build/docs/generate/overview/) — `buf.gen.yaml` and the `protoc-gen-connect-go` plugin.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-errors-interceptors-metadata.md](02-errors-interceptors-metadata.md)
