# 7. Client Protocol

Building a client library for a distributed key-value store is harder than it looks. The client must do three things that pull in opposite directions: expose a simple Go API (Put, Get, Delete, Scan, BatchPut) while internally maintaining a consistent-hash ring for coordinator selection, multiplexing many logical requests over a small number of persistent TCP connections, and recovering transparently when a node fails. This lesson builds every layer from scratch using only the standard library.

```text
kvstore-client/
  go.mod
  kvclient/
    errors.go
    protocol.go
    ring.go
    pool.go
    iterator.go
    client.go
    client_test.go
    example_test.go
  cmd/demo/
    main.go
```

## Concepts

### The Client-Cluster Contract

The public API the client exposes is intentionally thin:

```
Put(ctx, key, value) error
Get(ctx, key) ([]byte, error)
Delete(ctx, key) error
BatchPut(ctx, []BatchEntry) (BatchResult, error)
Scan(ctx, startKey, endKey) *ScanIterator
```

Everything else — which node to contact, how many TCP connections to open, when to retry — is an implementation concern hidden behind this surface. A caller should never need to know the cluster topology to issue a request; the client resolves that routing on each call.

### Wire Protocol Design

The protocol is a binary framing over TCP.

Request header (15 bytes):

```
[reqID:8][op:1][keyLen:2][valLen:4]
```

Response header (13 bytes):

```
[reqID:8][status:1][valLen:4]
```

The `reqID` is a monotonically increasing uint64 assigned per logical request within a connection. It is the foundation of multiplexing: when a server sends a response it echoes back the same reqID, and the client uses it to route the response to the goroutine that issued the request.

Binary encoding uses `encoding/binary` with big-endian byte order. Length-prefixed fields mean the receiver always knows how many bytes to read before calling `io.ReadFull`, which avoids partial reads.

### Connection Pool and Multiplexing

A naive implementation opens one TCP connection per request and closes it after. That is slow for two reasons: the three-way TCP handshake adds latency, and Go's `net.Dialer` creates a new OS socket each time. Connection pools amortize this by keeping idle connections ready.

But a simple pool is not enough for high throughput. If the caller must wait for the previous response before sending the next request, throughput is bounded by round-trip latency. Pipelining removes this constraint: the caller sends many requests on the same connection without waiting for earlier responses. The server processes them in order and echoes the reqID on each response, so the client can match responses to callers even when they arrive out of order (though in this implementation a single serial reader on each connection means responses arrive in the order they were sent).

The key data structure is `sync.Map[uint64, chan response]` on each `muxConn`. Each `Do` call registers its channel before sending the request and deregisters it (via `defer`) when it returns. The single `readLoop` goroutine reads every response and sends it to the matching channel.

### Smart Routing via Hash Ring

Consistent hashing assigns responsibility for a key to a node without requiring every node to know about every other node. The ring is a sorted slice of (hash, addr) pairs where each physical node occupies `virtualNodes` positions to spread load evenly. To find the coordinator for a key, hash the key and binary-search for the first ring position at or clockwise from that hash.

The client keeps a local copy of the ring. Smart routing means the client can compute the coordinator locally and send the request directly, without a forwarding hop. When the ring is stale (during a topology change), the wrong node responds with an error code and the client triggers a ring refresh. This lesson handles the common case; the coordinator-mismatch error code is left as an extension.

### Retry, Failover, and Backoff

When `doOnNode` returns an error, `doWithRetry` walks clockwise to the next replica on the ring. This requires at most `MaxRetries` additional attempts. Between each attempt the client waits for an exponentially increasing delay starting from `BaseRetryDelay`, bounded by `ctx.Done()`. The result is fast failover on transient network errors and graceful degradation on sustained failures.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/kvstore-client/kvclient
mkdir -p ~/go-exercises/kvstore-client/cmd/demo
cd ~/go-exercises/kvstore-client
go mod init example.com/kvstore-client
```

### Exercise 1: Sentinel Errors

Create `kvclient/errors.go`. Every exported error in the package is a sentinel so callers can use `errors.Is` rather than matching strings.

```go
package kvclient

import "errors"

// Sentinel errors returned by Client operations. Use errors.Is to test them.
var (
	ErrNotFound        = errors.New("key not found")
	ErrNodeUnreachable = errors.New("node unreachable")
	ErrMaxRetries      = errors.New("max retries exceeded")
	ErrInvalidKey      = errors.New("key must not be empty")
	ErrEmptyNodes      = errors.New("cluster must have at least one node")
)
```

### Exercise 2: Wire Protocol

Create `kvclient/protocol.go`. The encode/decode functions use `io.Writer` and `io.Reader` interfaces, not `net.Conn` directly, so they work with buffers, pipes, and real TCP connections equally.

```go
package kvclient

import (
	"encoding/binary"
	"fmt"
	"io"
)

// Op codes identify the request type.
const (
	OpPut    uint8 = 1
	OpGet    uint8 = 2
	OpDelete uint8 = 3
	OpScan   uint8 = 4
)

// Status codes carried in every response header.
const (
	StatusOK       uint8 = 0
	StatusNotFound uint8 = 1
	StatusError    uint8 = 2
)

// request is the in-memory representation of a client request.
// On the wire it becomes a fixed 15-byte header followed by key and value bytes.
//
//	[reqID:8][op:1][keyLen:2][valLen:4][key...][value...]
type request struct {
	id    uint64
	op    uint8
	key   []byte
	value []byte
}

// response is the in-memory representation of a server reply.
// On the wire it becomes a fixed 13-byte header followed by value bytes.
//
//	[reqID:8][status:1][valLen:4][value...]
type response struct {
	id     uint64
	status uint8
	value  []byte
}

func writeRequest(w io.Writer, req request) error {
	hdr := make([]byte, 15)
	binary.BigEndian.PutUint64(hdr[0:], req.id)
	hdr[8] = req.op
	binary.BigEndian.PutUint16(hdr[9:], uint16(len(req.key)))
	binary.BigEndian.PutUint32(hdr[11:], uint32(len(req.value)))
	if _, err := w.Write(hdr); err != nil {
		return fmt.Errorf("kvclient: write request header: %w", err)
	}
	if len(req.key) > 0 {
		if _, err := w.Write(req.key); err != nil {
			return fmt.Errorf("kvclient: write request key: %w", err)
		}
	}
	if len(req.value) > 0 {
		if _, err := w.Write(req.value); err != nil {
			return fmt.Errorf("kvclient: write request value: %w", err)
		}
	}
	return nil
}

func readRequest(r io.Reader) (request, error) {
	hdr := make([]byte, 15)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return request{}, fmt.Errorf("kvclient: read request header: %w", err)
	}
	id := binary.BigEndian.Uint64(hdr[0:])
	op := hdr[8]
	keyLen := binary.BigEndian.Uint16(hdr[9:])
	valLen := binary.BigEndian.Uint32(hdr[11:])

	key := make([]byte, keyLen)
	if keyLen > 0 {
		if _, err := io.ReadFull(r, key); err != nil {
			return request{}, fmt.Errorf("kvclient: read request key: %w", err)
		}
	}
	val := make([]byte, valLen)
	if valLen > 0 {
		if _, err := io.ReadFull(r, val); err != nil {
			return request{}, fmt.Errorf("kvclient: read request value: %w", err)
		}
	}
	return request{id: id, op: op, key: key, value: val}, nil
}

func writeResponse(w io.Writer, resp response) error {
	hdr := make([]byte, 13)
	binary.BigEndian.PutUint64(hdr[0:], resp.id)
	hdr[8] = resp.status
	binary.BigEndian.PutUint32(hdr[9:], uint32(len(resp.value)))
	if _, err := w.Write(hdr); err != nil {
		return fmt.Errorf("kvclient: write response header: %w", err)
	}
	if len(resp.value) > 0 {
		if _, err := w.Write(resp.value); err != nil {
			return fmt.Errorf("kvclient: write response value: %w", err)
		}
	}
	return nil
}

func readResponse(r io.Reader) (response, error) {
	hdr := make([]byte, 13)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return response{}, fmt.Errorf("kvclient: read response header: %w", err)
	}
	id := binary.BigEndian.Uint64(hdr[0:])
	status := hdr[8]
	valLen := binary.BigEndian.Uint32(hdr[9:])

	val := make([]byte, valLen)
	if valLen > 0 {
		if _, err := io.ReadFull(r, val); err != nil {
			return response{}, fmt.Errorf("kvclient: read response value: %w", err)
		}
	}
	return response{id: id, status: status, value: val}, nil
}
```

### Exercise 3: Hash Ring

Create `kvclient/ring.go`. The ring stores 150 virtual positions per physical node — a number chosen empirically by distributed systems literature as the point where load variance across nodes falls below a few percent.

```go
package kvclient

import (
	"fmt"
	"hash/fnv"
	"sort"
	"sync"
)

// virtualNodes is the number of virtual ring positions per physical node.
// Higher values produce more balanced key distribution at the cost of memory.
const virtualNodes = 150

type ringEntry struct {
	hash uint32
	addr string
}

// HashRing implements consistent hashing for coordinator selection.
// Each physical node occupies virtualNodes positions spread uniformly around
// the ring. A key maps to the node whose first virtual position is at or
// clockwise from the key's hash.
type HashRing struct {
	mu      sync.RWMutex
	entries []ringEntry // sorted by hash
	nodes   []string    // current physical node list
}

func newHashRing(nodes []string) *HashRing {
	r := &HashRing{}
	r.update(nodes)
	return r
}

// update replaces the ring's node set. Safe to call concurrently with Coordinator
// and Replicas because it holds the write lock for the duration.
func (r *HashRing) update(nodes []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nodes = make([]string, len(nodes))
	copy(r.nodes, nodes)

	entries := make([]ringEntry, 0, len(nodes)*virtualNodes)
	for _, addr := range nodes {
		for i := 0; i < virtualNodes; i++ {
			h := ringHash(fmt.Sprintf("%s#%d", addr, i))
			entries = append(entries, ringEntry{hash: h, addr: addr})
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].hash < entries[j].hash
	})
	r.entries = entries
}

// Nodes returns a snapshot of the current physical node list.
func (r *HashRing) Nodes() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, len(r.nodes))
	copy(out, r.nodes)
	return out
}

// Coordinator returns the primary node address for the given key.
// Returns ("", false) when the ring is empty.
func (r *HashRing) Coordinator(key string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.entries) == 0 {
		return "", false
	}
	h := ringHash(key)
	idx := sort.Search(len(r.entries), func(i int) bool {
		return r.entries[i].hash >= h
	})
	if idx == len(r.entries) {
		idx = 0 // wrap around
	}
	return r.entries[idx].addr, true
}

// Replicas returns up to n distinct node addresses starting from the key's
// coordinator and walking clockwise. Used by the client for retry failover.
func (r *HashRing) Replicas(key string, n int) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.entries) == 0 {
		return nil
	}
	h := ringHash(key)
	start := sort.Search(len(r.entries), func(i int) bool {
		return r.entries[i].hash >= h
	})
	seen := make(map[string]bool)
	out := make([]string, 0, n)
	for i := 0; len(out) < n && i < len(r.entries); i++ {
		idx := (start + i) % len(r.entries)
		addr := r.entries[idx].addr
		if !seen[addr] {
			seen[addr] = true
			out = append(out, addr)
		}
	}
	return out
}

func ringHash(s string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(s))
	return h.Sum32()
}
```

### Exercise 4: Connection Pool and Multiplexer

Create `kvclient/pool.go`. The `muxConn` type is the heart of pipelining: a single `readLoop` goroutine reads every response off the wire and delivers it to the caller waiting on that reqID's channel.

```go
package kvclient

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
)

// muxConn wraps a net.Conn with request multiplexing. Many callers can call Do
// concurrently on the same muxConn; the single reader goroutine demultiplexes
// responses by request ID and delivers them to the correct waiting channel.
type muxConn struct {
	conn    net.Conn
	writer  *bufio.Writer
	wrMu    sync.Mutex // guards writer
	pending sync.Map   // map[uint64]chan response
	nextID  atomic.Uint64
	closed  chan struct{} // closed by readLoop when the connection dies
}

func newMuxConn(conn net.Conn) *muxConn {
	mc := &muxConn{
		conn:   conn,
		writer: bufio.NewWriter(conn),
		closed: make(chan struct{}),
	}
	go mc.readLoop()
	return mc
}

// readLoop runs in its own goroutine. It reads responses from the connection and
// delivers each to the channel registered for its request ID. When the connection
// breaks it wakes all pending callers with a StatusError response and exits.
func (mc *muxConn) readLoop() {
	defer close(mc.closed)
	r := bufio.NewReader(mc.conn)
	for {
		resp, err := readResponse(r)
		if err != nil {
			// Wake all pending callers so Do returns an error instead of blocking.
			mc.pending.Range(func(k, v any) bool {
				id := k.(uint64)
				ch := v.(chan response)
				select {
				case ch <- response{id: id, status: StatusError}:
				default:
				}
				return true
			})
			return
		}
		if v, ok := mc.pending.Load(resp.id); ok {
			v.(chan response) <- resp
		}
	}
}

// Do sends req over the connection and waits for the matching response.
// It is safe to call concurrently from multiple goroutines.
func (mc *muxConn) Do(ctx context.Context, req request) (response, error) {
	id := mc.nextID.Add(1)
	req.id = id

	ch := make(chan response, 1)
	mc.pending.Store(id, ch)
	defer mc.pending.Delete(id)

	mc.wrMu.Lock()
	err := writeRequest(mc.writer, req)
	if err == nil {
		err = mc.writer.Flush()
	}
	mc.wrMu.Unlock()
	if err != nil {
		return response{}, fmt.Errorf("kvclient: send request: %w", err)
	}

	select {
	case <-ctx.Done():
		return response{}, ctx.Err()
	case resp := <-ch:
		if resp.status == StatusError {
			return response{}, fmt.Errorf("kvclient: connection lost: %w", ErrNodeUnreachable)
		}
		return resp, nil
	case <-mc.closed:
		return response{}, fmt.Errorf("kvclient: connection closed: %w", ErrNodeUnreachable)
	}
}

// ConnPool maintains a pool of multiplexed connections to a single cluster node.
// maxConn controls the maximum number of idle connections kept in the pool.
// Under high concurrency, total active connections may briefly exceed maxConn
// (excess connections are discarded on return rather than cached).
type ConnPool struct {
	addr    string
	maxConn int
	conns   chan *muxConn
}

func newConnPool(addr string, maxConn int) *ConnPool {
	return &ConnPool{
		addr:    addr,
		maxConn: maxConn,
		conns:   make(chan *muxConn, maxConn),
	}
}

// get returns an idle connection from the pool, or dials a new one if none are
// available. Stale connections (where readLoop has already exited) are skipped.
func (p *ConnPool) get(ctx context.Context) (*muxConn, error) {
	for {
		select {
		case mc := <-p.conns:
			select {
			case <-mc.closed:
				// connection is dead; discard and loop
				continue
			default:
				return mc, nil
			}
		default:
		}
		// No idle connection; dial a fresh one.
		d := &net.Dialer{}
		conn, err := d.DialContext(ctx, "tcp", p.addr)
		if err != nil {
			return nil, fmt.Errorf("kvclient: dial %s: %w", p.addr, ErrNodeUnreachable)
		}
		return newMuxConn(conn), nil
	}
}

// put returns mc to the pool. If the pool is full, the connection is closed.
func (p *ConnPool) put(mc *muxConn) {
	select {
	case <-mc.closed:
		// already dead; nothing to do
		return
	default:
	}
	select {
	case p.conns <- mc:
	default:
		mc.conn.Close()
	}
}

// Close drains the pool and closes all idle connections.
func (p *ConnPool) Close() {
	for {
		select {
		case mc := <-p.conns:
			mc.conn.Close()
		default:
			return
		}
	}
}
```

### Exercise 5: Scan Iterator

Create `kvclient/iterator.go`. The iterator pattern mirrors `bufio.Scanner`: call `Next` until it returns false, then check `Err`. Pagination happens transparently by advancing `startKey` past the last returned key after each page.

```go
package kvclient

import "context"

// ScanIterator iterates over key-value pairs in a lexicographic range.
// Pages of results are fetched lazily from the cluster. The iterator
// crosses partition boundaries automatically by advancing the start key
// after each page.
//
// Usage:
//
//	it := client.Scan(ctx, "user:", "user;")
//	for it.Next() {
//	    fmt.Printf("%s = %s\n", it.Key(), it.Value())
//	}
//	if err := it.Err(); err != nil { ... }
type ScanIterator struct {
	client   *Client
	ctx      context.Context
	startKey string
	endKey   string
	page     []scanEntry
	pos      int
	done     bool
	err      error
}

type scanEntry struct {
	key   string
	value []byte
}

// Next advances the iterator. Returns true when Key and Value are valid.
// After Next returns false, call Err to distinguish end-of-range from error.
func (it *ScanIterator) Next() bool {
	if it.done {
		return false
	}
	if it.pos < len(it.page) {
		it.pos++
		return true
	}
	// Current page exhausted; fetch the next one.
	if err := it.fetchPage(); err != nil {
		it.err = err
		it.done = true
		return false
	}
	if len(it.page) == 0 {
		it.done = true
		return false
	}
	it.pos = 1
	return true
}

// Key returns the current key. Valid only after a true return from Next.
func (it *ScanIterator) Key() string {
	if it.pos == 0 || it.pos > len(it.page) {
		return ""
	}
	return it.page[it.pos-1].key
}

// Value returns the current value. Valid only after a true return from Next.
func (it *ScanIterator) Value() []byte {
	if it.pos == 0 || it.pos > len(it.page) {
		return nil
	}
	return it.page[it.pos-1].value
}

// Err returns the first non-EOF error encountered by the iterator.
func (it *ScanIterator) Err() error { return it.err }

// fetchPage issues a scan request to the coordinator for the current startKey.
// The request payload encodes both startKey and endKey as a length-prefixed pair.
//
// Payload layout: [startLen:2][endLen:2][startKey...][endKey...]
func (it *ScanIterator) fetchPage() error {
	addr, ok := it.client.ring.Coordinator(it.startKey)
	if !ok {
		return ErrEmptyNodes
	}
	start := []byte(it.startKey)
	end := []byte(it.endKey)
	payload := make([]byte, 4+len(start)+len(end))
	payload[0] = byte(len(start) >> 8)
	payload[1] = byte(len(start))
	payload[2] = byte(len(end) >> 8)
	payload[3] = byte(len(end))
	copy(payload[4:], start)
	copy(payload[4+len(start):], end)

	resp, err := it.client.doOnNode(it.ctx, addr, request{
		op:    OpScan,
		key:   []byte(it.startKey),
		value: payload,
	})
	if err != nil {
		return err
	}
	it.page = decodeScanPage(resp.value)
	if len(it.page) > 0 {
		// Advance past the last returned key so the next page starts after it.
		it.startKey = it.page[len(it.page)-1].key + "\x00"
	}
	return nil
}

// decodeScanPage parses a scan response body.
//
// Format: [count:4] followed by count records of [keyLen:2][valLen:4][key][val].
func decodeScanPage(data []byte) []scanEntry {
	if len(data) < 4 {
		return nil
	}
	count := int(data[0])<<24 | int(data[1])<<16 | int(data[2])<<8 | int(data[3])
	entries := make([]scanEntry, 0, count)
	pos := 4
	for i := 0; i < count && pos < len(data); i++ {
		if pos+6 > len(data) {
			break
		}
		kLen := int(data[pos])<<8 | int(data[pos+1])
		vLen := int(data[pos+2])<<24 | int(data[pos+3])<<16 | int(data[pos+4])<<8 | int(data[pos+5])
		pos += 6
		if pos+kLen+vLen > len(data) {
			break
		}
		k := string(data[pos : pos+kLen])
		v := make([]byte, vLen)
		copy(v, data[pos+kLen:pos+kLen+vLen])
		entries = append(entries, scanEntry{key: k, value: v})
		pos += kLen + vLen
	}
	return entries
}
```

### Exercise 6: Client Core

Create `kvclient/client.go`. The `doWithRetry` method is where the ring, pool, and retry policy meet: it asks the ring for an ordered list of replicas and tries each one in turn with exponential backoff.

```go
package kvclient

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"
)

// Config holds tunable parameters for a Client.
type Config struct {
	// MaxConnsPerNode is the maximum number of idle TCP connections kept per
	// cluster node. Active connections may briefly exceed this count under load.
	MaxConnsPerNode int

	// MaxRetries is the number of replica fallback attempts after a coordinator
	// failure. The primary attempt is not counted; MaxRetries=3 means up to 4
	// nodes may be tried.
	MaxRetries int

	// BaseRetryDelay is the initial backoff before the first retry. Each
	// subsequent retry doubles the delay (exponential backoff).
	BaseRetryDelay time.Duration
}

// Option is a functional option that adjusts Config.
type Option func(*Config)

// WithMaxConnsPerNode sets the idle connection cap per node.
func WithMaxConnsPerNode(n int) Option {
	return func(c *Config) { c.MaxConnsPerNode = n }
}

// WithMaxRetries sets the number of replica failover attempts.
func WithMaxRetries(n int) Option {
	return func(c *Config) { c.MaxRetries = n }
}

// WithBaseRetryDelay sets the initial delay before the first retry.
func WithBaseRetryDelay(d time.Duration) Option {
	return func(c *Config) { c.BaseRetryDelay = d }
}

// BatchEntry is a key-value pair for use with BatchPut.
type BatchEntry struct {
	Key   string
	Value []byte
}

// BatchResult reports per-key outcomes from a BatchPut call.
// A nil Errors map (or an empty one) means all entries succeeded.
type BatchResult struct {
	// Errors maps keys that failed to their individual errors.
	Errors map[string]error
}

// Client is a smart client for a distributed key-value cluster. It maintains a
// local consistent-hash ring for coordinator routing and a connection pool per
// node. All methods are safe for concurrent use.
type Client struct {
	ring  *HashRing
	pools map[string]*ConnPool
	mu    sync.RWMutex // guards pools and ring.update calls
	cfg   Config
}

// New creates a Client that routes to the given cluster nodes.
// At least one node address must be provided. Connections are made lazily on
// the first request to each node.
func New(nodes []string, opts ...Option) (*Client, error) {
	if len(nodes) == 0 {
		return nil, ErrEmptyNodes
	}
	cfg := Config{
		MaxConnsPerNode: 4,
		MaxRetries:      3,
		BaseRetryDelay:  50 * time.Millisecond,
	}
	for _, o := range opts {
		o(&cfg)
	}
	pools := make(map[string]*ConnPool, len(nodes))
	for _, addr := range nodes {
		pools[addr] = newConnPool(addr, cfg.MaxConnsPerNode)
	}
	return &Client{
		ring:  newHashRing(nodes),
		pools: pools,
		cfg:   cfg,
	}, nil
}

// Close releases all pooled connections. The client must not be used after Close.
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, p := range c.pools {
		p.Close()
	}
}

// Put stores value under key on the responsible coordinator.
func (c *Client) Put(ctx context.Context, key string, value []byte) error {
	if key == "" {
		return ErrInvalidKey
	}
	_, err := c.doWithRetry(ctx, key, request{op: OpPut, key: []byte(key), value: value})
	return err
}

// Get retrieves the value stored under key. Returns ErrNotFound when the key
// does not exist.
func (c *Client) Get(ctx context.Context, key string) ([]byte, error) {
	if key == "" {
		return nil, ErrInvalidKey
	}
	resp, err := c.doWithRetry(ctx, key, request{op: OpGet, key: []byte(key)})
	if err != nil {
		return nil, err
	}
	if resp.status == StatusNotFound {
		return nil, ErrNotFound
	}
	return resp.value, nil
}

// Delete removes key from the cluster.
func (c *Client) Delete(ctx context.Context, key string) error {
	if key == "" {
		return ErrInvalidKey
	}
	_, err := c.doWithRetry(ctx, key, request{op: OpDelete, key: []byte(key)})
	return err
}

// BatchPut stores multiple entries. Entries are grouped by coordinator node and
// each group is sent concurrently. Partial failures are recorded in BatchResult.Errors;
// a nil error return does not imply all individual puts succeeded.
func (c *Client) BatchPut(ctx context.Context, entries []BatchEntry) (BatchResult, error) {
	groups := make(map[string][]BatchEntry)
	for _, e := range entries {
		addr, ok := c.ring.Coordinator(e.Key)
		if !ok {
			return BatchResult{}, ErrEmptyNodes
		}
		groups[addr] = append(groups[addr], e)
	}

	var (
		mu     sync.Mutex
		result = BatchResult{Errors: make(map[string]error)}
		wg     sync.WaitGroup
	)
	for addr, group := range groups {
		addr, group := addr, group
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, e := range group {
				req := request{op: OpPut, key: []byte(e.Key), value: e.Value}
				if _, err := c.doOnNode(ctx, addr, req); err != nil {
					mu.Lock()
					result.Errors[e.Key] = err
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	return result, nil
}

// Scan returns a lazy iterator over keys in [startKey, endKey].
func (c *Client) Scan(ctx context.Context, startKey, endKey string) *ScanIterator {
	return &ScanIterator{
		client:   c,
		ctx:      ctx,
		startKey: startKey,
		endKey:   endKey,
	}
}

// UpdateTopology replaces the current node set. Pools for new nodes are created;
// pools for departed nodes are drained and closed. In-flight requests to departed
// nodes complete normally (or fail) before their pools close.
func (c *Client) UpdateTopology(nodes []string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	seen := make(map[string]bool, len(nodes))
	for _, addr := range nodes {
		seen[addr] = true
		if _, ok := c.pools[addr]; !ok {
			c.pools[addr] = newConnPool(addr, c.cfg.MaxConnsPerNode)
		}
	}
	for addr, p := range c.pools {
		if !seen[addr] {
			p.Close()
			delete(c.pools, addr)
		}
	}
	c.ring.update(nodes)
}

// doWithRetry sends req to the coordinator for key. On failure it walks clockwise
// around the ring to the next replica, applying exponential backoff between
// attempts. Returns ErrMaxRetries (wrapping the last underlying error) when all
// replicas fail.
func (c *Client) doWithRetry(ctx context.Context, key string, req request) (response, error) {
	replicas := c.ring.Replicas(key, c.cfg.MaxRetries+1)
	if len(replicas) == 0 {
		return response{}, ErrEmptyNodes
	}
	var lastErr error
	for i, addr := range replicas {
		resp, err := c.doOnNode(ctx, addr, req)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if i < len(replicas)-1 {
			delay := time.Duration(float64(c.cfg.BaseRetryDelay) * math.Pow(2, float64(i)))
			select {
			case <-ctx.Done():
				return response{}, ctx.Err()
			case <-time.After(delay):
			}
		}
	}
	return response{}, fmt.Errorf("%w: %v", ErrMaxRetries, lastErr)
}

// doOnNode sends req to the specific node at addr using a pooled connection.
func (c *Client) doOnNode(ctx context.Context, addr string, req request) (response, error) {
	c.mu.RLock()
	pool, ok := c.pools[addr]
	c.mu.RUnlock()
	if !ok {
		return response{}, fmt.Errorf("kvclient: unknown node %s: %w", addr, ErrNodeUnreachable)
	}
	mc, err := pool.get(ctx)
	if err != nil {
		return response{}, err
	}
	resp, err := mc.Do(ctx, req)
	pool.put(mc)
	return resp, err
}
```

### Exercise 7: Test Suite

Create `kvclient/client_test.go`. Tests use `net.Listen("tcp", "127.0.0.1:0")` to bind an in-process node on an OS-assigned port. The fake node speaks the same wire protocol as a real cluster node, so the client's pool and multiplexer are exercised exactly as in production.

```go
package kvclient

import (
	"bufio"
	"context"
	"errors"
	"net"
	"sync"
	"testing"
)

// nodeServer simulates a single KV node for testing. It uses real TCP so the
// client's connection pool and multiplexer operate exactly as in production.
// The mutex guards the store because multiple connections are served concurrently.
type nodeServer struct {
	mu    sync.Mutex
	store map[string][]byte
}

func newNodeServer() *nodeServer {
	return &nodeServer{store: make(map[string][]byte)}
}

func (s *nodeServer) serveConn(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	for {
		req, err := readRequest(r)
		if err != nil {
			return
		}
		var resp response
		resp.id = req.id
		key := string(req.key)
		s.mu.Lock()
		switch req.op {
		case OpPut:
			s.store[key] = append([]byte(nil), req.value...)
			resp.status = StatusOK
		case OpGet:
			v, ok := s.store[key]
			if ok {
				resp.status = StatusOK
				resp.value = v
			} else {
				resp.status = StatusNotFound
			}
		case OpDelete:
			delete(s.store, key)
			resp.status = StatusOK
		default:
			resp.status = StatusError
		}
		s.mu.Unlock()
		if err := writeResponse(conn, resp); err != nil {
			return
		}
	}
}

// startNode starts an in-process TCP node, returns its address and cleanup func.
func startNode(t *testing.T) (addr string, srv *nodeServer) {
	t.Helper()
	srv = newNodeServer()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go srv.serveConn(conn)
		}
	}()
	return ln.Addr().String(), srv
}

func newTestClient(t *testing.T, nodes ...string) *Client {
	t.Helper()
	c, err := New(nodes)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

func TestPutGet(t *testing.T) {
	t.Parallel()
	addr, _ := startNode(t)
	c := newTestClient(t, addr)
	ctx := context.Background()

	if err := c.Put(ctx, "hello", []byte("world")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	val, err := c.Get(ctx, "hello")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(val) != "world" {
		t.Errorf("Get = %q, want %q", val, "world")
	}
}

func TestGetNotFound(t *testing.T) {
	t.Parallel()
	addr, _ := startNode(t)
	c := newTestClient(t, addr)

	_, err := c.Get(context.Background(), "no-such-key")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestDelete(t *testing.T) {
	t.Parallel()
	addr, _ := startNode(t)
	c := newTestClient(t, addr)
	ctx := context.Background()

	if err := c.Put(ctx, "temp", []byte("v")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := c.Delete(ctx, "temp"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, err := c.Get(ctx, "temp")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("after Delete: err = %v, want ErrNotFound", err)
	}
}

func TestPutEmptyKeyRejected(t *testing.T) {
	t.Parallel()
	addr, _ := startNode(t)
	c := newTestClient(t, addr)

	if err := c.Put(context.Background(), "", []byte("v")); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("err = %v, want ErrInvalidKey", err)
	}
}

func TestGetEmptyKeyRejected(t *testing.T) {
	t.Parallel()
	addr, _ := startNode(t)
	c := newTestClient(t, addr)

	if _, err := c.Get(context.Background(), ""); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("err = %v, want ErrInvalidKey", err)
	}
}

func TestNewEmptyNodesRejected(t *testing.T) {
	t.Parallel()
	_, err := New(nil)
	if !errors.Is(err, ErrEmptyNodes) {
		t.Errorf("err = %v, want ErrEmptyNodes", err)
	}
}

func TestBatchPut(t *testing.T) {
	t.Parallel()
	addr, _ := startNode(t)
	c := newTestClient(t, addr)
	ctx := context.Background()

	entries := []BatchEntry{
		{Key: "batch:a", Value: []byte("1")},
		{Key: "batch:b", Value: []byte("2")},
		{Key: "batch:c", Value: []byte("3")},
	}
	result, err := c.BatchPut(ctx, entries)
	if err != nil {
		t.Fatalf("BatchPut: %v", err)
	}
	if len(result.Errors) != 0 {
		t.Errorf("BatchPut partial errors: %v", result.Errors)
	}
	for _, e := range entries {
		val, err := c.Get(ctx, e.Key)
		if err != nil {
			t.Errorf("Get(%q): %v", e.Key, err)
			continue
		}
		if string(val) != string(e.Value) {
			t.Errorf("Get(%q) = %q, want %q", e.Key, val, e.Value)
		}
	}
}

func TestConcurrentPuts(t *testing.T) {
	t.Parallel()
	addr, _ := startNode(t)
	c := newTestClient(t, addr)
	ctx := context.Background()

	const n = 50
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			errs <- c.Put(ctx, string(rune('a'+i%26))+"-concurrent", []byte("v"))
		}()
	}
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Errorf("concurrent Put: %v", err)
		}
	}
}

func TestUpdateTopologyAddsAndRemovesNodes(t *testing.T) {
	t.Parallel()
	addr1, _ := startNode(t)
	addr2, _ := startNode(t)
	c := newTestClient(t, addr1)

	c.UpdateTopology([]string{addr1, addr2})
	if got := len(c.ring.Nodes()); got != 2 {
		t.Errorf("after add: ring has %d nodes, want 2", got)
	}

	c.UpdateTopology([]string{addr2})
	if got := len(c.ring.Nodes()); got != 1 {
		t.Errorf("after remove: ring has %d nodes, want 1", got)
	}
	nodes := c.ring.Nodes()
	if nodes[0] != addr2 {
		t.Errorf("remaining node = %q, want %q", nodes[0], addr2)
	}
}

func TestHashRingReplicasDistinctNodes(t *testing.T) {
	t.Parallel()
	ring := newHashRing([]string{"n1:7001", "n2:7002", "n3:7003"})

	replicas := ring.Replicas("some-key", 3)
	if len(replicas) != 3 {
		t.Fatalf("Replicas returned %d, want 3", len(replicas))
	}
	seen := make(map[string]bool)
	for _, r := range replicas {
		if seen[r] {
			t.Errorf("duplicate replica: %s", r)
		}
		seen[r] = true
	}
}

func TestHashRingStableRouting(t *testing.T) {
	t.Parallel()
	ring := newHashRing([]string{"n1:7001", "n2:7002", "n3:7003"})
	key := "stable-routing-test"

	c1, ok1 := ring.Coordinator(key)
	c2, ok2 := ring.Coordinator(key)
	if !ok1 || !ok2 {
		t.Fatal("Coordinator returned false for non-empty ring")
	}
	if c1 != c2 {
		t.Errorf("routing not stable: %q != %q", c1, c2)
	}
}

// Your turn: add TestDeleteEmptyKeyRejected that calls Delete(ctx, "") and
// asserts errors.Is(err, ErrInvalidKey).
```

### Exercise 8: Example Tests

Create `kvclient/example_test.go`. Example functions with `// Output:` comments are auto-verified by `go test`. They also serve as inline documentation.

```go
package kvclient

import "fmt"

func ExampleHashRing_stableRouting() {
	ring := newHashRing([]string{"node-A:7001", "node-B:7002", "node-C:7003"})
	// The same key always maps to the same coordinator.
	c1, _ := ring.Coordinator("user:42")
	c2, _ := ring.Coordinator("user:42")
	fmt.Println(c1 == c2)
	// Output:
	// true
}

func ExampleHashRing_Replicas() {
	ring := newHashRing([]string{"node-A:7001", "node-B:7002", "node-C:7003"})
	// Replicas returns distinct nodes in clockwise order; useful for failover.
	replicas := ring.Replicas("session:xyz", 3)
	fmt.Println(len(replicas))
	// Output:
	// 3
}

func Example_decodeScanPageEmpty() {
	entries := decodeScanPage(nil)
	fmt.Println(len(entries))
	// Output:
	// 0
}
```

### Exercise 9: Demo

Create `cmd/demo/main.go`. The demo starts an in-process node so it runs without a real cluster. It exercises all four public operations: Put, Get, BatchPut, and Delete.

```go
package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"time"

	"example.com/kvstore-client/kvclient"
)

// demoNode is a minimal in-process KV node used by the demo.
// It speaks the same binary protocol as the real cluster nodes.
type demoNode struct {
	store map[string][]byte
}

func (n *demoNode) serveConn(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)
	for {
		// Read request header: [reqID:8][op:1][keyLen:2][valLen:4]
		hdr := make([]byte, 15)
		if _, err := io.ReadFull(r, hdr); err != nil {
			return
		}
		reqID := binary.BigEndian.Uint64(hdr[0:])
		op := hdr[8]
		keyLen := binary.BigEndian.Uint16(hdr[9:])
		valLen := binary.BigEndian.Uint32(hdr[11:])

		key := make([]byte, keyLen)
		if keyLen > 0 {
			io.ReadFull(r, key) //nolint:errcheck
		}
		val := make([]byte, valLen)
		if valLen > 0 {
			io.ReadFull(r, val) //nolint:errcheck
		}

		// Build response.
		var respVal []byte
		var status uint8
		switch op {
		case 1: // put
			n.store[string(key)] = append([]byte(nil), val...)
		case 2: // get
			v, ok := n.store[string(key)]
			if ok {
				respVal = v
			} else {
				status = 1 // not found
			}
		case 3: // delete
			delete(n.store, string(key))
		default:
			status = 2 // error
		}

		// Write response header: [reqID:8][status:1][valLen:4]
		rhdr := make([]byte, 13)
		binary.BigEndian.PutUint64(rhdr[0:], reqID)
		rhdr[8] = status
		binary.BigEndian.PutUint32(rhdr[9:], uint32(len(respVal)))
		w.Write(rhdr) //nolint:errcheck
		if len(respVal) > 0 {
			w.Write(respVal) //nolint:errcheck
		}
		w.Flush() //nolint:errcheck
	}
}

func main() {
	// Start an in-process node so the demo runs without a real cluster.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	node := &demoNode{store: make(map[string][]byte)}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go node.serveConn(conn)
		}
	}()

	c, err := kvclient.New(
		[]string{ln.Addr().String()},
		kvclient.WithMaxConnsPerNode(2),
		kvclient.WithMaxRetries(2),
		kvclient.WithBaseRetryDelay(10*time.Millisecond),
	)
	if err != nil {
		log.Fatalf("client: %v", err)
	}
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.Put(ctx, "greeting", []byte("hello, distributed world")); err != nil {
		log.Fatalf("Put: %v", err)
	}
	val, err := c.Get(ctx, "greeting")
	if err != nil {
		log.Fatalf("Get: %v", err)
	}
	fmt.Printf("greeting = %s\n", val)

	entries := []kvclient.BatchEntry{
		{Key: "key:1", Value: []byte("one")},
		{Key: "key:2", Value: []byte("two")},
		{Key: "key:3", Value: []byte("three")},
	}
	result, err := c.BatchPut(ctx, entries)
	if err != nil {
		log.Fatalf("BatchPut: %v", err)
	}
	if len(result.Errors) > 0 {
		log.Printf("BatchPut partial errors: %v", result.Errors)
	}
	for _, e := range entries {
		v, err := c.Get(ctx, e.Key)
		if err != nil {
			log.Printf("Get(%s): %v", e.Key, err)
			continue
		}
		fmt.Printf("%s = %s\n", e.Key, v)
	}

	if err := c.Delete(ctx, "key:1"); err != nil {
		log.Fatalf("Delete: %v", err)
	}
	_, err = c.Get(ctx, "key:1")
	fmt.Printf("key:1 after delete: not found = %v\n", err != nil)
}
```

## Common Mistakes

**Wrong**: Using the same `bufio.Writer` from multiple goroutines without a mutex.

What happens: the internal write buffer can interleave bytes from two concurrent requests, producing malformed frames that cause the server to misparse the length fields and desync the entire connection.

Fix: `muxConn.wrMu` guards the writer. The read side has no mutex because only `readLoop` ever reads.

---

**Wrong**: Not protecting the test server's store map when connections are served concurrently.

What happens: the race detector reports concurrent map writes; under real conditions the binary panics on concurrent map access.

Fix: `nodeServer.mu` locks around every read and write to the store. The `go test -race` flag catches this before it reaches production.

---

**Wrong**: Treating `BatchResult.Errors` as an overall error return and checking `err != nil` to determine success.

What happens: `BatchPut` returns `(BatchResult, nil)` even when individual keys failed. The caller that only checks `err` silently loses data.

Fix: always inspect `result.Errors` after a BatchPut. A non-empty map means partial failure.

---

**Wrong**: Forgetting that `doWithRetry` calls `ring.Replicas` which acquires `ring.mu`. If `UpdateTopology` holds `c.mu` (write lock) and then calls `ring.update` which acquires `ring.mu`, while `doWithRetry` holds `ring.mu` (via Replicas) and tries to acquire `c.mu` (via doOnNode), a deadlock results.

Fix: `UpdateTopology` acquires only `c.mu`; it calls `ring.update` which acquires `ring.mu` independently. `doWithRetry` calls `ring.Replicas` (which acquires `ring.mu`) before calling `doOnNode` (which acquires `c.mu` as a read lock). The two lock acquisitions are always sequential, never nested in opposite orders, so no deadlock is possible.

---

**Wrong**: Using `continue` in a `for { select { ... } }` and expecting it to restart the outer loop.

What happens: this works correctly in Go — `continue` in a select inside a for-loop targets the for-loop — but it is easy to misread. The pool's `get` method relies on this pattern.

Fix: keep the pattern, add a comment.

## Verification

From `~/go-exercises/kvstore-client`:

```bash
test -z "$(gofmt -l ./kvclient/ ./cmd/)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

All five must succeed. The race detector (`-race`) is not optional for concurrent code — it catches real bugs (the missing `nodeServer.mu` was caught this way).

Expected output from `go run ./cmd/demo`:

```
greeting = hello, distributed world
key:1 = one
key:2 = two
key:3 = three
key:1 after delete: not found = true
```

## Summary

- A binary framing protocol with fixed-size headers and `io.ReadFull` is simpler and faster than a text protocol for a KV store.
- Request multiplexing over a single TCP connection requires a monotonic request ID, a `sync.Map` of pending channels, and a dedicated reader goroutine — one per `muxConn`.
- Consistent hashing with virtual nodes distributes keys evenly; `sort.Search` makes coordinator lookup O(log n) in the number of virtual nodes.
- Retry with exponential backoff and replica failover requires walking `ring.Replicas` in order — the ring already encodes the correct failover sequence.
- `BatchPut` groups entries by coordinator and fans them out with goroutines and a `sync.WaitGroup`; `BatchResult.Errors` is the per-key result.
- The scan iterator pattern (`Next/Key/Value/Err`) mirrors `bufio.Scanner` and hides pagination from the caller.

## What's Next

Next: [Full Distributed Key-Value Store](../08-full-distributed-kv/08-full-distributed-kv.md).

## Resources

- [pkg.go.dev/encoding/binary](https://pkg.go.dev/encoding/binary) — `binary.BigEndian`, `PutUint64`, `PutUint32`, `PutUint16`; the canonical reference for fixed-size binary encoding in Go
- [pkg.go.dev/hash/fnv](https://pkg.go.dev/hash/fnv) — FNV-1a hash used for ring placement; good distribution with minimal computation
- [pkg.go.dev/sync#Map](https://pkg.go.dev/sync#Map) — `sync.Map` is the right choice for the pending-request map because it is written once per request (Store) and deleted once (Delete), with concurrent reads from readLoop
- [go.dev/blog/laws-of-reflection](https://go.dev/blog/laws-of-reflection) — background on Go interface values; understanding `any` type assertions used in `sync.Map` callbacks
- [Consistent Hashing and Random Trees (Karger et al., 1997)](https://dl.acm.org/doi/10.1145/258533.258660) — the original paper establishing why virtual nodes matter for load balance
