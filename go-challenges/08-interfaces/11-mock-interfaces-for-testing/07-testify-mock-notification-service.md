# Exercise 7: testify/mock — On/Return, Argument Matchers, and AssertExpectations

`stretchr/testify/mock` is the other widely-used mocking library, and it takes a
reflection-driven, no-codegen approach: embed `mock.Mock`, implement each method
via `m.Called(...)`, and program expectations with `m.On("Send", ...).Return(...)`.
This module tests a `NotificationService` over a `Sender` port that way, and
weighs the testify style against gomock and the hand-rolled spy.

Fully self-contained. It fetches `github.com/stretchr/testify`, so gate it with
`GOFLAGS=-mod=mod`.

## What you'll build

```text
testifynotify/               independent module: example.com/notify
  go.mod                     go 1.26; requires github.com/stretchr/testify
  notify.go                  Message; Sender; NotificationService
  cmd/
    demo/
      main.go                runnable demo with a stdout Sender
  notify_test.go            testify mock: On/Return/Once, MatchedBy, AssertExpectations
```

- Files: `notify.go`, `cmd/demo/main.go`, `notify_test.go`.
- Implement: `NotificationService.NotifyOrderPlaced(ctx, email, orderID)` building a `Message` and sending it through `Sender`.
- Test: a `mockSender` embedding `mock.Mock`, `Send` implemented via `m.Called`; expectations set with `On("Send", mock.Anything, mock.MatchedBy(...)).Return(nil).Once()`; verified with `AssertExpectations` and `AssertNumberOfCalls`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go get github.com/stretchr/testify/mock
```

### The testify mock model

There is no code generation. You embed `mock.Mock` in your double and implement
each interface method by delegating to `m.Called(args...)`, which records the call
and returns the programmed results:

```go
func (m *mockSender) Send(ctx context.Context, msg Message) error {
	args := m.Called(ctx, msg)
	return args.Error(0)
}
```

`m.Called` looks up the matching `On(...)` expectation, records the invocation, and
returns an `Arguments` value; `args.Error(0)` type-asserts the first configured
return as an `error`. Expectations are declared with
`m.On("Send", mock.Anything, mock.MatchedBy(pred)).Return(nil).Once()`:
`"Send"` is the method name (a string — the reflection-driven cost is that a typo
is a runtime failure, not a compile error), `mock.Anything` matches any argument,
`mock.MatchedBy(pred)` matches when the predicate returns true, `Return` supplies
the results, and `Once()` (or `Times(n)`) bounds the count. `AssertExpectations(t)`
then fails the test if any `On` expectation went unmet, and `AssertNumberOfCalls`
checks an exact count.

### Where testify sits versus gomock and hand-rolled

The three approaches trade off along one axis: magic versus boilerplate.
Hand-rolled (Exercise 1) is the most code and zero magic — a struct and a slice you
assert on directly, fully type-checked. gomock (Exercise 6) generates the double
and verifies expectations strictly through the controller, with compile-time method
names but a codegen step. testify needs no codegen but identifies methods by
string and matches arguments through reflection, so mistakes surface at runtime.
testify's ergonomics are pleasant for small doubles and it composes with the rest
of `testify`; its weakness is exactly that stringly-typed, reflective core. Pick
per interface size, churn, and how much you value compile-time safety over
convenience — a mature codebase mixes all three.

`MatchedBy` is the tool that keeps the assertion focused: rather than pinning the
entire `Message`, match only the fields the contract requires — the recipient and
that the body mentions the order id — so a later change to the subject line does
not break the test.

Create `notify.go`:

```go
package notify

import (
	"context"
	"fmt"
)

// Message is an outbound notification.
type Message struct {
	To      string
	Subject string
	Body    string
}

// Sender is the outbound port to a mail/notification provider.
type Sender interface {
	Send(ctx context.Context, msg Message) error
}

// NotificationService composes messages and sends them.
type NotificationService struct {
	sender Sender
}

func NewNotificationService(s Sender) *NotificationService {
	return &NotificationService{sender: s}
}

// NotifyOrderPlaced sends the order-placed notification to email.
func (n *NotificationService) NotifyOrderPlaced(ctx context.Context, email, orderID string) error {
	msg := Message{
		To:      email,
		Subject: "Order placed",
		Body:    fmt.Sprintf("Your order %s has been placed.", orderID),
	}
	if err := n.sender.Send(ctx, msg); err != nil {
		return fmt.Errorf("notify order-placed to %s: %w", email, err)
	}
	return nil
}
```

### The runnable demo

The demo wires a real `Sender` that writes to stdout, so `go run ./cmd/demo`
prints the composed notification.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/notify"
)

// stdoutSender is a trivial real Sender for the demo.
type stdoutSender struct{}

func (stdoutSender) Send(_ context.Context, msg notify.Message) error {
	fmt.Printf("to=%s subject=%q body=%q\n", msg.To, msg.Subject, msg.Body)
	return nil
}

func main() {
	svc := notify.NewNotificationService(stdoutSender{})
	if err := svc.NotifyOrderPlaced(context.Background(), "alice@example.com", "order-42"); err != nil {
		fmt.Println("error:", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
to=alice@example.com subject="Order placed" body="Your order order-42 has been placed."
```

### Tests

`TestNotifyOrderPlacedSendsToRecipient` sets an `On` expectation with a `MatchedBy`
predicate on the recipient and body, drives the service, then verifies with
`AssertExpectations` and `AssertNumberOfCalls`. `TestNotifyPropagatesSenderError`
programs the mock to return an error and asserts it propagates via `errors.Is`.

Create `notify_test.go`:

```go
package notify

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/mock"
)

// mockSender is a testify mock of Sender.
type mockSender struct {
	mock.Mock
}

func (m *mockSender) Send(ctx context.Context, msg Message) error {
	args := m.Called(ctx, msg)
	return args.Error(0)
}

func TestNotifyOrderPlacedSendsToRecipient(t *testing.T) {
	t.Parallel()

	m := new(mockSender)
	m.On("Send", mock.Anything, mock.MatchedBy(func(msg Message) bool {
		return msg.To == "alice@example.com" && strings.Contains(msg.Body, "order-42")
	})).Return(nil).Once()

	svc := NewNotificationService(m)
	if err := svc.NotifyOrderPlaced(context.Background(), "alice@example.com", "order-42"); err != nil {
		t.Fatalf("NotifyOrderPlaced: %v", err)
	}

	m.AssertExpectations(t)
	m.AssertNumberOfCalls(t, "Send", 1)
}

func TestNotifyPropagatesSenderError(t *testing.T) {
	t.Parallel()

	smtpDown := errors.New("smtp unavailable")
	m := new(mockSender)
	m.On("Send", mock.Anything, mock.Anything).Return(smtpDown).Once()

	svc := NewNotificationService(m)
	err := svc.NotifyOrderPlaced(context.Background(), "bob@example.com", "order-7")
	if !errors.Is(err, smtpDown) {
		t.Fatalf("err = %v, want smtp unavailable", err)
	}

	m.AssertExpectations(t)
}
```

## Review

testify's model is declarative like gomock but reflection-driven and codegen-free:
`On("Send", ...).Return(...).Once()` programs the double, `m.Called` inside the
method records and dispatches the call, and `AssertExpectations` fails the test if
any programmed expectation went unmet. `MatchedBy` keeps the interaction assertion
minimal — matching the recipient and the order id in the body, not the whole
message — so the test does not break when an unrelated field changes.

The trade-off to internalize: the ergonomic convenience comes from stringly-typed
method names and reflective argument matching, so a mistyped `"Send"` or a
predicate over the wrong type fails at runtime rather than compile time. That is
the price against gomock's generated, compile-checked methods and against the
hand-rolled spy's plain type safety. And as always, an all-green testify suite
only proves the service honors *your* encoding of the `Sender` contract; pair it
with one integration test against a real provider sandbox.

## Resources

- [`github.com/stretchr/testify/mock`](https://pkg.go.dev/github.com/stretchr/testify/mock) — `Mock.On`, `Call.Return/Once/Times`, `MatchedBy`, `AssertExpectations`.
- [testify repository](https://github.com/stretchr/testify) — the mock, assert, and require packages and their idioms.
- [Martin Fowler: Mocks Aren't Stubs](https://martinfowler.com/articles/mocksArentStubs.html) — interaction-based verification, which testify's expectations implement.

---

Back to [06-gomock-generated-payment-gateway.md](06-gomock-generated-payment-gateway.md) | Next: [08-consumer-defined-interface-spy.md](08-consumer-defined-interface-spy.md)
