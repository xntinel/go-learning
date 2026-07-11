# Exercise 5: Errgroup vs WaitGroup -- Order Processing vs Notification Dispatch

Developers who learn the errgroup pattern often reach for it everywhere, but
concurrency comes in two fundamentally different shapes. All-or-nothing work --
charging a batch of orders where any invalid order must abort the whole batch --
needs the first failure to cancel its siblings. Best-effort work -- pushing
notifications to a thousand users where one dead device token must not stop the
other 999 -- needs each task to succeed or fail on its own. Pick the wrong model
and the damage is real: errgroup for notifications lets one bad token silence
everyone, and a plain WaitGroup for a batch leaves the accounting system
half-processed and inconsistent. This exercise builds the same problem both ways
so the choice becomes mechanical.

## What you'll build

```text
05-errgroup-vs-waitgroup/
  main.go        batch order processing (all-or-nothing), notification
                 dispatch (best-effort), a side-by-side comparison, and
                 a deploy pipeline that uses both
```

- Build: the same concurrent workload implemented as all-or-nothing (errgroup-style) and as best-effort (plain WaitGroup).
- Implement: an `OrderProcessor` using `WaitGroup` + `sync.Once` + `context.WithCancel` to abort on first error; a `NotificationDispatcher` that tolerates individual failures; a deploy pipeline that pairs both.
- Verify: `go run main.go` for each step.

### Why the two models are not interchangeable

There are two fundamentally different concurrency models:

1. **All-or-nothing**: Processing a batch of orders where any invalid order should abort the entire batch (database transaction semantics). If order 3 out of 10 fails validation, you do not want to process orders 4-10 because the batch is already broken.

2. **Best-effort**: Sending push notifications to 1000 users where one user's failed notification should not prevent the other 999 from receiving theirs. Each notification is independent.

Using the wrong model leads to real business impact: the errgroup pattern for notifications means one bad device token blocks all notifications. WaitGroup for batch orders means partial processing with inconsistent state.

## Step 1 -- All-or-Nothing: Batch Order Processing (Errgroup Pattern)

A payment processor validates and charges a batch of orders. If any order fails validation (insufficient funds, expired card, fraud flag), the entire batch must be aborted because the accounting system expects atomic batches:

```go
package main

import (
	"context"
	"fmt"
	"sync"
	"time"
)

const maxOrderAmount = 10_000

type OrderStatus string

const (
	StatusCharged   OrderStatus = "charged"
	StatusFailed    OrderStatus = "failed"
	StatusCancelled OrderStatus = "cancelled"
)

type Order struct {
	ID       int
	Customer string
	Amount   float64
}

type OrderProcessor struct {
	orders []Order
}

func NewOrderProcessor(orders []Order) *OrderProcessor {
	return &OrderProcessor{orders: orders}
}

func (op *OrderProcessor) ProcessBatch() ([]OrderStatus, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	var once sync.Once
	var firstErr error

	results := make([]OrderStatus, len(op.orders))

	for i, order := range op.orders {
		wg.Add(1)
		go func() {
			defer wg.Done()

			select {
			case <-ctx.Done():
				results[i] = StatusCancelled
				return
			default:
			}

			if err := op.validateAndCharge(ctx, order); err != nil {
				once.Do(func() {
					firstErr = err
					cancel()
				})
				results[i] = StatusFailed
				return
			}
			results[i] = StatusCharged
		}()
	}

	wg.Wait()
	return results, firstErr
}

func (op *OrderProcessor) validateAndCharge(ctx context.Context, order Order) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(time.Duration(30+order.ID%5*20) * time.Millisecond):
	}

	if order.Amount <= 0 {
		return fmt.Errorf("order %d: invalid amount $%.2f (must be positive)", order.ID, order.Amount)
	}
	if order.Amount > maxOrderAmount {
		return fmt.Errorf("order %d: amount $%.2f exceeds batch limit", order.ID, order.Amount)
	}

	fmt.Printf("  Charged order %d: $%.2f from %s\n", order.ID, order.Amount, order.Customer)
	return nil
}

func printBatchResults(orders []Order, results []OrderStatus, elapsed time.Duration, err error) {
	fmt.Printf("\nBatch results (in %v):\n", elapsed)
	for i, order := range orders {
		fmt.Printf("  Order %d (%s, $%.2f): %s\n", order.ID, order.Customer, order.Amount, results[i])
	}

	if err != nil {
		fmt.Printf("\nBATCH ABORTED: %v\n", err)
		fmt.Println("Action: Roll back all charged orders, notify accounting")
	} else {
		fmt.Println("\nBatch completed successfully")
	}
}

func main() {
	orders := []Order{
		{ID: 1001, Customer: "alice", Amount: 99.99},
		{ID: 1002, Customer: "bob", Amount: 249.50},
		{ID: 1003, Customer: "charlie", Amount: -50.00}, // INVALID: negative amount
		{ID: 1004, Customer: "diana", Amount: 175.00},
		{ID: 1005, Customer: "eve", Amount: 89.99},
	}

	processor := NewOrderProcessor(orders)

	fmt.Println("=== Batch Order Processing (all-or-nothing) ===")
	fmt.Println("\n--- Processing batch (errgroup pattern) ---")
	start := time.Now()

	results, err := processor.ProcessBatch()

	printBatchResults(orders, results, time.Since(start).Round(time.Millisecond), err)
}
```

**Expected output:**
```
=== Batch Order Processing (all-or-nothing) ===

--- Processing batch (errgroup pattern) ---
  Charged order 1001: $99.99 from alice
  Charged order 1002: $249.50 from bob

Batch results (in ~90ms):
  Order 1001 (alice, $99.99): charged
  Order 1002 (bob, $249.50): charged
  Order 1003 (charlie, $-50.00): failed
  Order 1004 (diana, $175.00): cancelled
  Order 1005 (eve, $89.99): cancelled

BATCH ABORTED: order 1003: invalid amount $-50.00 (must be positive)
Action: Roll back all charged orders, notify accounting
```

Order 1003 fails validation, which cancels the context. Orders 1004 and 1005 see the cancelled context and stop immediately. The batch is aborted as a unit. This is the correct behavior for financial transactions.

## Step 2 -- Best-Effort: Notification Dispatch (WaitGroup)

A notification system sends push notifications to all users. One user's expired device token must NOT prevent other users from receiving their notifications. Each notification is independent:

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

const pushNotificationLatency = 50 * time.Millisecond

type Notification struct {
	UserID  string
	Message string
}

type SendResult struct {
	UserID string
	OK     bool
	Error  string
}

type NotificationDispatcher struct {
	notifications []Notification
	mu            sync.Mutex
	results       []SendResult
}

func NewNotificationDispatcher(notifications []Notification) *NotificationDispatcher {
	return &NotificationDispatcher{notifications: notifications}
}

func (nd *NotificationDispatcher) SendAll() []SendResult {
	var wg sync.WaitGroup

	for _, notif := range nd.notifications {
		wg.Add(1)
		go func() {
			defer wg.Done()
			nd.sendAndCollect(notif)
		}()
	}

	wg.Wait()
	return nd.results
}

func (nd *NotificationDispatcher) sendAndCollect(notif Notification) {
	err := nd.sendPushNotification(notif)
	result := SendResult{UserID: notif.UserID, OK: err == nil}
	if err != nil {
		result.Error = err.Error()
		fmt.Printf("  WARN: %s: %v\n", notif.UserID, err)
	}

	nd.mu.Lock()
	nd.results = append(nd.results, result)
	nd.mu.Unlock()
}

func (nd *NotificationDispatcher) sendPushNotification(n Notification) error {
	time.Sleep(pushNotificationLatency)

	switch n.UserID {
	case "user-003":
		return fmt.Errorf("expired device token")
	case "user-007":
		return fmt.Errorf("no device registered")
	default:
		fmt.Printf("  Sent to %s: %s\n", n.UserID, n.Message)
		return nil
	}
}

func printNotificationSummary(results []SendResult, total int, elapsed time.Duration) {
	succeeded := 0
	failed := 0
	for _, r := range results {
		if r.OK {
			succeeded++
		} else {
			failed++
		}
	}

	fmt.Printf("\nNotification results (in %v):\n", elapsed)
	fmt.Printf("  Sent: %d/%d, Failed: %d/%d\n", succeeded, total, failed, total)
	if failed > 0 {
		fmt.Println("  Action: Queue failed notifications for retry via different channel (email, SMS)")
	}
}

func main() {
	notifications := []Notification{
		{"user-001", "Your order shipped!"},
		{"user-002", "Your order shipped!"},
		{"user-003", "Your order shipped!"}, // this user has an expired token
		{"user-004", "Your order shipped!"},
		{"user-005", "Your order shipped!"},
		{"user-006", "Your order shipped!"},
		{"user-007", "Your order shipped!"}, // this user has no device registered
		{"user-008", "Your order shipped!"},
	}

	dispatcher := NewNotificationDispatcher(notifications)

	fmt.Println("=== Notification Dispatch (best-effort) ===")
	fmt.Printf("\n--- Sending %d notifications (WaitGroup, best-effort) ---\n", len(notifications))
	start := time.Now()

	results := dispatcher.SendAll()

	printNotificationSummary(results, len(notifications), time.Since(start).Round(time.Millisecond))
}
```

**Expected output:**
```
=== Notification Dispatch (best-effort) ===

--- Sending 8 notifications (WaitGroup, best-effort) ---
  Sent to user-001: Your order shipped!
  Sent to user-002: Your order shipped!
  WARN: user-003: expired device token
  Sent to user-004: Your order shipped!
  Sent to user-005: Your order shipped!
  Sent to user-006: Your order shipped!
  WARN: user-007: no device registered
  Sent to user-008: Your order shipped!

Notification results (in 50ms):
  Sent: 6/8, Failed: 2/8
  Action: Queue failed notifications for retry via different channel (email, SMS)
```

Two notifications failed, but the other 6 were delivered successfully. Using the errgroup pattern here would have been wrong: user-003's expired token would have cancelled all remaining notifications, leaving users 4-8 without their shipping update.

## Step 3 -- Side-by-Side Comparison

Both patterns in one program to see the difference clearly:

```go
package main

import (
	"context"
	"fmt"
	"sync"
	"time"
)

const itemProcessingLatency = 50 * time.Millisecond

type AllOrNothingProcessor struct {
	items []string
}

func NewAllOrNothingProcessor(items []string) *AllOrNothingProcessor {
	return &AllOrNothingProcessor{items: items}
}

func (p *AllOrNothingProcessor) Process() (int, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	var once sync.Once
	var firstErr error

	processed := 0
	var mu sync.Mutex

	for _, item := range p.items {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case <-ctx.Done():
				fmt.Printf("  %s: SKIPPED (cancelled)\n", item)
				return
			default:
			}
			if err := processItem(item); err != nil {
				once.Do(func() {
					firstErr = err
					cancel()
				})
				fmt.Printf("  %s: FAILED\n", item)
				return
			}
			mu.Lock()
			processed++
			mu.Unlock()
			fmt.Printf("  %s: OK\n", item)
		}()
	}

	wg.Wait()
	return processed, firstErr
}

type BestEffortProcessor struct {
	items []string
}

func NewBestEffortProcessor(items []string) *BestEffortProcessor {
	return &BestEffortProcessor{items: items}
}

func (p *BestEffortProcessor) Process() (int, []string) {
	var wg sync.WaitGroup
	var mu sync.Mutex
	processed := 0
	var errors []string

	for _, item := range p.items {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := processItem(item); err != nil {
				mu.Lock()
				errors = append(errors, err.Error())
				mu.Unlock()
				fmt.Printf("  %s: FAILED (logged, continuing)\n", item)
				return
			}
			mu.Lock()
			processed++
			mu.Unlock()
			fmt.Printf("  %s: OK\n", item)
		}()
	}

	wg.Wait()
	return processed, errors
}

func processItem(item string) error {
	time.Sleep(itemProcessingLatency)
	if item == "INVALID" {
		return fmt.Errorf("%s: validation failed", item)
	}
	return nil
}

func main() {
	items := []string{"item-1", "item-2", "INVALID", "item-4", "item-5"}

	fmt.Println("=== Errgroup Pattern: stops on first error ===")
	allOrNothing := NewAllOrNothingProcessor(items)
	processed, err := allOrNothing.Process()
	fmt.Printf("  Result: processed=%d, error=%v\n", processed, err)

	fmt.Println("\n=== WaitGroup Pattern: continues despite errors ===")
	bestEffort := NewBestEffortProcessor(items)
	processed, errors := bestEffort.Process()
	fmt.Printf("  Result: processed=%d, errors=%d (%v)\n", processed, len(errors), errors)
}
```

**Expected output:**
```
=== Errgroup Pattern: stops on first error ===
  item-1: OK
  item-2: OK
  INVALID: FAILED
  item-4: SKIPPED (cancelled)
  item-5: SKIPPED (cancelled)
  Result: processed=2, error=INVALID: validation failed

=== WaitGroup Pattern: continues despite errors ===
  item-1: OK
  item-2: OK
  INVALID: FAILED (logged, continuing)
  item-4: OK
  item-5: OK
  Result: processed=4, errors=1 ([INVALID: validation failed])
```

Same input, different behavior. The errgroup pattern processed 2 items and stopped. The WaitGroup pattern processed 4 items and logged the error.

## Step 4 -- Both Patterns in One System

Real systems use both patterns together. A deployment pipeline validates configs (all-or-nothing) and then sends deploy notifications (best-effort):

```go
package main

import (
	"context"
	"fmt"
	"sync"
	"time"
)

const (
	configValidationLatency = 60 * time.Millisecond
	notificationLatency     = 40 * time.Millisecond
)

type ConfigValidator struct {
	configs []string
}

func NewConfigValidator(configs []string) *ConfigValidator {
	return &ConfigValidator{configs: configs}
}

func (cv *ConfigValidator) ValidateAll() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	var once sync.Once
	var firstErr error

	for _, cfg := range cv.configs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case <-ctx.Done():
				return
			default:
			}
			if err := cv.validateSingle(cfg); err != nil {
				once.Do(func() {
					firstErr = err
					cancel()
				})
				return
			}
			fmt.Printf("  Validated: %s\n", cfg)
		}()
	}

	wg.Wait()
	return firstErr
}

func (cv *ConfigValidator) validateSingle(name string) error {
	time.Sleep(configValidationLatency)
	return nil // all configs valid in this scenario
}

type DeployNotifier struct {
	recipients []string
}

func NewDeployNotifier(recipients []string) *DeployNotifier {
	return &DeployNotifier{recipients: recipients}
}

func (dn *DeployNotifier) NotifyAll() {
	var wg sync.WaitGroup

	for _, r := range dn.recipients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(notificationLatency)
			if r == "pagerduty-webhook" {
				fmt.Printf("  WARN: failed to notify %s (timeout)\n", r)
				return
			}
			fmt.Printf("  Notified: %s\n", r)
		}()
	}

	wg.Wait()
}

func main() {
	fmt.Println("=== Deployment Pipeline ===")

	// Phase 1: Validate configs (all-or-nothing, errgroup pattern)
	fmt.Println("\n--- Phase 1: Config Validation (all-or-nothing) ---")
	validator := NewConfigValidator([]string{"api-gateway.yaml", "auth-service.yaml", "payment-service.yaml"})
	if err := validator.ValidateAll(); err != nil {
		fmt.Printf("DEPLOY ABORTED: %v\n", err)
		return
	}
	fmt.Println("All configs valid -- proceeding to deploy")

	// Phase 2: Notify stakeholders (best-effort, WaitGroup)
	fmt.Println("\n--- Phase 2: Deploy Notifications (best-effort) ---")
	notifier := NewDeployNotifier([]string{"#deploys", "ops-team@company.com", "pagerduty-webhook"})
	notifier.NotifyAll()
	fmt.Println("\nDeployment complete")
}
```

**Expected output:**
```
=== Deployment Pipeline ===

--- Phase 1: Config Validation (all-or-nothing) ---
  Validated: api-gateway.yaml
  Validated: auth-service.yaml
  Validated: payment-service.yaml
All configs valid -- proceeding to deploy

--- Phase 2: Deploy Notifications (best-effort) ---
  Notified: #deploys
  Notified: ops-team@company.com
  WARN: failed to notify pagerduty-webhook (timeout)

Deployment complete
```

Phase 1 uses the errgroup pattern because a broken config should block deployment. Phase 2 uses WaitGroup because a failed Slack notification should not block the deploy.

## Decision Table

| Criterion | sync.WaitGroup | Errgroup Pattern |
|-----------|---------------|------------------|
| Error propagation | Manual (you log/collect errors) | Built-in (first error returned) |
| Sibling cancellation | Not built-in | Via context.WithCancel |
| Concurrency limit | Manual (semaphore channel) | Manual (semaphore) or errgroup.SetLimit |
| Add/Done tracking | Manual (easy to misuse) | Manual, or automatic with errgroup.Go |
| Fire-and-forget | Natural fit | Unnecessary overhead |
| **Best for** | **Independent tasks, best-effort** | **Dependent tasks, all-or-nothing** |

**Rule of thumb:**
- If one goroutine's failure means the whole operation is invalid: **errgroup pattern**
- If each goroutine's success/failure is independent: **WaitGroup with error logging**

## Verification

At this point, verify:
1. The errgroup pattern stops processing when INVALID is encountered
2. The WaitGroup pattern processes all items despite the INVALID one
3. Both patterns coexist naturally in the deployment pipeline
4. The decision table matches the behavior you observed

## Common Mistakes

### Using errgroup pattern when tasks are independent

**Wrong:**
```go
// Sending notifications with errgroup pattern -- one failure stops all
ctx, cancel := context.WithCancel(context.Background())
defer cancel()
// ... errgroup pattern for notifications
// Result: user-003's expired token blocks notifications to users 4-8
```

**Business impact:** Thousands of users miss their notifications because one device token expired.

**Fix:** Use WaitGroup for independent, best-effort work.

### Using WaitGroup when task failure should stop the batch

**Wrong:**
```go
// Processing orders with WaitGroup -- failures are logged and ignored
var wg sync.WaitGroup
for _, order := range orders {
    wg.Add(1)
    go func() {
        defer wg.Done()
        if err := processOrder(order); err != nil {
            log.Println(err) // logged but batch continues
        }
    }()
}
wg.Wait()
// Result: 7 of 10 orders processed, 3 failed silently, accounting is now inconsistent
```

**Business impact:** Partial batch processing leads to inconsistent state in the accounting system.

**Fix:** Use the errgroup pattern with context cancellation for atomic batch operations.

### Forgetting wg.Add before launching the goroutine

**Wrong:**
```go
go func() {
    wg.Add(1) // INSIDE the goroutine -- race with wg.Wait()
    defer wg.Done()
}()
wg.Wait() // might return before Add is called
```

**What happens:** `Wait()` can return before the goroutine calls `Add(1)`. The goroutine runs without being tracked. The errgroup pattern avoids this because `g.Go()` handles both launching and tracking atomically.

## Review

The two patterns are the same machinery pointed in opposite directions. The
errgroup shape -- a `WaitGroup` plus a `sync.Once` to capture the first error
plus a `context.CancelFunc` the siblings watch -- exists so that one failure
cancels the rest and the whole operation reports as failed; that is what you
want when a bad order or a broken config invalidates the batch. A plain
`WaitGroup`, where each goroutine logs its own error and returns, is the
best-effort model: failures are collected, not propagated, and the other
goroutines run to completion; that is what you want for independent work like
notifications. The decision table makes the axes explicit -- error propagation,
sibling cancellation, concurrency limits -- but the rule of thumb collapses it:
if one goroutine's failure means the whole operation is invalid, reach for
errgroup; if each success or failure stands alone, reach for WaitGroup. The
`golang.org/x/sync/errgroup` package packages the first shape so you never
reassemble the Once-and-cancel boilerplate by hand.

Run the full program and confirm you can predict each outcome: the errgroup
batch stops at the INVALID order and reports two processed, its siblings
cancelled; the WaitGroup dispatch sends every notification despite two failures;
the side-by-side run shows different processed counts from identical input; and
the deployment pipeline validates configs all-or-nothing before it notifies
stakeholders best-effort. If those results match your expectations before you
run it, you have internalized the choice rather than memorized the code.

## Resources
- [sync.WaitGroup documentation](https://pkg.go.dev/sync#WaitGroup) -- the counter that both patterns are built on.
- [sync.Once documentation](https://pkg.go.dev/sync#Once) -- the once-only guard the errgroup shape uses to capture just the first error.
- [errgroup package documentation](https://pkg.go.dev/golang.org/x/sync/errgroup) -- the ready-made all-or-nothing group with built-in context cancellation and `SetLimit`.
- [Go Proverbs: Don't just check errors, handle them gracefully](https://go-proverbs.github.io/) -- the principle behind deciding whether a failure should propagate or be tolerated.

---

Back to [Concurrency](../../concurrency.md) | Next: [06-errgroup-parallel-pipeline](../06-errgroup-parallel-pipeline/06-errgroup-parallel-pipeline.md)
