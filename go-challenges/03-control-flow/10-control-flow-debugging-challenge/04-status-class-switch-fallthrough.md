# Exercise 4: The HTTP Status Classifier That Misrouted 4xx as 5xx

A `classify(code)` function buckets HTTP responses into Success / ClientError /
ServerError / Retryable, and a retry-and-alerting layer keys off it. It shipped
with a stray `fallthrough` that leaks a 4xx code into the 5xx arm, and a missing
default that mislabels out-of-range codes. You will read the mismatched test rows,
fix the switch arms, and pin the classification contract.

## What you'll build

```text
httpclass/                 module example.com/httpclass
  go.mod
  httpclass.go             Class type + String; Classify(code) using a tagless switch
  cmd/demo/
    main.go                runnable demo: classify a handful of representative codes
  httpclass_test.go        boundary table (199..600 and out-of-range) pinning each class
```

- Files: `httpclass.go`, `cmd/demo/main.go`, `httpclass_test.go`.
- Implement: `Classify(code int) Class` with a tagless `switch` whose specific cases (429 retryable; 502/503/504 retryable) precede the broad ranges, and a `default` returning `ClassUnknown` for out-of-range codes.
- Test: a boundary table across `199 200 299 399 400 418 429 499 500 502 503 504 599` and out-of-range codes, asserting the exact class.
- Verify: `go test -count=1 -race ./...`.

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/10-control-flow-debugging-challenge/04-status-class-switch-fallthrough/cmd/demo
cd go-solutions/03-control-flow/10-control-flow-debugging-challenge/04-status-class-switch-fallthrough
```

### The artifact and the planted bug

The classifier turns a status code into a bucket the retry layer understands: a
429 or a 502/503/504 is worth retrying, a 500 is not, a 4xx is the caller's fault,
a 2xx/3xx is fine. The version that shipped folded the retryable-429 check into
the 4xx arm with a `fallthrough`, and left off the `default`:

```go
func Classify(code int) Class {
	switch {
	case code >= 200 && code < 400:
		return ClassSuccess
	case code >= 400 && code < 500:
		if code == http.StatusTooManyRequests { // 429
			fallthrough // BUG: enters the 5xx arm's body unconditionally
		}
		return ClassClientError
	case code >= 500 && code < 600:
		return ClassServerError
	}
	return ClassClientError // BUG: no default; out-of-range codes fall to this wrong bucket
}
```

`fallthrough` in an expression switch transfers to the *next case's body*
unconditionally — it does not re-check that case's condition. So a 429 (a 4xx)
enters the `500..599` arm and is classified `ClassServerError`. The retry layer
treats it as a non-retryable server fault and stops backing off a rate-limited
client that should slow down. Meanwhile the trailing `return ClassClientError`
mislabels a bogus 700 as a client error instead of flagging it as unknown.

The failing rows read:

```text
--- FAIL: TestClassify (0.00s)
    httpclass_test.go:47: Classify(429) = ServerError, want Retryable
    httpclass_test.go:47: Classify(600) = ClientError, want Unknown
```

The fix orders the arms so the *specific* retryable codes are matched before the
broad ranges, and adds an explicit `default` returning `ClassUnknown`.

Create `httpclass.go`:

```go
package httpclass

import "net/http"

// Class is the bucket a retry/alerting layer routes a status code into.
type Class int

const (
	ClassUnknown     Class = iota // out-of-range or unrecognized code
	ClassSuccess                  // 2xx and 3xx
	ClassClientError              // 4xx that the caller must fix
	ClassServerError              // 5xx that is not worth retrying
	ClassRetryable                // transient: 429, 502, 503, 504
)

func (c Class) String() string {
	switch c {
	case ClassSuccess:
		return "Success"
	case ClassClientError:
		return "ClientError"
	case ClassServerError:
		return "ServerError"
	case ClassRetryable:
		return "Retryable"
	default:
		return "Unknown"
	}
}

// Classify buckets an HTTP status code. Specific retryable codes are matched
// before the broad 4xx/5xx ranges, and any code outside 200..599 is Unknown.
func Classify(code int) Class {
	switch {
	case code >= 200 && code < 400:
		return ClassSuccess
	case code == http.StatusTooManyRequests: // 429
		return ClassRetryable
	case code >= 400 && code < 500:
		return ClassClientError
	case code == http.StatusBadGateway, // 502, 503, 504
		code == http.StatusServiceUnavailable,
		code == http.StatusGatewayTimeout:
		return ClassRetryable
	case code >= 500 && code < 600:
		return ClassServerError
	default:
		return ClassUnknown
	}
}
```

Arm order is the whole design here. Because a tagless `switch` takes the *first*
matching case, the `429` and `502/503/504` cases must appear before the `400..499`
and `500..599` ranges that would otherwise swallow them. The explicit `default`
makes the out-of-range policy visible rather than leaving it to a zero value.

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/httpclass"
)

func main() {
	for _, code := range []int{200, 404, 429, 503, 500, 700} {
		fmt.Printf("%d -> %s\n", code, httpclass.Classify(code))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
200 -> Success
404 -> ClientError
429 -> Retryable
503 -> Retryable
500 -> ServerError
700 -> Unknown
```

### Tests

`TestClassify` walks the boundaries where a switch bug hides: the edges of each
range (`199/200/299/399/400/499/500/599`), the specific retryable codes
(`429/502/503/504`), the teapot `418` (a plain client error), and out-of-range
values (`600/0/-1`) that only pass with the `default` arm. The `429` row fails the
moment the stray `fallthrough` is present; the `600` row fails without the
`default`.

Create `httpclass_test.go`:

```go
package httpclass

import (
	"fmt"
	"testing"
)

func TestClassify(t *testing.T) {
	t.Parallel()

	tests := []struct {
		code int
		want Class
	}{
		{199, ClassUnknown},
		{200, ClassSuccess},
		{299, ClassSuccess},
		{399, ClassSuccess},
		{400, ClassClientError},
		{418, ClassClientError},
		{429, ClassRetryable}, // fails if a stray fallthrough leaks it into 5xx
		{499, ClassClientError},
		{500, ClassServerError},
		{502, ClassRetryable},
		{503, ClassRetryable},
		{504, ClassRetryable},
		{599, ClassServerError},
		{600, ClassUnknown}, // proves the default arm
		{0, ClassUnknown},
		{-1, ClassUnknown},
	}
	for _, tc := range tests {
		if got := Classify(tc.code); got != tc.want {
			t.Errorf("Classify(%d) = %s, want %s", tc.code, got, tc.want)
		}
	}
}

func ExampleClassify() {
	fmt.Println(Classify(200), Classify(429), Classify(500), Classify(700))
	// Output: Success Retryable ServerError Unknown
}
```

## Review

The classifier is correct when every boundary code lands in the intended bucket
and no case leaks into another. Two properties do the work: a Go `switch` breaks
implicitly, so no arm falls into the next unless you write `fallthrough`
deliberately; and a tagless switch takes the first matching case, so specific
retryable codes must precede the broad ranges. The `default` is not a formality —
it is the policy for codes outside the protocol's range, and without it those
codes silently take the zero `Class`. Test the boundaries, not the middles: a
range bug shows up at `399/400` and `499/500`, never at `250`.

## Resources

- [Go Specification: Switch statements](https://go.dev/ref/spec#Switch_statements) — expression switches, implicit break, and `fallthrough`.
- [net/http status constants](https://pkg.go.dev/net/http#pkg-constants) — `StatusTooManyRequests`, `StatusBadGateway`, `StatusServiceUnavailable`, `StatusGatewayTimeout`.
- [Effective Go: Switch](https://go.dev/doc/effective_go#switch) — idiomatic tagless switches and case lists.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [03-defer-close-in-loop-fd-leak.md](03-defer-close-in-loop-fd-leak.md) | Next: [05-range-value-copy-mutation.md](05-range-value-copy-mutation.md)
