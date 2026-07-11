# 2. Lambda Cold Start Optimization

Lambda runs your Go binary in a managed execution environment. The environment has
two distinct phases: an **init phase** (one time per container) and an **invoke
phase** (once per invocation). Everything that runs in the init phase -- `init()`
functions, package-level variable initializers, and the code before
`lambda.Start()` -- pays the cold start tax. A warm invocation reuses the frozen
container and skips that tax entirely.

Go already has fast cold starts compared to Java or .NET. The hard part is
understanding *why*: knowing the phase boundary, what to hoist into init vs. what
to defer with `sync.Once`, and how to measure the difference. The structural
decision -- init-time vs. lazy vs. per-invocation -- has permanent implications
for latency and resource consumption.

```text
cold-start/
  go.mod
  handler/
    handler.go
    handler_test.go
  cmd/demo/
    main.go
```

The package under `handler/` contains the business logic as a testable library.
`cmd/demo/main.go` is the Lambda entry point; it calls `lambda.Start` with the
exported handler function. In a real deployment `cmd/demo/main.go` imports
`github.com/aws/aws-lambda-go/lambda`. Here the demo runs offline without that
dependency so `go build ./...` and `go test ./handler/...` are fully hermetic.

## Concepts

### The Init Phase and the Invoke Phase

When AWS receives a request for a function with no available execution
environment, it provisions a container, downloads the binary, and runs the
program from `main()`. The sequence is:

```
Extension init  ->  Runtime init  ->  Function init  ->  lambda.Start()
                                                               |
                                                       freeze container
                                                               |
                                               ... next invocation arrives ...
                                                               |
                                                       thaw container
                                                       handler() called
```

"Function init" is where your `init()` functions and package-level variable
initializers run. `lambda.Start()` blocks waiting for events; the init phase
ends when `lambda.Start()` is called. On a warm invocation, the container is
thawed from the frozen state, and the handler is called directly -- the init
phase does not repeat.

The AWS documentation states that the init phase is limited to 10 seconds for
on-demand concurrency. The billing REPORT log line shows `Init Duration` only on
cold invocations.

### What Belongs in Init

Any resource that:
- is always needed (every code path uses it),
- is expensive to construct (network round-trips, SDK configuration loading,
  connection establishment), and
- is safe to share across invocations (stateless or internally synchronized)

should be initialized in `init()` or as a package-level variable. The canonical
example is an AWS SDK client: constructing it involves loading credentials,
resolving endpoints, and building an HTTP transport. Paying that cost on every
invocation wastes several milliseconds.

Wrong -- per-invocation construction:

```go
func handler(ctx context.Context, event Event) (Response, error) {
	cfg, _ := config.LoadDefaultConfig(ctx)
	client := dynamodb.NewFromConfig(cfg)   // paid on EVERY invocation
	// ...
}
```

Fix -- init-time construction:

```go
var dbClient *dynamodb.Client

func init() {
	cfg, err := config.LoadDefaultConfig(context.Background())
	if err != nil {
		log.Fatalf("init: load SDK config: %v", err)
	}
	dbClient = dynamodb.NewFromConfig(cfg)
}
```

### Lazy Initialization with sync.Once

Some resources are only needed on certain code paths and are expensive enough
that paying their cost on every cold start is wasteful. `sync.Once` defers
construction to first use while guaranteeing it happens exactly once and is safe
for concurrent callers.

```go
var auditClientOnce sync.Once
var auditClientPtr unsafe.Pointer // *http.Client

func getAuditClient() *http.Client {
	auditClientOnce.Do(func() {
		c := &http.Client{
			Timeout: 5 * time.Second,
			Transport: &http.Transport{
				MaxIdleConnsPerHost: 10,
				IdleConnTimeout:     90 * time.Second,
			},
		}
		atomic.StorePointer(&auditClientPtr, unsafe.Pointer(c))
	})
	return (*http.Client)(atomic.LoadPointer(&auditClientPtr))
}
```

The `unsafe.Pointer` + `atomic.StorePointer` pattern is used here so that the
pointer write inside `Once.Do` is visible to concurrent readers via
`atomic.LoadPointer` without a data race. If you use a plain `*http.Client`
variable, the race detector will flag the write inside `Do` against a concurrent
read from `AuditClientInitialized`.

Three guarantees from `sync.Once.Do`:
1. The function runs at most once, regardless of how many goroutines call `Do`.
2. Any goroutine that calls `Do` after the first call completes waits until it
   finishes; they all see the initialized value.
3. If the function panics, `Do` considers it to have returned; subsequent callers
   do not retry -- they return immediately. This means a panicking initializer
   leaves the variable in its zero value permanently.

### Measuring the Phase Boundary

Capture `time.Now()` at package-level initialization to get the earliest
observable timestamp. The delta between that timestamp and the handler's entry
is the init duration on a cold start, and near-zero on a warm start.

```go
var programStart = time.Now()

// initNs stores the init duration in nanoseconds.  Written once by Init and
// read concurrently by Handle; atomic access avoids a race under -race.
var initNs atomic.Int64

func init() {
	// ... SDK setup ...
	initNs.Store(int64(time.Since(programStart)))
	slog.Info("init complete",
		slog.Duration("init_ms", InitDuration().Round(time.Millisecond)))
}

func handle(ctx context.Context, event Event) (Response, error) {
	invokeStart := time.Now()
	// ...
	initMs := initNs.Load() / int64(time.Millisecond)
	invokeMs := time.Since(invokeStart).Milliseconds()
	return Response{InitMs: initMs, InvokeMs: invokeMs}, nil
}
```

`atomic.Int64` is the correct tool here: `initNs` is written once (at init time)
and read on every invocation. Using a plain `time.Duration` variable would be a
data race under `-race` if any test calls `Init` concurrently with a handler
that reads it.

Lambda's REPORT log line emits the init duration separately from the invoke
duration, which is the ground truth. This instrumentation helps you reason about
where time is spent within your own code.

### Trade-offs and Failure Modes

**Init-time initialization fails fast**: if `config.LoadDefaultConfig` fails in
`init()`, the process calls `log.Fatalf` and exits. Lambda reports an
`INIT_REPORT Status: error` and retries on the next invocation. That is the
right behavior -- a misconfigured function should not silently limp with a nil
client.

**Lazy initialization on a hot path adds a lock**: `sync.Once` acquires a mutex
on the first call. On subsequent calls it only reads an atomic flag, so
contention is negligible. But if you put `sync.Once` on the critical path for
99% of invocations, you pay a mutex for a resource you should have initialized
eagerly.

**Global state leaks across invocations**: Lambda may reuse a container for
hours. Any mutable global state -- a counter, a cache, a half-written buffer --
persists. Design globals to be stateless or to be explicitly managed.

**Connections go idle**: Lambda purges idle TCP connections. After a container
sits frozen for several minutes, the underlying HTTP connections in the SDK's
transport pool may be dead. The SDK retries on connection failure, but setting
`IdleConnTimeout: 90 * time.Second` on the transport avoids a silent extra
round-trip on revival.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/cold-start/handler
mkdir -p ~/go-exercises/cold-start/cmd/demo
cd ~/go-exercises/cold-start
go mod init example.com/cold-start
```

This is a library plus an entry point. The handler package is verified with
`go test`; the cmd/demo package is a standalone program. Both run fully offline
with no external AWS dependencies.

### Exercise 1: Define the Handler Package

Create `handler/handler.go`. The package is named `handler`, not `main`, so the
test file can reach unexported identifiers.

```go
package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

// ErrEmptyKey is returned when the caller provides an empty lookup key.
var ErrEmptyKey = errors.New("handler: key must not be empty")

// ErrItemNotFound is returned when the store does not contain the requested key.
var ErrItemNotFound = errors.New("handler: item not found")

// programStart captures the process start time so we can compute the init
// duration even in tests (where there is no Lambda REPORT line).
var programStart = time.Now()

// initNs stores the init duration in nanoseconds.  Written once by Init and
// read concurrently by Handle; atomic access avoids a race under -race.
var initNs atomic.Int64

// Store is the interface the handler uses to fetch items.  In production this is
// backed by DynamoDB; in tests it is a MapStore.
type Store interface {
	Get(ctx context.Context, key string) (string, error)
}

// MapStore is an in-memory Store used in tests and demos.
type MapStore struct {
	items map[string]string
}

// NewMapStore returns a MapStore pre-populated with items.
func NewMapStore(items map[string]string) *MapStore {
	cp := make(map[string]string, len(items))
	for k, v := range items {
		cp[k] = v
	}
	return &MapStore{items: cp}
}

// Get returns the value for key or ErrItemNotFound.
func (m *MapStore) Get(_ context.Context, key string) (string, error) {
	v, ok := m.items[key]
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrItemNotFound, key)
	}
	return v, nil
}

// auditClientPtr holds the lazily-initialized *http.Client as an unsafe.Pointer
// so atomic.LoadPointer / StorePointer avoid a race between the Once initializer
// and AuditClientInitialized.
var auditClientPtr unsafe.Pointer // *http.Client, nil until first Audit invocation

// auditClientOnce guards the one-time initialization of the audit client.
var auditClientOnce sync.Once

// getAuditClient returns the shared audit HTTP client, initializing it on first
// call.  Subsequent calls return the same value with no lock contention.
func getAuditClient() *http.Client {
	auditClientOnce.Do(func() {
		c := &http.Client{
			Timeout: 5 * time.Second,
			Transport: &http.Transport{
				MaxIdleConnsPerHost: 10,
				IdleConnTimeout:     90 * time.Second,
			},
		}
		atomic.StorePointer(&auditClientPtr, unsafe.Pointer(c))
	})
	return (*http.Client)(atomic.LoadPointer(&auditClientPtr))
}

// AuditClientInitialized reports whether the audit client has been created yet.
func AuditClientInitialized() bool {
	return atomic.LoadPointer(&auditClientPtr) != nil
}

// Handler holds the dependencies injected at init time.
type Handler struct {
	store  Store
	logger *slog.Logger
}

// New constructs a Handler.  In a real Lambda main.go, store is a DynamoDB-backed
// implementation created in init().  In tests, store is a MapStore.
func New(store Store, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{store: store, logger: logger}
}

// Event is the input shape the Lambda handler accepts.
type Event struct {
	Key   string `json:"key"`
	Audit bool   `json:"audit,omitempty"`
}

// Response is the output shape the handler returns.
type Response struct {
	Key      string `json:"key"`
	Value    string `json:"value"`
	InitMs   int64  `json:"init_ms"`
	InvokeMs int64  `json:"invoke_ms"`
}

// Handle processes one Lambda invocation.
func (h *Handler) Handle(ctx context.Context, event Event) (Response, error) {
	invokeStart := time.Now()

	if event.Key == "" {
		return Response{}, ErrEmptyKey
	}

	// Optional audit path -- triggers lazy initialization.
	if event.Audit {
		c := getAuditClient()
		h.logger.Info("audit path active",
			slog.Bool("client_ready", c != nil))
	}

	value, err := h.store.Get(ctx, event.Key)
	if err != nil {
		return Response{}, err
	}

	invokeMs := time.Since(invokeStart).Milliseconds()
	initMs := initNs.Load() / int64(time.Millisecond)

	h.logger.Info("invocation complete",
		slog.String("key", event.Key),
		slog.Int64("invoke_ms", invokeMs),
		slog.Int64("init_ms", initMs))

	return Response{
		Key:      event.Key,
		Value:    value,
		InitMs:   initMs,
		InvokeMs: invokeMs,
	}, nil
}

// Init simulates the work that a real Lambda init() function performs: it
// records how long the init phase took from programStart.  Call this once at
// startup; in a real Lambda binary, this logic lives in init() and is called
// automatically.
func Init(store Store) {
	_ = store
	initNs.Store(int64(time.Since(programStart)))
}

// InitDuration returns the recorded init duration for tests and the demo.
func InitDuration() time.Duration { return time.Duration(initNs.Load()) }
```

`Handle` validates input, optionally triggers the lazy audit client, fetches from
the store, and returns structured timing data in the response. `initNs` is stored
and loaded atomically so parallel tests do not trigger the race detector.

### Exercise 2: Write the Test File

Create `handler/handler_test.go`. Tests that mutate package-level globals
(`auditClientPtr`, `auditClientOnce`, `initNs`) are marked sequential to avoid
data races with parallel tests that read those globals.

```go
package handler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestHandler(items map[string]string) *Handler {
	return New(NewMapStore(items), silentLogger())
}

// resetAuditClient resets the lazily-initialized audit client globals.
// Only safe to call from tests that do NOT run in parallel with tests that
// call getAuditClient or AuditClientInitialized.
func resetAuditClient() {
	atomic.StorePointer(&auditClientPtr, nil)
	auditClientOnce = sync.Once{}
}

func TestHandleReturnsValue(t *testing.T) {
	t.Parallel()

	h := newTestHandler(map[string]string{"k1": "hello"})
	resp, err := h.Handle(context.Background(), Event{Key: "k1"})
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if resp.Value != "hello" {
		t.Fatalf("Value = %q, want %q", resp.Value, "hello")
	}
	if resp.Key != "k1" {
		t.Fatalf("Key = %q, want %q", resp.Key, "k1")
	}
}

func TestHandleRejectsEmptyKey(t *testing.T) {
	t.Parallel()

	h := newTestHandler(nil)
	_, err := h.Handle(context.Background(), Event{Key: ""})
	if !errors.Is(err, ErrEmptyKey) {
		t.Fatalf("err = %v, want ErrEmptyKey", err)
	}
}

func TestHandleReturnsNotFound(t *testing.T) {
	t.Parallel()

	h := newTestHandler(map[string]string{"k1": "hello"})
	_, err := h.Handle(context.Background(), Event{Key: "missing"})
	if !errors.Is(err, ErrItemNotFound) {
		t.Fatalf("err = %v, want ErrItemNotFound", err)
	}
}

func TestHandleReturnsTimingFields(t *testing.T) {
	t.Parallel()

	h := newTestHandler(map[string]string{"k1": "v"})
	resp, err := h.Handle(context.Background(), Event{Key: "k1"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.InvokeMs < 0 {
		t.Fatalf("InvokeMs = %d, want >= 0", resp.InvokeMs)
	}
}

// TestAuditClientLazilyInitialized verifies that the audit HTTP client is not
// created until the first invocation that requests auditing.
// Sequential: mutates auditClientPtr and auditClientOnce.
func TestAuditClientLazilyInitialized(t *testing.T) {
	resetAuditClient()

	h := newTestHandler(map[string]string{"k1": "v"})
	if AuditClientInitialized() {
		t.Fatal("audit client should not be initialized before first Audit invocation")
	}
	if _, err := h.Handle(context.Background(), Event{Key: "k1", Audit: true}); err != nil {
		t.Fatal(err)
	}
	if !AuditClientInitialized() {
		t.Fatal("audit client should be initialized after first Audit invocation")
	}
}

// TestAuditClientIdempotent verifies that repeated calls to getAuditClient
// return the same pointer.
// Sequential: mutates auditClientPtr and auditClientOnce.
func TestAuditClientIdempotent(t *testing.T) {
	resetAuditClient()

	first := getAuditClient()
	second := getAuditClient()
	if first != second {
		t.Fatal("getAuditClient must return the same pointer on repeated calls")
	}
	h := newTestHandler(map[string]string{"k1": "v"})
	if _, err := h.Handle(context.Background(), Event{Key: "k1", Audit: true}); err != nil {
		t.Fatal(err)
	}
}

// TestInitDurationRecorded verifies that Init records a plausible duration.
// Sequential: writes to initNs.
func TestInitDurationRecorded(t *testing.T) {
	Init(NewMapStore(nil))
	d := InitDuration()
	if d < 0 {
		t.Fatalf("InitDuration = %v, want >= 0", d)
	}
	if d > time.Second {
		t.Fatalf("InitDuration = %v, want < 1s (no network in test)", d)
	}
}

// TestWarmInvocationReportsConsistentInitMs verifies that consecutive handler
// calls report the same InitMs -- confirming that init work is recorded once
// and does not re-run between invocations.
// Sequential: writes and reads initNs without other concurrent writers.
func TestWarmInvocationReportsConsistentInitMs(t *testing.T) {
	initNs.Store(int64(5 * time.Millisecond))

	h := newTestHandler(map[string]string{"k": "v"})
	r1, err := h.Handle(context.Background(), Event{Key: "k"})
	if err != nil {
		t.Fatal(err)
	}
	r2, err := h.Handle(context.Background(), Event{Key: "k"})
	if err != nil {
		t.Fatal(err)
	}

	if r1.InitMs != r2.InitMs {
		t.Fatalf("InitMs changed between invocations: %d vs %d (init must not repeat)",
			r1.InitMs, r2.InitMs)
	}
	if r1.InitMs != 5 {
		t.Fatalf("InitMs = %d, want 5 (5ms stored in initNs)", r1.InitMs)
	}
}

// ExampleNewMapStore is auto-verified by go test via the // Output: comment.
func ExampleNewMapStore() {
	store := NewMapStore(map[string]string{"hello": "world"})
	v, err := store.Get(context.Background(), "hello")
	if err != nil {
		panic(err)
	}
	fmt.Println(v)
	// Output: world
}

// Compile-time check: keep the unsafe import live.
var _ = unsafe.Pointer(nil)
```

### Exercise 3: The Lambda Entry Point

Create `cmd/demo/main.go`. This file exercises the exported API and doubles as
the offline demo:

```go
package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"example.com/cold-start/handler"
)

// store is initialized once during the init phase.
// In a real Lambda deployment, swap MapStore for a DynamoDB-backed Store and
// add: lambda.Start(h.Handle)
var store *handler.MapStore

var programStart = time.Now()

func init() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))

	store = handler.NewMapStore(map[string]string{
		"greeting": "hello",
		"env":      os.Getenv("APP_ENV"),
	})

	handler.Init(store)

	slog.Info("init complete",
		slog.Duration("init_duration", handler.InitDuration()),
		slog.Duration("wall_elapsed", time.Since(programStart)))
}

func main() {
	h := handler.New(store, slog.Default())
	ctx := context.Background()

	// Simulate a cold invocation.
	resp, err := h.Handle(ctx, handler.Event{Key: "greeting"})
	if err != nil {
		slog.Error("handler error", slog.String("err", err.Error()))
		os.Exit(1)
	}
	slog.Info("cold invocation",
		slog.String("key", resp.Key),
		slog.String("value", resp.Value),
		slog.Int64("init_ms", resp.InitMs),
		slog.Int64("invoke_ms", resp.InvokeMs))

	// Simulate a warm invocation: init does not repeat.
	resp2, err := h.Handle(ctx, handler.Event{Key: "greeting", Audit: true})
	if err != nil {
		slog.Error("handler error", slog.String("err", err.Error()))
		os.Exit(1)
	}
	slog.Info("warm invocation",
		slog.String("key", resp2.Key),
		slog.String("value", resp2.Value),
		slog.Int64("init_ms", resp2.InitMs),
		slog.Int64("invoke_ms", resp2.InvokeMs),
		slog.Bool("audit_client_ready", handler.AuditClientInitialized()))
}
```

Run the offline demo:

```bash
go run ./cmd/demo
```

The output shows one JSON log line from init and two from the handler. Both
handler lines show the same `init_ms`, and the second shows
`"audit_client_ready":true` because that invocation requested auditing.

## Common Mistakes

### Constructing SDK Clients Per Invocation

Wrong:

```go
func handler(ctx context.Context, event Event) (Response, error) {
	cfg, _ := config.LoadDefaultConfig(ctx)
	client := dynamodb.NewFromConfig(cfg)  // re-created every call
	// ...
}
```

What happens: `config.LoadDefaultConfig` performs credential chain resolution,
which may involve HTTP calls to the EC2 metadata service or ECS task role
endpoint. On a warm invocation this adds 20-100 ms of unnecessary latency and
creates a new connection pool every time, preventing connection reuse.

Fix: create the client once in `init()` or as a package-level variable; use it
across all invocations.

### Putting sync.Once on a Hot Path That Always Needs the Resource

Wrong:

```go
var (
	dbClient     *dynamodb.Client
	dbClientOnce sync.Once
)

func getDB() *dynamodb.Client {
	dbClientOnce.Do(func() { dbClient = dynamodb.NewFromConfig(cfg) })
	return dbClient
}

func handler(...) { getDB().GetItem(...) }  // called on every invocation
```

What happens: every invocation acquires a mutex on the first check. After the
first call, `sync.Once` uses an atomic flag, so contention is negligible -- but
the lazy pattern adds conceptual complexity where none is needed.

Fix: if a resource is always needed, initialize it eagerly in `init()`. Reserve
`sync.Once` for resources needed only on some code paths.

### Using a Plain Pointer Variable with sync.Once (Data Race)

Wrong:

```go
var auditClient *http.Client
var once sync.Once

func getAuditClient() *http.Client {
	once.Do(func() { auditClient = &http.Client{...} })
	return auditClient  // races with the write inside Do under -race
}
```

What happens: the write inside `once.Do` and a concurrent read of `auditClient`
from another goroutine are a data race that `-race` detects.

Fix: store the pointer via `atomic.StorePointer` inside `Do` and read it with
`atomic.LoadPointer`, as shown in Exercise 1. Or use a mutex-protected struct
instead of separate `sync.Once` + bare pointer.

### Logging Error on an Unrecoverable Init Failure

Wrong:

```go
func init() {
	cfg, err := config.LoadDefaultConfig(context.Background())
	if err != nil {
		slog.Error("failed to load config", slog.String("err", err.Error()))
		// continue with a nil client -- next handler call will panic
	}
	dbClient = dynamodb.NewFromConfig(cfg)
}
```

What happens: the init phase silently succeeds with a nil client. The first
invocation panics, Lambda catches the panic, and reports a runtime error instead
of an init error. Debugging is harder because the REPORT log shows no init
failure.

Fix: call `log.Fatalf` (or `os.Exit(1)`) on unrecoverable init errors. Lambda
reports `INIT_REPORT Status: error` and retries on the next invocation. A
function that cannot connect to its dependencies should not start at all.

### Storing Per-Invocation State in Global Variables

Wrong:

```go
var requestID string  // set in handler, read by helpers

func handler(ctx context.Context, event Event) (Response, error) {
	requestID = event.RequestID  // leaks to next invocation if handler panics
	// ...
}
```

What happens: if the handler exits early (panic, timeout), `requestID` retains
the value from the previous invocation. The next warm invocation reads stale
data.

Fix: pass per-invocation data as function arguments or through `context.Context`.
Never store per-request data in package-level variables.

## Verification

From `~/go-exercises/cold-start`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./handler/...
```

All four commands must complete cleanly. `go test` is the verification -- there is
no program to eyeball.

Run the offline demo:

```bash
go run ./cmd/demo
```

Your turn: add `TestHandleNotFoundMessageContainsKey` -- call
`h.Handle(ctx, Event{Key: "ghost"})`, assert `errors.Is(err, ErrItemNotFound)`,
and also confirm `err.Error()` contains the string `"ghost"` using
`strings.Contains`. This pins the contract that `Get` wraps the key in the error
message.

## Summary

- The Lambda init phase runs `init()` functions and package-level initializers
  once per container. The invoke phase runs the handler on every request.
- Hoist always-needed, expensive resources (SDK clients, connection pools) into
  `init()`. Warm invocations reuse them at zero cost.
- Use `sync.Once` only for resources needed on some code paths. It guarantees
  exactly-one execution and is safe under concurrent callers, but adds complexity
  where eager init would suffice.
- When `sync.Once` initializes a pointer, store it with `atomic.StorePointer` and
  read it with `atomic.LoadPointer` to avoid a data race under `-race`.
- If a `sync.Once` function panics, subsequent callers do not retry. An
  unrecoverable init failure should call `log.Fatalf` so Lambda reports it clearly.
- Store per-invocation state in local variables or `context.Context`, never in
  package-level globals -- containers are reused and stale globals leak across
  invocations.

## What's Next

Next: [SQS Message Handler](../03-sqs-message-handler/03-sqs-message-handler.md).

## Resources

- [AWS Lambda execution environment lifecycle](https://docs.aws.amazon.com/lambda/latest/dg/lambda-runtime-environment.html) -- authoritative description of init, invoke, and shutdown phases
- [Code best practices for Go Lambda functions](https://docs.aws.amazon.com/lambda/latest/dg/golang-handler.html#go-best-practices) -- AWS recommendation to initialize SDK clients outside the handler
- [sync.Once](https://pkg.go.dev/sync#Once) -- exact guarantees: executes once, panic semantics, concurrent-safe
- [sync/atomic and unsafe.Pointer](https://pkg.go.dev/sync/atomic#StorePointer) -- why atomic pointer ops are needed alongside sync.Once to avoid races
- [Optimizing static initialization](https://docs.aws.amazon.com/lambda/latest/dg/lambda-runtime-environment.html#static-initialization) -- AWS guidance on init-time vs. lazy loading trade-offs
