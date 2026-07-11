# 1. Lambda Handler Patterns

Writing a Go Lambda function means satisfying a specific runtime contract: the
`lambda.Start` function validates your handler signature at startup and routes
JSON-encoded invocation payloads through it. The hard parts are choosing the
right signature for your use case, decoupling the routing logic from the Lambda
glue so it is testable without AWS, and propagating context correctly so the
function can honor its timeout deadline.

```text
lambda-handler/
  go.mod
  handler/
    handler.go
    handler_test.go
  cmd/lambda/main.go
  cmd/demo/main.go
```

The `handler` package contains all routing logic. It depends only on the
`github.com/aws/aws-lambda-go/events` types for its function signatures; the
`lambda.Start` call lives exclusively in `cmd/lambda/main.go`. Tests call the
handler directly with fabricated event structs — no AWS credentials, no network.

## Concepts

### The Handler Signature Contract

`lambda.Start` accepts a function value. The runtime validates the signature
and returns an error at startup (not at compile time) if the shape is wrong.
The full set of valid signatures is:

```text
func ()
func () error
func () (TOut, error)

func (TIn)
func (TIn) error
func (TIn) (TOut, error)

func (context.Context)
func (context.Context) error
func (context.Context) (TOut, error)

func (context.Context, TIn)
func (context.Context, TIn) error
func (context.Context, TIn) (TOut, error)
```

Rules: the handler must be a function; if it takes two arguments the first must
be `context.Context`; if it returns two values the last must be `error`. `TIn`
and `TOut` must be JSON-serializable (compatible with `encoding/json`).

Production functions almost always use `func(context.Context, TIn) (TOut, error)`.
Omitting `context.Context` means you cannot propagate the deadline, cancel
in-flight work when Lambda is about to reclaim the container, or read the
request ID for structured logging.

### Typed Events vs. json.RawMessage

The `events` package provides Go structs that match each AWS service's event
schema. Using `events.APIGatewayV2HTTPRequest` instead of `json.RawMessage`
gives you compile-time field access and eliminates manual unmarshaling. The
Lambda runtime unmarshals the incoming JSON payload into your `TIn` before
calling your handler.

The key fields on `APIGatewayV2HTTPRequest`:

```text
RawPath                string
RawQueryString         string
Headers                map[string]string
QueryStringParameters  map[string]string   // parsed from RawQueryString
RequestContext         APIGatewayV2HTTPRequestContext
Body                   string
IsBase64Encoded        bool
```

The response type `APIGatewayV2HTTPResponse` must carry `StatusCode` (int),
`Headers` (map[string]string), and `Body` (string). Omitting `StatusCode`
defaults to 0, which API Gateway rejects.

### Context, Deadline, and the Lambda Context Object

The `context.Context` passed to your handler contains two useful pieces of
information:

1. The **deadline**: `ctx.Deadline()` returns the time at which Lambda will
   terminate the function. Use `time.Until(deadline)` to detect imminent
   expiry and return a graceful timeout error instead of being killed mid-write.

2. The **Lambda context**: the `lambdacontext` package provides
   `lambdacontext.FromContext(ctx)`, which returns a `*LambdaContext` with
   `AwsRequestID`, `InvokedFunctionArn`, and client context fields. The
   request ID is the most useful field for structured logging — every log line
   that includes it is automatically correlated to the same invocation in
   CloudWatch.

### Separation: Handler vs. Glue

The handler function is a pure Go function. The only Lambda-specific code is
`lambda.Start(handler.Handle)` in `cmd/lambda/main.go`. This separation is the
single highest-value design decision: it makes the routing logic testable with
a plain `go test` call and lets you run the same logic as an HTTP server
(for local development) without changing the handler code.

### Global State and Warm Starts

Lambda reuses the execution environment for subsequent invocations ("warm
starts"). Variables declared outside the handler (package-level `var` block or
`func init()`) persist across invocations. This is the right place for
expensive one-time setup: SDK clients, database connection pools, parsed
configuration. Avoid storing per-request mutable state at the package level —
it leaks between invocations.

### The `bootstrap` Binary

For Go functions deployed as a `.zip` archive with the `provided.al2023`
runtime, the executable must be named `bootstrap`. Build with:

```bash
GOOS=linux GOARCH=arm64 go build -tags lambda -o bootstrap ./cmd/lambda
```

The `-tags lambda` build tag is one way to conditionally include
`lambda.Start` only in the real binary; without it the `cmd/demo` binary
runs the same handler logic as a local CLI.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/lambda-handler/handler
mkdir -p ~/go-exercises/lambda-handler/cmd/lambda
mkdir -p ~/go-exercises/lambda-handler/cmd/demo
cd ~/go-exercises/lambda-handler
go mod init example.com/lambda-handler
go get github.com/aws/aws-lambda-go@latest
```

This is a library-plus-binary project. The `handler` package is verified with
`go test`; `cmd/lambda` produces the deployable `bootstrap` binary.

### Exercise 1: The Handler Package

Create `handler/handler.go`:

```go
package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambdacontext"
)

// jsonResponse builds an APIGatewayV2HTTPResponse with a JSON body and the
// Content-Type header set to application/json.
func jsonResponse(status int, body any) (events.APIGatewayV2HTTPResponse, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return events.APIGatewayV2HTTPResponse{StatusCode: 500}, fmt.Errorf("handler: marshal response: %w", err)
	}
	return events.APIGatewayV2HTTPResponse{
		StatusCode: status,
		Headers:    map[string]string{"Content-Type": "application/json"},
		Body:       string(b),
	}, nil
}

// Handle is the Lambda handler. It accepts an API Gateway V2 (HTTP API) event
// and routes on RawPath. It uses the context deadline to detect imminent
// expiry and includes the Lambda request ID in every log line.
func Handle(ctx context.Context, req events.APIGatewayV2HTTPRequest) (events.APIGatewayV2HTTPResponse, error) {
	// Log remaining time before the Lambda deadline.
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		slog.InfoContext(ctx, "invocation", "path", req.RawPath, "remaining_ms", remaining.Milliseconds())
	}

	// Attach the Lambda request ID to log lines when available.
	if lc, ok := lambdacontext.FromContext(ctx); ok {
		slog.InfoContext(ctx, "request", "aws_request_id", lc.AwsRequestID)
	}

	switch req.RawPath {
	case "/health":
		return jsonResponse(200, map[string]string{"status": "ok"})
	case "/hello":
		name := req.QueryStringParameters["name"]
		if name == "" {
			name = "World"
		}
		return jsonResponse(200, map[string]string{"message": fmt.Sprintf("Hello, %s!", name)})
	default:
		return jsonResponse(404, map[string]string{"error": "not found", "path": req.RawPath})
	}
}
```

`Handle` is a plain Go function. The routing switch, the JSON helpers, and the
context-deadline check are all independently testable. `lambda.Start` is not
imported here.

### Exercise 2: The Lambda Entry Point

Create `cmd/lambda/main.go`:

```go
package main

import (
	"github.com/aws/aws-lambda-go/lambda"

	"example.com/lambda-handler/handler"
)

func main() {
	lambda.Start(handler.Handle)
}
```

`lambda.Start` blocks forever: it connects to the Lambda runtime API, reads
invocation payloads, calls `handler.Handle`, and writes responses back. On any
supported handler signature violation it panics at startup, not at invocation
time.

### Exercise 3: Tests

Create `handler/handler_test.go`. The tests fabricate `APIGatewayV2HTTPRequest`
values and call `handler.Handle` directly — no AWS infrastructure required:

```go
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"
)

// makeReq is a helper that builds a minimal APIGatewayV2HTTPRequest.
func makeReq(path string, params map[string]string) events.APIGatewayV2HTTPRequest {
	if params == nil {
		params = map[string]string{}
	}
	return events.APIGatewayV2HTTPRequest{
		RawPath:               path,
		QueryStringParameters: params,
	}
}

func TestHandleHealth(t *testing.T) {
	t.Parallel()

	resp, err := Handle(context.Background(), makeReq("/health", nil))
	if err != nil {
		t.Fatalf("Handle error: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("StatusCode = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Headers["Content-Type"]; ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var body map[string]string
	if err := json.Unmarshal([]byte(resp.Body), &body); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if body["status"] != "ok" {
		t.Fatalf("body[status] = %q, want ok", body["status"])
	}
}

func TestHandleHelloWithName(t *testing.T) {
	t.Parallel()

	resp, err := Handle(context.Background(), makeReq("/hello", map[string]string{"name": "Lambda"}))
	if err != nil {
		t.Fatalf("Handle error: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("StatusCode = %d, want 200", resp.StatusCode)
	}
	var body map[string]string
	if err := json.Unmarshal([]byte(resp.Body), &body); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	msg := body["message"]
	if msg != "Hello, Lambda!" {
		t.Fatalf("message = %q, want Hello, Lambda!", msg)
	}
}

func TestHandleHelloDefaultName(t *testing.T) {
	t.Parallel()

	resp, err := Handle(context.Background(), makeReq("/hello", nil))
	if err != nil {
		t.Fatalf("Handle error: %v", err)
	}
	var body map[string]string
	if err := json.Unmarshal([]byte(resp.Body), &body); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if body["message"] != "Hello, World!" {
		t.Fatalf("default name: message = %q, want Hello, World!", body["message"])
	}
}

func TestHandleUnknownPath(t *testing.T) {
	t.Parallel()

	resp, err := Handle(context.Background(), makeReq("/not-a-route", nil))
	if err != nil {
		t.Fatalf("Handle error: %v", err)
	}
	if resp.StatusCode != 404 {
		t.Fatalf("StatusCode = %d, want 404", resp.StatusCode)
	}
	var body map[string]string
	if err := json.Unmarshal([]byte(resp.Body), &body); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if body["path"] != "/not-a-route" {
		t.Fatalf("path in body = %q, want /not-a-route", body["path"])
	}
}

func TestHandleAllResponsesHaveContentType(t *testing.T) {
	t.Parallel()

	paths := []string{"/health", "/hello", "/not-found"}
	for _, path := range paths {
		path := path
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			resp, err := Handle(context.Background(), makeReq(path, nil))
			if err != nil {
				t.Fatalf("Handle(%q) error: %v", path, err)
			}
			if ct := resp.Headers["Content-Type"]; ct != "application/json" {
				t.Fatalf("Handle(%q): Content-Type = %q, want application/json", path, ct)
			}
		})
	}
}

func TestHandleDeadlineInContext(t *testing.T) {
	t.Parallel()

	// A context with a deadline should not change the response content.
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(5*time.Second))
	defer cancel()

	resp, err := Handle(ctx, makeReq("/health", nil))
	if err != nil {
		t.Fatalf("Handle error: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("StatusCode = %d, want 200", resp.StatusCode)
	}
}

// ExampleHandle_health shows the /health route response shape.
func ExampleHandle_health() {
	resp, err := Handle(context.Background(), makeReq("/health", nil))
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("%d %s\n", resp.StatusCode, resp.Body)
	// Output: 200 {"status":"ok"}
}

// Your turn: add TestHandleHelloTableDriven that uses a table of (name, want)
// pairs — at minimum ("Go", "Hello, Go!") and ("", "Hello, World!") — and
// asserts errors.Is(err, nil) alongside the message check. The test must call
// t.Parallel() and each sub-test must also call t.Parallel().
var _ = errors.New // keep errors imported for the "your turn" test
```

### Exercise 4: The Demo CLI

Create `cmd/demo/main.go`. This runs outside Lambda and shows the handler's
behavior through a small local CLI — useful for integration checks in a CI
environment that lacks AWS credentials:

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"

	"github.com/aws/aws-lambda-go/events"

	"example.com/lambda-handler/handler"
)

func main() {
	paths := []struct {
		path   string
		params map[string]string
	}{
		{"/health", nil},
		{"/hello", map[string]string{"name": "Demo"}},
		{"/hello", nil},
		{"/unknown", nil},
	}

	for _, tc := range paths {
		req := events.APIGatewayV2HTTPRequest{
			RawPath:               tc.path,
			QueryStringParameters: tc.params,
		}
		resp, err := handler.Handle(context.Background(), req)
		if err != nil {
			log.Printf("ERROR %s: %v", tc.path, err)
			os.Exit(1)
		}
		var body any
		if err := json.Unmarshal([]byte(resp.Body), &body); err != nil {
			log.Printf("unmarshal: %v", err)
			os.Exit(1)
		}
		pretty, _ := json.MarshalIndent(body, "", "  ")
		fmt.Printf("%-20s  %d  %s\n", tc.path, resp.StatusCode, pretty)
	}
}
```

Run it locally (no AWS credentials needed):

```bash
go run ./cmd/demo
```

Expected output:

```text
/health               200  {
  "status": "ok"
}
/hello                200  {
  "message": "Hello, Demo!"
}
/hello                200  {
  "message": "Hello, World!"
}
/unknown              404  {
  "error": "not found",
  "path": "/unknown"
}
```

## Common Mistakes

### Putting lambda.Start in the Handler Package

Wrong: importing `github.com/aws/aws-lambda-go/lambda` inside `handler.go`
and calling `lambda.Start` there.

What happens: the handler package now depends on the Lambda runtime loop. It
cannot be imported by `cmd/demo` or by tests without triggering the runtime
connection attempt, and the function blocks indefinitely in tests.

Fix: keep `lambda.Start` exclusively in `cmd/lambda/main.go`. The handler
package depends only on the `events` and `lambdacontext` packages, which are
pure data-type libraries and have no runtime side effects.

### Ignoring Context Deadline

Wrong:

```go
func Handle(ctx context.Context, req events.APIGatewayV2HTTPRequest) (events.APIGatewayV2HTTPResponse, error) {
	result, err := expensiveDownstreamCall()
	// ...
}
```

What happens: if the Lambda timeout fires while `expensiveDownstreamCall` is
running, Lambda kills the process. Any buffered writes to downstream systems
(databases, queues) are lost. The caller receives a timeout error with no
structured context about what was in progress.

Fix: pass `ctx` into every downstream call and select on `ctx.Done()` for
work that cannot accept a context argument. Check `ctx.Deadline()` before
starting long work to decide whether to short-circuit early.

```go
if deadline, ok := ctx.Deadline(); ok {
	if time.Until(deadline) < 200*time.Millisecond {
		return jsonResponse(503, map[string]string{"error": "insufficient time remaining"})
	}
}
```

### Storing Per-Request State in Package-Level Variables

Wrong:

```go
var lastRequestID string

func Handle(ctx context.Context, req events.APIGatewayV2HTTPRequest) (...) {
	lc, _ := lambdacontext.FromContext(ctx)
	lastRequestID = lc.AwsRequestID // leaks between invocations
	// ...
}
```

What happens: on a warm start the previous invocation's request ID is visible
to the next invocation before `Handle` runs. In concurrent-invocation scenarios
(Lambda scales horizontally, not within a single execution environment, but
correctness is easier to reason about if handler state is local).

Fix: keep all per-request state in local variables inside `Handle`. Use
package-level variables only for resources that are safe to share across
invocations (SDK clients, connection pools, parsed config).

### Using the Wrong Event Type for Your Trigger

Wrong: using `events.APIGatewayProxyRequest` (V1 / REST API) when the function
is triggered by an API Gateway HTTP API (V2).

What happens: the `Path` field on `APIGatewayProxyRequest` is populated by V1
but `RawPath` on `APIGatewayV2HTTPRequest` is populated by V2. Reading the
wrong field yields an empty string for every invocation. The routing logic
silently falls through to the 404 branch.

Fix: match the event struct to the trigger. V2 HTTP APIs use
`events.APIGatewayV2HTTPRequest`. V1 REST APIs use
`events.APIGatewayProxyRequest`. Check the API Gateway configuration in the
console or IaC to confirm which format your trigger sends.

## Verification

From `~/go-exercises/lambda-handler`:

```bash
test -z "$(gofmt -l ./handler/)"
go vet ./...
go test -count=1 -race ./handler/...
go build ./...
```

Build the deployable binary (cross-compiled for Lambda's `provided.al2023`
runtime on Graviton):

```bash
GOOS=linux GOARCH=arm64 go build -o bootstrap ./cmd/lambda
```

The `handler` tests run without AWS credentials. The `ExampleHandle_health`
function is verified automatically by `go test` against its `// Output:`
comment. All four gate commands must pass before deploying.

## Summary

- `lambda.Start` validates the handler signature at startup, not at compile time; the safest signature for production is `func(context.Context, TIn) (TOut, error)`.
- Keep `lambda.Start` in `cmd/lambda/main.go` and all routing logic in a separate package; the separation makes the handler testable without AWS credentials.
- Use typed event structs from the `events` package instead of `json.RawMessage`; the runtime unmarshals the payload before calling your handler.
- Pass `ctx` into all downstream calls and check `ctx.Deadline()` before starting long work; ignoring the deadline causes unclean shutdowns.
- Use `lambdacontext.FromContext(ctx)` to read the request ID for structured log correlation.
- Initialize SDK clients and other expensive resources at package level (or in `init()`); they persist across warm-start invocations.
- Name the deployable binary `bootstrap` for the `provided.al2023` runtime.

## What's Next

Next: [Lambda Cold Start Optimization](../02-lambda-cold-start-optimization/02-lambda-cold-start-optimization.md).

## Resources

- [AWS Lambda Go handler — official documentation](https://docs.aws.amazon.com/lambda/latest/dg/golang-handler.html)
- [pkg.go.dev: github.com/aws/aws-lambda-go/lambda](https://pkg.go.dev/github.com/aws/aws-lambda-go/lambda) — handler signatures, `StartWithOptions`, and options
- [pkg.go.dev: github.com/aws/aws-lambda-go/events](https://pkg.go.dev/github.com/aws/aws-lambda-go/events) — `APIGatewayV2HTTPRequest`, `APIGatewayV2HTTPResponse`, and all trigger types
- [pkg.go.dev: github.com/aws/aws-lambda-go/lambdacontext](https://pkg.go.dev/github.com/aws/aws-lambda-go/lambdacontext) — `LambdaContext` struct and `FromContext`
- [AWS Lambda execution environment lifecycle](https://docs.aws.amazon.com/lambda/latest/dg/lambda-runtime-environment.html) — init phase, warm starts, and global state semantics
