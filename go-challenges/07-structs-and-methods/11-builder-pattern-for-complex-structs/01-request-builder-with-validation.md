# Exercise 1: Fluent Request Builder with Aggregated Validation

An outbound HTTP request has one or two required fields (method, URL) and a long
tail of optional ones (headers, body, timeout). This is the canonical case for a
builder: a fluent chain of setters plus a single terminal `Build` that validates
everything at once and hands back a value nobody can corrupt through the builder.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
reqbuilder/                 independent module: example.com/reqbuilder
  go.mod                    go 1.26
  builder.go                package req: Request, Builder, New, setters, Build
  cmd/
    demo/
      main.go               runnable demo: a full chain, then a rejected build
  builder_test.go           table + property tests, run under -race
```

- Files: `builder.go`, `cmd/demo/main.go`, `builder_test.go`.
- Implement: a `Builder` with pointer-receiver chainable setters (`Method`, `URL`, `Header`, `Body`, `Timeout`) each returning `*Builder`, a `New()` that returns a fresh builder with an initialized headers map, and a `Build()` that aggregates missing-required-field errors with `errors.Join` and returns a `Request` whose headers map is a defensive `maps.Clone` of the builder's internal map.
- Test: full-chain happy path, rejects missing method, rejects missing URL, `Build` joins both errors, headers are an independent copy across two `Build()` calls, and all set headers survive.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/reqbuilder/cmd/demo
cd ~/go-exercises/reqbuilder
go mod init example.com/reqbuilder
```

### Why the terminal is the only validation point

Each setter records one field and returns the receiver, so the caller can write
one uninterrupted chain. None of them validates: a setter that could fail would
force an error check on every link, and the fluent style would collapse into a
staircase of `if err != nil`. All the checks live in `Build`, which is the single
transition from "provisional accumulation" to "guaranteed-valid product". `Build`
does two things a careless implementation skips. First, it aggregates: it appends a
wrapped sentinel for *each* missing required field and joins them with
`errors.Join`, so a caller who forgot both the method and the URL learns both at
once and each is matchable with `errors.Is`. Second, it takes ownership: it returns
the headers as `maps.Clone(b.headers)`, a fresh map, so the produced `Request`
cannot be mutated through the builder's internal map and a second `Build` cannot see
a mutation made to the first result. Copying a struct copies the map header, not the
entries, so without the clone the two would silently share storage.

Create `builder.go`:

```go
package req

import (
	"errors"
	"maps"
	"time"
)

// Sentinels for the required-field checks. Build wraps these with errors.Join
// so a caller can match each one with errors.Is.
var (
	ErrEmptyMethod = errors.New("method is required")
	ErrEmptyURL    = errors.New("url is required")
)

// Request is the fully-owned product of the builder. Every field is public so a
// caller (and cmd/demo) can read it; Headers is always a private copy.
type Request struct {
	Method  string
	URL     string
	Headers map[string]string
	Body    string
	Timeout time.Duration
}

// Builder accumulates request fields. It is single-use and not safe for
// concurrent use; call New for each request.
type Builder struct {
	method  string
	url     string
	headers map[string]string
	body    string
	timeout time.Duration
}

// New returns a fresh builder with an initialized headers map so Header can be
// called without a nil-map panic.
func New() *Builder {
	return &Builder{headers: make(map[string]string)}
}

func (b *Builder) Method(m string) *Builder {
	b.method = m
	return b
}

func (b *Builder) URL(u string) *Builder {
	b.url = u
	return b
}

func (b *Builder) Header(key, value string) *Builder {
	b.headers[key] = value
	return b
}

func (b *Builder) Body(body string) *Builder {
	b.body = body
	return b
}

func (b *Builder) Timeout(d time.Duration) *Builder {
	b.timeout = d
	return b
}

// Build validates the accumulated state and returns a fully-owned Request. It
// reports every missing required field at once via errors.Join, and defensively
// clones the headers map so the result and the builder never alias.
func (b *Builder) Build() (Request, error) {
	var errs []error
	if b.method == "" {
		errs = append(errs, ErrEmptyMethod)
	}
	if b.url == "" {
		errs = append(errs, ErrEmptyURL)
	}
	if err := errors.Join(errs...); err != nil {
		return Request{}, err
	}
	return Request{
		Method:  b.method,
		URL:     b.url,
		Headers: maps.Clone(b.headers),
		Body:    b.body,
		Timeout: b.timeout,
	}, nil
}
```

### The runnable demo

The demo builds one complete request, prints its fields, then deliberately builds
an empty one to show the aggregated validation error.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/reqbuilder"
)

func main() {
	r, err := req.New().
		Method("POST").
		URL("https://api.example.com/users").
		Header("Content-Type", "application/json").
		Body(`{"name":"alice"}`).
		Timeout(5 * time.Second).
		Build()
	if err != nil {
		fmt.Println("unexpected error:", err)
		return
	}
	fmt.Printf("%s %s\n", r.Method, r.URL)
	fmt.Printf("Content-Type: %s\n", r.Headers["Content-Type"])
	fmt.Printf("body: %s\n", r.Body)
	fmt.Printf("timeout: %s\n", r.Timeout)

	if _, err := req.New().Build(); err != nil {
		fmt.Println("rejected empty build:", err)
	}
}
```

The package is named `req`, so the import path `example.com/reqbuilder`
resolves to the identifier `req`.

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
POST https://api.example.com/users
Content-Type: application/json
body: {"name":"alice"}
timeout: 5s
rejected empty build: method is required
url is required
```

The joined error prints both sentinels on separate lines because `errors.Join`
inserts a newline between its causes.

### Tests

The tests are the preserved table from the original lesson plus the
headers-preservation property. `TestBuildJoinsAllErrors` is the centerpiece: it
proves an empty build reports *both* missing fields and that each remains
matchable with `errors.Is`. `TestHeadersAreIndependentCopy` proves the
copy-on-build ownership contract — mutating one built result never leaks into a
later build.

Create `builder_test.go`:

```go
package req

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestBuilderChainsAllFields(t *testing.T) {
	t.Parallel()

	r, err := New().
		Method("POST").
		URL("https://api.example.com/users").
		Header("Content-Type", "application/json").
		Body(`{"name":"alice"}`).
		Timeout(5 * time.Second).
		Build()
	if err != nil {
		t.Fatal(err)
	}
	if r.Method != "POST" || r.URL != "https://api.example.com/users" {
		t.Fatalf("Method/URL mismatch: %+v", r)
	}
	if r.Headers["Content-Type"] != "application/json" {
		t.Fatalf("Content-Type = %q", r.Headers["Content-Type"])
	}
	if r.Body != `{"name":"alice"}` {
		t.Fatalf("Body = %q", r.Body)
	}
	if r.Timeout != 5*time.Second {
		t.Fatalf("Timeout = %s", r.Timeout)
	}
}

func TestBuildRejectsMissingMethod(t *testing.T) {
	t.Parallel()

	_, err := New().URL("https://api.example.com").Build()
	if !errors.Is(err, ErrEmptyMethod) {
		t.Fatalf("err = %v, want ErrEmptyMethod", err)
	}
}

func TestBuildRejectsMissingURL(t *testing.T) {
	t.Parallel()

	_, err := New().Method("GET").Build()
	if !errors.Is(err, ErrEmptyURL) {
		t.Fatalf("err = %v, want ErrEmptyURL", err)
	}
}

func TestBuildJoinsAllErrors(t *testing.T) {
	t.Parallel()

	_, err := New().Build()
	if !errors.Is(err, ErrEmptyMethod) {
		t.Fatalf("err should include ErrEmptyMethod: %v", err)
	}
	if !errors.Is(err, ErrEmptyURL) {
		t.Fatalf("err should include ErrEmptyURL: %v", err)
	}
}

func TestHeadersAreIndependentCopy(t *testing.T) {
	t.Parallel()

	b := New().Method("GET").URL("https://api.example.com").Header("X-Test", "1")
	r, _ := b.Build()
	r.Headers["X-New"] = "2"

	r2, _ := b.Build()
	if _, ok := r2.Headers["X-New"]; ok {
		t.Fatal("second Build() should not see mutation from first")
	}
}

func TestBuilderPreservesAllHeaders(t *testing.T) {
	t.Parallel()

	r, err := New().
		Method("GET").
		URL("https://api.example.com").
		Header("A", "1").
		Header("B", "2").
		Header("C", "3").
		Build()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"A": "1", "B": "2", "C": "3"}
	for k, v := range want {
		if r.Headers[k] != v {
			t.Fatalf("Headers[%q] = %q, want %q", k, r.Headers[k], v)
		}
	}
	if len(r.Headers) != len(want) {
		t.Fatalf("len(Headers) = %d, want %d", len(r.Headers), len(want))
	}
}

func Example() {
	r, _ := New().Method("GET").URL("https://example.com").Build()
	fmt.Println(r.Method, r.URL)
	// Output: GET https://example.com
}
```

## Review

The builder is correct when three properties hold. First, no setter ever returns
an error — validation is centralized in `Build`, so the fluent chain never breaks.
Second, `Build` reports every missing required field together: `TestBuildJoinsAllErrors`
confirms an empty build matches both `ErrEmptyMethod` and `ErrEmptyURL` through
`errors.Is`, which only works because each is wrapped by `errors.Join` rather than
formatted into one opaque string. Third, the produced `Request` owns its headers:
`TestHeadersAreIndependentCopy` mutates one result and proves a later build is
unaffected, which holds only because `Build` returns `maps.Clone(b.headers)` and
not the builder's own map. Run `go test -race` to confirm nothing shares mutable
state it should not.

## Resources

- [errors.Join](https://pkg.go.dev/errors#Join) — aggregating multiple errors while keeping each matchable.
- [maps.Clone](https://pkg.go.dev/maps#Clone) — the defensive copy that gives Build ownership of the headers.
- [time.Duration](https://pkg.go.dev/time#Duration) — the optional timeout field's type.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-functional-options-vs-builder.md](02-functional-options-vs-builder.md)
