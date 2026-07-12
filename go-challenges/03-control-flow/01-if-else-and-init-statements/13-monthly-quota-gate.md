# Exercise 13: Monthly Quota Gate: One Guard Over a Usage Map

**Nivel: Intermedio** — validacion rapida (un test corto).

A billing-adjacent API needs to reject a tenant's call once they hit their
monthly cap, and admit it (and record it) otherwise. This module builds
that as a single `if` with an init statement over a plain usage map: a
comma-ok read decides whether the tenant is over limit, and every admitted
call increments the same map that guards the next one.

## What you'll build

```text
quotagate/                  independent module: example.com/monthly-quota-gate
  go.mod                    go 1.24
  quota.go                  Allow(tenant, usage, limit) bool
  quota_test.go             sequential table over one shared usage map
```

- Files: `quota.go`, `quota_test.go`.
- Implement: `Allow(tenant string, usage map[string]int, limit int) bool` using `if used, ok := usage[tenant]; ok && used >= limit { return false }`, then incrementing `usage[tenant]` and returning `true` on the admitted path.
- Test: a sequential table driving one shared `usage` map through a new tenant's first calls, a rejection once the tenant reaches the limit, and a second tenant whose count stays independent of the first.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/01-if-else-and-init-statements/13-monthly-quota-gate
cd go-solutions/03-control-flow/01-if-else-and-init-statements/13-monthly-quota-gate
go mod edit -go=1.24
```

### Why `ok &&` matters, not just `used >= limit`

`usage[tenant]` on a brand-new tenant returns the zero value, `0`, and `0
>= limit` is false for any positive limit — so in this exercise a plain
`used >= limit` would happen to work for the common case. But it hides the
real question the guard is answering: is this tenant's count meaningful at
all? The comma-ok form makes that explicit and correct if the guard is
ever extended (a tenant explicitly reset to a count *at* the limit, for
example, must still be rejected, and `ok` staying true is what keeps that
case correct). A guard that reads `ok && used >= limit` documents its own
precondition instead of relying on a coincidence of the zero value.

Create `quota.go`:

```go
// Package quotagate enforces a per-tenant monthly usage cap over a plain map,
// the shape a billing-adjacent API uses before a call is allowed to run.
package quotagate

// Allow reports whether tenant may make one more call against limit, and
// records the call if so. usage holds each tenant's count for the current
// billing period; a tenant absent from usage is treated as zero calls so
// far, which the comma-ok read distinguishes from a tenant already at zero
// calls used (both read as "not over limit," but only presence tells you
// whether this is the tenant's first call of the period).
func Allow(tenant string, usage map[string]int, limit int) bool {
	if used, ok := usage[tenant]; ok && used >= limit {
		return false
	}

	usage[tenant]++
	return true
}
```

### Tests

The table runs the same shared `usage` map through a sequence of calls in
order — a new tenant's first two calls, an unrelated tenant's first call
interleaved in, the original tenant reaching then exceeding its limit, a
repeated rejection that must not keep incrementing the stored count, and a
final call proving the second tenant's count never moved.

Create `quota_test.go`:

```go
package quotagate

import "testing"

func TestAllow(t *testing.T) {
	t.Parallel()

	usage := make(map[string]int)
	const limit = 3

	steps := []struct {
		name      string
		tenant    string
		wantAllow bool
		wantUsage int
	}{
		{"first call for a brand new tenant is allowed", "acme", true, 1},
		{"second call for acme is allowed", "acme", true, 2},
		{"a different tenant starts its own independent count", "globex", true, 1},
		{"third call for acme reaches the limit and is still allowed", "acme", true, 3},
		{"fourth call for acme exceeds the limit and is rejected", "acme", false, 3},
		{"a rejected call does not bump the stored count further", "acme", false, 3},
		{"globex is unaffected by acme's rejections", "globex", true, 2},
	}

	for _, tc := range steps {
		got := Allow(tc.tenant, usage, limit)
		if got != tc.wantAllow {
			t.Errorf("%s: Allow(%q) = %v, want %v", tc.name, tc.tenant, got, tc.wantAllow)
		}
		if usage[tc.tenant] != tc.wantUsage {
			t.Errorf("%s: usage[%q] = %d, want %d", tc.name, tc.tenant, usage[tc.tenant], tc.wantUsage)
		}
	}
}
```

Verify: `go test -count=1 ./...`

## Review

Running the table sequentially over one shared map, instead of giving each
case a fresh map, is what actually exercises the gate: the fourth case
only fails correctly because the first three calls already moved `acme`'s
stored count, and the rejection cases prove the guard does not increment
on the failure path. Carry this forward: a decision that both reads and
mutates shared state needs its test to drive a *sequence* against one
instance, not isolated single-shot cases.

## Resources

- [Go Specification: Index expressions](https://go.dev/ref/spec#Index_expressions) — the two-result form of a map index and what `ok` means.
- [Stripe billing docs: Usage-based billing](https://stripe.com/docs/billing/subscriptions/usage-based) — the production shape of a per-tenant usage cap.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [12-tiered-cache-fallback.md](12-tiered-cache-fallback.md) | Next: [14-accept-header-negotiation.md](14-accept-header-negotiation.md)
