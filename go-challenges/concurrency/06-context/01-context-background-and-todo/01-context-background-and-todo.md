# Exercise 1: Context Background and TODO

Every Go service that handles HTTP requests, drains queues, or talks to a
database needs a way to carry deadlines, cancellation signals, and request-scoped
metadata down the call stack, and that mechanism is the `context` package. Every
context tree starts at one of two roots -- `context.Background()` or
`context.TODO()` -- and if you start the tree wrong, or grow a fresh root deep in
the stack, cancellation and deadlines silently stop propagating. This exercise
builds the layered flow that keeps them working and shows the anti-patterns that
break it.

## What you'll build

```text
01-context-background-and-todo/
  main.go        main() owning Background, TODO as a placeholder, the
                 deep-Background anti-pattern, and a handler -> service ->
                 repository chain that threads one context through
```

- Build: a layered order service that propagates a single root context from `main` down every layer.
- Implement: `OrderService`/`OrderRepository` methods taking `ctx` first, a `NotificationService.Send` using `context.TODO()`, and a `FetchBroken`-versus-`FetchCorrect` contrast that shows the broken cancellation chain.
- Verify: `go run main.go`.

### Why the root you choose matters

At the root of every context tree sits one of two functions:

- **`context.Background()`** is the standard root. Use it in `main()`, initialization code, tests, and as the top-level context for incoming requests. It signals: "this is the starting point of an operation."
- **`context.TODO()`** is a placeholder. Use it when you are writing new code that will eventually receive a context from a caller, but that caller does not pass one yet. It signals: "I know a context belongs here, but I have not wired it up yet."

Both return identical empty contexts that are never cancelled, have no deadline, and carry no values. The difference is purely semantic -- a signal to the reader (and to static analysis tools) about intent.

Understanding these roots matters because every `WithCancel`, `WithTimeout`, `WithDeadline`, and `WithValue` you will use in production derives from one of them. If you start the tree wrong, cancellation and deadlines will not propagate correctly.

## Step 1 -- The Correct Entry Point: main() Owns Background

In a real service, `main()` creates the root context and passes it down. This is the only place where `context.Background()` should appear in application code. Build a simple order processing service to see this pattern:

```go
package main

import (
	"context"
	"fmt"
	"time"
)

const orderProcessingDelay = 50 * time.Millisecond
const orderPersistenceDelay = 30 * time.Millisecond

type OrderService struct{}

func NewOrderService() *OrderService {
	return &OrderService{}
}

func (s *OrderService) Process(ctx context.Context, orderID string) error {
	if ctx.Err() != nil {
		return fmt.Errorf("context already cancelled: %w", ctx.Err())
	}
	fmt.Printf("[order-service] processing order %s\n", orderID)
	time.Sleep(orderProcessingDelay)

	return s.persist(ctx, orderID)
}

func (s *OrderService) persist(ctx context.Context, orderID string) error {
	if ctx.Err() != nil {
		return fmt.Errorf("context already cancelled: %w", ctx.Err())
	}
	fmt.Printf("[repository]    saving order %s to database\n", orderID)
	time.Sleep(orderPersistenceDelay)
	fmt.Printf("[repository]    order %s saved\n", orderID)
	return nil
}

func printContextInfo(ctx context.Context) {
	fmt.Printf("Root context type:   %T\n", ctx)
	fmt.Printf("Root context string: %s\n", ctx)
	fmt.Printf("Root context Err:    %v\n", ctx.Err())
	fmt.Printf("Root context Done:   %v (nil = never cancelled)\n", ctx.Done())
	fmt.Println()
}

func main() {
	ctx := context.Background()
	printContextInfo(ctx)

	svc := NewOrderService()
	if err := svc.Process(ctx, "ORD-2024-1001"); err != nil {
		fmt.Printf("Error: %v\n", err)
	}
}
```

### Verification
```bash
go run main.go
```
Expected output:
```
Root context type:   context.backgroundCtx
Root context string: context.Background
Root context Err:    <nil>
Root context Done:   <nil> (nil = never cancelled)

[order-service] processing order ORD-2024-1001
[repository]    saving order ORD-2024-1001 to database
[repository]    order ORD-2024-1001 saved
```

The background context has no deadline, no error, and a nil `Done()` channel. A nil `Done()` channel blocks forever on receive, which is correct because a root context should never be cancelled. The context flows from `main` -> `Process` -> `persist`, establishing the chain that cancellation and deadlines will follow.

## Step 2 -- Where context.TODO() Belongs

Imagine you are adding a new notification feature to the order service. The caller does not pass a context yet because you have not refactored the API. `context.TODO()` marks this spot for future work:

```go
package main

import (
	"context"
	"fmt"
)

type NotificationService struct{}

func NewNotificationService() *NotificationService {
	return &NotificationService{}
}

// Send is new code. The caller (an event handler) does not
// pass a context yet. TODO() marks this as "needs proper context wiring."
func (n *NotificationService) Send(userID string, message string) error {
	ctx := context.TODO()
	return n.deliverEmail(ctx, userID, message)
}

func (n *NotificationService) deliverEmail(ctx context.Context, userID string, message string) error {
	if ctx.Err() != nil {
		return fmt.Errorf("context cancelled: %w", ctx.Err())
	}
	fmt.Printf("[email] sending to user %s: %q (via %s)\n", userID, message, ctx)
	return nil
}

func compareRootContexts() {
	bg := context.Background()
	todo := context.TODO()

	fmt.Printf("Background: %s\n", bg)
	fmt.Printf("TODO:       %s\n", todo)
	fmt.Printf("Same Err:   %v\n", bg.Err() == todo.Err())
	fmt.Printf("Same Done:  %v\n", bg.Done() == todo.Done())
	fmt.Println()
}

func main() {
	compareRootContexts()

	notifier := NewNotificationService()
	if err := notifier.Send("user-42", "Your order has shipped"); err != nil {
		fmt.Printf("Error: %v\n", err)
	}
}
```

### Verification
```bash
go run main.go
```
Expected output:
```
Background: context.Background
TODO:       context.TODO
Same Err:   true
Same Done:  true

[email] sending to user user-42: "Your order has shipped" (via context.TODO)
```

`Background()` and `TODO()` are structurally identical. The only difference is the string representation. Static analysis tools like `go vet` and `staticcheck` can flag `TODO()` contexts that remain in production code, reminding you to finish the refactor.

## Step 3 -- The Anti-Pattern: Background() Deep in the Call Stack

This is the most common context mistake in production code. When a function creates its own `context.Background()` instead of accepting one from its caller, it breaks the cancellation chain. Deadlines, timeouts, and cancel signals from the caller silently stop working:

```go
package main

import (
	"context"
	"fmt"
	"time"
)

const userFetchDelay = 100 * time.Millisecond
const simulatedDeadline = 50 * time.Millisecond

type UserRepository struct{}

func NewUserRepository() *UserRepository {
	return &UserRepository{}
}

// FetchBroken creates its own root context, ignoring the caller's cancellation.
func (r *UserRepository) FetchBroken(userID string) (string, error) {
	ctx := context.Background()
	_ = ctx
	fmt.Printf("[broken-repo]  fetching user %s (ignores cancellation)\n", userID)
	time.Sleep(userFetchDelay)
	return "Alice", nil
}

// FetchCorrect accepts the caller's context, propagating cancellation.
func (r *UserRepository) FetchCorrect(ctx context.Context, userID string) (string, error) {
	if ctx.Err() != nil {
		return "", fmt.Errorf("fetch user: %w", ctx.Err())
	}
	fmt.Printf("[correct-repo] fetching user %s (respects cancellation)\n", userID)
	time.Sleep(userFetchDelay)
	return "Alice", nil
}

func demonstrateBrokenChain() {
	ctx, cancel := context.WithTimeout(context.Background(), simulatedDeadline)
	defer cancel()

	time.Sleep(60 * time.Millisecond)
	fmt.Printf("Context state: %v\n\n", ctx.Err())

	repo := NewUserRepository()

	name, err := repo.FetchBroken("user-42")
	fmt.Printf("Broken result:  name=%s, err=%v (wasted work!)\n\n", name, err)

	name, err = repo.FetchCorrect(ctx, "user-42")
	fmt.Printf("Correct result: name=%q, err=%v (failed fast)\n", name, err)
}

func main() {
	demonstrateBrokenChain()
}
```

### Verification
```bash
go run main.go
```
Expected output:
```
Context state: context deadline exceeded

[broken-repo]  fetching user user-42 (ignores cancellation)
Broken result:  name=Alice, err=<nil> (wasted work!)

Correct result: name="", err=fetch user: context deadline exceeded (failed fast)
```

The broken function does 100ms of work even though the deadline already expired. In a real service, this means database queries, HTTP calls, and CPU time are wasted on requests that nobody is waiting for. Multiply this by thousands of requests per second, and it becomes a serious resource leak.

## Step 4 -- Complete Layered Service with Proper Context Flow

Build the full pattern you will use in every Go service: `main` creates the root, and context flows through every layer. Each layer checks context before doing work:

```go
package main

import (
	"context"
	"fmt"
	"time"
)

const (
	handlerTimeout       = 500 * time.Millisecond
	serviceValidationDelay = 30 * time.Millisecond
	repositoryQueryDelay   = 50 * time.Millisecond
)

type OrderRepository struct{}

func NewOrderRepository() *OrderRepository {
	return &OrderRepository{}
}

func (r *OrderRepository) FindByID(ctx context.Context, orderID string) (string, error) {
	fmt.Printf("[repository] querying database for order %s\n", orderID)
	select {
	case <-time.After(repositoryQueryDelay):
		result := fmt.Sprintf("Order{id: %s, status: shipped}", orderID)
		fmt.Printf("[repository] query complete\n")
		return result, nil
	case <-ctx.Done():
		return "", fmt.Errorf("repository: %w", ctx.Err())
	}
}

type OrderService struct {
	repo *OrderRepository
}

func NewOrderService(repo *OrderRepository) *OrderService {
	return &OrderService{repo: repo}
}

func (s *OrderService) GetOrder(ctx context.Context, orderID string) (string, error) {
	fmt.Printf("[service]    validating order %s\n", orderID)
	time.Sleep(serviceValidationDelay)
	if ctx.Err() != nil {
		return "", fmt.Errorf("service: %w", ctx.Err())
	}
	return s.repo.FindByID(ctx, orderID)
}

type APIHandler struct {
	orderSvc *OrderService
}

func NewAPIHandler(orderSvc *OrderService) *APIHandler {
	return &APIHandler{orderSvc: orderSvc}
}

func (h *APIHandler) HandleGetOrder(ctx context.Context, orderID string) (string, error) {
	fmt.Printf("[handler]    received request for order %s\n", orderID)
	if ctx.Err() != nil {
		return "", fmt.Errorf("handler: %w", ctx.Err())
	}
	return h.orderSvc.GetOrder(ctx, orderID)
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), handlerTimeout)
	defer cancel()

	repo := NewOrderRepository()
	svc := NewOrderService(repo)
	handler := NewAPIHandler(svc)

	result, err := handler.HandleGetOrder(ctx, "ORD-2024-1001")
	if err != nil {
		fmt.Printf("\nError: %v\n", err)
		return
	}
	fmt.Printf("\nResult: %s\n", result)
}
```

### Verification
```bash
go run main.go
```
Expected output:
```
[handler]    received request for order ORD-2024-1001
[service]    validating order ORD-2024-1001
[repository] querying database for order ORD-2024-1001
[repository] query complete

Result: Order{id: ORD-2024-1001, status: shipped}
```

This is the pattern you will see in every well-structured Go service. Context flows `main -> handler -> service -> repository`. If the timeout fires at any point, every downstream layer detects it and stops. No wasted database queries, no wasted CPU.

## Common Mistakes

### Using context.TODO() Permanently
**Wrong:** Leaving `context.TODO()` in production code indefinitely.

**Why it matters:** `TODO()` signals "I need to figure out the right context later." If it stays, it means cancellation and deadlines are not propagated through that code path, which leads to resource leaks under load. A function using `TODO()` will keep running even when the caller has given up on the result.

**Fix:** Replace `TODO()` with a properly propagated context once the caller is refactored. Treat every `TODO()` like a `// TODO` comment -- it is technical debt that must be resolved.

### Creating Background() Inside a Helper
**Wrong:**
```go
package main

import (
	"context"
	"fmt"
)

func queryDatabase(query string) error {
	ctx := context.Background() // new root -- isolated from caller
	_ = ctx
	fmt.Println("querying...")
	return nil
}

func main() {
	queryDatabase("SELECT * FROM orders")
}
```
**Fix:**
```go
package main

import (
	"context"
	"fmt"
)

func queryDatabase(ctx context.Context, query string) error {
	_ = ctx // uses caller's context -- cancellation propagates
	fmt.Println("querying...")
	return nil
}

func main() {
	queryDatabase(context.Background(), "SELECT * FROM orders")
}
```

When a function creates its own `context.Background()`, the caller has no way to cancel or set a deadline on that operation. In a server handling thousands of requests, this leads to goroutine pileups when downstream services are slow.

### Storing Context in a Struct
**Wrong:**
```go
package main

import "context"

type OrderService struct {
	ctx context.Context // do not do this
}

func main() {
	_ = OrderService{ctx: context.Background()}
}
```
**Why it matters:** Contexts are request-scoped. The service outlives any individual request. A context stored in a struct becomes stale immediately after the request it was created for ends, leading to cancelled contexts being reused for new requests.

**Fix:** Pass context as the first parameter of each method:
```go
package main

import (
	"context"
	"fmt"
)

type OrderService struct{}

func (s *OrderService) GetOrder(ctx context.Context, id string) (string, error) {
	if ctx.Err() != nil {
		return "", fmt.Errorf("get order: %w", ctx.Err())
	}
	return fmt.Sprintf("Order{id: %s}", id), nil
}

func main() {
	svc := &OrderService{}
	result, _ := svc.GetOrder(context.Background(), "ORD-001")
	fmt.Println(result)
}
```

## Review

`context.Background()` is the standard root -- reserved for `main()`,
initialization, and tests -- while `context.TODO()` is a structurally identical
placeholder for code that will receive a real context once its caller is
refactored. Both return empty contexts that are never cancelled, carry no
deadline and no values, and expose a nil `Done()` channel that blocks forever on
receive, which is exactly right for a root that should never fire. Getting the
root right matters because everything downstream depends on it: context flows
from the entry point through handler, service, and repository, and each layer
checks it before doing work, so a timeout or cancel at the top stops every layer
beneath it. The two ways to break that chain are creating a fresh `Background()`
inside a helper -- which isolates the helper from the caller's cancellation --
and storing a context in a struct, which staples a request-scoped value to
something that outlives the request.

You should be able to build the proof yourself without re-reading: a three-layer
service -- handler to validator to storage -- where each layer prints the context
it received and returns early when `ctx.Err()` is non-nil. Call it once with
`context.Background()` and watch all three layers run through to a saved result;
call it again with a context you cancel immediately and watch the handler reject
the request before the validator or storage is ever reached. If cancellation is
detected at only some layers, the context is not being threaded as the first
argument of every call the way the convention requires.

## Resources
- [Go Blog: Go Concurrency Patterns: Context](https://go.dev/blog/context) -- how context propagates cancellation and deadlines across API boundaries.
- [Package context](https://pkg.go.dev/context) -- Background, TODO, WithCancel, WithTimeout, and the Context interface itself.
- [Go Proverb: Pass context.Context as the first argument](https://go-proverbs.github.io/) -- the convention that keeps the cancellation chain intact.

---

Back to [Concurrency](../../concurrency.md) | Next: [02-context-withcancel](../02-context-withcancel/02-context-withcancel.md)
