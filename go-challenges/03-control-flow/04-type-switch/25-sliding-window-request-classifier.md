# Exercise 25: Classify Requests Against Sliding Window Rate Limit Buckets

**Nivel: Intermedio** — validacion rapida (un test corto).

A production rate limiter almost never enforces a single ceiling. A gateway
typically stacks a tight per-minute bucket per client to stop a runaway
retry loop, a looser per-hour bucket per client to catch sustained abuse
that stays just under the per-minute ceiling, and a per-user bucket that
follows a logical user across every client they call through — a mobile
app, a browser session, and a partner API key all funneling into the same
account. Each of those three checks is backed by the same sliding-window
counting logic but a different bucket key, a different window duration, and
a different limit, so the request classifier has to look at the concrete
shape of the incoming check and route it correctly before the counting
logic ever runs. This module is fully self-contained: its own `go mod init`,
all code inline, its own demo and tests.

## What you'll build

```text
sliding-window-request-classifier/   independent module: example.com/sliding-window-request-classifier
  go.mod                              go 1.24
  slidingwindow.go                    (*Limiter).Allow(req any, now time.Time) (bool, error)
  cmd/
    demo/
      main.go                         a client bursts past a per-minute limit, then recovers
  slidingwindow_test.go                table of cases plus the window-boundary edge
```

- Files: `slidingwindow.go`, `cmd/demo/main.go`, `slidingwindow_test.go`.
- Implement: `(*Limiter).Allow(req any, now time.Time) (bool, error)`,
  type-switching on `PerMinuteRequest`, `PerHourRequest`, and
  `PerUserRequest` to pick each request's bucket key, window, and limit.
- Test: under-limit and over-limit admission, the exact window-boundary
  edge in both directions, independent buckets for the same client across
  tiers, independent buckets across distinct clients, a per-user bucket
  outliving a per-minute one, and an unsupported request type.

Set up the module:

```bash
mkdir -p ~/go-exercises/sliding-window-request-classifier/cmd/demo
cd ~/go-exercises/sliding-window-request-classifier
go mod init example.com/sliding-window-request-classifier
go mod edit -go=1.24
```

A sliding window log is more expensive than a fixed-window counter — it
keeps every admitted timestamp instead of one integer — but it is the only
approach immune to a fixed window's worst failure mode: a client that fires
its entire limit in the last second of one window and again in the first
second of the next effectively bursts through at double the intended rate,
because a fixed window resets its counter to zero on a clock boundary that
has nothing to do with any individual client's request pattern. Storing the
literal timestamps and evicting everything older than `now - window` on
every call makes the count exact at any instant, at the cost of the storage
and the eviction pass. The type switch is what lets one `Limiter` serve all
three tiers without three near-duplicate structs: each `case` supplies the
one thing that differs — the bucket key's prefix, the window length, and
which struct field holds the limit — and the eviction-and-append logic
underneath runs identically for all three. The eviction and the admission
check happen inside the same `mu.Lock()`/`Unlock()` pair specifically so
that two concurrent calls against the same bucket can never both observe
room for one more request and both be admitted past the limit; splitting
"check remaining count" and "append this timestamp" into two separate
critical sections would reopen exactly that race.

Create `slidingwindow.go`:

```go
package slidingwindow

import (
	"fmt"
	"sync"
	"time"
)

// PerMinuteRequest checks a client against a fixed number of calls in the
// preceding 60-second window — the tier that catches a single client
// hammering an endpoint in a tight loop.
type PerMinuteRequest struct {
	ClientID string
	Limit    int
}

// PerHourRequest checks a client against a fixed number of calls in the
// preceding 60-minute window — a coarser tier meant to catch sustained
// abuse that stays just under the per-minute ceiling.
type PerHourRequest struct {
	ClientID string
	Limit    int
}

// PerUserRequest checks a logical user against a fixed number of calls in
// the preceding 24-hour window, independent of which client (browser
// session, API key, mobile build) they call through — the tier that catches
// one user spreading load across many clients to dodge the other two.
type PerUserRequest struct {
	UserID string
	Limit  int
}

// Limiter enforces sliding-window rate limits across three independent
// bucket types. Each bucket keeps the exact timestamps of admitted calls
// inside its window, so the count compared against Limit is always exact —
// unlike a fixed-window counter, which lets a client burst up to 2x its
// limit by timing calls around a window boundary.
type Limiter struct {
	mu      sync.Mutex
	buckets map[string][]time.Time
}

// NewLimiter returns a Limiter ready to use.
func NewLimiter() *Limiter {
	return &Limiter{buckets: make(map[string][]time.Time)}
}

// Allow classifies req by its concrete type to select a bucket key, a
// window duration, and a limit, then evicts timestamps older than the
// window and admits the call only if the remaining count is still under the
// limit. The evict-count-append sequence runs under one lock acquisition,
// so two concurrent calls against the same bucket can never both observe
// room for one more call and both be admitted past the limit — the check
// and the mutation happen in the same critical section, not two.
func (l *Limiter) Allow(req any, now time.Time) (bool, error) {
	var key string
	var window time.Duration
	var limit int

	switch r := req.(type) {
	case PerMinuteRequest:
		key, window, limit = "minute:"+r.ClientID, time.Minute, r.Limit
	case PerHourRequest:
		key, window, limit = "hour:"+r.ClientID, time.Hour, r.Limit
	case PerUserRequest:
		key, window, limit = "user:"+r.UserID, 24*time.Hour, r.Limit
	default:
		return false, fmt.Errorf("slidingwindow: unsupported request type %T", req)
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	cutoff := now.Add(-window)
	existing := l.buckets[key]
	kept := existing[:0]
	for _, t := range existing {
		// A timestamp exactly at cutoff is exactly one window old and has
		// fallen out of the window; only timestamps strictly newer than
		// cutoff still count against the limit.
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= limit {
		l.buckets[key] = kept
		return false, nil
	}
	l.buckets[key] = append(kept, now)
	return true, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/sliding-window-request-classifier"
)

func main() {
	l := slidingwindow.NewLimiter()
	base := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	calls := []struct {
		req any
		at  time.Time
	}{
		{slidingwindow.PerMinuteRequest{ClientID: "mobile-app", Limit: 3}, base},
		{slidingwindow.PerMinuteRequest{ClientID: "mobile-app", Limit: 3}, base.Add(10 * time.Second)},
		{slidingwindow.PerMinuteRequest{ClientID: "mobile-app", Limit: 3}, base.Add(20 * time.Second)},
		{slidingwindow.PerMinuteRequest{ClientID: "mobile-app", Limit: 3}, base.Add(30 * time.Second)},
		{slidingwindow.PerMinuteRequest{ClientID: "mobile-app", Limit: 3}, base.Add(70 * time.Second)},
	}

	for _, c := range calls {
		allowed, err := l.Allow(c.req, c.at)
		if err != nil {
			fmt.Println("error:", err)
			continue
		}
		fmt.Printf("%s -> allowed=%v\n", c.at.Format("15:04:05"), allowed)
	}
}
```

```bash
go run ./cmd/demo
```

Expected output:

```text
12:00:00 -> allowed=true
12:00:10 -> allowed=true
12:00:20 -> allowed=true
12:00:30 -> allowed=false
12:01:10 -> allowed=true
```

The fourth call at `12:00:30` is the third request inside the same 60-second
window as the first three, so it is rejected against a limit of 3. By
`12:01:10`, the first two calls (`12:00:00` and `12:00:10`) have fallen more
than a minute behind and are evicted, leaving only two calls in the window
(`12:00:20` and `12:00:30`), so the fifth call is admitted.

### Tests

The table drives every scenario through fresh `Limiter` values so no
subtest's bucket state leaks into another's; a `setup` callback pre-seeds a
bucket where a test needs history before the call under test.

Create `slidingwindow_test.go`:

```go
package slidingwindow

import (
	"testing"
	"time"
)

func TestAllow(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name  string
		setup func(l *Limiter)
		req   any
		at    time.Time
		want  bool
	}{
		{
			name: "first call under limit is allowed",
			req:  PerMinuteRequest{ClientID: "c1", Limit: 2},
			at:   base,
			want: true,
		},
		{
			name: "third call over a limit of two is rejected",
			setup: func(l *Limiter) {
				l.Allow(PerMinuteRequest{ClientID: "c1", Limit: 2}, base)
				l.Allow(PerMinuteRequest{ClientID: "c1", Limit: 2}, base.Add(time.Second))
			},
			req:  PerMinuteRequest{ClientID: "c1", Limit: 2},
			at:   base.Add(2 * time.Second),
			want: false,
		},
		{
			name: "a timestamp exactly one window old has already fallen out",
			setup: func(l *Limiter) {
				l.Allow(PerMinuteRequest{ClientID: "c1", Limit: 1}, base)
			},
			req:  PerMinuteRequest{ClientID: "c1", Limit: 1},
			at:   base.Add(time.Minute), // exactly 60s later: the first call is no longer in window
			want: true,
		},
		{
			name: "a timestamp just under one window old still counts",
			setup: func(l *Limiter) {
				l.Allow(PerMinuteRequest{ClientID: "c1", Limit: 1}, base)
			},
			req:  PerMinuteRequest{ClientID: "c1", Limit: 1},
			at:   base.Add(time.Minute - time.Millisecond),
			want: false,
		},
		{
			name: "per-hour and per-minute buckets for the same client are independent",
			setup: func(l *Limiter) {
				l.Allow(PerMinuteRequest{ClientID: "c1", Limit: 1}, base)
			},
			req:  PerHourRequest{ClientID: "c1", Limit: 5},
			at:   base.Add(time.Second),
			want: true,
		},
		{
			name: "two distinct clients do not share a bucket",
			setup: func(l *Limiter) {
				l.Allow(PerMinuteRequest{ClientID: "client-a", Limit: 1}, base)
			},
			req:  PerMinuteRequest{ClientID: "client-b", Limit: 1},
			at:   base.Add(time.Second),
			want: true,
		},
		{
			name: "per-user bucket is keyed by user, not by client",
			setup: func(l *Limiter) {
				l.Allow(PerUserRequest{UserID: "u1", Limit: 1}, base)
			},
			req:  PerUserRequest{UserID: "u1", Limit: 1},
			at:   base.Add(time.Hour),
			want: false, // still inside the 24h window
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			l := NewLimiter()
			if tt.setup != nil {
				tt.setup(l)
			}
			got, err := l.Allow(tt.req, tt.at)
			if err != nil {
				t.Fatalf("Allow: unexpected error %v", err)
			}
			if got != tt.want {
				t.Fatalf("Allow(%+v, %s) = %v, want %v", tt.req, tt.at, got, tt.want)
			}
		})
	}

	t.Run("unsupported request type", func(t *testing.T) {
		t.Parallel()
		l := NewLimiter()
		if _, err := l.Allow("not-a-request", base); err == nil {
			t.Fatal("expected error for unsupported request type")
		}
	})
}
```

Verify: `go test -count=1 ./...`

## Review

`Allow` is correct because the eviction pass and the admission check happen
against the same `kept` slice inside the same lock acquisition: nothing
outside `Allow` can observe or mutate a bucket between "how many calls are
still in the window" and "record this one too." The `t.After(cutoff)` guard
is the boundary the test table pins down explicitly — using `!t.Before(cutoff)`
instead would keep a timestamp exactly one window old, silently narrowing
every client's effective window by one tick and making the limiter slightly
stricter than configured. Reusing `existing[:0]` as the backing array for
`kept` is the standard in-place filter idiom: because the write index into
`kept` never exceeds the read index into `existing` at any point in the
loop, overwriting already-read elements ahead of where the range expression
still needs to read from is safe, and it keeps a busy bucket from
reallocating a fresh slice on every single call.

## Resources

- [Go Specification: Type switches](https://go.dev/ref/spec#Type_switches)
- [Stripe API: rate limits](https://stripe.com/docs/rate-limits)
- [Cloudflare: How we scaled rate limiting](https://blog.cloudflare.com/counting-things-a-lot-of-different-things/)
- [sync.Mutex](https://pkg.go.dev/sync#Mutex)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [24-transaction-log-recovery.md](24-transaction-log-recovery.md) | Next: [26-bloom-filter-existence-check.md](26-bloom-filter-existence-check.md)
