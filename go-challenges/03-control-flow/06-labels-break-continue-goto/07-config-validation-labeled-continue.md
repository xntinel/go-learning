# Exercise 7: Validate nested config, skipping a service's endpoints on a bad header

A config loader validates services, each with a list of endpoints. If a service's
own required header is invalid, validating its endpoints is noise — the service is
already rejected, and a pile of endpoint errors under a broken service buries the
real cause. A labeled `continue` skips straight to the next service; otherwise
every endpoint is validated and its errors accumulated.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
configvalidate/            independent module: example.com/configvalidate
  go.mod                   go 1.24
  configvalidate.go        Endpoint, Service; Validate accumulates errors
  cmd/
    demo/
      main.go              runnable demo: one bad-header service, one with bad endpoints
  configvalidate_test.go   bad header skips endpoints; good services report each bad endpoint
```

- Files: `configvalidate.go`, `cmd/demo/main.go`, `configvalidate_test.go`.
- Implement: `Validate(services) error` with an outer loop over services and an inner loop over endpoints; an invalid service header triggers `continue nextService` (skipping endpoint validation for that service), while a valid header validates every endpoint and joins the errors.
- Test: with service[1] having an invalid header and 3 endpoints, exactly one error is reported for service[1] and zero endpoint errors for it, while valid services still report each bad endpoint.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/06-labels-break-continue-goto/07-config-validation-labeled-continue/cmd/demo
cd go-solutions/03-control-flow/06-labels-break-continue-goto/07-config-validation-labeled-continue
go mod edit -go=1.24
```

### Why skip the endpoints when the header is bad

A service whose auth header is malformed cannot be deployed at all, so its
endpoints will never be reached. Reporting three endpoint errors underneath that
one header error is misleading: it inflates the error count and hides the single
fix the operator needs to make. The validator therefore treats a bad header as
terminal *for that service* — it records one header error and jumps to the next
service with `continue nextService`, never entering the endpoint loop.

`continue` targets a `for`, so the label `nextService` sits on the outer service
loop. A bare `continue` inside the inner endpoint loop would only move to the next
endpoint of the *same* service, which is not what we want — the check happens
before the endpoint loop, at the top of each service iteration, and skips the
whole inner loop. Valid services fall through to the endpoint loop and accumulate
one error per bad endpoint. Everything is collected with `errors.Join` and wrapped
with sentinels so a caller can `errors.Is` and count precisely.

Create `configvalidate.go`:

```go
package configvalidate

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// Endpoint is one route a service exposes.
type Endpoint struct {
	Name string
	URL  string
}

// Service groups endpoints behind a required auth header.
type Service struct {
	Name    string
	Header  string // required auth header token, e.g. "Authorization"
	Targets []Endpoint
}

// Sentinel errors for the two failure classes.
var (
	ErrBadHeader   = errors.New("invalid service header")
	ErrBadEndpoint = errors.New("invalid endpoint url")
)

// Validate checks every service and its endpoints. If a service header is
// invalid, that service contributes exactly one error and its endpoints are not
// validated (skipped via a labeled continue). Otherwise each bad endpoint
// contributes its own error. All errors are joined.
func Validate(services []Service) error {
	var errs []error

nextService:
	for _, s := range services {
		if !validHeader(s.Header) {
			errs = append(errs, fmt.Errorf("service %q: %w", s.Name, ErrBadHeader))
			continue nextService // header is terminal; skip its endpoints
		}
		for _, ep := range s.Targets {
			if !validURL(ep.URL) {
				errs = append(errs, fmt.Errorf("service %q endpoint %q: %w", s.Name, ep.Name, ErrBadEndpoint))
			}
		}
	}

	return errors.Join(errs...)
}

// validHeader accepts a non-empty header token with no whitespace.
func validHeader(h string) bool {
	if h == "" {
		return false
	}
	return h == strings.TrimSpace(h) && !strings.ContainsAny(h, " \t")
}

// validURL accepts only absolute https URLs with a host.
func validURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return u.Scheme == "https" && u.Host != ""
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/configvalidate"
)

func main() {
	services := []configvalidate.Service{
		{
			Name:   "billing",
			Header: "bad header", // has a space: invalid
			Targets: []configvalidate.Endpoint{
				{Name: "charge", URL: "https://billing.internal/charge"},
				{Name: "refund", URL: "not-a-url"},
			},
		},
		{
			Name:   "search",
			Header: "Authorization",
			Targets: []configvalidate.Endpoint{
				{Name: "query", URL: "https://search.internal/query"},
				{Name: "index", URL: "http://insecure/index"}, // not https
			},
		},
	}

	if err := configvalidate.Validate(services); err != nil {
		fmt.Println("config invalid:")
		fmt.Println(err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
config invalid:
service "billing": invalid service header
service "search" endpoint "index": invalid endpoint url
```

The `billing` service has a bad endpoint too (`refund`), but because its header is
invalid, its endpoints are never validated — only the single header error appears.
The `search` service has a valid header, so its one insecure endpoint is reported.

### Tests

`TestBadHeaderSkipsEndpoints` is the core case: service[1] has an invalid header
and three endpoints (two of them bad). The test asserts exactly one error for
service[1] — the header — and zero endpoint errors for it, while a valid service
still reports each of its bad endpoints. It counts the joined errors by unwrapping
and classifying them.

Create `configvalidate_test.go`:

```go
package configvalidate

import (
	"errors"
	"strings"
	"testing"
)

func unwrapJoined(err error) []error {
	if err == nil {
		return nil
	}
	if u, ok := err.(interface{ Unwrap() []error }); ok {
		return u.Unwrap()
	}
	return []error{err}
}

func TestBadHeaderSkipsEndpoints(t *testing.T) {
	t.Parallel()

	services := []Service{
		{
			Name:   "good",
			Header: "Authorization",
			Targets: []Endpoint{
				{Name: "ok", URL: "https://good.internal/ok"},
				{Name: "bad", URL: "http://good.internal/bad"}, // not https
			},
		},
		{
			Name:   "broken",
			Header: "has space", // invalid header
			Targets: []Endpoint{
				{Name: "e0", URL: "not-a-url"},
				{Name: "e1", URL: "ftp://x/y"},
				{Name: "e2", URL: "https://ok.internal/z"},
			},
		},
	}

	err := Validate(services)
	errs := unwrapJoined(err)

	var headerErrs, endpointErrs int
	for _, e := range errs {
		switch {
		case errors.Is(e, ErrBadHeader):
			headerErrs++
		case errors.Is(e, ErrBadEndpoint):
			endpointErrs++
		}
	}

	if headerErrs != 1 {
		t.Fatalf("header errors = %d, want 1", headerErrs)
	}
	// Only the "good" service's one bad endpoint; "broken"'s endpoints are skipped.
	if endpointErrs != 1 {
		t.Fatalf("endpoint errors = %d, want 1 (broken service endpoints must be skipped)", endpointErrs)
	}
	for _, e := range errs {
		if errors.Is(e, ErrBadEndpoint) && strings.Contains(e.Error(), "broken") {
			t.Fatalf("endpoint of broken service was validated: %v", e)
		}
	}
}

func TestAllValid(t *testing.T) {
	t.Parallel()

	services := []Service{
		{
			Name:    "a",
			Header:  "Authorization",
			Targets: []Endpoint{{Name: "x", URL: "https://a/x"}},
		},
	}
	if err := Validate(services); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
}

func TestValidHeader(t *testing.T) {
	t.Parallel()

	tests := []struct {
		header string
		want   bool
	}{
		{"Authorization", true},
		{"", false},
		{"has space", false},
		{" leading", false},
	}
	for _, tc := range tests {
		if got := validHeader(tc.header); got != tc.want {
			t.Errorf("validHeader(%q) = %v, want %v", tc.header, got, tc.want)
		}
	}
}
```

## Review

The validator is correct when a bad-header service yields exactly one error and no
endpoint errors, and a good service yields one error per bad endpoint. The bug to
avoid is a bare `continue` inside the endpoint loop (it would skip a single
endpoint, not the whole service) or, worse, no skip at all (a broken service
buries the operator in endpoint noise). Placing the header check at the top of the
service iteration and using `continue nextService` is what makes "skip this whole
service" a single, readable statement. `errors.Join` plus wrapped sentinels keeps
the result both countable (`errors.Is`) and legible.

## Resources

- [Go Specification: Continue statements](https://go.dev/ref/spec#Continue_statements) — labeled `continue` targets the named `for`.
- [net/url.Parse](https://pkg.go.dev/net/url#Parse) — parsing and validating endpoint URLs.
- [errors.Join](https://pkg.go.dev/errors#Join) — accumulating validation failures.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [06-multi-resource-acquire-goto-cleanup.md](06-multi-resource-acquire-goto-cleanup.md) | Next: [08-logscan-multiline-records.md](08-logscan-multiline-records.md)
