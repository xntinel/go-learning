# Exercise 20: Webhook Delivery Runner with Anonymous Callback and Exponential Backoff

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde).

Delivering a webhook reliably means retrying transient failures with backoff and
giving up cleanly when the endpoint stays down — and escalating that failure instead
of swallowing it. This module builds a delivery runner that owns the entire retry
policy itself, takes the actual send as an anonymous callback, and uses a deferred
closure to escalate exactly when every attempt has failed.

This module is fully self-contained. Nothing here imports another exercise.

## What you'll build

```text
webhook/                      module example.com/webhook
  go.mod
  webhook.go                  Sender, Attempt, Deliver (retry runner), ExponentialBackoff
  webhook_test.go              success after retries, gives up, escalates, no escalation on success
  cmd/demo/main.go            a send callback that fails twice, then succeeds
```

- Files: `webhook.go`, `webhook_test.go`, `cmd/demo/main.go`.
- Implement: `Deliver(send, payload, maxAttempts, backoff, sleep, escalate)` retrying `send` with an injected `sleep` between attempts, and a deferred closure that calls `escalate` with the full attempt history only if the final outcome is still an error; `ExponentialBackoff(base)` returning a doubling backoff policy.
- Test: delivery succeeds after transient failures with the right number of attempts and waits; delivery gives up after `maxAttempts` and wraps the last error; `escalate` runs on final failure with the full history and does not run on success.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/webhook/cmd/demo
cd ~/go-exercises/webhook
go mod init example.com/webhook
go mod edit -go=1.24
```

### The callback owns delivery, `Deliver` owns the policy

`send` is the only thing that knows how to actually reach the remote endpoint — in
production it is a closure over an `*http.Client` and a URL, and here it can be as
simple as a closure over a counter. `Deliver` never sees any of that; it only calls
`send(payload)` and inspects the returned error. Everything about *when* to retry —
how many times, how long to wait between tries — is `Deliver`'s own responsibility,
driven by two more anonymous functions supplied at the call site: `backoff`, which
turns an attempt number into a wait duration, and `sleep`, which actually waits.
Injecting `sleep` instead of calling `time.Sleep` directly is what keeps the retry
loop's tests instant: a test's `sleep` just records the requested durations in a
slice.

The deferred closure is the escalation path. It runs after the loop exits, whichever
way it exits, and checks the named return `err`: if delivery is still failing, it
calls `escalate` with every attempt made so far, so a caller can page an on-call
rotation without threading that concern through the retry loop's control flow. If the
final attempt succeeded, `err` is `nil` and the closure does nothing.

Create `webhook.go`:

```go
package webhook

import (
	"fmt"
	"time"
)

// Sender delivers payload to a remote endpoint and reports whether it
// succeeded. In production it is an anonymous function closing over an
// *http.Client and a URL; tests pass a small closure that fails a fixed
// number of times.
type Sender func(payload string) error

// Attempt records the outcome of a single delivery attempt.
type Attempt struct {
	N   int
	Err error
}

// Deliver owns the entire retry policy: it calls send up to maxAttempts
// times, waiting backoff(attempt) between tries via sleep, and stops at the
// first success. sleep is injected so tests never wait on a real clock.
// backoff is a small anonymous function supplied by the caller — the
// exponential-backoff policy lives entirely in that one call-site literal,
// not inside Deliver.
//
// A deferred closure runs after every attempt loop: if the delivery is
// still failing when Deliver returns, it invokes escalate with the full
// attempt history, so callers can page an on-call rotation or open an
// incident without threading that concern through the retry loop itself.
func Deliver(send Sender, payload string, maxAttempts int, backoff func(attempt int) time.Duration, sleep func(time.Duration), escalate func([]Attempt)) (attempts []Attempt, err error) {
	defer func() {
		if err != nil && escalate != nil {
			escalate(attempts)
		}
	}()

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		aerr := send(payload)
		attempts = append(attempts, Attempt{N: attempt, Err: aerr})
		if aerr == nil {
			err = nil
			return attempts, nil
		}
		err = aerr
		if attempt < maxAttempts {
			sleep(backoff(attempt))
		}
	}
	err = fmt.Errorf("webhook delivery failed after %d attempts: %w", maxAttempts, err)
	return attempts, err
}

// ExponentialBackoff returns a backoff policy doubling base on every
// attempt: base, 2*base, 4*base, ...
func ExponentialBackoff(base time.Duration) func(attempt int) time.Duration {
	return func(attempt int) time.Duration {
		if attempt < 1 {
			attempt = 1
		}
		return base * time.Duration(1<<uint(attempt-1))
	}
}
```

### The runnable demo

The demo's `send` callback fails twice, then succeeds; `sleep` just records the
requested waits instead of actually sleeping, so the demo runs instantly and its
output is deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"time"

	"example.com/webhook"
)

func main() {
	tries := 0
	send := func(payload string) error {
		tries++
		if tries < 3 {
			return errors.New("connection reset")
		}
		return nil
	}

	var waited []time.Duration
	sleep := func(d time.Duration) { waited = append(waited, d) }

	attempts, err := webhook.Deliver(send, `{"event":"order.paid"}`, 5,
		webhook.ExponentialBackoff(100*time.Millisecond), sleep, nil)

	fmt.Println("attempts:", len(attempts))
	fmt.Println("waited:", waited)
	fmt.Println("final err:", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
attempts: 3
waited: [100ms 200ms]
final err: <nil>
```

### Tests

`TestDeliverSucceedsAfterRetries` checks the attempt count and the exact exponential
waits (100ms doubling, and no wait after the final success).
`TestDeliverGivesUpAfterMaxAttempts` checks a permanently failing `send` exhausts
every attempt, waits between each but the last, and returns a wrapped error.
`TestDeliverEscalatesOnFinalFailure` and `TestDeliverDoesNotEscalateOnSuccess` prove
the deferred closure calls `escalate` exactly when — and only when — delivery ends
in failure.

Create `webhook_test.go`:

```go
package webhook

import (
	"errors"
	"testing"
	"time"
)

func noSleep() (func(time.Duration), *[]time.Duration) {
	var waited []time.Duration
	return func(d time.Duration) { waited = append(waited, d) }, &waited
}

func TestDeliverSucceedsAfterRetries(t *testing.T) {
	t.Parallel()
	tries := 0
	send := func(string) error {
		tries++
		if tries < 3 {
			return errors.New("temporary failure")
		}
		return nil
	}
	sleep, waited := noSleep()

	attempts, err := Deliver(send, "p", 5, ExponentialBackoff(time.Millisecond), sleep, nil)
	if err != nil {
		t.Fatalf("Deliver() err = %v, want nil", err)
	}
	if len(attempts) != 3 {
		t.Fatalf("attempts = %d, want 3", len(attempts))
	}
	if len(*waited) != 2 {
		t.Fatalf("slept %d times, want 2 (no sleep after the final success)", len(*waited))
	}
	if (*waited)[0] != time.Millisecond || (*waited)[1] != 2*time.Millisecond {
		t.Fatalf("waited = %v, want [1ms 2ms] (exponential backoff)", *waited)
	}
}

func TestDeliverGivesUpAfterMaxAttempts(t *testing.T) {
	t.Parallel()
	want := errors.New("permanent failure")
	send := func(string) error { return want }
	sleep, waited := noSleep()

	attempts, err := Deliver(send, "p", 4, ExponentialBackoff(time.Millisecond), sleep, nil)
	if err == nil || !errors.Is(err, want) {
		t.Fatalf("Deliver() err = %v, want wrapping %v", err, want)
	}
	if len(attempts) != 4 {
		t.Fatalf("attempts = %d, want 4", len(attempts))
	}
	if len(*waited) != 3 {
		t.Fatalf("slept %d times, want 3 (no sleep after the last attempt)", len(*waited))
	}
}

func TestDeliverEscalatesOnFinalFailure(t *testing.T) {
	t.Parallel()
	send := func(string) error { return errors.New("down") }
	sleep, _ := noSleep()

	var escalated []Attempt
	escalate := func(a []Attempt) { escalated = a }

	_, err := Deliver(send, "p", 2, ExponentialBackoff(time.Millisecond), sleep, escalate)
	if err == nil {
		t.Fatal("Deliver() err = nil, want failure")
	}
	if len(escalated) != 2 {
		t.Fatalf("escalated %d attempts, want 2", len(escalated))
	}
}

func TestDeliverDoesNotEscalateOnSuccess(t *testing.T) {
	t.Parallel()
	send := func(string) error { return nil }
	sleep, _ := noSleep()

	escalatedCalled := false
	escalate := func([]Attempt) { escalatedCalled = true }

	_, err := Deliver(send, "p", 3, ExponentialBackoff(time.Millisecond), sleep, escalate)
	if err != nil {
		t.Fatalf("Deliver() err = %v, want nil", err)
	}
	if escalatedCalled {
		t.Fatal("escalate was called after a successful delivery, want not called")
	}
}
```

## Review

`Deliver` is correct when the attempt count, the wait sequence, and the escalation
decision all agree with the outcome of `send`. The wait-sequence tests are the
sharpest ones: they prove `Deliver` never sleeps after the attempt that actually
succeeds and never sleeps after the last attempt it is going to make, both of which
are easy off-by-one mistakes in a hand-written retry loop. The escalation split — a
deferred closure reading the named return, exactly as in the audit-logger exercise —
is what keeps "retry" and "alert on giving up" as two independent concerns instead of
tangling an alerting call inside the loop body itself.

## Resources

- [Go Language Specification: Defer statements](https://go.dev/ref/spec#Defer_statements)
- [Google SRE Workbook: retries and backoff](https://sre.google/workbook/implementing-slos/)
- [Stripe API docs: webhook retry schedule](https://docs.stripe.com/webhooks#retries)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [19-stream-sampler-goroutine-fanout.md](19-stream-sampler-goroutine-fanout.md) | Next: [21-oncefunc-signal-handler.md](21-oncefunc-signal-handler.md)
