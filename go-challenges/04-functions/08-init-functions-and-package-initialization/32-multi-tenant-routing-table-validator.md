# Exercise 32: Multi-Tenant Domain-to-Service Routing Rules Validated at init for Consistency and Completeness

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde de colision case-insensitive).

A multi-tenant service routes incoming requests by domain — `acme.tenant.example.com`
goes to the billing service, `initech.tenant.example.com` goes to
reporting — using a static table built once from configuration. That table
has two ways to be silently wrong: a tenant can point at a service name
that was never actually registered (a dangling reference, a typo), and a
registered service can end up with zero tenants pointing at it (dead
configuration nobody notices). This exercise validates both directions at
package initialization, plus a subtler edge case: two tenant domains that
differ only in letter case, which would otherwise create an ambiguous
routing decision at request time.

## What you'll build

```text
tenantrouting/              independent module: example.com/tenantrouting
  go.mod                     module example.com/tenantrouting
  tenantrouting.go             tenantToService, services, validateRouting, ResolveService
  cmd/
    demo/
      main.go                  resolves a mixed-case domain, a normal one, and an unrouted one
  tenantrouting_test.go         validateRouting table (consistent/dangling/orphan/blank/case-collision) + ResolveService test
```

Files: `tenantrouting.go`, `cmd/demo/main.go`, `tenantrouting_test.go`.
Implement: `validateRouting(routes map[string]string, services map[string]struct{}) error` checking every route targets a known service, every service is reachable by at least one route, no domain is blank, and no two domains collide once lowercased; `ResolveService(domain string) (string, bool)` matching case-insensitively against a table normalized once at init.
Test: a consistent routing table passes; a route to an unregistered service, an unreachable registered service, a blank domain, and a case-insensitive domain collision each return a descriptive error; `ResolveService` matches regardless of the input domain's case and reports `false` for an unrouted domain.
Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/08-init-functions-and-package-initialization/32-multi-tenant-routing-table-validator/cmd/demo
cd go-solutions/04-functions/08-init-functions-and-package-initialization/32-multi-tenant-routing-table-validator
go mod edit -go=1.24
```

### Why both directions, and why case matters

Checking only "every tenant's service exists" misses half the problem: a
service can be added to the `services` set (perhaps in preparation for a
migration) with no tenant routed to it yet, and that is a legitimate
transient state during a rollout — but a service that stays permanently
unreachable, forgotten in the set long after every tenant that once used it
moved elsewhere, is dead configuration that should be caught and removed.
`validateRouting` checks both directions in one pass over the routing
table: every route's target must be a real service, and by the end, every
declared service must have been some route's target at least once.

Domain names are conventionally case-insensitive (DNS itself does not
distinguish `ACME.example.com` from `acme.example.com`), so a routing table
that happened to contain both `"acme.tenant.example.com"` and
`"ACME.tenant.example.com"` mapped to different services would have an
ambiguous answer depending on which map entry a lookup happened to hit —
and Go map iteration order is randomized, so that ambiguity would not even
be *consistent* from one run to the next. `validateRouting` catches this at
load time by lowercasing every domain during validation and flagging a
collision, rather than letting `ResolveService` normalize per lookup and
silently pick one of the two colliding routes arbitrarily. `ResolveService`
itself works against `resolvedRoutes`, a lowercased copy of the table built
once at init — so a case-insensitive lookup at request time is a single
map access, not a fresh `strings.ToLower` plus scan every time.

As in the other registration-table exercises this chapter, the validation
logic is a plain function, `validateRouting`, returning a single joined
error naming every problem found — not just the first — so an operator
fixing a broken table sees the whole picture in one panic message instead
of playing whack-a-mole one error at a time across repeated restarts.

Create `tenantrouting.go`:

```go
// tenantrouting.go
// Package tenantrouting validates a multi-tenant domain-to-service routing
// table at package initialization: every tenant domain must route to a
// service that actually exists, and every declared service must be
// reachable through at least one tenant domain -- a service nobody can
// ever route to is dead configuration, and a route to a nonexistent
// service is a startup-time typo, not a request-time 404.
package tenantrouting

import (
	"fmt"
	"sort"
	"strings"
)

// tenantToService maps a tenant's domain to the service name that serves
// it. Domains are matched case-insensitively.
var tenantToService = map[string]string{
	"acme.tenant.example.com":    "billing-service",
	"globex.tenant.example.com":  "billing-service",
	"initech.tenant.example.com": "reporting-service",
}

// services is the set of service names routes are allowed to target.
var services = map[string]struct{}{
	"billing-service":   {},
	"reporting-service": {},
}

// resolvedRoutes is tenantToService with every domain key lowercased, built
// once at init so ResolveService never has to normalize per call.
var resolvedRoutes map[string]string

func init() {
	if err := validateRouting(tenantToService, services); err != nil {
		panic("tenantrouting: " + err.Error())
	}
	resolvedRoutes = normalizeRoutes(tenantToService)
}

// validateRouting confirms every route targets a known service, every
// known service is reachable by at least one route, no tenant domain is
// blank, and no two domains collide once case is normalized. It joins every
// problem found into one error rather than stopping at the first, and is
// extracted from init so tests can exercise each failure mode directly.
func validateRouting(routes map[string]string, services map[string]struct{}) error {
	var errs []string

	seenNormalized := make(map[string]string, len(routes))
	reachable := make(map[string]struct{}, len(services))

	domains := make([]string, 0, len(routes))
	for domain := range routes {
		domains = append(domains, domain)
	}
	sort.Strings(domains) // deterministic error ordering

	for _, domain := range domains {
		service := routes[domain]
		if domain == "" {
			errs = append(errs, "tenant routing table contains a blank domain")
			continue
		}
		norm := strings.ToLower(domain)
		if other, dup := seenNormalized[norm]; dup {
			errs = append(errs, fmt.Sprintf("domain %q collides case-insensitively with %q", domain, other))
		}
		seenNormalized[norm] = domain

		if _, ok := services[service]; !ok {
			errs = append(errs, fmt.Sprintf("tenant %q routes to unknown service %q", domain, service))
			continue
		}
		reachable[service] = struct{}{}
	}

	var unreachable []string
	for service := range services {
		if _, ok := reachable[service]; !ok {
			unreachable = append(unreachable, service)
		}
	}
	if len(unreachable) > 0 {
		sort.Strings(unreachable)
		errs = append(errs, fmt.Sprintf("service(s) unreachable by any tenant: %v", unreachable))
	}

	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("%s", strings.Join(errs, "; "))
}

// normalizeRoutes returns routes with every domain key lowercased.
func normalizeRoutes(routes map[string]string) map[string]string {
	out := make(map[string]string, len(routes))
	for domain, service := range routes {
		out[strings.ToLower(domain)] = service
	}
	return out
}

// ResolveService returns the service that serves domain, matched
// case-insensitively, and whether a route exists at all.
func ResolveService(domain string) (string, bool) {
	s, ok := resolvedRoutes[strings.ToLower(domain)]
	return s, ok
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/tenantrouting"
)

func main() {
	service, ok := tenantrouting.ResolveService("ACME.tenant.example.com")
	fmt.Println("acme (mixed case) routes to:", service, ok)

	service, ok = tenantrouting.ResolveService("globex.tenant.example.com")
	fmt.Println("globex routes to:", service, ok)

	_, ok = tenantrouting.ResolveService("unknown.tenant.example.com")
	fmt.Println("unknown tenant resolved:", ok)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
acme (mixed case) routes to: billing-service true
globex routes to: billing-service true
unknown tenant resolved: false
```

### Tests

Create `tenantrouting_test.go`:

```go
// tenantrouting_test.go
package tenantrouting

import (
	"strings"
	"testing"
)

func TestValidateRouting(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		routes  map[string]string
		svcs    map[string]struct{}
		wantErr string
	}{
		{
			name:   "consistent config",
			routes: map[string]string{"a.example.com": "svc-a", "b.example.com": "svc-b"},
			svcs:   map[string]struct{}{"svc-a": {}, "svc-b": {}},
		},
		{
			name:    "dangling service reference",
			routes:  map[string]string{"a.example.com": "svc-ghost"},
			svcs:    map[string]struct{}{"svc-a": {}},
			wantErr: "unknown service",
		},
		{
			name:    "orphan service unreachable",
			routes:  map[string]string{"a.example.com": "svc-a"},
			svcs:    map[string]struct{}{"svc-a": {}, "svc-unused": {}},
			wantErr: "unreachable by any tenant",
		},
		{
			name:    "blank domain",
			routes:  map[string]string{"": "svc-a"},
			svcs:    map[string]struct{}{"svc-a": {}},
			wantErr: "blank domain",
		},
		{
			name:    "case-insensitive collision",
			routes:  map[string]string{"a.example.com": "svc-a", "A.example.com": "svc-a"},
			svcs:    map[string]struct{}{"svc-a": {}},
			wantErr: "collides case-insensitively",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateRouting(tc.routes, tc.svcs)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestResolveServiceCaseInsensitive(t *testing.T) {
	t.Parallel()

	svc, ok := ResolveService("ACME.TENANT.example.COM")
	if !ok || svc != "billing-service" {
		t.Fatalf("ResolveService case-insensitive = (%q, %v), want (billing-service, true)", svc, ok)
	}

	if _, ok := ResolveService("nowhere.example.com"); ok {
		t.Fatal("ResolveService for an unrouted domain ok = true, want false")
	}
}
```

## Review

`validateRouting` is correct when it catches every documented shape of
drift in the routing table: a route naming a service that was never
registered, a registered service no route ever targets, a blank domain
entry, and two domains that are distinct strings but the same tenant once
case is normalized. `ResolveService` is correct when it matches
`"ACME.TENANT.example.COM"` to the same service as
`"acme.tenant.example.com"` — proving the lowercase normalization done once
at init is actually used for lookups, not just for validation — and returns
`false` cleanly for a domain with no route at all.

The mistake to avoid is validating only "does every route point at a real
service" and skipping the reverse check. That is the easier half to think
of, but a routing table that has quietly accumulated services nobody routes
to anymore is exactly the kind of drift that goes unnoticed for months
until someone tries to debug why a "supported" service never receives any
traffic. The case-insensitivity check is the other trap: treating domain
strings as opaque, case-sensitive map keys works right up until two
entries that a human would recognize as "the same tenant, typed
differently" end up with different services, and which one wins depends on
Go's randomized map iteration order — a bug that is present but invisible
until the wrong run surfaces it.

## Resources

- [Go spec: Package initialization](https://go.dev/ref/spec#Package_initialization) — why the routing table's consistency is checked once, in `init()`.
- [RFC 4343 — Domain Name System (DNS) Case Insensitivity Clarification](https://www.rfc-editor.org/rfc/rfc4343) — why domain names are conventionally matched case-insensitively.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [31-webhook-event-schema-registration-table.md](31-webhook-event-schema-registration-table.md) | Next: [33-encryption-key-derivation-lazy-init.md](33-encryption-key-derivation-lazy-init.md)
