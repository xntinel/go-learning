# Exercise 34: Enforce Cascading Quota Limits Across Org/User/Endpoint Tiers

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A multi-tenant API rarely enforces just one quota. An organization has a
total call ceiling its contract entitles it to; within that org, an
individual user has their own monthly allowance so one heavy user cannot
silently consume the whole org's budget; and a single hot endpoint has its
own concurrency ceiling so one slow downstream dependency cannot let
unlimited concurrent calls pile up against it regardless of whether the
org or the user still have quota left. These three tiers are not
independent — every call an endpoint claim makes also consumes org and
user budget — and different operations in the same API need different
subsets of that cascade: a bulk export job might only be billed at the org
level, while a synchronous request needs all three checks and, uniquely
among them, needs to give back the one resource it is actually holding
open. This module is fully self-contained: its own `go mod init`, all code
inline, its own demo and tests.

## What you'll build

```text
hierarchical-quota-token-enforcement/   independent module: example.com/hierarchical-quota-token-enforcement
  go.mod                                go 1.24
  quotacascade.go                       (*Enforcer).Claim(req any) (*Token, error); Backoff(err error, now time.Time) time.Duration
  cmd/
    demo/
      main.go                           an endpoint slot is claimed, saturates, releases, and reclaims
  quotacascade_test.go                    table of cascade cases, backoff mapping, and a concurrency test
```

- Files: `quotacascade.go`, `cmd/demo/main.go`, `quotacascade_test.go`.
- Implement: `(*Enforcer).Claim(req any) (*Token, error)`, type-switching
  on `OrgClaim`, `UserClaim`, and `EndpointClaim` to decide how far up the
  hierarchy to cascade; `Backoff(err error, now time.Time) time.Duration`,
  type-switching on the rejection to pick a tier-appropriate retry
  strategy.
- Test: org tier exhaustion rejecting before user or endpoint are ever
  checked, user tier exhaustion rejecting despite org headroom, endpoint
  tier saturation rejecting despite org and user headroom, an exact quota
  boundary, a released token being reclaimable, a nil or double-released
  token being a safe no-op, an unsupported claim type, the backoff mapping
  per rejection kind, and many goroutines racing to claim the same
  endpoint's concurrency slots without ever exceeding capacity.

Set up the module:

```bash
go mod edit -go=1.24
```

The three claim types do not just carry different fields — they encode
different *cascade depths*. `OrgClaim` checks only the org ceiling because
some operations genuinely have no per-user or per-endpoint meaning at all;
`UserClaim` cascades the org check and adds the user's monthly ceiling;
`EndpointClaim` cascades both and adds the endpoint's live concurrency
ceiling. The type switch is what encodes "how far to cascade" as a
property of the request's own shape rather than as a runtime flag a caller
could get wrong, and every case checks tiers in the same fixed order —
broadest first — so that an org already at its ceiling fails fast without
the enforcer ever bothering to look up a user or endpoint that will never
matter for this call. The org and user tiers are monotonic counters:
`orgUsed` and `userUsed` only ever go up, because they represent calls
already made and billed, and there is no sense in which a finished call
gives back budget it already consumed. The endpoint tier is different in
kind — `endpointInFlight` is a live gauge of calls currently in progress,
which is why only `EndpointClaim` returns a `*Token` at all, and why
`Token.Release` exists: it gives back the one resource that was ever a
*loan* rather than a *charge*. `Token.Release` is deliberately safe on a
nil receiver and safe to call twice, because `OrgClaim` and `UserClaim`
return a nil token by design, and a caller that unconditionally defers
`token.Release()` after every claim — the natural pattern — should never
need to special-case which claim kind it just made.

Create `quotacascade.go`:

```go
package quotacascade

import (
	"fmt"
	"sync"
	"time"
)

// OrgQuotaExceeded means the org's total call ceiling has been reached.
type OrgQuotaExceeded struct {
	OrgID string
	Limit int64
}

func (e OrgQuotaExceeded) Error() string {
	return fmt.Sprintf("org %s exhausted its total quota of %d calls", e.OrgID, e.Limit)
}

// UserQuotaExceeded means the user's monthly call ceiling has been reached.
type UserQuotaExceeded struct {
	UserID   string
	Limit    int64
	ResetsAt time.Time
}

func (e UserQuotaExceeded) Error() string {
	return fmt.Sprintf("user %s exhausted its monthly quota of %d calls, resets %s", e.UserID, e.Limit, e.ResetsAt.Format(time.RFC3339))
}

// EndpointSaturated means the endpoint is already serving its maximum
// number of concurrent in-flight calls.
type EndpointSaturated struct {
	Endpoint      string
	MaxConcurrent int
}

func (e EndpointSaturated) Error() string {
	return fmt.Sprintf("endpoint %s is at its concurrency limit of %d", e.Endpoint, e.MaxConcurrent)
}

// OrgClaim checks only the org-wide ceiling — used for an operation billed
// at the org level with no per-user or per-endpoint concept, such as a bulk
// export job.
type OrgClaim struct{ OrgID string }

// UserClaim cascades the org check and adds the user's monthly ceiling —
// used for an operation attributable to one user but not tied to any
// single endpoint's concurrency limit, such as enqueuing an async job.
type UserClaim struct {
	OrgID  string
	UserID string
}

// EndpointClaim cascades both the org and user checks and adds the
// endpoint's live concurrency ceiling — used for a synchronous API call,
// which is the only claim kind that actually holds a resource open until
// released.
type EndpointClaim struct {
	OrgID    string
	UserID   string
	Endpoint string
}

// Token represents a held endpoint-concurrency slot. Only EndpointClaim
// grants one, because only the endpoint tier is a live gauge that must be
// given back; the org and user tiers are monotonic counters of calls
// already consumed, which are never released.
type Token struct {
	enforcer *Enforcer
	endpoint string
	released bool
}

// Release gives back the concurrency slot this token holds. It is safe to
// call on a nil Token (the case for OrgClaim and UserClaim, which hold no
// resource) and safe to call more than once.
func (t *Token) Release() {
	if t == nil {
		return
	}
	t.enforcer.mu.Lock()
	defer t.enforcer.mu.Unlock()
	if t.released {
		return
	}
	t.released = true
	t.enforcer.endpointInFlight[t.endpoint]--
}

// Enforcer holds the three cascading quota tiers. All state is guarded by
// mu so Claim is safe to call concurrently from many request-handling
// goroutines at once, as a real API gateway must.
type Enforcer struct {
	mu                sync.Mutex
	orgLimits         map[string]int64
	orgUsed           map[string]int64
	userMonthlyLimits map[string]int64
	userUsed          map[string]int64
	endpointMax       map[string]int
	endpointInFlight  map[string]int
	monthReset        time.Time
}

// NewEnforcer returns an Enforcer with no configured limits. Call
// SetOrgLimit, SetUserMonthlyLimit, and SetEndpointMax to configure each
// tier before claiming against it; an unconfigured org, user, or endpoint
// has a limit of zero and rejects every claim.
func NewEnforcer(monthReset time.Time) *Enforcer {
	return &Enforcer{
		orgLimits:         make(map[string]int64),
		orgUsed:           make(map[string]int64),
		userMonthlyLimits: make(map[string]int64),
		userUsed:          make(map[string]int64),
		endpointMax:       make(map[string]int),
		endpointInFlight:  make(map[string]int),
		monthReset:        monthReset,
	}
}

// SetOrgLimit configures orgID's total call ceiling.
func (e *Enforcer) SetOrgLimit(orgID string, limit int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.orgLimits[orgID] = limit
}

// SetUserMonthlyLimit configures userID's calls-per-month ceiling.
func (e *Enforcer) SetUserMonthlyLimit(userID string, limit int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.userMonthlyLimits[userID] = limit
}

// SetEndpointMax configures endpoint's maximum concurrent in-flight calls.
func (e *Enforcer) SetEndpointMax(endpoint string, max int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.endpointMax[endpoint] = max
}

// Claim dispatches req by its concrete type to decide how far up the
// hierarchy to cascade. Every case checks from the broadest tier down to
// the narrowest it needs, in the same fixed order — org, then user, then
// endpoint — so that exhausting the org-wide ceiling always fails fast
// before the enforcer bothers computing anything about the user or
// endpoint tiers beneath it.
func (e *Enforcer) Claim(req any) (*Token, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	switch c := req.(type) {
	case OrgClaim:
		if err := e.checkOrg(c.OrgID); err != nil {
			return nil, err
		}
		e.orgUsed[c.OrgID]++
		return nil, nil

	case UserClaim:
		if err := e.checkOrg(c.OrgID); err != nil {
			return nil, err
		}
		if err := e.checkUser(c.UserID); err != nil {
			return nil, err
		}
		e.orgUsed[c.OrgID]++
		e.userUsed[c.UserID]++
		return nil, nil

	case EndpointClaim:
		if err := e.checkOrg(c.OrgID); err != nil {
			return nil, err
		}
		if err := e.checkUser(c.UserID); err != nil {
			return nil, err
		}
		if err := e.checkEndpoint(c.Endpoint); err != nil {
			return nil, err
		}
		e.orgUsed[c.OrgID]++
		e.userUsed[c.UserID]++
		e.endpointInFlight[c.Endpoint]++
		return &Token{enforcer: e, endpoint: c.Endpoint}, nil

	default:
		return nil, fmt.Errorf("quotacascade: unsupported claim type %T", req)
	}
}

func (e *Enforcer) checkOrg(orgID string) error {
	if e.orgUsed[orgID] >= e.orgLimits[orgID] {
		return OrgQuotaExceeded{OrgID: orgID, Limit: e.orgLimits[orgID]}
	}
	return nil
}

func (e *Enforcer) checkUser(userID string) error {
	if e.userUsed[userID] >= e.userMonthlyLimits[userID] {
		return UserQuotaExceeded{UserID: userID, Limit: e.userMonthlyLimits[userID], ResetsAt: e.monthReset}
	}
	return nil
}

func (e *Enforcer) checkEndpoint(endpoint string) error {
	if e.endpointInFlight[endpoint] >= e.endpointMax[endpoint] {
		return EndpointSaturated{Endpoint: endpoint, MaxConcurrent: e.endpointMax[endpoint]}
	}
	return nil
}

// Backoff type-switches on a rejection from Claim to pick a backoff
// duration appropriate to which tier failed. An endpoint saturated by
// concurrent load frees up in milliseconds, so a short retry is
// productive; a user over their monthly quota will not succeed again until
// the reset, so the backoff is however long that takes; an org that has
// exhausted its total quota needs a capacity change, not a retry, so no
// backoff duration is meaningful at all.
func Backoff(err error, now time.Time) time.Duration {
	switch e := err.(type) {
	case EndpointSaturated:
		return 50 * time.Millisecond
	case UserQuotaExceeded:
		return e.ResetsAt.Sub(now)
	case OrgQuotaExceeded:
		return -1
	default:
		return 0
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/hierarchical-quota-token-enforcement"
)

func main() {
	monthReset := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	e := quotacascade.NewEnforcer(monthReset)
	e.SetOrgLimit("acme", 5)
	e.SetUserMonthlyLimit("alice", 2)
	e.SetEndpointMax("checkout", 1)

	claims := []any{
		quotacascade.EndpointClaim{OrgID: "acme", UserID: "alice", Endpoint: "checkout"},
		quotacascade.EndpointClaim{OrgID: "acme", UserID: "alice", Endpoint: "checkout"}, // endpoint slot still held: saturated
	}

	var held *quotacascade.Token
	for i, c := range claims {
		token, err := e.Claim(c)
		if err != nil {
			fmt.Printf("claim %d rejected: %v (backoff %s)\n", i, err, quotacascade.Backoff(err, monthReset.Add(-time.Hour)))
			continue
		}
		fmt.Printf("claim %d granted\n", i)
		held = token
	}

	held.Release()
	fmt.Println("released the held endpoint slot")

	token, err := e.Claim(quotacascade.EndpointClaim{OrgID: "acme", UserID: "alice", Endpoint: "checkout"})
	if err != nil {
		fmt.Println("claim after release rejected:", err)
	} else {
		fmt.Println("claim after release granted")
		token.Release()
	}

	// alice has now used 2 of her monthly quota of 2.
	_, err = e.Claim(quotacascade.UserClaim{OrgID: "acme", UserID: "alice"})
	fmt.Println("user-tier claim after monthly quota spent:", err)
}
```

```bash
go run ./cmd/demo
```

Expected output:

```text
claim 0 granted
claim 1 rejected: endpoint checkout is at its concurrency limit of 1 (backoff 50ms)
released the held endpoint slot
claim after release granted
user-tier claim after monthly quota spent: user alice exhausted its monthly quota of 2 calls, resets 2026-08-01T00:00:00Z
```

The second `EndpointClaim` is rejected purely on endpoint saturation — org
and user still have headroom — because the first token is still held.
After releasing it and reclaiming the slot once more, alice has now used
both calls of her monthly quota of 2, so a further `UserClaim` (which
cascades only the org and user checks) correctly reports the user tier
exhausted, independent of the endpoint tier entirely.

### Tests

Create `quotacascade_test.go`:

```go
package quotacascade

import (
	"sync"
	"testing"
	"time"
)

var monthReset = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

func newTestEnforcer() *Enforcer {
	e := NewEnforcer(monthReset)
	e.SetOrgLimit("org1", 5)
	e.SetUserMonthlyLimit("user1", 3)
	e.SetEndpointMax("ep1", 2)
	return e
}

func TestClaimCascadesThroughTiers(t *testing.T) {
	t.Parallel()

	t.Run("org tier exhausted rejects before checking user or endpoint", func(t *testing.T) {
		t.Parallel()
		e := newTestEnforcer()
		e.SetOrgLimit("org1", 0)
		_, err := e.Claim(EndpointClaim{OrgID: "org1", UserID: "user1", Endpoint: "ep1"})
		if _, ok := err.(OrgQuotaExceeded); !ok {
			t.Fatalf("err = %v (%T), want OrgQuotaExceeded", err, err)
		}
	})

	t.Run("user tier exhausted rejects even though org has room", func(t *testing.T) {
		t.Parallel()
		e := newTestEnforcer()
		e.SetUserMonthlyLimit("user1", 0)
		_, err := e.Claim(EndpointClaim{OrgID: "org1", UserID: "user1", Endpoint: "ep1"})
		if _, ok := err.(UserQuotaExceeded); !ok {
			t.Fatalf("err = %v (%T), want UserQuotaExceeded", err, err)
		}
	})

	t.Run("endpoint tier exhausted rejects even though org and user have room", func(t *testing.T) {
		t.Parallel()
		e := newTestEnforcer()
		e.SetEndpointMax("ep1", 0)
		_, err := e.Claim(EndpointClaim{OrgID: "org1", UserID: "user1", Endpoint: "ep1"})
		if _, ok := err.(EndpointSaturated); !ok {
			t.Fatalf("err = %v (%T), want EndpointSaturated", err, err)
		}
	})

	t.Run("org claim at the exact limit boundary is rejected, one under is granted", func(t *testing.T) {
		t.Parallel()
		e := newTestEnforcer()
		e.SetOrgLimit("org1", 1)
		if _, err := e.Claim(OrgClaim{OrgID: "org1"}); err != nil {
			t.Fatalf("first claim: unexpected error %v", err)
		}
		if _, err := e.Claim(OrgClaim{OrgID: "org1"}); err == nil {
			t.Fatal("second claim at the limit boundary should be rejected")
		}
	})

	t.Run("user claim cascades the org check", func(t *testing.T) {
		t.Parallel()
		e := newTestEnforcer()
		token, err := e.Claim(UserClaim{OrgID: "org1", UserID: "user1"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if token != nil {
			t.Fatal("UserClaim should not hold a releasable resource")
		}
	})

	t.Run("endpoint claim grants a token that can be released and reclaimed", func(t *testing.T) {
		t.Parallel()
		e := newTestEnforcer()
		e.SetEndpointMax("ep1", 1)
		token, err := e.Claim(EndpointClaim{OrgID: "org1", UserID: "user1", Endpoint: "ep1"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, err := e.Claim(EndpointClaim{OrgID: "org1", UserID: "user1", Endpoint: "ep1"}); err == nil {
			t.Fatal("expected saturation while the first token is still held")
		}
		token.Release()
		if _, err := e.Claim(EndpointClaim{OrgID: "org1", UserID: "user1", Endpoint: "ep1"}); err != nil {
			t.Fatalf("expected the released slot to be reclaimable, got %v", err)
		}
	})

	t.Run("releasing a nil token and double-releasing are both safe no-ops", func(t *testing.T) {
		t.Parallel()
		var nilToken *Token
		nilToken.Release()
		e := newTestEnforcer()
		token, err := e.Claim(EndpointClaim{OrgID: "org1", UserID: "user1", Endpoint: "ep1"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		token.Release()
		token.Release() // second release must not double-decrement
		if e.endpointInFlight["ep1"] != 0 {
			t.Fatalf("endpointInFlight = %d, want 0", e.endpointInFlight["ep1"])
		}
	})

	t.Run("unsupported claim type is an error", func(t *testing.T) {
		t.Parallel()
		e := newTestEnforcer()
		if _, err := e.Claim("not-a-claim"); err == nil {
			t.Fatal("expected error for unsupported claim type")
		}
	})
}

func TestBackoff(t *testing.T) {
	t.Parallel()
	now := monthReset.Add(-time.Hour)

	tests := []struct {
		name string
		err  error
		want time.Duration
	}{
		{"endpoint saturation backs off briefly", EndpointSaturated{Endpoint: "ep1", MaxConcurrent: 2}, 50 * time.Millisecond},
		{"user quota backs off until the monthly reset", UserQuotaExceeded{UserID: "user1", Limit: 3, ResetsAt: monthReset}, time.Hour},
		{"org quota has no useful backoff", OrgQuotaExceeded{OrgID: "org1", Limit: 5}, -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := Backoff(tt.err, now)
			if got != tt.want {
				t.Fatalf("Backoff(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestConcurrentEndpointClaimsNeverExceedCapacity fires many goroutines at
// one endpoint whose concurrency ceiling is far below the number of
// attempts, and proves the number of simultaneously held tokens never
// exceeds that ceiling — the property the mutex inside Claim exists to
// guarantee under real concurrent request handling.
func TestConcurrentEndpointClaimsNeverExceedCapacity(t *testing.T) {
	e := NewEnforcer(monthReset)
	e.SetOrgLimit("org1", 10000)
	e.SetUserMonthlyLimit("user1", 10000)
	e.SetEndpointMax("checkout", 3)

	const attempts = 30
	var mu sync.Mutex
	current, maxObserved := 0, 0

	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			token, err := e.Claim(EndpointClaim{OrgID: "org1", UserID: "user1", Endpoint: "checkout"})
			if err != nil {
				return // saturated: this attempt simply gets no slot
			}

			mu.Lock()
			current++
			if current > maxObserved {
				maxObserved = current
			}
			mu.Unlock()

			time.Sleep(time.Millisecond) // hold the slot briefly so contention is real

			mu.Lock()
			current--
			mu.Unlock()
			token.Release()
		}(i)
	}
	wg.Wait()

	if maxObserved > 3 {
		t.Fatalf("observed %d concurrent claims, want <= 3", maxObserved)
	}
	if maxObserved == 0 {
		t.Fatal("no claim ever succeeded; test is not exercising contention")
	}
}
```

Verify: `go test -race -count=1 ./...`

## Review

`Claim` is correct because the org, user, and endpoint checks run in the
same fixed order in every case that needs more than one of them, so a
tenant exhausted at the org level is always told about the org, never
about a user or endpoint tier the enforcer only got to because it checked
in an inconsistent order across call sites. `Token.Release`'s nil-safety
is what makes `defer token.Release()` a pattern callers can use
unconditionally after any successful claim, regardless of which of the
three claim types they made — without it, every caller would need its own
type switch just to know whether calling `Release` is even safe, which
defeats the purpose of returning a token at all. The concurrency test is
the one that would catch the bug a purely sequential test suite cannot: an
`Enforcer` that checked `endpointInFlight` and incremented it in two
separate lock acquisitions, instead of one, would pass every table-driven
case in this file while still letting `maxObserved` exceed the configured
ceiling under real concurrent load, because two goroutines could both read
"one slot free" before either one gets to increment it.

## Resources

- [Stripe API: rate limits](https://stripe.com/docs/rate-limits)
- [Google Cloud: Quotas and limits overview](https://cloud.google.com/docs/quotas/overview)
- [Go Specification: Type switches](https://go.dev/ref/spec#Type_switches)
- [sync.Mutex](https://pkg.go.dev/sync#Mutex)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [33-watermark-stream-processor.md](33-watermark-stream-processor.md) | Next: [../05-range-over-collections/00-concepts.md](../05-range-over-collections/00-concepts.md)
