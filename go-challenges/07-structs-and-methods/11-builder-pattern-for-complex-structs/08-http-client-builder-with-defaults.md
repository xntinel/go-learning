# Exercise 8: HTTP Client Builder that Applies Safe Production Defaults

An `http.Client` built carelessly is a production incident waiting to happen: a zero
`Timeout` means *no timeout*, so one slow upstream can pin a goroutine forever. This
module builds a client builder that guarantees a finite timeout, a tuned connection
pool, its own transport per build, and an optional custom `RoundTripper` for retry
wrapping.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
clientbuild/                independent module: example.com/clientbuild
  go.mod                    go 1.26
  builder.go                package httpclient: Builder, New, setters, Build
  cmd/
    demo/
      main.go               runnable demo: default client, custom client, rejection
  builder_test.go           defaults, timeout validation, custom RT, independent transports
```

- Files: `builder.go`, `cmd/demo/main.go`, `builder_test.go`.
- Implement: a builder producing a `*http.Client` with a finite default `Timeout`, a fresh `*http.Transport` per build (tuned `MaxIdleConnsPerHost` and `IdleConnTimeout` filled via `cmp.Or`, `MaxConnsPerHost` passed through since its zero means "unlimited"), an optional custom `RoundTripper`, and validation rejecting a non-positive timeout override or an explicitly nil transport.
- Test: default build yields a finite non-zero timeout and a non-nil transport; `Timeout(0)` or negative errors; a custom `RoundTripper` is honored (stub captures the request); two builds produce independent `*http.Transport` instances; one `httptest` round-trip smoke test.
- Verify: `go test -count=1 -race ./...`

### Why the timeout is tri-state but the pool defaults use cmp.Or

Two of the three "unset" strategies from earlier show up here, each where it fits.
The pooling fields `MaxIdleConnsPerHost` and `IdleConnTimeout` use
`cmp.Or(setValue, default)`: for these, zero genuinely means "not tuned", so falling
through to a sane default is correct. `MaxConnsPerHost` is the exception among the
pool fields — its zero value already means "unlimited" to `net/http`, which is the
default we want, so it is passed through directly rather than routed through a
`cmp.Or` fallback (a `cmp.Or(x, 0)` would be a no-op anyway). The `Timeout` cannot
use `cmp.Or` for the opposite reason: zero is not "unset" to `net/http` — it is the
explicit, dangerous value "wait forever", and no default may silently override an
operator who typed it. So the builder tracks whether a timeout was set (a pointer field):
if it was set to zero or negative, that is an error (`ErrNonPositiveTimeout`); if it
was never set, the builder fills a finite 30-second default. This is the exact
"zero is a real value, use tri-state not cmp.Or" rule, applied to the one field
where getting it wrong hangs production.

Two more guarantees matter. First, every build constructs its *own* `*http.Transport`.
Sharing one transport across clients means tuning or closing one client's transport
silently affects the others; a fresh transport per build keeps them isolated.
Second, an optional custom `RoundTripper` lets a caller wrap the client with retry
or instrumentation logic; the builder honors it as the client's transport, and
rejects an explicitly nil one (`ErrNilTransport`) rather than producing a client
that panics on first use. Validation aggregates with `errors.Join` so a caller who
gets two things wrong learns both at once.

Create `builder.go`:

```go
package httpclient

import (
	"cmp"
	"errors"
	"net/http"
	"time"
)

var (
	// ErrNonPositiveTimeout is returned when a timeout override is set to zero
	// or a negative duration.
	ErrNonPositiveTimeout = errors.New("timeout must be positive")
	// ErrNilTransport is returned when a nil RoundTripper is explicitly set.
	ErrNilTransport = errors.New("transport must not be nil")
)

const defaultTimeout = 30 * time.Second

// Builder accumulates HTTP client settings and produces a fully-configured,
// safe *http.Client at Build.
type Builder struct {
	timeout         *time.Duration // tri-state: nil means "use default"
	maxIdlePerHost  int
	maxConnsPerHost int
	idleConnTimeout time.Duration
	transport       http.RoundTripper
	transportSet    bool
}

func New() *Builder {
	return &Builder{}
}

// Timeout sets the overall request timeout. Zero or negative is rejected at Build.
func (b *Builder) Timeout(d time.Duration) *Builder {
	b.timeout = &d
	return b
}

func (b *Builder) MaxIdleConnsPerHost(n int) *Builder {
	b.maxIdlePerHost = n
	return b
}

func (b *Builder) MaxConnsPerHost(n int) *Builder {
	b.maxConnsPerHost = n
	return b
}

func (b *Builder) IdleConnTimeout(d time.Duration) *Builder {
	b.idleConnTimeout = d
	return b
}

// Transport sets a custom RoundTripper (for example a retry wrapper). A nil
// value is rejected at Build.
func (b *Builder) Transport(rt http.RoundTripper) *Builder {
	b.transport = rt
	b.transportSet = true
	return b
}

func (b *Builder) Build() (*http.Client, error) {
	var errs []error
	if b.timeout != nil && *b.timeout <= 0 {
		errs = append(errs, ErrNonPositiveTimeout)
	}
	if b.transportSet && b.transport == nil {
		errs = append(errs, ErrNilTransport)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}

	timeout := defaultTimeout
	if b.timeout != nil {
		timeout = *b.timeout
	}

	var rt http.RoundTripper = b.transport
	if rt == nil {
		// A fresh transport per build: never shared mutable state.
		rt = &http.Transport{
			MaxIdleConnsPerHost: cmp.Or(b.maxIdlePerHost, 100),
			// Zero means unlimited to net/http, which is the intended
			// default, so there is no cmp.Or fallback here.
			MaxConnsPerHost: b.maxConnsPerHost,
			IdleConnTimeout: cmp.Or(b.idleConnTimeout, 90*time.Second),
		}
	}

	return &http.Client{Timeout: timeout, Transport: rt}, nil
}
```

### The runnable demo

The demo builds a default client and a tuned one, prints their pool settings, and
shows the rejection of a zero timeout.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http"
	"time"

	"example.com/clientbuild"
)

func main() {
	c, _ := httpclient.New().Build()
	tr := c.Transport.(*http.Transport)
	fmt.Printf("default: timeout=%s maxIdlePerHost=%d idleTimeout=%s\n",
		c.Timeout, tr.MaxIdleConnsPerHost, tr.IdleConnTimeout)

	c2, _ := httpclient.New().Timeout(5 * time.Second).MaxIdleConnsPerHost(20).Build()
	tr2 := c2.Transport.(*http.Transport)
	fmt.Printf("custom:  timeout=%s maxIdlePerHost=%d\n", c2.Timeout, tr2.MaxIdleConnsPerHost)

	if _, err := httpclient.New().Timeout(0).Build(); err != nil {
		fmt.Println("rejected:", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
default: timeout=30s maxIdlePerHost=100 idleTimeout=1m30s
custom:  timeout=5s maxIdlePerHost=20
rejected: timeout must be positive
```

### Tests

The tests prove the safety guarantees: a finite default timeout, rejection of a
non-positive override, a custom `RoundTripper` honored (a stub captures the outgoing
request without touching the network), independent transports across builds, and one
real round trip through `httptest`.

Create `builder_test.go`:

```go
package httpclient

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDefaultBuildIsSafe(t *testing.T) {
	t.Parallel()

	c, err := New().Build()
	if err != nil {
		t.Fatal(err)
	}
	if c.Timeout <= 0 {
		t.Fatalf("timeout = %s, want finite non-zero default", c.Timeout)
	}
	if c.Transport == nil {
		t.Fatal("transport must not be nil")
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", c.Transport)
	}
	if tr.MaxIdleConnsPerHost != 100 {
		t.Fatalf("MaxIdleConnsPerHost = %d, want 100", tr.MaxIdleConnsPerHost)
	}
}

func TestTimeoutValidation(t *testing.T) {
	t.Parallel()

	for _, d := range []time.Duration{0, -time.Second} {
		if _, err := New().Timeout(d).Build(); !errors.Is(err, ErrNonPositiveTimeout) {
			t.Fatalf("Timeout(%s) err = %v, want ErrNonPositiveTimeout", d, err)
		}
	}
}

func TestNilTransportRejected(t *testing.T) {
	t.Parallel()

	if _, err := New().Transport(nil).Build(); !errors.Is(err, ErrNilTransport) {
		t.Fatalf("err = %v, want ErrNilTransport", err)
	}
}

type captureRT struct {
	got  *http.Request
	body string
}

func (c *captureRT) RoundTrip(r *http.Request) (*http.Response, error) {
	c.got = r
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(c.body)),
		Header:     make(http.Header),
	}, nil
}

func TestCustomRoundTripperHonored(t *testing.T) {
	t.Parallel()

	stub := &captureRT{body: "stubbed"}
	c, err := New().Transport(stub).Build()
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Get("https://api.example.com/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if stub.got == nil || stub.got.URL.Path != "/health" {
		t.Fatalf("custom RoundTripper did not capture the request: %+v", stub.got)
	}
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "stubbed" {
		t.Fatalf("body = %q, want stubbed", b)
	}
}

func TestIndependentTransports(t *testing.T) {
	t.Parallel()

	c1, _ := New().Build()
	c2, _ := New().Build()
	if c1.Transport == c2.Transport {
		t.Fatal("two builds must not share one *http.Transport")
	}
}

func TestRealRoundTrip(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "pong")
	}))
	defer srv.Close()

	c, err := New().Timeout(2 * time.Second).Build()
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "pong" {
		t.Fatalf("body = %q, want pong", b)
	}
}

func ExampleBuilder_Build() {
	c, _ := New().Build()
	fmt.Println(c.Timeout)
	// Output: 30s
}
```

## Review

The client builder is correct when it can never hand back an unsafe client.
`TestDefaultBuildIsSafe` proves a zero-config build still has a finite timeout and a
real transport, so the "forgot to set a timeout" incident is impossible.
`TestTimeoutValidation` proves an explicit zero or negative is rejected — the reason
`Timeout` is a tri-state pointer rather than a `cmp.Or` field, since zero is a
meaningful (and dangerous) value to `net/http`, not "unset". `TestIndependentTransports`
proves each build owns its transport, so tuning one client never mutates another.
`TestCustomRoundTripperHonored` proves the retry-wrapper seam works and does so
without a network call by capturing the request in a stub. The traps to avoid:
building a client with a zero `Timeout` (an unbounded hang) and sharing one mutable
`*http.Transport` across clients. Run `go test -race` to confirm.

## Resources

- [net/http.Client](https://pkg.go.dev/net/http#Client) — the `Timeout` and `Transport` fields and why a zero timeout means no timeout.
- [net/http.Transport](https://pkg.go.dev/net/http#Transport) — `MaxIdleConnsPerHost`, `MaxConnsPerHost`, and `IdleConnTimeout` connection-pool tuning.
- [net/http.RoundTripper](https://pkg.go.dev/net/http#RoundTripper) — the interface a retry or instrumentation wrapper implements.
- [cmp.Or](https://pkg.go.dev/cmp#Or) — filling the pool defaults where zero legitimately means unset.

---

Back to [00-concepts.md](00-concepts.md) | Next: [../12-designing-a-domain-model/00-concepts.md](../12-designing-a-domain-model/00-concepts.md)
