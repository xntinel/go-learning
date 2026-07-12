# Exercise 2: A Machine-Readable Deprecation and Sunset Path

This is the real on-the-job work: standing up an automatable deprecation program
for an API other teams depend on. You build lifecycle middleware that emits the
RFC 9745 `Deprecation` and RFC 8594 `Sunset` headers plus a deprecation `Link`,
and enforces the sunset by failing closed with `410 Gone` once the removal instant
passes. A clock is injected so the boundary is deterministic in tests.

This module is fully self-contained: its own `go mod init`, code, demo, and tests.

## What you'll build

```text
deprecation/                   independent module: example.com/deprecation
  go.mod                       go 1.24
  deprecation.go               Lifecycle, New (validates ordering), Wrap middleware
  cmd/
    demo/
      main.go                  before-sunset vs after-sunset with an injected clock
  deprecation_test.go          recorder tests for headers, ordering, 410, and no-op
```

- Files: `deprecation.go`, `cmd/demo/main.go`, `deprecation_test.go`.
- Implement: a `Lifecycle` with an injected clock, a `New` constructor that enforces the RFC 9745 ordering invariant via a wrapped sentinel error, and a `Wrap` middleware that attaches the three signalling headers and returns `410 Gone` with a problem body at or after sunset.
- Test: a deprecated-but-not-sunset request returns 200 with a parseable `Sunset` and a `@<unix>` `Deprecation`; the deprecation instant is not after the sunset instant; advancing the clock past sunset yields 410 with `application/problem+json`; a non-deprecated lifecycle emits none of the headers.
- Verify: `go test -count=1 -race ./...`

### The two header formats are different on purpose

The single most error-prone part of this exercise is that the two dates use two
different serializations, and mixing them up produces headers clients cannot parse.

`Deprecation` (RFC 9745) carries a Structured-Fields *Date* item: an at-sign
followed by integer Unix seconds. `time.Time.Unix()` gives the seconds; the header
value is `"@" + strconv.FormatInt(t.Unix(), 10)`, e.g. `@1767225600`.

`Sunset` (RFC 8594) carries an HTTP-date (IMF-fixdate), always in GMT. The stdlib
constant `http.TimeFormat` is exactly that layout
(`"Mon, 02 Jan 2006 15:04:05 GMT"`), so the value is
`t.UTC().Format(http.TimeFormat)`, e.g. `Wed, 01 Jul 2026 00:00:00 GMT`. Calling
`.UTC()` first matters: the layout ends in the literal `GMT`, so formatting a
non-UTC time would print the wrong instant under a `GMT` label.

The deprecation `Link` relation points clients at migration docs; when a sunset is
scheduled the middleware adds a second `Link` with `rel="sunset"`. Both are added
with `Header.Add` (not `Set`) because a response may legitimately carry several
`Link` values.

### Enforcing the ordering invariant at construction

RFC 9745 states the Sunset instant MUST NOT precede the Deprecation instant. The
`New` constructor enforces this once, up front, returning a wrapped sentinel error
(`ErrSunsetBeforeDeprecation`) so callers can assert it with `errors.Is`. Catching
it at construction is better than at request time: a misconfigured lifecycle
should fail to build, not silently emit contradictory headers on every response.

### Failing closed at sunset

Before the sunset instant the middleware attaches the headers and calls through to
the wrapped handler — a normal 200 with warnings. At or after the sunset instant
(`!now.Before(SunsetAt)`) it stops serving the resource and returns `410 Gone`
with an `application/problem+json` body. Failing closed is deliberate: a version
that keeps working after its announced sunset teaches every client to ignore your
sunsets. The clock is the injected `Now func() time.Time`, which is what makes the
boundary testable to the second without sleeping.

A zero `DeprecatedAt` means the version is not deprecated at all; `Wrap` then
passes straight through and emits none of the three headers, so the same
middleware type can wrap current versions harmlessly.

Create `deprecation.go`:

```go
package deprecation

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// ErrSunsetBeforeDeprecation reports a lifecycle whose sunset instant precedes
// its deprecation instant, which RFC 9745 forbids.
var ErrSunsetBeforeDeprecation = errors.New("sunset instant is before deprecation instant")

// Lifecycle describes the deprecation program for one API version. A zero
// DeprecatedAt means the version is not deprecated and Wrap is a passthrough.
type Lifecycle struct {
	// DeprecatedAt is when the version became (or becomes) deprecated. It is
	// emitted as the RFC 9745 Deprecation header, a Structured-Fields Date
	// serialized as @<unix-seconds>.
	DeprecatedAt time.Time
	// SunsetAt is when the version stops responding. It is emitted as the RFC
	// 8594 Sunset header in HTTP-date (IMF-fixdate) form. Zero means announced
	// deprecation with no scheduled removal yet.
	SunsetAt time.Time
	// DocsURL points clients at migration guidance via a deprecation link.
	DocsURL string
	// Now is an injectable clock; nil uses time.Now.
	Now func() time.Time
}

// New validates and constructs a Lifecycle, enforcing the RFC 9745 ordering
// invariant that sunset must not precede deprecation.
func New(deprecatedAt, sunsetAt time.Time, docsURL string, now func() time.Time) (*Lifecycle, error) {
	if !sunsetAt.IsZero() && sunsetAt.Before(deprecatedAt) {
		return nil, fmt.Errorf("new lifecycle: %w", ErrSunsetBeforeDeprecation)
	}
	return &Lifecycle{
		DeprecatedAt: deprecatedAt,
		SunsetAt:     sunsetAt,
		DocsURL:      docsURL,
		Now:          now,
	}, nil
}

func (l *Lifecycle) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

type problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail"`
}

// Wrap returns middleware that attaches the deprecation signalling headers and
// enforces the sunset boundary. Before sunset it serves normally with the
// warning headers; at or after sunset it fails closed with 410 Gone.
func (l *Lifecycle) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if l.DeprecatedAt.IsZero() {
			next.ServeHTTP(w, r)
			return
		}
		h := w.Header()
		h.Set("Deprecation", "@"+strconv.FormatInt(l.DeprecatedAt.Unix(), 10))
		if l.DocsURL != "" {
			h.Add("Link", fmt.Sprintf("<%s>; rel=\"deprecation\"", l.DocsURL))
		}
		if !l.SunsetAt.IsZero() {
			h.Set("Sunset", l.SunsetAt.UTC().Format(http.TimeFormat))
			if l.DocsURL != "" {
				h.Add("Link", fmt.Sprintf("<%s>; rel=\"sunset\"", l.DocsURL))
			}
		}
		if !l.SunsetAt.IsZero() && !l.now().Before(l.SunsetAt) {
			h.Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusGone)
			_ = json.NewEncoder(w).Encode(problem{
				Type:   l.DocsURL,
				Title:  "Gone",
				Status: http.StatusGone,
				Detail: "This API version has been sunset; migrate to the current version.",
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}
```

### The runnable demo

The demo wraps a trivial handler in a lifecycle deprecated on 2026-01-01 with a
sunset of 2026-07-01, driving it first with a clock before the sunset and then
after, so you see the 200-with-warnings response turn into a 410.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"example.com/deprecation"
)

func main() {
	deprecatedAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	sunsetAt := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	clock := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	lc, err := deprecation.New(deprecatedAt, sunsetAt, "https://api.acme.example/deprecations/orders-v1", func() time.Time { return clock })
	if err != nil {
		fmt.Println(err)
		return
	}

	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"id":"ord-42"}`)
	})
	h := lc.Wrap(ok)

	call := func(label string) {
		req := httptest.NewRequest(http.MethodGet, "/orders", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		fmt.Printf("%s\n", label)
		fmt.Printf("  status:      %d\n", rec.Code)
		fmt.Printf("  Deprecation: %s\n", rec.Header().Get("Deprecation"))
		fmt.Printf("  Sunset:      %s\n", rec.Header().Get("Sunset"))
		fmt.Printf("  Link:        %s\n", strings.Join(rec.Header().Values("Link"), ", "))
		fmt.Printf("  body:        %s\n", strings.TrimSpace(rec.Body.String()))
	}

	call("before sunset (2026-03-01)")
	clock = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	call("after sunset (2026-08-01)")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
before sunset (2026-03-01)
  status:      200
  Deprecation: @1767225600
  Sunset:      Wed, 01 Jul 2026 00:00:00 GMT
  Link:        <https://api.acme.example/deprecations/orders-v1>; rel="deprecation", <https://api.acme.example/deprecations/orders-v1>; rel="sunset"
  body:        {"id":"ord-42"}
after sunset (2026-08-01)
  status:      410
  Deprecation: @1767225600
  Sunset:      Wed, 01 Jul 2026 00:00:00 GMT
  Link:        <https://api.acme.example/deprecations/orders-v1>; rel="deprecation", <https://api.acme.example/deprecations/orders-v1>; rel="sunset"
  body:        {"type":"https://api.acme.example/deprecations/orders-v1","title":"Gone","status":410,"detail":"This API version has been sunset; migrate to the current version."}
```

### Tests

`TestDeprecatedButNotSunset` asserts the 200 path carries a `@<unix>` `Deprecation`
and a `Sunset` that parses back via `time.Parse(http.TimeFormat, ...)`, proving the
format is a real HTTP-date. `TestDeprecationNotAfterSunset` parses both headers
back to instants and checks the ordering invariant on the wire values themselves —
the correctness trap being that the two formats are not interchangeable.
`TestNewRejectsSunsetBeforeDeprecation` asserts the sentinel via `errors.Is`.
`TestAfterSunsetReturnsGone` advances the injected clock past sunset and checks the
410 and the problem media type. `TestNotDeprecatedEmitsNoHeaders` proves a zero
lifecycle is a no-op.

Create `deprecation_test.go`:

```go
package deprecation

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

var (
	deprecatedAt = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	sunsetAt     = time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	docsURL      = "https://api.acme.example/deprecations/orders-v1"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"id":"ord-42"}`)
	})
}

func serve(t *testing.T, lc *Lifecycle) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/orders", nil)
	rec := httptest.NewRecorder()
	lc.Wrap(okHandler()).ServeHTTP(rec, req)
	return rec
}

func TestDeprecatedButNotSunset(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	lc, err := New(deprecatedAt, sunsetAt, docsURL, func() time.Time { return clock })
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rec := serve(t, lc)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rec.Code)
	}
	if got, want := rec.Header().Get("Deprecation"), "@1767225600"; got != want {
		t.Fatalf("Deprecation = %q; want %q", got, want)
	}
	sunset, err := time.Parse(http.TimeFormat, rec.Header().Get("Sunset"))
	if err != nil {
		t.Fatalf("Sunset not an HTTP-date: %v", err)
	}
	if !sunset.Equal(sunsetAt) {
		t.Fatalf("Sunset = %v; want %v", sunset, sunsetAt)
	}
	links := strings.Join(rec.Header().Values("Link"), ", ")
	if !strings.Contains(links, `rel="deprecation"`) {
		t.Fatalf("Link missing rel=deprecation: %q", links)
	}
}

func TestDeprecationNotAfterSunset(t *testing.T) {
	t.Parallel()
	// The two headers use different formats on purpose: parse each back to an
	// instant and assert the RFC 9745 ordering invariant on the wire values.
	clock := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	lc, err := New(deprecatedAt, sunsetAt, docsURL, func() time.Time { return clock })
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rec := serve(t, lc)

	secs, err := strconv.ParseInt(strings.TrimPrefix(rec.Header().Get("Deprecation"), "@"), 10, 64)
	if err != nil {
		t.Fatalf("Deprecation not @<unix>: %q", rec.Header().Get("Deprecation"))
	}
	dep := time.Unix(secs, 0).UTC()

	sunset, err := time.Parse(http.TimeFormat, rec.Header().Get("Sunset"))
	if err != nil {
		t.Fatalf("Sunset parse: %v", err)
	}
	if dep.After(sunset) {
		t.Fatalf("deprecation %v is after sunset %v", dep, sunset)
	}
}

func TestNewRejectsSunsetBeforeDeprecation(t *testing.T) {
	t.Parallel()
	_, err := New(sunsetAt, deprecatedAt, docsURL, nil)
	if !errors.Is(err, ErrSunsetBeforeDeprecation) {
		t.Fatalf("err = %v; want ErrSunsetBeforeDeprecation", err)
	}
}

func TestAfterSunsetReturnsGone(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	lc, err := New(deprecatedAt, sunsetAt, docsURL, func() time.Time { return clock })
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rec := serve(t, lc)

	if rec.Code != http.StatusGone {
		t.Fatalf("status = %d; want 410", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("Content-Type = %q; want application/problem+json", ct)
	}
	var p problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if p.Status != http.StatusGone {
		t.Fatalf("problem.status = %d; want 410", p.Status)
	}
}

func TestNotDeprecatedEmitsNoHeaders(t *testing.T) {
	t.Parallel()
	lc := &Lifecycle{} // zero DeprecatedAt: not deprecated
	rec := serve(t, lc)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rec.Code)
	}
	for _, h := range []string{"Deprecation", "Sunset", "Link"} {
		if v := rec.Header().Get(h); v != "" {
			t.Fatalf("header %s = %q; want empty", h, v)
		}
	}
}

func ExampleLifecycle_Wrap() {
	lc, _ := New(deprecatedAt, sunsetAt, docsURL, func() time.Time {
		return time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	})
	req := httptest.NewRequest(http.MethodGet, "/orders", nil)
	rec := httptest.NewRecorder()
	lc.Wrap(okHandler()).ServeHTTP(rec, req)
	fmt.Println(rec.Code)
	fmt.Println(rec.Header().Get("Deprecation"))
	fmt.Println(rec.Header().Get("Sunset"))
	// Output:
	// 200
	// @1767225600
	// Wed, 01 Jul 2026 00:00:00 GMT
}
```

## Review

The lifecycle is correct when the two headers never share a format and the sunset
boundary is enforced exactly at the injected instant. The classic failures are
writing both dates the same way (a client then fails to parse one), setting the
Content-Type after `WriteHeader` (too late — headers must be set first, which is
why `Wrap` sets `application/problem+json` before `w.WriteHeader(http.StatusGone)`),
and forgetting to `.UTC()` the sunset time before formatting so the `GMT` label
lies. The ordering invariant is enforced at construction, not per request, so a
bad configuration cannot ship. Run `go test -race` to confirm the middleware holds
no shared mutable state across concurrent requests.

## Resources

- [RFC 9745 — The Deprecation HTTP Response Header Field](https://www.rfc-editor.org/rfc/rfc9745.html) — the `@<unix>` Structured-Fields Date value and the ordering invariant.
- [RFC 8594 — The Sunset HTTP Header Field](https://www.rfc-editor.org/rfc/rfc8594.html) — the HTTP-date `Sunset` value and the `sunset` link relation.
- [`http.TimeFormat`](https://pkg.go.dev/net/http#TimeFormat) — the IMF-fixdate layout used for `Sunset`.
- [RFC 9457 — Problem Details for HTTP APIs](https://www.rfc-editor.org/rfc/rfc9457.html) — the `application/problem+json` body returned at sunset.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-uri-vs-header-version-routing.md](01-uri-vs-header-version-routing.md) | Next: [03-schema-evolution-tolerant-reader.md](03-schema-evolution-tolerant-reader.md)
