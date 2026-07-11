# 1. L4 TCP Proxy — Concepts

Building an L4 TCP proxy is the lowest-level challenge in a service mesh data plane. The proxy accepts TCP connections from downstream clients, establishes corresponding connections to upstream backends, and copies bytes bidirectionally without inspecting the payload. That description undersells the difficulty: production implementations must handle TCP half-close correctly, enforce idle timeouts without goroutine leaks, track per-connection metrics under concurrent write access, support graceful shutdown that drains in-flight traffic, and reject new connections when resource limits are reached. The interaction between half-close and idle timeouts is where most implementations fail. Read this file once and the two exercises that follow — a complete tested proxy package and a standalone round-robin balancer — become a matter of typing in code you already understand.

## Concepts

### TCP Half-Close

A TCP connection carries two independent byte streams. Each stream can be terminated independently: when a side calls `shutdown(SHUT_WR)` it sends a FIN and signals end-of-stream in that direction while the other direction remains open. This is half-close.

In Go, `net.Conn.Close()` terminates both directions simultaneously. To close only one direction, type-assert the connection to `*net.TCPConn` and call `CloseWrite()` or `CloseRead()`:

```go
if tc, ok := conn.(*net.TCPConn); ok {
	tc.CloseWrite() // sends FIN without closing the read side
}
```

An L4 proxy must propagate half-close correctly. When the downstream client closes its write side, the proxy must call `CloseWrite()` on the upstream connection — not `Close()`. Calling `Close()` terminates both directions and prevents the upstream from delivering any data still buffered in transit.

`net.Listen` and `net.DialTimeout` return `net.Conn`. The underlying value is a `*net.TCPConn` when the network is `"tcp"`. The type assertion is the correct way to access `CloseWrite()`; the interface does not expose it. When the destination is not a `*net.TCPConn` — for example a `net.Pipe` connection in a test — the only available primitive is `Close()`, so the copy helper falls back to it.

### Bidirectional Copy With Two Goroutines

The canonical pattern for proxying is one goroutine per direction:

```go
var wg sync.WaitGroup
wg.Add(2)
go func() {
	defer wg.Done()
	copyHalfClose(upstream, downstream) // downstream -> upstream
}()
go func() {
	defer wg.Done()
	copyHalfClose(downstream, upstream) // upstream -> downstream
}()
wg.Wait()
```

`io.Copy` returns when its source returns a non-nil error or EOF. At that point the goroutine calls `CloseWrite()` on the destination, propagating the half-close signal. The other goroutine continues until its own source also signals EOF. `wg.Wait()` ensures the `handle` function does not return — and does not close the underlying connections — until both directions are done.

A common mistake is calling `Close()` on the upstream connection when one copy direction finishes, which kills the second goroutine's reads and silently discards in-flight data.

`wg.Wait()` also makes reading the two byte counters race-free. Each goroutine writes exactly one variable (`sent` or `recv`) before its `wg.Done()`, and `WaitGroup` guarantees those writes are visible to the goroutine that returns from `wg.Wait()`. No extra synchronization is needed, and Go's race detector agrees.

### Idle Timeouts With SetDeadline

`net.Conn.SetDeadline(t time.Time)` sets an absolute deadline on all future I/O for that connection. When the deadline is exceeded, any in-progress or future `Read` or `Write` call returns an error wrapping `os.ErrDeadlineExceeded`. The deadline applies to the connection, not to a single call.

A simple, effective idle timeout for an L4 proxy is a fixed deadline set once per connection before the copy goroutines start:

```go
deadline := time.Now().Add(idleTimeout)
downstream.SetDeadline(deadline)
upstream.SetDeadline(deadline)
```

When either connection goes idle for `idleTimeout` duration, `io.Copy` returns with a deadline-exceeded error, the half-close fires, and the other goroutine's source also closes, ending the handler cleanly.

A more precise approach — resetting the deadline after each successful read — requires a custom `io.Reader` wrapper. For most L4 proxy use cases, a fixed deadline per connection is the correct trade-off: it terminates truly idle connections while still allowing long transfers to complete within the window.

### Connection Tracking Under Concurrent Access

The proxy handles hundreds of connections concurrently. The tracking table requires safe concurrent access. The canonical approach is a `sync.RWMutex`-protected `map[uint64]*ConnRecord`:

- Write locks are held only during insert (`addConn`) and delete (`removeConn`).
- Read locks are held during snapshot reads (`Conns()`).
- Aggregate counters (`totalAccepted`, `bytesSent`, etc.) use `sync/atomic.Int64` to avoid holding the map lock for counter increments.

Using `sync.Map` is an alternative, but `sync.Map` is optimized for high-read, low-write scenarios with stable keys. A connection tracking table has frequent inserts and deletes, which makes a `sync.RWMutex` map a better fit.

### Graceful Shutdown and Connection Draining

Graceful shutdown has two phases:

1. Stop accepting new connections: close the listener, which causes `Accept()` to return immediately with an error.
2. Drain active handlers: wait for all in-flight connections to finish.

The idiomatic Go approach is to pass a `context.Context` to `Serve`. A background goroutine closes the listener when the context is cancelled. The `Serve` function tracks active handlers with a `sync.WaitGroup` and defers `wg.Wait()` so it does not return until all handlers have exited:

```go
func (p *Proxy) Serve(ctx context.Context, ln net.Listener) error {
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	defer p.handlers.Wait()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("proxy: accept: %w", err)
		}
		p.handlers.Add(1)
		go func() {
			defer p.handlers.Done()
			p.handle(conn)
		}()
	}
}
```

The caller controls the drain timeout by wrapping the context. `context.WithTimeout(context.Background(), 10*time.Second)` makes `Serve` return after at most ten seconds of draining once the context is cancelled.

The detail that makes the drain correct is where `handlers.Add(1)` is called. It must run in the `for` loop body, in the parent goroutine, before the handler goroutine starts. If `Add(1)` ran inside the goroutine instead, a handler scheduled after `defer p.handlers.Wait()` began could let `Wait()` return while it was still in flight. `Add` in the parent and `Done` in the child is the rule.

### Round-Robin Backend Selection

A mutex-protected index distributes connections across backends in round-robin order:

```go
func (rr *roundRobin) next() string {
	rr.mu.Lock()
	addr := rr.addrs[rr.idx%len(rr.addrs)]
	rr.idx++
	rr.mu.Unlock()
	return addr
}
```

Using an `atomic.Int64` for the index with `Load(); idx%len; Store(idx+1)` is a check-then-act race: two goroutines can read the same index, compute the same modulo, and select the same backend, while the counter drifts arbitrarily. The mutex holds the whole read-compute-increment sequence as one step. The critical section is three operations and the mutex overhead is negligible compared to the subsequent TCP dial. The second exercise studies this balancer in isolation.

## Common Mistakes

### Calling Close Instead of CloseWrite When One Direction Finishes

Wrong: `dst.Close()` when `io.Copy` returns, regardless of connection type. The peer's read side gets RST instead of FIN, which looks like an error rather than a clean EOF, and any data the peer buffered for the other direction is discarded. Fix: type-assert to `*net.TCPConn` and call `tc.CloseWrite()`, which sends FIN on the write side only and leaves the read side open for the other goroutine. Fall back to `Close()` only when the destination is not a TCP connection.

### Using an Atomic Index for Round-Robin

Wrong: an atomic load, a modulo, then an atomic store of the incremented value. Two goroutines read the same index, compute the same backend, and both increment from the same value, so the distribution is uneven and the counter drifts. Fix: hold a mutex for the entire read-compute-increment sequence.

### Setting a Deadline on the Listener Instead of the Connection

Wrong: `ln.SetDeadline(time.Now().Add(timeout))` to implement per-connection timeouts. The listener's deadline applies to `Accept()`, not to accepted connections, so `Accept()` starts returning errors while already-accepted connections keep running with no deadline. Fix: call `conn.SetDeadline(...)` on the accepted `net.Conn` after `Accept()` returns, before handing it to the handler.

### Calling handlers.Add(1) Inside the Handler Goroutine

Wrong: `p.handlers.Add(1)` as the first statement of the handler goroutine. If that goroutine is scheduled after `defer p.handlers.Wait()` runs, `Wait()` returns while the handler is still in flight and the drain misses it. Fix: call `Add(1)` in the parent — the `for` loop body — before starting the goroutine, with `Done` deferred in the child.

### Treating sent/recv After wg.Wait() as a Race

Not a bug, but a question the race detector prompts: reading `sent` after `wg.Wait()` when a goroutine wrote it is safe. `sync.WaitGroup` guarantees that everything a goroutine did before its `wg.Done()` is visible to the goroutine returning from `wg.Wait()`. The race detector knows this, so no extra synchronization is needed.

Next: [01-l4-tcp-proxy.md](01-l4-tcp-proxy.md)
