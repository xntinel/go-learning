# Exercise 5: Design an error contract callers can branch on

One sentinel is enough for a toy. Real operational code needs a contract: expected
conditions callers detect by identity, structured data callers read out of the
error, cause chains preserved through wrapping, and independent failures from a
fan-out aggregated into one value. This module builds that contract around a batch
health check.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
errcontract/                independent module: example.com/errcontract
  go.mod                    go 1.26
  errcontract.go            sentinels, StatusError (Error+Unwrap), Classify, BatchCheck
  cmd/demo/main.go          runnable demo of Is / As / Join branching
  errcontract_test.go       Is through two %w layers, As populates *StatusError, Join
```

Files: `errcontract.go`, `cmd/demo/main.go`, `errcontract_test.go`.
Implement: package sentinels (`ErrClientError`, `ErrServerError`), a typed
`StatusError{URL, Code, Err}` with `Error()` and `Unwrap()`, `Classify(url, code)`
that returns a wrapped `StatusError` for `4xx`/`5xx`, and `BatchCheck` that
aggregates per-URL failures with `errors.Join`.
Test: `errors.Is` finds a sentinel through two `%w` layers, `errors.As` populates
a `*StatusError` and exposes `Code`, and `errors.Join` of N failures reports each
via `errors.Is` — asserting identity, never `Error()` text.
Verify: `go test -count=1 ./...`, `go vet ./...` (which catches malformed
`Errorf` verbs), `gofmt -l` empty.

Set up the module:

```bash
mkdir -p ~/go-exercises/errcontract/cmd/demo
cd ~/go-exercises/errcontract
go mod init example.com/errcontract
```

### Four tools, four jobs

The `errors` package gives four distinct capabilities, and a good contract uses
each for what it is:

- **`errors.Is`** answers "is this error, anywhere in its chain, *that* specific
  condition?" You expose package sentinels (`var ErrServerError = errors.New(...)`)
  for the conditions callers branch on, and they match with `errors.Is`. Identity
  survives wrapping, so a sentinel buried under two `%w` layers is still found.
- **`errors.As`** answers "is there, anywhere in the chain, an error of *this
  type*, and if so give it to me." You expose a typed error (`StatusError`) when
  the caller needs data — here, the numeric status code — and they retrieve it
  with `errors.As(err, &target)`.
- **`%w` wrapping** (`fmt.Errorf("...: %w", cause)`) is what builds the chain both
  of the above walk. Each wrap prepends human context while preserving the cause
  for code.
- **`errors.Join`** aggregates *independent* failures. A batch of health checks
  produces one error per failed URL; `errors.Join(errs...)` bundles them into a
  single error whose tree `errors.Is` and `errors.As` still traverse, so a caller
  can ask "did any check hit a server error?" without unpacking a slice.

`StatusError` implements both `Error() string` (the human message) and
`Unwrap() error` (returning its embedded sentinel cause). Because `Classify`
builds a `StatusError` whose `Err` is `ErrServerError`, a caller can match the
*class* with `errors.Is(err, ErrServerError)` and read the *code* with
`errors.As` — from the same error value.

Create `errcontract.go`:

```go
package errcontract

import (
	"errors"
	"fmt"
)

// Sentinels for the classes a caller branches on.
var (
	ErrClientError = errors.New("client error status")
	ErrServerError = errors.New("server error status")
)

// StatusError carries the structured data a caller reads with errors.As, and
// wraps a class sentinel so the same value also answers errors.Is.
type StatusError struct {
	URL  string
	Code int
	Err  error
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s: unhealthy status %d", e.URL, e.Code)
}

// Unwrap exposes the class sentinel so errors.Is can match it.
func (e *StatusError) Unwrap() error { return e.Err }

// Classify maps a status code to an error, or nil for a healthy response. A 4xx
// carries ErrClientError, a 5xx carries ErrServerError.
func Classify(url string, code int) error {
	switch {
	case code >= 500:
		return &StatusError{URL: url, Code: code, Err: ErrServerError}
	case code >= 400:
		return &StatusError{URL: url, Code: code, Err: ErrClientError}
	default:
		return nil
	}
}

// BatchCheck classifies each url/code pair and aggregates the failures into a
// single error with errors.Join. It returns nil when every check is healthy.
func BatchCheck(codes map[string]int) error {
	var errs []error
	for url, code := range codes {
		if err := Classify(url, code); err != nil {
			errs = append(errs, fmt.Errorf("check %s: %w", url, err))
		}
	}
	return errors.Join(errs...)
}
```

### The runnable demo

The demo shows the three branching styles. It avoids printing any joined error
*string* (map iteration order is unspecified, so the aggregated text is not
stable) and instead prints identity checks, which are order-independent.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/errcontract"
)

func main() {
	err := errcontract.Classify("http://api.internal", 503)

	// Identity: which class?
	fmt.Println("is server error:", errors.Is(err, errcontract.ErrServerError))
	fmt.Println("is client error:", errors.Is(err, errcontract.ErrClientError))

	// Structured data: read the code out of the typed error.
	var se *errcontract.StatusError
	if errors.As(err, &se) {
		fmt.Println("code:", se.Code)
	}

	// Aggregate: one error covering a whole batch.
	batch := errcontract.BatchCheck(map[string]int{
		"http://a": 200,
		"http://b": 404,
		"http://c": 500,
	})
	fmt.Println("batch has a client error:", errors.Is(batch, errcontract.ErrClientError))
	fmt.Println("batch has a server error:", errors.Is(batch, errcontract.ErrServerError))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
is server error: true
is client error: false
code: 503
batch has a client error: true
batch has a server error: true
```

### Tests

The tests assert identity and structure only — never `Error()` text, which is for
humans and free to change. `TestIsThroughTwoWrapLayers` proves a sentinel is found
under two `%w` layers; `TestAsPopulatesStatusError` proves `errors.As` fills a
`*StatusError` and exposes `Code`; `TestJoinReportsEach` proves every failure in a
joined error is individually reachable.

Create `errcontract_test.go`:

```go
package errcontract

import (
	"errors"
	"fmt"
	"testing"
)

func TestClassify(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		code    int
		wantErr error // sentinel to match, or nil for healthy
	}{
		{name: "ok", code: 200, wantErr: nil},
		{name: "not found", code: 404, wantErr: ErrClientError},
		{name: "server error", code: 500, wantErr: ErrServerError},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := Classify("http://x", tc.code)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("Classify(%d) = %v, want nil", tc.code, err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Classify(%d) = %v, want errors.Is %v", tc.code, err, tc.wantErr)
			}
		})
	}
}

func TestIsThroughTwoWrapLayers(t *testing.T) {
	t.Parallel()
	base := Classify("http://x", 502) // wraps ErrServerError
	twice := fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", base))
	if !errors.Is(twice, ErrServerError) {
		t.Fatal("errors.Is did not find ErrServerError through two wrap layers")
	}
}

func TestAsPopulatesStatusError(t *testing.T) {
	t.Parallel()
	wrapped := fmt.Errorf("context: %w", Classify("http://x", 418))
	var se *StatusError
	if !errors.As(wrapped, &se) {
		t.Fatal("errors.As did not find *StatusError")
	}
	if se.Code != 418 {
		t.Fatalf("StatusError.Code = %d, want 418", se.Code)
	}
}

func TestJoinReportsEach(t *testing.T) {
	t.Parallel()
	err := BatchCheck(map[string]int{
		"http://a": 200, // healthy, contributes nothing
		"http://b": 404, // client
		"http://c": 500, // server
	})
	if !errors.Is(err, ErrClientError) {
		t.Fatal("joined error missing ErrClientError")
	}
	if !errors.Is(err, ErrServerError) {
		t.Fatal("joined error missing ErrServerError")
	}
}

func TestBatchAllHealthyIsNil(t *testing.T) {
	t.Parallel()
	if err := BatchCheck(map[string]int{"http://a": 200, "http://b": 204}); err != nil {
		t.Fatalf("BatchCheck(all healthy) = %v, want nil", err)
	}
}

func ExampleClassify() {
	err := Classify("http://x", 500)
	fmt.Println(errors.Is(err, ErrServerError))
	// Output: true
}
```

## Review

The contract is correct when identity and structure are both retrievable from one
value: `errors.Is(err, ErrServerError)` matches the class, `errors.As(err, &se)`
reads the code, and both keep working when the error is wrapped or joined. That
dual access is exactly why `StatusError` implements `Unwrap` — the embedded
sentinel is what `errors.Is` walks to. `BatchCheck` returning `nil` when every
check is healthy is part of the contract too: `errors.Join` of zero errors is
`nil`, so a caller's `if err != nil` means "something failed" with no special
case.

The trap this whole design exists to prevent is string matching. Never write
`if err.Error() == "..."`; the wording is not API and wrapping breaks it
instantly. Assert `errors.Is` for conditions and `errors.As` for data. `go vet`
will catch a malformed `%w` verb (for instance `%w` on a non-error, or a stray
`%v` where you meant `%w` that would silently break the chain) — keep it in the
gate.

## Resources

- [errors package](https://pkg.go.dev/errors) — `Is`, `As`, `Join`, `Unwrap`.
- [fmt.Errorf and %w](https://pkg.go.dev/fmt#Errorf) — wrapping a cause into the chain.
- [Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — the wrapping model and `Is`/`As`.
- [errors.Join proposal notes](https://pkg.go.dev/errors#Join) — aggregating independent failures.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-build-info-version-stamping.md](04-build-info-version-stamping.md) | Next: [06-race-detector-and-test-cache.md](06-race-detector-and-test-cache.md)
