# Exercise 6: Implement a Custom As(any) bool To Synthesize a Typed View

The original version of this lesson named the `As(target any) bool` method but
never demonstrated it. This exercise closes that gap: a `*DriverError` implements
`As` so that, when a caller asks for a `**APIError`, the method *builds* a fresh
public-facing `*APIError` — mapping the driver's raw code to a stable public code,
a safe message, and a retryability verdict — and hands it back, without the caller
ever learning the driver type.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
asadapter/                      independent module: example.com/asadapter
  go.mod                        go 1.25
  adapter.go                    DriverError.As synthesizes an APIError; APIError type
  adapter_test.go               As populates APIError; unrelated target false; raw still reachable
  cmd/demo/main.go              runnable demo extracting the public APIError
```

Files: `adapter.go`, `adapter_test.go`, `cmd/demo/main.go`.
Implement: `*DriverError` with an `As(target any) bool` method that populates a `**APIError` with mapped fields, plus `Error`.
Test: `errors.As(err, &apiErr)` returns true with mapped `Code`/`Public`/`Retryable`; `errors.As` into an unrelated pointer type returns false; the raw `*DriverError` is still reachable via `errors.As` into `**DriverError`.
Verify: `go test -count=1 -race ./... && go vet ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/asadapter/cmd/demo
cd ~/go-exercises/asadapter
go mod init example.com/asadapter
go mod edit -go=1.25
```

### An error that synthesizes a different typed error

`errors.As` normally matches by assignability: it binds a tree element to your
target when the element's concrete type *is* the target type. But an error can
override that with an `As(target any) bool` method, and the override is far more
than a shortcut — it lets an error hand back a typed value it does not structurally
contain. The `*DriverError` here has fields `Code int` and `Detail string` (raw,
driver-shaped). It does not contain an `*APIError` anywhere. Yet when a caller
writes `errors.As(err, &apiErr)` with `apiErr` of type `*APIError`, the driver's
`As` method runs, sees the target is `**APIError`, *constructs* a new `*APIError`
by adapting its own raw fields, assigns it through the pointer, and returns true.

That adaptation is the whole point of a translation boundary expressed as a
method. The public `*APIError` carries a stable string `Code` (not the driver's
integer), a `Public` message safe to show a client, and a `Retryable` flag derived
from the driver code — none of which exist on `*DriverError`. The caller gets a
clean, transport-neutral typed error and never imports or names the driver type.
Because the synthesized fields (`Public`, `Retryable`, a string `Code`) are not
present on `DriverError`, the tests can prove it was the `As` method that produced
them and not some incidental assignability.

The method must be disciplined about its target. It type-asserts
`target.(**APIError)`; if that assertion fails — the caller asked for some
unrelated pointer type — it returns false so that `errors.As` keeps traversing and
does not wrongly claim a match. The raw driver error remains reachable purely by
ordinary assignability at the `fmt.Errorf("...: %w", driverErr)` wrapper node:
`errors.As` descends the wrapper's `Unwrap` and binds `*DriverError` by
assignability, so `errors.As(err, &drvErr)` with `drvErr` of type `*DriverError`
still returns the original — nothing is lost, and a caller who *does* want the raw
detail can get it. `DriverError` itself needs no `Unwrap` method because it wraps
no underlying cause; it is a leaf in the error tree.

Create `adapter.go`:

```go
package asadapter

import "fmt"

// APIError is the clean public-facing error. Its fields (string Code, Public,
// Retryable) do NOT exist on DriverError; the As method synthesizes them.
type APIError struct {
	Code      string
	Public    string
	Retryable bool
}

func (e *APIError) Error() string { return e.Code + ": " + e.Public }

// DriverError is the raw, driver-shaped error. It carries an integer status and
// an internal detail string.
type DriverError struct {
	Status int
	Detail string
}

func (e *DriverError) Error() string { return fmt.Sprintf("driver status %d: %s", e.Status, e.Detail) }

// As synthesizes an *APIError from the raw driver fields when the target is a
// **APIError, and returns false for any other target so traversal continues.
func (e *DriverError) As(target any) bool {
	p, ok := target.(**APIError)
	if !ok {
		return false
	}
	*p = &APIError{
		Code:      mapCode(e.Status),
		Public:    "the request could not be completed",
		Retryable: e.Status == 503 || e.Status == 429,
	}
	return true
}

func mapCode(status int) string {
	switch status {
	case 404:
		return "not_found"
	case 409:
		return "conflict"
	case 429, 503:
		return "unavailable"
	default:
		return "internal"
	}
}
```

### The runnable demo

The demo wraps a driver error, extracts the synthesized `*APIError`, and prints
the mapped public fields, then extracts the raw `*DriverError` to show it is still
reachable.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/asadapter"
)

func main() {
	err := fmt.Errorf("query users: %w", &asadapter.DriverError{Status: 503, Detail: "pool exhausted on db-3"})

	var apiErr *asadapter.APIError
	if errors.As(err, &apiErr) {
		fmt.Printf("public: code=%s retryable=%v msg=%q\n", apiErr.Code, apiErr.Retryable, apiErr.Public)
	}

	var drv *asadapter.DriverError
	if errors.As(err, &drv) {
		fmt.Printf("raw still reachable: status=%d detail=%q\n", drv.Status, drv.Detail)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
public: code=unavailable retryable=true msg="the request could not be completed"
raw still reachable: status=503 detail="pool exhausted on db-3"
```

### Tests

`TestAsSynthesizesAPIError` asserts the `As` method maps the driver status to the
public fields. `TestAsUnrelatedTargetFalse` asserts extracting into an unrelated
type returns false (the method did not falsely match). `TestRawStillReachable`
asserts the original `*DriverError` is still bindable, so nothing is lost.

Create `adapter_test.go`:

```go
package asadapter

import (
	"errors"
	"fmt"
	"testing"
)

// unrelated is an error type DriverError.As does not recognize.
type unrelated struct{}

func (unrelated) Error() string { return "unrelated" }

func TestAsSynthesizesAPIError(t *testing.T) {
	t.Parallel()
	err := fmt.Errorf("query: %w", &DriverError{Status: 503, Detail: "pool exhausted"})

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatal("errors.As into *APIError = false, want true")
	}
	if apiErr.Code != "unavailable" {
		t.Fatalf("Code = %q, want unavailable", apiErr.Code)
	}
	if !apiErr.Retryable {
		t.Fatal("Retryable = false, want true for status 503")
	}
	if apiErr.Public == "" {
		t.Fatal("Public message is empty; As did not synthesize it")
	}
}

func TestAsMapsNonRetryable(t *testing.T) {
	t.Parallel()
	err := &DriverError{Status: 404, Detail: "missing"}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatal("errors.As into *APIError = false, want true")
	}
	if apiErr.Code != "not_found" {
		t.Fatalf("Code = %q, want not_found", apiErr.Code)
	}
	if apiErr.Retryable {
		t.Fatal("Retryable = true, want false for status 404")
	}
}

func TestAsUnrelatedTargetFalse(t *testing.T) {
	t.Parallel()
	err := &DriverError{Status: 503, Detail: "x"}
	var u *unrelated
	if errors.As(err, &u) {
		t.Fatal("errors.As into *unrelated = true, want false")
	}
}

func TestRawStillReachable(t *testing.T) {
	t.Parallel()
	err := fmt.Errorf("query: %w", &DriverError{Status: 409, Detail: "dup key"})
	var drv *DriverError
	if !errors.As(err, &drv) {
		t.Fatal("raw *DriverError not reachable via errors.As")
	}
	if drv.Status != 409 || drv.Detail != "dup key" {
		t.Fatalf("raw fields = %d/%q, want 409/dup key", drv.Status, drv.Detail)
	}
}

func Example() {
	err := &DriverError{Status: 429, Detail: "rate limited"}
	var apiErr *APIError
	_ = errors.As(err, &apiErr)
	fmt.Println(apiErr.Code, apiErr.Retryable)
	// Output: unavailable true
}
```

## Review

The adapter is correct when `errors.As(err, &apiErr)` yields an `*APIError` whose
fields were *built* by the `As` method — a string `Code`, a safe `Public` message,
a `Retryable` verdict — none of which exist on `*DriverError`, which is the proof
the method did the work. The method's discipline is the load-bearing part: it
type-asserts `target.(**APIError)` and returns false for anything else, so
`errors.As` keeps traversing rather than falsely matching, and it leaves the raw
`*DriverError` reachable by ordinary assignability. The mistake to avoid is an
`As` method that panics on an unexpected target or that forgets to set the target
before returning true — both break the `errors.As` contract. Run `go test -race`.

## Resources

- [errors.As](https://pkg.go.dev/errors#As) — the custom `As(any) bool` method and target rules.
- [errors package](https://pkg.go.dev/errors) — how `As` traversal consults a custom method.
- [Go Blog: Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — the `Is`/`As` method hooks.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [05-retry-classification-as-interface.md](05-retry-classification-as-interface.md) | Next: [07-context-error-classification.md](07-context-error-classification.md)
