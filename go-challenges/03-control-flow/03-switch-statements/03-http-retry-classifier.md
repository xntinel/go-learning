# Exercise 3: Decide Retryability From an HTTP Status With a Range Switch

An outbound HTTP client's retry/backoff layer lives or dies on one function: is
this response worth trying again? This module builds that function as a tagless
switch whose cases are status ranges and specific codes, encoding a real retry
policy — and it is the exercise where case *ordering* stops being a style note
and becomes a correctness requirement.

This module is fully self-contained: its own `go mod init`, code, demo, and
tests. It makes no network calls.

## What you'll build

```text
retry/                     independent module: example.com/http-retry-classifier
  go.mod                   go 1.24
  retry.go                 type RetryDecision; ClassifyResponse(status) RetryDecision
  cmd/
    demo/
      main.go              runnable demo over representative status codes
  retry_test.go            table over representative codes + boundary ordering test
```

- Files: `retry.go`, `cmd/demo/main.go`, `retry_test.go`.
- Implement: `ClassifyResponse(status int) RetryDecision` using a tagless switch with range predicates and named-constant cases.
- Test: a table over 200/201/301/400/401/404/409/429/500/502/503/504/100 asserting the exact decision, plus a 499-vs-500 boundary test proving the `>= 500` ordering.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why a tagless switch, and why order is load-bearing

Retryability is not a single-value comparison — it is a set of predicates over a
numeric range: `2xx` succeeds, most `5xx` are worth retrying, `4xx` are
permanent client errors, and a couple of specific codes (`429`, `503`) get
special treatment because they usually carry a `Retry-After` the caller should
honor. Ranges rule out an expression switch (`==` cannot say `>= 500`), so this
is a tagless switch.

Ordering is the crux. `429` is inside the `4xx` range and `503` is inside the
`5xx` range, so the specific cases must come *before* the broad range cases, or
the range case swallows them and their special reason never surfaces. Reverse
the order — put `case status >= 500` above `case status == http.StatusServiceUnavailable`
— and a 503 gets the generic "server error" reason instead of the "honor
Retry-After" one, so the backoff layer ignores the server's timing hint. The
comment on the ordering is not optional documentation; it is a guardrail against
a future edit that reorders the cases and quietly breaks the policy.

Everything outside `2xx`/`4xx`/`5xx` — a stray `1xx` or an unexpected `3xx`
reaching a layer that should have followed redirects already — is treated as
*unexpected* and not retried, because retrying an informational or redirect
status blindly is how a client wedges itself in a loop.

Create `retry.go`:

```go
package retry

import "net/http"

// RetryDecision tells the backoff layer whether to retry and why.
type RetryDecision struct {
	Retry  bool
	Reason string
}

// ClassifyResponse encodes the outbound-client retry policy as a tagless switch.
// The specific codes (429, 503) MUST precede the broad range cases (>= 500,
// >= 400) they fall inside, or their special "honor Retry-After" reason is
// swallowed by the range case.
func ClassifyResponse(status int) RetryDecision {
	switch {
	case status >= 200 && status < 300:
		return RetryDecision{Retry: false, Reason: "success"}
	case status == http.StatusTooManyRequests: // 429, inside 4xx: must precede >= 400
		return RetryDecision{Retry: true, Reason: "rate limited; honor Retry-After"}
	case status == http.StatusServiceUnavailable: // 503, inside 5xx: must precede >= 500
		return RetryDecision{Retry: true, Reason: "service unavailable; honor Retry-After"}
	case status >= 500 && status < 600:
		return RetryDecision{Retry: true, Reason: "server error"}
	case status >= 400 && status < 500:
		return RetryDecision{Retry: false, Reason: "client error; permanent"}
	default:
		return RetryDecision{Retry: false, Reason: "unexpected status"}
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/http-retry-classifier"
)

func main() {
	for _, status := range []int{200, 301, 404, 429, 500, 503} {
		d := retry.ClassifyResponse(status)
		fmt.Printf("%d retry=%-5v %s\n", status, d.Retry, d.Reason)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
200 retry=false success
301 retry=false unexpected status
404 retry=false client error; permanent
429 retry=true  rate limited; honor Retry-After
500 retry=true  server error
503 retry=true  service unavailable; honor Retry-After
```

### Tests

`TestClassifyResponse` pins the exact decision for a representative status from
each policy region. `TestBoundaryOrdering` is the ordering proof: 499 is a
permanent client error and 500 is a retryable server error, so the `>= 500`
predicate must sit above the `>= 400` one; if a future edit swaps them, 500
would match `>= 400` first and this test fails.

Create `retry_test.go`:

```go
package retry

import "testing"

func TestClassifyResponse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		status     int
		wantRetry  bool
		wantReason string
	}{
		{200, false, "success"},
		{201, false, "success"},
		{301, false, "unexpected status"},
		{400, false, "client error; permanent"},
		{401, false, "client error; permanent"},
		{404, false, "client error; permanent"},
		{409, false, "client error; permanent"},
		{429, true, "rate limited; honor Retry-After"},
		{500, true, "server error"},
		{502, true, "server error"},
		{503, true, "service unavailable; honor Retry-After"},
		{504, true, "server error"},
		{100, false, "unexpected status"},
	}

	for _, tc := range tests {
		got := ClassifyResponse(tc.status)
		if got.Retry != tc.wantRetry || got.Reason != tc.wantReason {
			t.Errorf("ClassifyResponse(%d) = {%v, %q}, want {%v, %q}",
				tc.status, got.Retry, got.Reason, tc.wantRetry, tc.wantReason)
		}
	}
}

func TestBoundaryOrdering(t *testing.T) {
	t.Parallel()

	if got := ClassifyResponse(499); got.Retry {
		t.Errorf("ClassifyResponse(499).Retry = true, want false (permanent client error)")
	}
	if got := ClassifyResponse(500); !got.Retry {
		t.Errorf("ClassifyResponse(500).Retry = false, want true (server error) — check >= 500 ordering")
	}
}
```

## Review

The classifier is correct when every status lands in exactly the policy region
the retry design intends, and the two tests together defend that. The table
covers one representative per region so a broad regression is caught; the
boundary test defends the specific ordering invariant (`429`/`503` before their
ranges, `>= 500` before `>= 400`) that a careless reorder would silently break.
The design point to carry forward: whenever a tagless switch has overlapping
predicates, the ordering is part of the specification and deserves both a comment
and a boundary test.

## Resources

- [Go Specification: Switch statements](https://go.dev/ref/spec#Switch_statements) — the tagless (expressionless) form and first-true matching.
- [net/http status constants](https://pkg.go.dev/net/http#pkg-constants) — `StatusTooManyRequests`, `StatusServiceUnavailable`, and the rest.
- [MDN: Retry-After](https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Retry-After) — why 429 and 503 get special retry treatment.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-kind-dispatcher.md](02-kind-dispatcher.md) | Next: [04-log-level-config-loader.md](04-log-level-config-loader.md)
