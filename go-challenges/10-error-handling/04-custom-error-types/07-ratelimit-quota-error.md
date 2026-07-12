# Exercise 7: A QuotaExceededError Carrying Retry-After for the Handler

A custom error type can be a structured signal that crosses two layers carrying
actionable data. This module builds a rate-limiter gate returning a
`*QuotaExceededError{Limit, Remaining, RetryAfter}` that an HTTP handler
translates into a `429 Too Many Requests` with a correct `Retry-After` header —
the error, not a message, carries exactly what the handler needs.

This module is fully self-contained: its own module, code, demo, and tests.
Nothing here imports any other exercise.

## What you'll build

```text
quotaerr/                  independent module: example.com/quotaerr
  go.mod                   go 1.24
  quotaerr.go              QuotaExceededError; Limiter.Allow; Handler
  cmd/
    demo/
      main.go              sends N+1 requests, prints the 429 and Retry-After
  quotaerr_test.go         429+header past limit, 200 under, As reads Limit/Remaining
```

Files: `quotaerr.go`, `cmd/demo/main.go`, `quotaerr_test.go`.
Implement: `*QuotaExceededError` with `Error()`, a `Limiter` whose `Allow()` returns it when over budget, and an `http.Handler` that maps it to a 429 with `Retry-After`.
Test: driving past the limit yields 429 with `Retry-After` equal to the typed error's seconds; under-limit yields 200; `errors.As` extracts the quota error's `Limit`/`Remaining` for logging.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/10-error-handling/04-custom-error-types/07-ratelimit-quota-error/cmd/demo
cd go-solutions/10-error-handling/04-custom-error-types/07-ratelimit-quota-error
go mod edit -go=1.24
```

### The error as a cross-layer signal

The limiter and the handler are different layers with different vocabularies. The
limiter knows *quota* facts: the configured `Limit`, how many calls `Remaining` in
the window, and how long until the window resets (`RetryAfter`). The handler knows
*HTTP* facts: status codes and headers. `QuotaExceededError` is the structured
value that carries the limiter's facts up to the handler so the handler can render
them faithfully — a `429` with a `Retry-After` header set to exactly the window's
remaining seconds, plus a body the client can act on.

Contrast the string alternative: if the limiter returned `errors.New("rate limit
exceeded, try again in 30s")`, the handler would have to parse "30s" back out of
prose to set the header. The typed error hands the handler `RetryAfter` as a
`time.Duration`, and the handler formats it once with
`strconv.Itoa(int(e.RetryAfter.Seconds()))`. The data flows as data, not as text
to be re-parsed.

### Rendering Retry-After correctly

The `Retry-After` HTTP header takes either an HTTP-date or a number of *seconds*
(RFC 9110). The delta-seconds form is the right choice for a rate limiter: the
handler writes `w.Header().Set("Retry-After", strconv.Itoa(seconds))` before
`WriteHeader(429)`. Headers must be set before the status is written — set them
after and they are silently dropped, a classic handler bug the test guards
against by asserting the header value on the recorded response.

Create `quotaerr.go`:

```go
// Package quotaerr shows a QuotaExceededError carrying quota facts (Limit,
// Remaining, RetryAfter) from a rate limiter up to an HTTP handler that renders
// them as a 429 with a Retry-After header.
package quotaerr

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// QuotaExceededError is the structured signal a limiter returns when a caller is
// over budget. It carries everything the handler needs to build a 429.
type QuotaExceededError struct {
	Limit      int
	Remaining  int
	RetryAfter time.Duration
}

func (e *QuotaExceededError) Error() string {
	return fmt.Sprintf("quota exceeded: limit=%d remaining=%d retry_after=%s",
		e.Limit, e.Remaining, e.RetryAfter)
}

// Limiter is a minimal fixed-count gate: it allows Limit calls, then returns a
// *QuotaExceededError. A real limiter would use a token bucket over a time
// window; this keeps the focus on the error as a signal.
type Limiter struct {
	mu         sync.Mutex
	limit      int
	used       int
	retryAfter time.Duration
}

func NewLimiter(limit int, retryAfter time.Duration) *Limiter {
	return &Limiter{limit: limit, retryAfter: retryAfter}
}

// Allow consumes one unit of quota. It returns nil while under the limit and a
// *QuotaExceededError once the budget is spent.
func (l *Limiter) Allow() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.used >= l.limit {
		return &QuotaExceededError{Limit: l.limit, Remaining: 0, RetryAfter: l.retryAfter}
	}
	l.used++
	return nil
}

// Handler gates each request through the limiter and renders a quota failure as a
// 429 with a Retry-After header.
type Handler struct {
	limiter *Limiter
}

func NewHandler(l *Limiter) *Handler { return &Handler{limiter: l} }

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := h.limiter.Allow(); err != nil {
		var qe *QuotaExceededError
		if errors.As(err, &qe) {
			// Retry-After in delta-seconds; headers before WriteHeader.
			secs := int(qe.RetryAfter.Seconds())
			w.Header().Set("Retry-After", strconv.Itoa(secs))
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprintf(w, "rate limited; retry after %d seconds\n", secs)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "ok")
}
```

### The runnable demo

The demo builds a limiter of 2, sends three requests through the handler, and
prints each status and any `Retry-After` header so you can see the third request
get a 429.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"time"

	"example.com/quotaerr"
)

func main() {
	h := quotaerr.NewHandler(quotaerr.NewLimiter(2, 30*time.Second))

	for i := 1; i <= 3; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		fmt.Printf("request %d: status=%d retry-after=%q\n",
			i, rec.Code, rec.Header().Get("Retry-After"))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
request 1: status=200 retry-after=""
request 2: status=200 retry-after=""
request 3: status=429 retry-after="30"
```

### Tests

`TestUnderLimit` asserts allowed requests return 200. `TestOverLimit429` drives past
the budget and asserts the 429 and the `Retry-After` header equals the typed
error's seconds. `TestQuotaErrorFieldsForLogging` proves `errors.As` extracts the
quota error and its `Limit`/`Remaining` fields — the data a logging middleware
would record.

Create `quotaerr_test.go`:

```go
package quotaerr

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestUnderLimit(t *testing.T) {
	t.Parallel()
	h := NewHandler(NewLimiter(3, 30*time.Second))

	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d; want 200", i, rec.Code)
		}
	}
}

func TestOverLimit429(t *testing.T) {
	t.Parallel()
	h := NewHandler(NewLimiter(1, 45*time.Second))

	// Spend the single unit of quota.
	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec1.Code != http.StatusOK {
		t.Fatalf("first request status = %d; want 200", rec1.Code)
	}

	// Next request is over budget.
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("over-limit status = %d; want 429", rec2.Code)
	}
	if got := rec2.Header().Get("Retry-After"); got != "45" {
		t.Errorf("Retry-After = %q; want 45", got)
	}
}

func TestQuotaErrorFieldsForLogging(t *testing.T) {
	t.Parallel()
	l := NewLimiter(1, 10*time.Second)

	if err := l.Allow(); err != nil {
		t.Fatalf("first Allow = %v; want nil", err)
	}
	err := l.Allow() // over budget

	var qe *QuotaExceededError
	if !errors.As(err, &qe) {
		t.Fatalf("errors.As should extract *QuotaExceededError from %v", err)
	}
	if qe.Limit != 1 {
		t.Errorf("Limit = %d; want 1", qe.Limit)
	}
	if qe.Remaining != 0 {
		t.Errorf("Remaining = %d; want 0", qe.Remaining)
	}
	if qe.RetryAfter != 10*time.Second {
		t.Errorf("RetryAfter = %s; want 10s", qe.RetryAfter)
	}
}
```

## Review

The type carries the limiter's facts across the layer boundary intact: the handler
reads `RetryAfter` as a `time.Duration` and renders it as delta-seconds in the
header without re-parsing any prose, and `TestOverLimit429` proves the header value
matches the typed field. Setting the header before `WriteHeader` is the detail that
makes it actually reach the client. `TestQuotaErrorFieldsForLogging` shows the same
error also serving observability: `errors.As` pulls `Limit`/`Remaining` for a log
line. The limiter here is a deliberate minimal fixed-count gate so the lesson stays
on the error-as-signal; a production token-bucket limiter carries the same error
type. Run `go test -race` to confirm the gate is safe under concurrent `Allow`.

## Resources

- [RFC 9110: Retry-After](https://www.rfc-editor.org/rfc/rfc9110#field.retry-after) — the header's delta-seconds and HTTP-date forms.
- [net/http: StatusTooManyRequests](https://pkg.go.dev/net/http#StatusTooManyRequests) — the 429 constant and header API.
- [errors: As](https://pkg.go.dev/errors#As) — extracting the quota error to render and log its fields.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [06-retryable-error-classification.md](06-retryable-error-classification.md) | Next: [08-error-slog-logvaluer.md](08-error-slog-logvaluer.md)
