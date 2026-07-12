# Exercise 18: DNS Resolution Strategy Callbacks with Fallback Routing

**Nivel: Intermedio** — validacion rapida (un test corto).

A service mesh client resolves a name several possible ways — a service
discovery registry, SRV-style records advertised by the platform, a direct
static table for legacy hosts — and wants to try them in a preferred order,
falling back to the next when one has no answer. Each strategy is a named
function value with the same signature; a `Chain` combinator ties them
together.

## What you'll build

```text
resolver/                   independent module: example.com/dns-resolver-strategy-callback
  go.mod                     go 1.24
  resolver.go                  type ResolveFunc, ErrNotFound, StaticResolver, func Chain
  cmd/
    demo/
      main.go                  runnable demo: service discovery -> SRV -> direct fallback chain
  resolver_test.go             table test: single strategy hit/miss, chain fallback, chain ordering, empty chain
```

Files: `resolver.go`, `cmd/demo/main.go`, `resolver_test.go`.
Implement: `type ResolveFunc func(name string) ([]string, error)`, a sentinel `ErrNotFound`, `StaticResolver(table map[string][]string) ResolveFunc` as the deterministic stand-in for any concrete strategy, and `func Chain(strategies ...ResolveFunc) ResolveFunc`.
Test: a lone `StaticResolver` on hit and miss, `Chain` falling back from a strategy that doesn't know a name to one that does, `Chain` preferring the earliest strategy when more than one has an answer, `Chain` returning `ErrNotFound` when nothing matches, and `Chain()` with zero strategies.
Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why one function type covers direct, SRV, and service discovery

Direct lookup, SRV-style resolution, and service discovery are three
completely different wire protocols in production — one hits `/etc/hosts` or
a static table, one queries `_service._proto.name` SRV records, one calls a
registry's HTTP or gRPC API. None of that protocol difference matters to the
caller, who only wants "give me addresses for this name, or tell me you
don't have any." Modeling every strategy as `ResolveFunc func(name string)
([]string, error)` lets `Chain` treat "try service discovery, then SRV, then
the static table" as an ordinary slice of function values with no type
switch and no strategy-specific branch. This module implements every
strategy with the same `StaticResolver` helper over a fixed table — the
point of the exercise is the callback and fallback shape, and a fixed table
makes the tests deterministic instead of depending on a live network.

Create `resolver.go`:

```go
// Package resolver selects among DNS resolution strategies (direct A/AAAA
// records, SRV-style service records, service discovery) via a common
// function type, and chains them with fallback.
package resolver

import "fmt"

// ResolveFunc resolves a service name to a list of addresses. Every
// strategy — direct lookup, SRV records, service discovery — implements
// this same shape.
type ResolveFunc func(name string) ([]string, error)

// ErrNotFound is returned by a strategy that has no entry for the name.
var ErrNotFound = fmt.Errorf("resolver: name not found")

// StaticResolver builds a ResolveFunc backed by a fixed lookup table. It
// stands in for whatever a strategy actually looks up over the wire (an
// /etc/hosts-style direct table, a service registry's SRV set, a discovery
// API's instance list) so the exercise and its tests stay deterministic.
func StaticResolver(table map[string][]string) ResolveFunc {
	return func(name string) ([]string, error) {
		addrs, ok := table[name]
		if !ok || len(addrs) == 0 {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
		}
		out := make([]string, len(addrs))
		copy(out, addrs)
		return out, nil
	}
}

// Chain tries each strategy in order and returns the first one that
// resolves the name successfully. If every strategy fails, Chain returns
// the last error encountered.
func Chain(strategies ...ResolveFunc) ResolveFunc {
	return func(name string) ([]string, error) {
		var lastErr error
		for _, strategy := range strategies {
			addrs, err := strategy(name)
			if err == nil && len(addrs) > 0 {
				return addrs, nil
			}
			if err != nil {
				lastErr = err
			}
		}
		if lastErr == nil {
			lastErr = fmt.Errorf("%w: %s", ErrNotFound, name)
		}
		return nil, lastErr
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/dns-resolver-strategy-callback"
)

func main() {
	serviceDiscovery := resolver.StaticResolver(map[string][]string{
		"payments.svc": {"10.0.1.11", "10.0.1.12"},
	})
	srv := resolver.StaticResolver(map[string][]string{
		"payments.svc": {"10.0.2.20"},
		"billing.svc":  {"10.0.2.30"},
	})
	direct := resolver.StaticResolver(map[string][]string{
		"legacy.internal": {"192.168.0.5"},
	})

	resolve := resolver.Chain(serviceDiscovery, srv, direct)

	for _, name := range []string{"payments.svc", "billing.svc", "legacy.internal", "ghost.svc"} {
		addrs, err := resolve(name)
		if err != nil {
			fmt.Printf("%s: error: %v\n", name, err)
			continue
		}
		fmt.Printf("%s: %v\n", name, addrs)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
payments.svc: [10.0.1.11 10.0.1.12]
billing.svc: [10.0.2.30]
legacy.internal: [192.168.0.5]
ghost.svc: error: resolver: name not found: ghost.svc
```

### Tests

Create `resolver_test.go`:

```go
package resolver

import (
	"errors"
	"slices"
	"testing"
)

func TestStaticResolverFindsRegisteredName(t *testing.T) {
	t.Parallel()
	r := StaticResolver(map[string][]string{"a.svc": {"1.1.1.1"}})
	addrs, err := r("a.svc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !slices.Equal(addrs, []string{"1.1.1.1"}) {
		t.Fatalf("addrs = %v, want [1.1.1.1]", addrs)
	}
}

func TestStaticResolverMissReturnsErrNotFound(t *testing.T) {
	t.Parallel()
	r := StaticResolver(map[string][]string{"a.svc": {"1.1.1.1"}})
	_, err := r("b.svc")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want wrapping ErrNotFound", err)
	}
}

func TestChainFallsBackThroughStrategies(t *testing.T) {
	t.Parallel()
	first := StaticResolver(map[string][]string{"only-first": {"1.1.1.1"}})
	second := StaticResolver(map[string][]string{"only-second": {"2.2.2.2"}})
	chain := Chain(first, second)

	tests := []struct {
		name string
		want []string
	}{
		{"only-first", []string{"1.1.1.1"}},
		{"only-second", []string{"2.2.2.2"}},
	}
	for _, tc := range tests {
		got, err := chain(tc.name)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", tc.name, err)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestChainPrefersEarlierStrategy(t *testing.T) {
	t.Parallel()
	preferred := StaticResolver(map[string][]string{"svc": {"preferred-ip"}})
	fallback := StaticResolver(map[string][]string{"svc": {"fallback-ip"}})
	chain := Chain(preferred, fallback)

	got, err := chain("svc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !slices.Equal(got, []string{"preferred-ip"}) {
		t.Fatalf("got %v, want [preferred-ip] (first match should win)", got)
	}
}

func TestChainReturnsErrorWhenNoStrategyMatches(t *testing.T) {
	t.Parallel()
	chain := Chain(
		StaticResolver(map[string][]string{"a": {"1.1.1.1"}}),
		StaticResolver(map[string][]string{"b": {"2.2.2.2"}}),
	)
	_, err := chain("ghost")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want wrapping ErrNotFound", err)
	}
}

func TestChainOfNoStrategies(t *testing.T) {
	t.Parallel()
	chain := Chain()
	_, err := chain("anything")
	if err == nil {
		t.Fatal("expected an error when no strategies are configured")
	}
}
```

## Review

`Chain` never inspects which strategy it is calling — it only reacts to the
`(addrs, err)` pair each `ResolveFunc` returns, trying the next one whenever
the current one comes back empty or erroring. `TestChainFallsBackThrough
Strategies` proves the fallback direction works both ways (first-only and
second-only names each resolve), `TestChainPrefersEarlierStrategy` pins that
ordering is meaningful — the first strategy with an answer wins, not the
"best" one — and the empty-chain case guards against a nil-strategies caller
silently getting a nil slice back instead of a clear error.

## Resources

- [Go Specification: Function types](https://go.dev/ref/spec#Function_types)
- [RFC 2782: A DNS RR for specifying the location of services (SRV)](https://www.rfc-editor.org/rfc/rfc2782)
- [errors.Is and error wrapping](https://pkg.go.dev/errors#Is)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [17-compression-codec-adapter.md](17-compression-codec-adapter.md) | Next: [19-elasticsearch-query-builder-callback.md](19-elasticsearch-query-builder-callback.md)
