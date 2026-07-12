# Exercise 31: Hierarchical Quota Token Deduction

**Nivel: Intermedio** — validacion rapida (un test corto).

A multi-tenant API gateway enforces rate limits at several nested levels at
once: an organization-wide budget, a per-user budget inside it, and a
per-endpoint budget inside that. The request that actually gets served is
governed by whichever tier is tightest, and a request that fails at the
innermost tier must not have already spent budget from the outer ones —
otherwise a single blocked user request quietly drains organization-wide
quota for everyone else. This module builds that cascade as two small
loops over the same tier list: one that only checks, one that only
commits.

This module is fully self-contained: its own `go mod init`, one test file,
one runnable demo.

## What you'll build

```text
quota/                         module example.com/quota
  go.mod                       go 1.24
  quota.go                     Tier; Cascade; (*Cascade).Allow(cost) (bool, string)
  quota_test.go                   success deducts all tiers, rejection at tightest tier leaks nothing, first-exhausted-in-order, repeated calls, exact-remaining boundary
  cmd/demo/
    main.go                      four requests against org/user/endpoint tiers, the fourth rejected at the endpoint tier
```

- Files: `quota.go`, `quota_test.go`, `cmd/demo/main.go`.
- Implement: `(*Cascade).Allow(cost int) (bool, string)` — a first `for _, t := range c.Tiers` pass that only checks `t.Remaining >= cost`, returning `false` and the first failing tier's name the instant one is found; a second identical-shaped pass, run only if the first found nothing, that actually deducts `cost` from every tier.
- Test: a successful call deducts from every tier; a rejection at the tightest tier leaves every other tier's `Remaining` completely untouched; when multiple tiers are exhausted, the first one in order is reported; repeated calls exhaust the tightest tier exactly at its configured limit; a request whose cost exactly equals the remaining budget succeeds (not treated as exhaustion).
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/02-for-loops/31-hierarchical-quota-token-cascade/cmd/demo
cd go-solutions/03-control-flow/02-for-loops/31-hierarchical-quota-token-cascade
go mod edit -go=1.24
```

### Why the check and the deduction are two separate loops

The entire correctness argument for `Allow` rests on one property: a
rejected request must have zero side effects on any tier's `Remaining`,
not just the tier that caused the rejection. A single loop that deducts as
it walks the tiers and tries to detect failure partway through would have
to *undo* every deduction already applied to earlier tiers the moment a
later tier comes up short — and that undo logic is itself a loop, running
backward over exactly the tiers already touched, which is more code and
more chances to miscount than simply not touching anything until every
tier has already agreed to proceed. Splitting `Allow` into "first pass
checks, second pass commits" sidesteps the undo problem entirely: nothing
is ever deducted until the first pass has already walked every tier
without finding a shortfall, so there is never a partial deduction to
reverse. This is the same "check current state, then act" discipline that
matters even more under concurrent access (where the check and the act
have to be one atomic critical section to avoid a race) — here, in a
single-threaded cascade, the two-pass split is what makes the *sequential*
version of that discipline visible and testable.

Reporting *which* tier rejected the request, rather than just `false`,
also has a loop-shape consequence: the first pass returns on the first
tier it finds insufficient, in tier order, so if `org`, `user`, and
`endpoint` are all simultaneously exhausted, the caller is told `"org"` —
the outermost, most consequential tier — not whichever tier happened to be
checked last. `TestAllowRejectsAtFirstExhaustedTierInOrder` pins that
ordering down directly.

Create `quota.go`:

```go
package quota

// Tier is one level of a hierarchical rate limit: an organization-wide
// budget, a per-user budget within it, a per-endpoint budget within that,
// and so on. Remaining is the number of tokens left in the current window.
type Tier struct {
	Name      string
	Remaining int
}

// Cascade enforces a request against every configured tier, where the
// effective limit is the minimum remaining budget across all of them: an
// organization with plenty of quota left still rejects a request if the
// caller's own per-user or per-endpoint tier is exhausted.
type Cascade struct {
	Tiers []*Tier
}

// Allow attempts to deduct cost tokens from every tier. It runs two
// separate passes rather than one: the first pass only checks whether every
// tier currently has at least cost remaining, and the second pass performs
// the deduction, and only the second pass ever runs if the first pass found
// no problem. This is what prevents a tier from "leaking" quota -- if the
// third tier out of three is exhausted, the first two must end this call
// with their Remaining completely untouched, because the request as a
// whole did not succeed. A single combined loop that deducts as it goes and
// tries to roll back on failure would have to reverse partial deductions
// exactly, which is extra bookkeeping this two-pass shape avoids entirely.
//
// Allow returns (true, "") on success, or (false, name) naming the first
// tier (in order) that did not have enough remaining budget.
func (c *Cascade) Allow(cost int) (bool, string) {
	for _, t := range c.Tiers {
		if t.Remaining < cost {
			return false, t.Name
		}
	}

	for _, t := range c.Tiers {
		t.Remaining -= cost
	}
	return true, ""
}
```

### The runnable demo

Three tiers start at `org=100`, `user=5`, `endpoint=3`. Four requests of
cost 1 each are made; the fourth is rejected because the endpoint tier ran
out first, and `org`/`user` show no further deduction on that call.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/quota"
)

func main() {
	c := &quota.Cascade{
		Tiers: []*quota.Tier{
			{Name: "org", Remaining: 100},
			{Name: "user", Remaining: 5},
			{Name: "endpoint", Remaining: 3},
		},
	}

	for i := 1; i <= 4; i++ {
		ok, rejectedTier := c.Allow(1)
		fmt.Printf("request %d: allowed=%v rejectedTier=%q org=%d user=%d endpoint=%d\n",
			i, ok, rejectedTier, c.Tiers[0].Remaining, c.Tiers[1].Remaining, c.Tiers[2].Remaining)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
request 1: allowed=true rejectedTier="" org=99 user=4 endpoint=2
request 2: allowed=true rejectedTier="" org=98 user=3 endpoint=1
request 3: allowed=true rejectedTier="" org=97 user=2 endpoint=0
request 4: allowed=false rejectedTier="endpoint" org=97 user=2 endpoint=0
```

### Tests

`TestAllowDeductsFromEveryTierOnSuccess` checks the ordinary success path.
`TestAllowRejectsAtTheTightestTierWithoutLeakingQuota` is the core
guarantee: draining only the innermost tier and confirming the outer two
are byte-for-byte unchanged after a rejected call.
`TestAllowRejectsAtFirstExhaustedTierInOrder` pins the reporting order
when several tiers are simultaneously exhausted.
`TestAllowRepeatedCallsExhaustTheTightestTier` drives the cascade through
several real calls end to end. `TestAllowExactRemainingIsSufficientNotExhausted`
checks the boundary where `cost == Remaining` must still succeed.

Create `quota_test.go`:

```go
package quota

import "testing"

func newCascade() *Cascade {
	return &Cascade{
		Tiers: []*Tier{
			{Name: "org", Remaining: 100},
			{Name: "user", Remaining: 5},
			{Name: "endpoint", Remaining: 3},
		},
	}
}

func TestAllowDeductsFromEveryTierOnSuccess(t *testing.T) {
	t.Parallel()

	c := newCascade()

	ok, tier := c.Allow(1)
	if !ok || tier != "" {
		t.Fatalf("Allow() = %v, %q; want true, \"\"", ok, tier)
	}
	if c.Tiers[0].Remaining != 99 || c.Tiers[1].Remaining != 4 || c.Tiers[2].Remaining != 2 {
		t.Fatalf("remaining = %d, %d, %d; want 99, 4, 2",
			c.Tiers[0].Remaining, c.Tiers[1].Remaining, c.Tiers[2].Remaining)
	}
}

func TestAllowRejectsAtTheTightestTierWithoutLeakingQuota(t *testing.T) {
	t.Parallel()

	c := newCascade()

	// Drain the endpoint tier down to exactly cost-1 remaining so the next
	// call must be rejected there, while org and user still have plenty.
	c.Tiers[2].Remaining = 0

	ok, tier := c.Allow(1)
	if ok {
		t.Fatal("Allow() = true, want false")
	}
	if tier != "endpoint" {
		t.Fatalf("rejecting tier = %q, want %q", tier, "endpoint")
	}
	if c.Tiers[0].Remaining != 100 || c.Tiers[1].Remaining != 5 {
		t.Fatalf("org/user remaining = %d, %d; want 100, 5 (no leak on rejection)",
			c.Tiers[0].Remaining, c.Tiers[1].Remaining)
	}
}

func TestAllowRejectsAtFirstExhaustedTierInOrder(t *testing.T) {
	t.Parallel()

	c := &Cascade{
		Tiers: []*Tier{
			{Name: "org", Remaining: 0},
			{Name: "user", Remaining: 0},
		},
	}

	ok, tier := c.Allow(1)
	if ok {
		t.Fatal("Allow() = true, want false")
	}
	if tier != "org" {
		t.Fatalf("rejecting tier = %q, want %q (org is checked first)", tier, "org")
	}
}

func TestAllowRepeatedCallsExhaustTheTightestTier(t *testing.T) {
	t.Parallel()

	c := newCascade() // endpoint starts at 3

	for i := 0; i < 3; i++ {
		ok, tier := c.Allow(1)
		if !ok {
			t.Fatalf("call %d: Allow() = false, %q; want true", i, tier)
		}
	}

	ok, tier := c.Allow(1)
	if ok {
		t.Fatal("4th call: Allow() = true, want false (endpoint tier exhausted)")
	}
	if tier != "endpoint" {
		t.Fatalf("4th call rejecting tier = %q, want %q", tier, "endpoint")
	}
	if c.Tiers[0].Remaining != 97 || c.Tiers[1].Remaining != 2 || c.Tiers[2].Remaining != 0 {
		t.Fatalf("remaining after 3 successes + 1 rejection = %d, %d, %d; want 97, 2, 0",
			c.Tiers[0].Remaining, c.Tiers[1].Remaining, c.Tiers[2].Remaining)
	}
}

func TestAllowExactRemainingIsSufficientNotExhausted(t *testing.T) {
	t.Parallel()

	c := &Cascade{Tiers: []*Tier{{Name: "org", Remaining: 5}}}

	ok, _ := c.Allow(5)
	if !ok {
		t.Fatal("Allow(5) with Remaining=5 = false, want true (exact match is not exhaustion)")
	}
	if c.Tiers[0].Remaining != 0 {
		t.Fatalf("Remaining = %d, want 0", c.Tiers[0].Remaining)
	}
}
```

## Review

`Allow` is correct when a rejected call leaves every tier's `Remaining`
identical to what it was before the call, and a successful call deducts
`cost` from every tier exactly once. The common mistake this design avoids
is a single-pass version that deducts from each tier as it goes and
returns `false` the moment a shortfall is found — that leaks quota from
every tier checked *before* the failing one, and the bug only shows up
under a specific ordering of tiers and a specific exhaustion pattern,
which is exactly the kind of thing that passes a quick manual smoke test
and then causes a slow, silent quota leak in production.
`TestAllowRejectsAtTheTightestTierWithoutLeakingQuota` is written
specifically to catch that. Run `go test -count=1 ./...`.

## Resources

- [Stripe API rate limits](https://stripe.com/blog/rate-limiters) — a production description of layered, hierarchical rate limiting.
- [Go Specification: For statements](https://go.dev/ref/spec#For_statements) — the two sequential range loops over the same tier slice.
- [The Token Bucket Algorithm (Wikipedia)](https://en.wikipedia.org/wiki/Token_bucket) — the per-tier budget model each `Tier.Remaining` represents.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [30-write-ahead-log-compaction-snapshot.md](30-write-ahead-log-compaction-snapshot.md) | Next: [32-dns-discovery-ttl-cache-refresh.md](32-dns-discovery-ttl-cache-refresh.md)
