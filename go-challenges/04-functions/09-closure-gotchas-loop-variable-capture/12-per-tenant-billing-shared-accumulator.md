# Exercise 12: Per-Tenant Billing Summarizer: Closures Over a Shared Accumulator

**Nivel: Intermedio** — validacion rapida (un test corto).

A billing service builds one running-total closure per tenant so callers can
add metered usage throughout the day and read the current total at any time.
Go 1.22 makes the per-tenant range variable safe to close over, but the
running total itself is a variable YOU declare — and declaring it in the
wrong place quietly merges every tenant's bill into one.

## What you'll build

```text
billing/                     independent module: example.com/billing
  go.mod                     go 1.24
  billing.go                  BuildTenantAccumulators, BuildTenantAccumulatorsBuggy
  billing_test.go             table test: independent totals vs. shared leak
```

- Files: `billing.go`, `billing_test.go`.
- Implement: `BuildTenantAccumulators(tenants) map[string]func(int) int` declaring a fresh total per tenant inside the loop; `BuildTenantAccumulatorsBuggy` declaring one total above the loop, shared by every tenant.
- Test: one table test driving a sequence of calls across two tenants for each variant.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/09-closure-gotchas-loop-variable-capture/12-per-tenant-billing-shared-accumulator
cd go-solutions/04-functions/09-closure-gotchas-loop-variable-capture/12-per-tenant-billing-shared-accumulator
go mod edit -go=1.24
```

### The shared cell is not the loop variable

In `BuildTenantAccumulatorsBuggy`, `tenant` is a well-behaved per-iteration
range variable on a `go 1.24` module — the map key binds correctly. The bug
is `total`, declared ONCE before the loop starts, so every closure the loop
builds shares that SAME `int`. This is intended capture-by-reference (a
stateful closure IS supposed to share state across calls) applied at the
WRONG scope: the state must live per tenant, not per accumulator factory.
`BuildTenantAccumulators` fixes it by moving `total := 0` inside the loop
body, so each tenant gets an independent cell.

Create `billing.go`:

```go
package billing

// BuildTenantAccumulatorsBuggy builds one running-total closure per tenant,
// but declares the accumulator ONCE before the loop instead of once PER
// tenant. `total` lives OUTSIDE the loop body, so every closure shares the
// SAME storage location: adding usage for one tenant silently inflates every
// other tenant's running total.
func BuildTenantAccumulatorsBuggy(tenants []string) map[string]func(delta int) int {
	acc := make(map[string]func(delta int) int, len(tenants))
	total := 0 // BUG: one shared accumulator for every tenant
	for _, tenant := range tenants {
		acc[tenant] = func(delta int) int {
			total += delta
			return total
		}
	}
	return acc
}

// BuildTenantAccumulators builds one running-total closure per tenant, each
// closing over its OWN accumulator declared inside the loop body, so adding
// usage for one tenant never affects another tenant's total.
func BuildTenantAccumulators(tenants []string) map[string]func(delta int) int {
	acc := make(map[string]func(delta int) int, len(tenants))
	for _, tenant := range tenants {
		total := 0 // fresh accumulator per tenant
		acc[tenant] = func(delta int) int {
			total += delta
			return total
		}
	}
	return acc
}
```

### Test

One table test drives the same call sequence — add usage for `acme`, then
`globex`, then `acme` again — against both variants and checks the running
totals each returns.

Create `billing_test.go`:

```go
package billing

import "testing"

func TestTenantAccumulators(t *testing.T) {
	tests := []struct {
		name  string
		build func([]string) map[string]func(int) int
		calls []struct {
			tenant string
			delta  int
			want   int
		}
	}{
		{
			name:  "fresh accumulator per tenant keeps totals independent",
			build: BuildTenantAccumulators,
			calls: []struct {
				tenant string
				delta  int
				want   int
			}{
				{"acme", 10, 10},
				{"globex", 5, 5},
				{"acme", 3, 13},
				{"globex", 0, 5},
			},
		},
		{
			name:  "shared accumulator leaks totals across tenants",
			build: BuildTenantAccumulatorsBuggy,
			calls: []struct {
				tenant string
				delta  int
				want   int
			}{
				{"acme", 10, 10},
				{"globex", 5, 15}, // BUG: shared total, should be 5
				{"acme", 3, 18},   // BUG: still the same shared total
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			acc := tt.build([]string{"acme", "globex"})
			for _, c := range tt.calls {
				if got := acc[c.tenant](c.delta); got != c.want {
					t.Fatalf("%s(%d) = %d, want %d", c.tenant, c.delta, got, c.want)
				}
			}
		})
	}
}
```

Verify: `go test -count=1 ./...`

## Review

The buggy table row is the whole lesson: `globex`'s first call returns `15`,
not `5`, because it is reading the same accumulator `acme` had just written
to. This is the loop-capture family wearing a billing-logic costume —
intended shared mutable state (a running total legitimately persists across
calls) built at the WRONG granularity. The fix is one line: move the
accumulator's declaration from above the loop to inside it, so the loop's
per-iteration semantics — which Go 1.22 already guarantees for the range
variable — extend to every variable the closure needs private per tenant.

## Resources

- [Go spec: For statements with range clause](https://go.dev/ref/spec#For_range) — per-iteration variable semantics since Go 1.22.
- [Go blog: Closures](https://go.dev/tour/moretypes/25) — closures capturing variables, not values.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [11-defer-named-return-shadowed-err.md](11-defer-named-return-shadowed-err.md) | Next: [13-rule-engine-config-pointer-mutation.md](13-rule-engine-config-pointer-mutation.md)
