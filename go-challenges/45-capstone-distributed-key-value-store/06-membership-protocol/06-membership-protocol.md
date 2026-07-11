# 6. Membership Protocol

The SWIM protocol (Scalable Weakly-consistent Infection-style Membership) solves the cluster membership problem: every node must know which peers are alive, which have failed, and which have left — without a central coordinator and without flooding the network. Three interlocking problems make the implementation hard. First, distinguishing a slow peer from a dead one without triggering false positives. Second, propagating membership changes to all N nodes in O(log N) protocol rounds using only the bandwidth that failure detection already consumes. Third, handling the race where a falsely suspected node must refute the accusation before other nodes declare it dead and reassign its partitions.

This lesson builds the full SWIM membership layer. The implementation is UDP-based, uses a compact binary wire format, and integrates with the hash-ring reassignment logic from the previous lessons.

```text
membership/
  go.mod
  membership.go      (types, Config, Membership, state machine)
  gossip.go          (GossipQueue — priority dissemination buffer)
  message.go         (MessageType, Message, wire encode/decode)
  probe.go           (failure detection loop, send/receive, indirect probing)
  membership_test.go (table-driven tests + Example functions)
  cmd/
    demo/
      main.go        (three-node local cluster demonstration)
```

## Concepts

### The SWIM Failure Detection Model

Every T milliseconds (default 200 ms) each node picks one random peer and sends it a `ping` UDP datagram. If an `ack` arrives within a direct timeout D (default 500 ms), the peer is healthy. If not, rather than declaring the peer dead, the probing node sends a `ping-req` to k other random members (default k=3), asking each of them to probe the suspect on its behalf. This indirect sub-round absorbs transient network asymmetries: a packet lost in one direction is unlikely to be lost across all k alternate paths.

Only if neither the direct ping nor any of the k indirect probes produces an ack within a second window (default 1 s total) is the peer flagged as `suspect`. The two-phase design achieves low false-positive rates without long timeouts: it uses spatial redundancy rather than temporal patience, and it does so without any extra protocol traffic because the indirect probes piggyback on the same UDP flow.

### Suspicion and Incarnation Numbers

Once a peer is `suspect`, a timer begins (default 5 s). If the peer has not refuted the suspicion before the timer fires, it transitions to `dead` and its partitions are reassigned. Refutation relies on incarnation numbers: each node owns a counter that only it can increment. When node A learns that it has been marked `suspect`, it increments its incarnation and broadcasts an `alive` message carrying the new higher number. Any node receiving `alive` with a higher incarnation than its local record immediately overrides the suspicion. Because incarnation numbers are monotone, a refutation is never undone by a stale `suspect` arriving later.

State transition rules:
- `alive(inc=n)` is superseded by any update with incarnation > n, or by `suspect(inc=n)` from a third party.
- `suspect(inc=n)` is overridden by `alive(inc=n+1)` (refutation) or `dead(inc≥n)`.
- `dead` and `left` are terminal for any given incarnation. The only re-entry to `alive` is a restart with a higher incarnation.
- At equal incarnation, terminal states (`dead`, `left`) override transient states (`alive`, `suspect`).

### Infection-Style Gossip Dissemination

Failure detection messages already traverse the cluster every T milliseconds. Rather than send a separate broadcast when membership changes, SWIM piggybacks membership updates on these existing messages at zero extra round-trip cost. Each piggybacked entry tracks a send count. Once an entry has been piggybacked ceil(log2(N+1)) times it is pruned: at that point it has reached all N nodes with high probability. Newer events take priority so a critical `dead` announcement is not starved by routine `alive` heartbeats. The Lamport timestamp in each entry encodes creation order locally; receivers use the incarnation number (which crosses the wire) — not the timestamp — to determine whether an incoming update supersedes their local state.

### The Wire Format Constraint

SWIM uses UDP because datagrams fit the one-way probe model and add no per-message overhead. The practical constraint is that each message must fit in a single Ethernet frame to avoid IP fragmentation, which would break the atomic delivery UDP is valued for. The limit is 1 400 bytes (1 500-byte Ethernet MTU minus 60 bytes of IP header headroom minus 8 bytes of UDP header).

Wire layout (big-endian):

```text
byte  0:      MessageType  (uint8)
bytes 1–8:    SeqNo        (uint64)
byte  9:      target addr length  (uint8; 0 = no target; non-zero only in MsgPingReq)
bytes 10–…:   target addr bytes   (if length > 0)
byte  N:      gossip entry count  (uint8; max 6)
for each gossip entry:
  byte:        addr length  (uint8)
  bytes:       addr bytes
  byte:        NodeState    (uint8)
  bytes 0–7:   incarnation  (uint64, big-endian)
```

With 21-byte addresses ("192.168.255.255:65535") and six gossip entries plus a target, the encoded size is 1+8+1+21+1+6×(1+21+1+8) = 218 bytes — well under 1 400.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/membership/cmd/demo
cd ~/go-exercises/membership
go mod init example.com/membership
```

### Exercise 1: Core Types and the Gossip Queue

Create `membership.go`. This file defines every type the rest of the package builds on: the member state machine, the sentinel errors, the cluster configuration, and the central `Membership` struct. Networking methods (Start, Stop, Join, probe loop) are added in Exercise 3.

```go
package membership

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// NodeState is the liveness state of a cluster member in the SWIM state machine.
type NodeState uint8

const (
	StateAlive   NodeState = iota // responding normally to pings
	StateSuspect                  // not responding; awaiting indirect confirmation
	StateDead                     // confirmed failed; partitions will be reassigned
	StateLeft                     // gracefully departed
)

// String implements fmt.Stringer.
func (s NodeState) String() string {
	switch s {
	case StateAlive:
		return "alive"
	case StateSuspect:
		return "suspect"
	case StateDead:
		return "dead"
	case StateLeft:
		return "left"
	default:
		return fmt.Sprintf("NodeState(%d)", uint8(s))
	}
}

var (
	// ErrAlreadyStarted is returned by Start when called on a running Membership.
	ErrAlreadyStarted = errors.New("membership: already started")
	// ErrNotStarted is returned by Stop or Join before Start has been called.
	ErrNotStarted = errors.New("membership: not started")
	// ErrMessageTooLarge is returned when a message would exceed maxUDPPayload.
	ErrMessageTooLarge = errors.New("membership: message exceeds MTU limit")
	// ErrBadConfig is returned by DefaultConfig and New for invalid parameters.
	ErrBadConfig = errors.New("membership: invalid configuration")
)

// maxUDPPayload is the largest UDP payload that fits in one Ethernet frame
// without IP fragmentation (1 500-byte MTU − 60-byte IP headroom − 8-byte UDP).
const maxUDPPayload = 1400

// maxGossipEntries is the maximum number of membership updates piggybacked per
// protocol message. Six entries fit comfortably within maxUDPPayload.
const maxGossipEntries = 6

// Member holds the protocol state for one cluster node.
type Member struct {
	Addr        string // "host:port" UDP address, e.g. "10.0.0.1:7946"
	State       NodeState
	Incarnation uint64
}

// EventType classifies a membership change notification.
type EventType uint8

const (
	EventJoined  EventType = iota // member reached alive state for the first time
	EventLeft                     // member performed graceful leave
	EventFailed                   // member confirmed dead
	EventSuspect                  // member newly suspected; partitions not yet reassigned
)

// Event is published to subscribers when membership changes.
type Event struct {
	Member Member
	Type   EventType
}

// Config holds the tunable parameters for the SWIM protocol.
type Config struct {
	BindAddr         string
	ProbeInterval    time.Duration // T: period between probe rounds (default 200 ms)
	ProbeTimeout     time.Duration // direct-ping acknowledgement window (default 500 ms)
	IndirectProbes   int           // k: indirect probers per failed direct ping (default 3)
	SuspicionTimeout time.Duration // suspect-to-dead transition delay (default 5 s)
	MaxGossipEntries int           // piggybacked updates per message (default 6)
}

// DefaultConfig returns a Config initialised with the parameters from the SWIM
// paper: T=200 ms, direct timeout=500 ms, k=3, suspicion=5 s.
func DefaultConfig(bindAddr string) (Config, error) {
	if bindAddr == "" {
		return Config{}, fmt.Errorf("%w: bind address is required", ErrBadConfig)
	}
	return Config{
		BindAddr:         bindAddr,
		ProbeInterval:    200 * time.Millisecond,
		ProbeTimeout:     500 * time.Millisecond,
		IndirectProbes:   3,
		SuspicionTimeout: 5 * time.Second,
		MaxGossipEntries: maxGossipEntries,
	}, nil
}

// Membership implements the SWIM failure detection and membership dissemination
// protocol. Create one with New; call Start to begin probing.
type Membership struct {
	mu      sync.RWMutex
	config  Config
	self    Member
	members map[string]*Member // addr → state, guarded by mu

	queue   *GossipQueue
	conn    *net.UDPConn
	seqNo   atomic.Uint64
	pending *pendingProbes
	done    chan struct{}
	started bool

	events chan Event
	logger *slog.Logger
}

// New creates a Membership instance for the given Config.
// Call Start after New to bind the UDP port and begin probing.
func New(config Config) (*Membership, error) {
	if config.BindAddr == "" {
		return nil, fmt.Errorf("%w: bind address is required", ErrBadConfig)
	}
	if config.ProbeInterval <= 0 {
		return nil, fmt.Errorf("%w: probe interval must be positive", ErrBadConfig)
	}
	if config.IndirectProbes < 1 {
		return nil, fmt.Errorf("%w: indirect probes must be at least 1", ErrBadConfig)
	}
	if config.MaxGossipEntries < 1 {
		config.MaxGossipEntries = maxGossipEntries
	}
	return &Membership{
		config:  config,
		self:    Member{Addr: config.BindAddr, State: StateAlive, Incarnation: 1},
		members: make(map[string]*Member),
		queue:   NewGossipQueue(),
		pending: newPendingProbes(),
		done:    make(chan struct{}),
		events:  make(chan Event, 64),
		logger:  slog.Default(),
	}, nil
}

// Members returns a snapshot of the live membership list.
// Dead and left nodes are excluded. The local node is always included.
func (m *Membership) Members() []Member {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Member, 0, len(m.members)+1)
	out = append(out, m.self)
	for _, mem := range m.members {
		if mem.State != StateDead && mem.State != StateLeft {
			out = append(out, *mem)
		}
	}
	return out
}

// Subscribe returns a channel that receives Event values as membership changes.
// The channel is buffered for 64 events; drain it promptly to avoid drops.
func (m *Membership) Subscribe() <-chan Event {
	return m.events
}

// tryUpdateMember applies an incoming Member state if it supersedes the local
// record according to SWIM incarnation-number rules. Returns true if the local
// state changed.
//
// Rules:
//   - Higher incarnation always wins, regardless of state direction.
//   - At equal incarnation, terminal states (dead, left) override alive/suspect.
//   - If we receive a suspect for ourselves, we refute by incrementing incarnation
//     and queuing an alive announcement.
func (m *Membership) tryUpdateMember(incoming Member) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Self-refutation: if we are suspected, counter with a higher incarnation.
	if incoming.Addr == m.self.Addr {
		if incoming.State == StateSuspect && incoming.Incarnation >= m.self.Incarnation {
			m.self.Incarnation++
			refutation := Member{
				Addr:        m.self.Addr,
				State:       StateAlive,
				Incarnation: m.self.Incarnation,
			}
			m.queue.Add(refutation)
		}
		return false
	}

	existing, ok := m.members[incoming.Addr]
	if !ok {
		cp := incoming
		m.members[incoming.Addr] = &cp
		m.queue.Add(incoming)
		m.publishEventLocked(incoming)
		return true
	}

	// Higher incarnation supersedes unconditionally.
	if incoming.Incarnation > existing.Incarnation {
		*existing = incoming
		m.queue.Add(incoming)
		m.publishEventLocked(incoming)
		return true
	}
	// Equal incarnation: terminal state overrides transient.
	if incoming.Incarnation == existing.Incarnation {
		terminal := incoming.State == StateDead || incoming.State == StateLeft
		transient := existing.State == StateAlive || existing.State == StateSuspect
		if terminal && transient {
			*existing = incoming
			m.queue.Add(incoming)
			m.publishEventLocked(incoming)
			return true
		}
	}
	return false
}

// publishEventLocked emits a membership change event.
// Must be called with m.mu write-locked.
func (m *Membership) publishEventLocked(member Member) {
	var etype EventType
	switch member.State {
	case StateAlive:
		etype = EventJoined
	case StateLeft:
		etype = EventLeft
	case StateDead:
		etype = EventFailed
	case StateSuspect:
		etype = EventSuspect
	default:
		return
	}
	select {
	case m.events <- Event{Member: member, Type: etype}:
	default:
		m.logger.Warn("event channel full; dropping event", "addr", member.Addr)
	}
}

// clusterSize returns the current number of known members (including self).
func (m *Membership) clusterSize() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.members) + 1
}
```

Create `gossip.go`. The gossip queue is the dissemination buffer: it holds membership updates ordered by recency and prunes entries once each has been piggybacked ceil(log2(N+1)) times.

```go
package membership

import (
	"math"
	"sort"
	"sync"
)

// GossipEntry is one membership update waiting to be piggybacked on outgoing
// protocol messages.
type GossipEntry struct {
	Member    Member
	SendCount int   // number of times this entry has been piggybacked
	Timestamp int64 // logical clock value at the time of Add; higher = more recent
}

// GossipQueue is a priority buffer for membership updates.
// Entries are ordered by descending Timestamp (newest first) and pruned
// once SendCount reaches ceil(log2(N+1)) where N is the cluster size.
type GossipQueue struct {
	mu      sync.Mutex
	entries []*GossipEntry
	clock   int64 // monotonic logical clock, incremented on every Add
}

// NewGossipQueue returns an empty, ready-to-use GossipQueue.
func NewGossipQueue() *GossipQueue {
	return &GossipQueue{}
}

// Add inserts or replaces a membership update.
// An existing entry for the same address is replaced only if the incoming
// member carries a strictly higher incarnation, or the same incarnation with
// a terminal state overriding a transient one. Stale updates are silently
// discarded to prevent old gossip from reintroducing a previously pruned entry.
func (q *GossipQueue) Add(m Member) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.clock++
	for i, e := range q.entries {
		if e.Member.Addr != m.Addr {
			continue
		}
		if m.Incarnation > e.Member.Incarnation {
			q.entries[i] = &GossipEntry{Member: m, Timestamp: q.clock}
			return
		}
		if m.Incarnation == e.Member.Incarnation {
			terminal := m.State == StateDead || m.State == StateLeft
			transient := e.Member.State == StateAlive || e.Member.State == StateSuspect
			if terminal && transient {
				q.entries[i] = &GossipEntry{Member: m, Timestamp: q.clock}
				return
			}
		}
		// Incoming is not an upgrade; retain the existing entry.
		return
	}
	q.entries = append(q.entries, &GossipEntry{Member: m, Timestamp: q.clock})
}

// Take returns up to n entries ordered newest-first for piggybacking.
// Entries whose SendCount has reached ceil(log2(clusterSize+1)) are pruned
// before selection. Each returned entry's SendCount is incremented.
func (q *GossipQueue) Take(n, clusterSize int) []GossipEntry {
	q.mu.Lock()
	defer q.mu.Unlock()

	maxSends := int(math.Ceil(math.Log2(float64(clusterSize) + 1)))
	if maxSends < 3 {
		maxSends = 3
	}

	// Prune over-sent entries in-place.
	live := q.entries[:0]
	for _, e := range q.entries {
		if e.SendCount < maxSends {
			live = append(live, e)
		}
	}
	q.entries = live

	// Newest events first.
	sort.Slice(q.entries, func(i, j int) bool {
		return q.entries[i].Timestamp > q.entries[j].Timestamp
	})

	out := make([]GossipEntry, 0, n)
	for _, e := range q.entries {
		if len(out) >= n {
			break
		}
		out = append(out, *e)
		e.SendCount++
	}
	return out
}

// Len returns the number of entries currently in the queue.
func (q *GossipQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.entries)
}
```

### Exercise 2: Wire Protocol

Create `message.go`. Each SWIM message is one UDP datagram. The compact binary encoding keeps six gossip entries well within the 1 400-byte MTU constraint.

```go
package membership

import (
	"encoding/binary"
	"fmt"
)

// MessageType identifies the kind of SWIM protocol message.
type MessageType uint8

const (
	MsgPing    MessageType = iota // direct failure probe
	MsgPingReq                    // indirect probe: ask a third party to ping Target
	MsgAck                        // acknowledgement for a ping or ping-req
	MsgAlive                      // incarnation-number refutation of a suspicion
	MsgDead                       // confirmed failure announcement
	MsgLeave                      // graceful departure
)

// Message is a decoded SWIM protocol message.
// Target is non-empty only for MsgPingReq; it identifies the node to probe.
// Gossip carries piggybacked membership updates (at most maxGossipEntries).
type Message struct {
	Type   MessageType
	SeqNo  uint64
	Target string // non-empty only in MsgPingReq
	Gossip []GossipEntry
}

// EncodeMessage serialises msg into a byte slice ready for UDP transmission.
// Returns ErrMessageTooLarge if the encoded size exceeds maxUDPPayload or if
// len(msg.Gossip) exceeds maxGossipEntries.
func EncodeMessage(msg Message) ([]byte, error) {
	if len(msg.Gossip) > maxGossipEntries {
		return nil, fmt.Errorf("%w: %d gossip entries exceeds limit %d",
			ErrMessageTooLarge, len(msg.Gossip), maxGossipEntries)
	}
	if len(msg.Target) > 255 {
		return nil, fmt.Errorf("%w: target address length %d exceeds 255",
			ErrMessageTooLarge, len(msg.Target))
	}

	// Pre-allocate with a generous estimate; append grows as needed.
	buf := make([]byte, 0, 64)
	buf = append(buf, byte(msg.Type))
	buf = binary.BigEndian.AppendUint64(buf, msg.SeqNo)
	// Target field: one length byte followed by the address bytes.
	buf = append(buf, byte(len(msg.Target)))
	buf = append(buf, msg.Target...)
	// Gossip entries.
	buf = append(buf, byte(len(msg.Gossip)))
	for _, e := range msg.Gossip {
		if len(e.Member.Addr) > 255 {
			return nil, fmt.Errorf("%w: member address length %d exceeds 255",
				ErrMessageTooLarge, len(e.Member.Addr))
		}
		buf = append(buf, byte(len(e.Member.Addr)))
		buf = append(buf, e.Member.Addr...)
		buf = append(buf, byte(e.Member.State))
		buf = binary.BigEndian.AppendUint64(buf, e.Member.Incarnation)
	}
	if len(buf) > maxUDPPayload {
		return nil, fmt.Errorf("%w: encoded size %d > %d",
			ErrMessageTooLarge, len(buf), maxUDPPayload)
	}
	return buf, nil
}

// DecodeMessage deserialises a byte slice received from a UDP datagram.
// Returns an error if the slice is truncated or structurally invalid.
func DecodeMessage(data []byte) (Message, error) {
	// Minimum valid message: type(1)+seqno(8)+targetLen(1)+gossipCount(1) = 11.
	if len(data) < 11 {
		return Message{}, fmt.Errorf("membership: message too short: %d bytes", len(data))
	}
	msg := Message{
		Type:  MessageType(data[0]),
		SeqNo: binary.BigEndian.Uint64(data[1:9]),
	}
	pos := 9
	targetLen := int(data[pos])
	pos++
	if targetLen > 0 {
		if pos+targetLen > len(data) {
			return Message{}, fmt.Errorf("membership: truncated target field")
		}
		msg.Target = string(data[pos : pos+targetLen])
		pos += targetLen
	}
	if pos >= len(data) {
		return msg, nil
	}
	gossipCount := int(data[pos])
	pos++
	for i := 0; i < gossipCount; i++ {
		// Each gossip entry: addrLen(1) + addr + state(1) + incarnation(8).
		if pos >= len(data) {
			return Message{}, fmt.Errorf("membership: truncated before gossip entry %d", i)
		}
		addrLen := int(data[pos])
		pos++
		if pos+addrLen+9 > len(data) {
			return Message{}, fmt.Errorf("membership: truncated gossip entry %d", i)
		}
		addr := string(data[pos : pos+addrLen])
		pos += addrLen
		state := NodeState(data[pos])
		pos++
		incarnation := binary.BigEndian.Uint64(data[pos : pos+8])
		pos += 8
		msg.Gossip = append(msg.Gossip, GossipEntry{
			Member: Member{Addr: addr, State: state, Incarnation: incarnation},
		})
	}
	return msg, nil
}
```

### Exercise 3: Failure Detection Loop

Create `probe.go`. This file contains every component that touches the network: the UDP socket lifecycle, the periodic probe round, indirect probing, the receive loop, and the suspicion timer. The state machine (`tryUpdateMember`) stays in `membership.go` to keep the two concerns separate.

```go
package membership

import (
	"fmt"
	"math/rand"
	"net"
	"sync"
	"time"
)

// pendingProbes tracks in-flight probe sequence numbers.
// Each registered seqNo gets a buffered channel closed when an ack arrives.
type pendingProbes struct {
	mu      sync.Mutex
	waiting map[uint64]chan struct{}
}

func newPendingProbes() *pendingProbes {
	return &pendingProbes{waiting: make(map[uint64]chan struct{})}
}

func (p *pendingProbes) register(seqNo uint64) <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	ch := make(chan struct{}, 1)
	p.waiting[seqNo] = ch
	return ch
}

func (p *pendingProbes) unregister(seqNo uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.waiting, seqNo)
}

func (p *pendingProbes) signal(seqNo uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if ch, ok := p.waiting[seqNo]; ok {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// Start binds the UDP port and launches the receive and probe goroutines.
func (m *Membership) Start() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.started {
		return ErrAlreadyStarted
	}
	addr, err := net.ResolveUDPAddr("udp", m.config.BindAddr)
	if err != nil {
		return fmt.Errorf("membership: resolve %s: %w", m.config.BindAddr, err)
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return fmt.Errorf("membership: listen: %w", err)
	}
	m.conn = conn
	m.started = true
	go m.receiveLoop()
	go m.probeLoop()
	return nil
}

// Stop broadcasts a graceful-leave message and closes the UDP socket.
func (m *Membership) Stop() error {
	m.mu.Lock()
	if !m.started {
		m.mu.Unlock()
		return ErrNotStarted
	}
	m.self.State = StateLeft
	m.mu.Unlock()

	gossip := m.queue.Take(m.config.MaxGossipEntries, m.clusterSize())
	msg := Message{
		Type:   MsgLeave,
		SeqNo:  m.seqNo.Add(1),
		Gossip: gossip,
	}
	m.broadcastMessage(msg)
	close(m.done)
	return m.conn.Close()
}

// Join contacts seed addresses and announces this node's presence by sending
// a ping carrying a gossip entry for self. Seeds add us to their member list
// through the gossip entries on the ack they send back.
func (m *Membership) Join(seeds []string) error {
	m.mu.RLock()
	if !m.started {
		m.mu.RUnlock()
		return ErrNotStarted
	}
	selfInc := m.self.Incarnation
	m.mu.RUnlock()

	self := Member{Addr: m.config.BindAddr, State: StateAlive, Incarnation: selfInc}
	m.queue.Add(self)

	for _, seed := range seeds {
		if seed == m.config.BindAddr {
			continue
		}
		msg := Message{
			Type:   MsgPing,
			SeqNo:  m.seqNo.Add(1),
			Gossip: m.queue.Take(m.config.MaxGossipEntries, m.clusterSize()),
		}
		if err := m.sendTo(msg, seed); err != nil {
			m.logger.Warn("join ping failed", "seed", seed, "err", err)
		}
	}
	return nil
}

// probeLoop runs the periodic probe round on the ProbeInterval ticker.
func (m *Membership) probeLoop() {
	ticker := time.NewTicker(m.config.ProbeInterval)
	defer ticker.Stop()
	for {
		select {
		case <-m.done:
			return
		case <-ticker.C:
			m.probeRandom()
		}
	}
}

// probeRandom selects a random alive or suspect peer, probes it directly, then
// via indirect probers if the direct ping times out. Marks the peer suspect if
// both attempts fail.
func (m *Membership) probeRandom() {
	target := m.pickRandomMember()
	if target == "" {
		return
	}

	size := m.clusterSize()
	gossip := m.queue.Take(m.config.MaxGossipEntries, size)

	// Direct probe.
	seqNo := m.seqNo.Add(1)
	ackCh := m.pending.register(seqNo)
	defer m.pending.unregister(seqNo)

	if err := m.sendTo(Message{Type: MsgPing, SeqNo: seqNo, Gossip: gossip}, target); err != nil {
		m.logger.Warn("direct ping failed", "target", target, "err", err)
		return
	}
	select {
	case <-ackCh:
		return // direct ack received
	case <-time.After(m.config.ProbeTimeout):
	}

	// Indirect probe via k random members.
	indirects := m.pickIndirectProbers(target, m.config.IndirectProbes)
	seqNo2 := m.seqNo.Add(1)
	indirectCh := m.pending.register(seqNo2)
	defer m.pending.unregister(seqNo2)

	for _, via := range indirects {
		req := Message{
			Type:   MsgPingReq,
			SeqNo:  seqNo2,
			Target: target,
			Gossip: gossip,
		}
		if err := m.sendTo(req, via); err != nil {
			m.logger.Warn("ping-req failed", "via", via, "err", err)
		}
	}
	select {
	case <-indirectCh:
		return // indirect ack received
	case <-time.After(m.config.ProbeTimeout * 2):
	}

	// No response from any path: suspect the peer.
	m.mu.RLock()
	var inc uint64
	if mem, ok := m.members[target]; ok {
		inc = mem.Incarnation
	}
	m.mu.RUnlock()

	suspect := Member{Addr: target, State: StateSuspect, Incarnation: inc}
	if m.tryUpdateMember(suspect) {
		go m.scheduleSuspicionTimer(target, inc)
	}
}

// scheduleSuspicionTimer waits SuspicionTimeout then transitions the peer from
// suspect to dead, unless the suspicion was refuted (higher incarnation observed).
func (m *Membership) scheduleSuspicionTimer(addr string, incarnation uint64) {
	select {
	case <-time.After(m.config.SuspicionTimeout):
	case <-m.done:
		return
	}
	m.mu.RLock()
	mem, ok := m.members[addr]
	stale := !ok ||
		mem.Incarnation > incarnation ||
		mem.State == StateDead ||
		mem.State == StateLeft
	m.mu.RUnlock()
	if stale {
		return // peer refuted the suspicion or is already terminal
	}
	m.tryUpdateMember(Member{Addr: addr, State: StateDead, Incarnation: incarnation})
}

// receiveLoop reads UDP datagrams, applies piggybacked gossip, and dispatches
// each message to handleMessage.
func (m *Membership) receiveLoop() {
	buf := make([]byte, maxUDPPayload)
	for {
		n, fromAddr, err := m.conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-m.done:
				return
			default:
				m.logger.Warn("receive error", "err", err)
				continue
			}
		}
		msg, err := DecodeMessage(buf[:n])
		if err != nil {
			m.logger.Warn("decode error", "from", fromAddr, "err", err)
			continue
		}
		// Apply gossip before handling the message so that a ping-req handler
		// already has an up-to-date view of the cluster when it picks indirects.
		for _, entry := range msg.Gossip {
			m.tryUpdateMember(entry.Member)
		}
		m.handleMessage(msg, fromAddr.String())
	}
}

// handleMessage dispatches a decoded SWIM message to the appropriate handler.
func (m *Membership) handleMessage(msg Message, from string) {
	switch msg.Type {
	case MsgPing:
		ack := Message{
			Type:   MsgAck,
			SeqNo:  msg.SeqNo,
			Gossip: m.queue.Take(m.config.MaxGossipEntries, m.clusterSize()),
		}
		if err := m.sendTo(ack, from); err != nil {
			m.logger.Warn("ack send failed", "to", from, "err", err)
		}
	case MsgPingReq:
		go m.forwardPingReq(msg, from)
	case MsgAck:
		m.pending.signal(msg.SeqNo)
	case MsgLeave:
		m.mu.Lock()
		if mem, ok := m.members[from]; ok {
			mem.State = StateLeft
			m.queue.Add(*mem)
			m.publishEventLocked(*mem)
		}
		m.mu.Unlock()
	}
}

// forwardPingReq pings msg.Target on behalf of the peer that sent the MsgPingReq,
// then relays the ack back to the original sender so it can resolve its probe.
func (m *Membership) forwardPingReq(msg Message, originalSender string) {
	if msg.Target == "" {
		return
	}
	seqNo := m.seqNo.Add(1)
	ackCh := m.pending.register(seqNo)
	defer m.pending.unregister(seqNo)

	fwd := Message{
		Type:   MsgPing,
		SeqNo:  seqNo,
		Gossip: m.queue.Take(m.config.MaxGossipEntries, m.clusterSize()),
	}
	if err := m.sendTo(fwd, msg.Target); err != nil {
		return
	}
	select {
	case <-ackCh:
		relay := Message{Type: MsgAck, SeqNo: msg.SeqNo}
		if err := m.sendTo(relay, originalSender); err != nil {
			m.logger.Warn("relay ack failed", "to", originalSender, "err", err)
		}
	case <-time.After(m.config.ProbeTimeout * 2):
	}
}

// sendTo encodes msg and writes it as a UDP datagram to the given addr string.
func (m *Membership) sendTo(msg Message, addr string) error {
	data, err := EncodeMessage(msg)
	if err != nil {
		return err
	}
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return fmt.Errorf("membership: resolve %s: %w", addr, err)
	}
	_, err = m.conn.WriteToUDP(data, udpAddr)
	return err
}

// broadcastMessage sends msg to every known alive or suspect member.
func (m *Membership) broadcastMessage(msg Message) {
	m.mu.RLock()
	addrs := make([]string, 0, len(m.members))
	for addr, mem := range m.members {
		if mem.State == StateAlive || mem.State == StateSuspect {
			addrs = append(addrs, addr)
		}
	}
	m.mu.RUnlock()
	for _, addr := range addrs {
		if err := m.sendTo(msg, addr); err != nil {
			m.logger.Warn("broadcast failed", "addr", addr, "err", err)
		}
	}
}

// pickRandomMember returns the address of a random alive or suspect peer,
// or the empty string when no eligible peers exist.
func (m *Membership) pickRandomMember() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	pool := make([]string, 0, len(m.members))
	for addr, mem := range m.members {
		if mem.State == StateAlive || mem.State == StateSuspect {
			pool = append(pool, addr)
		}
	}
	if len(pool) == 0 {
		return ""
	}
	return pool[rand.Intn(len(pool))]
}

// pickIndirectProbers returns up to k alive peers excluding the given address,
// in random order.
func (m *Membership) pickIndirectProbers(exclude string, k int) []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	pool := make([]string, 0, len(m.members))
	for addr, mem := range m.members {
		if addr != exclude && mem.State == StateAlive {
			pool = append(pool, addr)
		}
	}
	rand.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })
	if k > len(pool) {
		k = len(pool)
	}
	return pool[:k]
}
```

### Exercise 4: Test the Contract

Create `membership_test.go`. The tests cover the pure-logic components — the state machine, gossip queue, and wire encoding — and do not open network sockets, so they run fully offline and under the race detector.

```go
package membership

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// --- NodeState ---

func TestNodeStateString(t *testing.T) {
	t.Parallel()
	cases := []struct {
		state NodeState
		want  string
	}{
		{StateAlive, "alive"},
		{StateSuspect, "suspect"},
		{StateDead, "dead"},
		{StateLeft, "left"},
		{NodeState(99), "NodeState(99)"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.want, func(t *testing.T) {
			t.Parallel()
			if got := tc.state.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}

func ExampleNodeState_String() {
	states := []NodeState{StateAlive, StateSuspect, StateDead, StateLeft}
	for _, s := range states {
		fmt.Println(s)
	}
	// Output:
	// alive
	// suspect
	// dead
	// left
}

// --- Config / New ---

func TestDefaultConfigRejectsEmptyAddr(t *testing.T) {
	t.Parallel()
	_, err := DefaultConfig("")
	if !errors.Is(err, ErrBadConfig) {
		t.Errorf("DefaultConfig(\"\") err = %v, want ErrBadConfig", err)
	}
}

func TestDefaultConfigValues(t *testing.T) {
	t.Parallel()
	cfg, err := DefaultConfig("127.0.0.1:7946")
	if err != nil {
		t.Fatalf("DefaultConfig: %v", err)
	}
	if cfg.IndirectProbes != 3 {
		t.Errorf("IndirectProbes = %d, want 3", cfg.IndirectProbes)
	}
	if cfg.ProbeInterval != 200*time.Millisecond {
		t.Errorf("ProbeInterval = %v, want 200ms", cfg.ProbeInterval)
	}
	if cfg.MaxGossipEntries != maxGossipEntries {
		t.Errorf("MaxGossipEntries = %d, want %d", cfg.MaxGossipEntries, maxGossipEntries)
	}
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		config Config
	}{
		{
			name: "empty bind addr",
			config: Config{
				BindAddr:       "",
				ProbeInterval:  200 * time.Millisecond,
				IndirectProbes: 3,
			},
		},
		{
			name: "zero probe interval",
			config: Config{
				BindAddr:       "127.0.0.1:7946",
				ProbeInterval:  0,
				IndirectProbes: 3,
			},
		},
		{
			name: "zero indirect probes",
			config: Config{
				BindAddr:       "127.0.0.1:7946",
				ProbeInterval:  time.Millisecond,
				IndirectProbes: 0,
			},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := New(tc.config)
			if !errors.Is(err, ErrBadConfig) {
				t.Errorf("New() err = %v, want ErrBadConfig", err)
			}
		})
	}
}

// --- GossipQueue ---

func TestGossipQueuePriority(t *testing.T) {
	t.Parallel()
	q := NewGossipQueue()
	q.Add(Member{Addr: "a:1", State: StateAlive, Incarnation: 1})
	q.Add(Member{Addr: "b:1", State: StateAlive, Incarnation: 1})
	q.Add(Member{Addr: "c:1", State: StateDead, Incarnation: 1})
	// c:1 was added last and has the highest timestamp: it must come out first.
	entries := q.Take(2, 10)
	if len(entries) != 2 {
		t.Fatalf("Take(2) returned %d entries, want 2", len(entries))
	}
	if entries[0].Member.Addr != "c:1" {
		t.Errorf("entries[0].Addr = %q, want %q (newest first)", entries[0].Member.Addr, "c:1")
	}
}

func TestGossipQueueOverwritesLowerIncarnation(t *testing.T) {
	t.Parallel()
	q := NewGossipQueue()
	q.Add(Member{Addr: "a:1", State: StateAlive, Incarnation: 1})
	q.Add(Member{Addr: "a:1", State: StateSuspect, Incarnation: 2})
	entries := q.Take(10, 10)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry after overwrite, got %d", len(entries))
	}
	if entries[0].Member.Incarnation != 2 || entries[0].Member.State != StateSuspect {
		t.Errorf("entry = %+v, want incarnation=2 state=suspect", entries[0].Member)
	}
}

func TestGossipQueueIgnoresLowerIncarnation(t *testing.T) {
	t.Parallel()
	q := NewGossipQueue()
	q.Add(Member{Addr: "a:1", State: StateDead, Incarnation: 5})
	// Stale alive at incarnation 3 must not overwrite dead at incarnation 5.
	q.Add(Member{Addr: "a:1", State: StateAlive, Incarnation: 3})
	entries := q.Take(10, 10)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Member.State != StateDead {
		t.Errorf("state = %v, want dead (stale alive must not overwrite)", entries[0].Member.State)
	}
}

func TestGossipQueuePrune(t *testing.T) {
	t.Parallel()
	// clusterSize=4: ceil(log2(5)) = 3; entry is pruned after 3 sends.
	q := NewGossipQueue()
	q.Add(Member{Addr: "a:1", State: StateAlive, Incarnation: 1})
	for i := 0; i < 3; i++ {
		got := q.Take(1, 4)
		if len(got) == 0 {
			t.Fatalf("round %d: expected 1 entry, got none", i)
		}
	}
	// Fourth Take: entry must have been pruned.
	got := q.Take(1, 4)
	if len(got) != 0 {
		t.Errorf("expected empty queue after max sends, got %d entries", len(got))
	}
}

// --- Message wire encoding ---

func TestMessageRoundTrip(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		msg  Message
	}{
		{
			name: "ping no gossip",
			msg:  Message{Type: MsgPing, SeqNo: 1},
		},
		{
			name: "ack with gossip",
			msg: Message{
				Type:  MsgAck,
				SeqNo: 42,
				Gossip: []GossipEntry{
					{Member: Member{Addr: "192.168.1.1:7946", State: StateAlive, Incarnation: 7}},
					{Member: Member{Addr: "192.168.1.2:7946", State: StateSuspect, Incarnation: 3}},
				},
			},
		},
		{
			name: "ping-req with target and gossip",
			msg: Message{
				Type:   MsgPingReq,
				SeqNo:  99,
				Target: "10.0.0.5:7946",
				Gossip: []GossipEntry{
					{Member: Member{Addr: "10.0.0.1:7946", State: StateDead, Incarnation: 12}},
				},
			},
		},
		{
			name: "leave no gossip",
			msg:  Message{Type: MsgLeave, SeqNo: 1000},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			data, err := EncodeMessage(tc.msg)
			if err != nil {
				t.Fatalf("EncodeMessage: %v", err)
			}
			got, err := DecodeMessage(data)
			if err != nil {
				t.Fatalf("DecodeMessage: %v", err)
			}
			if got.Type != tc.msg.Type || got.SeqNo != tc.msg.SeqNo {
				t.Errorf("header: got {%v %d}, want {%v %d}",
					got.Type, got.SeqNo, tc.msg.Type, tc.msg.SeqNo)
			}
			if got.Target != tc.msg.Target {
				t.Errorf("target: got %q, want %q", got.Target, tc.msg.Target)
			}
			if len(got.Gossip) != len(tc.msg.Gossip) {
				t.Fatalf("gossip count: got %d, want %d", len(got.Gossip), len(tc.msg.Gossip))
			}
			for i, e := range got.Gossip {
				o := tc.msg.Gossip[i]
				if e.Member != o.Member {
					t.Errorf("gossip[%d]: got %+v, want %+v", i, e.Member, o.Member)
				}
			}
		})
	}
}

func TestMessageSizeBound(t *testing.T) {
	t.Parallel()
	// Worst case: maxGossipEntries entries with long addresses and a target.
	addr := "192.168.255.255:65535" // 21 bytes
	gossip := make([]GossipEntry, maxGossipEntries)
	for i := range gossip {
		gossip[i] = GossipEntry{
			Member: Member{Addr: addr, State: StateDead, Incarnation: ^uint64(0)},
		}
	}
	msg := Message{
		Type:   MsgPingReq,
		SeqNo:  ^uint64(0),
		Target: addr,
		Gossip: gossip,
	}
	data, err := EncodeMessage(msg)
	if err != nil {
		t.Fatalf("EncodeMessage: %v", err)
	}
	if len(data) > maxUDPPayload {
		t.Errorf("encoded size %d exceeds maxUDPPayload %d", len(data), maxUDPPayload)
	}
}

func TestMessageRejectsTooManyGossipEntries(t *testing.T) {
	t.Parallel()
	gossip := make([]GossipEntry, maxGossipEntries+1)
	_, err := EncodeMessage(Message{Type: MsgPing, SeqNo: 1, Gossip: gossip})
	if !errors.Is(err, ErrMessageTooLarge) {
		t.Errorf("err = %v, want ErrMessageTooLarge", err)
	}
}

func TestDecodeMessageTooShort(t *testing.T) {
	t.Parallel()
	_, err := DecodeMessage([]byte{byte(MsgPing), 0, 0})
	if err == nil {
		t.Error("expected error for truncated message, got nil")
	}
}

// --- tryUpdateMember state machine ---

func newTestMembership(t *testing.T) *Membership {
	t.Helper()
	cfg, err := DefaultConfig("127.0.0.1:7946")
	if err != nil {
		t.Fatalf("DefaultConfig: %v", err)
	}
	m, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m
}

func TestTryUpdateMemberFirstSeen(t *testing.T) {
	t.Parallel()
	m := newTestMembership(t)
	changed := m.tryUpdateMember(Member{Addr: "10.0.0.2:7946", State: StateAlive, Incarnation: 1})
	if !changed {
		t.Error("first-seen member: expected changed=true")
	}
	found := false
	for _, mem := range m.Members() {
		if mem.Addr == "10.0.0.2:7946" {
			found = true
		}
	}
	if !found {
		t.Error("new member not found in Members()")
	}
}

func TestTryUpdateMemberHigherIncarnationWins(t *testing.T) {
	t.Parallel()
	m := newTestMembership(t)
	m.tryUpdateMember(Member{Addr: "10.0.0.2:7946", State: StateSuspect, Incarnation: 5})
	changed := m.tryUpdateMember(Member{Addr: "10.0.0.2:7946", State: StateAlive, Incarnation: 6})
	if !changed {
		t.Error("higher incarnation: expected changed=true")
	}
	for _, mem := range m.Members() {
		if mem.Addr == "10.0.0.2:7946" {
			if mem.State != StateAlive || mem.Incarnation != 6 {
				t.Errorf("member = %+v, want state=alive incarnation=6", mem)
			}
			return
		}
	}
	t.Error("updated member not found in Members()")
}

func TestTryUpdateMemberDeadTerminalAtEqualIncarnation(t *testing.T) {
	t.Parallel()
	m := newTestMembership(t)
	m.tryUpdateMember(Member{Addr: "10.0.0.2:7946", State: StateDead, Incarnation: 3})
	// alive at same incarnation must not override dead.
	changed := m.tryUpdateMember(Member{Addr: "10.0.0.2:7946", State: StateAlive, Incarnation: 3})
	if changed {
		t.Error("dead state must not be overridden by alive at equal incarnation")
	}
}

func TestTryUpdateMemberSelfRefutation(t *testing.T) {
	t.Parallel()
	m := newTestMembership(t)
	initialInc := m.self.Incarnation
	// Receiving a suspect for ourselves triggers incarnation increment and alive broadcast.
	m.tryUpdateMember(Member{Addr: m.config.BindAddr, State: StateSuspect, Incarnation: initialInc})
	if m.self.Incarnation <= initialInc {
		t.Errorf("self incarnation = %d, want > %d after refutation", m.self.Incarnation, initialInc)
	}
	if m.self.State != StateAlive {
		t.Errorf("self state = %v after refutation, want alive", m.self.State)
	}
	// The gossip queue should have an alive entry with the new incarnation.
	entries := m.queue.Take(10, 1)
	if len(entries) == 0 {
		t.Fatal("expected refutation entry in gossip queue, got none")
	}
	e := entries[0]
	if e.Member.State != StateAlive || e.Member.Incarnation != m.self.Incarnation {
		t.Errorf("refutation gossip = %+v, want state=alive incarnation=%d",
			e.Member, m.self.Incarnation)
	}
}

// Your turn: add TestTryUpdateMemberStaleGossipIgnored that creates a Membership,
// applies an alive update at incarnation 10, then applies a suspect at incarnation 8,
// and asserts the member remains alive at incarnation 10.
```

Create `cmd/demo/main.go` to exercise the exported API against real UDP sockets:

```go
package main

import (
	"fmt"
	"log"
	"time"

	"example.com/membership"
)

func main() {
	addrs := []string{
		"127.0.0.1:17946",
		"127.0.0.1:17947",
		"127.0.0.1:17948",
	}

	nodes := make([]*membership.Membership, len(addrs))
	for i, addr := range addrs {
		cfg, err := membership.DefaultConfig(addr)
		if err != nil {
			log.Fatalf("config node %d: %v", i, err)
		}
		m, err := membership.New(cfg)
		if err != nil {
			log.Fatalf("new node %d: %v", i, err)
		}
		if err := m.Start(); err != nil {
			log.Fatalf("start node %d: %v", i, err)
		}
		nodes[i] = m
	}

	// Nodes 1 and 2 join through node 0 as the seed.
	seeds := addrs[:1]
	for i := 1; i < len(nodes); i++ {
		if err := nodes[i].Join(seeds); err != nil {
			log.Fatalf("join node %d: %v", i, err)
		}
	}

	time.Sleep(2 * time.Second)
	printView("after join", nodes[0])

	fmt.Println("\nnode 2 leaving gracefully...")
	if err := nodes[2].Stop(); err != nil {
		log.Printf("stop node 2: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	printView("after node 2 left", nodes[0])

	for i := 0; i < 2; i++ {
		_ = nodes[i].Stop()
	}
}

func printView(label string, m *membership.Membership) {
	fmt.Printf("\n=== membership view (%s) ===\n", label)
	for _, mem := range m.Members() {
		fmt.Printf("  %-22s  state=%-7s  incarnation=%d\n",
			mem.Addr, mem.State, mem.Incarnation)
	}
}
```

## Common Mistakes

**Wrong**: Treating the incarnation number as a simple version counter that increments on every state change, including normal alive heartbeats.
What happens: the incarnation field inflates rapidly, and a restarted node whose stored incarnation was lost begins at 1 and is immediately overridden by stale `dead` or `suspect` gossip still circulating from its previous failure.
Fix: increment incarnation only to refute a suspicion or on restart. A restarted node must begin with a value higher than any previously observed for its address; a common production strategy is to seed incarnation from `time.Now().UnixNano()` so restarts separated by any interval produce a safely higher value.

**Wrong**: Selecting the same probe target on every round — for example always choosing `m.members[0]` — instead of drawing uniformly at random.
What happens: one peer receives all probes and its failure is detected quickly, while the rest of the cluster could fail undetected indefinitely.
Fix: `pickRandomMember` must draw uniformly from the full alive/suspect pool on every call, as shown. The SWIM paper proves that uniform random selection achieves O(log N) expected detection time per member across the whole cluster.

**Wrong**: Forgetting to call `m.pending.unregister(seqNo)` after a probe round completes.
What happens: every timed-out probe round leaks one entry in `pending.waiting`. Over hours, the map grows without bound. A late ack also signals a stale channel that no goroutine is reading.
Fix: always pair `register` with a `defer unregister` in the same function, as shown in `probeRandom`.

**Wrong**: Sending `MsgLeave` only to the seed node and relying on gossip to propagate the departure.
What happens: if the seed is unreachable at shutdown time, no other node learns of the graceful departure and the cluster waits out the full 5-second suspicion timeout before removing the departed node.
Fix: `broadcastMessage` sends to every known live member at the moment of departure so the leave is immediately visible to all reachable peers.

**Wrong**: Including the local Lamport timestamp in the gossip entry wire format and using it on the receiver to decide whether an update supersedes the local state.
What happens: logical clocks are not synchronised across nodes; a receiver with a lagging clock treats fresh gossip as stale and ignores it, keeping a falsely suspected node in the `suspect` state indefinitely.
Fix: the `Timestamp` field in `GossipEntry` is a local priority hint — it is never serialised. Receivers assign their own timestamp on `GossipQueue.Add`. The `Incarnation` field, which crosses the wire, is the only ordering signal receivers trust.

## Verification

From `~/go-exercises/membership`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass before moving on. The tests run without network access; `go test` is the verification for the state machine, gossip queue, and wire encoding.

To exercise the full network path:

```bash
go run ./cmd/demo
```

The demo should print three members after the join phase and two members (nodes 0 and 1) after node 2 departs. Node 2 must not appear in the second view because `Members()` filters terminal states.

For packet-loss resilience, wrap the UDP conn in a test double that drops a configurable fraction of writes and confirm that the indirect probing path prevents the surviving node from being declared dead. The gossip pruning bound is pinned by `TestGossipQueuePrune`; verify it holds as cluster size grows.

## Summary

- SWIM interleaves failure detection (probe rounds every T milliseconds) and membership dissemination (gossip piggybacking) in a single UDP message stream with no extra bandwidth.
- Two-phase probing (direct then indirect via k peers) bounds the false-positive rate without relying on long timeouts.
- Incarnation numbers provide a totally ordered refutation: a falsely suspected node overrides the suspicion by incrementing and rebroadcasting its own incarnation.
- The gossip queue enforces O(log N) convergence by tracking per-entry send counts and pruning entries once they have been piggybacked ceil(log2(N+1)) times.
- Dead and left are terminal states at a given incarnation; re-entry to alive requires a restart with a strictly higher incarnation.
- The binary wire format keeps six gossip entries under 250 bytes in the common case, well within the 1 400-byte UDP payload limit.

## What's Next

Next: [Client Protocol](../07-client-protocol/07-client-protocol.md).

## Resources

- Das, Gupta, Motivala. "SWIM: Scalable Weakly-consistent Infection-style Process Group Membership Protocol." DSN 2002. The primary source for all protocol parameters (T, k, suspicion sub-protocol, incarnation numbers). https://ieeexplore.ieee.org/document/1028914
- Dadgar, Bluth, Cole. "Lifeguard: Local Health Awareness for More Accurate Failure Detection." 2017. HashiCorp's extensions: dynamic suspicion timeouts and self-awareness. https://arxiv.org/abs/1707.00788
- Go `net` package — `UDPConn`, `ListenUDP`, `ReadFromUDP`, `WriteToUDP`. https://pkg.go.dev/net
- Go `encoding/binary` package — `AppendUint64`, `BigEndian.Uint64` (added Go 1.21). https://pkg.go.dev/encoding/binary
- HashiCorp memberlist — production SWIM implementation in Go; `state.go` and `net.go` show real-world engineering choices (dynamic suspicion, dead node cleanup, address conflict resolution). https://github.com/hashicorp/memberlist
