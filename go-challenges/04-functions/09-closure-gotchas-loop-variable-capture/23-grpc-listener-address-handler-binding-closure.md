# Exercise 23: gRPC Server Listener Binding: Handlers Capturing Loop Address Variable

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde).

A gRPC server binds one handler per listener address against a shared
`*ListenerConfig`, then reuses that SAME config instance for a later
listener registration (or a failover swap) by overwriting `Address` before
binding again — a common shortcut in listener setup code. Because the
earlier handlers hold a pointer to the shared config, they retroactively
report whatever address the config says NOW, not the one they were bound to.

## What you'll build

```text
grpcbind/                    independent module: example.com/grpcbind
  go.mod                     go 1.24
  grpcbind.go                  ListenerConfig, Handler, BindHandlers, BindHandlersBuggy
  cmd/
    demo/
      main.go                runnable demo: bind handlers, mutate config, print reports
  grpcbind_test.go             table test: snapshot vs. leaked mutation, empty and repeated-mutation edges
```

- Files: `grpcbind.go`, `cmd/demo/main.go`, `grpcbind_test.go`.
- Implement: `BindHandlers(cfg, addrs) map[string]Handler` snapshotting each address at bind time; `BindHandlersBuggy` closing over the `*ListenerConfig` pointer directly.
- Test: a table test covering the snapshot-vs-leak contrast, an empty-address-list edge case, and a case with two mutations after bind.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/grpcbind/cmd/demo
cd ~/go-exercises/grpcbind
go mod init example.com/grpcbind
go mod edit -go=1.24
```

### The address variable is fine; the shared config pointer is not

`BindHandlersBuggy` closes over both `addr` and `cfg`. `addr` is a
well-behaved per-iteration range variable on a `go 1.24` module — each
handler is registered under its own map key correctly. The bug is entirely
in `cfg`: setup code mutates it AFTER these handlers are bound, when a
failover promotes a different address or the next listener registration
reuses the same config instance. Since every handler holds that same
pointer, they all start reporting the new address, including the ones bound
for the OLD address. `BindHandlers` fixes it by reading `addr` into a local
`bound` at bind time — the handler closes over that snapshot.

Create `grpcbind.go`:

```go
package grpcbind

// ListenerConfig holds a listener's bind address while a set of gRPC
// listeners is being configured -- for example during a failover that
// promotes a standby address and reuses the same config instance for the
// next listener registration.
type ListenerConfig struct {
	Address string
}

// Handler reports the address its listener believes it is bound to.
type Handler func() string

// BindHandlersBuggy binds one handler per listener address, but every
// handler closes over a POINTER to the SAME shared ListenerConfig instead of
// its own address. When a later reconfiguration step mutates cfg.Address
// (promoting the next listener's address, or a failover swap), every
// already-bound handler retroactively reports the NEW address instead of the
// one it was bound to.
func BindHandlersBuggy(cfg *ListenerConfig, addrs []string) map[string]Handler {
	handlers := make(map[string]Handler, len(addrs))
	for _, addr := range addrs {
		cfg.Address = addr
		handlers[addr] = func() string {
			return cfg.Address // BUG: reads the live, mutable config
		}
	}
	return handlers
}

// BindHandlers binds one handler per listener address, each snapshotting its
// OWN address at bind time, so a later reconfiguration of the shared config
// cannot change what an already-bound handler reports.
func BindHandlers(cfg *ListenerConfig, addrs []string) map[string]Handler {
	handlers := make(map[string]Handler, len(addrs))
	for _, addr := range addrs {
		cfg.Address = addr
		bound := addr // snapshot taken now, not read later
		handlers[addr] = func() string {
			return bound
		}
	}
	return handlers
}
```

### The runnable demo

The demo binds three listener addresses, simulates a later failover mutation
to the shared config, then reports what the first two handlers believe they
are bound to.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/grpcbind"
)

func main() {
	addrs := []string{":9001", ":9002", ":9003"}

	cfg := &grpcbind.ListenerConfig{}
	handlers := grpcbind.BindHandlers(cfg, addrs)

	// A later failover step promotes a standby address, reusing this same
	// config instance.
	cfg.Address = ":9099"

	fmt.Println("correct handlers[:9001]():", handlers[":9001"]())
	fmt.Println("correct handlers[:9002]():", handlers[":9002"]())

	buggyCfg := &grpcbind.ListenerConfig{}
	buggyHandlers := grpcbind.BindHandlersBuggy(buggyCfg, addrs)
	buggyCfg.Address = ":9099"

	fmt.Println("buggy   handlers[:9001]():", buggyHandlers[":9001"]())
	fmt.Println("buggy   handlers[:9002]():", buggyHandlers[":9002"]())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
correct handlers[:9001](): :9001
correct handlers[:9002](): :9002
buggy   handlers[:9001](): :9099
buggy   handlers[:9002](): :9099
```

### Tests

`TestBindHandlers` is a table test covering the snapshot-vs-leak contrast for
both variants, including a single-listener case that still leaks under the
buggy variant. `TestBindHandlersEmptyAddrsEdgeCase` covers binding against an
empty address list. `TestBindHandlersEachAddressIndependent` confirms the
correct variant survives two mutations after bind, not just one.

Create `grpcbind_test.go`:

```go
package grpcbind

import "testing"

func TestBindHandlers(t *testing.T) {
	tests := []struct {
		name  string
		bind  func(*ListenerConfig, []string) map[string]Handler
		addrs []string
		want  string // handlers[addrs[0]]() after cfg.Address is mutated post-bind
	}{
		{
			name:  "snapshot at bind time keeps the original address",
			bind:  BindHandlers,
			addrs: []string{":9001", ":9002"},
			want:  ":9001",
		},
		{
			name:  "live config pointer leaks the later address mutation",
			bind:  BindHandlersBuggy,
			addrs: []string{":9001", ":9002"},
			want:  ":9099",
		},
		{
			name:  "single listener still leaks under the buggy variant",
			bind:  BindHandlersBuggy,
			addrs: []string{":9001"},
			want:  ":9099",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &ListenerConfig{}
			handlers := tt.bind(cfg, tt.addrs)

			// A later failover step reuses the same config instance.
			cfg.Address = ":9099"

			if got := handlers[tt.addrs[0]](); got != tt.want {
				t.Fatalf("handlers[%s]() = %q, want %q", tt.addrs[0], got, tt.want)
			}
		})
	}
}

func TestBindHandlersEmptyAddrsEdgeCase(t *testing.T) {
	cfg := &ListenerConfig{}
	handlers := BindHandlers(cfg, nil)
	if len(handlers) != 0 {
		t.Fatalf("len(handlers) = %d, want 0", len(handlers))
	}
}

func TestBindHandlersEachAddressIndependent(t *testing.T) {
	addrs := []string{":9001", ":9002", ":9003"}
	cfg := &ListenerConfig{}
	handlers := BindHandlers(cfg, addrs)

	cfg.Address = ":9099" // simulate two more reconfigurations after bind
	cfg.Address = ":9100"

	for _, addr := range addrs {
		if got := handlers[addr](); got != addr {
			t.Fatalf("handlers[%s]() = %q, want %q", addr, got, addr)
		}
	}
}
```

## Review

This is the loop-capture family applied to gRPC listener setup: the range
variable is not the problem (`addr` binds correctly on Go 1.22+), the
shared, mutable `*ListenerConfig` is. Reusing one config instance across
listener registrations — or during a failover that promotes a new address —
is a realistic shortcut, and it is exactly the shape that makes earlier
handlers drift when later setup code bumps a shared field.
`TestBindHandlersEachAddressIndependent` shows the fix holds even under
repeated later mutation, because the snapshot happened once, at bind time,
and nothing after that can reach it.

## Resources

- [`google.golang.org/grpc`](https://pkg.go.dev/google.golang.org/grpc) — the production server this pattern generalizes to.
- [Go spec: Pointer types](https://go.dev/ref/spec#Pointer_types) — why closing over `*ListenerConfig` captures the mutation stream, not a value.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [22-cache-key-invalidation-callback-registration.md](22-cache-key-invalidation-callback-registration.md) | Next: [24-nested-transaction-savepoint-defer-timing-violation.md](24-nested-transaction-savepoint-defer-timing-violation.md)
