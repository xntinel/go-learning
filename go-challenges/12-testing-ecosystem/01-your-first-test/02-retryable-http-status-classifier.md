# Exercise 2: IsRetryable: A First Test For The Retry Layer

Every HTTP client with a retry middleware needs one function at its core: given a
response status code, should the request be retried? This is the canonical place
to learn `t.Errorf` versus `t.Fatalf`, because you want *every* misclassification
to surface in a single run.

## What you'll build

```text
retryclass/                independent module: example.com/retryclass
  go.mod
  retry.go                 func IsRetryable(status int) bool
  retry_test.go            TestIsRetryable (discrete t.Errorf per status), ExampleIsRetryable
  cmd/
    demo/
      main.go              prints the verdict for a spread of statuses
```

- Files: `retry.go`, `retry_test.go`, `cmd/demo/main.go`.
- Implement: `IsRetryable(status int) bool` — true for 429, 500, 502, 503, 504; false for everything else.
- Test: one `TestIsRetryable` with a handful of discrete `t.Errorf` assertions, one per representative status.
- Verify: `gofmt -l .`, `go vet ./...`, `go test -count=1 -race ./...`.

### Which statuses are retryable, and why the test is not a table

Retrying is only safe for transient, server-side failures where a second attempt
has a real chance of succeeding. `429 Too Many Requests` means "back off and try
again". `500`, `502`, `503`, `504` are server or gateway faults that are often
momentary. A `4xx` like `400`, `401`, `404`, or `422` is a client error — the
request is wrong, and retrying it unchanged will fail identically forever, so
retrying wastes budget and can amplify an outage. Any `2xx` or `3xx` succeeded or
redirected; there is nothing to retry. The subtle discriminator is `501 Not
Implemented`: it is a `5xx`, but the server is telling you the operation will
never work, so it must be classified non-retryable. A test that only checked "is
it `5xx`" would miss that.

The test is written as a sequence of individual `t.Errorf` calls, one per
representative status, on purpose. This is the teaching moment for `Errorf` versus
`Fatalf`. Because each status is an independent check, `t.Errorf` lets a single
run report *every* status the classifier gets wrong — if you broke the `429` case
and the `501` case at once, `Fatalf` would abort at the first and hide the second,
while `Errorf` shows both. The table-driven form that collapses these into a loop
is the subject of lesson 02; here the assertions are spelled out so the mechanics
stay visible. Failure messages use `http.StatusText` so CI output reads
`IsRetryable(429 Too Many Requests) = false, want true` rather than a bare number.

Create `retry.go`. It uses the `net/http` status constants rather than magic
numbers, so the intent is legible:

```go
package retryclass

import "net/http"

// IsRetryable reports whether an HTTP response with the given status code should
// be retried. Only transient server-side failures are retryable; client errors
// (4xx except 429) and successes are not.
func IsRetryable(status int) bool {
	switch status {
	case http.StatusTooManyRequests, // 429
		http.StatusInternalServerError, // 500
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503
		http.StatusGatewayTimeout:      // 504
		return true
	default:
		return false
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http"

	"example.com/retryclass"
)

func main() {
	for _, status := range []int{200, 400, 404, 429, 500, 501, 503} {
		fmt.Printf("%d %-21s retryable=%v\n",
			status, http.StatusText(status), retryclass.IsRetryable(status))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
200 OK                    retryable=false
400 Bad Request           retryable=false
404 Not Found             retryable=false
429 Too Many Requests     retryable=true
500 Internal Server Error retryable=true
501 Not Implemented       retryable=false
503 Service Unavailable   retryable=true
```

### The tests

Create `retry_test.go`:

```go
package retryclass

import (
	"fmt"
	"net/http"
	"testing"
)

func TestIsRetryable(t *testing.T) {
	t.Parallel()

	// Retryable: transient server-side failures. Each is a discrete t.Errorf so
	// a broken classifier reports every wrong status in one run.
	if !IsRetryable(http.StatusTooManyRequests) {
		t.Errorf("IsRetryable(%d %s) = false, want true",
			http.StatusTooManyRequests, http.StatusText(http.StatusTooManyRequests))
	}
	if !IsRetryable(http.StatusInternalServerError) {
		t.Errorf("IsRetryable(%d %s) = false, want true",
			http.StatusInternalServerError, http.StatusText(http.StatusInternalServerError))
	}
	if !IsRetryable(http.StatusServiceUnavailable) {
		t.Errorf("IsRetryable(%d %s) = false, want true",
			http.StatusServiceUnavailable, http.StatusText(http.StatusServiceUnavailable))
	}

	// Not retryable: successes and client errors, plus the 5xx that never works.
	if IsRetryable(http.StatusOK) {
		t.Errorf("IsRetryable(%d %s) = true, want false",
			http.StatusOK, http.StatusText(http.StatusOK))
	}
	if IsRetryable(http.StatusBadRequest) {
		t.Errorf("IsRetryable(%d %s) = true, want false",
			http.StatusBadRequest, http.StatusText(http.StatusBadRequest))
	}
	if IsRetryable(http.StatusNotFound) {
		t.Errorf("IsRetryable(%d %s) = true, want false",
			http.StatusNotFound, http.StatusText(http.StatusNotFound))
	}
	if IsRetryable(http.StatusNotImplemented) {
		t.Errorf("IsRetryable(%d %s) = true, want false",
			http.StatusNotImplemented, http.StatusText(http.StatusNotImplemented))
	}
}

func ExampleIsRetryable() {
	fmt.Println(IsRetryable(503))
	fmt.Println(IsRetryable(404))
	// Output:
	// true
	// false
}
```

## Review

The classifier is correct when exactly `{429, 500, 502, 503, 504}` map to true
and everything else to false — note `501` is the trap that separates a real
classifier from "is it `5xx`". The reason for `t.Errorf` over `t.Fatalf` here is
concrete: break two cases at once and re-run; `Errorf` reports both failing
statuses, which is what you want when triaging a regression, whereas `Fatalf`
would stop at the first. The messages carry `http.StatusText(status)` so the CI
line names the status in words. Gate with `gofmt -l .`, `go vet ./...`, and
`go test -count=1 -race ./...`.

## Resources

- [net/http status constants](https://pkg.go.dev/net/http#pkg-constants) — the named codes and `StatusText`.
- [testing package](https://pkg.go.dev/testing) — `Errorf` versus `Fatalf`.
- [RFC 9110: HTTP Semantics, Status Codes](https://www.rfc-editor.org/rfc/rfc9110#name-status-codes) — the authoritative meaning of each code.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-sum-library-first-test.md](01-sum-library-first-test.md) | Next: [03-parse-price-to-cents.md](03-parse-price-to-cents.md)
