# 14. Chaos Testing Framework

Distributed systems claim to tolerate faults. Chaos testing verifies that claim by injecting controlled failures — network partitions, node crashes, message delays — into a running system and checking that the invariants the system promises (no lost writes, eventual consistency) still hold. The hard part is not causing faults; it is recording every operation with enough fidelity to determine, after the fact, whether the system's observed behavior was correct.

This lesson builds a self-contained, in-process chaos testing framework. The "cluster" is a set of goroutines communicating through channels wrapped in a `FaultyTransport` that can drop, delay, or partition messages on demand. A `Recorder` appends every operation invocation and response to an immutable log. After a test run, an invariant checker walks the log to find violations.

```text
chaos/
  go.mod
  chaos.go
  chaos_test.go
  cmd/demo/main.go
```

## Concepts

### Fault Injection Through a Transport Abstraction

Real chaos tools (Jepsen, Chaos Monkey) intercept network packets at the OS layer. In an in-process simulation the equivalent is a channel wrapper that sits between every pair of nodes. Every message passes through `FaultyTransport.Send`, which consults the current fault configuration before deciding whether to deliver, drop, or delay the message.

The fault configuration is mutable at runtime, so a test can partition the network at time T, run workload, then heal the partition at T+2s and observe recovery — without stopping the simulated nodes.

### The Connectivity Matrix

A network partition is represented as a boolean matrix `connected[from][to]`. When `connected[i][j]` is false, node i cannot send to node j. Healing the partition sets the cell back to true. A test client (sender id below zero) is treated as always connected so that tests can inject partitions between nodes without also cutting off the test harness itself.

### Reproducible Randomness

Every random decision (which message to drop) flows through a single `*rand.Rand` seeded at the start of the run. Given the same seed, the same fault sequence plays back identically — the minimum requirement for debugging a detected violation.

`rand.New(rand.NewSource(seed))` gives a deterministic, non-thread-safe PRNG. The transport holds a mutex and draws only under that lock, so determinism is not broken by concurrent callers.

### The Operation Log and Invariant Checking

The log stores `Event` values in append order. An event is either a fault event (partition, heal, crash, restart, delay) or an operation event (invoke or response). The simplest useful invariant for a key-value store is "no acknowledged write is lost": for every `Put(k,v)` response recorded, a subsequent `Get(k)` response must return v. The checker walks the log forward in time, maintaining a map of the last acknowledged write per key, and reports any Get whose value does not match.

A full linearizability check (the Porcupine/Wing-Gong algorithm) is NP-complete in the worst case and requires an external dependency. This lesson implements the tractable "acknowledged-write durability" invariant, which catches the most common correctness bugs without any external library.

### Fault Types

| Fault | Effect |
|-------|--------|
| Partition(from, to) | Drops all messages from node `from` to node `to` |
| Heal(from, to) | Restores the connection |
| Delay(from, to, d) | Adds `d` latency before delivery |
| Crash(id) | Stops the node from processing incoming messages |
| Restart(id) | Resumes a crashed node |

## Exercises

### Exercise 1: Event Log and Fault Configuration

Create `chaos.go`:

```go
package chaos

import (
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"time"
)

// EventKind distinguishes fault events from operation events.
type EventKind int

const (
	KindFault EventKind = iota
	KindInvoke
	KindResponse
)

// Event is a single entry in the operation log.
type Event struct {
	At     time.Time
	Kind   EventKind
	NodeID int
	Op     string // "put", "get", "partition", "heal", "crash", "restart"
	Key    string
	Value  string
	Detail string // human-readable detail for fault events
}

// ErrDropped is returned when a transport drops a message due to a fault.
var ErrDropped = errors.New("message dropped by fault injector")

// Recorder is a thread-safe append-only event log.
type Recorder struct {
	mu     sync.Mutex
	events []Event
}

// Append adds an event to the log.
func (r *Recorder) Append(e Event) {
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
}

// Snapshot returns a copy of the log at the time of the call.
func (r *Recorder) Snapshot() []Event {
	r.mu.Lock()
	out := make([]Event, len(r.events))
	copy(out, r.events)
	r.mu.Unlock()
	return out
}

// Len returns the number of recorded events.
func (r *Recorder) Len() int {
	r.mu.Lock()
	n := len(r.events)
	r.mu.Unlock()
	return n
}

// FaultConfig is the mutable fault state consulted by FaultyTransport.
// All methods are safe for concurrent use. Node IDs must be in [0, 7].
type FaultConfig struct {
	mu        sync.RWMutex
	connected [8][8]bool
	crashed   [8]bool
	delays    [8][8]time.Duration
	n         int
}

// NewFaultConfig returns a fully connected configuration for n nodes (n <= 8).
func NewFaultConfig(n int) *FaultConfig {
	if n > 8 {
		panic("FaultConfig supports at most 8 nodes")
	}
	fc := &FaultConfig{n: n}
	for i := 0; i < n; i++ {
		for j := 0; j < n; j++ {
			fc.connected[i][j] = true
		}
	}
	return fc
}

// Partition drops messages from node from to node to (one direction).
func (fc *FaultConfig) Partition(from, to int) {
	fc.mu.Lock()
	fc.connected[from][to] = false
	fc.mu.Unlock()
}

// Heal restores the connection from node from to node to.
func (fc *FaultConfig) Heal(from, to int) {
	fc.mu.Lock()
	fc.connected[from][to] = true
	fc.mu.Unlock()
}

// Crash stops a node from processing received messages.
func (fc *FaultConfig) Crash(id int) {
	fc.mu.Lock()
	fc.crashed[id] = true
	fc.mu.Unlock()
}

// Restart allows a node to process messages again.
func (fc *FaultConfig) Restart(id int) {
	fc.mu.Lock()
	fc.crashed[id] = false
	fc.mu.Unlock()
}

// SetDelay configures added latency for messages on the from->to link.
func (fc *FaultConfig) SetDelay(from, to int, d time.Duration) {
	fc.mu.Lock()
	fc.delays[from][to] = d
	fc.mu.Unlock()
}

// IsCrashed reports whether a node is currently crashed.
func (fc *FaultConfig) IsCrashed(id int) bool {
	fc.mu.RLock()
	v := fc.crashed[id]
	fc.mu.RUnlock()
	return v
}

// CanSend reports whether from can send to to.
// Senders with id < 0 are treated as test clients and are always permitted.
func (fc *FaultConfig) CanSend(from, to int) bool {
	if from < 0 {
		return true
	}
	fc.mu.RLock()
	ok := fc.connected[from][to] && !fc.crashed[from]
	fc.mu.RUnlock()
	return ok
}

// DelayFor returns the configured latency for the from->to link.
// Returns zero for test-client senders (from < 0).
func (fc *FaultConfig) DelayFor(from, to int) time.Duration {
	if from < 0 {
		return 0
	}
	fc.mu.RLock()
	d := fc.delays[from][to]
	fc.mu.RUnlock()
	return d
}

// Message is a key-value request passed between nodes or from a test client.
type Message struct {
	From  int
	To    int
	Op    string // "put" or "get"
	Key   string
	Value string
}

// FaultyTransport delivers messages with configurable fault injection.
type FaultyTransport struct {
	fc      *FaultConfig
	rng     *rand.Rand
	mu      sync.Mutex // guards rng
	dropPct float64    // fraction [0,1) of messages to drop randomly
}

// NewFaultyTransport returns a transport seeded for reproducible randomness.
func NewFaultyTransport(fc *FaultConfig, seed int64, dropPct float64) *FaultyTransport {
	return &FaultyTransport{
		fc:      fc,
		rng:     rand.New(rand.NewSource(seed)),
		dropPct: dropPct,
	}
}

// Send delivers msg to dst if faults permit; returns ErrDropped otherwise.
// A configured delay is applied before delivery but after the fault check.
func (ft *FaultyTransport) Send(msg Message, dst chan<- Message) error {
	if !ft.fc.CanSend(msg.From, msg.To) {
		return fmt.Errorf("%w: %d->%d partitioned", ErrDropped, msg.From, msg.To)
	}
	ft.mu.Lock()
	roll := ft.rng.Float64()
	ft.mu.Unlock()
	if roll < ft.dropPct {
		return fmt.Errorf("%w: random drop %d->%d", ErrDropped, msg.From, msg.To)
	}
	if d := ft.fc.DelayFor(msg.From, msg.To); d > 0 {
		time.Sleep(d)
	}
	dst <- msg
	return nil
}
```

The `CanSend` method treats negative sender IDs as test clients exempt from the partition matrix, preventing out-of-bounds array indexing.

### Exercise 2: Simulated Node

Append to `chaos.go`:

```go
// Node is a simulated key-value store node driven by a goroutine.
type Node struct {
	ID      int
	storeMu sync.RWMutex
	store   map[string]string
	inbox   chan Message
	fc      *FaultConfig
	ft      *FaultyTransport
	rec     *Recorder
	stopCh  chan struct{}
}

// NewNode creates a node wired to the given transport and recorder.
func NewNode(id int, fc *FaultConfig, ft *FaultyTransport, rec *Recorder) *Node {
	return &Node{
		ID:     id,
		store:  make(map[string]string),
		inbox:  make(chan Message, 64),
		fc:     fc,
		ft:     ft,
		rec:    rec,
		stopCh: make(chan struct{}),
	}
}

// Inbox returns the write end of this node's message channel.
func (n *Node) Inbox() chan<- Message {
	return n.inbox
}

// Transport returns the node's FaultyTransport (useful for inter-node sends).
func (n *Node) Transport() *FaultyTransport {
	return n.ft
}

// Start begins processing incoming messages until Stop is called.
func (n *Node) Start() {
	go func() {
		for {
			select {
			case <-n.stopCh:
				return
			case msg := <-n.inbox:
				if n.fc.IsCrashed(n.ID) {
					continue // silently drop while crashed
				}
				switch msg.Op {
				case "put":
					n.storeMu.Lock()
					n.store[msg.Key] = msg.Value
					n.storeMu.Unlock()
					n.rec.Append(Event{
						At:     time.Now(),
						Kind:   KindResponse,
						NodeID: n.ID,
						Op:     "put",
						Key:    msg.Key,
						Value:  msg.Value,
					})
				case "get":
					n.storeMu.RLock()
					v := n.store[msg.Key]
					n.storeMu.RUnlock()
					n.rec.Append(Event{
						At:     time.Now(),
						Kind:   KindResponse,
						NodeID: n.ID,
						Op:     "get",
						Key:    msg.Key,
						Value:  v,
					})
				}
			}
		}
	}()
}

// Stop shuts down the node's processing goroutine.
func (n *Node) Stop() {
	close(n.stopCh)
}

// Put sends a put request to this node.
// from identifies the caller; use any value < 0 for a test client.
func (n *Node) Put(from int, key, value string) error {
	n.rec.Append(Event{
		At:     time.Now(),
		Kind:   KindInvoke,
		NodeID: from,
		Op:     "put",
		Key:    key,
		Value:  value,
	})
	msg := Message{From: from, To: n.ID, Op: "put", Key: key, Value: value}
	return n.ft.Send(msg, n.inbox)
}

// Get sends a get request to this node.
func (n *Node) Get(from int, key string) error {
	n.rec.Append(Event{
		At:     time.Now(),
		Kind:   KindInvoke,
		NodeID: from,
		Op:     "get",
		Key:    key,
	})
	msg := Message{From: from, To: n.ID, Op: "get", Key: key}
	return n.ft.Send(msg, n.inbox)
}

// ReadStore reads the node's current value for key directly (test helper).
func (n *Node) ReadStore(key string) string {
	n.storeMu.RLock()
	v := n.store[key]
	n.storeMu.RUnlock()
	return v
}
```

### Exercise 3: Invariant Checker and Report

Append to `chaos.go`:

```go
// Violation describes a detected invariant breach.
type Violation struct {
	Key      string
	Expected string
	Got      string
	At       time.Time
	NodeID   int
}

// Error implements the error interface.
func (v Violation) Error() string {
	return fmt.Sprintf("node %d at %s: get(%q) = %q, want %q",
		v.NodeID, v.At.Format(time.RFC3339Nano), v.Key, v.Got, v.Expected)
}

// CheckDurability scans the event log for acknowledged writes that are
// not reflected in a subsequent read. It returns one Violation per lost write.
//
// Algorithm: when a KindResponse "put" event is seen, record lastWrite[key].
// When a KindResponse "get" event is seen and the value differs from
// lastWrite[key], that is a durability violation.
func CheckDurability(events []Event) []Violation {
	lastWrite := make(map[string]string)
	var violations []Violation
	for _, e := range events {
		if e.Kind != KindResponse {
			continue
		}
		switch e.Op {
		case "put":
			lastWrite[e.Key] = e.Value
		case "get":
			want, seen := lastWrite[e.Key]
			if seen && e.Value != want {
				violations = append(violations, Violation{
					Key:      e.Key,
					Expected: want,
					Got:      e.Value,
					At:       e.At,
					NodeID:   e.NodeID,
				})
			}
		}
	}
	return violations
}

// Report summarises a completed chaos test run.
type Report struct {
	Seed       int64
	Duration   time.Duration
	Events     int
	Faults     int
	Violations []Violation
}

// OK reports whether the run produced no invariant violations.
func (r *Report) OK() bool {
	return len(r.Violations) == 0
}

// BuildReport assembles a Report from a completed run.
func BuildReport(seed int64, dur time.Duration, events []Event) Report {
	faults := 0
	for _, e := range events {
		if e.Kind == KindFault {
			faults++
		}
	}
	return Report{
		Seed:       seed,
		Duration:   dur,
		Events:     len(events),
		Faults:     faults,
		Violations: CheckDurability(events),
	}
}
```

### Exercise 4: Test the Framework

Create `chaos_test.go`:

```go
package chaos

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// buildCluster creates n nodes connected through a FaultyTransport.
func buildCluster(n int, seed int64, dropPct float64) ([]*Node, *FaultConfig, *Recorder) {
	fc := NewFaultConfig(n)
	rec := &Recorder{}
	ft := NewFaultyTransport(fc, seed, dropPct)
	nodes := make([]*Node, n)
	for i := 0; i < n; i++ {
		nodes[i] = NewNode(i, fc, ft, rec)
		nodes[i].Start()
	}
	return nodes, fc, rec
}

func stopAll(nodes []*Node) {
	for _, n := range nodes {
		n.Stop()
	}
}

func TestPutIsVisibleWithoutFaults(t *testing.T) {
	t.Parallel()

	nodes, _, rec := buildCluster(3, 42, 0.0)
	defer stopAll(nodes)

	if err := nodes[0].Put(-1, "x", "hello"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	time.Sleep(20 * time.Millisecond)

	if got := nodes[0].ReadStore("x"); got != "hello" {
		t.Fatalf("ReadStore(x) = %q, want %q", got, "hello")
	}
	if rec.Len() == 0 {
		t.Fatal("recorder has no events")
	}
}

func TestPartitionDropsNodeToNodeMessages(t *testing.T) {
	t.Parallel()

	nodes, fc, _ := buildCluster(2, 1, 0.0)
	defer stopAll(nodes)

	fc.Partition(0, 1)
	msg := Message{From: 0, To: 1, Op: "put", Key: "k", Value: "v"}
	err := nodes[1].ft.Send(msg, nodes[1].inbox)
	if !errors.Is(err, ErrDropped) {
		t.Fatalf("expected ErrDropped, got %v", err)
	}
}

func TestHealRestoresDelivery(t *testing.T) {
	t.Parallel()

	nodes, fc, _ := buildCluster(2, 2, 0.0)
	defer stopAll(nodes)

	fc.Partition(0, 1)
	msg := Message{From: 0, To: 1, Op: "put", Key: "k", Value: "v"}
	if err := nodes[1].ft.Send(msg, nodes[1].inbox); !errors.Is(err, ErrDropped) {
		t.Fatalf("expected ErrDropped before heal, got %v", err)
	}

	fc.Heal(0, 1)
	if err := nodes[1].ft.Send(msg, nodes[1].inbox); err != nil {
		t.Fatalf("send after heal: %v", err)
	}
}

func TestCrashNodeDropsProcessing(t *testing.T) {
	t.Parallel()

	nodes, fc, _ := buildCluster(2, 3, 0.0)
	defer stopAll(nodes)

	fc.Crash(1)
	// The message is delivered to the inbox but the crashed node silently drops it.
	if err := nodes[1].Put(-1, "k", "v"); err != nil {
		t.Fatalf("Put to crashed node inbox: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	if got := nodes[1].ReadStore("k"); got != "" {
		t.Fatalf("crashed node stored k=%q, want empty", got)
	}

	fc.Restart(1)
	if err := nodes[1].Put(-1, "k", "after"); err != nil {
		t.Fatalf("Put after restart: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	if got := nodes[1].ReadStore("k"); got != "after" {
		t.Fatalf("ReadStore(k) = %q, want %q", got, "after")
	}
}

func TestDelaySlowsDelivery(t *testing.T) {
	t.Parallel()

	nodes, fc, _ := buildCluster(2, 4, 0.0)
	defer stopAll(nodes)

	const delay = 30 * time.Millisecond
	fc.SetDelay(0, 1, delay)

	start := time.Now()
	msg := Message{From: 0, To: 1, Op: "put", Key: "slow", Value: "value"}
	if err := nodes[1].ft.Send(msg, nodes[1].inbox); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if elapsed := time.Since(start); elapsed < delay {
		t.Fatalf("elapsed %v < configured delay %v", elapsed, delay)
	}
}

func TestCheckDurabilityPassesOnConsistentLog(t *testing.T) {
	t.Parallel()

	events := []Event{
		{Kind: KindResponse, Op: "put", Key: "a", Value: "1"},
		{Kind: KindResponse, Op: "get", Key: "a", Value: "1"},
	}
	if v := CheckDurability(events); len(v) != 0 {
		t.Fatalf("expected no violations, got %v", v)
	}
}

func TestCheckDurabilityDetectsLostWrite(t *testing.T) {
	t.Parallel()

	events := []Event{
		{Kind: KindResponse, Op: "put", Key: "a", Value: "1"},
		{Kind: KindResponse, Op: "get", Key: "a", Value: "stale"},
	}
	viols := CheckDurability(events)
	if len(viols) != 1 {
		t.Fatalf("expected 1 violation, got %d: %v", len(viols), viols)
	}
	if viols[0].Key != "a" || viols[0].Expected != "1" || viols[0].Got != "stale" {
		t.Fatalf("unexpected violation: %+v", viols[0])
	}
}

func TestBuildReportCountsFaults(t *testing.T) {
	t.Parallel()

	events := []Event{
		{Kind: KindFault, Op: "partition"},
		{Kind: KindFault, Op: "heal"},
		{Kind: KindInvoke, Op: "put"},
		{Kind: KindResponse, Op: "put", Key: "x", Value: "y"},
	}
	r := BuildReport(99, time.Second, events)
	if r.Faults != 2 {
		t.Fatalf("Faults = %d, want 2", r.Faults)
	}
	if r.Events != 4 {
		t.Fatalf("Events = %d, want 4", r.Events)
	}
	if !r.OK() {
		t.Fatalf("expected OK report, got violations: %v", r.Violations)
	}
	if r.Seed != 99 {
		t.Fatalf("Seed = %d, want 99", r.Seed)
	}
}

func TestTableDrivenFaultConfig(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		partition bool
		wantErr   bool
	}{
		{"partition blocks send", true, true},
		{"no fault allows send", false, false},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			nodes, fc, _ := buildCluster(2, 7, 0.0)
			defer stopAll(nodes)

			if tc.partition {
				fc.Partition(0, 1)
			}
			msg := Message{From: 0, To: 1, Op: "put", Key: "k", Value: "v"}
			err := nodes[1].ft.Send(msg, nodes[1].inbox)
			if tc.wantErr && !errors.Is(err, ErrDropped) {
				t.Fatalf("want ErrDropped, got %v", err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func ExampleCheckDurability() {
	events := []Event{
		{Kind: KindResponse, Op: "put", Key: "x", Value: "42"},
		{Kind: KindResponse, Op: "get", Key: "x", Value: "42"},
	}
	v := CheckDurability(events)
	if len(v) == 0 {
		fmt.Println("no violations")
	}
	// Output: no violations
}
```

Your turn: add `TestDropPctOneHundredPercent` — build a cluster with `dropPct=1.0`, send ten `Message{From: 0, To: 1, ...}` messages via `nodes[1].ft.Send`, and assert that every call returns an error wrapping `ErrDropped`. This pins the 100% drop guarantee.

### Exercise 5: Demo Program

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/chaos"
)

func main() {
	const seed = int64(2025)
	fc := chaos.NewFaultConfig(3)
	rec := &chaos.Recorder{}
	ft := chaos.NewFaultyTransport(fc, seed, 0.0)

	nodes := make([]*chaos.Node, 3)
	for i := 0; i < 3; i++ {
		nodes[i] = chaos.NewNode(i, fc, ft, rec)
		nodes[i].Start()
	}
	defer func() {
		for _, n := range nodes {
			n.Stop()
		}
	}()

	start := time.Now()

	// Normal write from test client (from=-1 bypasses the partition matrix).
	if err := nodes[0].Put(-1, "leader", "node0"); err != nil {
		fmt.Printf("put error: %v\n", err)
	}
	time.Sleep(20 * time.Millisecond)
	fmt.Printf("node0 store[leader] = %q\n", nodes[0].ReadStore("leader"))

	// Partition node 0 -> node 1, then send from node 0 (blocked).
	fc.Partition(0, 1)
	rec.Append(chaos.Event{
		At:     time.Now(),
		Kind:   chaos.KindFault,
		Op:     "partition",
		Detail: "0->1 severed",
	})
	msg := chaos.Message{From: 0, To: 1, Op: "put", Key: "key", Value: "val"}
	if err := nodes[1].Transport().Send(msg, nodes[1].Inbox()); err != nil {
		fmt.Printf("partitioned send error: %v\n", err)
	}

	// Heal and confirm delivery.
	fc.Heal(0, 1)
	rec.Append(chaos.Event{
		At:     time.Now(),
		Kind:   chaos.KindFault,
		Op:     "heal",
		Detail: "0->1 restored",
	})
	if err := nodes[1].Transport().Send(msg, nodes[1].Inbox()); err != nil {
		fmt.Printf("post-heal send error: %v\n", err)
	}
	time.Sleep(20 * time.Millisecond)

	snap := rec.Snapshot()
	report := chaos.BuildReport(seed, time.Since(start), snap)
	fmt.Printf("events=%d faults=%d ok=%v\n", report.Events, report.Faults, report.OK())
}
```

## Common Mistakes

### Holding a Lock While Calling time.Sleep

Wrong: acquiring `fc.mu` before applying a delay, then calling `time.Sleep` inside the lock.

```go
fc.mu.Lock()
time.Sleep(fc.delays[from][to]) // holds lock during sleep
fc.mu.Unlock()
```

What happens: every goroutine that needs to read the fault config blocks for the full delay. Tests time out unpredictably.

Fix: read the delay value under the lock, then release before sleeping:

```go
d := fc.DelayFor(from, to)
if d > 0 {
	time.Sleep(d)
}
```

### Non-Deterministic RNG Despite a Fixed Seed

Wrong: creating one `rand.New(rand.NewSource(seed))` per goroutine without coordination.

What happens: goroutine scheduling changes which calls happen in which order, so the same seed produces a different fault sequence on every run.

Fix: hold a single `*rand.Rand` in the transport behind a mutex. All random draws go through one serialized path.

### Reading the Store Before the Message Is Processed

Wrong: calling `ReadStore` immediately after `Put` returns.

```go
nodes[0].Put(-1, "k", "v")
got := nodes[0].ReadStore("k") // may race the goroutine
```

What happens: `Put` returns as soon as the message is enqueued in the inbox channel; the node's goroutine may not have processed it yet. The assertion sees a stale empty string.

Fix: add a `time.Sleep` in tests, or design `Put` to wait for an acknowledgement on a per-request response channel before returning.

### Indexing the Connectivity Matrix With a Negative Sender

Wrong: passing `from = -1` (a test client sentinel) to `CanSend` when `connected` is a fixed-size array.

What happens: `connected[-1][to]` is an out-of-bounds index; the runtime panics.

Fix: guard the array access — if `from < 0`, the caller is a test client and is always permitted:

```go
if from < 0 {
	return true
}
```

## Verification

From `~/go-exercises/chaos`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. Then run the demo:

```bash
go run ./cmd/demo
```

Expected output (approximate):

```
node0 store[leader] = "node0"
partitioned send error: message dropped by fault injector: 0->1 partitioned
events=5 faults=2 ok=true
```

Add `TestDropPctOneHundredPercent` from Exercise 4 before marking this lesson complete.

## Summary

- A chaos framework intercepts the communication layer; `FaultyTransport.Send` is the single chokepoint where faults are applied.
- The `FaultConfig` connectivity matrix represents partitions as booleans; flipping a cell heals the partition without restarting any node.
- All random decisions flow through a single seeded `*rand.Rand` behind a mutex; the same seed replays the same fault sequence exactly.
- `CheckDurability` walks the event log to find acknowledged writes not reflected in a subsequent read.
- In-process simulation removes external dependencies and makes the test suite runnable offline with `go test -race`.

## What's Next

Next: [Paxos Consensus](../15-paxos-consensus/15-paxos-consensus.md).

## Resources

- [pkg.go.dev/math/rand](https://pkg.go.dev/math/rand) — `rand.New`, `rand.NewSource`, seeded PRNG API
- [pkg.go.dev/sync](https://pkg.go.dev/sync) — `Mutex`, `RWMutex`, `WaitGroup` signatures
- [Jepsen: A Framework for Distributed Systems Verification](https://jepsen.io/) — the originator of systematic partition testing methodology
- [Porcupine: A Fast Linearizability Checker in Go](https://github.com/anishathalye/porcupine) — extends the durability checker here to full linearizability
- [FoundationDB: Testing Distributed Systems with Deterministic Simulation](https://www.foundationdb.org/files/fdb-paper.pdf) — the definitive reference for in-process simulation-based chaos testing
