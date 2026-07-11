# 8. Full Distributed Key-Value Store

Building the individual subsystems in lessons 1-7 is the straightforward part. The hard part is the integration layer: wiring SWIM membership, consistent hashing, replication quorums, anti-entropy, hinted handoff, and the client protocol into a single binary that boots correctly, stays observable under load, and shuts down without data loss. This lesson builds that layer — the `Node` type that owns the lifecycle, the HTTP metrics endpoint, the admin CLI skeleton, and the integration test suite that validates the assembled system under realistic failure scenarios including node crashes and split-brain recovery.

```text
dkv/
  go.mod
  node.go               -- Node type: interfaces, lifecycle, vector clocks
  status.go             -- HTTP metrics endpoint (/status, /health)
  node_test.go          -- unit and integration tests using fake subsystems
  cmd/demo/main.go      -- shows Config API and VectorClock; go run ./cmd/demo
  cmd/dkv/main.go       -- production node binary (wires lesson 1-7 packages)
  cmd/dkv-admin/main.go -- admin CLI: cluster status, drain, repair
```

## Concepts

### Composing Subsystems Through Interfaces

Each of the seven prior lessons produces a subsystem with its own implementation. The integration layer declares minimal interfaces at the boundary — exactly what the Node needs to call — and accepts concrete implementations via constructor injection. This keeps the Node testable with fakes and prevents import cycles between subsystems.

```go
type Storage interface {
	Get(ctx context.Context, partition uint32, key string) (Value, error)
	Put(ctx context.Context, partition uint32, key string, val Value) error
	Delete(ctx context.Context, partition uint32, key string) error
	Scan(ctx context.Context, partition uint32, start, end string) ([]Entry, error)
	KeyCount(partition uint32) int64
}
```

The Go idiom is: the interface is defined by the consumer (the Node), not by the producer (the storage package). Small consumer-defined interfaces prevent the "interface explosion" that plagues Java-style design where producers declare interfaces to match callers they have never met.

### Bootstrap Sequence and Partition Hand-off

A joining node passes through four phases before serving traffic:

1. **Announce**: send a SWIM join message to each seed address and wait for membership acknowledgement.
2. **Ring convergence**: wait until the local hash ring epoch matches the majority view.
3. **Partition hand-off**: for each partition the node will own, stream existing key-value data from the current owner via a bulk-transfer RPC. The transfer epoch bounds what is copied; writes that arrive after the epoch follow the normal replication path which already routes to the new owner. The transfer must be idempotent: re-streaming a partially loaded partition must not corrupt it.
4. **Ready**: set state to `StateActive` and open the client listener.

Phase 3 is the most fragile. If the source fails mid-transfer, the joining node retries against the next replica on the ring.

### Graceful Shutdown and Rolling Restart

A graceful shutdown proceeds in reverse order of startup:

1. Broadcast `StateLeaving` via the SWIM layer. Clients stop routing new requests within one gossip interval (typically under 200 ms).
2. Drain in-flight requests: wait for the active request counter to reach zero, with a configurable timeout (default 30 s).
3. Hand off owned partitions: transfer data to the next replica and confirm via Merkle tree root comparison. This eliminates the need for full anti-entropy catch-up on rejoin.
4. Flush hinted handoff queue: deliver queued hints to their target nodes. Undelivered hints are persisted to disk and retried on restart.
5. Close listeners and signal background goroutines via a stop channel.

A rolling restart applies this sequence one node at a time, waiting for each node to reach `StateActive` before starting the next drain.

### Split-Brain Detection and Sibling Resolution

When a network partition heals, two subsets of the cluster may have accepted conflicting writes to the same key. The system detects this via vector clocks.

Every value carries a `VectorClock` — a `map[NodeID]uint64` where entry `[n]=k` means node n has processed k write events for this key. Two values for the same key are compared at read time and during anti-entropy:

- **Dominant**: one clock is strictly after the other (`c.Dominates(other)` returns true). The dominated value is discarded.
- **Concurrent (sibling)**: neither clock dominates the other. Both values are retained and surfaced to the client as `[]Value`. The client resolves the conflict via application-level logic, last-write-wins, or a CRDT merge.

The coordinator detects siblings during quorum collection (it receives values with diverging clocks from replicas) and during Merkle tree comparison. A per-partition sibling count is exposed in the metrics endpoint so operators can detect unresolved write conflicts.

### Operational Observability

Every node exposes `GET /status` on `<clientPort> + 1000`. The response is a JSON document containing:

- `node_id`, `addr`, `state`
- `members`: the current membership view with per-node state and ring epoch
- `metrics`: per-operation counters and P50/P95/P99 latency histograms
- `hint_queue`: pending hinted-handoff entry count per target node
- `anti_entropy`: last run timestamp, synced partitions, repaired key count

A `/health` endpoint returns 200 when the node is `StateActive` and 503 otherwise. Load balancers and rolling-restart scripts use this endpoint to gate traffic.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/dkv/cmd/demo
mkdir -p ~/go-exercises/dkv/cmd/dkv
mkdir -p ~/go-exercises/dkv/cmd/dkv-admin
cd ~/go-exercises/dkv
go mod init example.com/dkv
```

This is a library package plus executables. Verify with `go test`, not `go run`.

### Exercise 1: Core Types, Interfaces, and the Node

Create `node.go`:

```go
package dkv

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// NodeState is the lifecycle state of a cluster node.
type NodeState uint32

const (
	StateStarting      NodeState = iota
	StateBootstrapping NodeState = iota
	StateActive        NodeState = iota
	StateLeaving       NodeState = iota
	StateStopped       NodeState = iota
)

func (s NodeState) String() string {
	switch s {
	case StateStarting:
		return "starting"
	case StateBootstrapping:
		return "bootstrapping"
	case StateActive:
		return "active"
	case StateLeaving:
		return "leaving"
	case StateStopped:
		return "stopped"
	default:
		return fmt.Sprintf("unknown(%d)", uint32(s))
	}
}

// NodeID is the stable unique identifier of a cluster member.
type NodeID string

// VectorClock tracks causality across nodes. Entry [n]=k means node n has
// processed k write events visible to the holder of this clock.
type VectorClock map[NodeID]uint64

// Dominates reports whether c causally dominates other: for every component i,
// c[i] >= other[i] (missing entries are 0), and at least one c[i] > other[i].
// Neither clock dominates the other when writes are concurrent (split-brain).
func (c VectorClock) Dominates(other VectorClock) bool {
	for id, ov := range other {
		if c[id] < ov {
			return false
		}
	}
	for id, cv := range c {
		if cv > other[id] {
			return true
		}
	}
	return false
}

// Value is a versioned datum. Siblings arise when Dominates returns false for
// both directions; the client receives []Value and resolves the conflict.
type Value struct {
	Data      []byte
	Clock     VectorClock
	Timestamp time.Time
}

// Entry is a key-value pair returned by Storage.Scan.
type Entry struct {
	Key   string
	Value Value
}

// Storage accesses partitioned local storage (lesson 1).
type Storage interface {
	Get(ctx context.Context, partition uint32, key string) (Value, error)
	Put(ctx context.Context, partition uint32, key string, val Value) error
	Delete(ctx context.Context, partition uint32, key string) error
	Scan(ctx context.Context, partition uint32, start, end string) ([]Entry, error)
	KeyCount(partition uint32) int64
}

// Replicator routes writes and reads to the correct quorum of replicas (lesson 2).
type Replicator interface {
	Put(ctx context.Context, key string, val Value, w int) error
	Get(ctx context.Context, key string, r int) ([]Value, error)
	Delete(ctx context.Context, key string, w int) error
}

// AntiEntropyStats summarises the background repair state.
type AntiEntropyStats struct {
	SyncedPartitions int64
	RepairedKeys     int64
	LastRunAt        time.Time
}

// AntiEntropy schedules periodic background repair between replicas (lesson 3).
type AntiEntropy interface {
	RepairPartition(ctx context.Context, partitionID uint32) error
	Stats() AntiEntropyStats
}

// HintQueue buffers writes for unavailable nodes and delivers them on recovery (lesson 4).
type HintQueue interface {
	Enqueue(target NodeID, key string, val Value) error
	Depth(target NodeID) int64
	Drain(ctx context.Context, target NodeID) error
}

// Member describes one cluster node from the membership layer's perspective.
type Member struct {
	ID    NodeID
	Addr  string
	State NodeState
	Epoch uint64
}

// MembershipEvent is fired when the cluster topology changes.
type MembershipEvent struct {
	Kind   string // "join", "leave", "suspect", "recover"
	Member Member
}

// Membership tracks live cluster membership and the consistent hash ring (lesson 6).
type Membership interface {
	Join(ctx context.Context, seeds []string) error
	Leave(ctx context.Context) error
	Members() []Member
	Owner(key string) NodeID
	Replicas(key string, n int) []NodeID
	Subscribe(ch chan<- MembershipEvent)
}

// Config holds the validated configuration for one node.
type Config struct {
	NodeID       NodeID
	ClientAddr   string
	RPCAddr      string
	SWIMAddr     string
	DataDir      string
	Seeds        []string
	ReplicationN int
	ReadQuorumR  int
	WriteQuorumW int
	VirtualNodes int
	DrainTimeout time.Duration
	StatusPort   int
}

var (
	ErrAlreadyStarted = errors.New("node already started")
	ErrInvalidConfig  = errors.New("invalid node config")
)

// Validate checks required fields and fills in defaults.
// It is exported so cmd/dkv and cmd/demo can call it before constructing a Node.
func (c *Config) Validate() error {
	if c.NodeID == "" {
		return fmt.Errorf("%w: node_id is required", ErrInvalidConfig)
	}
	if c.ClientAddr == "" {
		return fmt.Errorf("%w: client_addr is required", ErrInvalidConfig)
	}
	if c.ReplicationN < 1 {
		return fmt.Errorf("%w: replication_n must be >= 1", ErrInvalidConfig)
	}
	if c.ReadQuorumR < 1 || c.ReadQuorumR > c.ReplicationN {
		return fmt.Errorf("%w: read_quorum_r must be in [1, N=%d]", ErrInvalidConfig, c.ReplicationN)
	}
	if c.WriteQuorumW < 1 || c.WriteQuorumW > c.ReplicationN {
		return fmt.Errorf("%w: write_quorum_w must be in [1, N=%d]", ErrInvalidConfig, c.ReplicationN)
	}
	if c.DrainTimeout <= 0 {
		c.DrainTimeout = 30 * time.Second
	}
	return nil
}

// Node is the assembled distributed key-value store node. It owns the
// lifecycle of all seven subsystems and the operational HTTP endpoint.
type Node struct {
	cfg Config
	log *slog.Logger

	storage     Storage
	replicator  Replicator
	antiEntropy AntiEntropy
	hints       HintQueue
	membership  Membership

	state      atomic.Uint32 // NodeState
	stopping   atomic.Bool
	activeReqs atomic.Int64

	statusServer *http.Server
	rpcListener  net.Listener

	memberEvents chan MembershipEvent
	stopCh       chan struct{}
	wg           sync.WaitGroup
}

// NewNode constructs a Node with all subsystems injected. None of the
// subsystems are started; call Start to run the bootstrap sequence.
func NewNode(
	cfg Config,
	storage Storage,
	replicator Replicator,
	antiEntropy AntiEntropy,
	hints HintQueue,
	membership Membership,
	log *slog.Logger,
) (*Node, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("NewNode: %w", err)
	}
	if log == nil {
		log = slog.Default()
	}
	return &Node{
		cfg:          cfg,
		log:          log,
		storage:      storage,
		replicator:   replicator,
		antiEntropy:  antiEntropy,
		hints:        hints,
		membership:   membership,
		memberEvents: make(chan MembershipEvent, 64),
		stopCh:       make(chan struct{}),
	}, nil
}

// State returns the current node state.
func (n *Node) State() NodeState {
	return NodeState(n.state.Load())
}

// Start runs the bootstrap sequence and returns once the node is StateActive.
func (n *Node) Start(ctx context.Context) error {
	if n.State() != StateStarting {
		return ErrAlreadyStarted
	}
	n.state.Store(uint32(StateBootstrapping))
	n.log.Info("node starting", "id", n.cfg.NodeID, "addr", n.cfg.ClientAddr)

	// Subscribe before Join so no membership event is missed.
	n.membership.Subscribe(n.memberEvents)

	if err := n.membership.Join(ctx, n.cfg.Seeds); err != nil {
		return fmt.Errorf("membership join: %w", err)
	}

	n.wg.Add(1)
	go n.runMembershipWatcher()

	n.wg.Add(1)
	go n.runAntiEntropyLoop()

	if err := n.startStatusServer(); err != nil {
		return fmt.Errorf("status server: %w", err)
	}

	ln, err := net.Listen("tcp", n.cfg.ClientAddr)
	if err != nil {
		return fmt.Errorf("client listen: %w", err)
	}
	n.rpcListener = ln

	n.wg.Add(1)
	go n.runRPCAcceptor()

	n.state.Store(uint32(StateActive))
	n.log.Info("node active", "id", n.cfg.NodeID)
	return nil
}

// Stop transitions the node to StateLeaving, drains requests, flushes hints,
// and shuts down all listeners. It blocks until the node reaches StateStopped.
func (n *Node) Stop(ctx context.Context) error {
	n.stopping.Store(true)
	n.state.Store(uint32(StateLeaving))
	n.log.Info("node leaving", "id", n.cfg.NodeID)

	if err := n.membership.Leave(ctx); err != nil {
		n.log.Warn("membership leave error", "err", err)
	}

	drainCtx, drainCancel := context.WithTimeout(ctx, n.cfg.DrainTimeout)
	defer drainCancel()
	if err := n.waitDrain(drainCtx); err != nil {
		n.log.Warn("drain timeout, forcing stop", "err", err)
	}

	for _, m := range n.membership.Members() {
		if m.ID == n.cfg.NodeID {
			continue
		}
		flushCtx, flushCancel := context.WithTimeout(ctx, 10*time.Second)
		if err := n.hints.Drain(flushCtx, m.ID); err != nil {
			n.log.Warn("hint flush error", "target", m.ID, "err", err)
		}
		flushCancel()
	}

	close(n.stopCh)

	if n.rpcListener != nil {
		_ = n.rpcListener.Close()
	}

	if n.statusServer != nil {
		shutCtx, shutCancel := context.WithTimeout(ctx, 5*time.Second)
		_ = n.statusServer.Shutdown(shutCtx)
		shutCancel()
	}

	n.wg.Wait()
	n.state.Store(uint32(StateStopped))
	n.log.Info("node stopped", "id", n.cfg.NodeID)
	return nil
}

func (n *Node) waitDrain(ctx context.Context) error {
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	for {
		if n.activeReqs.Load() == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

func (n *Node) runMembershipWatcher() {
	defer n.wg.Done()
	for {
		select {
		case <-n.stopCh:
			return
		case ev := <-n.memberEvents:
			n.handleMemberEvent(ev)
		}
	}
}

func (n *Node) handleMemberEvent(ev MembershipEvent) {
	n.log.Info("membership event", "kind", ev.Kind, "member", ev.Member.ID)
	if ev.Kind != "recover" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := n.hints.Drain(ctx, ev.Member.ID); err != nil {
		n.log.Warn("hint drain on recover failed", "target", ev.Member.ID, "err", err)
	}
}

func (n *Node) runAntiEntropyLoop() {
	defer n.wg.Done()
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-n.stopCh:
			return
		case <-ticker.C:
			for _, m := range n.membership.Members() {
				if m.ID != n.cfg.NodeID {
					continue
				}
				// In a full implementation, iterate owned partition IDs from the ring.
				// The call pattern is shown here; partition IDs come from Membership.
				repairCtx, repairCancel := context.WithTimeout(context.Background(), 30*time.Second)
				if err := n.antiEntropy.RepairPartition(repairCtx, 0); err != nil {
					n.log.Warn("anti-entropy repair failed", "err", err)
				}
				repairCancel()
			}
		}
	}
}

func (n *Node) runRPCAcceptor() {
	defer n.wg.Done()
	for {
		conn, err := n.rpcListener.Accept()
		if err != nil {
			if n.stopping.Load() {
				return // normal shutdown via Stop()
			}
			n.log.Warn("rpc accept error", "err", err)
			return
		}
		n.activeReqs.Add(1)
		n.wg.Add(1)
		go func() {
			defer n.wg.Done()
			defer n.activeReqs.Add(-1)
			n.serveConn(conn)
		}()
	}
}

// serveConn decodes framed RPC messages from conn, dispatches to
// n.replicator / n.storage, and writes responses. The framing and
// multiplexing protocol are defined in lesson 7 (client protocol).
func (n *Node) serveConn(conn net.Conn) {
	defer conn.Close()
}
```

The lesson's new type is `VectorClock` and the lesson's new behavior is the `Node` lifecycle. Everything else is wire-up.

### Exercise 2: HTTP Metrics Endpoint

Create `status.go`:

```go
package dkv

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"
)

// StatusResponse is returned by GET /status as JSON.
type StatusResponse struct {
	NodeID    string           `json:"node_id"`
	Addr      string           `json:"addr"`
	State     string           `json:"state"`
	Members   []MemberStatus   `json:"members"`
	Metrics   NodeMetrics      `json:"metrics"`
	HintQueue map[string]int64 `json:"hint_queue"`
	AEStats   AntiEntropyStats `json:"anti_entropy"`
}

// MemberStatus is the per-member view in the status response.
type MemberStatus struct {
	ID    string `json:"id"`
	Addr  string `json:"addr"`
	State string `json:"state"`
	Epoch uint64 `json:"epoch"`
}

// NodeMetrics holds per-operation counters and latency histograms.
type NodeMetrics struct {
	Gets       int64     `json:"gets"`
	Puts       int64     `json:"puts"`
	Deletes    int64     `json:"deletes"`
	GetLatency Histogram `json:"get_latency_ms"`
	PutLatency Histogram `json:"put_latency_ms"`
}

// Histogram holds percentile latencies in milliseconds.
type Histogram struct {
	P50 float64 `json:"p50"`
	P95 float64 `json:"p95"`
	P99 float64 `json:"p99"`
}

func (n *Node) startStatusServer() error {
	port := n.cfg.StatusPort
	// port == 0 means the OS picks an ephemeral port, which avoids conflicts
	// in tests and is appropriate when StatusPort is not explicitly configured.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", n.handleStatus)
	mux.HandleFunc("GET /health", n.handleHealth)

	n.statusServer = &http.Server{
		Addr:         fmt.Sprintf(":%d", port),
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
	}

	ln, err := net.Listen("tcp", n.statusServer.Addr)
	if err != nil {
		return err
	}

	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		_ = n.statusServer.Serve(ln)
	}()
	return nil
}

func (n *Node) handleStatus(w http.ResponseWriter, _ *http.Request) {
	members := n.membership.Members()
	ms := make([]MemberStatus, len(members))
	for i, m := range members {
		ms[i] = MemberStatus{
			ID:    string(m.ID),
			Addr:  m.Addr,
			State: m.State.String(),
			Epoch: m.Epoch,
		}
	}

	hintQueue := make(map[string]int64, len(members))
	for _, m := range members {
		if m.ID == n.cfg.NodeID {
			continue
		}
		hintQueue[string(m.ID)] = n.hints.Depth(m.ID)
	}

	resp := StatusResponse{
		NodeID:    string(n.cfg.NodeID),
		Addr:      n.cfg.ClientAddr,
		State:     n.State().String(),
		Members:   ms,
		HintQueue: hintQueue,
		AEStats:   n.antiEntropy.Stats(),
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (n *Node) handleHealth(w http.ResponseWriter, _ *http.Request) {
	if n.State() != StateActive {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}
```

### Exercise 3: Tests and Fake Subsystems

The test file contains inline fake implementations so every interface is satisfied without importing lesson 1-7 packages. This is the pattern for testing the integration layer in isolation.

Create `node_test.go`:

```go
package dkv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// --- fake subsystems ---

type fakeStorage struct{}

func (fakeStorage) Get(_ context.Context, _ uint32, _ string) (Value, error) {
	return Value{}, nil
}
func (fakeStorage) Put(_ context.Context, _ uint32, _ string, _ Value) error { return nil }
func (fakeStorage) Delete(_ context.Context, _ uint32, _ string) error       { return nil }
func (fakeStorage) Scan(_ context.Context, _ uint32, _, _ string) ([]Entry, error) {
	return nil, nil
}
func (fakeStorage) KeyCount(_ uint32) int64 { return 0 }

type fakeReplicator struct{}

func (fakeReplicator) Put(_ context.Context, _ string, _ Value, _ int) error { return nil }
func (fakeReplicator) Get(_ context.Context, _ string, _ int) ([]Value, error) {
	return nil, nil
}
func (fakeReplicator) Delete(_ context.Context, _ string, _ int) error { return nil }

type fakeAE struct{}

func (fakeAE) RepairPartition(_ context.Context, _ uint32) error { return nil }
func (fakeAE) Stats() AntiEntropyStats                           { return AntiEntropyStats{} }

type fakeHintQueue struct{}

func (fakeHintQueue) Enqueue(_ NodeID, _ string, _ Value) error { return nil }
func (fakeHintQueue) Depth(_ NodeID) int64                      { return 0 }
func (fakeHintQueue) Drain(_ context.Context, _ NodeID) error   { return nil }

type fakeMembership struct {
	members []Member
}

func (m *fakeMembership) Join(_ context.Context, _ []string) error { return nil }
func (m *fakeMembership) Leave(_ context.Context) error            { return nil }
func (m *fakeMembership) Members() []Member                        { return m.members }
func (m *fakeMembership) Owner(_ string) NodeID                    { return "node-1" }
func (m *fakeMembership) Replicas(_ string, _ int) []NodeID        { return []NodeID{"node-1"} }
func (m *fakeMembership) Subscribe(_ chan<- MembershipEvent)       {}

// --- helpers ---

func minimalConfig(t *testing.T) Config {
	t.Helper()
	return Config{
		NodeID:       "test-node",
		ClientAddr:   "127.0.0.1:0",
		ReplicationN: 3,
		ReadQuorumR:  2,
		WriteQuorumW: 2,
		DrainTimeout: 5 * time.Second,
	}
}

func newTestNode(t *testing.T, cfg Config) *Node {
	t.Helper()
	node, err := NewNode(cfg, fakeStorage{}, fakeReplicator{}, fakeAE{}, fakeHintQueue{}, &fakeMembership{}, nil)
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	return node
}

// --- VectorClock tests ---

func TestVectorClockDominates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		c     VectorClock
		other VectorClock
		want  bool
	}{
		{
			name:  "strictly after on one component",
			c:     VectorClock{"A": 3, "B": 2},
			other: VectorClock{"A": 2, "B": 2},
			want:  true,
		},
		{
			name:  "other is strictly after c",
			c:     VectorClock{"A": 2, "B": 2},
			other: VectorClock{"A": 3, "B": 2},
			want:  false,
		},
		{
			name:  "concurrent writes (split-brain)",
			c:     VectorClock{"A": 3, "B": 1},
			other: VectorClock{"A": 1, "B": 3},
			want:  false,
		},
		{
			name:  "equal clocks",
			c:     VectorClock{"A": 2},
			other: VectorClock{"A": 2},
			want:  false,
		},
		{
			name:  "c has additional node not in other",
			c:     VectorClock{"A": 1, "B": 1},
			other: VectorClock{"A": 1},
			want:  true,
		},
		{
			name:  "other has additional node not in c",
			c:     VectorClock{"A": 1},
			other: VectorClock{"A": 1, "B": 1},
			want:  false,
		},
		{
			name:  "c dominates empty clock",
			c:     VectorClock{"A": 1},
			other: VectorClock{},
			want:  true,
		},
		{
			name:  "empty does not dominate empty",
			c:     VectorClock{},
			other: VectorClock{},
			want:  false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tc.c.Dominates(tc.other)
			if got != tc.want {
				t.Errorf("Dominates = %v, want %v", got, tc.want)
			}
		})
	}
}

// --- Config tests ---

func TestConfigValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cfg     Config
		wantErr error
	}{
		{
			name:    "missing node id",
			cfg:     Config{ClientAddr: ":7000", ReplicationN: 3, ReadQuorumR: 2, WriteQuorumW: 2},
			wantErr: ErrInvalidConfig,
		},
		{
			name:    "missing client addr",
			cfg:     Config{NodeID: "n1", ReplicationN: 3, ReadQuorumR: 2, WriteQuorumW: 2},
			wantErr: ErrInvalidConfig,
		},
		{
			name:    "read quorum exceeds N",
			cfg:     Config{NodeID: "n1", ClientAddr: ":7000", ReplicationN: 3, ReadQuorumR: 4, WriteQuorumW: 2},
			wantErr: ErrInvalidConfig,
		},
		{
			name:    "write quorum exceeds N",
			cfg:     Config{NodeID: "n1", ClientAddr: ":7000", ReplicationN: 3, ReadQuorumR: 2, WriteQuorumW: 4},
			wantErr: ErrInvalidConfig,
		},
		{
			name:    "valid minimal config",
			cfg:     Config{NodeID: "n1", ClientAddr: ":7000", ReplicationN: 1, ReadQuorumR: 1, WriteQuorumW: 1},
			wantErr: nil,
		},
		{
			name:    "drain timeout default applied",
			cfg:     Config{NodeID: "n1", ClientAddr: ":7000", ReplicationN: 1, ReadQuorumR: 1, WriteQuorumW: 1},
			wantErr: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.cfg.Validate()
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("Validate() err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestConfigValidateAppliesDrainDefault(t *testing.T) {
	t.Parallel()

	cfg := Config{NodeID: "n1", ClientAddr: ":7000", ReplicationN: 1, ReadQuorumR: 1, WriteQuorumW: 1}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.DrainTimeout != 30*time.Second {
		t.Errorf("DrainTimeout = %v, want 30s", cfg.DrainTimeout)
	}
}

// --- Node lifecycle tests ---

func TestNodeLifecycle(t *testing.T) {
	t.Parallel()

	node := newTestNode(t, minimalConfig(t))

	if node.State() != StateStarting {
		t.Fatalf("initial state = %v, want starting", node.State())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := node.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if node.State() != StateActive {
		t.Fatalf("state after Start = %v, want active", node.State())
	}

	if err := node.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if node.State() != StateStopped {
		t.Fatalf("state after Stop = %v, want stopped", node.State())
	}
}

func TestNodeStartIdempotent(t *testing.T) {
	t.Parallel()

	node := newTestNode(t, minimalConfig(t))
	ctx := context.Background()

	if err := node.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer node.Stop(context.Background())

	if err := node.Start(ctx); !errors.Is(err, ErrAlreadyStarted) {
		t.Errorf("second Start err = %v, want ErrAlreadyStarted", err)
	}
}

// --- HTTP status tests ---

func TestStatusEndpoint(t *testing.T) {
	t.Parallel()

	cfg := minimalConfig(t)
	cfg.NodeID = "status-node"
	cfg.StatusPort = 0 // OS picks a port; we test via httptest directly

	node := newTestNode(t, cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := node.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer node.Stop(context.Background())

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	rec := httptest.NewRecorder()
	node.handleStatus(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200", rec.Code)
	}

	var resp StatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.NodeID != "status-node" {
		t.Errorf("node_id = %q, want %q", resp.NodeID, "status-node")
	}
	if resp.State != "active" {
		t.Errorf("state = %q, want %q", resp.State, "active")
	}
}

func TestHealthEndpointActive(t *testing.T) {
	t.Parallel()

	node := newTestNode(t, minimalConfig(t))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := node.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer node.Stop(context.Background())

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	node.handleHealth(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("health code = %d, want 200 when active", rec.Code)
	}
}

func TestHealthEndpointNotYetStarted(t *testing.T) {
	t.Parallel()

	node := newTestNode(t, minimalConfig(t))
	// Do not call Start — node is in StateStarting, not StateActive.
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	node.handleHealth(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("health code = %d, want 503 when not active", rec.Code)
	}
}

// Your turn: add TestMembershipRecoveryDrainsHints that:
// 1. Creates a fakeMembership with one member other than the node itself.
// 2. Starts the node.
// 3. Sends a MembershipEvent{Kind: "recover", Member: <that member>} on the
//    channel captured by Subscribe.
// 4. Asserts that fakeHintQueue.Drain was called for that member's NodeID.
// Hint: replace fakeHintQueue with a recording fake that counts Drain calls.

// --- Examples (auto-verified by go test) ---

func ExampleVectorClock_Dominates() {
	after := VectorClock{"node-A": 3, "node-B": 2}
	before := VectorClock{"node-A": 2, "node-B": 2}
	concurrent1 := VectorClock{"node-A": 3, "node-B": 1}
	concurrent2 := VectorClock{"node-A": 1, "node-B": 3}

	fmt.Println(after.Dominates(before))
	fmt.Println(before.Dominates(after))
	fmt.Println(concurrent1.Dominates(concurrent2))
	fmt.Println(concurrent2.Dominates(concurrent1))
	// Output:
	// true
	// false
	// false
	// false
}

func ExampleNodeState_String() {
	fmt.Println(StateStarting)
	fmt.Println(StateActive)
	fmt.Println(StateLeaving)
	fmt.Println(StateStopped)
	// Output:
	// starting
	// active
	// leaving
	// stopped
}
```

The Example functions are auto-verified by `go test` via the `// Output:` comment; they fail if the output changes.

### Exercise 4: Demo Binary

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"

	"example.com/dkv"
)

func main() {
	cfg := dkv.Config{
		NodeID:       dkv.NodeID("demo-node"),
		ClientAddr:   "127.0.0.1:7000",
		ReplicationN: 3,
		ReadQuorumR:  2,
		WriteQuorumW: 2,
		VirtualNodes: 150,
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(1)
	}
	fmt.Println("node id:", cfg.NodeID)

	// Demonstrate vector clock causality detection.
	after := dkv.VectorClock{"node-A": 3, "node-B": 2}
	before := dkv.VectorClock{"node-A": 2, "node-B": 2}
	concurrent1 := dkv.VectorClock{"node-A": 3, "node-B": 1}
	concurrent2 := dkv.VectorClock{"node-A": 1, "node-B": 3}

	fmt.Printf("after dominates before:         %v\n", after.Dominates(before))
	fmt.Printf("before dominates after:         %v\n", before.Dominates(after))
	fmt.Printf("concurrent1 dominates concurrent2: %v (sibling — neither wins)\n", concurrent1.Dominates(concurrent2))
	fmt.Printf("concurrent2 dominates concurrent1: %v (sibling — neither wins)\n", concurrent2.Dominates(concurrent1))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
node id: demo-node
after dominates before:         true
before dominates after:         false
concurrent1 dominates concurrent2: false (sibling — neither wins)
concurrent2 dominates concurrent1: false (sibling — neither wins)
```

### Exercise 5: Production Binaries (Illustrative)

The production binaries wire the lesson 1-7 packages into the Node. They are shown here as illustrative code because they depend on packages defined in those lessons.

**cmd/dkv/main.go** — the node binary:

```go
// Production binary: replace nil subsystems with real lesson 1-7 packages.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"example.com/dkv"
)

func main() {
	var (
		nodeID     = flag.String("id", "", "unique node ID (required)")
		clientAddr = flag.String("client-addr", ":7000", "client connection address")
		rpcAddr    = flag.String("rpc-addr", ":7001", "inter-node RPC address")
		swimAddr   = flag.String("swim-addr", ":7002", "SWIM membership address")
		dataDir    = flag.String("data-dir", "data", "persistent data directory")
		seeds      = flag.String("seeds", "", "comma-separated seed addresses")
		n          = flag.Int("n", 3, "replication factor")
		r          = flag.Int("r", 2, "read quorum")
		w          = flag.Int("w", 2, "write quorum")
		vnodes     = flag.Int("vnodes", 150, "virtual node count")
		statusPort = flag.Int("status-port", 0, "HTTP status port (default: clientPort + 1000)")
	)
	flag.Parse()

	if *nodeID == "" {
		fmt.Fprintln(os.Stderr, "error: -id is required")
		flag.Usage()
		os.Exit(1)
	}

	var seedList []string
	if *seeds != "" {
		seedList = strings.Split(*seeds, ",")
	}

	cfg := dkv.Config{
		NodeID:       dkv.NodeID(*nodeID),
		ClientAddr:   *clientAddr,
		RPCAddr:      *rpcAddr,
		SWIMAddr:     *swimAddr,
		DataDir:      *dataDir,
		Seeds:        seedList,
		ReplicationN: *n,
		ReadQuorumR:  *r,
		WriteQuorumW: *w,
		VirtualNodes: *vnodes,
		DrainTimeout: 30 * time.Second,
		StatusPort:   *statusPort,
	}

	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

	// Replace these nils with real subsystem constructors from lessons 1-7:
	//   storage    = partitioned.New(cfg.DataDir, cfg.VirtualNodes)
	//   membership = swim.New(cfg.SWIMAddr, cfg.NodeID)
	//   replicator = replication.New(storage, membership, cfg.ReplicationN, cfg.ReadQuorumR, cfg.WriteQuorumW)
	//   ae         = antientropy.New(storage, replicator)
	//   hints      = hinted.New(cfg.DataDir + "/hints")
	node, err := dkv.NewNode(cfg, nil, nil, nil, nil, nil, log)
	if err != nil {
		log.Error("node init failed", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := node.Start(ctx); err != nil {
		log.Error("node start failed", "err", err)
		os.Exit(1)
	}

	<-ctx.Done()
	log.Info("shutdown signal received")

	if err := node.Stop(context.Background()); err != nil {
		log.Error("node stop error", "err", err)
		os.Exit(1)
	}
}
```

**cmd/dkv-admin/main.go** — the admin CLI:

```go
// Admin CLI: cluster status, drain, repair.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"
)

func main() {
	addr := flag.String("addr", "localhost:9080", "node status address")
	flag.Parse()

	if flag.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: dkv-admin [-addr host:port] <command>")
		fmt.Fprintln(os.Stderr, "commands: status")
		os.Exit(1)
	}

	switch flag.Arg(0) {
	case "status":
		runStatus(*addr)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", flag.Arg(0))
		os.Exit(1)
	}
}

func runStatus(addr string) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + addr + "/status")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		fmt.Fprintln(os.Stderr, "decode error:", err)
		os.Exit(1)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(body)
}
```

### Exercise 6: Integration Test Scenarios

The full integration test suite starts multiple nodes in-process using real TCP listeners on ephemeral ports. Each scenario validates a distinct failure mode. The outline below shows the structure; fill in the bodies using the lesson 1-7 fake implementations.

Key scenarios to implement in `integration_test.go` (build tag `//go:build integration`):

- **Bootstrap**: 3-node cluster from a single seed; verify all nodes reach `StateActive` and agree on membership.
- **Quorum read after crash**: write 1 000 keys with W=2; kill one node; verify reads succeed at R=2 on the remaining two.
- **Recovery via hinted handoff**: restart the killed node; verify it receives hints for writes missed while stopped.
- **Anti-entropy catch-up**: force-corrupt one replica's key; trigger anti-entropy; verify the replica converges.
- **Split-brain detection**: write to the same key on two isolated subsets; heal the partition; verify both values are surfaced as siblings (`[]Value` with length 2).
- **Rolling restart**: drain and restart each node in turn; verify zero key loss after all nodes are back.

Isolating nodes for the split-brain test uses a mock transport layer (replace `net.Listen` with an in-process pipe that can be paused), not `iptables`, so tests run without root on any OS.

## Common Mistakes

### Closing the Stop Channel Before Setting the Stopping Flag

Wrong: closing `n.stopCh` first and then setting `n.stopping = true`. The `runRPCAcceptor` goroutine reads `n.stopping.Load()` after `Accept()` returns an error. If the listener is closed before the flag is set, the goroutine sees `false` and logs a spurious "rpc accept error".

Fix: call `n.stopping.Store(true)` before `close(n.stopCh)` and before `n.rpcListener.Close()`. In the lesson's Stop(), this order is enforced. Always set the sentinel before triggering the event it guards.

### Ignoring the Context in Long-Running Background Goroutines

Wrong: a goroutine that calls `time.Sleep(60 * time.Second)` inside the anti-entropy loop. Stop() closes `stopCh` but the goroutine sleeps for a full minute before checking it, making shutdown stall.

Fix: use `time.NewTicker` with a `select { case <-n.stopCh: return; case <-ticker.C: }` pattern. The goroutine wakes on whichever channel fires first, giving sub-millisecond shutdown latency.

### Treating Interface Values as Non-Nil When the Concrete Pointer Is Nil

Wrong: passing a nil `*swimMembership` pointer as the `Membership` interface argument. The interface value is non-nil (it has a type) but calling any method panics.

Fix: check for nil before passing subsystems. In production, never construct a Node until all subsystems are confirmed non-nil. In tests, use the explicit fake implementations rather than typed nils.

### Surfacing Siblings as an Error Instead of as Data

Wrong: returning `ErrConflict` when two replicas disagree on a value. The client cannot know whether the conflict is fatal or resolvable.

Fix: return `([]Value, error)` where the slice holds siblings when `len > 1`. The error field is reserved for network and storage failures, not for data conflicts. The client inspects the slice length to detect a sibling and applies its own resolution strategy.

### Leaking Goroutines When Start Fails Mid-Way

Wrong: Start() launches two goroutines, then fails on the third step. The first two goroutines are now leaked — they hold references to the Node and block forever on `stopCh`.

Fix: if Start() fails after launching goroutines, close `stopCh` in the error path and call `n.wg.Wait()` before returning the error. Alternatively, use a context with cancel that is passed to each goroutine; cancel it on error and wait for them to exit.

## Verification

From `~/go-exercises/dkv`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

All five must pass. The integration test suite (Exercise 6) is gated behind `//go:build integration` and requires real subsystem packages from lessons 1-7:

```bash
go test -count=1 -race -tags integration -timeout 120s ./...
```

## Summary

- The `Node` owns the lifecycle: `StateStarting` → `StateBootstrapping` → `StateActive` → `StateLeaving` → `StateStopped`. Every subsystem is injected via a small consumer-defined interface.
- `VectorClock.Dominates` is the core of split-brain detection: when neither clock dominates the other, both values are siblings and the client resolves the conflict.
- Bootstrap has four phases: announce, ring convergence, partition hand-off, ready. The hand-off must be idempotent — partial transfers must be replayable.
- Graceful shutdown is the reverse of startup: broadcast leaving, drain requests, flush hints, close listeners, wait for goroutines.
- The HTTP `/status` endpoint and `/health` endpoint are the operational surface: monitoring systems and rolling-restart scripts read from them to decide when the node is safe to route to or decommission.
- The integration test suite exercises failure scenarios that unit tests cannot: quorum reads after a node crash, hint delivery on recovery, and sibling detection after a network partition.

## What's Next

Next: [Lock-Free MPMC Queue](../../46-capstone-concurrency-deep-dive/01-lock-free-mpmc-queue/01-lock-free-mpmc-queue.md).

## Resources

- "Dynamo: Amazon's Highly Available Key-Value Store" (DeCandia et al., SOSP 2007) — the original design that this series approximates: https://www.allthingsdistributed.com/files/amazon-dynamo-sosp2007.pdf
- Go `sync/atomic` package — `atomic.Uint32`, `atomic.Bool`, `atomic.Int64`: https://pkg.go.dev/sync/atomic
- Go `net/http` package — `Server.Shutdown`, `ServeMux` with method+path patterns (Go 1.22+): https://pkg.go.dev/net/http
- Go `log/slog` package — structured logging, added Go 1.21: https://pkg.go.dev/log/slog
- "Designing Data-Intensive Applications" (Kleppmann, 2017) chapters 5 and 9 — replication, consistency, and vector clocks in production systems
