# 9. Control Plane gRPC — Concepts

The xDS protocol separates configuration ownership from configuration execution: the control plane holds authority over cluster topology, routing rules, and listener bindings; the data-plane proxies apply them. The interface between the two is a family of gRPC bidirectional-streaming APIs over which the control plane pushes typed resources — Listeners, Routes, Clusters, Endpoints — to every connected proxy, each of which either ACKs a version into its running configuration or NACKs it with an error. This file is the conceptual foundation: read it once and you will have everything needed to reason through the four exercises, which build the pieces as independent, self-contained Go modules — the atomic config store, the ACK/NACK protocol, the reconnect state machine, and finally a real gRPC bidirectional control-plane stream.

Three properties make this harder than a simple push-on-change design. ACK/NACK semantics require the server to track per-client state across a long-lived stream rather than per-request state. Atomic snapshots require all resource types to be mutually consistent, so a Route that references a Cluster cannot go live before that Cluster's Endpoints are ready. And graceful disconnection requires the proxy to keep serving traffic under the last-known-good configuration while the control plane is unreachable, then resync on reconnect.

## Concepts

### The xDS Resource Hierarchy

xDS defines four resource types in a strict dependency order:

- Listener (LDS): a bound address and port with a filter chain. The filter chain selects a Route table by name.
- Route (RDS): a set of virtual hosts and route rules. Each rule maps a request prefix to a Cluster name.
- Cluster (CDS): a named upstream service with a load-balancing policy and health-check configuration.
- Endpoint (EDS): the physical host:port pairs that belong to a Cluster's load-balancing pool.

The dependency order is the reason atomic snapshots exist: if a Route rule names a Cluster that the data plane has not yet applied, the proxy enters an inconsistent state and drops requests. A single atomic swap of a complete configuration eliminates the window between partial applies. In production each of these resources is a protobuf-generated type from the Envoy v3 API; the exercises model them as small Go structs and carry the real type-URL strings (such as `type.googleapis.com/envoy.config.listener.v3.Listener`) verbatim, because those strings appear unchanged on the wire.

### Bidirectional Streaming and the ACK/NACK Protocol

The xDS streaming RPC is, in proto, a single bidirectional method:

```proto
service DiscoveryService {
  rpc StreamResources(stream DiscoveryRequest)
      returns (stream DiscoveryResponse);
}

message DiscoveryRequest {
  string          node_id        = 1;
  string          type_url       = 2;
  string          version_info   = 4;
  string          response_nonce = 5;
  google.rpc.Status error_detail  = 6;
}

message DiscoveryResponse {
  string version_info = 1;
  repeated google.protobuf.Any resources = 2;
  string type_url     = 3;
  string nonce        = 4;
}
```

The nonce creates an explicit correlation between a response and the ACK or NACK that follows it:

1. The client sends `DiscoveryRequest{type_url: "...", version_info: "", response_nonce: ""}` to subscribe.
2. The server sends `DiscoveryResponse{nonce: "0000000000000001", version_info: "3", resources: [...]}`.
3. If the client applies the resources, it sends `DiscoveryRequest{response_nonce: "0000000000000001", version_info: "3"}` (an ACK). If it fails, it sends `DiscoveryRequest{response_nonce: "0000000000000001", error_detail: {...}}` (a NACK).
4. On NACK the server re-pushes the current config under a new nonce so the client can retry.

The version on an ACK is informational — it tells the control plane which version the proxy is now running — while the nonce is the load-bearing field that decides which response is being acknowledged. A response that is never ACKed or NACKed is not well-behaved; production servers time the response out and drop the stream. The subtle point is that the server must track this state per resource type per stream: a client can be three versions behind on Endpoints while fully caught up on Listeners, and the tracker has to represent both at once.

### Atomic Snapshots

An `atomic.Pointer[ConfigSnapshot]` (Go 1.19+) gives a lock-free read path. `Apply` increments a version counter with `atomic.Uint64.Add` first, then stores the new snapshot pointer. A reader that loads the pointer after the store always observes a snapshot whose `Version` field is consistent with the counter — no reader ever sees a half-updated configuration, because the swap publishes the entire new struct in one indivisible store. This is what lets the proxy read its current config on the hot request path without ever taking a lock and without ever colliding with a concurrent control-plane update.

### Coalescing Notifications

A buffered channel of capacity 1 with a non-blocking send decouples the Apply rate from the push rate:

```
ch := make(chan struct{}, 1)  // capacity 1
select {
case ch <- struct{}{}:
default:                       // already has a pending notification; do not block
}
```

If Apply is called ten times in rapid succession before any client processes the notification, the channel holds exactly one pending signal. The client wakes once, reads the latest snapshot (which already reflects all ten changes), and pushes a single response. This pattern keeps a burst of config changes from turning into a burst of gRPC sends, and it is why the notification channel carries `struct{}{}` rather than the snapshot itself: the signal means "something changed, go read the current state," not "here is change number seven."

### Graceful Disconnection and Exponential Backoff

The client tracks its connection lifecycle as a small state machine:

```
idle -> connecting -> connected -> disconnected -> reconnecting -> connecting -> ...
```

The data-plane proxy holds a reference to the last applied snapshot regardless of where in the lifecycle the client sits. When the stream drops, the proxy keeps routing under that cached snapshot while the state machine waits in `disconnected`, transitions to `reconnecting` (where it sleeps an exponentially backed-off duration), then re-dials. Separating connection state from config state is what makes a control-plane outage survivable: an unreachable control plane degrades the freshness of the config, not the availability of the proxy.

Exponential backoff doubles the wait on each failed attempt and caps it at a ceiling. Without jitter it causes a thundering herd: when a control plane restarts, every proxy that disconnected at the same instant backs off by the same schedule and re-dials in lockstep, hammering the freshly started server. The fix is to add a random component:

```
wait = Backoff(base, max, attempt) + time.Duration(rand.Int63n(int64(base)))
```

The state machine also has to reject illegal transitions. A client that is merely `connecting` has not yet disconnected, so a jump straight to `reconnecting` is a bug; encoding the legal edges in a table and refusing everything else turns that class of bug into an immediate, testable error rather than a silent corruption of the lifecycle.

## Common Mistakes

### Sending on a Cancelled gRPC Stream

Wrong: a push goroutine that ignores the error from `stream.Send`. After the stream context is cancelled, `Send` returns `codes.Canceled` or `io.EOF`; ignoring it means the goroutine silently drops every later update and the server loop never learns the client is gone. Fix: check every `Send` error and return from the goroutine the moment one appears, propagating it so the stream handler can clean up its subscription.

### Applying a Partial Snapshot

Wrong: exposing per-type apply methods (`ApplyRoutes`, then `ApplyClusters`) so a reader can observe Route rules that reference Clusters which do not yet exist. Requests matching those routes are dropped or sent to stale endpoints until the second write lands. Fix: always swap a complete snapshot in one atomic store. The `atomic.Pointer` swap is all-or-nothing — the proxy sees either the entire old set or the entire new set, never a mixture.

### Forgetting to Re-Subscribe After Reconnection

Wrong: subscribing to the store once at startup with a deferred cancel, then reconnecting in a loop. After the first disconnect the cleanup removes the node from the subscriber map, and the reconnected stream never receives push notifications again — it only ever sees the snapshot it fetched on its initial subscription. Fix: subscribe at the start of each stream invocation with a cancel scoped to that invocation, so every reconnect re-registers for notifications.

### Not Re-Pushing After a NACK

Wrong: recording a NACK and doing nothing else. The client rejected a broken configuration; since the server does not re-push, the client cannot apply what it just rejected, will not ACK, and the server believes it is on the older ACKed version. Configuration drift accumulates silently. Fix: on NACK, immediately re-push the current snapshot under a new nonce so the client gets a fresh, correlatable response to retry against. Ignoring a NACK leaves the client stranded on a stale config indefinitely.

---

Next: [01-config-snapshot-store.md](01-config-snapshot-store.md)
