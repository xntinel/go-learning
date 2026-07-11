# Exercise 21: Multi-Tenant Request Router With Isolation Level Options

**Nivel: Intermedio** — validacion rapida (un test corto).

A multi-tenant router promises an isolation level — shared, a dedicated
database, a dedicated network, or both — but a promise is only as good as
the resources actually behind it. This module builds the router through
options, checking that a claimed dedicated resource was genuinely
provisioned, and that every tenant's individual quota still fits inside the
router's total budget.

## What you'll build

```text
tenantrouter/                    independent module: example.com/tenantrouter
  go.mod                         go 1.24
  tenantrouter.go                IsolationLevel, Router, Option, New,
                                  WithIsolationLevel, WithDedicatedDB,
                                  WithDedicatedNetwork, WithTotalQuota,
                                  WithTenantQuota, Route, TenantQuota
  cmd/
    demo/
      main.go                    a provisioned dedicated DB, an unprovisioned claim, an oversubscribed quota
  tenantrouter_test.go            table test over isolation/provisioning and quota combos
```

- Files: `tenantrouter.go`, `cmd/demo/main.go`, `tenantrouter_test.go`.
- Implement: `New(opts ...Option) (*Router, error)` whose `Route` reflects the configured isolation level, validating that any claimed dedicated resource was actually provisioned and that tenant quotas never exceed the total.
- Test: every isolation-level/provisioning combination and every quota combination, including the exact boundary where quotas sum to the total.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/tenantrouter/cmd/demo
cd ~/go-exercises/tenantrouter
go mod init example.com/tenantrouter
go mod edit -go=1.24
```

`WithIsolationLevel` and `WithDedicatedDB`/`WithDedicatedNetwork` are
independent options — a caller can claim `DedicatedDB` isolation and simply
forget to call `WithDedicatedDB`, or provision a dedicated DB under
`Shared` isolation where it does nothing. The first is the dangerous
mistake: a router that believes it has tenant isolation it does not
actually have. `New` checks, after every option has run, that whichever
dedicated resources the isolation level requires were provisioned — proven
by a non-empty DSN or network ID, not just a flag. Separately,
`WithTenantQuota` can be called once per tenant, and no single call knows
what every other tenant's quota adds up to; only the constructor, summing
across the whole map, can catch the total exceeding the router's budget.

Create `tenantrouter.go`:

```go
package tenantrouter

import "fmt"

// IsolationLevel selects how strongly a tenant's traffic is separated from
// every other tenant's.
type IsolationLevel int

const (
	// Shared routes every tenant through the same DB and network path.
	Shared IsolationLevel = iota
	// DedicatedDB gives the tenant its own database, sharing the network.
	DedicatedDB
	// DedicatedNetwork gives the tenant its own network path, sharing the DB.
	DedicatedNetwork
	// DedicatedDBAndNetwork gives the tenant both a dedicated DB and network.
	DedicatedDBAndNetwork
)

func (l IsolationLevel) String() string {
	switch l {
	case Shared:
		return "Shared"
	case DedicatedDB:
		return "DedicatedDB"
	case DedicatedNetwork:
		return "DedicatedNetwork"
	case DedicatedDBAndNetwork:
		return "DedicatedDBAndNetwork"
	default:
		return fmt.Sprintf("IsolationLevel(%d)", int(l))
	}
}

// Router selects a route target per tenant, honoring an isolation level
// whose dedicated resources must actually be provisioned, and per-tenant
// quotas that must fit within a total budget.
type Router struct {
	isolation    IsolationLevel
	dedicatedDB  string
	dedicatedNet string
	totalQuota   int
	tenantQuotas map[string]int
}

// Option configures a Router and may reject invalid input.
type Option func(*Router) error

// New seeds defaults, applies opts in order, then validates two invariants
// no single option could see on its own: the isolation level's required
// dedicated resources must have actually been provisioned, and the sum of
// every tenant quota must fit within the total quota.
func New(opts ...Option) (*Router, error) {
	r := &Router{
		isolation:    Shared,
		totalQuota:   1000,
		tenantQuotas: make(map[string]int),
	}
	for _, opt := range opts {
		if err := opt(r); err != nil {
			return nil, err
		}
	}

	switch r.isolation {
	case DedicatedDB:
		if r.dedicatedDB == "" {
			return nil, fmt.Errorf("isolation level %s requires WithDedicatedDB", r.isolation)
		}
	case DedicatedNetwork:
		if r.dedicatedNet == "" {
			return nil, fmt.Errorf("isolation level %s requires WithDedicatedNetwork", r.isolation)
		}
	case DedicatedDBAndNetwork:
		if r.dedicatedDB == "" {
			return nil, fmt.Errorf("isolation level %s requires WithDedicatedDB", r.isolation)
		}
		if r.dedicatedNet == "" {
			return nil, fmt.Errorf("isolation level %s requires WithDedicatedNetwork", r.isolation)
		}
	}

	sum := 0
	for _, q := range r.tenantQuotas {
		sum += q
	}
	if sum > r.totalQuota {
		return nil, fmt.Errorf("tenant quotas sum to %d, exceeding total quota %d", sum, r.totalQuota)
	}

	return r, nil
}

// WithIsolationLevel sets the isolation level from the closed set of named
// constants.
func WithIsolationLevel(level IsolationLevel) Option {
	return func(r *Router) error {
		switch level {
		case Shared, DedicatedDB, DedicatedNetwork, DedicatedDBAndNetwork:
			r.isolation = level
			return nil
		default:
			return fmt.Errorf("unknown isolation level: %d", int(level))
		}
	}
}

// WithDedicatedDB records the DSN provisioned for this tenant group's own
// database. A non-empty value is what proves provisioning happened.
func WithDedicatedDB(dsn string) Option {
	return func(r *Router) error {
		if dsn == "" {
			return fmt.Errorf("dedicated DB dsn must not be empty")
		}
		r.dedicatedDB = dsn
		return nil
	}
}

// WithDedicatedNetwork records the network identifier provisioned for this
// tenant group's own network path.
func WithDedicatedNetwork(networkID string) Option {
	return func(r *Router) error {
		if networkID == "" {
			return fmt.Errorf("dedicated network id must not be empty")
		}
		r.dedicatedNet = networkID
		return nil
	}
}

// WithTotalQuota sets the total requests/sec budget shared across tenants.
func WithTotalQuota(n int) Option {
	return func(r *Router) error {
		if n < 1 {
			return fmt.Errorf("total quota must be >= 1, got %d", n)
		}
		r.totalQuota = n
		return nil
	}
}

// WithTenantQuota assigns tenantID a slice of the total quota.
func WithTenantQuota(tenantID string, quota int) Option {
	return func(r *Router) error {
		if tenantID == "" {
			return fmt.Errorf("tenant id must not be empty")
		}
		if quota < 1 {
			return fmt.Errorf("tenant quota must be >= 1, got %d", quota)
		}
		r.tenantQuotas[tenantID] = quota
		return nil
	}
}

// IsolationLevel reports the configured isolation level.
func (r *Router) IsolationLevel() IsolationLevel { return r.isolation }

// Route reports the route target a tenant's traffic would use.
func (r *Router) Route(tenantID string) string {
	switch r.isolation {
	case DedicatedDB:
		return fmt.Sprintf("db=%s,net=shared", r.dedicatedDB)
	case DedicatedNetwork:
		return fmt.Sprintf("db=shared,net=%s", r.dedicatedNet)
	case DedicatedDBAndNetwork:
		return fmt.Sprintf("db=%s,net=%s", r.dedicatedDB, r.dedicatedNet)
	default:
		return "db=shared,net=shared"
	}
}

// TenantQuota reports the quota assigned to tenantID, or 0 if unassigned.
func (r *Router) TenantQuota(tenantID string) int {
	return r.tenantQuotas[tenantID]
}
```

### The runnable demo

The demo builds a router with a genuinely provisioned dedicated DB, then
shows both failure modes: an isolation level claimed without its resource,
and tenant quotas that oversubscribe the total.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/tenantrouter"
)

func main() {
	r, err := tenantrouter.New(
		tenantrouter.WithIsolationLevel(tenantrouter.DedicatedDB),
		tenantrouter.WithDedicatedDB("postgres://acme-dedicated/db"),
		tenantrouter.WithTotalQuota(500),
		tenantrouter.WithTenantQuota("acme", 200),
		tenantrouter.WithTenantQuota("globex", 150),
	)
	if err != nil {
		panic(err)
	}

	fmt.Printf("isolation level: %s\n", r.IsolationLevel())
	fmt.Printf("acme route: %s\n", r.Route("acme"))
	fmt.Printf("acme quota: %d\n", r.TenantQuota("acme"))

	_, err = tenantrouter.New(
		tenantrouter.WithIsolationLevel(tenantrouter.DedicatedNetwork),
		// no WithDedicatedNetwork call: the isolation level claims a
		// dedicated network path that was never provisioned.
	)
	fmt.Printf("unprovisioned dedicated network error: %v\n", err)

	_, err = tenantrouter.New(
		tenantrouter.WithTotalQuota(100),
		tenantrouter.WithTenantQuota("acme", 60),
		tenantrouter.WithTenantQuota("globex", 60),
	)
	fmt.Printf("quota oversubscription error: %v\n", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
isolation level: DedicatedDB
acme route: db=postgres://acme-dedicated/db,net=shared
acme quota: 200
unprovisioned dedicated network error: isolation level DedicatedNetwork requires WithDedicatedNetwork
quota oversubscription error: tenant quotas sum to 120, exceeding total quota 100
```

### Tests

`TestNew` tables every isolation level against provisioned and
unprovisioned dedicated resources, plus tenant quotas under, at, and over
the total. `TestRouteReflectsIsolationLevel` proves `Route` embeds the
actual provisioned identifiers rather than a placeholder.

Create `tenantrouter_test.go`:

```go
package tenantrouter

import "testing"

func TestNew(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		opts    []Option
		wantErr bool
	}{
		{name: "defaults only, shared isolation"},
		{name: "dedicated DB provisioned", opts: []Option{
			WithIsolationLevel(DedicatedDB), WithDedicatedDB("dsn"),
		}},
		{name: "dedicated DB claimed but not provisioned", opts: []Option{
			WithIsolationLevel(DedicatedDB),
		}, wantErr: true},
		{name: "dedicated network claimed but not provisioned", opts: []Option{
			WithIsolationLevel(DedicatedNetwork),
		}, wantErr: true},
		{name: "dedicated both provisioned", opts: []Option{
			WithIsolationLevel(DedicatedDBAndNetwork), WithDedicatedDB("dsn"), WithDedicatedNetwork("vpc-1"),
		}},
		{name: "dedicated both, only DB provisioned", opts: []Option{
			WithIsolationLevel(DedicatedDBAndNetwork), WithDedicatedDB("dsn"),
		}, wantErr: true},
		{name: "unknown isolation level", opts: []Option{
			WithIsolationLevel(IsolationLevel(99)),
		}, wantErr: true},
		{name: "tenant quotas within total", opts: []Option{
			WithTotalQuota(100), WithTenantQuota("a", 40), WithTenantQuota("b", 40),
		}},
		{name: "tenant quotas exceed total", opts: []Option{
			WithTotalQuota(100), WithTenantQuota("a", 60), WithTenantQuota("b", 60),
		}, wantErr: true},
		{name: "tenant quotas exactly at total is allowed", opts: []Option{
			WithTotalQuota(100), WithTenantQuota("a", 50), WithTenantQuota("b", 50),
		}},
		{name: "empty tenant id rejected", opts: []Option{WithTenantQuota("", 10)}, wantErr: true},
		{name: "non-positive tenant quota rejected", opts: []Option{WithTenantQuota("a", 0)}, wantErr: true},
		{name: "empty dedicated db dsn rejected", opts: []Option{WithDedicatedDB("")}, wantErr: true},
		{name: "empty dedicated network id rejected", opts: []Option{WithDedicatedNetwork("")}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := New(tt.opts...)
			if tt.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestRouteReflectsIsolationLevel(t *testing.T) {
	t.Parallel()

	r, err := New(WithIsolationLevel(DedicatedDBAndNetwork), WithDedicatedDB("dsn"), WithDedicatedNetwork("vpc-1"))
	if err != nil {
		t.Fatal(err)
	}
	want := "db=dsn,net=vpc-1"
	if got := r.Route("acme"); got != want {
		t.Fatalf("Route() = %q, want %q", got, want)
	}
}
```

## Review

The router is correct when a claimed isolation level is backed by genuinely
provisioned resources rather than just a named constant, and when every
tenant's slice of the total quota is checked against what every other tenant
was also promised. The provisioning check is the general shape for "a
config value implies a precondition that a different option is responsible
for satisfying" — `DedicatedDB` implies "a DSN exists," and only the
constructor, after all options ran, can see whether that implication was
kept. The quota check is the same "sum across a map, compare to a total"
pattern used for queue depth and batch size elsewhere in this chapter, just
applied to a `map[string]int` instead of two scalars.

## Resources

- [AWS Well-Architected: multi-tenant SaaS isolation](https://docs.aws.amazon.com/wellarchitected/latest/saas-lens/tenant-isolation.html)
- [Google Cloud: multi-tenancy patterns](https://cloud.google.com/architecture/patterns-for-scalable-and-resilient-apps)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [20-encryption-key-version-manager.md](20-encryption-key-version-manager.md) | Next: [22-sql-migration-executor-strategy.md](22-sql-migration-executor-strategy.md)
