# Exercise 4: Route Timeout and Deadline Propagation

A per-route timeout bounds how long the proxy will wait on an upstream, but a fixed timeout alone is not enough: if the caller already attached a tighter deadline, the proxy must honour it rather than do work the caller has stopped waiting for. This exercise builds the policy that computes the effective timeout as the tighter of a configured per-route limit and any deadline propagated in a request header.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
timeout.go             RouteTimeout, EffectiveTimeout (min of configured and propagated deadline)
cmd/
  demo/
    main.go            configured-only, caller-deadline-tighter, and neither cases
timeout_test.go        the four-quadrant table of configured x header presence
```

- Files: `timeout.go`, `cmd/demo/main.go`, `timeout_test.go`.
- Implement: `RouteTimeout` with `EffectiveTimeout(r *http.Request) (time.Duration, bool)`.
- Test: `timeout_test.go` covers no-config/no-header, configured-only, header-tighter, configured-tighter, and an expired header.
- Verify: `go test -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### Why propagate the caller's deadline at all

Picture a client that issues a request with a 500ms deadline through a proxy that applies a flat 2s per-route timeout. Without deadline propagation, the proxy keeps trying the upstream for a full 2s — 1.5s after the client has already given up and returned an error to its own user. Every one of those wasted seconds holds an upstream worker, a database connection, and a socket open on behalf of a request whose result will be thrown away. Under load this is how a slow dependency turns into a resource leak that takes down healthy services.

Deadline propagation fixes this by carrying the caller's deadline across the hop. The convention used here is a header — `X-Request-Deadline` — holding the absolute deadline as a Unix nanosecond timestamp. `EffectiveTimeout` reads it, converts it to a remaining duration with `time.Until`, and returns the tighter of that remaining time and the configured per-route timeout. An absolute timestamp rather than a relative duration is the right wire format because it is immune to clock skew accumulating across hops: each proxy computes "time from now until that instant" against its own clock, so a multi-hop chain converges on the original deadline instead of each hop re-adding its own slack.

The function is careful about degenerate headers. A header that fails to parse, or one whose deadline has already passed (`time.Until` returns a non-positive duration), is treated as absent: `hasDeadline` stays false and the configured timeout governs. This is the safe failure mode — a malformed or stale deadline never collapses the effective timeout to zero or a negative value, which would reject every request. The four outcomes fall out of one `switch`: both present picks the minimum, configured-only returns the config, header-only returns the remaining time, and neither returns `(0, false)` to mean "no timeout applies".

Create `timeout.go`:

```go
// Package timeout implements per-route request timeouts with deadline
// propagation: the effective timeout is the tighter of a configured per-route
// limit and any deadline the caller passed in a request header.
package timeout

import (
	"net/http"
	"strconv"
	"time"
)

// RouteTimeout configures per-route request timeouts and deadline propagation.
type RouteTimeout struct {
	// RequestTimeout is the maximum duration allowed for the entire forwarded
	// call, including all retry attempts. Zero disables the per-route timeout.
	RequestTimeout time.Duration
	// DeadlineHeader is the name of a request header carrying an incoming
	// deadline as a Unix nanosecond timestamp (e.g. "X-Request-Deadline").
	// When set and the header is present, the remaining time is used as the
	// timeout if it is tighter than RequestTimeout.
	DeadlineHeader string
}

// EffectiveTimeout returns the timeout to apply to the outgoing context.
// It returns (duration, true) when a finite timeout applies; (0, false) otherwise.
func (rt *RouteTimeout) EffectiveTimeout(r *http.Request) (time.Duration, bool) {
	configured := rt.RequestTimeout

	var remaining time.Duration
	hasDeadline := false
	if rt.DeadlineHeader != "" {
		if raw := r.Header.Get(rt.DeadlineHeader); raw != "" {
			if nsec, err := strconv.ParseInt(raw, 10, 64); err == nil {
				rem := time.Until(time.Unix(0, nsec))
				if rem > 0 {
					remaining = rem
					hasDeadline = true
				}
			}
		}
	}

	switch {
	case configured > 0 && hasDeadline:
		if remaining < configured {
			return remaining, true
		}
		return configured, true
	case configured > 0:
		return configured, true
	case hasDeadline:
		return remaining, true
	default:
		return 0, false
	}
}
```

The boolean second return value is what lets the caller distinguish "no timeout" from "a zero timeout". A transport uses it directly: when `ok` is true it wraps the outgoing context in `context.WithTimeout(ctx, d)`; when false it leaves the context untouched so the request runs unbounded (or under whatever deadline the context already carries). Returning a bare `time.Duration` could not express the no-timeout case without overloading zero, which is exactly the ambiguity the boolean removes.

### The runnable demo

The demo shows three of the four quadrants. With only a configured timeout it returns 2s. With a caller deadline 500ms in the future it returns a remaining duration at or below 500ms — the demo asserts the bound rather than printing the exact nanoseconds so the output is reproducible. With neither configured nor a header it returns `ok=false`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http"
	"time"

	"example.com/route-timeout"
)

func main() {
	rt := &timeout.RouteTimeout{
		RequestTimeout: 2 * time.Second,
		DeadlineHeader: "X-Request-Deadline",
	}

	// Configured timeout only, no caller deadline.
	req1, _ := http.NewRequest(http.MethodGet, "http://upstream/", nil)
	d, ok := rt.EffectiveTimeout(req1)
	fmt.Printf("configured only: %v ok=%v\n", d, ok)

	// Caller deadline tighter than the configured timeout.
	req2, _ := http.NewRequest(http.MethodGet, "http://upstream/", nil)
	req2.Header.Set("X-Request-Deadline", fmt.Sprintf("%d", time.Now().Add(500*time.Millisecond).UnixNano()))
	d2, ok2 := rt.EffectiveTimeout(req2)
	fmt.Printf("caller deadline tighter: <=500ms? %v ok=%v\n", d2 <= 500*time.Millisecond, ok2)

	// Neither configured nor a caller deadline.
	rt2 := &timeout.RouteTimeout{}
	req3, _ := http.NewRequest(http.MethodGet, "http://upstream/", nil)
	_, ok3 := rt2.EffectiveTimeout(req3)
	fmt.Printf("no config no header: ok=%v\n", ok3)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
configured only: 2s ok=true
caller deadline tighter: <=500ms? true ok=true
no config no header: ok=false
```

### Tests

`TestRouteTimeoutEffectiveTimeout` is a table over the configured-timeout and header-presence axes, including the case where the configured timeout is tighter than the header, the case where the header is tighter, and an expired header that must fall back to the configured value rather than producing a negative duration.

Create `timeout_test.go`:

```go
package timeout

import (
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestRouteTimeoutEffectiveTimeout(t *testing.T) {
	t.Parallel()

	const headerName = "X-Request-Deadline"
	futureNs := time.Now().Add(5 * time.Second).UnixNano()

	cases := []struct {
		name       string
		configured time.Duration
		headerVal  string
		wantOK     bool
	}{
		{"no config no header", 0, "", false},
		{"only configured", 2 * time.Second, "", true},
		{"header tighter", 5 * time.Second, fmt.Sprintf("%d", futureNs), true},
		{"configured tighter", 1 * time.Second, fmt.Sprintf("%d", futureNs), true},
		{"expired deadline", 2 * time.Second, "1", true}, // header expired; falls back to configured
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rt := &RouteTimeout{RequestTimeout: tc.configured, DeadlineHeader: headerName}
			req, _ := http.NewRequest(http.MethodGet, "http://upstream/", nil)
			if tc.headerVal != "" {
				req.Header.Set(headerName, tc.headerVal)
			}
			_, ok := rt.EffectiveTimeout(req)
			if ok != tc.wantOK {
				t.Errorf("EffectiveTimeout ok = %v, want %v", ok, tc.wantOK)
			}
		})
	}
}

func TestRouteTimeoutConfiguredTighterWins(t *testing.T) {
	t.Parallel()

	rt := &RouteTimeout{RequestTimeout: time.Second, DeadlineHeader: "X-Request-Deadline"}
	req, _ := http.NewRequest(http.MethodGet, "http://upstream/", nil)
	req.Header.Set("X-Request-Deadline", fmt.Sprintf("%d", time.Now().Add(10*time.Second).UnixNano()))

	d, ok := rt.EffectiveTimeout(req)
	if !ok || d != time.Second {
		t.Fatalf("EffectiveTimeout = (%v, %v), want (1s, true)", d, ok)
	}
}

func TestRouteTimeoutHeaderTighterWins(t *testing.T) {
	t.Parallel()

	rt := &RouteTimeout{RequestTimeout: 10 * time.Second, DeadlineHeader: "X-Request-Deadline"}
	req, _ := http.NewRequest(http.MethodGet, "http://upstream/", nil)
	req.Header.Set("X-Request-Deadline", fmt.Sprintf("%d", time.Now().Add(500*time.Millisecond).UnixNano()))

	d, ok := rt.EffectiveTimeout(req)
	if !ok || d <= 0 || d > 500*time.Millisecond {
		t.Fatalf("EffectiveTimeout = (%v, %v), want a positive duration <= 500ms", d, ok)
	}
}
```

## Review

The policy is correct when it always returns the tighter of the two bounds and never returns a non-positive duration with `ok=true`. The mistake that matters most is trusting the header blindly: an expired or garbage deadline parsed straight into the timeout produces a zero or negative value that rejects every request, so the implementation only sets `hasDeadline` when `time.Until` is strictly positive. The second mistake is propagating a relative duration instead of an absolute timestamp; a relative value lets each hop re-add its own slack and the end-to-end deadline drifts, whereas an absolute instant converges. The third is conflating "no timeout" with "zero timeout" by returning a bare duration — the boolean second value is what keeps those distinct, and the table test pins `ok` for every quadrant.

## Resources

- [context.WithTimeout](https://pkg.go.dev/context#WithTimeout) — how the returned duration is applied to the outgoing request context.
- [time.Until](https://pkg.go.dev/time#Until) — converting an absolute deadline into the remaining duration.
- [gRPC deadlines and deadline propagation](https://grpc.io/docs/guides/deadlines/) — the canonical treatment of why deadlines, not timeouts, must cross service boundaries.

---

Back to [03-retry-budget.md](03-retry-budget.md) | Next: [05-resilience-transport.md](05-resilience-transport.md)
