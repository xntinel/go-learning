# Exercise 9: Classify HTTP Responses for Retry Using Typed Status-Class Constants

An HTTP client's retry logic hinges on reading a status code correctly, and
scattering `503` and `429` through that code is how typos become outages. This
module builds a retry classifier: a typed `StatusClass` enum derived from a
status code, plus a retryable-status set that references `net/http` named
constants instead of magic numbers.

This module is fully self-contained: its own module, its own demo, its own tests.

## What you'll build

```text
retryclass/                     module: example.com/retryclass
  go.mod                        go 1.26
  retryclass.go                 StatusClass enum, Classify(code), Retryable(code)
  cmd/
    demo/
      main.go                   classifies representative codes and prints retryability
  retryclass_test.go            class boundaries, retryable set, http constant values
```

Files: `retryclass.go`, `cmd/demo/main.go`, `retryclass_test.go`.
Implement: a `StatusClass` enum, `Classify(code int) StatusClass`, and `Retryable(code int) bool` keyed on `net/http` constants.
Test: `Classify` maps `2xx`/`3xx`/`4xx`/`5xx` to the right class across boundaries; `Retryable` is true only for the designated statuses; the classifier's constants match `net/http`.
Verify: `go test -count=1 ./...`

## Why an enum plus named constants

Two modeling decisions carry this module.

First, the *status class* is a derived category, and a derived category deserves
its own typed enum. Instead of passing raw `int` codes around and re-deriving
"is this a server error?" at every call site, `Classify` maps a code to a
`StatusClass` once. Integer division by 100 does the mapping cleanly: `code / 100`
is `2` for any `2xx`, `5` for any `5xx`, and so on. A `switch` on that quotient
is exhaustive and readable, and `ClassUnknown` at `iota == 0` catches anything
outside `100..599` — a code like `0` or `700` is `Unknown`, not silently
mis-bucketed.

Second, the *retryable set* must reference `net/http` named constants, not
literals. `http.StatusTooManyRequests` reads as intent and cannot be typo'd into
`530`; `503` in the source can. Retryability is not the same as "is a server
error" — a `500 Internal Server Error` usually should *not* be blindly retried
(the request may have had side effects), while `429 Too Many Requests`, `502 Bad
Gateway`, `503 Service Unavailable`, and `504 Gateway Timeout` are the classic
transient failures that a backoff-and-retry loop should handle. Encoding that set
explicitly as a map keyed on named constants makes the policy auditable: the
retryable statuses are listed in one place, by name.

The values behind the constants are real and fixed by the HTTP spec:
`http.StatusTooManyRequests == 429`, `http.StatusBadGateway == 502`,
`http.StatusServiceUnavailable == 503`, `http.StatusGatewayTimeout == 504`. The
test asserts the classifier agrees with those, so the mapping cannot drift from
the stdlib.

Create `retryclass.go`:

```go
package retryclass

import "net/http"

// StatusClass is the coarse category of an HTTP status code. ClassUnknown at
// iota==0 catches codes outside the standard 1xx-5xx ranges.
type StatusClass uint8

const (
	ClassUnknown StatusClass = iota
	ClassInformational
	ClassSuccess
	ClassRedirect
	ClassClientError
	ClassServerError
)

func (c StatusClass) String() string {
	switch c {
	case ClassInformational:
		return "informational"
	case ClassSuccess:
		return "success"
	case ClassRedirect:
		return "redirect"
	case ClassClientError:
		return "client_error"
	case ClassServerError:
		return "server_error"
	default:
		return "unknown"
	}
}

// Classify buckets a status code into its class by integer division.
func Classify(code int) StatusClass {
	switch code / 100 {
	case 1:
		return ClassInformational
	case 2:
		return ClassSuccess
	case 3:
		return ClassRedirect
	case 4:
		return ClassClientError
	case 5:
		return ClassServerError
	default:
		return ClassUnknown
	}
}

// retryableStatuses lists the transient failures worth retrying, by name.
var retryableStatuses = map[int]bool{
	http.StatusTooManyRequests:    true,
	http.StatusBadGateway:         true,
	http.StatusServiceUnavailable: true,
	http.StatusGatewayTimeout:     true,
}

// Retryable reports whether a request that got this status should be retried.
func Retryable(code int) bool {
	return retryableStatuses[code]
}
```

## The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http"

	"example.com/retryclass"
)

func main() {
	codes := []int{
		http.StatusOK,
		http.StatusMovedPermanently,
		http.StatusNotFound,
		http.StatusTooManyRequests,
		http.StatusServiceUnavailable,
		http.StatusInternalServerError,
	}
	for _, c := range codes {
		fmt.Printf("%d %s retryable=%v\n", c, retryclass.Classify(c), retryclass.Retryable(c))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
200 success retryable=false
301 redirect retryable=false
404 client_error retryable=false
429 client_error retryable=true
503 server_error retryable=true
500 server_error retryable=false
```

## Tests

`TestClassify` walks the class boundaries — `199`/`200`/`299`/`300`/`400`/`500`
and out-of-range codes — so an off-by-one in the division mapping is caught.
`TestRetryable` asserts the retryable set exactly, including that `500` and `404`
are *not* retryable. `TestUsesHTTPConstants` ties the retryable codes to the
`net/http` values so the policy cannot silently drift.

Create `retryclass_test.go`:

```go
package retryclass

import (
	"fmt"
	"net/http"
	"testing"
)

func TestClassify(t *testing.T) {
	t.Parallel()

	tests := []struct {
		code int
		want StatusClass
	}{
		{100, ClassInformational},
		{199, ClassInformational},
		{200, ClassSuccess},
		{299, ClassSuccess},
		{300, ClassRedirect},
		{404, ClassClientError},
		{500, ClassServerError},
		{599, ClassServerError},
		{0, ClassUnknown},
		{700, ClassUnknown},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprint(tt.code), func(t *testing.T) {
			t.Parallel()
			if got := Classify(tt.code); got != tt.want {
				t.Fatalf("Classify(%d) = %s, want %s", tt.code, got, tt.want)
			}
		})
	}
}

func TestRetryable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		code int
		want bool
	}{
		{http.StatusTooManyRequests, true},
		{http.StatusBadGateway, true},
		{http.StatusServiceUnavailable, true},
		{http.StatusGatewayTimeout, true},
		{http.StatusOK, false},
		{http.StatusBadRequest, false},
		{http.StatusNotFound, false},
		{http.StatusInternalServerError, false},
		{http.StatusNotImplemented, false},
	}
	for _, tt := range tests {
		if got := Retryable(tt.code); got != tt.want {
			t.Fatalf("Retryable(%d) = %v, want %v", tt.code, got, tt.want)
		}
	}
}

func TestUsesHTTPConstants(t *testing.T) {
	t.Parallel()

	// The classifier must agree with the stdlib's canonical values.
	if http.StatusServiceUnavailable != 503 || http.StatusGatewayTimeout != 504 {
		t.Fatal("net/http status constants unexpected")
	}
	if !Retryable(http.StatusServiceUnavailable) {
		t.Fatal("503 must be retryable")
	}
	if Classify(http.StatusServiceUnavailable) != ClassServerError {
		t.Fatal("503 must classify as server_error")
	}
}

func ExampleClassify() {
	fmt.Println(Classify(503), Retryable(503))
	// Output: server_error true
}
```

## Review

The classifier is correct when every code maps to the right class at the
boundaries and the retryable set is exactly the four transient statuses — with
`500` deliberately excluded, because a blind retry of a non-idempotent request
that already had effects is its own bug. Referencing `net/http` constants keeps
the policy readable and typo-proof, and `TestUsesHTTPConstants` locks the
mapping to the stdlib. Model the class as its own enum rather than re-deriving
`code / 100` at every call site.

## Resources

- [net/http: Status constants](https://pkg.go.dev/net/http#pkg-constants)
- [Go Specification: Iota](https://go.dev/ref/spec#Iota)
- [MDN: HTTP response status codes](https://developer.mozilla.org/en-US/docs/Web/HTTP/Status)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [08-enum-exhaustiveness-guard.md](08-enum-exhaustiveness-guard.md) | Next: [10-api-error-code-registry.md](10-api-error-code-registry.md)
