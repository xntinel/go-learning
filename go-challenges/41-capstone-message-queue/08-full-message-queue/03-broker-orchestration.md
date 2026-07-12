# Exercise 3: Broker Orchestration

This is the integration step: one `Broker` struct that composes partition logs, a consumer-group coordinator, and lock-free metrics under a single lifecycle. It owns the produce path, the long-polling fetch path, topic management, crash recovery on open, and a Prometheus metrics endpoint started and stopped in the right order.

This module is fully self-contained. It bundles its own partition log, coordinator, and metrics — every type it needs is defined inline in package `broker` — plus its own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
partition.go      PartitionLog: append-only segment, index, recovery
coordinator.go    coordinator: group membership, range assignment, offsets
metrics.go        BrokerMetrics: atomic counters as Prometheus text
broker.go         Broker: topics, Produce, Fetch long-poll, Start/Shutdown
cmd/
  demo/
    main.go       start a broker, produce, consume, commit, print metrics
broker_test.go    produce/fetch, long-poll, multi-partition, recovery, metrics
example_test.go   ExampleBroker with verified // Output
```

- Files: `partition.go`, `coordinator.go`, `metrics.go`, `broker.go`, `cmd/demo/main.go`, `broker_test.go`, `example_test.go`.
- Implement: `Broker` with `CreateTopic`, `Produce`, `Fetch`, `CommitOffset`, `FetchOffset`, `JoinGroup`, `Start`, `Shutdown`, and `Metrics`.
- Test: produce then fetch, long-poll timeout and unblock-on-produce, multiple partitions are isolated, crash recovery across reopen, group assignment and offsets, and metrics over the HTTP endpoint.
- Verify: `go test -race ./...`

### Why the broker owns the lifecycle, and in what order

The broker is an orchestrator, not a base class. It holds concrete pointers to each subsystem and is responsible for one thing the subsystems cannot do for themselves: sequencing their lifecycles so nothing is used after it is closed.

`NewBroker` recovers first. It scans the data directory for topic subdirectories, opens each partition (which recovers its segment), and rebuilds the in-memory topic map before returning — so a broker is fully consistent with disk the moment it exists. `Start` then makes external state reachable by binding the admin HTTP listener and serving `/metrics`. `Shutdown` runs that in reverse and adds the critical drain step: it closes the `stopped` channel, broadcasts every partition's condition so parked fetchers wake and return `ErrBrokerStopped`, shuts the HTTP server, waits for the wait group to drain, and only then closes the partition files. Closing a file also acquires that partition's mutex, so it cannot race a fetcher that still holds the lock — the lock ordering does the synchronization for free.

The produce and fetch paths obey the fixed lock order from the concepts file: read-lock `Broker.mu` to look up the partition, release it, then take `PartitionLog.mu`. `Fetch` adds the long-poll. After taking the partition lock it loops: if the context is done, return its error; if the broker is stopping, return `ErrBrokerStopped`; if messages are available, count them in metrics and return; otherwise `cond.Wait()`. A watcher goroutine broadcasts on context cancellation, and `Produce` broadcasts after every append, so a parked fetcher wakes on whichever happens first. Metrics are pure `sync/atomic`, so the hot path takes no extra lock.

Create `partition.go`:

```go
package broker

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
	"time"
)

const headerSize = 24

// Message is one record in a partition log.
type Message struct {
	Offset    int64
	Timestamp time.Time
	Key       []byte
	Value     []byte
}

type indexEntry struct {
	offset  int64
	filePos int64
}

// PartitionLog is an append-only binary log for a single partition.
type PartitionLog struct {
	mu      sync.Mutex
	cond    *sync.Cond
	file    *os.File
	index   []indexEntry
	nextOff int64
	size    int64
}

// NewPartitionLog opens or creates dir/segment.log and recovers its contents.
func NewPartitionLog(dir string) (*PartitionLog, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("partition: mkdir %s: %w", dir, err)
	}
	f, err := os.OpenFile(dir+"/segment.log", os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("partition: open: %w", err)
	}
	pl := &PartitionLog{file: f}
	pl.cond = sync.NewCond(&pl.mu)
	if err := pl.recover(); err != nil {
		f.Close()
		return nil, err
	}
	return pl, nil
}

// Append encodes (key, value) and appends it. Returns the assigned offset.
func (pl *PartitionLog) Append(key, value []byte) (int64, error) {
	pl.mu.Lock()
	defer pl.mu.Unlock()

	off := pl.nextOff
	kl := len(key)
	vl := len(value)

	buf := make([]byte, headerSize+kl+vl)
	binary.BigEndian.PutUint64(buf[0:8], uint64(off))
	binary.BigEndian.PutUint64(buf[8:16], uint64(time.Now().UnixNano()))
	binary.BigEndian.PutUint32(buf[16:20], uint32(kl))
	binary.BigEndian.PutUint32(buf[20:24], uint32(vl))
	copy(buf[24:24+kl], key)
	copy(buf[24+kl:], value)

	filePos := pl.size
	n, err := pl.file.Write(buf)
	if err != nil {
		return 0, fmt.Errorf("partition: write offset %d: %w", off, err)
	}
	pl.index = append(pl.index, indexEntry{offset: off, filePos: filePos})
	pl.nextOff++
	pl.size += int64(n)
	pl.cond.Broadcast()
	return off, nil
}

// readFromLocked returns up to maxMsgs messages at startOffset. Caller holds mu.
func (pl *PartitionLog) readFromLocked(startOffset int64, maxMsgs int) ([]*Message, error) {
	if len(pl.index) == 0 || startOffset >= pl.nextOff {
		return nil, nil
	}
	i := sort.Search(len(pl.index), func(k int) bool {
		return pl.index[k].offset >= startOffset
	})
	if i >= len(pl.index) {
		return nil, nil
	}
	pos := pl.index[i].filePos
	var msgs []*Message
	for len(msgs) < maxMsgs {
		msg, next, err := pl.decodeAt(pos)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				break
			}
			return nil, fmt.Errorf("partition: decode at %d: %w", pos, err)
		}
		msgs = append(msgs, msg)
		pos = next
	}
	return msgs, nil
}

func (pl *PartitionLog) decodeAt(pos int64) (*Message, int64, error) {
	var hdr [headerSize]byte
	if _, err := pl.file.ReadAt(hdr[:], pos); err != nil {
		return nil, 0, err
	}
	off := int64(binary.BigEndian.Uint64(hdr[0:8]))
	tsNano := int64(binary.BigEndian.Uint64(hdr[8:16]))
	keyLen := int(binary.BigEndian.Uint32(hdr[16:20]))
	valLen := int(binary.BigEndian.Uint32(hdr[20:24]))
	pos += headerSize

	var key []byte
	if keyLen > 0 {
		key = make([]byte, keyLen)
		if _, err := pl.file.ReadAt(key, pos); err != nil {
			return nil, 0, err
		}
		pos += int64(keyLen)
	}
	var val []byte
	if valLen > 0 {
		val = make([]byte, valLen)
		if _, err := pl.file.ReadAt(val, pos); err != nil {
			return nil, 0, err
		}
		pos += int64(valLen)
	}
	return &Message{Offset: off, Timestamp: time.Unix(0, tsNano).UTC(), Key: key, Value: val}, pos, nil
}

func (pl *PartitionLog) recover() error {
	info, err := pl.file.Stat()
	if err != nil {
		return fmt.Errorf("partition: stat: %w", err)
	}
	pl.size = info.Size()
	pl.index = pl.index[:0]
	pl.nextOff = 0

	pos := int64(0)
	for pos < pl.size {
		msg, next, err := pl.decodeAt(pos)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				if terr := pl.file.Truncate(pos); terr != nil {
					return fmt.Errorf("partition: truncate partial tail: %w", terr)
				}
				pl.size = pos
				break
			}
			return fmt.Errorf("partition: recover at %d: %w", pos, err)
		}
		pl.index = append(pl.index, indexEntry{offset: msg.Offset, filePos: pos})
		if msg.Offset >= pl.nextOff {
			pl.nextOff = msg.Offset + 1
		}
		pos = next
	}
	return nil
}

// Close wakes parked fetchers and closes the segment file.
func (pl *PartitionLog) Close() error {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	pl.cond.Broadcast()
	return pl.file.Close()
}
```

Create `coordinator.go`:

```go
package broker

import (
	"fmt"
	"sort"
	"sync"
)

type topicPartition struct {
	Topic     string
	Partition int
}

type groupState struct {
	mu      sync.Mutex
	members map[string][]int
	offsets map[topicPartition]int64
}

type coordinator struct {
	mu     sync.RWMutex
	groups map[string]*groupState
}

func newCoordinator() *coordinator {
	return &coordinator{groups: make(map[string]*groupState)}
}

func (c *coordinator) getOrCreate(id string) *groupState {
	c.mu.Lock()
	defer c.mu.Unlock()
	g, ok := c.groups[id]
	if !ok {
		g = &groupState{members: make(map[string][]int), offsets: make(map[topicPartition]int64)}
		c.groups[id] = g
	}
	return g
}

// join adds consumerID and rebalances the group with a range assignment.
func (c *coordinator) join(groupID, consumerID string, numPartitions int) ([]int, error) {
	if groupID == "" || consumerID == "" {
		return nil, fmt.Errorf("coordinator: join requires non-empty ids")
	}
	g := c.getOrCreate(groupID)
	g.mu.Lock()
	defer g.mu.Unlock()
	g.members[consumerID] = nil

	ids := make([]string, 0, len(g.members))
	for id := range g.members {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	m := len(ids)
	for i, id := range ids {
		count := numPartitions / m
		base := i*count + min(i, numPartitions%m)
		if i < numPartitions%m {
			count++
		}
		parts := make([]int, 0, count)
		for p := base; p < base+count; p++ {
			parts = append(parts, p)
		}
		g.members[id] = parts
	}
	return append([]int(nil), g.members[consumerID]...), nil
}

func (c *coordinator) commitOffset(groupID, topic string, partition int, offset int64) {
	g := c.getOrCreate(groupID)
	g.mu.Lock()
	defer g.mu.Unlock()
	g.offsets[topicPartition{topic, partition}] = offset
}

func (c *coordinator) fetchOffset(groupID, topic string, partition int) int64 {
	c.mu.RLock()
	g, ok := c.groups[groupID]
	c.mu.RUnlock()
	if !ok {
		return -1
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	off, ok := g.offsets[topicPartition{topic, partition}]
	if !ok {
		return -1
	}
	return off
}
```

Create `metrics.go`:

```go
package broker

import (
	"fmt"
	"io"
	"sync/atomic"
)

// BrokerMetrics is a lock-free set of operational counters, safe for concurrent use.
type BrokerMetrics struct {
	messagesProduced atomic.Int64
	messagesConsumed atomic.Int64
	bytesIn          atomic.Int64
	bytesOut         atomic.Int64
}

func (m *BrokerMetrics) incProduced()        { m.messagesProduced.Add(1) }
func (m *BrokerMetrics) addConsumed(n int64) { m.messagesConsumed.Add(n) }
func (m *BrokerMetrics) addBytesIn(n int64)  { m.bytesIn.Add(n) }
func (m *BrokerMetrics) addBytesOut(n int64) { m.bytesOut.Add(n) }

// MessagesProduced returns the total records appended since startup.
func (m *BrokerMetrics) MessagesProduced() int64 { return m.messagesProduced.Load() }

// MessagesConsumed returns the total records delivered to Fetch callers.
func (m *BrokerMetrics) MessagesConsumed() int64 { return m.messagesConsumed.Load() }

// WritePrometheusText writes the counters in the Prometheus text exposition format.
func (m *BrokerMetrics) WritePrometheusText(w io.Writer) {
	type sample struct {
		name, help, typ string
		val             int64
	}
	for _, s := range []sample{
		{"mq_messages_produced_total", "Total messages produced", "counter", m.messagesProduced.Load()},
		{"mq_messages_consumed_total", "Total messages consumed", "counter", m.messagesConsumed.Load()},
		{"mq_bytes_in_total", "Total bytes received from producers", "counter", m.bytesIn.Load()},
		{"mq_bytes_out_total", "Total bytes sent to consumers", "counter", m.bytesOut.Load()},
	} {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n%s %d\n", s.name, s.help, s.name, s.typ, s.name, s.val)
	}
}
```

Create `broker.go`:

```go
package broker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
)

// Sentinel errors for the broker API.
var (
	ErrTopicNotFound    = errors.New("broker: topic not found")
	ErrPartitionInvalid = errors.New("broker: invalid partition")
	ErrBrokerStopped    = errors.New("broker: stopped")
)

// BrokerConfig holds broker configuration.
type BrokerConfig struct {
	DataDir   string
	AdminAddr string // HTTP address for /metrics, e.g. "127.0.0.1:0"
}

// DefaultBrokerConfig returns a config rooted at dataDir with an ephemeral admin port.
func DefaultBrokerConfig(dataDir string) BrokerConfig {
	return BrokerConfig{DataDir: dataDir, AdminAddr: "127.0.0.1:0"}
}

type topic struct {
	mu         sync.RWMutex
	partitions []*PartitionLog
}

// Broker composes partition logs, a coordinator, and metrics under one lifecycle.
type Broker struct {
	cfg      BrokerConfig
	mu       sync.RWMutex
	topics   map[string]*topic
	coord    *coordinator
	metrics  *BrokerMetrics
	adminLn  net.Listener
	admin    *http.Server
	stopped  chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// NewBroker creates a broker, recovering any topics found under cfg.DataDir.
func NewBroker(cfg BrokerConfig) (*Broker, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, fmt.Errorf("broker: mkdir %s: %w", cfg.DataDir, err)
	}
	b := &Broker{
		cfg:     cfg,
		topics:  make(map[string]*topic),
		coord:   newCoordinator(),
		metrics: &BrokerMetrics{},
		stopped: make(chan struct{}),
	}
	if err := b.recoverTopics(); err != nil {
		return nil, err
	}
	return b, nil
}

func (b *Broker) recoverTopics() error {
	entries, err := os.ReadDir(b.cfg.DataDir)
	if err != nil {
		return fmt.Errorf("broker: readdir: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		topicDir := filepath.Join(b.cfg.DataDir, e.Name())
		parts, err := os.ReadDir(topicDir)
		if err != nil {
			return fmt.Errorf("broker: readdir topic %s: %w", e.Name(), err)
		}
		var partitions []*PartitionLog
		for _, pe := range parts {
			if !pe.IsDir() {
				continue
			}
			pl, err := NewPartitionLog(filepath.Join(topicDir, pe.Name()))
			if err != nil {
				return err
			}
			partitions = append(partitions, pl)
		}
		if len(partitions) > 0 {
			b.topics[e.Name()] = &topic{partitions: partitions}
		}
	}
	return nil
}

// Start binds the admin listener and serves /metrics.
func (b *Broker) Start() error {
	ln, err := net.Listen("tcp", b.cfg.AdminAddr)
	if err != nil {
		return fmt.Errorf("broker: listen %s: %w", b.cfg.AdminAddr, err)
	}
	b.adminLn = ln
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		b.metrics.WritePrometheusText(w)
	})
	b.admin = &http.Server{Handler: mux}
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		if err := b.admin.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(os.Stderr, "broker: admin server:", err)
		}
	}()
	return nil
}

// AdminAddr returns the bound admin address, or "" before Start.
func (b *Broker) AdminAddr() string {
	if b.adminLn == nil {
		return ""
	}
	return b.adminLn.Addr().String()
}

// Metrics exposes the broker's counters.
func (b *Broker) Metrics() *BrokerMetrics { return b.metrics }

// Shutdown stops the broker gracefully within ctx, draining in-flight fetches
// before closing partition files.
func (b *Broker) Shutdown(ctx context.Context) error {
	b.stopOnce.Do(func() {
		close(b.stopped)
		b.wakeAllFetchers()
		if b.admin != nil {
			b.admin.Shutdown(ctx) //nolint:errcheck
		}
	})
	done := make(chan struct{})
	go func() {
		b.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		return fmt.Errorf("broker: shutdown: %w", ctx.Err())
	}
	return b.closeTopics()
}

func (b *Broker) wakeAllFetchers() {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, t := range b.topics {
		t.mu.RLock()
		for _, pl := range t.partitions {
			pl.mu.Lock()
			pl.cond.Broadcast()
			pl.mu.Unlock()
		}
		t.mu.RUnlock()
	}
}

func (b *Broker) closeTopics() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	var first error
	for _, t := range b.topics {
		t.mu.Lock()
		for _, pl := range t.partitions {
			if err := pl.Close(); err != nil && first == nil {
				first = err
			}
		}
		t.mu.Unlock()
	}
	return first
}

// CreateTopic creates a topic with numPartitions partitions. Idempotent.
func (b *Broker) CreateTopic(name string, numPartitions int) error {
	if numPartitions < 1 {
		return fmt.Errorf("broker: %w: partitions must be >= 1", ErrPartitionInvalid)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, exists := b.topics[name]; exists {
		return nil
	}
	topicDir := filepath.Join(b.cfg.DataDir, name)
	partitions := make([]*PartitionLog, numPartitions)
	for i := range partitions {
		pl, err := NewPartitionLog(filepath.Join(topicDir, fmt.Sprintf("partition-%d", i)))
		if err != nil {
			return fmt.Errorf("broker: create partition %d: %w", i, err)
		}
		partitions[i] = pl
	}
	b.topics[name] = &topic{partitions: partitions}
	return nil
}

// ListTopics returns the names of all known topics.
func (b *Broker) ListTopics() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	names := make([]string, 0, len(b.topics))
	for n := range b.topics {
		names = append(names, n)
	}
	return names
}

func (b *Broker) getPartition(topicName string, partition int) (*PartitionLog, error) {
	b.mu.RLock()
	t, ok := b.topics[topicName]
	b.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrTopicNotFound, topicName)
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	if partition < 0 || partition >= len(t.partitions) {
		return nil, fmt.Errorf("%w: %s/%d", ErrPartitionInvalid, topicName, partition)
	}
	return t.partitions[partition], nil
}

// Produce appends a message and returns its offset.
func (b *Broker) Produce(topicName string, partition int, key, value []byte) (int64, error) {
	select {
	case <-b.stopped:
		return 0, ErrBrokerStopped
	default:
	}
	pl, err := b.getPartition(topicName, partition)
	if err != nil {
		return 0, err
	}
	off, err := pl.Append(key, value)
	if err != nil {
		return 0, err
	}
	b.metrics.incProduced()
	b.metrics.addBytesIn(int64(len(key) + len(value)))
	return off, nil
}

// Fetch returns up to maxMsgs messages from offset, long-polling until messages
// arrive, ctx is cancelled, or the broker stops.
func (b *Broker) Fetch(ctx context.Context, topicName string, partition int, offset int64, maxMsgs int) ([]*Message, error) {
	pl, err := b.getPartition(topicName, partition)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-ctx.Done()
		pl.mu.Lock()
		pl.cond.Broadcast()
		pl.mu.Unlock()
	}()

	pl.mu.Lock()
	defer pl.mu.Unlock()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		select {
		case <-b.stopped:
			return nil, ErrBrokerStopped
		default:
		}
		msgs, err := pl.readFromLocked(offset, maxMsgs)
		if err != nil {
			return nil, err
		}
		if len(msgs) > 0 {
			b.metrics.addConsumed(int64(len(msgs)))
			b.metrics.addBytesOut(totalBytes(msgs))
			return msgs, nil
		}
		pl.cond.Wait()
	}
}

func totalBytes(msgs []*Message) int64 {
	var n int64
	for _, m := range msgs {
		n += int64(len(m.Key) + len(m.Value))
	}
	return n
}

// JoinGroup registers a consumer in a group and returns its partition assignment.
func (b *Broker) JoinGroup(groupID, consumerID, topicName string) ([]int, error) {
	b.mu.RLock()
	t, ok := b.topics[topicName]
	b.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrTopicNotFound, topicName)
	}
	t.mu.RLock()
	n := len(t.partitions)
	t.mu.RUnlock()
	return b.coord.join(groupID, consumerID, n)
}

// CommitOffset stores a consumer group's committed offset for a partition.
func (b *Broker) CommitOffset(groupID, topicName string, partition int, offset int64) error {
	b.coord.commitOffset(groupID, topicName, partition, offset)
	return nil
}

// FetchOffset returns a group's last committed offset, or -1 if none.
func (b *Broker) FetchOffset(groupID, topicName string, partition int) (int64, error) {
	return b.coord.fetchOffset(groupID, topicName, partition), nil
}
```

`Fetch` is where the integration shows. The look-up takes `Broker.mu` (read) and releases it before taking `PartitionLog.mu`, honoring the one-true lock order. The watcher goroutine broadcasts on context cancellation under the partition lock, so a cancellation cannot be lost between the loop's `ctx.Err()` check and `cond.Wait()`. The `select` on `b.stopped` is what lets `Shutdown` reclaim a parked fetcher: `Shutdown` closes `stopped` and broadcasts every condition, the fetcher wakes, sees `stopped`, and returns, releasing the partition lock so `closeTopics` can close the file.

### The runnable demo

The demo starts a broker, produces ten messages across two partitions, consumes each partition, commits a group offset, and prints metrics. Output is deterministic — no addresses or timings are printed.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"example.com/broker"
)

func main() {
	dir, err := os.MkdirTemp("", "broker-demo-*")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)

	b, err := broker.NewBroker(broker.DefaultBrokerConfig(dir))
	if err != nil {
		log.Fatal(err)
	}
	if err := b.Start(); err != nil {
		log.Fatal(err)
	}

	const topic = "events"
	if err := b.CreateTopic(topic, 2); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("created topic %q with 2 partitions\n", topic)

	for i := 0; i < 10; i++ {
		part := i % 2
		off, err := b.Produce(topic, part, nil, []byte(fmt.Sprintf("event-%d", i)))
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("produced event-%d -> partition=%d offset=%d\n", i, part, off)
	}

	for p := 0; p < 2; p++ {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		msgs, err := b.Fetch(ctx, topic, p, 0, 20)
		cancel()
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("partition %d: %d messages\n", p, len(msgs))
	}

	if err := b.CommitOffset("demo-group", topic, 0, 4); err != nil {
		log.Fatal(err)
	}
	off, _ := b.FetchOffset("demo-group", topic, 0)
	fmt.Printf("demo-group committed offset for %s/0: %d\n", topic, off)

	m := b.Metrics()
	fmt.Printf("metrics: produced=%d consumed=%d\n", m.MessagesProduced(), m.MessagesConsumed())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := b.Shutdown(ctx); err != nil {
		log.Printf("shutdown: %v", err)
	}
	fmt.Println("broker stopped")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
created topic "events" with 2 partitions
produced event-0 -> partition=0 offset=0
produced event-1 -> partition=1 offset=0
produced event-2 -> partition=0 offset=1
produced event-3 -> partition=1 offset=1
produced event-4 -> partition=0 offset=2
produced event-5 -> partition=1 offset=2
produced event-6 -> partition=0 offset=3
produced event-7 -> partition=1 offset=3
produced event-8 -> partition=0 offset=4
produced event-9 -> partition=1 offset=4
partition 0: 5 messages
partition 1: 5 messages
demo-group committed offset for events/0: 4
metrics: produced=10 consumed=10
broker stopped
```

### Tests

The tests exercise every path: produce and fetch by offset, the long-poll timing out and unblocking on produce, partition isolation, crash recovery across a reopen, group assignment and offsets, and the metrics endpoint over real HTTP.

Create `broker_test.go`:

```go
package broker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func newTestBroker(t *testing.T) *Broker {
	t.Helper()
	b, err := NewBroker(DefaultBrokerConfig(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		b.Shutdown(ctx) //nolint:errcheck
	})
	return b
}

func TestProduceFetch(t *testing.T) {
	t.Parallel()
	b := newTestBroker(t)
	if err := b.CreateTopic("events", 1); err != nil {
		t.Fatal(err)
	}
	off, err := b.Produce("events", 0, []byte("k"), []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if off != 0 {
		t.Fatalf("first offset = %d, want 0", off)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	msgs, err := b.Fetch(ctx, "events", 0, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || string(msgs[0].Value) != "hello" || string(msgs[0].Key) != "k" {
		t.Fatalf("got %v, want one (k,hello) message", msgs)
	}
}

func TestProduceMultipleOffsets(t *testing.T) {
	t.Parallel()
	b := newTestBroker(t)
	if err := b.CreateTopic("log", 1); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		off, err := b.Produce("log", 0, nil, []byte(fmt.Sprintf("m%d", i)))
		if err != nil {
			t.Fatal(err)
		}
		if off != int64(i) {
			t.Fatalf("offset %d = %d, want %d", i, off, i)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	msgs, err := b.Fetch(ctx, "log", 0, 2, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 || string(msgs[0].Value) != "m2" {
		t.Fatalf("from offset 2 got %v, want m2,m3,m4", msgs)
	}
}

func TestFetchLongPollTimesOut(t *testing.T) {
	t.Parallel()
	b := newTestBroker(t)
	if err := b.CreateTopic("empty", 1); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	if _, err := b.Fetch(ctx, "empty", 0, 0, 10); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}

func TestFetchLongPollUnblocksOnProduce(t *testing.T) {
	t.Parallel()
	b := newTestBroker(t)
	if err := b.CreateTopic("stream", 1); err != nil {
		t.Fatal(err)
	}
	type result struct {
		msgs []*Message
		err  error
	}
	ch := make(chan result, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() {
		msgs, err := b.Fetch(ctx, "stream", 0, 0, 1)
		ch <- result{msgs, err}
	}()
	time.Sleep(20 * time.Millisecond)
	if _, err := b.Produce("stream", 0, nil, []byte("late")); err != nil {
		t.Fatal(err)
	}
	res := <-ch
	if res.err != nil {
		t.Fatalf("fetch error: %v", res.err)
	}
	if len(res.msgs) != 1 || string(res.msgs[0].Value) != "late" {
		t.Fatalf("got %v, want late", res.msgs)
	}
}

func TestMultiplePartitions(t *testing.T) {
	t.Parallel()
	b := newTestBroker(t)
	if err := b.CreateTopic("multi", 3); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := b.Produce("multi", i, nil, []byte(fmt.Sprintf("part-%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		msgs, err := b.Fetch(ctx, "multi", i, 0, 1)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		want := fmt.Sprintf("part-%d", i)
		if len(msgs) != 1 || string(msgs[0].Value) != want {
			t.Fatalf("partition %d: got %v, want %q", i, msgs, want)
		}
	}
}

func TestProduceUnknownTopic(t *testing.T) {
	t.Parallel()
	b := newTestBroker(t)
	if _, err := b.Produce("missing", 0, nil, []byte("x")); !errors.Is(err, ErrTopicNotFound) {
		t.Fatalf("err = %v, want ErrTopicNotFound", err)
	}
}

func TestProduceInvalidPartition(t *testing.T) {
	t.Parallel()
	b := newTestBroker(t)
	if err := b.CreateTopic("t", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Produce("t", 5, nil, []byte("x")); !errors.Is(err, ErrPartitionInvalid) {
		t.Fatalf("err = %v, want ErrPartitionInvalid", err)
	}
}

func TestCreateTopicIdempotent(t *testing.T) {
	t.Parallel()
	b := newTestBroker(t)
	if err := b.CreateTopic("t", 2); err != nil {
		t.Fatal(err)
	}
	if err := b.CreateTopic("t", 2); err != nil {
		t.Fatalf("second CreateTopic errored: %v", err)
	}
	if _, err := b.Produce("t", 1, nil, []byte("x")); err != nil {
		t.Fatalf("produce to partition 1: %v", err)
	}
}

func TestCrashRecovery(t *testing.T) {
	t.Parallel()
	cfg := DefaultBrokerConfig(t.TempDir())

	b1, err := NewBroker(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := b1.CreateTopic("durable", 1); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err := b1.Produce("durable", 0, nil, []byte(fmt.Sprintf("m%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	ctx1, cancel1 := context.WithTimeout(context.Background(), 5*time.Second)
	if err := b1.Shutdown(ctx1); err != nil {
		t.Fatal(err)
	}
	cancel1()

	b2, err := NewBroker(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		b2.Shutdown(ctx) //nolint:errcheck
	})
	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	msgs, err := b2.Fetch(ctx2, "durable", 0, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 5 {
		t.Fatalf("recovered %d messages, want 5", len(msgs))
	}
	for i, m := range msgs {
		if want := fmt.Sprintf("m%d", i); string(m.Value) != want {
			t.Fatalf("msg %d = %q, want %q", i, m.Value, want)
		}
	}
}

func TestJoinGroup(t *testing.T) {
	t.Parallel()
	b := newTestBroker(t)
	if err := b.CreateTopic("orders", 4); err != nil {
		t.Fatal(err)
	}
	if _, err := b.JoinGroup("g", "consumer-A", "orders"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.JoinGroup("g", "consumer-B", "orders"); err != nil {
		t.Fatal(err)
	}
	aParts, err := b.JoinGroup("g", "consumer-A", "orders")
	if err != nil {
		t.Fatal(err)
	}
	bParts, err := b.JoinGroup("g", "consumer-B", "orders")
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[int]bool)
	for _, p := range aParts {
		seen[p] = true
	}
	for _, p := range bParts {
		if seen[p] {
			t.Fatalf("partition %d assigned to both consumers", p)
		}
		seen[p] = true
	}
	if len(seen) != 4 {
		t.Fatalf("covered %d partitions, want 4", len(seen))
	}
}

func TestOffsetCommitFetch(t *testing.T) {
	t.Parallel()
	b := newTestBroker(t)
	if off, _ := b.FetchOffset("grp", "events", 0); off != -1 {
		t.Fatalf("initial offset = %d, want -1", off)
	}
	if err := b.CommitOffset("grp", "events", 0, 42); err != nil {
		t.Fatal(err)
	}
	if off, _ := b.FetchOffset("grp", "events", 0); off != 42 {
		t.Fatalf("committed offset = %d, want 42", off)
	}
}

func TestMetricsOverHTTP(t *testing.T) {
	t.Parallel()
	b := newTestBroker(t)
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	if err := b.CreateTopic("stats", 1); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := b.Produce("stats", 0, nil, []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	resp, err := http.Get("http://" + b.AdminAddr() + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "mq_messages_produced_total 3") {
		t.Fatalf("metrics body missing produced counter:\n%s", body)
	}
}
```

Create `example_test.go`:

```go
package broker_test

import (
	"context"
	"fmt"
	"os"
	"time"

	"example.com/broker"
)

func ExampleBroker() {
	dir, err := os.MkdirTemp("", "broker-example-*")
	if err != nil {
		fmt.Println("mkdirtemp:", err)
		return
	}
	defer os.RemoveAll(dir)

	b, err := broker.NewBroker(broker.DefaultBrokerConfig(dir))
	if err != nil {
		fmt.Println("new:", err)
		return
	}
	defer b.Shutdown(context.Background()) //nolint:errcheck

	if err := b.CreateTopic("greetings", 1); err != nil {
		fmt.Println("create:", err)
		return
	}
	off, err := b.Produce("greetings", 0, nil, []byte("world"))
	if err != nil {
		fmt.Println("produce:", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	msgs, err := b.Fetch(ctx, "greetings", 0, off, 1)
	if err != nil {
		fmt.Println("fetch:", err)
		return
	}
	fmt.Printf("offset=%d value=%s\n", msgs[0].Offset, msgs[0].Value)
	// Output: offset=0 value=world
}
```

## Review

The broker is correct when lifecycle order and lock order are both respected. The lifecycle test that matters most is recovery: `TestCrashRecovery` produces five messages, shuts down cleanly, opens a fresh broker over the same directory, and asserts all five come back in order — proof that `NewBroker` reconstructs topics from disk before serving. The long-poll pair (`TestFetchLongPollTimesOut`, `TestFetchLongPollUnblocksOnProduce`) proves the `sync.Cond` path both honors a deadline and wakes on a produce. `TestMultiplePartitions` proves partition isolation, and `TestProduceFetch` now carries a real key, exercising the fixed header that the earlier interleaved-length bug would have broken. The lock order — `Broker.mu` then `PartitionLog.mu`, never the reverse — is what keeps `Produce`, `Fetch`, `CreateTopic`, and `Shutdown` deadlock-free under `-race`; the most common way to break it is to call a method that re-enters `Broker.mu` while holding a partition lock. `Shutdown`'s drain sequence (close `stopped`, broadcast, wait the group, then close files) is what prevents the use-after-close panic that a naive shutdown produces by closing a file out from under a parked fetcher.

## Resources

- [`net/http.Server.Shutdown`](https://pkg.go.dev/net/http#Server.Shutdown) — graceful shutdown of the admin endpoint within a context deadline.
- [`sync.WaitGroup`](https://pkg.go.dev/sync#WaitGroup) — the drain primitive that makes "wait for in-flight work before closing" correct.
- [Prometheus text exposition format](https://prometheus.io/docs/instrumenting/exposition_formats/#text-based-format) — the exact format the metrics endpoint emits.
- [Apache Kafka design: the log](https://kafka.apache.org/documentation/#design_filesystem) — the append-only-log-as-broker model this lesson follows.

---

Back to [02-consumer-group-coordinator.md](02-consumer-group-coordinator.md) | Next: [04-end-to-end-replay.md](04-end-to-end-replay.md)
