# Exercise 14: Route Table Setup: Handlers Capturing a Mutated Shared Config

**Nivel: Intermedio** — validacion rapida (un test corto).

A service registers its v1 routes against a shared `*RouterConfig`, then
reuses that SAME config instance for a later route group by bumping
`APIVersion` before registering more routes — a common shortcut in router
setup code. Because the earlier handlers hold a pointer to the shared
config, they retroactively report whatever the config says NOW, not what it
said when they were registered.

## What you'll build

```text
routergroup/                 independent module: example.com/routergroup
  go.mod                     go 1.24
  router.go                   RouterConfig, Handler, RegisterRoutes, RegisterRoutesBuggy
  router_test.go              table test: snapshot vs. leaked mutation
```

- Files: `router.go`, `router_test.go`.
- Implement: `RegisterRoutes(cfg, routes) map[string]Handler` snapshotting `cfg.APIVersion` per route at registration time; `RegisterRoutesBuggy` closing over the `*RouterConfig` pointer directly.
- Test: one table test that registers handlers, mutates `cfg.APIVersion` afterward (simulating a later route group's setup), then calls a handler.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/09-closure-gotchas-loop-variable-capture/14-router-setup-shared-config-mutation
cd go-solutions/04-functions/09-closure-gotchas-loop-variable-capture/14-router-setup-shared-config-mutation
go mod edit -go=1.24
```

### The route variable is fine; the shared config pointer is not

`RegisterRoutesBuggy` closes over both `route` and `cfg`. `route` is a
well-behaved per-iteration range variable on a `go 1.24` module — each
handler correctly reports its own path. The bug is entirely in `cfg`: setup
code mutates it AFTER these handlers are registered, when it moves on to the
next route group. Since every handler holds that same pointer, they all
start reporting the new `APIVersion`, including the ones registered for the
OLD group. `RegisterRoutes` fixes it by reading `cfg.APIVersion` into a local
`version` at registration time — the handler closes over that snapshot.

Create `router.go`:

```go
package router

// RouterConfig holds settings shared while a route table is being built, such
// as the API version stamped onto responses.
type RouterConfig struct {
	APIVersion string
}

// Handler is a registered route's response body producer.
type Handler func() string

// RegisterRoutesBuggy registers one handler per route, and every handler
// closes over a POINTER to the shared RouterConfig instead of the version
// value in effect at registration time. Because the earlier handlers hold a
// pointer to the shared config, they retroactively report whatever the
// config says NOW, not what it said when they were registered.
func RegisterRoutesBuggy(cfg *RouterConfig, routes []string) map[string]Handler {
	handlers := make(map[string]Handler, len(routes))
	for _, route := range routes {
		handlers[route] = func() string {
			return route + ":" + cfg.APIVersion // BUG: reads the live, mutable config
		}
	}
	return handlers
}

// RegisterRoutes registers one handler per route, each snapshotting the
// APIVersion AT REGISTRATION TIME, so a later mutation to the shared config
// cannot change what an already-registered handler reports.
func RegisterRoutes(cfg *RouterConfig, routes []string) map[string]Handler {
	handlers := make(map[string]Handler, len(routes))
	for _, route := range routes {
		version := cfg.APIVersion // snapshot taken now, not read later
		handlers[route] = func() string {
			return route + ":" + version
		}
	}
	return handlers
}
```

### Test

One table test registers handlers for `/users` and `/orders` against
`APIVersion: "v1"`, mutates the config to `"v2"` (simulating a later route
group reusing the same config instance), then calls the `/users` handler.

Create `router_test.go`:

```go
package router

import "testing"

func TestRegisterRoutes(t *testing.T) {
	tests := []struct {
		name     string
		register func(*RouterConfig, []string) map[string]Handler
		want     string // want[route] after cfg.APIVersion is mutated to v2
	}{
		{
			name:     "snapshot at registration keeps the v1 response",
			register: RegisterRoutes,
			want:     "/users:v1",
		},
		{
			name:     "live config read leaks the later v2 mutation",
			register: RegisterRoutesBuggy,
			want:     "/users:v2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &RouterConfig{APIVersion: "v1"}
			handlers := tt.register(cfg, []string{"/users", "/orders"})

			// A later setup phase reuses the same config instance for a v2 group.
			cfg.APIVersion = "v2"

			if got := handlers["/users"](); got != tt.want {
				t.Fatalf("handlers[/users]() = %q, want %q", got, tt.want)
			}
		})
	}
}
```

Verify: `go test -count=1 ./...`

## Review

This is the loop-capture family applied to router setup: the range variable
is not the problem (`route` binds correctly on Go 1.22+), the shared,
mutable `*RouterConfig` is. Reusing one config instance across route-group
registrations is a realistic shortcut — it is also exactly the shape that
makes early handlers drift when later setup code bumps a shared field.
`RegisterRoutes` shows the fix costs one extra local variable per
registration: read the field now, close over that value, and the handler
stops caring what happens to the config afterward.

## Resources

- [`net/http.ServeMux`](https://pkg.go.dev/net/http#ServeMux) — the production router this pattern generalizes to.
- [Go spec: Pointer types](https://go.dev/ref/spec#Pointer_types) — why closing over `*RouterConfig` captures the mutation stream, not a value.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [13-rule-engine-config-pointer-mutation.md](13-rule-engine-config-pointer-mutation.md) | Next: [15-sliding-window-rate-limit-ticker-capture.md](15-sliding-window-rate-limit-ticker-capture.md)
