# Exercise 29: Route Connection Requests to the Appropriate Pool Type

**Nivel: Intermedio** — validacion rapida (un test corto).

A backend that talks to the same logical database through three different
topologies — a direct connection for local development, a connection
routed through PgBouncer to survive a connection-hungry fleet, and an
SSH-tunneled connection to reach a database sitting behind a bastion host
in a private subnet — cannot validate or size all three the same way. Each
topology is missing a different piece of information when it is
misconfigured, and each has its own idea of "capacity," whether that is a
direct connection limit, a proxy's multiplexed ceiling, or a tunnel's
narrow allowance. A connection router that does not classify the config it
was handed before validating it risks either crashing on a field that
does not exist for that topology, or worse, silently connecting through
the wrong path. This module is fully self-contained: its own `go mod
init`, all code inline, its own demo and tests.

## What you'll build

```text
connection-pool-route-selection/   independent module: example.com/connection-pool-route-selection
  go.mod                           go 1.24
  connpool.go                      Route(cfg any, activeConns int) (Connection, error)
  cmd/
    demo/
      main.go                      routes a direct, an exhausted proxied, and a tunneled pool
  connpool_test.go                  table of valid, invalid, and exhausted cases per pool kind
```

- Files: `connpool.go`, `cmd/demo/main.go`, `connpool_test.go`.
- Implement: `Route(cfg any, activeConns int) (Connection, error)`,
  type-switching on `DirectConfig`, `ProxiedConfig`, and `SSHTunnelConfig`
  to validate each pool kind's required fields and check capacity.
- Test: a successful route per pool kind, a missing-required-field case per
  pool kind, an at-capacity rejection per pool kind, and an unsupported
  config type.

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/04-type-switch/29-connection-pool-route-selection/cmd/demo
cd go-solutions/03-control-flow/04-type-switch/29-connection-pool-route-selection
go mod edit -go=1.24
```

Each pool type is its own struct rather than one shared `PoolConfig` with
every possible field, because a shared struct would let a zero-value field
meant for a different pool type pass validation by accident — Go gives
`ProxyAddr` the same empty-string zero value whether the caller forgot to
set it on a `ProxiedConfig` or never meant to set it at all on a
`DirectConfig` that has no such concept. Keeping the three configs
disjoint means `Route`'s validation for each `case` only ever looks at
fields that exist and are meaningful for that exact topology. Capacity
checking is deliberately the same shape in every branch — compare
`activeConns` against that config's own `MaxConns` — but is not factored
out into one shared helper, because each branch also needs to build a
`Connection` whose `Endpoint` is computed differently: a direct pool's
endpoint is its DSN, a proxied pool's is the proxy address rather than the
database DSN behind it, and a tunneled pool's is a description of both
hops. Collapsing that into a generic helper would need its own type switch
just to know which field to read, defeating the point of having already
switched once.

Create `connpool.go`:

```go
package connpool

import (
	"errors"
	"fmt"
)

// ErrPoolExhausted is returned when a pool is already serving its
// configured maximum number of connections.
var ErrPoolExhausted = errors.New("connpool: pool exhausted")

// ErrInvalidConfig is returned when a pool's configuration is missing a
// field that pool type requires to route at all.
var ErrInvalidConfig = errors.New("connpool: invalid configuration")

// DirectConfig connects straight to the database with no intermediary.
type DirectConfig struct {
	DSN      string
	MaxConns int
}

// ProxiedConfig connects through a connection-pooling proxy such as
// PgBouncer, which multiplexes many client connections onto fewer real
// database connections.
type ProxiedConfig struct {
	ProxyAddr string
	DSN       string
	MaxConns  int
}

// SSHTunnelConfig connects through an SSH tunnel to a database that is not
// directly reachable from the caller's network, such as one sitting behind
// a bastion host in a private subnet.
type SSHTunnelConfig struct {
	TunnelHost string
	RemoteAddr string
	MaxConns   int
}

// Connection describes the endpoint a caller should actually dial once
// routing has decided which pool serves the request.
type Connection struct {
	Kind     string
	Endpoint string
}

// Route validates cfg for its concrete pool type and checks it against
// activeConns, the number of connections already checked out from that
// pool. Each pool type validates a different set of required fields
// because each is missing a different piece of information when
// misconfigured — a direct pool with no DSN, a proxied pool with no proxy
// address, a tunnel with no remote host — and folding all three into one
// generic config struct would let a zero-value field meant for a different
// pool type pass validation by accident, since Go gives every unset string
// field the same zero value regardless of which pool it belongs to.
func Route(cfg any, activeConns int) (Connection, error) {
	switch c := cfg.(type) {
	case DirectConfig:
		if c.DSN == "" {
			return Connection{}, fmt.Errorf("%w: direct pool requires a DSN", ErrInvalidConfig)
		}
		if activeConns >= c.MaxConns {
			return Connection{}, fmt.Errorf("%w: direct pool at %d/%d connections", ErrPoolExhausted, activeConns, c.MaxConns)
		}
		return Connection{Kind: "direct", Endpoint: c.DSN}, nil

	case ProxiedConfig:
		if c.ProxyAddr == "" || c.DSN == "" {
			return Connection{}, fmt.Errorf("%w: proxied pool requires a proxy address and a DSN", ErrInvalidConfig)
		}
		if activeConns >= c.MaxConns {
			return Connection{}, fmt.Errorf("%w: proxied pool at %d/%d connections", ErrPoolExhausted, activeConns, c.MaxConns)
		}
		return Connection{Kind: "proxied", Endpoint: c.ProxyAddr}, nil

	case SSHTunnelConfig:
		if c.TunnelHost == "" || c.RemoteAddr == "" {
			return Connection{}, fmt.Errorf("%w: ssh-tunneled pool requires a tunnel host and a remote address", ErrInvalidConfig)
		}
		if activeConns >= c.MaxConns {
			return Connection{}, fmt.Errorf("%w: ssh-tunneled pool at %d/%d connections", ErrPoolExhausted, activeConns, c.MaxConns)
		}
		return Connection{Kind: "ssh-tunnel", Endpoint: c.TunnelHost + "->" + c.RemoteAddr}, nil

	default:
		return Connection{}, fmt.Errorf("%w: unsupported pool config type %T", ErrInvalidConfig, cfg)
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/connection-pool-route-selection"
)

func main() {
	pools := []struct {
		cfg    any
		active int
	}{
		{connpool.DirectConfig{DSN: "postgres://db-primary/app", MaxConns: 20}, 5},
		{connpool.ProxiedConfig{ProxyAddr: "pgbouncer:6432", DSN: "postgres://db-primary/app", MaxConns: 200}, 200},
		{connpool.SSHTunnelConfig{TunnelHost: "bastion.internal", RemoteAddr: "10.0.4.12:5432", MaxConns: 5}, 2},
	}

	for _, p := range pools {
		conn, err := connpool.Route(p.cfg, p.active)
		if err != nil {
			fmt.Printf("%-20T -> error: %v\n", p.cfg, err)
			continue
		}
		fmt.Printf("%-20T -> %s via %s\n", p.cfg, conn.Kind, conn.Endpoint)
	}
}
```

```bash
go run ./cmd/demo
```

Expected output:

```text
connpool.DirectConfig -> direct via postgres://db-primary/app
connpool.ProxiedConfig -> error: connpool: pool exhausted: proxied pool at 200/200 connections
connpool.SSHTunnelConfig -> ssh-tunnel via bastion.internal->10.0.4.12:5432
```

The proxied pool is deliberately configured at exactly its `MaxConns`
ceiling to show the exhaustion path even though PgBouncer-style pools
typically have far more headroom than a direct connection pool would —
the routing logic does not care which topology is being checked, only
whether that topology's own configured ceiling has been reached.

### Tests

Create `connpool_test.go`:

```go
package connpool

import (
	"errors"
	"testing"
)

func TestRoute(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		cfg         any
		activeConns int
		wantErr     error
	}{
		{
			name: "direct pool under capacity routes",
			cfg:  DirectConfig{DSN: "postgres://db/app", MaxConns: 10},
		},
		{
			name:    "direct pool missing DSN is invalid",
			cfg:     DirectConfig{MaxConns: 10},
			wantErr: ErrInvalidConfig,
		},
		{
			name:        "direct pool at capacity is exhausted",
			cfg:         DirectConfig{DSN: "postgres://db/app", MaxConns: 10},
			activeConns: 10,
			wantErr:     ErrPoolExhausted,
		},
		{
			name: "proxied pool under capacity routes",
			cfg:  ProxiedConfig{ProxyAddr: "pgbouncer:6432", DSN: "postgres://db/app", MaxConns: 100},
		},
		{
			name:    "proxied pool missing proxy address is invalid",
			cfg:     ProxiedConfig{DSN: "postgres://db/app", MaxConns: 100},
			wantErr: ErrInvalidConfig,
		},
		{
			name:        "proxied pool at capacity is exhausted",
			cfg:         ProxiedConfig{ProxyAddr: "pgbouncer:6432", DSN: "postgres://db/app", MaxConns: 100},
			activeConns: 100,
			wantErr:     ErrPoolExhausted,
		},
		{
			name: "ssh-tunneled pool under capacity routes",
			cfg:  SSHTunnelConfig{TunnelHost: "bastion", RemoteAddr: "10.0.0.5:5432", MaxConns: 5},
		},
		{
			name:    "ssh-tunneled pool missing remote address is invalid",
			cfg:     SSHTunnelConfig{TunnelHost: "bastion", MaxConns: 5},
			wantErr: ErrInvalidConfig,
		},
		{
			name:        "ssh-tunneled pool at capacity is exhausted",
			cfg:         SSHTunnelConfig{TunnelHost: "bastion", RemoteAddr: "10.0.0.5:5432", MaxConns: 5},
			activeConns: 5,
			wantErr:     ErrPoolExhausted,
		},
		{
			name:    "unsupported config type is invalid",
			cfg:     "bogus",
			wantErr: ErrInvalidConfig,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Route(tt.cfg, tt.activeConns)
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("Route(%v) unexpected error: %v", tt.cfg, err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Route(%v) err = %v, want %v", tt.cfg, err, tt.wantErr)
			}
		})
	}
}
```

Verify: `go test -count=1 ./...`

## Review

`Route` is correct because each pool kind validates only the fields that
exist for that kind, so a `DirectConfig` can never be silently rejected —
or silently accepted — based on a `ProxyAddr` field it does not have and
never will. Comparing `activeConns >= c.MaxConns` after validation, rather
than before it, is deliberate ordering: a caller should learn about a
missing DSN before it learns about capacity, since the configuration
problem is the one that needs fixing regardless of current load, while a
capacity rejection is transient and will resolve itself as connections are
returned to the pool. The `default` case returning `ErrInvalidConfig`
rather than silently attempting to connect with whatever fields happen to
be present is what stops a fourth pool topology, added elsewhere without a
corresponding `case` here, from being routed through undefined behavior
instead of failing with a clear, typed error.

## Resources

- [PgBouncer documentation](https://www.pgbouncer.org/)
- [OpenSSH manual: ssh, -L (local port forwarding)](https://man.openbsd.org/ssh)
- [Go Specification: Type switches](https://go.dev/ref/spec#Type_switches)
- [database/sql: managing connections](https://go.dev/doc/database/manage-connections)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [28-dns-resolution-record-dispatch.md](28-dns-resolution-record-dispatch.md) | Next: [30-graceful-config-reload-dispatcher.md](30-graceful-config-reload-dispatcher.md)
