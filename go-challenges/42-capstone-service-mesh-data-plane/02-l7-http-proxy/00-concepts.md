# 2. L7 HTTP Proxy — Concepts

Moving from L4 to L7 means the proxy stops shuttling opaque bytes and starts parsing the application protocol, then makes routing decisions from what it reads. An L7 HTTP reverse proxy reads the request line, the `Host` header, the path, and arbitrary headers; selects an upstream; rewrites headers; and streams the response back without ever buffering the payload in memory. The hard part is not plumbing but semantic correctness: hop-by-hop headers must be stripped, proxy-identification headers must be chained rather than overwritten, per-route timeouts must cancel the upstream context, and the response body must never be held whole in a buffer. This file is the conceptual foundation; read it once and each exercise — the request router, the header-rewriting layer, and the integrating reverse proxy — becomes a straightforward build of one self-contained Go module.

## Concepts

### L4 vs L7: what HTTP parsing enables

An L4 proxy copies raw bytes between two TCP sockets. It cannot distinguish a request to `/api/users` from one to `/static/logo.png` because both are an opaque byte stream to it. An L7 HTTP proxy parses the HTTP/1.1 framing, so it sees the request line, the `Host` header, and every named field. Because it sees them, it can route `/api/` to a backend cluster and `/static/` to a CDN origin; route `api.example.com` to one pool and `web.example.com` to another on the same listener port; strip or inject headers before forwarding; enforce per-route timeouts rather than a single idle timeout; and return a structured 502 or 504 instead of a raw TCP reset. Service-mesh data planes such as Envoy and linkerd-proxy operate exactly this way for HTTP workloads.

### Routing: ordered rules, first match wins

A routing table is a slice of route values. Each rule carries optional host and path-prefix constraints; a rule with an empty constraint matches everything. The router walks the slice in declaration order and returns the first match. Three consequences follow. More specific rules must come first (`/api/v2/` before `/api/`), because the first match wins and a broad prefix placed first would shadow the narrow one. A catch-all route — empty host and empty path prefix — placed last acts as a default. And because an HTTP/1.1 `Host` header often carries a port suffix (`api.example.com:443`), the router strips the port before comparing, or every virtual-host match against a port-bearing request would fail.

### Hop-by-hop headers: RFC 9110 section 7.6.1

HTTP headers are either end-to-end or hop-by-hop. End-to-end headers are carried through the entire chain — `Content-Type`, `Authorization`, custom `X-*` headers. Hop-by-hop headers concern only the two endpoints of a single connection and must not be forwarded. The standard hop-by-hop set is `Connection`, `Keep-Alive`, `Proxy-Authenticate`, `Proxy-Authorization`, `TE`, `Trailers`, `Transfer-Encoding`, and `Upgrade`.

There is also a dynamic extension. If a sender includes `Connection: close, X-Custom`, the `X-Custom` header is *nominated* as hop-by-hop for that single hop and must also be removed before forwarding. Correct stripping is therefore two passes: first parse every `Connection` header value and delete each nominated header, then delete the standard set. Forwarding `Transfer-Encoding` is the most common mistake here — the upstream then receives a double-chunked body or a garbled Content-Length, because the proxy already decoded the chunked framing once.

### Proxy identification headers

A proxy chain must let upstream services reconstruct the original client context, and the rule for each forwarding header is different. `X-Forwarded-For` carries the client IP; if the header already exists, having been set by a previous proxy, the new address is *appended* (`10.0.0.1, 10.0.0.2`), never overwritten — overwriting erases the original client. `X-Forwarded-Host` carries the `Host` the client originally sent and is set once; a downstream proxy that already finds it set must leave it alone. `X-Forwarded-Proto` carries the client scheme, `http` or `https`, determined by whether `r.TLS != nil` at the edge.

### Body streaming and connection pooling

`io.Copy(dst, src)` copies between an `io.Reader` and an `io.Writer` in 32 KB chunks without holding the whole payload; passing `r.Body` directly as the outbound request body does the same for the upload direction. An `http.Client` with a shared `http.Transport` maintains a connection pool per upstream, so idle connections are reused and the TCP handshake leaves the hot path. Two invariants keep that pool healthy. Always read and close `resp.Body` after every `client.Do`, even on error paths: a body left undrained pins the connection out of the pool, and under load the proxy leaks file descriptors until the OS connection limit bites. And set `CheckRedirect` to return `http.ErrUseLastResponse` so the proxy does not silently follow a 3xx — redirects are application-level and must be relayed to the original client unchanged.

### 502 vs 504

HTTP defines two distinct upstream-error codes, and conflating them is wrong. 502 Bad Gateway means the upstream returned an invalid response or was not reachable at all — connection refused, DNS failure. 504 Gateway Timeout means the upstream was reachable but did not respond within the allowed time. The proxy distinguishes the two by checking `ctx.Err()` after a failed `client.Do`: if the context was cancelled or timed out, return 504; otherwise return 502. Treating every `client.Do` error as a timeout reports 504 for a connection-refused upstream, which is simply the wrong status.

### Per-route timeouts and context layering

Each route may carry its own timeout; a zero value inherits the proxy's default. The handler derives a context with `context.WithTimeout(r.Context(), timeout)`, so the upstream request is cancelled both by the route deadline *and* by the client disconnecting — the inbound request's context is the parent, so a client that hangs up cancels the upstream call as a second, independent source of cancellation. The derived context's `cancel` is always deferred, so no timer leaks regardless of how the handler returns.

## Common Mistakes

### Forwarding Transfer-Encoding to the upstream

Copying all request headers verbatim and forwarding leaves `Transfer-Encoding` (and the rest of the hop-by-hop set) in the outbound request. The upstream then tries to re-interpret chunked framing the proxy already decoded, producing a garbled body. Fix: call the hop-by-hop strip immediately after cloning the headers, before forwarding.

### Overwriting X-Forwarded-For instead of appending

`h.Set("X-Forwarded-For", clientIP)` erases any value a previous proxy set, so the upstream loses the original client address. Fix: if the header already exists, append `", " + clientIP`; only set it outright when it is absent.

### Not draining the response body before returning

Returning from the handler without `resp.Body.Close()` — on a partial read or an early error path — prevents the `http.Transport` from reusing the connection. Under load the proxy leaks file descriptors and goroutines. Fix: `defer resp.Body.Close()` immediately after a successful `client.Do`.

### Returning 504 for a connection-refused error

Treating all `client.Do` errors as timeouts returns 504 when the upstream is simply not running, but RFC 9110 reserves 504 for timeout and 502 for an absent or invalid response. Fix: inspect `ctx.Err()` to tell a timeout from a connection error.

### Placing a catch-all route before specific rules

A catch-all (empty host and path prefix) listed first absorbs every request, leaving the specific `/api/` rule as dead code. Fix: order rules most-specific first; the catch-all belongs last.

### Not setting the outbound Host

Omitting `outReq.Host = r.Host` lets the client fill in the upstream's own address as the `Host`, so virtual-hosting upstreams see `Host: 127.0.0.1:PORT` instead of `api.example.com` and may answer 400 or 421. Fix: set `outReq.Host = r.Host` explicitly so the upstream sees the virtual host the client requested.

---

Next: [01-request-router.md](01-request-router.md)
