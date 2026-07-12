# 23. DNS Recursive Resolver

A recursive DNS resolver cannot rely on a library to send and parse packets — it
must encode and decode the RFC 1035 wire format itself, follow NS delegation
chains from root servers down to authoritative servers, and maintain a TTL-aware
cache to avoid redundant queries. The hard parts are name compression (pointer
labels that reference earlier offsets in the same message), the delegation loop
(each authority section yields a new set of servers to query), CNAME indirection
that crosses delegation boundaries, and loop/depth detection to prevent infinite
recursion.

```text
dnsr/
  go.mod
  message.go       DNS types, name encoding/decoding, message encode/decode
  cache.go         TTL-aware cache
  resolver.go      Recursive resolution algorithm
  dnsr_test.go     Table-driven tests + Example functions
  cmd/demo/main.go Runnable demo (exported API only)
```

## Concepts

### The DNS Wire Format (RFC 1035 §4)

Every DNS message is a binary structure: a 12-byte fixed header followed by
question records and three resource-record sections (answer, authority,
additional). All multi-byte integers are big-endian.

The header fields that matter for resolution:

| Bytes | Field   | Notes                                      |
|-------|---------|---------------------------------------------|
| 0-1   | ID      | Correlates query with response               |
| 2-3   | Flags   | QR (bit 15), Opcode, AA, TC, RD, RA, RCODE |
| 4-5   | QDCOUNT | Number of questions                          |
| 6-7   | ANCOUNT | Number of answer RRs                         |
| 8-9   | NSCOUNT | Number of authority RRs                      |
| 10-11 | ARCOUNT | Number of additional RRs                     |

Domain names on the wire are sequences of length-prefixed labels terminated by
a zero byte: `\x07example\x03com\x00`. A label whose two high bits are both 1
(`0xC0`) is a compression pointer: the remaining 14 bits are an absolute byte
offset into the same message where the remainder of the name continues. Failing
to follow pointers transitively, or not detecting pointer loops, causes silent
mis-parses.

### Name Compression and the Decode Contract

`DecodeName(msg []byte, offset int)` must return the decoded name AND the
offset just past the bytes it consumed in the current position (before any
pointer jump). Callers use that end offset to find the next field. Forgetting
to return the pre-jump end offset when a pointer is followed is the most common
parse bug: subsequent fields are decoded at the wrong position.

### Recursive Resolution Algorithm

The algorithm is iterative from the inside:

1. Start with the 13 root-server IPs hardcoded in the binary.
2. Send a non-recursive query (RD=1 is set but the root servers will not
   recurse for you — they will return referrals instead).
3. If the response has answers, return them.
4. If the response has authority (NS) records, look for glue A records in the
   additional section. If glue is present, query those IPs next. If glue is
   absent, recursively resolve the NS name itself (a separate resolution call
   that starts again from the roots).
5. Repeat until answers are found or all paths are exhausted.

CNAME records complicate the path: if the answer section contains a CNAME but
the caller asked for TypeA, the resolver must start a new resolution for the
CNAME target. Detecting a CNAME cycle requires tracking the names already in
the current resolution chain.

### TTL-Aware Cache

Every resource record carries a TTL in seconds. The cache must store the
absolute expiry time (`time.Now().Add(ttl * time.Second)`) not the raw TTL,
because the TTL decrements as time passes. On a cache hit, the stored records
are returned directly; no attempt is made to adjust the TTL field on returned
records (a production resolver would decrement it, but that adds complexity
without changing what this lesson teaches).

### Loop and Depth Guards

Without guards, a malformed or adversarial delegation chain produces infinite
recursion. Two guards are needed:

- A `seen` map keyed on `"name/type"` tracks what is currently on the call
  stack. Revisiting the same (name, type) pair before the first resolution
  completes is a cycle.
- A `depth` counter rejects queries that descend past a fixed limit even when
  names are all distinct (a very long valid chain can still exhaust the stack).

## Exercises

### Exercise 1: DNS Message Types, Name Encoding, and Message Decode

Create `message.go`:

```go
package dnsr

import (
	"encoding/binary"
	"fmt"
	"net"
	"strings"
)

// Record types (RFC 1035 §3.2.2, RFC 3596 §2.1).
const (
	TypeA     uint16 = 1
	TypeNS    uint16 = 2
	TypeCNAME uint16 = 5
	TypeSOA   uint16 = 6
	TypeMX    uint16 = 15
	TypeTXT   uint16 = 16
	TypeAAAA  uint16 = 28
)

// ClassIN is the Internet class (RFC 1035 §3.2.4).
const ClassIN uint16 = 1

// Response codes (RFC 1035 §4.1.1).
const (
	RcodeSuccess  uint8 = 0
	RcodeServFail uint8 = 2
	RcodeNXDomain uint8 = 3
)

// Header is the fixed 12-byte DNS message header (RFC 1035 §4.1.1).
type Header struct {
	ID      uint16
	Flags   uint16
	QDCount uint16
	ANCount uint16
	NSCount uint16
	ARCount uint16
}

// IsResponse reports whether the QR bit (bit 15) is set.
func (h Header) IsResponse() bool { return h.Flags&0x8000 != 0 }

// Rcode returns the 4-bit response code from the lower nibble of Flags.
func (h Header) Rcode() uint8 { return uint8(h.Flags & 0x000F) }

// Question is a DNS question record (RFC 1035 §4.1.2).
type Question struct {
	Name  string
	Type  uint16
	Class uint16
}

// RR is a DNS resource record (RFC 1035 §4.1.3).
// Data holds the decoded RDATA: dotted-decimal for A/AAAA, domain for NS/CNAME,
// "pref domain" for MX, or a byte-count string for other types.
type RR struct {
	Name  string
	Type  uint16
	Class uint16
	TTL   uint32
	Data  string
}

// Message is a parsed DNS message (RFC 1035 §4.1).
type Message struct {
	Header     Header
	Questions  []Question
	Answers    []RR
	Authority  []RR
	Additional []RR
}

// EncodeName encodes a domain name as length-prefixed labels terminated by a
// zero byte (RFC 1035 §3.1). A trailing dot is stripped before encoding.
func EncodeName(name string) []byte {
	name = strings.TrimRight(name, ".")
	if name == "" {
		return []byte{0}
	}
	var b []byte
	for _, label := range strings.Split(name, ".") {
		b = append(b, byte(len(label)))
		b = append(b, label...)
	}
	return append(b, 0)
}

// DecodeName reads a DNS name from msg at offset, following compression
// pointers as needed (RFC 1035 §4.1.4). It returns the decoded name and the
// offset of the byte immediately after the name bytes in the current position
// (before any pointer jump). Callers must use the returned offset, not
// offset+len(name), to locate the next field.
func DecodeName(msg []byte, offset int) (string, int, error) {
	var labels []string
	visited := make(map[int]bool)
	end := -1 // first end: offset past the bytes at the call site

	for {
		if offset >= len(msg) {
			return "", 0, fmt.Errorf("dnsr: name decode past end of message at %d", offset)
		}
		if visited[offset] {
			return "", 0, fmt.Errorf("dnsr: pointer loop at offset %d", offset)
		}
		visited[offset] = true

		length := int(msg[offset])

		if length == 0 {
			if end < 0 {
				end = offset + 1
			}
			break
		}

		// Compression pointer: two high bits are both 1.
		if length&0xC0 == 0xC0 {
			if offset+1 >= len(msg) {
				return "", 0, fmt.Errorf("dnsr: pointer extends past message at %d", offset)
			}
			if end < 0 {
				end = offset + 2 // the 2-byte pointer is the last in-place byte
			}
			ptr := int(binary.BigEndian.Uint16(msg[offset:offset+2]) & 0x3FFF)
			offset = ptr
			continue
		}

		offset++
		if offset+length > len(msg) {
			return "", 0, fmt.Errorf("dnsr: label extends past message")
		}
		labels = append(labels, string(msg[offset:offset+length]))
		offset += length
	}

	if end < 0 {
		end = offset + 1
	}
	return strings.Join(labels, "."), end, nil
}

// encodeQuestion encodes a question record to wire bytes.
func encodeQuestion(q Question) []byte {
	b := EncodeName(q.Name)
	b = binary.BigEndian.AppendUint16(b, q.Type)
	b = binary.BigEndian.AppendUint16(b, q.Class)
	return b
}

// EncodeQuery builds a minimal DNS query for name and qtype.
// RD (Recursion Desired) is always set so the message is valid even when sent
// to a recursive resolver; the resolver's own iterative logic does not depend
// on the server recursing.
func EncodeQuery(id uint16, name string, qtype uint16) []byte {
	const rdFlag = uint16(0x0100) // RD bit
	var hdr [12]byte
	binary.BigEndian.PutUint16(hdr[0:], id)
	binary.BigEndian.PutUint16(hdr[2:], rdFlag)
	binary.BigEndian.PutUint16(hdr[4:], 1) // QDCOUNT=1
	// ANCOUNT, NSCOUNT, ARCOUNT remain 0
	return append(hdr[:], encodeQuestion(Question{Name: name, Type: qtype, Class: ClassIN})...)
}

// DecodeMessage parses a DNS message from wire bytes.
func DecodeMessage(msg []byte) (*Message, error) {
	if len(msg) < 12 {
		return nil, fmt.Errorf("dnsr: message too short (%d bytes)", len(msg))
	}
	m := &Message{}
	m.Header.ID = binary.BigEndian.Uint16(msg[0:])
	m.Header.Flags = binary.BigEndian.Uint16(msg[2:])
	m.Header.QDCount = binary.BigEndian.Uint16(msg[4:])
	m.Header.ANCount = binary.BigEndian.Uint16(msg[6:])
	m.Header.NSCount = binary.BigEndian.Uint16(msg[8:])
	m.Header.ARCount = binary.BigEndian.Uint16(msg[10:])

	off := 12
	var err error

	for i := 0; i < int(m.Header.QDCount); i++ {
		var q Question
		q.Name, off, err = DecodeName(msg, off)
		if err != nil {
			return nil, err
		}
		if off+4 > len(msg) {
			return nil, fmt.Errorf("dnsr: question section truncated")
		}
		q.Type = binary.BigEndian.Uint16(msg[off:])
		q.Class = binary.BigEndian.Uint16(msg[off+2:])
		off += 4
		m.Questions = append(m.Questions, q)
	}

	m.Answers, off, err = decodeRRSection(msg, int(m.Header.ANCount), off)
	if err != nil {
		return nil, err
	}
	m.Authority, off, err = decodeRRSection(msg, int(m.Header.NSCount), off)
	if err != nil {
		return nil, err
	}
	m.Additional, _, err = decodeRRSection(msg, int(m.Header.ARCount), off)
	if err != nil {
		return nil, err
	}
	return m, nil
}

// decodeRRSection decodes count resource records from msg starting at off.
func decodeRRSection(msg []byte, count, off int) ([]RR, int, error) {
	rrs := make([]RR, 0, count)
	for i := 0; i < count; i++ {
		var (
			rr  RR
			err error
		)
		rr.Name, off, err = DecodeName(msg, off)
		if err != nil {
			return nil, off, err
		}
		if off+10 > len(msg) {
			return nil, off, fmt.Errorf("dnsr: RR fixed fields truncated at %d", off)
		}
		rr.Type = binary.BigEndian.Uint16(msg[off:])
		rr.Class = binary.BigEndian.Uint16(msg[off+2:])
		rr.TTL = binary.BigEndian.Uint32(msg[off+4:])
		rdlen := int(binary.BigEndian.Uint16(msg[off+8:]))
		off += 10
		if off+rdlen > len(msg) {
			return nil, off, fmt.Errorf("dnsr: RDATA truncated")
		}
		rdStart := off
		off += rdlen
		rr.Data, err = decodeRDATA(msg, rr.Type, msg[rdStart:off], rdStart)
		if err != nil {
			return nil, off, err
		}
		rrs = append(rrs, rr)
	}
	return rrs, off, nil
}

// decodeRDATA decodes RDATA for a given record type.
// msg is the full message (for pointer decompression); rdStart is the offset
// of the first RDATA byte within msg.
func decodeRDATA(msg []byte, typ uint16, rdata []byte, rdStart int) (string, error) {
	switch typ {
	case TypeA:
		if len(rdata) != 4 {
			return "", fmt.Errorf("dnsr: A RDATA must be 4 bytes, got %d", len(rdata))
		}
		return net.IP(rdata).String(), nil
	case TypeAAAA:
		if len(rdata) != 16 {
			return "", fmt.Errorf("dnsr: AAAA RDATA must be 16 bytes, got %d", len(rdata))
		}
		return net.IP(rdata).String(), nil
	case TypeNS, TypeCNAME:
		name, _, err := DecodeName(msg, rdStart)
		return name, err
	case TypeMX:
		if len(rdata) < 2 {
			return "", fmt.Errorf("dnsr: MX RDATA too short")
		}
		pref := binary.BigEndian.Uint16(rdata[:2])
		name, _, err := DecodeName(msg, rdStart+2)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%d %s", pref, name), nil
	default:
		return fmt.Sprintf("(%d bytes)", len(rdata)), nil
	}
}
```

### Exercise 2: TTL-Aware Cache

Create `cache.go`:

```go
package dnsr

import (
	"sync"
	"time"
)

type cacheKey struct {
	name string
	typ  uint16
}

// CacheEntry holds resolved records and their absolute expiry time.
type CacheEntry struct {
	Records []RR
	Expiry  time.Time
}

// Cache is a thread-safe TTL-aware DNS record cache.
type Cache struct {
	mu      sync.RWMutex
	entries map[cacheKey]*CacheEntry
}

// NewCache returns an empty, ready-to-use Cache.
func NewCache() *Cache {
	return &Cache{entries: make(map[cacheKey]*CacheEntry)}
}

// Set stores rrs under (name, typ) with the given TTL in seconds.
// If ttl is 0 the entry is not stored (RFC 1035 §7.4).
func (c *Cache) Set(name string, typ uint16, rrs []RR, ttl uint32) {
	if ttl == 0 {
		return
	}
	key := cacheKey{name: name, typ: typ}
	expiry := time.Now().Add(time.Duration(ttl) * time.Second)
	c.mu.Lock()
	c.entries[key] = &CacheEntry{Records: rrs, Expiry: expiry}
	c.mu.Unlock()
}

// Get returns cached records for (name, typ). Returns nil, false if the entry
// is absent or has expired.
func (c *Cache) Get(name string, typ uint16) ([]RR, bool) {
	key := cacheKey{name: name, typ: typ}
	c.mu.RLock()
	e, ok := c.entries[key]
	c.mu.RUnlock()
	if !ok || time.Now().After(e.Expiry) {
		return nil, false
	}
	return e.Records, true
}

// Size returns the count of live (unexpired) entries.
func (c *Cache) Size() int {
	now := time.Now()
	c.mu.RLock()
	defer c.mu.RUnlock()
	n := 0
	for _, e := range c.entries {
		if now.Before(e.Expiry) {
			n++
		}
	}
	return n
}

// Evict removes all expired entries. Call periodically to reclaim memory.
func (c *Cache) Evict() {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, e := range c.entries {
		if now.After(e.Expiry) {
			delete(c.entries, k)
		}
	}
}
```

### Exercise 3: Recursive Resolver

Create `resolver.go`:

```go
package dnsr

import (
	"errors"
	"fmt"
	"math/rand"
	"net"
	"time"
)

// rootServers is the fixed set of 13 DNS root server IPv4 addresses
// (a through m.root-servers.net), per IANA https://www.iana.org/domains/root/servers.
var rootServers = []string{
	"198.41.0.4",     // a.root-servers.net
	"170.247.170.2",  // b.root-servers.net
	"192.33.4.12",    // c.root-servers.net
	"199.7.91.13",    // d.root-servers.net
	"192.203.230.10", // e.root-servers.net
	"192.5.5.241",    // f.root-servers.net
	"192.112.36.4",   // g.root-servers.net
	"198.97.190.53",  // h.root-servers.net
	"192.36.148.17",  // i.root-servers.net
	"192.58.128.30",  // j.root-servers.net
	"193.0.14.129",   // k.root-servers.net
	"199.7.83.42",    // l.root-servers.net
	"202.12.27.33",   // m.root-servers.net
}

// Sentinel errors for resolution failures.
var (
	ErrNXDomain = errors.New("dnsr: domain does not exist")
	ErrServFail = errors.New("dnsr: server failure")
	ErrLoop     = errors.New("dnsr: resolution loop detected")
	ErrMaxDepth = errors.New("dnsr: maximum resolution depth exceeded")
)

// maxDepth caps the recursion depth to prevent stack exhaustion from deeply
// nested or adversarial delegation chains.
const maxDepth = 20

// Transport sends a single DNS query to a server and returns the parsed
// response. Implementations must be safe for concurrent use.
type Transport interface {
	Query(server, name string, qtype uint16) (*Message, error)
}

// UDPTransport is the default Transport: it sends queries over UDP port 53.
type UDPTransport struct {
	Timeout time.Duration
}

// Query sends name/qtype to server:53 over UDP and returns the first response.
func (t *UDPTransport) Query(server, name string, qtype uint16) (*Message, error) {
	addr := net.JoinHostPort(server, "53")
	conn, err := net.DialTimeout("udp", addr, t.Timeout)
	if err != nil {
		return nil, fmt.Errorf("dnsr: dial %s: %w", server, err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(t.Timeout)); err != nil {
		return nil, err
	}

	id := uint16(rand.Intn(65536)) //nolint:gosec // non-cryptographic use
	if _, err := conn.Write(EncodeQuery(id, name, qtype)); err != nil {
		return nil, fmt.Errorf("dnsr: write query: %w", err)
	}
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		return nil, fmt.Errorf("dnsr: read response from %s: %w", server, err)
	}
	return DecodeMessage(buf[:n])
}

// Resolver performs recursive DNS resolution using a Transport and Cache.
type Resolver struct {
	tr    Transport
	cache *Cache
}

// NewResolver returns a Resolver backed by UDP with a 2-second timeout.
func NewResolver() *Resolver {
	return &Resolver{
		tr:    &UDPTransport{Timeout: 2 * time.Second},
		cache: NewCache(),
	}
}

// NewResolverWith returns a Resolver using a custom Transport and Cache.
// This is the constructor used in tests to inject a mock transport.
func NewResolverWith(tr Transport, c *Cache) *Resolver {
	return &Resolver{tr: tr, cache: c}
}

// Resolve resolves name for the given record type, starting from root servers.
func (r *Resolver) Resolve(name string, qtype uint16) ([]RR, error) {
	return r.resolve(name, qtype, rootServers, 0, make(map[string]bool))
}

func (r *Resolver) resolve(
	name string,
	qtype uint16,
	servers []string,
	depth int,
	seen map[string]bool,
) ([]RR, error) {
	if depth > maxDepth {
		return nil, ErrMaxDepth
	}

	key := fmt.Sprintf("%s/%d", name, qtype)
	if seen[key] {
		return nil, fmt.Errorf("%w: %s", ErrLoop, name)
	}
	seen[key] = true
	defer func() { delete(seen, key) }()

	// Fast path: cache hit.
	if rrs, ok := r.cache.Get(name, qtype); ok {
		return rrs, nil
	}

	for _, server := range servers {
		msg, err := r.tr.Query(server, name, qtype)
		if err != nil {
			continue // unreachable server; try the next one
		}

		switch msg.Header.Rcode() {
		case RcodeNXDomain:
			return nil, fmt.Errorf("%w: %s", ErrNXDomain, name)
		case RcodeServFail:
			return nil, fmt.Errorf("%w from %s", ErrServFail, server)
		}

		// Answer section: we have records.
		if len(msg.Answers) > 0 {
			// Follow CNAME chains when the caller asked for a different type.
			for _, rr := range msg.Answers {
				if rr.Type == TypeCNAME && qtype != TypeCNAME {
					cname, err := r.resolve(rr.Data, qtype, rootServers, depth+1, seen)
					if err != nil {
						return nil, err
					}
					all := append(msg.Answers, cname...)
					r.cache.Set(name, qtype, all, minTTL(all))
					return all, nil
				}
			}
			r.cache.Set(name, qtype, msg.Answers, minTTL(msg.Answers))
			return msg.Answers, nil
		}

		// Authority section: follow NS referral.
		if len(msg.Authority) == 0 {
			continue
		}

		// Build a glue map from the additional section.
		glue := make(map[string]string, len(msg.Additional))
		for _, rr := range msg.Additional {
			if rr.Type == TypeA {
				glue[rr.Name] = rr.Data
			}
		}

		var nextServers []string
		for _, ns := range msg.Authority {
			if ns.Type != TypeNS {
				continue
			}
			if ip, ok := glue[ns.Data]; ok {
				nextServers = append(nextServers, ip)
				continue
			}
			// No glue: resolve the NS name from scratch.
			nsRRs, err := r.resolve(ns.Data, TypeA, rootServers, depth+1, seen)
			if err != nil {
				if errors.Is(err, ErrLoop) || errors.Is(err, ErrMaxDepth) {
					return nil, err
				}
				continue
			}
			for _, rr := range nsRRs {
				if rr.Type == TypeA {
					nextServers = append(nextServers, rr.Data)
				}
			}
		}
		if len(nextServers) == 0 {
			continue
		}
		return r.resolve(name, qtype, nextServers, depth+1, seen)
	}
	return nil, fmt.Errorf("dnsr: no answer for %s", name)
}

// minTTL returns the smallest TTL across rrs, defaulting to 300 when empty.
func minTTL(rrs []RR) uint32 {
	min := uint32(300)
	for _, rr := range rrs {
		if rr.TTL < min {
			min = rr.TTL
		}
	}
	return min
}
```

### Exercise 4: Tests and Example Functions

Create `dnsr_test.go`:

```go
package dnsr

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// --- EncodeName / DecodeName ---

func TestEncodeNameSimple(t *testing.T) {
	t.Parallel()
	b := EncodeName("example.com")
	// 1+7 ("example") + 1+3 ("com") + 1 (root) = 13
	if len(b) != 13 {
		t.Fatalf("len = %d, want 13", len(b))
	}
	if b[0] != 7 {
		t.Fatalf("b[0] = %d, want 7 (length of 'example')", b[0])
	}
	if b[8] != 3 {
		t.Fatalf("b[8] = %d, want 3 (length of 'com')", b[8])
	}
	if b[12] != 0 {
		t.Fatalf("b[12] = %d, want 0 (root label)", b[12])
	}
}

func TestEncodeNameRoot(t *testing.T) {
	t.Parallel()
	b := EncodeName(".")
	if len(b) != 1 || b[0] != 0 {
		t.Fatalf("root name = %v, want [0]", b)
	}
}

func TestEncodeNameTrailingDot(t *testing.T) {
	t.Parallel()
	b1 := EncodeName("example.com")
	b2 := EncodeName("example.com.")
	if len(b1) != len(b2) {
		t.Fatalf("trailing dot changes length: %d vs %d", len(b1), len(b2))
	}
	for i := range b1 {
		if b1[i] != b2[i] {
			t.Fatalf("byte[%d] differs: %d vs %d", i, b1[i], b2[i])
		}
	}
}

func TestDecodeNameSimple(t *testing.T) {
	t.Parallel()
	encoded := EncodeName("example.com")
	// Prepend a 12-byte dummy header so offsets reflect a real message.
	msg := make([]byte, 12+len(encoded))
	copy(msg[12:], encoded)
	name, end, err := DecodeName(msg, 12)
	if err != nil {
		t.Fatal(err)
	}
	if name != "example.com" {
		t.Fatalf("name = %q, want %q", name, "example.com")
	}
	if end != 12+len(encoded) {
		t.Fatalf("end = %d, want %d", end, 12+len(encoded))
	}
}

func TestDecodeNameWithPointer(t *testing.T) {
	t.Parallel()
	// Build a message: "example.com" encoded inline (13 bytes at offset 0),
	// then a 2-byte compression pointer (0xC000) at offset 13 that jumps back
	// to offset 0. Decoding at offset 13 must return "example.com" and end=15.
	inline := EncodeName("example.com")
	msg := append(inline, 0xC0, 0x00)

	name, end, err := DecodeName(msg, 13)
	if err != nil {
		t.Fatal(err)
	}
	if name != "example.com" {
		t.Fatalf("name = %q, want %q", name, "example.com")
	}
	if end != 15 {
		t.Fatalf("end = %d, want 15 (2 pointer bytes consumed at offset 13-14)", end)
	}
}

func TestDecodeNameLoopReturnsError(t *testing.T) {
	t.Parallel()
	// Pointer at offset 0 points back to offset 0.
	msg := []byte{0xC0, 0x00}
	_, _, err := DecodeName(msg, 0)
	if err == nil {
		t.Fatal("expected error for self-referential pointer loop")
	}
}

// --- EncodeQuery / DecodeMessage roundtrip ---

func TestEncodeQueryRoundtrip(t *testing.T) {
	t.Parallel()
	raw := EncodeQuery(0xABCD, "example.com", TypeA)
	msg, err := DecodeMessage(raw)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Header.ID != 0xABCD {
		t.Fatalf("ID = %#x, want 0xabcd", msg.Header.ID)
	}
	if len(msg.Questions) != 1 {
		t.Fatalf("QDCOUNT = %d, want 1", len(msg.Questions))
	}
	q := msg.Questions[0]
	if q.Name != "example.com" {
		t.Fatalf("QNAME = %q, want %q", q.Name, "example.com")
	}
	if q.Type != TypeA {
		t.Fatalf("QTYPE = %d, want TypeA (%d)", q.Type, TypeA)
	}
	if q.Class != ClassIN {
		t.Fatalf("QCLASS = %d, want ClassIN (%d)", q.Class, ClassIN)
	}
}

func TestEncodeQuerySetsRDBit(t *testing.T) {
	t.Parallel()
	raw := EncodeQuery(1, "example.com", TypeA)
	msg, err := DecodeMessage(raw)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Header.Flags&0x0100 == 0 {
		t.Fatalf("Flags = %#x, Recursion Desired bit (0x0100) not set", msg.Header.Flags)
	}
}

func TestDecodeMessageTooShort(t *testing.T) {
	t.Parallel()
	_, err := DecodeMessage([]byte{0, 1, 2})
	if err == nil {
		t.Fatal("expected error for message shorter than 12 bytes")
	}
}

// --- Cache ---

func TestCacheGetMissOnEmpty(t *testing.T) {
	t.Parallel()
	c := NewCache()
	_, ok := c.Get("example.com", TypeA)
	if ok {
		t.Fatal("empty cache should miss")
	}
}

func TestCacheSetGet(t *testing.T) {
	t.Parallel()
	c := NewCache()
	rrs := []RR{{Name: "example.com", Type: TypeA, Class: ClassIN, TTL: 60, Data: "93.184.216.34"}}
	c.Set("example.com", TypeA, rrs, 60)
	got, ok := c.Get("example.com", TypeA)
	if !ok {
		t.Fatal("expected cache hit")
	}
	if len(got) != 1 || got[0].Data != "93.184.216.34" {
		t.Fatalf("got = %v", got)
	}
}

func TestCacheZeroTTLNotStored(t *testing.T) {
	t.Parallel()
	c := NewCache()
	c.Set("example.com", TypeA, []RR{{Data: "1.2.3.4"}}, 0)
	_, ok := c.Get("example.com", TypeA)
	if ok {
		t.Fatal("zero-TTL entry must not be stored (RFC 1035 §7.4)")
	}
}

func TestCacheExpiredEntryMisses(t *testing.T) {
	t.Parallel()
	c := NewCache()
	// Inject an already-expired entry directly via the unexported fields.
	key := cacheKey{name: "stale.com", typ: TypeA}
	c.mu.Lock()
	c.entries[key] = &CacheEntry{
		Records: []RR{{Data: "1.2.3.4"}},
		Expiry:  time.Now().Add(-time.Second),
	}
	c.mu.Unlock()
	_, ok := c.Get("stale.com", TypeA)
	if ok {
		t.Fatal("expired entry should miss")
	}
}

func TestCacheEvictRemovesExpired(t *testing.T) {
	t.Parallel()
	c := NewCache()
	key := cacheKey{name: "stale.com", typ: TypeA}
	c.mu.Lock()
	c.entries[key] = &CacheEntry{
		Records: []RR{{Data: "1.2.3.4"}},
		Expiry:  time.Now().Add(-time.Second),
	}
	c.mu.Unlock()
	c.Evict()
	c.mu.RLock()
	_, still := c.entries[key]
	c.mu.RUnlock()
	if still {
		t.Fatal("Evict should delete expired entry")
	}
}

func TestCacheSize(t *testing.T) {
	t.Parallel()
	c := NewCache()
	c.Set("a.com", TypeA, []RR{{Data: "1.1.1.1"}}, 60)
	c.Set("b.com", TypeA, []RR{{Data: "2.2.2.2"}}, 60)
	if c.Size() != 2 {
		t.Fatalf("Size = %d, want 2", c.Size())
	}
}

// --- Resolver (mock transport, no network) ---

// mockTransport returns pre-canned responses keyed on "name/qtype".
// It ignores the server argument, simulating any server returning the same answer.
type mockTransport struct {
	responses map[string]*Message
}

func (m *mockTransport) Query(server, name string, qtype uint16) (*Message, error) {
	key := fmt.Sprintf("%s/%d", name, qtype)
	resp, ok := m.responses[key]
	if !ok {
		return nil, fmt.Errorf("mock: no response for %s from %s", key, server)
	}
	return resp, nil
}

func TestResolverCacheHit(t *testing.T) {
	t.Parallel()
	c := NewCache()
	rrs := []RR{{Name: "cached.com", Type: TypeA, TTL: 300, Data: "9.9.9.9"}}
	c.Set("cached.com", TypeA, rrs, 300)
	// The mock has no responses; a cache hit must prevent any Query call.
	r := NewResolverWith(&mockTransport{responses: map[string]*Message{}}, c)
	got, err := r.Resolve("cached.com", TypeA)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Data != "9.9.9.9" {
		t.Fatalf("cache hit returned wrong records: %v", got)
	}
}

func TestResolverNXDomain(t *testing.T) {
	t.Parallel()
	mt := &mockTransport{responses: map[string]*Message{
		"notexist.com/1": {
			Header: Header{Flags: uint16(RcodeNXDomain)}, // RCODE=3
		},
	}}
	r := NewResolverWith(mt, NewCache())
	_, err := r.Resolve("notexist.com", TypeA)
	if !errors.Is(err, ErrNXDomain) {
		t.Fatalf("err = %v, want ErrNXDomain", err)
	}
}

func TestResolverDirectAnswer(t *testing.T) {
	t.Parallel()
	rrs := []RR{{Name: "direct.com", Type: TypeA, TTL: 60, Data: "10.0.0.1"}}
	mt := &mockTransport{responses: map[string]*Message{
		"direct.com/1": {
			Header:  Header{ANCount: 1},
			Answers: rrs,
		},
	}}
	r := NewResolverWith(mt, NewCache())
	got, err := r.Resolve("direct.com", TypeA)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Data != "10.0.0.1" {
		t.Fatalf("got = %v", got)
	}
}

func TestResolverLoopDetection(t *testing.T) {
	t.Parallel()
	// ns.loop.com always refers back to itself via an NS record with no glue.
	loopMsg := &Message{
		Header: Header{NSCount: 1},
		Authority: []RR{
			{Name: "ns.loop.com", Type: TypeNS, TTL: 60, Data: "ns.loop.com"},
		},
	}
	mt := &mockTransport{responses: map[string]*Message{
		"loop.com/1":    loopMsg,
		"ns.loop.com/1": loopMsg,
	}}
	r := NewResolverWith(mt, NewCache())
	_, err := r.Resolve("loop.com", TypeA)
	if err == nil {
		t.Fatal("expected error for delegation loop")
	}
	if !errors.Is(err, ErrLoop) && !errors.Is(err, ErrMaxDepth) {
		t.Fatalf("err = %v, want ErrLoop or ErrMaxDepth", err)
	}
}

// Your turn: add TestEncodeQueryIDPreserved that calls EncodeQuery with a
// specific ID (e.g. 0xBEEF), decodes it with DecodeMessage, and asserts that
// msg.Header.ID equals 0xBEEF. This pins the contract that the ID field
// survives an encode/decode roundtrip unchanged.

// --- Example functions (auto-verified by go test) ---

func ExampleEncodeName() {
	b := EncodeName("example.com")
	fmt.Println(len(b))
	// Output: 13
}

func ExampleNewCache() {
	c := NewCache()
	fmt.Println(c.Size())
	// Output: 0
}
```

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"

	"example.com/dnsr"
)

func main() {
	// --- DNS message encoding ---
	name := "example.com"
	encoded := dnsr.EncodeName(name)
	fmt.Printf("EncodeName(%q): %d bytes\n", name, len(encoded))

	query := dnsr.EncodeQuery(0x1234, name, dnsr.TypeA)
	fmt.Printf("EncodeQuery: %d bytes total\n", len(query))

	msg, err := dnsr.DecodeMessage(query)
	if err != nil {
		fmt.Fprintln(os.Stderr, "DecodeMessage:", err)
		os.Exit(1)
	}
	fmt.Printf("Decoded: ID=%#x  QDCOUNT=%d  name=%q  type=%d  RD=%v\n",
		msg.Header.ID,
		len(msg.Questions),
		msg.Questions[0].Name,
		msg.Questions[0].Type,
		msg.Header.Flags&0x0100 != 0,
	)

	// --- TTL-aware cache ---
	c := dnsr.NewCache()
	c.Set("example.com", dnsr.TypeA, []dnsr.RR{{
		Name:  "example.com",
		Type:  dnsr.TypeA,
		Class: dnsr.ClassIN,
		TTL:   300,
		Data:  "93.184.216.34",
	}}, 300)
	fmt.Printf("Cache size after Set: %d\n", c.Size())

	records, ok := c.Get("example.com", dnsr.TypeA)
	if ok {
		fmt.Printf("Cache hit: %s -> %s\n", name, records[0].Data)
	}

	// --- Live resolution (uncomment to test against real DNS) ---
	// r := dnsr.NewResolver()
	// rrs, err := r.Resolve("example.com", dnsr.TypeA)
	// if err != nil {
	// 	fmt.Fprintln(os.Stderr, "Resolve:", err)
	// 	os.Exit(1)
	// }
	// for _, rr := range rrs {
	// 	fmt.Printf("  %s  %d  A  %s\n", rr.Name, rr.TTL, rr.Data)
	// }
}
```

## Common Mistakes

### Wrong end offset when a pointer is followed

Wrong: `DecodeName` returns `pointer_offset + 2` as the end when a pointer is
encountered mid-name (after some plain labels).

What happens: the fields immediately after the name (QTYPE, QCLASS, TTL, etc.)
are read from the wrong byte position. The decode looks successful but produces
garbage values. The test `TestEncodeQueryRoundtrip` would fail with a QTYPE of
0 instead of TypeA.

Fix: record the end offset (`end`) at the first in-place byte that ends the
name sequence. Once set, further pointer jumps do not update it.

### Treating name compression as optional

Wrong: checking only the top bit (`length&0x80 == 0x80`) instead of both high
bits (`length&0xC0 == 0xC0`).

What happens: a label length of `0x40` or `0x80` (RFC 1035 reserves those for
future use) is misread as a pointer. Real server responses may trigger the bug.
The test `TestDecodeNameWithPointer` would still pass (both bits happen to be
set in `0xC0`), but a label of length 65 (`0x41`) would be silently misread.

Fix: use the `0xC0` mask exactly as RFC 1035 §4.1.4 specifies.

### Propagating only fatal NS-resolution errors

Wrong: swallowing all errors from `r.resolve(nsName, TypeA, ...)` with an
unconditional `continue`.

What happens: a loop (ErrLoop) or depth exceeded (ErrMaxDepth) is silently
ignored; the resolver retries through all root servers and eventually returns a
generic "no answer" error instead of the diagnostic error. The test
`TestResolverLoopDetection` would fail with the wrong error type.

Fix: propagate `ErrLoop` and `ErrMaxDepth` immediately; only `continue` for
network errors and NXDOMAIN.

### Forgetting that TTL=0 means "do not cache"

Wrong: caching an entry with `ttl == 0` so it expires immediately but still
occupies a map slot until the next `Evict` call.

What happens: `Get` always misses (entry is expired), but `Size` counts the
stale slot until `Evict` runs. Tests comparing `Size` to expected counts
produce wrong results under a race with `Evict`.

Fix: `Set` returns early when `ttl == 0` and never inserts the entry, as
recommended by RFC 1035 §7.4.

## Verification

From `~/go-exercises/dnsr`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

All five must succeed. The live resolver (commented out in the demo) can be
enabled manually when a network connection and DNS egress are available.

## Summary

- DNS messages have a 12-byte big-endian header; all fields require
  `encoding/binary.BigEndian` reads and writes.
- Domain names on the wire are length-prefixed label sequences; the `0xC0` mask
  identifies a 14-bit compression pointer that jumps to an earlier offset.
- `DecodeName` must return the offset past the in-place bytes before any
  pointer jump; callers depend on this to locate the next field correctly.
- Recursive resolution is a loop: query current servers, if answers arrive
  return them, if authority arrives extract NS IPs (from glue or by a
  sub-resolution) and repeat at greater depth.
- A `seen` map prevents re-entering the same (name, type) pair currently on
  the call stack; a depth counter provides a second guard for long but
  non-cyclic chains.
- TTL-aware caching stores the absolute expiry time, not the raw TTL, so the
  expiry check is a simple time comparison.

## What's Next

Next: [QUIC Transport Protocol](../24-quic-transport-protocol/24-quic-transport-protocol.md).

## Resources

- [RFC 1035 -- Domain Names -- Implementation and Specification](https://datatracker.ietf.org/doc/html/rfc1035) -- §3.1 (names), §4.1 (message format), §7.4 (TTL caching rule)
- [RFC 1034 -- Domain Names -- Concepts and Facilities](https://datatracker.ietf.org/doc/html/rfc1034) -- §4.3.2 recursive algorithm, §5.3 resolvers
- [pkg.go.dev/encoding/binary](https://pkg.go.dev/encoding/binary) -- BigEndian methods used throughout
- [pkg.go.dev/net#IP](https://pkg.go.dev/net#IP) -- `net.IP.String()` for A/AAAA RDATA formatting
- [IANA Root Server List](https://www.iana.org/domains/root/servers) -- authoritative source for the 13 root-server IPs
