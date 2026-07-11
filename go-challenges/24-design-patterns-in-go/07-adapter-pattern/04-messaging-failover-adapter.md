# Exercise 4: A Resilient Messaging Port With Retry And Failover

A senior system rarely has just one way to reach a user. It has three vendors with three incompatible failure models, and it has to keep working when one of them is having a bad day. This exercise builds a single domain `Sender` port, three adapters that translate three vendor SDKs into it, and two *decorator-adapters* that wrap any `Sender`: a `RetryingSender` that retries only the failures worth retrying, and a `FailoverSender` that moves to the next provider when one is down. The hinge that makes the decorators possible is error translation: every adapter collapses its vendor's idiosyncratic failure into one of three domain error classes, so the policy code can decide what to do without importing a single vendor package.

This module is fully self-contained. It begins with its own `go mod init`, simulates the three vendor SDKs in their own sub-packages, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
messaging.go                      Message, Sender port; Email/SMS/Webhook adapters; RetryingSender, FailoverSender
thirdparty/
  smtpx/smtpx.go                  simulated SMTP library (wrapped sentinels: greylisted=transient, bad recipient=permanent)
  smsx/smsx.go                    simulated SMS API (returns *APIError with an HTTP-style Code)
  hookx/hookx.go                  simulated chat webhook (returns an (int status, error) pair)
cmd/
  demo/main.go                    wires retry over failover over three providers, then shows translation
messaging_test.go                 fakes assert translation, retry policy, and failover behavior
```

- Files: `thirdparty/smtpx/smtpx.go`, `thirdparty/smsx/smsx.go`, `thirdparty/hookx/hookx.go`, `messaging.go`, `cmd/demo/main.go`, `messaging_test.go`.
- Implement: the `Sender` port; `EmailSender`, `SMSSender`, `WebhookSender` translating each vendor failure into `ErrTransient` or `ErrPermanent`; `RetryingSender` and `FailoverSender` decorators.
- Test: each adapter's error translation via `errors.Is`/`errors.As`, that retry repeats only transient failures and gives up after N, that failover falls through to a healthy provider but short-circuits a bad message, and that the two decorators compose.
- Verify: `go test -race ./...` and `go run ./cmd/demo`.

Set up the module:

```bash
mkdir -p messaging/thirdparty/smtpx messaging/thirdparty/smsx messaging/thirdparty/hookx messaging/cmd/demo
cd messaging
go mod init example.com/messaging
```

### Three vendors, three failure models

The whole reason an adapter earns its keep here is that the three vendors disagree about how to report a failure, and a retry policy needs them to agree. The SMTP stand-in returns idiomatic wrapped sentinels: `ErrGreylisted` is a transient 4xx that will likely succeed on a retry, and `ErrBadRecipient` is a permanent 5xx that never will. The SMS stand-in returns a structured `*APIError` carrying an HTTP-style numeric `Code`, the kind of error a caller reaches with `errors.As`. The webhook stand-in is the awkward one: it reports failure as an `(int, error)` pair where the `int` is an HTTP status the domain never models, so the adapter has to read the status to decide the class. Build all three first.
Create `thirdparty/smtpx/smtpx.go`:

```go
package smtpx

import (
	"errors"
	"fmt"
)

// Envelope is the vendor's idea of an email. Note the field names and the
// fact that the body is called "Content" - nothing here matches our domain.
type Envelope struct {
	From    string
	To      string
	Subject string
	Content string
}

// ErrGreylisted is a transient SMTP 4xx condition: retry later and it may work.
var ErrGreylisted = errors.New("smtpx: 421 greylisted, try again later")

// ErrBadRecipient is a permanent SMTP 5xx condition: the address is wrong.
var ErrBadRecipient = errors.New("smtpx: 550 no such recipient")

// Client is a stand-in for a real SMTP library.
type Client struct {
	Host string
}

// Deliver returns a wrapped sentinel so callers can classify with errors.Is.
func (c *Client) Deliver(env Envelope) error {
	if c.Host == "" {
		return errors.New("smtpx: host is required")
	}
	switch env.To {
	case "greylisted@example.com":
		return fmt.Errorf("smtpx: delivery to %s: %w", env.To, ErrGreylisted)
	case "missing@example.com":
		return fmt.Errorf("smtpx: delivery to %s: %w", env.To, ErrBadRecipient)
	}
	return nil
}
```

Create `thirdparty/smsx/smsx.go`:

```go
package smsx

import "fmt"

// APIError is the vendor's structured error: an HTTP-ish numeric code plus a
// human message. A caller that wants the code must reach it with errors.As.
type APIError struct {
	Code    int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("smsx: code=%d message=%s", e.Code, e.Message)
}

// Gateway is a stand-in for an SMS HTTP API.
type Gateway struct {
	AccountSID string
}

// SendText returns (messageSID, error). The error is always an *APIError whose
// Code follows HTTP conventions: 429/503 are transient, 4xx are permanent.
func (g *Gateway) SendText(to, text string) (string, error) {
	if g.AccountSID == "" {
		return "", &APIError{Code: 401, Message: "missing account sid"}
	}
	switch to {
	case "+1-555-0429":
		return "", &APIError{Code: 429, Message: "rate limited"}
	case "+1-555-0400":
		return "", &APIError{Code: 400, Message: "invalid destination"}
	}
	return "sms-" + to, nil
}
```

Create `thirdparty/hookx/hookx.go`:

```go
package hookx

import (
	"errors"
	"net/http"
)

// ErrBadStatus is returned alongside a non-2xx status code. The webhook vendor
// reports failure as an (int, error) pair, status-code-as-error style.
var ErrBadStatus = errors.New("hookx: non-2xx response")

// Webhook is a stand-in for a chat webhook (Slack/Teams style).
type Webhook struct {
	URL string
}

// Post returns the HTTP status and, for non-2xx, ErrBadStatus. A 5xx is
// transient; a 4xx is permanent.
func (w *Webhook) Post(payload []byte) (int, error) {
	if w.URL == "" {
		return 0, errors.New("hookx: url is required")
	}
	switch w.URL {
	case "https://hooks.example/5xx":
		return http.StatusBadGateway, ErrBadStatus
	case "https://hooks.example/4xx":
		return http.StatusBadRequest, ErrBadStatus
	}
	return http.StatusOK, nil
}
```

### The port, the adapters, and the two decorators

Everything domain-facing lives in one file, and it is worth reading top to bottom as a single idea: a `Sender` interface that both the leaf adapters and the wrapping decorators satisfy, which is exactly what lets a decorator hold a `Sender` without caring whether it wraps a real vendor or another decorator.

The three domain error classes are the load-bearing design choice. `ErrInvalidMessage` means the request is wrong and no provider can fix it. `ErrTransient` means try again, maybe elsewhere. `ErrPermanent` means this provider refused and a retry is pointless. Each adapter's job is to map its vendor's failure onto exactly one class while keeping the original vendor error reachable, and it does that with `fmt.Errorf` and *two* `%w` verbs: `fmt.Errorf("...: %w: %w", ErrTransient, err)` makes both `errors.Is(e, ErrTransient)` and `errors.Is(e, smtpx.ErrGreylisted)` true at once. The class is what the policy code matches on; the wrapped vendor error is what a human reads in the log.

Watch how each adapter classifies. `EmailSender` switches on the SMTP sentinel: greylisting is transient, a bad recipient is permanent. `SMSSender` pulls the `*APIError` out with `errors.As` and reads its `Code`: a 429 or any 5xx is transient, everything else permanent. `WebhookSender` reads the returned status directly and folds it into the wrap, so a 502 becomes transient and a 400 permanent. Validation happens first in every adapter, so a missing recipient fails as `ErrInvalidMessage` before any vendor call.

The two decorators are where the translation pays off. `RetryingSender.Send` calls its inner sender, returns immediately on success, returns immediately on any error that is *not* `ErrTransient` (retrying a permanent failure is waste), and otherwise loops up to `attempts` times with a growing backoff. The `sleep` field is a function so a test can replace `time.Sleep` with a no-op and run instantly under the race detector. `FailoverSender.Send` walks its providers in order, returns on the first success, short-circuits immediately on `ErrInvalidMessage` (no point failing a bad message over to a second provider), and otherwise collects the errors and joins them so the caller can still inspect every class that was tried.
Create `messaging.go`:

```go
package messaging

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"example.com/messaging/thirdparty/hookx"
	"example.com/messaging/thirdparty/smsx"
	"example.com/messaging/thirdparty/smtpx"
)

// Message is the domain's vendor-free notification. Every provider has to be
// expressed in these words.
type Message struct {
	To      string
	Subject string
	Body    string
}

// Sender is the port. Adapters and decorators all implement this one method,
// which is what lets a decorator wrap a leaf without knowing which it is.
type Sender interface {
	Send(ctx context.Context, msg Message) error
}

// Domain error classes. Adapters translate every vendor failure into a wrap of
// exactly one of these, so the retry decorator can decide policy without
// importing any vendor package.
var (
	ErrInvalidMessage = errors.New("messaging: invalid message")
	ErrTransient      = errors.New("messaging: transient delivery failure")
	ErrPermanent      = errors.New("messaging: permanent delivery failure")
)

func validate(msg Message) error {
	if msg.To == "" {
		return fmt.Errorf("%w: recipient is required", ErrInvalidMessage)
	}
	return nil
}

// EmailSender adapts smtpx.Client to the Sender port.
type EmailSender struct {
	client *smtpx.Client
	from   string
}

func NewEmailSender(client *smtpx.Client, from string) (*EmailSender, error) {
	if client == nil {
		return nil, errors.New("messaging: smtpx client is required")
	}
	if from == "" {
		return nil, errors.New("messaging: from address is required")
	}
	return &EmailSender{client: client, from: from}, nil
}

func (s *EmailSender) Send(_ context.Context, msg Message) error {
	if err := validate(msg); err != nil {
		return err
	}
	err := s.client.Deliver(smtpx.Envelope{
		From:    s.from,
		To:      msg.To,
		Subject: msg.Subject,
		Content: msg.Body,
	})
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, smtpx.ErrGreylisted):
		return fmt.Errorf("email to %s: %w: %w", msg.To, ErrTransient, err)
	case errors.Is(err, smtpx.ErrBadRecipient):
		return fmt.Errorf("email to %s: %w: %w", msg.To, ErrPermanent, err)
	default:
		return fmt.Errorf("email to %s: %w: %w", msg.To, ErrTransient, err)
	}
}

// SMSSender adapts smsx.Gateway to the Sender port.
type SMSSender struct {
	gateway *smsx.Gateway
}

func NewSMSSender(gateway *smsx.Gateway) (*SMSSender, error) {
	if gateway == nil {
		return nil, errors.New("messaging: smsx gateway is required")
	}
	return &SMSSender{gateway: gateway}, nil
}

func (s *SMSSender) Send(_ context.Context, msg Message) error {
	if err := validate(msg); err != nil {
		return err
	}
	_, err := s.gateway.SendText(msg.To, msg.Body)
	if err == nil {
		return nil
	}
	var apiErr *smsx.APIError
	if errors.As(err, &apiErr) {
		class := ErrPermanent
		if apiErr.Code == http.StatusTooManyRequests || apiErr.Code >= 500 {
			class = ErrTransient
		}
		return fmt.Errorf("sms to %s: %w: %w", msg.To, class, err)
	}
	return fmt.Errorf("sms to %s: %w: %w", msg.To, ErrTransient, err)
}

// WebhookSender adapts hookx.Webhook to the Sender port, folding the status
// code into the error class.
type WebhookSender struct {
	hook *hookx.Webhook
}

func NewWebhookSender(hook *hookx.Webhook) (*WebhookSender, error) {
	if hook == nil {
		return nil, errors.New("messaging: hookx webhook is required")
	}
	return &WebhookSender{hook: hook}, nil
}

func (s *WebhookSender) Send(_ context.Context, msg Message) error {
	if err := validate(msg); err != nil {
		return err
	}
	status, err := s.hook.Post([]byte(msg.Body))
	if err == nil {
		return nil
	}
	class := ErrPermanent
	if status >= 500 || status == 0 {
		class = ErrTransient
	}
	return fmt.Errorf("webhook (status=%d): %w: %w", status, class, err)
}

// RetryingSender is a decorator-adapter: it implements Sender by wrapping
// another Sender and retrying only failures classified as ErrTransient.
type RetryingSender struct {
	inner    Sender
	attempts int
	backoff  time.Duration
	sleep    func(time.Duration)
}

func NewRetryingSender(inner Sender, attempts int, backoff time.Duration) (*RetryingSender, error) {
	if inner == nil {
		return nil, errors.New("messaging: inner sender is required")
	}
	if attempts < 1 {
		return nil, errors.New("messaging: attempts must be >= 1")
	}
	return &RetryingSender{inner: inner, attempts: attempts, backoff: backoff, sleep: time.Sleep}, nil
}

func (r *RetryingSender) Send(ctx context.Context, msg Message) error {
	var last error
	for attempt := 1; attempt <= r.attempts; attempt++ {
		last = r.inner.Send(ctx, msg)
		if last == nil {
			return nil
		}
		if !errors.Is(last, ErrTransient) {
			return last
		}
		if attempt < r.attempts {
			r.sleep(r.backoff * time.Duration(attempt))
		}
	}
	return fmt.Errorf("gave up after %d attempts: %w", r.attempts, last)
}

// FailoverSender is a decorator-adapter that tries each provider in order and
// returns on the first success. A bad message short-circuits: no provider can
// fix it, so failing over is pointless.
type FailoverSender struct {
	providers []Sender
}

func NewFailoverSender(providers ...Sender) (*FailoverSender, error) {
	if len(providers) == 0 {
		return nil, errors.New("messaging: at least one provider is required")
	}
	return &FailoverSender{providers: providers}, nil
}

func (f *FailoverSender) Send(ctx context.Context, msg Message) error {
	var errs []error
	for _, p := range f.providers {
		err := p.Send(ctx, msg)
		if err == nil {
			return nil
		}
		if errors.Is(err, ErrInvalidMessage) {
			return err
		}
		errs = append(errs, err)
	}
	return fmt.Errorf("all providers failed: %w", errors.Join(errs...))
}
```

Notice that `Send` takes a `context.Context` it does not always use: `smtpx.Deliver`, `smsx.SendText`, and `hookx.Post` have no context parameter, but the port keeps one because a uniform signature is the entire point of a port, and a real implementation would pass it to an HTTP request. Accepting what the domain offers and using what the vendor provides is the adapter's defining move.

### A runnable demo

The demo builds the realistic shape: a `RetryingSender` wrapping a `FailoverSender` wrapping all three providers, so a transient blip retries and a dead provider fails over. It then triggers each translation path so you can see the class flags, and finishes with a failover that survives a primary error.
Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"example.com/messaging"
	"example.com/messaging/thirdparty/hookx"
	"example.com/messaging/thirdparty/smsx"
	"example.com/messaging/thirdparty/smtpx"
)

func main() {
	ctx := context.Background()

	email, err := messaging.NewEmailSender(&smtpx.Client{Host: "mail.example.com"}, "noreply@example.com")
	if err != nil {
		log.Fatalf("email: %v", err)
	}
	sms, err := messaging.NewSMSSender(&smsx.Gateway{AccountSID: "sid"})
	if err != nil {
		log.Fatalf("sms: %v", err)
	}
	hook, err := messaging.NewWebhookSender(&hookx.Webhook{URL: "https://hooks.example/ok"})
	if err != nil {
		log.Fatalf("hook: %v", err)
	}

	// Email is the primary; failover prefers email, then sms, then webhook.
	failover, err := messaging.NewFailoverSender(email, sms, hook)
	if err != nil {
		log.Fatalf("failover: %v", err)
	}
	resilient, err := messaging.NewRetryingSender(failover, 3, 10*time.Millisecond)
	if err != nil {
		log.Fatalf("retry: %v", err)
	}

	msg := messaging.Message{To: "alice@example.com", Subject: "Welcome", Body: "hello"}
	if err := resilient.Send(ctx, msg); err != nil {
		fmt.Printf("send failed: %v\n", err)
	} else {
		fmt.Println("delivered via resilient pipeline")
	}

	fmt.Println("--- error translation ---")

	greylisted := email.Send(ctx, messaging.Message{To: "greylisted@example.com", Body: "x"})
	fmt.Printf("greylisted transient=%v permanent=%v\n",
		errors.Is(greylisted, messaging.ErrTransient), errors.Is(greylisted, messaging.ErrPermanent))

	missing := email.Send(ctx, messaging.Message{To: "missing@example.com", Body: "x"})
	fmt.Printf("missing transient=%v permanent=%v\n",
		errors.Is(missing, messaging.ErrTransient), errors.Is(missing, messaging.ErrPermanent))

	rate := sms.Send(ctx, messaging.Message{To: "+1-555-0429", Body: "x"})
	var apiErr *smsx.APIError
	errors.As(rate, &apiErr)
	fmt.Printf("sms 429 transient=%v code=%d\n", errors.Is(rate, messaging.ErrTransient), apiErr.Code)

	fmt.Println("--- failover ---")

	// A down email plus a healthy sms: failover should still deliver.
	downEmail, _ := messaging.NewEmailSender(&smtpx.Client{Host: "mail.example.com"}, "noreply@example.com")
	fo, _ := messaging.NewFailoverSender(downEmail, sms)
	if err := fo.Send(ctx, messaging.Message{To: "greylisted@example.com", Body: "x"}); err != nil {
		fmt.Printf("failover failed: %v\n", err)
	} else {
		fmt.Println("failover delivered after primary error")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
delivered via resilient pipeline
--- error translation ---
greylisted transient=true permanent=false
missing transient=false permanent=true
sms 429 transient=true code=429
--- failover ---
failover delivered after primary error
```

### Tests

The tests pin the two things this design exists to guarantee: that translation is correct and that policy acts on the translation. A `scriptedSender` fake returns a queue of errors and counts its calls, which is all you need to prove retry and failover behavior deterministically, with no real network and no real sleeping. The translation tests drive the real adapters against the simulated vendors and assert both the domain class (`errors.Is`) and the recovered vendor detail (`errors.As` on `*smsx.APIError`). The policy tests assert that retry repeats a transient failure, refuses to repeat a permanent one, gives up after `attempts`, that failover falls through to a healthy provider, short-circuits a bad message, and that the two decorators compose into one resilient pipeline.
Create `messaging_test.go`:

```go
package messaging

import (
	"context"
	"errors"
	"testing"
	"time"

	"example.com/messaging/thirdparty/hookx"
	"example.com/messaging/thirdparty/smsx"
	"example.com/messaging/thirdparty/smtpx"
)

// scriptedSender is a fake leaf Sender. It returns the next error from script
// on each call (nil means success) and records how many times it was called.
type scriptedSender struct {
	script []error
	calls  int
}

func (s *scriptedSender) Send(_ context.Context, _ Message) error {
	i := s.calls
	s.calls++
	if i < len(s.script) {
		return s.script[i]
	}
	return nil
}

var okMsg = Message{To: "alice@example.com", Subject: "hi", Body: "hello"}

func TestEmailSender_TranslatesTransientSentinel(t *testing.T) {
	t.Parallel()
	s, err := NewEmailSender(&smtpx.Client{Host: "mail.example.com"}, "noreply@example.com")
	if err != nil {
		t.Fatalf("NewEmailSender: %v", err)
	}
	err = s.Send(context.Background(), Message{To: "greylisted@example.com", Body: "x"})
	if !errors.Is(err, ErrTransient) {
		t.Fatalf("err = %v, want ErrTransient", err)
	}
	if !errors.Is(err, smtpx.ErrGreylisted) {
		t.Fatalf("vendor sentinel lost: %v", err)
	}
}

func TestEmailSender_TranslatesPermanentSentinel(t *testing.T) {
	t.Parallel()
	s, _ := NewEmailSender(&smtpx.Client{Host: "mail.example.com"}, "noreply@example.com")
	err := s.Send(context.Background(), Message{To: "missing@example.com", Body: "x"})
	if !errors.Is(err, ErrPermanent) {
		t.Fatalf("err = %v, want ErrPermanent", err)
	}
}

func TestSMSSender_TranslatesByCode(t *testing.T) {
	t.Parallel()
	s, _ := NewSMSSender(&smsx.Gateway{AccountSID: "sid"})

	transient := s.Send(context.Background(), Message{To: "+1-555-0429", Body: "x"})
	if !errors.Is(transient, ErrTransient) {
		t.Errorf("429 => %v, want ErrTransient", transient)
	}
	permanent := s.Send(context.Background(), Message{To: "+1-555-0400", Body: "x"})
	if !errors.Is(permanent, ErrPermanent) {
		t.Errorf("400 => %v, want ErrPermanent", permanent)
	}
	var apiErr *smsx.APIError
	if !errors.As(permanent, &apiErr) || apiErr.Code != 400 {
		t.Errorf("errors.As should reach *smsx.APIError with Code 400, got %v", permanent)
	}
}

func TestWebhookSender_FoldsStatusIntoClass(t *testing.T) {
	t.Parallel()
	srv, _ := NewWebhookSender(&hookx.Webhook{URL: "https://hooks.example/5xx"})
	if err := srv.Send(context.Background(), okMsg); !errors.Is(err, ErrTransient) {
		t.Errorf("5xx => %v, want ErrTransient", err)
	}
	cli, _ := NewWebhookSender(&hookx.Webhook{URL: "https://hooks.example/4xx"})
	if err := cli.Send(context.Background(), okMsg); !errors.Is(err, ErrPermanent) {
		t.Errorf("4xx => %v, want ErrPermanent", err)
	}
}

func TestRetryingSender_RetriesTransientThenSucceeds(t *testing.T) {
	t.Parallel()
	// One transient failure, then success.
	leaf := &scriptedSender{script: []error{ErrTransient}}
	r, err := NewRetryingSender(leaf, 3, time.Millisecond)
	if err != nil {
		t.Fatalf("NewRetryingSender: %v", err)
	}
	r.sleep = func(time.Duration) {} // no real sleeping under -race
	if err := r.Send(context.Background(), okMsg); err != nil {
		t.Fatalf("Send = %v, want success after retry", err)
	}
	if leaf.calls != 2 {
		t.Errorf("calls = %d, want 2 (one fail, one success)", leaf.calls)
	}
}

func TestRetryingSender_DoesNotRetryPermanent(t *testing.T) {
	t.Parallel()
	leaf := &scriptedSender{script: []error{ErrPermanent, ErrPermanent}}
	r, _ := NewRetryingSender(leaf, 5, time.Millisecond)
	r.sleep = func(time.Duration) {}
	err := r.Send(context.Background(), okMsg)
	if !errors.Is(err, ErrPermanent) {
		t.Fatalf("err = %v, want ErrPermanent", err)
	}
	if leaf.calls != 1 {
		t.Errorf("calls = %d, want 1 (permanent must not retry)", leaf.calls)
	}
}

func TestRetryingSender_GivesUpAfterAttempts(t *testing.T) {
	t.Parallel()
	leaf := &scriptedSender{script: []error{ErrTransient, ErrTransient, ErrTransient}}
	r, _ := NewRetryingSender(leaf, 3, time.Millisecond)
	r.sleep = func(time.Duration) {}
	err := r.Send(context.Background(), okMsg)
	if !errors.Is(err, ErrTransient) {
		t.Fatalf("err = %v, want ErrTransient", err)
	}
	if leaf.calls != 3 {
		t.Errorf("calls = %d, want 3", leaf.calls)
	}
}

func TestFailoverSender_FallsThroughToHealthyProvider(t *testing.T) {
	t.Parallel()
	down := &scriptedSender{script: []error{ErrTransient}}
	up := &scriptedSender{script: []error{nil}}
	f, err := NewFailoverSender(down, up)
	if err != nil {
		t.Fatalf("NewFailoverSender: %v", err)
	}
	if err := f.Send(context.Background(), okMsg); err != nil {
		t.Fatalf("Send = %v, want failover success", err)
	}
	if down.calls != 1 || up.calls != 1 {
		t.Errorf("calls down=%d up=%d, want 1 and 1", down.calls, up.calls)
	}
}

func TestFailoverSender_ShortCircuitsOnInvalidMessage(t *testing.T) {
	t.Parallel()
	first := &scriptedSender{script: []error{ErrInvalidMessage}}
	second := &scriptedSender{script: []error{nil}}
	f, _ := NewFailoverSender(first, second)
	err := f.Send(context.Background(), Message{Body: "no recipient"})
	if !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("err = %v, want ErrInvalidMessage", err)
	}
	if second.calls != 0 {
		t.Errorf("second.calls = %d, want 0 (must not fail over a bad message)", second.calls)
	}
}

func TestFailoverSender_JoinsWhenAllFail(t *testing.T) {
	t.Parallel()
	a := &scriptedSender{script: []error{ErrTransient}}
	b := &scriptedSender{script: []error{ErrPermanent}}
	f, _ := NewFailoverSender(a, b)
	err := f.Send(context.Background(), okMsg)
	if !errors.Is(err, ErrTransient) || !errors.Is(err, ErrPermanent) {
		t.Fatalf("joined err must reach both classes, got %v", err)
	}
}

func TestRetryThenFailover_Compose(t *testing.T) {
	t.Parallel()
	// A flaky primary that always returns transient, wrapped in retry, then
	// behind a failover with a healthy secondary.
	flaky := &scriptedSender{script: []error{ErrTransient, ErrTransient}}
	retried, _ := NewRetryingSender(flaky, 2, time.Millisecond)
	retried.sleep = func(time.Duration) {}
	healthy := &scriptedSender{script: []error{nil}}
	f, _ := NewFailoverSender(retried, healthy)
	if err := f.Send(context.Background(), okMsg); err != nil {
		t.Fatalf("composed Send = %v, want success via failover", err)
	}
	if flaky.calls != 2 || healthy.calls != 1 {
		t.Errorf("flaky=%d healthy=%d, want 2 and 1", flaky.calls, healthy.calls)
	}
}

func TestConstructors_RejectBadInput(t *testing.T) {
	t.Parallel()
	if _, err := NewEmailSender(nil, "x"); err == nil {
		t.Error("nil client must be rejected")
	}
	if _, err := NewRetryingSender(nil, 1, 0); err == nil {
		t.Error("nil inner must be rejected")
	}
	if _, err := NewRetryingSender(&scriptedSender{}, 0, 0); err == nil {
		t.Error("attempts < 1 must be rejected")
	}
	if _, err := NewFailoverSender(); err == nil {
		t.Error("empty failover must be rejected")
	}
}
```

## Review

The design is correct when three properties hold. First, no vendor vocabulary escapes an adapter: only the three adapter methods import the `thirdparty` packages, and the decorators and policy code speak only `ErrTransient`, `ErrPermanent`, and `ErrInvalidMessage`. If a vendor type appears in `RetryingSender` or `FailoverSender`, the boundary has leaked and the decorators are no longer reusable across providers. Second, every classification keeps its evidence: because each wrap uses two `%w` verbs, a test and a log can both reach the domain class and the original vendor error from the same value. Third, the policy matches the class: retry repeats only `ErrTransient`, and failover refuses to move an `ErrInvalidMessage` to another provider.

The common mistakes are the ones the tests are built to catch. Classifying every failure as transient turns a permanent 5xx-recipient error into an infinite-looking retry storm; `TestRetryingSender_DoesNotRetryPermanent` asserts the leaf is called exactly once. Using `%v` instead of `%w` for the vendor error compiles and reads fine but silently breaks `errors.As`, so `TestSMSSender_TranslatesByCode` would fail to recover the `Code`. Failing a bad message over to every provider in turn multiplies one client error into N identical failures and N times the load; `TestFailoverSender_ShortCircuitsOnInvalidMessage` asserts the second provider is never called. Sleeping for real in `RetryingSender` makes the suite slow and flaky under `-race`; injecting `sleep` as a function and replacing it with a no-op keeps the policy logic fully tested in microseconds. Running `go test -race ./...` exercises every path, and the parallel subtests confirm the adapters hold no shared mutable state.

## Resources

- [Adapter pattern](https://refactoring.guru/design-patterns/adapter) — the canonical object-adapter description, the structure both the leaf adapters and the decorators follow.
- [`fmt.Errorf` and `%w`](https://pkg.go.dev/fmt#Errorf) — the documented rule that multiple `%w` verbs produce a multi-error `Unwrap`, which is how each adapter keeps the domain class and the vendor error reachable at once.
- [`errors.Join`](https://pkg.go.dev/errors#Join) — the standard-library function `FailoverSender` uses to report every provider it tried.
- [Addressing Cascading Failures](https://sre.google/sre-book/addressing-cascading-failures/) — the Google SRE Book chapter on why blind retries amplify outages, the reason retry policy keys on a transient/permanent distinction.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-io-stream-adapters.md](03-io-stream-adapters.md) | Next: [05-legacy-api-anti-corruption.md](05-legacy-api-anti-corruption.md)
