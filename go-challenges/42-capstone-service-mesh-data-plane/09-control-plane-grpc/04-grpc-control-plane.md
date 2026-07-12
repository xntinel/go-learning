# Exercise 4: gRPC Control Plane

Now the pieces come together over a real gRPC bidirectional stream. The control plane runs a `DiscoveryService` that pushes config snapshots; the data-plane client subscribes, applies what it receives into its own local store, and ACKs every response. This exercise wires a genuine grpc-go server and client — handshake, framing, stream lifecycle, and all — over an in-memory `bufconn` pipe so the whole protocol is exercised end to end with no network and no generated protobuf code.

This module is fully self-contained: its own `go mod init`, all types defined inline, its own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
types.go              ResourceType, Listener/Cluster/Endpoint, ConfigSnapshot,
                      DiscoveryRequest, DiscoveryResponse
store.go              Store (atomic snapshot + coalescing subs), NonceGen, AckTracker
grpc.go               jsonCodec, ServiceDesc, ControlPlaneServer.StreamResources,
                      XDSClient.Run
cmd/
  demo/
    main.go           real grpc server+client over bufconn; push reaches the proxy
grpc_test.go          end-to-end push over bufconn; NACK re-push via a fake stream
```

- Files: `types.go`, `store.go`, `grpc.go`, `cmd/demo/main.go`, `grpc_test.go`.
- Implement: `ControlPlaneServer.StreamResources` (the server-side recv/notify loop) and `XDSClient.Run` (subscribe, apply, ACK), registered through a hand-written `grpc.ServiceDesc` with a JSON codec.
- Test: a client connected over `bufconn` receives the initial snapshot and a later pushed update; a NACK drives the server to re-push under a fresh nonce.
- Verify: `go test -race ./...`

Set up the module and add grpc:

```bash
mkdir -p go-solutions/42-capstone-service-mesh-data-plane/09-control-plane-grpc/04-grpc-control-plane/cmd/demo && cd go-solutions/42-capstone-service-mesh-data-plane/09-control-plane-grpc/04-grpc-control-plane
go mod edit -go=1.26
go get google.golang.org/grpc@latest
```

### Why a JSON codec instead of generated protobuf

A production xDS server transmits protobuf-generated types and registers a service via `protoc-gen-go-grpc` output. That codegen step needs `protoc` and a pile of `.pb.go` files, none of which a self-contained teaching module can carry. The mechanism that lets this exercise run a *real* gRPC stream anyway is grpc-go's pluggable codec: `encoding.RegisterCodec` installs a marshaller keyed by name, and `grpc.ForceCodec` tells a call to use it. We register a `jsonCodec` whose `Marshal`/`Unmarshal` are `json.Marshal`/`json.Unmarshal`, so `DiscoveryRequest` and `DiscoveryResponse` travel as JSON over the wire. Everything else — the HTTP/2 framing, the stream handshake, flow control, the `RecvMsg`/`SendMsg` lifecycle — is the genuine grpc-go machinery. Swap the codec for the protobuf one and register generated types, and the same server and client logic is production xDS.

Registration without generated code means writing the `grpc.ServiceDesc` by hand. The descriptor names the service (`xds.v1.DiscoveryService`), declares one stream method that is both client- and server-streaming, and supplies a handler that adapts the raw `grpc.ServerStream` into a typed interface. The `HandlerType` field must be an interface type — grpc reflects on it to check the implementation satisfies it — so we use `(*any)(nil)`, which every implementation trivially satisfies.

### The grpcStream seam and the two-goroutine server loop

`StreamResources` is written against a narrow `grpcStream` interface — `Send`, `Recv`, `Context` — not against the concrete grpc stream. That seam is the key testing device: the real server wraps `grpc.ServerStream` in an adapter that satisfies the interface via `SendMsg`/`RecvMsg`, while a test supplies an in-memory fake backed by channels. The same server logic runs in both, so the NACK-re-push path can be tested without standing up a network at all.

The server loop has to wait on two event sources at once: requests arriving from the client (subscriptions, ACKs, NACKs) and config changes arriving from the store. `stream.Recv` blocks, so a goroutine drains it into `recvCh`, freeing the main `select` to also watch the store's notification channel. The main loop handles three cases. A request with no nonce is an initial subscription: record the resource type and send the current snapshot. A request with a nonce and an error detail is a NACK: re-push the current snapshot under a *new* nonce so the client has a fresh, correlatable response to retry. A request with a nonce and no error is an ACK: record it and wait. When the store fires its notification, the loop re-sends every resource type the client has subscribed to. The subscription is established lazily on the first request (so the server learns the node ID), and a deferred `unsub` removes it when the stream ends, which is what keeps the store from leaking a channel per disconnected proxy.

The client side mirrors this with roles reversed. `Run` opens the stream, sends one subscription request per resource type, then loops on `RecvMsg`: each response is merged into the local store via `apply` (preserving the resource types the response did not carry, so a Cluster push does not wipe the Listeners), and an ACK echoing the response's nonce goes back. An `io.EOF` from `RecvMsg` is a clean stream close, not an error.

Create `types.go`:

```go
package controlplane

// ResourceType is the full gRPC type URL used in xDS discovery requests and
// responses. These constants match the real Envoy v3 API type URLs.
type ResourceType string

const (
	TypeListener ResourceType = "type.googleapis.com/envoy.config.listener.v3.Listener"
	TypeRoute    ResourceType = "type.googleapis.com/envoy.config.route.v3.RouteConfiguration"
	TypeCluster  ResourceType = "type.googleapis.com/envoy.config.cluster.v3.Cluster"
	TypeEndpoint ResourceType = "type.googleapis.com/envoy.config.endpoint.v3.ClusterLoadAssignment"
)

// Listener is a minimal Go representation of an xDS Listener resource.
type Listener struct {
	Name    string
	Address string
	Port    uint32
}

// Cluster is a minimal xDS Cluster resource.
type Cluster struct {
	Name     string
	LBPolicy string
}

// Endpoint is a single host within a Cluster's load-balancing pool.
type Endpoint struct {
	ClusterName string
	Address     string
	Port        uint32
	Healthy     bool
}

// ConfigSnapshot is an immutable, version-stamped set of resources. Applying a
// snapshot atomically replaces the entire running configuration.
type ConfigSnapshot struct {
	Version   uint64
	Listeners []Listener
	Clusters  []Cluster
	Endpoints []Endpoint
}

// DiscoveryRequest is sent by the data-plane client to subscribe or to
// ACK/NACK a previously received DiscoveryResponse.
type DiscoveryRequest struct {
	NodeID        string
	ResourceType  ResourceType
	VersionInfo   string
	ResponseNonce string
	ErrorDetail   string
}

// DiscoveryResponse is sent by the control plane to push resource updates.
type DiscoveryResponse struct {
	ResourceType ResourceType
	VersionInfo  string
	Nonce        string
	Listeners    []Listener
	Clusters     []Cluster
	Endpoints    []Endpoint
}
```

Create `store.go`:

```go
package controlplane

import (
	"fmt"
	"sync"
	"sync/atomic"
)

// Store is the control-plane configuration store. It holds the current
// ConfigSnapshot behind an atomic pointer so reads never block writers, and it
// fans out coalesced change notifications to subscribed clients.
type Store struct {
	snapshot atomic.Pointer[ConfigSnapshot]
	version  atomic.Uint64

	subMu sync.Mutex
	subs  map[string]chan struct{}
}

// NewStore returns an empty Store at version 0.
func NewStore() *Store {
	s := &Store{subs: make(map[string]chan struct{})}
	s.snapshot.Store(&ConfigSnapshot{})
	return s
}

// Apply atomically replaces the snapshot, increments the version, and notifies
// every subscriber without blocking.
func (s *Store) Apply(snap ConfigSnapshot) {
	snap.Version = s.version.Add(1)
	s.snapshot.Store(&snap)
	s.subMu.Lock()
	for _, ch := range s.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	s.subMu.Unlock()
}

// Snapshot returns the current snapshot. Callers must not mutate it.
func (s *Store) Snapshot() *ConfigSnapshot { return s.snapshot.Load() }

// Version returns the current version counter.
func (s *Store) Version() uint64 { return s.version.Load() }

// Subscribe registers nodeID and returns a coalescing notification channel plus
// a cancel function that removes the subscription.
func (s *Store) Subscribe(nodeID string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	s.subMu.Lock()
	s.subs[nodeID] = ch
	s.subMu.Unlock()
	return ch, func() {
		s.subMu.Lock()
		delete(s.subs, nodeID)
		s.subMu.Unlock()
	}
}

// NonceGen generates unique, monotonic nonces for a single stream.
type NonceGen struct{ n atomic.Uint64 }

// Next returns the next nonce as a zero-padded 16-character hex string.
func (g *NonceGen) Next() string { return fmt.Sprintf("%016x", g.n.Add(1)) }

// AckTracker records per-resource-type ACK state for one connected client.
type AckTracker struct {
	mu      sync.Mutex
	acked   map[ResourceType]string
	pending map[ResourceType]string
}

// NewAckTracker returns an empty tracker.
func NewAckTracker() *AckTracker {
	return &AckTracker{
		acked:   make(map[ResourceType]string),
		pending: make(map[ResourceType]string),
	}
}

// Sent records that a response with nonce was dispatched for rt.
func (t *AckTracker) Sent(rt ResourceType, nonce string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pending[rt] = nonce
}

// Ack confirms the client applied the config; it returns true when nonce
// matches the last sent response for rt.
func (t *AckTracker) Ack(rt ResourceType, nonce string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.pending[rt] == nonce {
		t.acked[rt] = nonce
		return true
	}
	return false
}

// LastAcked returns the last nonce the client confirmed for rt.
func (t *AckTracker) LastAcked(rt ResourceType) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.acked[rt]
}
```

Create `grpc.go`:

```go
package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"google.golang.org/grpc"
	"google.golang.org/grpc/encoding"
)

// jsonCodec lets us run a real gRPC bidirectional stream without generated
// protobuf code: requests and responses are marshalled as JSON. A production
// xDS server uses the protobuf codec with generated .pb.go types; the wire
// framing, stream lifecycle, and ACK/NACK logic are identical.
const codecName = "xds-json"

type jsonCodec struct{}

func (jsonCodec) Marshal(v any) ([]byte, error)      { return json.Marshal(v) }
func (jsonCodec) Unmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }
func (jsonCodec) Name() string                       { return codecName }

func init() { encoding.RegisterCodec(jsonCodec{}) }

const (
	serviceName = "xds.v1.DiscoveryService"
	methodName  = "StreamResources"
	fullMethod  = "/" + serviceName + "/" + methodName
)

// grpcStream is the narrow interface the server logic needs. The real gRPC
// server stream satisfies it through an adapter, and tests satisfy it with an
// in-memory fake, so StreamResources can be exercised without a network.
type grpcStream interface {
	Send(*DiscoveryResponse) error
	Recv() (*DiscoveryRequest, error)
	Context() context.Context
}

// serverAdapter wraps a grpc.ServerStream as a typed grpcStream.
type serverAdapter struct{ grpc.ServerStream }

func (a serverAdapter) Send(r *DiscoveryResponse) error { return a.SendMsg(r) }

func (a serverAdapter) Recv() (*DiscoveryRequest, error) {
	req := new(DiscoveryRequest)
	if err := a.RecvMsg(req); err != nil {
		return nil, err
	}
	return req, nil
}

var streamDesc = grpc.StreamDesc{
	StreamName:    methodName,
	ServerStreams: true,
	ClientStreams: true,
}

var serviceDesc = grpc.ServiceDesc{
	ServiceName: serviceName,
	HandlerType: (*any)(nil),
	Streams: []grpc.StreamDesc{{
		StreamName:    methodName,
		ServerStreams: true,
		ClientStreams: true,
		Handler: func(srv any, stream grpc.ServerStream) error {
			return srv.(*ControlPlaneServer).StreamResources(serverAdapter{stream})
		},
	}},
}

// RegisterServer registers srv on a gRPC server using the JSON codec.
func RegisterServer(gs *grpc.Server, srv *ControlPlaneServer) {
	gs.RegisterService(&serviceDesc, srv)
}

// ControlPlaneServer implements the DiscoveryService gRPC server backed by a
// Store. It maintains per-stream state and pushes config changes to the client.
type ControlPlaneServer struct{ store *Store }

// NewControlPlaneServer returns a server backed by store.
func NewControlPlaneServer(store *Store) *ControlPlaneServer {
	return &ControlPlaneServer{store: store}
}

// StreamResources handles one bidirectional xDS stream. A recv goroutine reads
// requests into recvCh so the main loop can also wait on store notifications.
func (srv *ControlPlaneServer) StreamResources(stream grpcStream) error {
	ctx := stream.Context()
	tracker := NewAckTracker()
	nonces := &NonceGen{}

	var (
		nodeID   string
		notifyCh <-chan struct{}
		unsub    func()
	)
	defer func() {
		if unsub != nil {
			unsub()
		}
	}()

	recvCh := make(chan *DiscoveryRequest, 8)
	go func() {
		defer close(recvCh)
		for {
			req, err := stream.Recv()
			if err != nil {
				return
			}
			select {
			case recvCh <- req:
			case <-ctx.Done():
				return
			}
		}
	}()

	send := func(rt ResourceType) error {
		snap := srv.store.Snapshot()
		nonce := nonces.Next()
		resp := &DiscoveryResponse{
			ResourceType: rt,
			VersionInfo:  fmt.Sprintf("%d", snap.Version),
			Nonce:        nonce,
		}
		switch rt {
		case TypeListener:
			resp.Listeners = snap.Listeners
		case TypeCluster:
			resp.Clusters = snap.Clusters
		case TypeEndpoint:
			resp.Endpoints = snap.Endpoints
		}
		if err := stream.Send(resp); err != nil {
			return err
		}
		tracker.Sent(rt, nonce)
		return nil
	}

	subscribed := map[ResourceType]bool{}
	for {
		select {
		case req, ok := <-recvCh:
			if !ok {
				return nil
			}
			if nodeID == "" {
				nodeID = req.NodeID
				notifyCh, unsub = srv.store.Subscribe(nodeID)
			}
			rt := req.ResourceType
			subscribed[rt] = true
			if req.ResponseNonce != "" {
				if req.ErrorDetail != "" {
					// NACK: re-push under a new nonce so the client can retry.
					if err := send(rt); err != nil {
						return err
					}
					continue
				}
				tracker.Ack(rt, req.ResponseNonce)
				continue
			}
			if err := send(rt); err != nil {
				return err
			}
		case <-notifyCh:
			for rt := range subscribed {
				if err := send(rt); err != nil {
					return err
				}
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// XDSClient is the data-plane client. It opens a stream, subscribes, applies
// received snapshots to its local store, and ACKs each response.
type XDSClient struct {
	cc     *grpc.ClientConn
	nodeID string
	store  *Store
}

// NewXDSClient returns a client over cc that writes received config to store.
func NewXDSClient(cc *grpc.ClientConn, nodeID string, store *Store) *XDSClient {
	return &XDSClient{cc: cc, nodeID: nodeID, store: store}
}

// Run opens the stream, subscribes to listeners, clusters, and endpoints, and
// applies every DiscoveryResponse until ctx is cancelled or the stream closes.
func (c *XDSClient) Run(ctx context.Context) error {
	cs, err := c.cc.NewStream(ctx, &streamDesc, fullMethod, grpc.ForceCodec(jsonCodec{}))
	if err != nil {
		return err
	}
	types := []ResourceType{TypeListener, TypeCluster, TypeEndpoint}
	for _, rt := range types {
		if err := cs.SendMsg(&DiscoveryRequest{NodeID: c.nodeID, ResourceType: rt}); err != nil {
			return err
		}
	}
	for {
		resp := new(DiscoveryResponse)
		if err := cs.RecvMsg(resp); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		c.apply(resp)
		ack := &DiscoveryRequest{
			NodeID:        c.nodeID,
			ResourceType:  resp.ResourceType,
			VersionInfo:   resp.VersionInfo,
			ResponseNonce: resp.Nonce,
		}
		if err := cs.SendMsg(ack); err != nil {
			return err
		}
	}
}

// apply merges one response into the local store, preserving the resource types
// the response did not carry.
func (c *XDSClient) apply(resp *DiscoveryResponse) {
	cur := c.store.Snapshot()
	next := ConfigSnapshot{
		Listeners: cur.Listeners,
		Clusters:  cur.Clusters,
		Endpoints: cur.Endpoints,
	}
	switch resp.ResourceType {
	case TypeListener:
		next.Listeners = resp.Listeners
	case TypeCluster:
		next.Clusters = resp.Clusters
	case TypeEndpoint:
		next.Endpoints = resp.Endpoints
	}
	c.store.Apply(next)
}
```

### The runnable demo

The demo stands up a real grpc-go server and client connected over a `bufconn` in-memory listener — no TCP port, no certificates — seeds one endpoint, starts the client, then applies a second endpoint on the control plane and watches it stream through to the proxy's local store.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"net"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	controlplane "example.com/control-plane"
)

func waitFor(cond func() bool) bool {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return false
}

func main() {
	// Control-plane store, seeded with one endpoint.
	server := controlplane.NewStore()
	server.Apply(controlplane.ConfigSnapshot{
		Listeners: []controlplane.Listener{{Name: "ingress", Address: "0.0.0.0", Port: 8080}},
		Clusters:  []controlplane.Cluster{{Name: "backend", LBPolicy: "round_robin"}},
		Endpoints: []controlplane.Endpoint{
			{ClusterName: "backend", Address: "10.0.0.1", Port: 9090, Healthy: true},
		},
	})

	// Wire a real gRPC server and client over an in-memory bufconn pipe.
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	controlplane.RegisterServer(gs, controlplane.NewControlPlaneServer(server))
	go gs.Serve(lis)
	defer gs.Stop()

	cc, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		fmt.Println("dial:", err)
		return
	}
	defer cc.Close()

	// Data-plane local store: the proxy applies what the control plane pushes.
	local := controlplane.NewStore()
	client := controlplane.NewXDSClient(cc, "proxy-1", local)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go client.Run(ctx)

	if waitFor(func() bool { return len(local.Snapshot().Endpoints) == 1 }) {
		fmt.Printf("initial: endpoints=%d\n", len(local.Snapshot().Endpoints))
	}

	// Push a second endpoint; the control plane streams it to the proxy.
	server.Apply(controlplane.ConfigSnapshot{
		Listeners: []controlplane.Listener{{Name: "ingress", Address: "0.0.0.0", Port: 8080}},
		Clusters:  []controlplane.Cluster{{Name: "backend", LBPolicy: "round_robin"}},
		Endpoints: []controlplane.Endpoint{
			{ClusterName: "backend", Address: "10.0.0.1", Port: 9090, Healthy: true},
			{ClusterName: "backend", Address: "10.0.0.2", Port: 9090, Healthy: true},
		},
	})

	if waitFor(func() bool { return len(local.Snapshot().Endpoints) == 2 }) {
		fmt.Printf("after push: endpoints=%d\n", len(local.Snapshot().Endpoints))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
initial: endpoints=1
after push: endpoints=2
```

### Tests

The first test runs the whole protocol over a real `bufconn`-backed gRPC connection: the client receives the seeded snapshot, then a later `Apply` on the control plane streams through and the local store grows to two endpoints. The second drives the server's NACK path directly through the `grpcStream` seam with an in-memory fake, asserting the re-push carries a fresh nonce.

Create `grpc_test.go`:

```go
package controlplane

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

func dialBuf(t *testing.T, srv *ControlPlaneServer) (*grpc.ClientConn, func()) {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	RegisterServer(gs, srv)
	go gs.Serve(lis)

	cc, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.ForceCodec(jsonCodec{})),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return cc, func() {
		cc.Close()
		gs.Stop()
		lis.Close()
	}
}

func TestStreamPushesInitialAndUpdates(t *testing.T) {
	t.Parallel()
	server := NewStore()
	server.Apply(ConfigSnapshot{
		Listeners: []Listener{{Name: "ingress", Address: "0.0.0.0", Port: 8080}},
		Clusters:  []Cluster{{Name: "backend", LBPolicy: "round_robin"}},
		Endpoints: []Endpoint{{ClusterName: "backend", Address: "10.0.0.1", Port: 9090, Healthy: true}},
	})

	cc, cleanup := dialBuf(t, NewControlPlaneServer(server))
	defer cleanup()

	local := NewStore()
	client := NewXDSClient(cc, "proxy-1", local)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go client.Run(ctx)

	waitFor(t, func() bool {
		s := local.Snapshot()
		return len(s.Listeners) == 1 && len(s.Endpoints) == 1
	}, "initial config")

	server.Apply(ConfigSnapshot{
		Listeners: []Listener{{Name: "ingress", Address: "0.0.0.0", Port: 8080}},
		Clusters:  []Cluster{{Name: "backend", LBPolicy: "round_robin"}},
		Endpoints: []Endpoint{
			{ClusterName: "backend", Address: "10.0.0.1", Port: 9090, Healthy: true},
			{ClusterName: "backend", Address: "10.0.0.2", Port: 9090, Healthy: true},
		},
	})

	waitFor(t, func() bool {
		return len(local.Snapshot().Endpoints) == 2
	}, "pushed update")
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// fakeStream drives StreamResources without a network.
type fakeStream struct {
	ctx context.Context
	in  chan *DiscoveryRequest
	out chan *DiscoveryResponse
}

func (f *fakeStream) Send(r *DiscoveryResponse) error { f.out <- r; return nil }
func (f *fakeStream) Recv() (*DiscoveryRequest, error) {
	req, ok := <-f.in
	if !ok {
		return nil, context.Canceled
	}
	return req, nil
}
func (f *fakeStream) Context() context.Context { return f.ctx }

func TestServerRepushesAfterNACK(t *testing.T) {
	t.Parallel()
	store := NewStore()
	store.Apply(ConfigSnapshot{Clusters: []Cluster{{Name: "backend"}}})
	srv := NewControlPlaneServer(store)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fs := &fakeStream{ctx: ctx, in: make(chan *DiscoveryRequest, 4), out: make(chan *DiscoveryResponse, 4)}
	go srv.StreamResources(fs)

	fs.in <- &DiscoveryRequest{NodeID: "p", ResourceType: TypeCluster}
	first := <-fs.out
	if first.Nonce == "" {
		t.Fatal("first response missing nonce")
	}
	// NACK the response; the server must re-push under a new nonce.
	fs.in <- &DiscoveryRequest{
		NodeID: "p", ResourceType: TypeCluster,
		ResponseNonce: first.Nonce, ErrorDetail: "rejected",
	}
	second := <-fs.out
	if second.Nonce == first.Nonce {
		t.Fatalf("re-push reused nonce %q", second.Nonce)
	}
}
```

## Review

The module is correct when a config applied on the control plane reaches a real gRPC client's local store, and when a NACK produces a re-push under a new nonce. The subtlest failure mode is the single-blocking-source trap: if the server loop calls `stream.Recv` inline instead of in the drain goroutine, it can never react to a store notification while waiting for a request, and pushed updates stall until the client happens to send something. `TestStreamPushesInitialAndUpdates` catches that because the second `Apply` is delivered with no client request in between — it can only arrive via the `notifyCh` branch. The `apply` method's preserve-other-types logic matters too: a naive client that overwrites the whole snapshot from each typed response would clobber Listeners every time a Cluster push lands; the test's first assertion checks Listeners and Endpoints both survive. On the protocol side, dropping the re-push on NACK strands the client on a config it rejected — `TestServerRepushesAfterNACK` asserts the fresh nonce that lets a retry be correlated. Note the JSON codec is a teaching substitute for protobuf; the grpc-go transport, the `ServiceDesc` registration, and the `RecvMsg`/`SendMsg` lifecycle are exactly what a production xDS implementation uses.

## Resources

- [grpc-go: custom `encoding.Codec`](https://pkg.go.dev/google.golang.org/grpc/encoding#Codec) — the pluggable-marshaller interface that lets this stream run on JSON instead of protobuf.
- [grpc-go: `test/bufconn`](https://pkg.go.dev/google.golang.org/grpc/test/bufconn) — the in-memory listener that exercises a full gRPC connection with no network.
- [xDS REST and gRPC protocol specification](https://www.envoyproxy.io/docs/envoy/latest/api-docs/xds_protocol) — the normative reference for `StreamResources`, type URLs, and the ACK/NACK contract.
- [Envoy go-control-plane](https://github.com/envoyproxy/go-control-plane) — a reference Go xDS server showing the real protobuf types and snapshot cache this module models.

---

Back to [03-reconnect-state-machine.md](03-reconnect-state-machine.md) | Next: [Full Data Plane](../10-full-data-plane/00-concepts.md)
