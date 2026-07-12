# Exercise 2: Coded errors, error details, and interceptor middleware

Production RPC failures are a contract, not a string. This exercise gives the
`UserService` deliberate gRPC-compatible error codes, attaches machine-readable
detail messages, reads and echoes request metadata, and adds the cross-cutting
middleware — token auth with per-method authorization, and structured request
logging — as Connect interceptors.

This module is self-contained: its own `go mod init`, its own generated code
(described in Exercise 1), its own demo, and its own tests. It depends on
`connectrpc.com/connect` and the well-known `errdetails` protos, so the Go files
live behind a `//go:build connect` tag and the offline gate does not compile
them; this is a bar-mode lesson.

## What you'll build

```text
usersvc/                     independent module: example.com/usersvc
  go.mod                     requires connectrpc.com/connect, protobuf, genproto errdetails
  proto/user/v1/user.proto   the schema from Exercise 1 (User, GetUser, CreateUser)
  gen/user/v1/               GENERATED: user.pb.go, userv1connect/ (described, not hand-written)
  service.go                 //go:build connect: coded errors + typed details + metadata echo
  auth.go                    //go:build connect: AuthInterceptor (authn+authz), logging interceptor
  cmd/
    demo/
      main.go                //go:build connect: print the code+message+detail a client sees
  usersvc_test.go            //go:build connect: interceptor unit tests + error-detail decode
```

- Files: `service.go`, `auth.go`, `cmd/demo/main.go`, `usersvc_test.go` (plus schema and generated code).
- Implement: handlers returning `connect.NewError` with `CodeNotFound` / `CodeInvalidArgument` plus a typed `errdetails` detail; an `AuthInterceptor` (full `connect.Interceptor`) doing bearer authn and per-procedure authz; a `connect.UnaryInterceptorFunc` for structured logging.
- Test: feed the interceptor a fake `connect.UnaryFunc` and assert it rejects missing/invalid tokens with `CodeUnauthenticated` and passes valid ones through; call a handler and assert `connect.CodeOf(err)` plus the decoded detail.
- Verify: `go test -tags connect ./...` (needs the modules fetched and `buf generate` run).

### Set up the module

Reuse the schema and generated code from Exercise 1; this module adds one more
dependency, the well-known error-detail protos.

```bash
mkdir -p go-solutions/51-rpc-and-api-design/01-connectrpc-services/02-errors-interceptors-metadata/proto/user/v1 go-solutions/51-rpc-and-api-design/01-connectrpc-services/02-errors-interceptors-metadata/cmd/demo
cd go-solutions/51-rpc-and-api-design/01-connectrpc-services/02-errors-interceptors-metadata
go mod edit -go=1.26
go get connectrpc.com/connect@latest
go get google.golang.org/protobuf@latest
go get google.golang.org/genproto/googleapis/rpc/errdetails@latest
buf generate proto
```

### Codes are the retry contract; details are the remediation

Every failure a handler returns must carry a deliberate code, because the code is
what the client's retry and alerting logic reads. `CodeNotFound` means the entity
is absent (do not retry blindly); `CodeInvalidArgument` means the request is
malformed (never retry, fix the caller); `CodeUnauthenticated` and
`CodePermissionDenied` are the authn and authz failures. A bare `errors.New`
returned from a handler collapses to `CodeUnknown` on the wire, which tells the
client nothing, so `connect.NewError(code, err)` is used on every failure path.

The code is coarse; the *detail* is the fine-grained, machine-readable
remediation. `connect.NewErrorDetail` accepts a Protobuf message and attaches it
to the error. We use the Google well-known error details: `ResourceInfo` on a
not-found (naming the type and id that was missing) and `BadRequest` with one
`FieldViolation` per invalid field on an invalid-argument. The client decodes
these back into structured data with `errors.As` into a `*connect.Error`
(`var cerr *connect.Error; errors.As(err, &cerr)`) and `detail.Value()`, so it
never has to parse the human-readable message string.

### Metadata rides in the envelope

Because the handler receives `*connect.Request[T]`, request metadata is available
via `req.Header()`. `GetUser` reads a `Request-Id` correlation header and echoes
it on the response via `connect.NewResponse(msg).Header().Set(...)`, the standard
way to thread a trace/correlation id back to the caller. This is exactly why
handlers take the envelope and not the bare message.

Create `service.go`:

```go
//go:build connect

package usersvc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"connectrpc.com/connect"
	userv1 "example.com/usersvc/gen/user/v1"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
)

// Service is an in-memory UserService with production-grade error mapping.
type Service struct {
	mu    sync.RWMutex
	users map[string]*userv1.User
	seq   int
}

// NewService returns an empty, ready-to-serve UserService.
func NewService() *Service {
	return &Service{users: make(map[string]*userv1.User)}
}

// GetUser returns the user, echoing a Request-Id correlation header, or a
// CodeNotFound error carrying a ResourceInfo detail naming what was missing.
func (s *Service) GetUser(
	ctx context.Context,
	req *connect.Request[userv1.GetUserRequest],
) (*connect.Response[userv1.GetUserResponse], error) {
	s.mu.RLock()
	u, ok := s.users[req.Msg.GetId()]
	s.mu.RUnlock()
	if !ok {
		cerr := connect.NewError(connect.CodeNotFound,
			fmt.Errorf("user %q not found", req.Msg.GetId()))
		if d, derr := connect.NewErrorDetail(&errdetails.ResourceInfo{
			ResourceType: "user.v1.User",
			ResourceName: req.Msg.GetId(),
			Description:  "no user with that id",
		}); derr == nil {
			cerr.AddDetail(d)
		}
		return nil, cerr
	}
	resp := connect.NewResponse(&userv1.GetUserResponse{User: u})
	if rid := req.Header().Get("Request-Id"); rid != "" {
		resp.Header().Set("Request-Id", rid) // echo the correlation id back
	}
	return resp, nil
}

// CreateUser validates the request and, on failure, returns CodeInvalidArgument
// with a BadRequest detail listing every offending field at once.
func (s *Service) CreateUser(
	ctx context.Context,
	req *connect.Request[userv1.CreateUserRequest],
) (*connect.Response[userv1.CreateUserResponse], error) {
	if violations := validateCreate(req.Msg); len(violations) > 0 {
		cerr := connect.NewError(connect.CodeInvalidArgument,
			errors.New("invalid CreateUserRequest"))
		if d, derr := connect.NewErrorDetail(&errdetails.BadRequest{
			FieldViolations: violations,
		}); derr == nil {
			cerr.AddDetail(d)
		}
		return nil, cerr
	}
	s.mu.Lock()
	s.seq++
	id := fmt.Sprintf("u%d", s.seq)
	u := &userv1.User{Id: id, Name: req.Msg.GetName(), Email: req.Msg.GetEmail()}
	s.users[id] = u
	s.mu.Unlock()
	return connect.NewResponse(&userv1.CreateUserResponse{User: u}), nil
}

// validateCreate collects field-level violations for a BadRequest detail.
func validateCreate(msg *userv1.CreateUserRequest) []*errdetails.BadRequest_FieldViolation {
	var v []*errdetails.BadRequest_FieldViolation
	if msg.GetName() == "" {
		v = append(v, &errdetails.BadRequest_FieldViolation{
			Field:       "name",
			Description: "must not be empty",
		})
	}
	if e := msg.GetEmail(); e != "" && !strings.Contains(e, "@") {
		v = append(v, &errdetails.BadRequest_FieldViolation{
			Field:       "email",
			Description: "must be a valid address",
		})
	}
	return v
}
```

### The interceptors: authn+authz, and logging

`AuthInterceptor` implements the full `connect.Interceptor` interface, so it
covers unary and streaming handlers alike. Authentication extracts the bearer
token and compares it in constant time; authorization is per-procedure, using
`req.Spec().Procedure`: any valid token may read, but only the write token may
call `CreateUser`. A missing or unknown token is `CodeUnauthenticated`; a valid
but insufficient token is `CodePermissionDenied` — the distinction a client needs
to decide whether to re-authenticate or to give up.

The logging interceptor is a `connect.UnaryInterceptorFunc`, the shortcut for
middleware that only touches unary RPCs (streaming passes through untouched). It
times the call and logs the procedure and resulting code. Ordering is the subtle
part: interceptors wrap outermost-first, so listing auth before logging in
`WithInterceptors` means a rejected request never reaches the logger — you do not
log unauthenticated calls as if they were served. Reverse the order and every
probe with a bad token shows up in your success logs.

Create `auth.go`:

```go
//go:build connect

package usersvc

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"
)

// AuthInterceptor authenticates a bearer token and authorizes per procedure.
// It implements the full connect.Interceptor interface.
type AuthInterceptor struct {
	readToken  string
	writeToken string
}

// NewAuthInterceptor builds an interceptor that accepts either token for reads
// and requires the write token for CreateUser.
func NewAuthInterceptor(readToken, writeToken string) *AuthInterceptor {
	return &AuthInterceptor{readToken: readToken, writeToken: writeToken}
}

func (a *AuthInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if err := a.authorize(req.Spec().Procedure, req.Header()); err != nil {
			return nil, err
		}
		return next(ctx, req)
	}
}

// WrapStreamingClient is a pass-through: this interceptor guards the server side.
func (a *AuthInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (a *AuthInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		if err := a.authorize(conn.Spec().Procedure, conn.RequestHeader()); err != nil {
			return err
		}
		return next(ctx, conn)
	}
}

// authorize is the pure policy function, tested directly.
func (a *AuthInterceptor) authorize(procedure string, h http.Header) error {
	tok, err := bearerToken(h)
	if err != nil {
		return err
	}
	isWrite := subtle.ConstantTimeCompare([]byte(tok), []byte(a.writeToken)) == 1
	isRead := subtle.ConstantTimeCompare([]byte(tok), []byte(a.readToken)) == 1
	switch {
	case isWrite:
		return nil
	case isRead:
		if strings.HasSuffix(procedure, "/CreateUser") {
			return connect.NewError(connect.CodePermissionDenied,
				errors.New("read-only token may not create users"))
		}
		return nil
	default:
		return connect.NewError(connect.CodeUnauthenticated, errors.New("invalid token"))
	}
}

// bearerToken extracts the token from a "Bearer <token>" Authorization header.
func bearerToken(h http.Header) (string, error) {
	got := h.Get("Authorization")
	if got == "" {
		return "", connect.NewError(connect.CodeUnauthenticated,
			errors.New("missing authorization header"))
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(got, prefix) {
		return "", connect.NewError(connect.CodeUnauthenticated,
			errors.New("authorization must use the Bearer scheme"))
	}
	return strings.TrimPrefix(got, prefix), nil
}

// NewLoggingInterceptor logs procedure, code, and latency for every unary RPC.
func NewLoggingInterceptor(logger *slog.Logger) connect.UnaryInterceptorFunc {
	return connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			start := time.Now()
			res, err := next(ctx, req)
			code := "ok"
			if err != nil {
				code = connect.CodeOf(err).String()
			}
			logger.Info("rpc handled",
				slog.String("procedure", req.Spec().Procedure),
				slog.String("code", code),
				slog.Duration("elapsed", time.Since(start)),
			)
			return res, err
		}
	})
}
```

### The runnable demo

The demo mounts the service with the interceptor chain `[auth, logging]` (auth
outermost), then makes three calls with a real client and prints the code,
message, and any decoded detail the client sees. Logging output is sent to
`io.Discard` so the printed lines are deterministic.

Create `cmd/demo/main.go`:

```go
//go:build connect

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"

	"connectrpc.com/connect"
	"example.com/usersvc"
	userv1 "example.com/usersvc/gen/user/v1"
	"example.com/usersvc/gen/user/v1/userv1connect"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
)

func main() {
	auth := usersvc.NewAuthInterceptor("read-tok", "write-tok")
	logging := usersvc.NewLoggingInterceptor(slog.New(slog.NewTextHandler(io.Discard, nil)))
	path, handler := userv1connect.NewUserServiceHandler(
		usersvc.NewService(),
		connect.WithInterceptors(auth, logging), // auth outermost: rejects before logging
	)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	ctx := context.Background()
	client := userv1connect.NewUserServiceClient(http.DefaultClient, ts.URL)

	// 1. Authorized read of a missing user: CodeNotFound + ResourceInfo detail.
	get := connect.NewRequest(&userv1.GetUserRequest{Id: "ghost"})
	get.Header().Set("Authorization", "Bearer read-tok")
	_, err := client.GetUser(ctx, get)
	report("GetUser [read token, missing id]", err)

	// 2. Unauthenticated write: CodeUnauthenticated (never reaches the handler).
	_, err = client.CreateUser(ctx, connect.NewRequest(&userv1.CreateUserRequest{Name: "Ada"}))
	report("CreateUser [no auth]", err)

	// 3. Authenticated but under-privileged write: CodePermissionDenied.
	create := connect.NewRequest(&userv1.CreateUserRequest{Name: "Ada"})
	create.Header().Set("Authorization", "Bearer read-tok")
	_, err = client.CreateUser(ctx, create)
	report("CreateUser [read token]", err)
}

func report(label string, err error) {
	fmt.Printf("%s: code=%s msg=%q\n", label, connect.CodeOf(err), messageOf(err))
	var cerr *connect.Error
	if errors.As(err, &cerr) {
		for _, d := range cerr.Details() {
			if msg, verr := d.Value(); verr == nil {
				if ri, ok := msg.(*errdetails.ResourceInfo); ok {
					fmt.Printf("  detail: resource_type=%s resource_name=%s\n",
						ri.GetResourceType(), ri.GetResourceName())
				}
			}
		}
	}
}

func messageOf(err error) string {
	var cerr *connect.Error
	if errors.As(err, &cerr) {
		return cerr.Message()
	}
	return err.Error()
}
```

Run it:

```bash
go run -tags connect ./cmd/demo
```

Expected output:

```
GetUser [read token, missing id]: code=not_found msg="user \"ghost\" not found"
  detail: resource_type=user.v1.User resource_name=ghost
CreateUser [no auth]: code=unauthenticated msg="missing authorization header"
CreateUser [read token]: code=permission_denied msg="read-only token may not create users"
```

### Tests

Two tiers. The interceptor is tested as a plain function: build a fake
`connect.UnaryFunc` that records whether it was called, wrap it with
`WrapUnary`, and drive requests with different `Authorization` headers, asserting
that missing and invalid tokens are rejected with `CodeUnauthenticated` and never
reach `next`, while a valid token passes through. The per-procedure authorization
is tested by calling the pure `authorize` method with an explicit procedure
string (a bare `connect.NewRequest` has an empty `Spec().Procedure` until the
client sends it, so testing the policy directly is both simpler and more precise).
The handler error mapping is tested by calling the method and asserting
`connect.CodeOf(err)` plus decoding the attached detail.

Create `usersvc_test.go`:

```go
//go:build connect

package usersvc

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"connectrpc.com/connect"
	userv1 "example.com/usersvc/gen/user/v1"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
)

func TestAuthInterceptorUnary(t *testing.T) {
	t.Parallel()
	interceptor := NewAuthInterceptor("read-tok", "write-tok")

	tests := []struct {
		name   string
		header string
		wantOK bool
	}{
		{"valid read token passes", "Bearer read-tok", true},
		{"valid write token passes", "Bearer write-tok", true},
		{"missing header rejected", "", false},
		{"unknown token rejected", "Bearer nope", false},
		{"wrong scheme rejected", "Basic read-tok", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var reached bool
			next := connect.UnaryFunc(func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
				reached = true
				return connect.NewResponse(&userv1.GetUserResponse{}), nil
			})
			req := connect.NewRequest(&userv1.GetUserRequest{Id: "u1"})
			if tc.header != "" {
				req.Header().Set("Authorization", tc.header)
			}
			_, err := interceptor.WrapUnary(next)(context.Background(), req)
			if tc.wantOK {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if !reached {
					t.Fatal("next was not called for an authorized request")
				}
				return
			}
			if reached {
				t.Fatal("next was called despite rejection")
			}
			if got := connect.CodeOf(err); got != connect.CodeUnauthenticated {
				t.Fatalf("code = %v, want unauthenticated", got)
			}
		})
	}
}

func TestAuthorizeByProcedure(t *testing.T) {
	t.Parallel()
	interceptor := NewAuthInterceptor("read-tok", "write-tok")
	hdr := func(tok string) http.Header {
		h := http.Header{}
		h.Set("Authorization", "Bearer "+tok)
		return h
	}
	const create = "/user.v1.UserService/CreateUser"
	const get = "/user.v1.UserService/GetUser"

	tests := []struct {
		name      string
		procedure string
		token     string
		wantCode  connect.Code
		wantOK    bool
	}{
		{"read token may get", get, "read-tok", 0, true},
		{"write token may create", create, "write-tok", 0, true},
		{"read token may not create", create, "read-tok", connect.CodePermissionDenied, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := interceptor.authorize(tc.procedure, hdr(tc.token))
			if tc.wantOK {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if got := connect.CodeOf(err); got != tc.wantCode {
				t.Fatalf("code = %v, want %v", got, tc.wantCode)
			}
		})
	}
}

func TestNotFoundDetail(t *testing.T) {
	t.Parallel()
	svc := NewService()
	_, err := svc.GetUser(context.Background(),
		connect.NewRequest(&userv1.GetUserRequest{Id: "ghost"}))

	if got := connect.CodeOf(err); got != connect.CodeNotFound {
		t.Fatalf("code = %v, want not_found", got)
	}
	var cerr *connect.Error
	if !errors.As(err, &cerr) {
		t.Fatal("error is not a *connect.Error")
	}
	var found bool
	for _, d := range cerr.Details() {
		msg, verr := d.Value()
		if verr != nil {
			t.Fatalf("decoding detail: %v", verr)
		}
		if ri, ok := msg.(*errdetails.ResourceInfo); ok {
			if ri.GetResourceName() != "ghost" {
				t.Errorf("resource_name = %q, want %q", ri.GetResourceName(), "ghost")
			}
			found = true
		}
	}
	if !found {
		t.Error("expected a ResourceInfo detail on the not-found error")
	}
}

func TestInvalidArgumentFieldViolations(t *testing.T) {
	t.Parallel()
	svc := NewService()
	_, err := svc.CreateUser(context.Background(),
		connect.NewRequest(&userv1.CreateUserRequest{Name: "", Email: "not-an-email"}))

	if got := connect.CodeOf(err); got != connect.CodeInvalidArgument {
		t.Fatalf("code = %v, want invalid_argument", got)
	}
	var cerr *connect.Error
	if !errors.As(err, &cerr) {
		t.Fatal("error is not a *connect.Error")
	}
	fields := map[string]bool{}
	for _, d := range cerr.Details() {
		msg, verr := d.Value()
		if verr != nil {
			t.Fatalf("decoding detail: %v", verr)
		}
		if br, ok := msg.(*errdetails.BadRequest); ok {
			for _, fv := range br.GetFieldViolations() {
				fields[fv.GetField()] = true
			}
		}
	}
	for _, want := range []string{"name", "email"} {
		if !fields[want] {
			t.Errorf("missing field violation for %q", want)
		}
	}
}

func Example_notFound() {
	svc := NewService()
	_, err := svc.GetUser(context.Background(),
		connect.NewRequest(&userv1.GetUserRequest{Id: "ghost"}))
	fmt.Println(connect.CodeOf(err))
	// Output: not_found
}
```

## Review

The error mapping is correct when each failure path carries the code a client can
act on and a detail it can decode: `GetUser` on a missing id is `CodeNotFound`
with a `ResourceInfo` naming the id, and `CreateUser` on bad input is
`CodeInvalidArgument` with a `BadRequest` listing every offending field.
`TestNotFoundDetail` and `TestInvalidArgumentFieldViolations` prove the code and
the decoded detail; `Example_notFound` pins the code string. The auth interceptor
is correct when a missing or unknown token is `CodeUnauthenticated` and never
reaches `next`, while a valid-but-insufficient token is `CodePermissionDenied`;
`TestAuthInterceptorUnary` and `TestAuthorizeByProcedure` prove both.

The traps: returning a bare `errors.New`/`fmt.Errorf` from a handler collapses to
`CodeUnknown` and destroys the retry contract, so always wrap with
`connect.NewError` and a chosen code. Put remediation in a typed detail, not the
message string. And mind interceptor order — auth must precede logging in
`WithInterceptors`, or unauthenticated probes land in your success logs. This is a
bar-mode lesson: the offline gate cannot fetch `connectrpc.com/connect` or the
`errdetails` protos, so verify by generating the code and running
`go test -tags connect ./...` with network access.

## Resources

- [Connect for Go — Errors](https://connectrpc.com/docs/go/errors/) — `NewError`, codes, `NewErrorDetail`, and client-side inspection with `CodeOf` and `errors.As`.
- [Connect for Go — Interceptors](https://connectrpc.com/docs/go/interceptors/) — the `Interceptor` interface, `UnaryInterceptorFunc`, and `WithInterceptors` ordering.
- [`errdetails` well-known error details](https://pkg.go.dev/google.golang.org/genproto/googleapis/rpc/errdetails) — `ResourceInfo`, `BadRequest`, and `RetryInfo`.
- [`connectrpc.com/connect` reference](https://pkg.go.dev/connectrpc.com/connect) — `Code` constants, `Error`, `AnyRequest.Spec`, and `Request[T].Header`.

---

Back to [01-unary-service-multi-protocol.md](01-unary-service-multi-protocol.md) | Next: [03-server-streaming-and-in-process-testing.md](03-server-streaming-and-in-process-testing.md)
