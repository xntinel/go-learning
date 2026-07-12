# Exercise 10: Extract A Specific Typed Error From An Aggregate (errors.As / AsType)

An HTTP handler that receives a joined domain error must map it to a status code:
a validation failure is 400, an overloaded dependency is 503, anything else is 500.
This exercise pulls a specific typed error out of an aggregate with `errors.As`,
shows the Go 1.26 `errors.AsType` form that returns the value directly, and pins
that both find only the *first* match — so handling every match needs a flatten.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. It uses `errors.AsType`, which requires Go 1.26.

## What you'll build

```text
apierr/                    independent module: example.com/apierr
  go.mod                   go 1.26
  apierr.go                FieldError, RetryableError; Classify, ClassifyAsType;
                           Flatten; Server (http.Handler)
  cmd/
    demo/
      main.go              classifies three aggregates, prints status codes
  apierr_test.go           httptest: 400/503/500; first-match; Flatten all
```

- Files: `apierr.go`, `cmd/demo/main.go`, `apierr_test.go`.
- Implement: typed `*FieldError` and `*RetryableError`; `Classify(err) (int, string)` using `errors.As`; `ClassifyAsType(err) (int, string)` using `errors.AsType[E]`; a `Flatten` for all-matches; a `Server` that writes the classified status.
- Test: an aggregate with a `*FieldError` yields 400; with a `*RetryableError` yields 503; with neither yields 500; `errors.As` returns the first `*FieldError` when several are present, and `Flatten` recovers all of them. Use `httptest`.
- Verify: `go test -count=1 -race ./...`

### From an aggregate to a status code

The domain layer returns a joined error — maybe a validation `Join` of `*FieldError`s,
maybe a single `*RetryableError` from an overloaded upstream, maybe an unexpected
`*fmt.wrapError` from a bug. The handler's job is to translate that into an HTTP
response. It does so by asking the aggregate what it *contains*: `errors.As(err, &fe)`
walks the tree and, if any member is a `*FieldError`, sets `fe` and returns true,
so the handler answers 400. If not, it tries `*RetryableError` for 503. If neither
matches, it falls through to 500 — the safe default for an error the handler does
not recognize.

`errors.As` is the classic form: declare `var fe *FieldError`, pass `&fe`, check the
bool. The pointer ceremony exists because `As` must be able to *set* the target, and
passing anything but a non-nil pointer to an error type panics. Go 1.26 adds the
generic `errors.AsType[E error](err error) (E, bool)`, which returns the matched
value directly: `fe, ok := errors.AsType[*FieldError](err)`. Same tree walk, same
first-match semantics, no address-of dance and no panic mode. `ClassifyAsType`
mirrors `Classify` with the newer form so you can compare them.

Both functions find only the **first** match in the tree. If an aggregate holds
three `*FieldError`s and the handler needs to report all three in the response body,
`As`/`AsType` alone is not enough — you `Flatten` the tree and range over the
members, type-asserting each. That is why this module ships a `Flatten` and a test
proving `errors.As` returns just the first while `Flatten` recovers all.

Create `apierr.go`:

```go
package apierr

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
)

// FieldError is a validation violation -> 400.
type FieldError struct {
	Field  string
	Reason string
}

func (e *FieldError) Error() string { return fmt.Sprintf("field %q: %s", e.Field, e.Reason) }

// RetryableError signals an overloaded dependency -> 503.
type RetryableError struct {
	AfterSeconds int
}

func (e *RetryableError) Error() string {
	return "temporarily unavailable, retry after " + strconv.Itoa(e.AfterSeconds) + "s"
}

// Classify maps a (possibly joined) error to an HTTP status and body using
// errors.As. It returns the first match found in the tree; unknown errors are 500.
func Classify(err error) (int, string) {
	var fe *FieldError
	if errors.As(err, &fe) {
		return http.StatusBadRequest, fe.Error()
	}
	var re *RetryableError
	if errors.As(err, &re) {
		return http.StatusServiceUnavailable, re.Error()
	}
	return http.StatusInternalServerError, "internal error"
}

// ClassifyAsType is Classify using the Go 1.26 errors.AsType form, which returns
// the matched value instead of taking a pointer argument.
func ClassifyAsType(err error) (int, string) {
	if fe, ok := errors.AsType[*FieldError](err); ok {
		return http.StatusBadRequest, fe.Error()
	}
	if re, ok := errors.AsType[*RetryableError](err); ok {
		return http.StatusServiceUnavailable, re.Error()
	}
	return http.StatusInternalServerError, "internal error"
}

// Flatten returns the leaf errors of a possibly-nested aggregate, so a handler can
// act on EVERY match, not just the first that errors.As returns.
func Flatten(err error) []error {
	if err == nil {
		return nil
	}
	if agg, ok := err.(interface{ Unwrap() []error }); ok {
		var out []error
		for _, child := range agg.Unwrap() {
			out = append(out, Flatten(child)...)
		}
		return out
	}
	return []error{err}
}

// Server maps the error from a domain operation to an HTTP response.
type Server struct {
	Process func(*http.Request) error
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	err := s.Process(r)
	if err == nil {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "ok")
		return
	}
	status, body := Classify(err)
	w.WriteHeader(status)
	io.WriteString(w, body)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/apierr"
)

func main() {
	cases := []struct {
		name string
		err  error
	}{
		{"validation", errors.Join(
			&apierr.FieldError{Field: "email", Reason: "must contain @"},
			errors.New("some log noise"),
		)},
		{"overloaded", errors.Join(&apierr.RetryableError{AfterSeconds: 5})},
		{"unexpected", errors.New("nil pointer deref")},
	}

	for _, c := range cases {
		status, body := apierr.Classify(c.err)
		fmt.Printf("%-11s -> %d %s\n", c.name, status, body)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
validation  -> 400 field "email": must contain @
overloaded  -> 503 temporarily unavailable, retry after 5s
unexpected  -> 500 internal error
```

### Tests

The httptest tests drive the `Server` with a `Process` that returns each aggregate
and assert the status code. `TestClassifyFirstMatch` puts two `*FieldError`s in the
aggregate and asserts `errors.As` returns the first while `Flatten` recovers both —
the motivation for flattening when every match matters. `TestClassifyAsType` runs
the same cases through the Go 1.26 form to prove it agrees with `errors.As`.

Create `apierr_test.go`:

```go
package apierr

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func serve(t *testing.T, err error) (int, string) {
	t.Helper()
	s := &Server{Process: func(*http.Request) error { return err }}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/users", nil)
	s.ServeHTTP(rec, req)
	body, _ := io.ReadAll(rec.Result().Body)
	return rec.Code, string(body)
}

func TestServeStatusCodes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		err    error
		status int
	}{
		{
			name:   "field error is 400",
			err:    errors.Join(&FieldError{Field: "email", Reason: "bad"}, errors.New("noise")),
			status: http.StatusBadRequest,
		},
		{
			name:   "retryable is 503",
			err:    errors.Join(&RetryableError{AfterSeconds: 3}),
			status: http.StatusServiceUnavailable,
		},
		{
			name:   "unknown is 500",
			err:    errors.New("boom"),
			status: http.StatusInternalServerError,
		},
		{
			name:   "nil is 200",
			err:    nil,
			status: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			code, _ := serve(t, tt.err)
			if code != tt.status {
				t.Errorf("status = %d, want %d", code, tt.status)
			}
		})
	}
}

func TestClassifyFirstMatch(t *testing.T) {
	t.Parallel()

	agg := errors.Join(
		&FieldError{Field: "email", Reason: "bad"},
		&FieldError{Field: "age", Reason: "too low"},
	)

	var fe *FieldError
	if !errors.As(agg, &fe) {
		t.Fatal("errors.As found no *FieldError")
	}
	if fe.Field != "email" {
		t.Errorf("errors.As first match = %q, want email", fe.Field)
	}

	// Flatten recovers every match, not just the first.
	var fields []string
	for _, leaf := range Flatten(agg) {
		var f *FieldError
		if errors.As(leaf, &f) {
			fields = append(fields, f.Field)
		}
	}
	if len(fields) != 2 || fields[0] != "email" || fields[1] != "age" {
		t.Errorf("Flatten fields = %v, want [email age]", fields)
	}
}

func TestClassifyAsType(t *testing.T) {
	t.Parallel()

	status, _ := ClassifyAsType(errors.Join(&FieldError{Field: "x", Reason: "y"}))
	if status != http.StatusBadRequest {
		t.Errorf("AsType field status = %d, want 400", status)
	}
	status, _ = ClassifyAsType(errors.Join(&RetryableError{AfterSeconds: 1}))
	if status != http.StatusServiceUnavailable {
		t.Errorf("AsType retryable status = %d, want 503", status)
	}
	status, _ = ClassifyAsType(errors.New("boom"))
	if status != http.StatusInternalServerError {
		t.Errorf("AsType unknown status = %d, want 500", status)
	}
}
```

## Review

The handler is correct when a `*FieldError` in the tree yields 400, a
`*RetryableError` yields 503, an unrecognized error falls through to 500, and nil is
200 — the `httptest` table pins all four. `errors.As` and `errors.AsType` return the
*first* match, which `TestClassifyFirstMatch` proves, along with `Flatten`
recovering every match for a body that must list all violations. Never call
`errors.As` with a value (it panics); pass a non-nil pointer, or prefer the
`errors.AsType[E]` value-returning form on Go 1.26. Run with `-race`; `httptest` and
the handler are safe to exercise concurrently.

## Resources

- [errors.As](https://pkg.go.dev/errors#As) — first assignable match; panics on a non-pointer target.
- [errors.AsType](https://pkg.go.dev/errors#AsType) — the Go 1.26 generic form returning the value.
- [net/http/httptest](https://pkg.go.dev/net/http/httptest) — `NewRecorder` and `NewRequest` for handler tests.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [09-concurrent-collect-vs-errgroup.md](09-concurrent-collect-vs-errgroup.md) | Next: [../08-panic-vs-error/00-concepts.md](../08-panic-vs-error/00-concepts.md)
