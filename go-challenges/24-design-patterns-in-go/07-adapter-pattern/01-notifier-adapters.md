# Exercise 1: Multi-Channel Notifier Adapters

Your application needs to notify users over email, SMS, and Slack, and each of those is a different third-party SDK with a different method name, argument order, and failure model. This exercise builds the classic adapter setup: one domain `Notifier` port, three single-channel adapters that each wrap one vendor client, and a `MultiNotifier` that is itself an adapter and fans a single `Send` out to all of them.

This module is fully self-contained. It begins with its own `go mod init`, simulates the three vendor SDKs in their own sub-packages, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
notifier.go                       Notifier port; EmailAdapter, SMSAdapter, SlackAdapter; MultiNotifier
thirdparty/
  sendgrid/sendgrid.go            simulated email SDK (returns a wrapped sentinel)
  twilio/twilio.go                simulated SMS SDK (returns a custom *Error with a Code)
  slack/slack.go                  simulated webhook SDK (returns an HTTP status + sentinel)
cmd/
  demo/main.go                    wires all three into a MultiNotifier and exercises failures
example_test.go                   a runnable go-doc Example
notifier_test.go                  contract tests: fan-out, error wrapping, sentinel mapping
```

- Files: `thirdparty/sendgrid/sendgrid.go`, `thirdparty/twilio/twilio.go`, `thirdparty/slack/slack.go`, `notifier.go`, `cmd/demo/main.go`, `example_test.go`, `notifier_test.go`.
- Implement: the `Notifier` interface; `EmailAdapter`, `SMSAdapter`, `SlackAdapter` with validating constructors; `MultiNotifier` composing them with `errors.Join`.
- Test: round-trip fan-out, an empty-recipient table across all three channels, `errors.As` reaching `*twilio.Error`, `errors.Is` reaching the Slack and SendGrid sentinels, and the joined partial-failure path.
- Verify: `go test -race ./...` and `go run ./cmd/demo`.

Set up the module:

```bash
mkdir -p go-solutions/24-design-patterns-in-go/07-adapter-pattern/01-notifier-adapters/thirdparty/sendgrid go-solutions/24-design-patterns-in-go/07-adapter-pattern/01-notifier-adapters/thirdparty/twilio go-solutions/24-design-patterns-in-go/07-adapter-pattern/01-notifier-adapters/thirdparty/slack go-solutions/24-design-patterns-in-go/07-adapter-pattern/01-notifier-adapters/cmd/demo
cd go-solutions/24-design-patterns-in-go/07-adapter-pattern/01-notifier-adapters
```

### Three incompatible SDKs, on purpose

Real vendors do not agree on anything, and the simulated SDKs reproduce three genuinely different failure models so the adapters have something real to reconcile.

The SendGrid stand-in returns an idiomatic Go `error`, and that error wraps a *package sentinel* (`ErrInvalidFrom`) so a caller can match it with `errors.Is`. The Twilio stand-in returns a *custom error type*, `*twilio.Error`, that carries a numeric `Code` field — the kind of structured error a test wants to reach with `errors.As`. The Slack stand-in is the odd one: it models a webhook POST and returns an `(int, error)` pair where the `int` is an HTTP status code, so the adapter has to decide what to do with a status the domain never mentions. Build all three first; the adapters in the next file translate them into one uniform `Notifier`.

Create `thirdparty/sendgrid/sendgrid.go`:

```go
package sendgrid

import (
	"context"
	"errors"
	"fmt"
)

type SendGridResponse struct {
	MessageID string
}

type SendGridClient struct {
	APIKey string
}

var ErrInvalidFrom = errors.New("sendgrid: from address is required")

func (c *SendGridClient) SendEmail(_ context.Context, from, to, subject, htmlBody string) (SendGridResponse, error) {
	if c.APIKey == "" {
		return SendGridResponse{}, errors.New("sendgrid: APIKey is required")
	}
	if from == "" {
		return SendGridResponse{}, fmt.Errorf("sendgrid: %w", ErrInvalidFrom)
	}
	return SendGridResponse{MessageID: "sg-" + to}, nil
}
```

Create `thirdparty/twilio/twilio.go`:

```go
package twilio

import (
	"context"
	"fmt"
)

type Error struct {
	Code    int
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("twilio: code=%d message=%s", e.Code, e.Message)
}

type TwilioClient struct {
	AccountSID string
	AuthToken  string
}

func (c *TwilioClient) CreateMessage(_ context.Context, from, to, body string) (string, error) {
	if c.AccountSID == "" {
		return "", &Error{Code: 20003, Message: "authentication required"}
	}
	if from == "" || to == "" {
		return "", &Error{Code: 21211, Message: "from and to are required"}
	}
	return "tw-" + to, nil
}

var _ error = (*Error)(nil)
```

Create `thirdparty/slack/slack.go`:

```go
package slack

import (
	"errors"
	"fmt"
	"net/http"
)

type SlackWebhook struct {
	WebhookURL string
}

var ErrWebhookDown = errors.New("slack: webhook returned error status")

func (s *SlackWebhook) Post(_ map[string]string) (int, error) {
	if s.WebhookURL == "" {
		return 0, nil
	}
	if s.WebhookURL == "https://hooks.example/down" {
		return http.StatusInternalServerError, fmt.Errorf("slack: %w (status=%d)", ErrWebhookDown, http.StatusInternalServerError)
	}
	return http.StatusOK, nil
}
```

### The port and the three adapters

Now the domain side. `Notifier` is the port, phrased in domain words — `Send(ctx, recipient, message) error` — with no trace of any vendor. Each adapter holds one vendor client as an unexported field and a small piece of channel configuration (the from-address, the from-number, the channel name) that the domain `Send` signature does not carry. Each constructor validates its dependencies up front and returns a sentinel on misconfiguration, so a `nil` client or an empty from-address fails at construction time rather than surfacing as a confusing error on the first send.

Read the three `Send` bodies as variations on one theme. `EmailAdapter.Send` checks the recipient, calls `SendEmail` with the vendor's four-argument shape (supplying a constant subject the domain never models), and on failure wraps the vendor error with `%w` so `errors.Is(err, sendgrid.ErrInvalidFrom)` still works through the wrapper. `SMSAdapter.Send` does the same against `CreateMessage`, and because the vendor returns `*twilio.Error`, the wrapped error stays reachable by `errors.As`. `SlackAdapter.Send` is the interesting one: it builds the vendor's `map[string]string` payload, captures the returned HTTP status, and folds the status *into* the wrapped error so monitoring can later split a `500` from a `400`. The status is information the domain does not want in its interface but does want in its logs, and the adapter is exactly the place that keeps both true at once.

`MultiNotifier` closes the loop: it implements the same `Notifier` interface by holding a slice of `Notifier` and fanning out. `errors.Join` collapses the results — `nil` when every child succeeded, a single walkable joined error when one or more failed. The small `From`, `Channel`, and `Count` accessors exist so the demo can print configuration without the foreign clients ever leaving the package.

Create `notifier.go`:

```go
package notifier

import (
	"context"
	"errors"
	"fmt"

	"example.com/notifier/thirdparty/sendgrid"
	"example.com/notifier/thirdparty/slack"
	"example.com/notifier/thirdparty/twilio"
)

var (
	ErrEmptyRecipient = errors.New("notifier: recipient is required")
	ErrEmptyFromEmail = errors.New("notifier: fromEmail is required")
	ErrEmptyFromSMS   = errors.New("notifier: from number is required")
	ErrEmptyChannel   = errors.New("notifier: channel is required")
	ErrNoNotifiers    = errors.New("notifier: at least one notifier is required")
	ErrMissingClient  = errors.New("notifier: vendor client is required")
)

type Notifier interface {
	Send(ctx context.Context, recipient, message string) error
}

type EmailAdapter struct {
	client    *sendgrid.SendGridClient
	fromEmail string
}

func NewEmailAdapter(client *sendgrid.SendGridClient, fromEmail string) (*EmailAdapter, error) {
	if client == nil {
		return nil, fmt.Errorf("%w: sendgrid client", ErrMissingClient)
	}
	if fromEmail == "" {
		return nil, ErrEmptyFromEmail
	}
	return &EmailAdapter{client: client, fromEmail: fromEmail}, nil
}

func (a *EmailAdapter) Send(ctx context.Context, recipient, message string) error {
	if recipient == "" {
		return ErrEmptyRecipient
	}
	if _, err := a.client.SendEmail(ctx, a.fromEmail, recipient, "Notification", message); err != nil {
		return fmt.Errorf("send email to %s: %w", recipient, err)
	}
	return nil
}

func (a *EmailAdapter) From() string { return a.fromEmail }

type SMSAdapter struct {
	client *twilio.TwilioClient
	from   string
}

func NewSMSAdapter(client *twilio.TwilioClient, from string) (*SMSAdapter, error) {
	if client == nil {
		return nil, fmt.Errorf("%w: twilio client", ErrMissingClient)
	}
	if from == "" {
		return nil, ErrEmptyFromSMS
	}
	return &SMSAdapter{client: client, from: from}, nil
}

func (a *SMSAdapter) Send(ctx context.Context, recipient, message string) error {
	if recipient == "" {
		return ErrEmptyRecipient
	}
	if _, err := a.client.CreateMessage(ctx, a.from, recipient, message); err != nil {
		return fmt.Errorf("send sms to %s: %w", recipient, err)
	}
	return nil
}

func (a *SMSAdapter) From() string { return a.from }

type SlackAdapter struct {
	webhook *slack.SlackWebhook
	channel string
}

func NewSlackAdapter(webhook *slack.SlackWebhook, channel string) (*SlackAdapter, error) {
	if webhook == nil {
		return nil, fmt.Errorf("%w: slack webhook", ErrMissingClient)
	}
	if channel == "" {
		return nil, ErrEmptyChannel
	}
	return &SlackAdapter{webhook: webhook, channel: channel}, nil
}

func (a *SlackAdapter) Send(ctx context.Context, recipient, message string) error {
	if recipient == "" {
		return ErrEmptyRecipient
	}
	payload := map[string]string{
		"channel": a.channel,
		"to":      recipient,
		"text":    message,
	}
	status, err := a.webhook.Post(payload)
	if err != nil {
		return fmt.Errorf("post slack to %s (status=%d): %w", a.channel, status, err)
	}
	return nil
}

func (a *SlackAdapter) Channel() string { return a.channel }

type MultiNotifier struct {
	notifiers []Notifier
}

func NewMultiNotifier(notifiers ...Notifier) (*MultiNotifier, error) {
	if len(notifiers) == 0 {
		return nil, ErrNoNotifiers
	}
	return &MultiNotifier{notifiers: notifiers}, nil
}

func (m *MultiNotifier) Send(ctx context.Context, recipient, message string) error {
	var errs []error
	for _, n := range m.notifiers {
		if err := n.Send(ctx, recipient, message); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (m *MultiNotifier) Count() int { return len(m.notifiers) }
```

Note the `SlackAdapter.Send` context parameter is part of the port even though this particular vendor method (`Post`) does not take one; the adapter accepts the domain's signature and uses what the vendor offers. The `ctx` is unused by `Post` but kept in the interface because the other two channels need it and a uniform port is the whole point.

### A runnable demo

The demo wires all three adapters into one `MultiNotifier`, runs the happy path, then deliberately triggers three different failure modes: an empty from-address rejected at construction, missing credentials that only surface when the SMS is actually sent, and a webhook configured to return a 5xx. The last line proves the Slack sentinel survives the adapter's wrapping by way of `errors.Is`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"log"

	"example.com/notifier"
	"example.com/notifier/thirdparty/sendgrid"
	"example.com/notifier/thirdparty/slack"
	"example.com/notifier/thirdparty/twilio"
)

func main() {
	email, err := notifier.NewEmailAdapter(&sendgrid.SendGridClient{APIKey: "test"}, "noreply@example.com")
	if err != nil {
		log.Fatalf("email adapter: %v", err)
	}
	sms, err := notifier.NewSMSAdapter(&twilio.TwilioClient{AccountSID: "test"}, "+1-555-0000")
	if err != nil {
		log.Fatalf("sms adapter: %v", err)
	}
	slackAdapter, err := notifier.NewSlackAdapter(&slack.SlackWebhook{WebhookURL: "https://hooks.example/test"}, "#alerts")
	if err != nil {
		log.Fatalf("slack adapter: %v", err)
	}

	multi, err := notifier.NewMultiNotifier(email, sms, slackAdapter)
	if err != nil {
		log.Fatalf("multi: %v", err)
	}

	fmt.Printf("email=%s sms=%s slack=%s channels=%d\n",
		email.From(), sms.From(), slackAdapter.Channel(), multi.Count())

	if err := multi.Send(context.Background(), "alice@example.com", "hello"); err != nil {
		fmt.Printf("send error (joined): %v\n", err)
	} else {
		fmt.Println("fan-out succeeded on all channels")
	}

	fmt.Println("--- error cases ---")

	if _, err = notifier.NewEmailAdapter(&sendgrid.SendGridClient{}, ""); err == nil {
		log.Fatal("expected error for empty fromEmail")
	} else {
		fmt.Printf("empty fromEmail: %v\n", err)
	}

	badSMS, err := notifier.NewSMSAdapter(&twilio.TwilioClient{}, "+1")
	if err != nil {
		log.Fatalf("SMS adapter: %v", err)
	}
	if err = badSMS.Send(context.Background(), "+1-555-0101", "hi"); err == nil {
		log.Fatal("expected error for missing AccountSID at send")
	} else {
		fmt.Printf("missing AccountSID surfaces at send time: %v\n", err)
	}

	bad := &slack.SlackWebhook{WebhookURL: "https://hooks.example/down"}
	badAdapter, _ := notifier.NewSlackAdapter(bad, "#alerts")
	err = badAdapter.Send(context.Background(), "alice", "hi")
	fmt.Printf("slack down: %v\n", err)
	fmt.Printf("errors.Is(slack.ErrWebhookDown): %v\n", errors.Is(err, slack.ErrWebhookDown))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
email=noreply@example.com sms=+1-555-0000 slack=#alerts channels=3
fan-out succeeded on all channels
--- error cases ---
empty fromEmail: notifier: fromEmail is required
missing AccountSID surfaces at send time: send sms to +1-555-0101: twilio: code=20003 message=authentication required
slack down: post slack to #alerts (status=500): slack: slack: webhook returned error status (status=500)
errors.Is(slack.ErrWebhookDown): true
```

### A go-doc Example

An `Example` function doubles as documentation and as a test: `go test` runs it and compares stdout to the `// Output:` comment, so a regression in `Count` or `From` fails the build.

Create `example_test.go`:

```go
package notifier_test

import (
	"context"
	"fmt"

	"example.com/notifier"
	"example.com/notifier/thirdparty/sendgrid"
)

func ExampleMultiNotifier() {
	email, _ := notifier.NewEmailAdapter(&sendgrid.SendGridClient{APIKey: "test"}, "noreply@example.com")
	multi, _ := notifier.NewMultiNotifier(email)
	if err := multi.Send(context.Background(), "alice@example.com", "hello"); err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("channels=%d from=%s\n", multi.Count(), email.From())
	// Output: channels=1 from=noreply@example.com
}
```

### Tests

The tests pin the contract the adapters exist to provide. `TestMultiNotifier_FansOutToAllNotifiers` proves the happy path returns `nil`. `TestAdapters_RejectEmptyRecipient` is a table-driven subtest asserting the same empty-recipient invariant across all three channels through the `Notifier` interface. `TestSMSAdapter_WrapsVendorErrorWithIs` proves `errors.As` reaches the `*twilio.Error` and recovers its `Code` through the adapter's wrap. `TestSlackAdapter_PropagatesVendorSentinel` proves the 5xx vendor sentinel survives via `errors.Is`. `TestMultiNotifier_JoinsPartialFailures` proves a sentinel buried in one child of a `MultiNotifier` is still reachable in the joined error. The remaining tests pin the constructors' fail-fast behavior, including the `nil`-client rejection that the original "your turn" prompt asked for.

Create `notifier_test.go`:

```go
package notifier

import (
	"context"
	"errors"
	"testing"

	"example.com/notifier/thirdparty/sendgrid"
	"example.com/notifier/thirdparty/slack"
	"example.com/notifier/thirdparty/twilio"
)

func TestMultiNotifier_FansOutToAllNotifiers(t *testing.T) {
	t.Parallel()

	email, err := NewEmailAdapter(&sendgrid.SendGridClient{APIKey: "test"}, "noreply@example.com")
	if err != nil {
		t.Fatalf("NewEmailAdapter: %v", err)
	}
	sms, err := NewSMSAdapter(&twilio.TwilioClient{AccountSID: "test"}, "+1-555-0000")
	if err != nil {
		t.Fatalf("NewSMSAdapter: %v", err)
	}
	slackAdapter, err := NewSlackAdapter(&slack.SlackWebhook{WebhookURL: "https://hooks.example/test"}, "#alerts")
	if err != nil {
		t.Fatalf("NewSlackAdapter: %v", err)
	}

	multi, err := NewMultiNotifier(email, sms, slackAdapter)
	if err != nil {
		t.Fatalf("NewMultiNotifier: %v", err)
	}

	if err := multi.Send(context.Background(), "alice@example.com", "hello"); err != nil {
		t.Fatalf("Send returned error: %v", err)
	}
}

func TestAdapters_RejectEmptyRecipient(t *testing.T) {
	t.Parallel()

	sg := &sendgrid.SendGridClient{APIKey: "test"}
	tw := &twilio.TwilioClient{AccountSID: "test"}
	sl := &slack.SlackWebhook{WebhookURL: "https://hooks.example/test"}

	email, err := NewEmailAdapter(sg, "noreply@example.com")
	if err != nil {
		t.Fatalf("NewEmailAdapter: %v", err)
	}
	sms, err := NewSMSAdapter(tw, "+1-555-0000")
	if err != nil {
		t.Fatalf("NewSMSAdapter: %v", err)
	}
	slackAdapter, err := NewSlackAdapter(sl, "#alerts")
	if err != nil {
		t.Fatalf("NewSlackAdapter: %v", err)
	}

	tests := []struct {
		name    string
		adapter Notifier
	}{
		{name: "email", adapter: email},
		{name: "sms", adapter: sms},
		{name: "slack", adapter: slackAdapter},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.adapter.Send(context.Background(), "", "hello")
			if !errors.Is(err, ErrEmptyRecipient) {
				t.Errorf("err = %v, want ErrEmptyRecipient", err)
			}
		})
	}
}

func TestSMSAdapter_WrapsVendorErrorWithIs(t *testing.T) {
	t.Parallel()

	a, err := NewSMSAdapter(&twilio.TwilioClient{}, "+1-555-0000")
	if err != nil {
		t.Fatalf("NewSMSAdapter: %v", err)
	}
	err = a.Send(context.Background(), "+1-555-0101", "hi")
	if err == nil {
		t.Fatal("expected error wrapping the twilio failure, got nil")
	}
	var tErr *twilio.Error
	if !errors.As(err, &tErr) {
		t.Fatalf("errors.As should reach *twilio.Error, got: %v", err)
	}
	if tErr.Code != 20003 {
		t.Errorf("Code = %d, want 20003", tErr.Code)
	}
}

func TestEmailAdapter_RejectsEmptyFromAtConstruction(t *testing.T) {
	t.Parallel()

	_, err := NewEmailAdapter(&sendgrid.SendGridClient{APIKey: "test"}, "")
	if !errors.Is(err, ErrEmptyFromEmail) {
		t.Fatalf("err = %v, want ErrEmptyFromEmail", err)
	}
}

func TestSlackAdapter_PropagatesVendorSentinel(t *testing.T) {
	t.Parallel()

	a, err := NewSlackAdapter(&slack.SlackWebhook{WebhookURL: "https://hooks.example/down"}, "#alerts")
	if err != nil {
		t.Fatalf("NewSlackAdapter: %v", err)
	}
	err = a.Send(context.Background(), "alice", "hi")
	if err == nil {
		t.Fatal("expected error for 5xx webhook, got nil")
	}
	if !errors.Is(err, slack.ErrWebhookDown) {
		t.Fatalf("expected errors.Is to reach slack.ErrWebhookDown, got: %v", err)
	}
}

func TestMultiNotifier_JoinsPartialFailures(t *testing.T) {
	t.Parallel()

	bad := failingNotifier{err: errors.New("upstream down")}
	good, err := NewEmailAdapter(&sendgrid.SendGridClient{APIKey: "test"}, "noreply@example.com")
	if err != nil {
		t.Fatalf("NewEmailAdapter: %v", err)
	}
	multi, err := NewMultiNotifier(bad, good)
	if err != nil {
		t.Fatalf("NewMultiNotifier: %v", err)
	}

	err = multi.Send(context.Background(), "alice@example.com", "hi")
	if err == nil {
		t.Fatal("expected joined error from partial failure, got nil")
	}
	if !errors.Is(err, bad.err) {
		t.Fatalf("expected errors.Is to find sentinel in joined error, got: %v", err)
	}
}

func TestNewSMSAdapter_RejectsEmptyFrom(t *testing.T) {
	t.Parallel()

	_, err := NewSMSAdapter(&twilio.TwilioClient{AccountSID: "test"}, "")
	if !errors.Is(err, ErrEmptyFromSMS) {
		t.Fatalf("err = %v, want ErrEmptyFromSMS", err)
	}
}

func TestNewSlackAdapter_RejectsEmptyChannel(t *testing.T) {
	t.Parallel()

	_, err := NewSlackAdapter(&slack.SlackWebhook{WebhookURL: "https://hooks.example/test"}, "")
	if !errors.Is(err, ErrEmptyChannel) {
		t.Fatalf("err = %v, want ErrEmptyChannel", err)
	}
}

func TestNewEmailAdapter_RejectsNilClient(t *testing.T) {
	t.Parallel()

	_, err := NewEmailAdapter(nil, "x@example.com")
	if !errors.Is(err, ErrMissingClient) {
		t.Fatalf("err = %v, want ErrMissingClient", err)
	}
}

func TestNewSMSAdapter_RejectsNilClient(t *testing.T) {
	t.Parallel()

	_, err := NewSMSAdapter(nil, "+1")
	if !errors.Is(err, ErrMissingClient) {
		t.Fatalf("err = %v, want ErrMissingClient", err)
	}
}

func TestNewMultiNotifier_RejectsEmpty(t *testing.T) {
	t.Parallel()

	_, err := NewMultiNotifier()
	if !errors.Is(err, ErrNoNotifiers) {
		t.Fatalf("err = %v, want ErrNoNotifiers", err)
	}
}

func TestAccessorsExposeConfiguration(t *testing.T) {
	t.Parallel()

	email, _ := NewEmailAdapter(&sendgrid.SendGridClient{APIKey: "test"}, "noreply@example.com")
	sms, _ := NewSMSAdapter(&twilio.TwilioClient{AccountSID: "test"}, "+1-555-0000")
	sl, _ := NewSlackAdapter(&slack.SlackWebhook{WebhookURL: "https://hooks.example/test"}, "#alerts")

	if email.From() != "noreply@example.com" {
		t.Errorf("From = %q", email.From())
	}
	if sms.From() != "+1-555-0000" {
		t.Errorf("From = %q", sms.From())
	}
	if sl.Channel() != "#alerts" {
		t.Errorf("Channel = %q", sl.Channel())
	}
}

type failingNotifier struct{ err error }

func (f failingNotifier) Send(_ context.Context, _, _ string) error {
	return f.err
}
```

## Review

The adapters are correct when three properties hold. First, the port carries no vendor vocabulary: `Notifier.Send` takes a recipient and a message, and only the three adapter files import the `thirdparty` packages. If a vendor type appears in any signature outside an adapter, the boundary has leaked. Second, every vendor failure is reachable through the wrap: the SMS path keeps `*twilio.Error` recoverable by `errors.As`, and the Slack and SendGrid paths keep their sentinels reachable by `errors.Is`, because every wrapper uses `%w` and never `%v`. Third, the composite behaves like a leaf: `MultiNotifier.Send` returns `nil` on full success (that is what `errors.Join` of all-`nil` gives) and a single walkable error on partial failure.

The common mistakes here are the ones the tests are built to catch. Returning the vendor error verbatim instead of wrapping it forces every caller to import the vendor package to assert on failures; `TestSMSAdapter_WrapsVendorErrorWithIs` would still pass on a verbatim return, but the looser `errors.Is` chain in `TestSlackAdapter_PropagatesVendorSentinel` shows why the wrap with context is what you want. Dropping the HTTP status from the Slack error makes a 500 and a 400 indistinguishable to monitoring; the adapter folds the status into the wrapped message precisely so they stay distinct. Wrapping a single adapter in a `MultiNotifier` adds an allocation and a stack frame for a fan-out of one — use the leaf directly. Running `go test -race ./...` exercises every one of these paths, and the parallel subtests confirm the adapters hold no shared mutable state.

## Resources

- [Adapter pattern](https://refactoring.guru/design-patterns/adapter) — the canonical description of the object adapter and what it is for.
- [`errors.Join`](https://pkg.go.dev/errors#Join) — the standard-library function that powers `MultiNotifier`'s fan-out semantics.
- [Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — the official explanation of `%w`, `errors.Is`, and `errors.As`, the wrapping primitives every adapter here relies on.
- [Hexagonal Architecture](https://alistair.cockburn.us/hexagonal-architecture/) — Cockburn's ports-and-adapters article, where the "domain owns the port" rule comes from.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-payment-gateway-acl.md](02-payment-gateway-acl.md)
