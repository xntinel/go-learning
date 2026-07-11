# 10. Full Data Plane — Concepts

Assembling nine independently built components into a cohesive data plane is a different class of difficulty than building any one of them. The hard part is not any single algorithm; it is the wiring: mTLS peer identity must flow into the rate limiter, health-checker state must update the load balancer's backend pool, circuit-breaker transitions must be observable in metrics, and configuration must update atomically without dropping in-flight requests. This file is the conceptual foundation. Read it once and you will have what you need to reason through both exercises, which build the assembled plane as two self-contained Go modules: the configuration-and-routing core (validation plus atomic hot-reload over a healthy backend pool), and the full wired server (the middleware pipeline, the component interfaces, the admin API, and the startup/shutdown lifecycle stitched into one process).

## Concepts

### The Integration Problem

Building nine separate components — an L4 proxy, an L7 proxy, mTLS termination, a load balancer, a health checker, traffic management, a rate limiter, observability, and an xDS client — and making them work together is an integration problem, not an algorithmic one. The components have implicit contracts with each other:

- The mTLS layer knows the authenticated peer identity. The rate limiter needs that identity to apply per-service rate limits. If the identity is not carried through the request context, rate limits silently apply to the wrong key (the empty string or a shared key), making them ineffective.
- The health checker knows which backends are alive. The load balancer needs that knowledge before routing. If health state and backend selection are not synchronized under a shared lock or via atomic flags, requests are sent to dead backends even after the health check fires.
- The circuit breaker trips on error rate. The metrics system records those errors. The two must agree on what constitutes a failure: a 5xx from the upstream, not a 4xx from a policy rejection.

A production data plane (Envoy, Linkerd-proxy) solves this by making each component a pluggable interface and wiring them together at construction time. Both exercises use the same approach: each component is an interface, no-op implementations serve as defaults, and real implementations are injected via functional options. No component holds a pointer to another component's concrete type.

### Dependency Order at Startup and Shutdown

Components have dependencies that determine initialization order. A dependency violation — starting the listener before the rate limiter is ready — means the first request may arrive before the system can process it correctly.

A safe startup order is: the metrics registry first (no dependencies; everything else depends on it), then the health checker (it updates the backend pool through a callback), then the backend pool itself (populated at construction), then the rate limiter and circuit breaker (which depend on metrics), then the mTLS configuration (which depends on certificate material), then the data-plane listener (which depends on all of the above), and finally the admin server (which depends on all of the above for accurate status).

Safe shutdown order is the reverse: stop the listener first to reject new connections, drain in-flight requests, then tear down the components those requests depend on. The drain timeout is a first-class parameter; a proxy that hangs forever on shutdown is not production-ready. `http.Server.Shutdown` with a deadline is the correct primitive — it stops accepting new connections, then waits for active handlers to finish or for the deadline to pass. Its counterpart `Close` cuts active connections off mid-stream and is wrong for a graceful drain.

### The Request Processing Pipeline as a Middleware Chain

Each policy is a middleware that wraps an `http.Handler`. Wrapping composes correctly: the first middleware in the chain is outermost — it runs first on the way in and last on the way out. The canonical chain, outermost to innermost, is:

```text
mTLS extraction → rate limit → timeout → circuit breaker → metrics → upstream proxy
```

mTLS runs first so the peer identity is available to every downstream policy. Metrics runs last (innermost, immediately before the upstream proxy) so it measures the true upstream round-trip latency, excluding the overhead of the policies above it. Using `http.Handler` middleware is the idiomatic Go pattern for an L7 pipeline. The alternative — a single monolithic request handler — cannot be unit-tested per policy; each middleware here can be tested in isolation against an `httptest.ResponseRecorder`.

A small detail makes the chain inspectable: a `responseWriter` wrapper captures the status code that the downstream handler writes, so an outer middleware can decide whether the request was a success (record success on the circuit breaker) or a failure (record an error in metrics and a failure on the breaker). The wrapper records only the first `WriteHeader` call, matching `net/http` semantics where later header writes are ignored.

### Cross-Component Wiring

The health checker runs periodic probes. When a backend's health changes, it must update the load balancer's view. The canonical pattern is a callback registered at construction, conceptually `WithOnChange(func(addr string, healthy bool) { lb.SetHealthy(addr, healthy) })`. This avoids a shared pointer between the two components: the health checker does not know the load balancer's type, and the load balancer does not know the health checker's type. Both are isolated behind their interfaces. In the configuration-and-routing core this surfaces as a `SetHealthy(cluster, addr, healthy)` method that flips the backend's atomic flag; backend selection then skips any backend whose flag is false.

The mTLS-to-rate-limiter wiring uses the request context. The mTLS middleware calls `WithPeerIdentity(r.Context(), cn)` to attach the authenticated Common Name; the rate-limit middleware calls `PeerIdentity(r.Context())` to read it. The context is the correct carrier for per-request data that must cross middleware boundaries without appearing in every function signature. The context key is an unexported named type, which is what prevents collisions with keys set by other packages.

### Configuration Validation and Atomic Updates

A configuration update that references a non-existent cluster, or duplicates a cluster name, will cause silent routing failures at runtime. Validation runs before any component is updated and returns all errors at once via `errors.Join`, not just the first, so the operator can fix everything in one pass. Each joined sub-error remains reachable with `errors.Is`, so callers can still branch on a specific sentinel (`ErrDuplicateCluster`, `ErrNoEndpoints`).

Updates are atomic from the request pipeline's perspective. The update builds the complete new backend map, validates it, and only then swaps it under a single `sync.RWMutex` acquisition. A partial update that fails midway leaves the system inconsistent — a request racing between two separate writes could see the new config but the old backend map. The validate-then-build-then-swap pattern prevents that window: readers always see either the entirely old state or the entirely new state.

Backend selection itself is round-robin over the healthy members of a cluster. A monotonically increasing atomic counter, taken modulo the backend count, picks the next index; the selector loops at most `n` times so that an all-unhealthy cluster returns an error rather than spinning. The counter is a per-cluster `atomic.Uint64`, so selection takes no lock beyond the brief read-lock that fetches the slice and counter pointers.

### Hot Restart via Socket Passing

Zero-downtime binary upgrades require the new process to take over the listener without dropping in-flight requests. The mechanism is Unix file-descriptor passing: the old process sends the listener's `*os.File` to the new process over a Unix domain socket using `syscall.Sendmsg` with `SCM_RIGHTS` ancillary data. The new process reconstructs a `net.Listener` from the received descriptor via `net.FileListener`, then the old process stops accepting new connections and drains its in-flight requests before exiting. Envoy's hot restart uses exactly this mechanism. It is a Linux-specific facility guarded by `//go:build linux`; on platforms without `SCM_RIGHTS` the restart is cold, with a brief listen gap. The implementation is platform-specific and outside the portable stdlib portion these exercises build, so it is described here but not coded.

## Common Mistakes

### Swapping Configuration Partially

Wrong: updating the active config first, then building the new backend map in a second step under a second lock acquisition. A request racing between the two updates sees the new config but the old backend map; if the new config adds a cluster, selection returns "no backends" for it until the second step completes. Fix: build the complete new backend map, validate it, and swap both the config and the backend map under a single lock acquisition.

### Placing Metrics Outside mTLS in the Chain

Wrong: putting the metrics middleware outermost. Requests rejected by rate limiting then short-circuit before reaching metrics, so they go unrecorded, and metrics measures total time including outer-policy overhead rather than upstream latency. Fix: place the metrics middleware innermost (last element passed to the chain) so it sees only requests that passed every policy and measures the real upstream round-trip.

### Ignoring the Drain Timeout

Wrong: calling `Close` on the server instead of `Shutdown(ctx)`. `Close` immediately severs all active connections, cutting off in-flight requests mid-stream and giving clients a connection reset. Fix: `Shutdown` with a context that carries a deadline. The drain timeout is a first-class parameter because the right value depends on the upstream's SLO.

### Forgetting That errors.Join and ServeMux Patterns Need a Recent go Directive

Wrong: using `errors.Join` under a `go.mod` pinned at `go 1.18`, or the `"GET /path"` method-qualified `ServeMux` patterns under anything below `go 1.22`. The build fails with an undefined symbol or the route silently fails to match the method. Fix: pin a recent `go` directive (these exercises use `go 1.26`). The `go.mod` directive controls which standard-library behavior is available; it is not a documentation hint.

### Rate Limiter Keyed on the Wrong Identity

Wrong: a rate limiter that ignores the identity argument and applies a single global limit. A single slow client then exhausts the limit for every other service, defeating the purpose of per-identity limits, which is to isolate failure domains. Fix: key the limit on the identity string carried in the context. When mTLS is not in use the identity is the empty string; apply an explicit default policy for unauthenticated callers rather than letting them share a key by accident.

---

Next: [01-config-and-atomic-reload.md](01-config-and-atomic-reload.md)
