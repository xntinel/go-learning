# Exercise 11: Route Webhook Events With a Two-Level String Switch

**Nivel: Intermedio** — validacion rapida (un test corto).

Every service that accepts webhooks from Stripe, GitHub, or similar providers
needs one function at the front door: given the provider and the event name,
which internal handler should process it? This module builds that function
as a switch on provider nested inside a switch on event, using
comma-separated case lists so multiple events fan into the same handler.
It is self-contained: its own `go mod init`, code, demo, and test.

## What you'll build

```text
webhook/                   independent module: example.com/webhook-event-router
  go.mod                   go 1.24
  router.go                package router; ErrUnknownProvider; Route(provider, event) (string, error)
  cmd/demo/main.go         runnable demo over known and unknown pairs
  router_test.go            table over known routes, unknown event, unknown provider
```

- Implement: `Route(provider, event string) (handler string, err error)` — outer switch on `provider`, inner switch on `event` with multi-value cases.
- Test: a table covering one route per handler, an unknown event, and an unknown provider checked with `errors.Is`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/03-switch-statements/11-webhook-event-router/cmd/demo
cd go-solutions/03-control-flow/03-switch-statements/11-webhook-event-router
go mod edit -go=1.24
```

### Why a nested switch beats an if/else ladder or a bare map

A webhook router dispatches on two axes: provider, then event. An if/else
ladder chained on both reads as a wall of
`provider == "stripe" && (event == "a" || event == "b")` conditions. A flat
`map[string]string` keyed by `provider+"."+event` avoids the ladder but
cannot distinguish "unknown provider" from "unknown event" — every miss
looks the same, and there's nowhere to hang per-provider logic.

The outer switch on `provider` isolates per-provider concerns; the inner
switch is where comma-separated cases pay off — `case
"payment_intent.succeeded", "payment_intent.failed":` reads as one routing
rule, because both events mean the same thing here: "something happened to
a payment." Each inner `default` is exhaustive by construction: any event
the case list doesn't name is unhandled for that provider, named in the
error, instead of a map's silent zero value.

Create `router.go`:

```go
package router

import (
	"errors"
	"fmt"
)

// ErrUnknownProvider marks a provider that isn't onboarded at all, as
// opposed to a known provider sending an event we don't handle.
var ErrUnknownProvider = errors.New("webhook: unknown provider")

// Route maps a provider + event pair to the handler responsible for it.
func Route(provider, event string) (handler string, err error) {
	switch provider {
	case "stripe":
		switch event {
		case "payment_intent.succeeded", "payment_intent.failed":
			return "payments", nil
		case "customer.created":
			return "crm", nil
		default:
			return "", fmt.Errorf("webhook: unknown stripe event %q", event)
		}
	case "github":
		switch event {
		case "push", "pull_request":
			return "ci", nil
		case "issues":
			return "tracker", nil
		default:
			return "", fmt.Errorf("webhook: unknown github event %q", event)
		}
	default:
		return "", fmt.Errorf("%w: %q", ErrUnknownProvider, provider)
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	router "example.com/webhook-event-router"
)

func main() {
	pairs := [][2]string{
		{"stripe", "payment_intent.succeeded"},
		{"github", "issues"},
		{"stripe", "invoice.upcoming"},
		{"shopify", "orders/create"},
	}

	for _, p := range pairs {
		handler, err := router.Route(p[0], p[1])
		if err != nil {
			fmt.Printf("%-8s %-28s error: %v\n", p[0], p[1], err)
			continue
		}
		fmt.Printf("%-8s %-28s -> %s\n", p[0], p[1], handler)
	}
}
```

Run `go run ./cmd/demo`, expected output:

```
stripe   payment_intent.succeeded     -> payments
github   issues                       -> tracker
stripe   invoice.upcoming             error: webhook: unknown stripe event "invoice.upcoming"
shopify  orders/create                error: webhook: unknown provider: "shopify"
```

### Test

`TestRoute` runs a table over one route per handler plus an unknown event,
then checks the unknown-provider case separately with `errors.Is` against
`ErrUnknownProvider`, so a caller can branch on "provider not onboarded"
without string-matching the message.

Create `router_test.go`:

```go
package router

import (
	"errors"
	"testing"
)

func TestRoute(t *testing.T) {
	t.Parallel()

	tests := []struct {
		provider, event, wantHandler string
		wantErr                      bool
	}{
		{"stripe", "payment_intent.succeeded", "payments", false},
		{"stripe", "customer.created", "crm", false},
		{"github", "push", "ci", false},
		{"github", "issues", "tracker", false},
		{"stripe", "invoice.upcoming", "", true},
	}

	for _, tc := range tests {
		handler, err := Route(tc.provider, tc.event)
		if tc.wantErr {
			if err == nil {
				t.Errorf("Route(%q, %q) error = nil, want error", tc.provider, tc.event)
			}
			continue
		}
		if err != nil {
			t.Errorf("Route(%q, %q) unexpected error: %v", tc.provider, tc.event, err)
		}
		if handler != tc.wantHandler {
			t.Errorf("Route(%q, %q) = %q, want %q", tc.provider, tc.event, handler, tc.wantHandler)
		}
	}

	if _, err := Route("shopify", "orders/create"); !errors.Is(err, ErrUnknownProvider) {
		t.Errorf("Route(shopify, orders/create) error = %v, want errors.Is match for ErrUnknownProvider", err)
	}
}
```

## Review

The router is correct when every known pair lands on its handler and every
unknown case fails in a way the caller can act on: a plain error naming the
event for an unhandled event, a sentinel-wrapped error for an unknown
provider. Carry this forward: reach for a nested switch with multi-value
cases whenever dispatch has two axes and different misses need different,
checkable errors.

## Resources

- [Go Specification: Switch statements](https://go.dev/ref/spec#Switch_statements) — expression switches and comma-separated case lists.
- [errors.Is](https://pkg.go.dev/errors#Is) — matching wrapped sentinel errors.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [10-rate-limit-tier-resolver.md](10-rate-limit-tier-resolver.md) | Next: [12-card-brand-detector.md](12-card-brand-detector.md)
