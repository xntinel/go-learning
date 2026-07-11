# Exercise 7: Race in Closure Loops

One of the most common concurrency bugs in Go is launching goroutines inside a
loop where each closure captures a variable the loop reassigns every iteration.
Because the closures share that one variable by reference, and the loop usually
finishes before the goroutines run, they all read the final value -- your batch
notifier emails every notification to the last user, your fan-out caller sends
every request with the wrong ID. This exercise reproduces the bug in two
settings, fixes it by passing the value as a parameter, and shows precisely what
Go 1.22 did and did not change.

## What you'll build

```text
07-race-in-closure-loops/
  main.go        the batch-notification bug and its fix, the parallel-API-caller
                 bug and its fix, and a Go 1.22 loop-semantics demo
```

- Build: a notification sender and an API caller that each demonstrate the closure-capture race and its parameter-passing fix.
- Implement: `SendAllBuggy`/`SendAllFixed`, `FanOutBuggy`/`FanOutFixed`, and a `LoopSemanticDemo` contrasting per-iteration and outer-variable capture.
- Verify: `go run -race main.go` on each step.

### Why Go 1.22 does not make this go away

In a real system, this bug means your batch notification sender emails ALL
notifications to the LAST user in the list, your parallel API caller sends every
request with the wrong user ID, and your batch processor processes the same item
repeatedly while skipping the rest.

Starting with Go 1.22, the `for` loop creates a new variable for each iteration,
which fixes the most common manifestation. But understanding the underlying
mechanism still matters: the same pattern appears with variables declared outside
the loop (which Go 1.22 does not touch), a great deal of existing code predates
1.22, and "capture by reference" applies everywhere closures are used, not just
in loops.

## Step 1 -- The Batch Notification Bug

Build a batch notification sender that processes user IDs in a loop, launching a goroutine per user. This is the kind of code you write when sending emails, push notifications, or webhook callbacks in parallel:

```go
package main

import (
	"fmt"
	"sync"
)

// Notification represents a message to be delivered to a user.
type Notification struct {
	UserID  string
	Message string
}

// NotificationSender dispatches notifications to a list of users concurrently.
// BUG: the shared notification variable causes all goroutines to read the last value.
type NotificationSender struct {
	users []string
}

func NewNotificationSender(users []string) *NotificationSender {
	return &NotificationSender{users: users}
}

func (ns *NotificationSender) buildNotification(userID string) Notification {
	return Notification{
		UserID:  userID,
		Message: fmt.Sprintf("Hello %s, your order has shipped!", userID),
	}
}

// SendAllBuggy demonstrates the closure capture bug.
// DATA RACE: notification is written by the loop and read by goroutines concurrently.
func (ns *NotificationSender) SendAllBuggy() {
	var wg sync.WaitGroup

	fmt.Println("--- Buggy Sender: all notifications go to the WRONG user ---")

	var notification Notification
	for _, userID := range ns.users {
		notification = ns.buildNotification(userID)
		wg.Add(1)
		go func() {
			defer wg.Done()
			// DATA RACE: notification is written by the loop and read by
			// this goroutine concurrently.
			fmt.Printf("  Sending to %-10s: %s\n", notification.UserID, notification.Message)
		}()
	}

	wg.Wait()
}

func main() {
	sender := NewNotificationSender(
		[]string{"alice", "bob", "charlie", "diana", "evan"},
	)
	sender.SendAllBuggy()
}
```

### Verification
```bash
go run main.go
```
Expected: most or all goroutines send to "evan" (the last user):
```
--- Buggy Sender: all notifications go to the WRONG user ---
  Sending to evan      : Hello evan, your order has shipped!
  Sending to evan      : Hello evan, your order has shipped!
  Sending to evan      : Hello evan, your order has shipped!
  Sending to evan      : Hello evan, your order has shipped!
  Sending to evan      : Hello evan, your order has shipped!
```

Alice, Bob, Charlie, and Diana never receive their notifications. Evan gets five. In a real system, this means four customers never learn their order shipped, and one customer gets five duplicate emails.

```bash
go run -race main.go
```
Expected: `WARNING: DATA RACE` because `notification` is written by the loop and read by goroutines concurrently.

## Step 2 -- Fix by Passing as Parameter

Pass the notification as a function parameter. Go copies the argument at the call site, giving each goroutine its own independent copy:

```go
package main

import (
	"fmt"
	"sync"
)

// Notification represents a message to be delivered to a user.
type Notification struct {
	UserID  string
	Message string
}

// NotificationSender dispatches notifications to a list of users concurrently.
type NotificationSender struct {
	users []string
}

func NewNotificationSender(users []string) *NotificationSender {
	return &NotificationSender{users: users}
}

func (ns *NotificationSender) buildNotification(userID string) Notification {
	return Notification{
		UserID:  userID,
		Message: fmt.Sprintf("Hello %s, your order has shipped!", userID),
	}
}

// SendAllFixed passes the notification as a parameter so each goroutine
// gets its own independent copy at the call site.
func (ns *NotificationSender) SendAllFixed() {
	var wg sync.WaitGroup

	fmt.Println("--- Fixed Sender: each notification goes to the correct user ---")

	var notification Notification
	for _, userID := range ns.users {
		notification = ns.buildNotification(userID)
		wg.Add(1)
		// notif is a PARAMETER: Go copies notification's value at the call site.
		go func(notif Notification) {
			defer wg.Done()
			fmt.Printf("  Sending to %-10s: %s\n", notif.UserID, notif.Message)
		}(notification)
	}

	wg.Wait()
}

func main() {
	sender := NewNotificationSender(
		[]string{"alice", "bob", "charlie", "diana", "evan"},
	)
	sender.SendAllFixed()
}
```

### Verification
```bash
go run -race main.go
```
Expected: all five users receive their correct notification (in any order), zero race warnings:
```
--- Fixed Sender: each notification goes to the correct user ---
  Sending to charlie   : Hello charlie, your order has shipped!
  Sending to alice     : Hello alice, your order has shipped!
  Sending to bob       : Hello bob, your order has shipped!
  Sending to evan      : Hello evan, your order has shipped!
  Sending to diana     : Hello diana, your order has shipped!
```

## Step 3 -- The Parallel API Caller Bug

The same bug appears when making parallel API calls. This is extremely common in microservice architectures where you fan out requests to multiple services:

```go
package main

import (
	"fmt"
	"sync"
)

// APIRequest represents a fan-out call to a downstream service.
type APIRequest struct {
	UserID   string
	Endpoint string
}

// RequestProcessor dispatches API requests in parallel.
type RequestProcessor struct {
	requests []APIRequest
}

func NewRequestProcessor(requests []APIRequest) *RequestProcessor {
	return &RequestProcessor{requests: requests}
}

// FanOutBuggy demonstrates the closure capture bug: all goroutines see the last request.
// BUG: req is declared outside the loop and shared by all goroutines.
func (rp *RequestProcessor) FanOutBuggy() {
	var wg sync.WaitGroup

	fmt.Println("--- Buggy API Caller ---")

	var req APIRequest
	for _, r := range rp.requests {
		req = r
		wg.Add(1)
		go func() {
			defer wg.Done()
			// BUG: all goroutines see the last request.
			fmt.Printf("  Calling %s for user %s\n", req.Endpoint, req.UserID)
		}()
	}

	wg.Wait()
}

// FanOutFixed passes each request as a parameter so each goroutine gets its own copy.
func (rp *RequestProcessor) FanOutFixed() {
	var wg sync.WaitGroup

	fmt.Println("--- Fixed API Caller ---")

	var req APIRequest
	for _, r := range rp.requests {
		req = r
		wg.Add(1)
		go func(request APIRequest) {
			defer wg.Done()
			fmt.Printf("  Calling %s for user %s\n", request.Endpoint, request.UserID)
		}(req)
	}

	wg.Wait()
}

func main() {
	processor := NewRequestProcessor([]APIRequest{
		{UserID: "u-101", Endpoint: "/api/billing"},
		{UserID: "u-202", Endpoint: "/api/shipping"},
		{UserID: "u-303", Endpoint: "/api/notifications"},
		{UserID: "u-404", Endpoint: "/api/analytics"},
	})

	processor.FanOutBuggy()
	fmt.Println()
	processor.FanOutFixed()
}
```

### Verification
```bash
go run -race main.go
```

The buggy version calls `/api/analytics` for `u-404` four times. The fixed version calls each endpoint for the correct user.

In production, this means:
- Billing charges the wrong customer
- Shipping sends to the wrong address
- Notifications go to the wrong person
- Analytics records wrong user activity

## Step 4 -- Go 1.22 and Why Explicit Passing Is Still Best

Go 1.22 changed loop variable semantics: each iteration creates a new variable. Using the loop variable directly in a closure is now safe:

```go
package main

import (
	"fmt"
	"sync"
)

// LoopSemanticDemo illustrates the Go 1.22 per-iteration variable behavior
// and its limitations with variables declared outside the loop.
type LoopSemanticDemo struct {
	users []string
}

func NewLoopSemanticDemo(users []string) *LoopSemanticDemo {
	return &LoopSemanticDemo{users: users}
}

// RunPerIterationSafe shows that Go 1.22+ creates a new loop variable per iteration.
func (d *LoopSemanticDemo) RunPerIterationSafe() {
	var wg sync.WaitGroup

	fmt.Println("--- Go 1.22+: loop variable is per-iteration ---")

	for _, userID := range d.users {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fmt.Printf("  Notifying: %s\n", userID)
		}()
	}

	wg.Wait()
}

// RunOuterVariableBuggy shows that variables declared outside the loop
// are still shared, even in Go 1.22+.
// BUG: current is declared outside the loop and captured by reference.
func (d *LoopSemanticDemo) RunOuterVariableBuggy() {
	var wg sync.WaitGroup

	fmt.Println("--- BUT: variables declared outside the loop are still shared ---")

	var current string
	for _, userID := range d.users {
		current = userID // current is declared OUTSIDE the loop
		wg.Add(1)
		go func() {
			defer wg.Done()
			fmt.Printf("  Notifying: %s\n", current) // STILL A BUG even in Go 1.22+
		}()
	}

	wg.Wait()
}

func printRecommendation() {
	fmt.Println("Recommendation: always pass as parameter for clarity,")
	fmt.Println("regardless of Go version. It makes the intent explicit.")
}

func main() {
	demo := NewLoopSemanticDemo(
		[]string{"alice", "bob", "charlie", "diana", "evan"},
	)

	demo.RunPerIterationSafe()
	fmt.Println()
	demo.RunOuterVariableBuggy()
	fmt.Println()
	printRecommendation()
}
```

### Verification
```bash
go run -race main.go
```

The first block works correctly in Go 1.22+. The second block still has the bug because `current` is declared outside the loop.

**Why explicit parameter passing is still the best practice:**
1. It works in ALL Go versions
2. It makes the intent unmistakably clear: "this goroutine gets its own copy"
3. It catches bugs that Go 1.22 does NOT fix (variables declared outside the loop)
4. It is self-documenting: readers immediately see what data the goroutine receives

## Common Mistakes

### Assuming All Variables in a Loop Are Per-Iteration
Only the loop variables declared in the `for` statement itself get per-iteration semantics in Go 1.22. Variables declared before the loop and modified inside it are still shared.

### Race Detector Not Catching All Closure Bugs
If all goroutines happen to read the variable after the loop finishes (no concurrent write), the race detector may not report it. The bug (all goroutines seeing the same value) still exists: it is a **logic bug**, not just a data race.

### Thinking time.Sleep Fixes It
Adding sleep between goroutine launches does not fix the problem. The goroutine captures a **reference** to the variable, not a snapshot. Even if the goroutine starts immediately, the next loop iteration can change the variable before the goroutine reads it.

### Passing Pointers Instead of Values
```go
go func(notif *Notification) {
    // BUG: still shares the underlying data
}(&notification)
```
Passing a pointer copies the pointer, not the data. All goroutines still share the same `Notification`. Pass by value to get a true copy.

## Review

Closures capture variables by reference, not by value. In a loop, every
goroutine closure shares the same outer variable, so by the time they run --
typically after the loop has finished -- they all read the last value, and the
consequence is that every goroutine processes the same final item: the wrong
user, the wrong request, the wrong notification. The fix is to pass the variable
as a function argument, which copies its value at the call site so each goroutine
gets an independent snapshot. Go 1.22 makes the loop variable itself
per-iteration, so capturing it directly is now safe -- but only for variables
declared in the `for` statement. A variable declared outside the loop and
reassigned inside it is still shared even under 1.22, which is why explicit
parameter passing remains the clearest, version-independent habit, and it applies
to structs, strings, integers, and every other type.

You should be able to answer the four questions the exercise ends on. Run each
version with `-race` and identify which functions warn -- the buggy senders and
the outer-variable demo, not the fixed ones. A closure captures a reference
because it closes over the variable itself, not a copy of its current value, so
it sees whatever that variable holds when the goroutine finally executes. Go 1.22
changed loop semantics so each iteration binds a fresh loop variable, closing the
classic loop-capture hole. And parameter passing stays the recommendation
regardless: it works on every Go version, it makes each goroutine's private copy
explicit at the call site, and it catches the one case 1.22 leaves open -- a
variable declared outside the loop.

## Resources
- [Go Wiki: Common Mistakes -- Using Goroutines on Loop Iterator Variables](https://go.dev/wiki/CommonMistakes) -- the canonical description of this bug and its fix.
- [Go 1.22 Release Notes: Loopvar](https://go.dev/doc/go1.22#language) -- the language change that makes loop variables per-iteration.
- [Go Blog: Fixing For Loops in Go 1.22](https://go.dev/blog/loopvar-preview) -- the rationale and compatibility story behind the loopvar change.

---

Back to [Concurrency](../../concurrency.md) | Next: [08-race-free-design-patterns](../08-race-free-design-patterns/08-race-free-design-patterns.md)
