# Exercise 6: Context Propagation Chain

In a production Go web server, a single HTTP request flows through several
middleware layers before it reaches business logic, and each layer may enrich
the context, check it, or derive a tighter one. When the client disconnects or a
deadline fires, that cancellation signal has to reach the entire chain -- or the
work keeps running. The failure this exercise targets is quiet and expensive: one
layer that swaps the incoming context for its own `context.Background()` silently
severs every layer below it from the caller, so a query the client abandoned 100ms
ago keeps holding a database connection for another 400ms.

## What you'll build

```text
06-context-propagation-chain/
  main.go        a five-layer request flow -- auth, rate limiter, handler,
                 service, repository -- with one context threaded top to bottom
```

- Build: a complete request flow through five layers where context carries the request ID and cancellation from top to bottom.
- Implement: `AuthMiddleware`, `RateLimiter`, `OrderHandler`, `OrderService`, and `OrderRepository`; a tight-timeout demo that cancels mid-query; a broken-chain demo that shows what `context.Background()` destroys; and a full pipeline with early auth rejection.
- Verify: `go run main.go` for each step.

### Why the chain must stay unbroken

A single request moves through layers like this:

```
Client -> Auth Middleware -> Rate Limiter -> Handler -> Service -> Database
```

Each layer may add context values (request ID, authenticated user), check context
(rate limit exceeded?), or derive new contexts (add a per-query timeout). When the
client disconnects or a timeout fires, the cancellation signal must propagate
through the ENTIRE chain. If any layer breaks the chain by creating its own
`context.Background()`, all downstream work continues uselessly -- wasting
database connections, CPU, and memory.

This exercise builds the pattern you will use in every Go web server: middleware
enriches context, handlers pass it to services, services pass it to repositories,
and cancellation flows from top to bottom.

## Step 1 -- Define the Middleware and Service Layers

Build a complete request flow with five layers. Each layer accepts context, does its work, and passes context to the next layer:

```go
package main

import (
	"context"
	"fmt"
	"time"
)

const (
	requestTimeout       = 1 * time.Second
	authDelay            = 30 * time.Millisecond
	rateLimitCheckDelay  = 10 * time.Millisecond
	serviceValidateDelay = 40 * time.Millisecond
	databaseQueryDelay   = 80 * time.Millisecond
)

type requestIDKey struct{}
type userIDKey struct{}
type rateLimitKey struct{}

func requestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

func logLayer(ctx context.Context, layer, msg string) {
	fmt.Printf("[%-12s] req=%s | %s\n", layer, requestIDFrom(ctx), msg)
}

type AuthMiddleware struct{}

func (a *AuthMiddleware) Authenticate(ctx context.Context, token string) (context.Context, error) {
	logLayer(ctx, "auth", "validating token")
	time.Sleep(authDelay)

	if token == "" {
		return ctx, fmt.Errorf("auth: missing token")
	}
	if token != "valid-token-xyz" {
		return ctx, fmt.Errorf("auth: invalid token")
	}

	ctx = context.WithValue(ctx, userIDKey{}, "user-42")
	logLayer(ctx, "auth", "authenticated as user-42")
	return ctx, nil
}

type RateLimiter struct{}

func (r *RateLimiter) Check(ctx context.Context) (context.Context, error) {
	userID, _ := ctx.Value(userIDKey{}).(string)
	logLayer(ctx, "rate-limiter", fmt.Sprintf("checking rate for %s", userID))
	time.Sleep(rateLimitCheckDelay)

	ctx = context.WithValue(ctx, rateLimitKey{}, "50/min")
	logLayer(ctx, "rate-limiter", "within limits (50/min)")
	return ctx, nil
}

type OrderHandler struct {
	service *OrderService
}

func NewOrderHandler(service *OrderService) *OrderHandler {
	return &OrderHandler{service: service}
}

func (h *OrderHandler) Handle(ctx context.Context, orderID string) (string, error) {
	logLayer(ctx, "handler", fmt.Sprintf("processing order %s", orderID))

	select {
	case <-ctx.Done():
		return "", fmt.Errorf("handler: %w", ctx.Err())
	default:
	}

	return h.service.GetOrder(ctx, orderID)
}

type OrderService struct {
	repo *OrderRepository
}

func NewOrderService(repo *OrderRepository) *OrderService {
	return &OrderService{repo: repo}
}

func (s *OrderService) GetOrder(ctx context.Context, orderID string) (string, error) {
	logLayer(ctx, "service", "validating business rules")
	time.Sleep(serviceValidateDelay)

	select {
	case <-ctx.Done():
		return "", fmt.Errorf("service: %w", ctx.Err())
	default:
	}

	return s.repo.FindByID(ctx, orderID)
}

type OrderRepository struct{}

func NewOrderRepository() *OrderRepository {
	return &OrderRepository{}
}

func (r *OrderRepository) FindByID(ctx context.Context, orderID string) (string, error) {
	logLayer(ctx, "database", fmt.Sprintf("SELECT * FROM orders WHERE id = '%s'", orderID))

	select {
	case <-time.After(databaseQueryDelay):
		result := fmt.Sprintf("Order{id: %s, status: processing, user: %s}",
			orderID, ctx.Value(userIDKey{}))
		logLayer(ctx, "database", "query complete")
		return result, nil
	case <-ctx.Done():
		logLayer(ctx, "database", fmt.Sprintf("query cancelled: %v", ctx.Err()))
		return "", fmt.Errorf("database: %w", ctx.Err())
	}
}

func main() {
	ctx := context.WithValue(context.Background(), requestIDKey{}, "req-7f3a")
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	fmt.Printf("=== Request Flow (budget: 1s) ===\n\n")

	auth := &AuthMiddleware{}
	ctx, err := auth.Authenticate(ctx, "valid-token-xyz")
	if err != nil {
		fmt.Printf("REJECTED: %v\n", err)
		return
	}

	limiter := &RateLimiter{}
	ctx, err = limiter.Check(ctx)
	if err != nil {
		fmt.Printf("REJECTED: %v\n", err)
		return
	}

	repo := NewOrderRepository()
	svc := NewOrderService(repo)
	handler := NewOrderHandler(svc)

	result, err := handler.Handle(ctx, "ORD-2024-5678")
	if err != nil {
		fmt.Printf("\nFAILED: %v\n", err)
		return
	}

	fmt.Printf("\nSUCCESS: %s\n", result)
}
```

### Verification
```bash
go run main.go
```
Expected output:
```
=== Request Flow (budget: 1s) ===

[auth        ] req=req-7f3a | validating token
[auth        ] req=req-7f3a | authenticated as user-42
[rate-limiter] req=req-7f3a | checking rate for user-42
[rate-limiter] req=req-7f3a | within limits (50/min)
[handler     ] req=req-7f3a | processing order ORD-2024-5678
[service     ] req=req-7f3a | validating business rules
[database    ] req=req-7f3a | SELECT * FROM orders WHERE id = 'ORD-2024-5678'
[database    ] req=req-7f3a | query complete

SUCCESS: Order{id: ORD-2024-5678, status: processing, user: user-42}
```

Total: auth(30ms) + rate-limiter(10ms) + service(40ms) + database(80ms) = 160ms, well within the 1-second budget. The request ID is visible at every layer, and context flows through the entire chain.

## Step 2 -- Timeout Cancels All Downstream Layers

Reduce the timeout so cancellation happens during the database query, and observe how it propagates back through every layer:

```go
package main

import (
	"context"
	"fmt"
	"time"
)

const (
	tightTimeout   = 100 * time.Millisecond
	authLatency    = 30 * time.Millisecond
	rateLatency    = 10 * time.Millisecond
	serviceLatency = 40 * time.Millisecond
	dbLatency      = 200 * time.Millisecond
)

type requestIDKey struct{}

func requestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

type Server struct{}

func NewServer() *Server {
	return &Server{}
}

func (s *Server) authMiddleware(ctx context.Context) (context.Context, error) {
	fmt.Printf("[auth]       req=%s processing\n", requestIDFrom(ctx))
	time.Sleep(authLatency)
	return ctx, nil
}

func (s *Server) rateLimiter(ctx context.Context) error {
	fmt.Printf("[rate-limit] req=%s checking\n", requestIDFrom(ctx))
	time.Sleep(rateLatency)
	return nil
}

func (s *Server) handler(ctx context.Context) (string, error) {
	fmt.Printf("[handler]    req=%s starting\n", requestIDFrom(ctx))
	return s.service(ctx)
}

func (s *Server) service(ctx context.Context) (string, error) {
	fmt.Printf("[service]    req=%s business logic\n", requestIDFrom(ctx))
	time.Sleep(serviceLatency)
	return s.database(ctx)
}

func (s *Server) database(ctx context.Context) (string, error) {
	fmt.Printf("[database]   req=%s executing query (needs %v)\n", requestIDFrom(ctx), dbLatency)
	select {
	case <-time.After(dbLatency):
		return "data", nil
	case <-ctx.Done():
		fmt.Printf("[database]   req=%s CANCELLED: %v\n", requestIDFrom(ctx), ctx.Err())
		return "", fmt.Errorf("database: %w", ctx.Err())
	}
}

func (s *Server) ProcessRequest(ctx context.Context) (string, error) {
	ctx, _ = s.authMiddleware(ctx)
	_ = s.rateLimiter(ctx)
	return s.handler(ctx)
}

func main() {
	ctx := context.WithValue(context.Background(), requestIDKey{}, "req-timeout-demo")
	ctx, cancel := context.WithTimeout(ctx, tightTimeout)
	defer cancel()

	fmt.Printf("=== Request Flow (budget: %v, needs: 280ms) ===\n\n", tightTimeout)

	server := NewServer()
	result, err := server.ProcessRequest(ctx)
	if err != nil {
		fmt.Printf("\nRequest failed: %v\n", err)
		fmt.Println("The timeout propagated through handler -> service -> database")
	} else {
		fmt.Printf("Result: %s\n", result)
	}
}
```

### Verification
```bash
go run main.go
```
Expected output:
```
=== Request Flow (budget: 100ms, needs: 280ms) ===

[auth]       req=req-timeout-demo processing
[rate-limit] req=req-timeout-demo checking
[handler]    req=req-timeout-demo starting
[service]    req=req-timeout-demo business logic
[database]   req=req-timeout-demo executing query (needs 200ms)
[database]   req=req-timeout-demo CANCELLED: context deadline exceeded

Request failed: database: context deadline exceeded
The timeout propagated through handler -> service -> database
```

The timeout fires during the database query. Because every layer uses the same context, the cancellation signal reaches the deepest layer immediately. The error propagates back up through the chain.

## Step 3 -- What Happens When the Chain Breaks

This is the anti-pattern that causes real production incidents. One layer creates its own `context.Background()`, disconnecting downstream layers from the caller's context:

```go
package main

import (
	"context"
	"fmt"
	"time"
)

const (
	callerTimeout    = 100 * time.Millisecond
	slowQueryLatency = 500 * time.Millisecond
)

type requestIDKey struct{}

func requestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	if id == "" {
		return "MISSING"
	}
	return id
}

type BrokenService struct{}

func (b *BrokenService) Handler(ctx context.Context) (string, error) {
	fmt.Printf("[handler]  req=%s starting\n", requestIDFrom(ctx))
	return b.brokenServiceLayer(ctx)
}

func (b *BrokenService) brokenServiceLayer(ctx context.Context) (string, error) {
	fmt.Printf("[service]  req=%s (BROKEN: creating new Background)\n", requestIDFrom(ctx))
	newCtx := context.Background()
	return b.database(newCtx)
}

func (b *BrokenService) database(ctx context.Context) (string, error) {
	fmt.Printf("[database] req=%s executing slow query...\n", requestIDFrom(ctx))
	select {
	case <-time.After(slowQueryLatency):
		fmt.Printf("[database] req=%s query finished (but caller already gave up!)\n",
			requestIDFrom(ctx))
		return "data", nil
	case <-ctx.Done():
		fmt.Printf("[database] req=%s cancelled\n", requestIDFrom(ctx))
		return "", ctx.Err()
	}
}

func main() {
	ctx := context.WithValue(context.Background(), requestIDKey{}, "req-broken-chain")
	ctx, cancel := context.WithTimeout(ctx, callerTimeout)
	defer cancel()

	fmt.Printf("=== Broken Chain: service creates new Background() ===\n\n")

	svc := &BrokenService{}

	done := make(chan struct{})
	go func() {
		result, err := svc.Handler(ctx)
		if err != nil {
			fmt.Printf("\nResult: error=%v\n", err)
		} else {
			fmt.Printf("\nResult: %s\n", result)
		}
		close(done)
	}()

	<-ctx.Done()
	fmt.Printf("\n[caller] timeout fired at %v, but database is STILL running...\n", callerTimeout)

	<-done
	fmt.Println("\n[caller] database finally finished 400ms AFTER the caller gave up.")
	fmt.Println("[caller] That was a wasted database connection, CPU, and 400ms of work.")
	fmt.Println("[caller] FIX: pass ctx through the service instead of creating Background().")
}
```

### Verification
```bash
go run main.go
```
Expected output:
```
=== Broken Chain: service creates new Background() ===

[handler]  req=req-broken-chain starting
[service]  req=req-broken-chain (BROKEN: creating new Background)
[database] req=MISSING executing slow query...

[caller] timeout fired at 100ms, but database is STILL running...
[database] req=MISSING query finished (but caller already gave up!)

Result: data

[caller] database finally finished 400ms AFTER the caller gave up.
[caller] That was a wasted database connection, CPU, and 400ms of work.
[caller] FIX: pass ctx through the service instead of creating Background().
```

Two problems: (1) the request ID is lost because `Background()` has no values, and (2) the database keeps running for 500ms even though the caller gave up after 100ms. In a real system, this wastes a database connection that could serve another request.

## Step 4 -- Complete Middleware Chain with Auth Rejection

Show the full pattern including early rejection. If auth fails, no downstream layer runs:

```go
package main

import (
	"context"
	"fmt"
	"time"
)

const (
	defaultRequestTimeout = 1 * time.Second
	tightRequestTimeout   = 80 * time.Millisecond
	rateLimitDelay        = 5 * time.Millisecond
	handlerDelay          = 50 * time.Millisecond
	dbQueryDelay          = 60 * time.Millisecond
	tokenPrefixLength     = 10
)

type requestIDKey struct{}
type userIDKey struct{}

func requestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

func logRequest(ctx context.Context, layer, msg string) {
	fmt.Printf("[%-12s] req=%s | %s\n", layer, requestIDFrom(ctx), msg)
}

type RequestPipeline struct{}

func NewRequestPipeline() *RequestPipeline {
	return &RequestPipeline{}
}

func (p *RequestPipeline) authMiddleware(ctx context.Context, token string) (context.Context, error) {
	logRequest(ctx, "auth", fmt.Sprintf("checking token: %s...", token[:tokenPrefixLength]))
	if token != "valid-token" {
		logRequest(ctx, "auth", "REJECTED: invalid token")
		return ctx, fmt.Errorf("auth: invalid token")
	}
	ctx = context.WithValue(ctx, userIDKey{}, "user-42")
	logRequest(ctx, "auth", "OK")
	return ctx, nil
}

func (p *RequestPipeline) rateLimiter(ctx context.Context) error {
	logRequest(ctx, "rate-limiter", "checking quota")
	time.Sleep(rateLimitDelay)
	logRequest(ctx, "rate-limiter", "OK (45/50 remaining)")
	return nil
}

func (p *RequestPipeline) handler(ctx context.Context) (string, error) {
	logRequest(ctx, "handler", "processing")
	select {
	case <-ctx.Done():
		return "", fmt.Errorf("handler: %w", ctx.Err())
	case <-time.After(handlerDelay):
	}
	return p.databaseQuery(ctx)
}

func (p *RequestPipeline) databaseQuery(ctx context.Context) (string, error) {
	logRequest(ctx, "database", "executing query")
	select {
	case <-time.After(dbQueryDelay):
		logRequest(ctx, "database", "complete")
		return "Order{status: confirmed}", nil
	case <-ctx.Done():
		return "", fmt.Errorf("database: %w", ctx.Err())
	}
}

func (p *RequestPipeline) ProcessRequest(reqID string, token string, timeout time.Duration) {
	ctx := context.WithValue(context.Background(), requestIDKey{}, reqID)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ctx, err := p.authMiddleware(ctx, token)
	if err != nil {
		logRequest(ctx, "response", fmt.Sprintf("401: %v", err))
		return
	}

	if err := p.rateLimiter(ctx); err != nil {
		logRequest(ctx, "response", fmt.Sprintf("429: %v", err))
		return
	}

	result, err := p.handler(ctx)
	if err != nil {
		logRequest(ctx, "response", fmt.Sprintf("500: %v", err))
		return
	}

	logRequest(ctx, "response", fmt.Sprintf("200: %s", result))
}

func main() {
	pipeline := NewRequestPipeline()

	fmt.Println("=== Request 1: Happy path ===")
	pipeline.ProcessRequest("req-001", "valid-token", defaultRequestTimeout)

	fmt.Println("\n=== Request 2: Auth failure ===")
	pipeline.ProcessRequest("req-002", "bad-token!!", defaultRequestTimeout)

	fmt.Println("\n=== Request 3: Timeout ===")
	pipeline.ProcessRequest("req-003", "valid-token", tightRequestTimeout)
}
```

### Verification
```bash
go run main.go
```
Expected output:
```
=== Request 1: Happy path ===
[auth        ] req=req-001 | checking token: valid-toke...
[auth        ] req=req-001 | OK
[rate-limiter] req=req-001 | checking quota
[rate-limiter] req=req-001 | OK (45/50 remaining)
[handler     ] req=req-001 | processing
[database    ] req=req-001 | executing query
[database    ] req=req-001 | complete
[response    ] req=req-001 | 200: Order{status: confirmed}

=== Request 2: Auth failure ===
[auth        ] req=req-002 | checking token: bad-token!...
[auth        ] req=req-002 | REJECTED: invalid token
[response    ] req=req-002 | 401: auth: invalid token

=== Request 3: Timeout ===
[auth        ] req=req-003 | checking token: valid-toke...
[auth        ] req=req-003 | OK
[rate-limiter] req=req-003 | checking quota
[rate-limiter] req=req-003 | OK (45/50 remaining)
[handler     ] req=req-003 | processing
[response    ] req=req-003 | 500: handler: context deadline exceeded
```

Three scenarios: success, early rejection at auth, and timeout during processing. In all cases, context carries the request ID through every layer, and no downstream work runs after a failure.

## Common Mistakes

### Breaking the Chain with context.Background()
**Wrong:**
```go
func service(ctx context.Context, id string) (string, error) {
    newCtx := context.Background() // breaks the chain
    return repository(newCtx, id)
}
```
**Fix:** Always derive from the incoming context:
```go
func service(ctx context.Context, id string) (string, error) {
    return repository(ctx, id) // propagates caller's cancellation and values
}
```

### Not Checking Context in Each Layer
Each layer should check `ctx.Err()` or `ctx.Done()` before starting work. If the context is already cancelled when a layer is entered, proceeding wastes resources:
```go
func service(ctx context.Context) (string, error) {
    if ctx.Err() != nil {
        return "", fmt.Errorf("service: %w", ctx.Err())
    }
    // proceed...
}
```

### Wrapping Errors Without Layer Identification
**Wrong:**
```go
return "", err // caller has no idea which layer failed
```
**Fix:**
```go
return "", fmt.Errorf("service: %w", err) // clear error chain
```

## Review

The rule is that one context flows through every layer -- middleware to handler
to service to repository -- carrying both request-scoped values (request ID,
authenticated user, rate-limit info) and the cancellation signal. Middleware
enriches the context and the layers below consume it, so a timeout or client
disconnect at the top propagates to the deepest layer immediately, and each layer
that checks `ctx.Done()` before working avoids starting effort that is already
doomed. Early rejection -- a failed auth check, an exceeded rate limit -- stops
the chain before any downstream layer runs at all. The anti-pattern is the one
Step 3 dramatizes: a layer that calls `context.Background()` instead of deriving
from its argument silently loses the request ID and disconnects everything below
it from the caller's deadline, so a query keeps burning a database connection
long after the client gave up. Wrapping errors with a layer prefix (`fmt.Errorf("service: %w", err)`)
keeps the failure traceable back to where it happened.

To prove the pattern is yours, build a four-layer chain -- gateway, auth, handler,
storage -- driven by a slice of per-layer durations, and run it under two budgets:
a generous one where every layer finishes, and a tight one that cancels at
storage. You should be able to predict, before running, which layer prints the
cancellation and why the request ID survives at every layer while a
`context.Background()` swap would have erased it.

## Resources

- [Go Blog: Context](https://go.dev/blog/context) -- the canonical explanation of context propagation and cancellation through a request.
- [Go Code Review: Context](https://go.dev/wiki/CodeReviewComments#contexts) -- the review conventions for passing context as the first argument and never storing it in a struct.
- [Package context](https://pkg.go.dev/context) -- the API reference for `WithValue`, `WithTimeout`, `Done`, and `Err` used at every layer here.

---

Back to [Concurrency](../../concurrency.md) | Next: [07-context-aware-long-worker](../07-context-aware-long-worker/07-context-aware-long-worker.md)
