# 26. VPN Tunnel Implementation

Building a VPN tunnel in Go requires three distinct skills: manipulating raw IP packets through a TUN device, designing an authenticated-encryption wire protocol, and detecting replay attacks. The hard part is not the encryption — AES-GCM is two function calls — it is getting the protocol right: using the packet header as GCM additional authenticated data (AAD) so sequence numbers are integrity-protected without being encrypted, choosing the correct MTU arithmetic to prevent fragmentation on the underlying path, and implementing a sliding-window replay detector that is both correct and concurrent-safe.

This lesson builds the cryptographic core of the tunnel as a fully testable `package tunnel` using only the standard library. The TUN integration and UDP relay loop are shown as reference code, clearly marked as requiring `github.com/songgao/water` and root privileges.

```text
tunnel/
  go.mod
  header.go
  crypto.go
  replay.go
  tunnel_test.go
  cmd/
    demo/
      main.go
```

## Concepts

### TUN Devices and the IP Layer

A TUN device is a kernel virtual network interface that operates at the IP layer (Layer 3). When the VPN daemon opens `/dev/net/tun` and creates an interface named `tun0`, the kernel routes any IP traffic destined for that interface into user space as raw bytes. The daemon reads complete IP datagrams from the file descriptor, encrypts them, and ships them over a UDP socket to the remote peer. The peer reverses the process and writes the decrypted datagram back into its own TUN device. Applications on either side see a normal network link; the VPN is invisible.

Key invariants:
- Each `Read` from the TUN file descriptor returns exactly one IP datagram (IPv4 or IPv6).
- The `IFF_NO_PI` flag strips the 4-byte protocol-info prefix so `buf[0]` is the first byte of the IP header.
- The TUN device MTU must be reduced to account for encapsulation overhead so the kernel never hands the daemon a plaintext packet that would exceed the underlying path MTU after encryption.

### Wire Protocol: Header as AAD

The wire format for each packet is:

```text
+---8 bytes---+---4 bytes---+---4 bytes---+
| Seq (uint64, big-endian)  | Type (u32)  | BodyLen (u32) |
+-------------+-------------+-------------+
+---12 bytes--+
|  GCM Nonce  |
+-------------+
+---- BodyLen + 16 bytes ----------------->
|  GCM Ciphertext || Tag                  |
+------------------------------------------+
```

The 16-byte header (Seq, Type, BodyLen) is transmitted in plaintext but is passed to `cipher.AEAD.Seal` and `Open` as **additional authenticated data (AAD)**. The GCM tag covers both the ciphertext and the AAD, so any tampering with the sequence number or packet type causes `Open` to return an authentication error. This design — protect the header without encrypting it — is the same approach used in WireGuard and TLS 1.3.

Total per-packet overhead: 16 (header) + 12 (nonce) + 16 (GCM tag) = 44 bytes.

### MTU Arithmetic

Failure to set the TUN MTU correctly causes TCP-over-TCP meltdown: the kernel sends a 1500-byte IP packet to the daemon; after adding the 44-byte tunnel overhead and the 28-byte UDP/IP header, the UDP datagram is 1572 bytes, which exceeds the Ethernet MTU of 1500. The kernel either fragments the datagram (hurting throughput) or drops it if the DF bit is set (breaking the connection silently).

The formula:

```text
tunMTU = pathMTU - udpIPOverhead(28) - tunnelOverhead(44)
       = 1500 - 28 - 44
       = 1428  (for standard Ethernet)
```

Set the TUN device MTU with `ip link set tun0 mtu 1428` before bringing the interface up.

### Replay Attack Detection

AES-GCM authentication does not stop replay attacks. An adversary who records a legitimate encrypted packet can resend it later; `Open` will succeed because the ciphertext and tag are valid. The defense is a sliding-window detector.

A 64-bit integer encodes which sequence numbers in `[maxSeq - 63, maxSeq]` have been received. Bit position k (0 = LSB) represents sequence number `maxSeq - k`.

- `seq > maxSeq`: shift bitmap left by `seq - maxSeq`, set bit 0, update `maxSeq`.
- `seq` within window but bit already set: reject (replay).
- `seq` within window and bit clear: accept, set bit.
- `seq < maxSeq - 63`: reject (too old).

The check runs in O(1) with a single mutex lock.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/tunnel/cmd/demo
cd ~/go-exercises/tunnel
go mod init example.com/tunnel
```

### Exercise 1: The Wire Header

Create `header.go`. The `Header` type represents the 16-byte plaintext prefix of every wire packet, encoded as big-endian integers.

```go
package tunnel

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Wire format: Seq(8) | Type(4) | BodyLen(4) — 16 bytes total.
const (
	HeaderSize = 16

	TypeData      uint32 = 0
	TypeKeepalive uint32 = 1

	// MaxBody is the largest IP datagram body the tunnel accepts.
	MaxBody uint32 = 65535
)

var (
	// ErrShortHeader is returned when a received buffer is too small to hold a Header.
	ErrShortHeader = errors.New("tunnel: buffer too short for header")
	// ErrBodyTooLarge is returned when BodyLen exceeds MaxBody.
	ErrBodyTooLarge = errors.New("tunnel: body length exceeds maximum")
	// ErrBadType is returned when the Type field contains an unknown value.
	ErrBadType = errors.New("tunnel: unknown packet type")
)

// Header is the 16-byte cleartext wire prefix.
// It is used as GCM additional authenticated data (AAD) so that the sequence
// number and type are integrity-protected without being encrypted.
type Header struct {
	Seq     uint64
	Type    uint32
	BodyLen uint32
}

// Marshal writes h into b[0:HeaderSize]. b must be at least HeaderSize bytes.
func (h Header) Marshal(b []byte) {
	binary.BigEndian.PutUint64(b[0:8], h.Seq)
	binary.BigEndian.PutUint32(b[8:12], h.Type)
	binary.BigEndian.PutUint32(b[12:16], h.BodyLen)
}

// UnmarshalHeader decodes the first HeaderSize bytes of b.
// It rejects unknown packet types and BodyLen values exceeding MaxBody.
func UnmarshalHeader(b []byte) (Header, error) {
	if len(b) < HeaderSize {
		return Header{}, fmt.Errorf("%w: got %d, need %d", ErrShortHeader, len(b), HeaderSize)
	}
	h := Header{
		Seq:     binary.BigEndian.Uint64(b[0:8]),
		Type:    binary.BigEndian.Uint32(b[8:12]),
		BodyLen: binary.BigEndian.Uint32(b[12:16]),
	}
	if h.Type != TypeData && h.Type != TypeKeepalive {
		return Header{}, fmt.Errorf("%w: %d", ErrBadType, h.Type)
	}
	if h.BodyLen > MaxBody {
		return Header{}, fmt.Errorf("%w: %d", ErrBodyTooLarge, h.BodyLen)
	}
	return h, nil
}
```

`UnmarshalHeader` validates the type and body length before accepting the header, so the rest of the receive path can trust those fields.

### Exercise 2: Authenticated Encryption

Create `crypto.go`. `Encryptor` wraps `crypto/cipher.AEAD` (AES-256-GCM). `Seal` produces `nonce || ciphertext || tag`; `Open` reverses it. Both operations pass the 16-byte header as AAD.

```go
package tunnel

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
)

const (
	// KeySize is the required AES-256 key length in bytes.
	KeySize = 32
	// NonceSize is the GCM standard nonce length.
	NonceSize = 12
)

var (
	// ErrKeySize is returned when the provided key is not 32 bytes.
	ErrKeySize = errors.New("tunnel: key must be 32 bytes for AES-256")
	// ErrAuth is returned when GCM authentication fails (wrong key or tampered data).
	ErrAuth = errors.New("tunnel: authentication failed")
)

// Encryptor holds an AES-256-GCM AEAD constructed once from the pre-shared key.
// It is safe for concurrent use.
type Encryptor struct {
	aead cipher.AEAD
}

// NewEncryptor returns an Encryptor backed by AES-256-GCM.
// key must be exactly KeySize (32) bytes.
func NewEncryptor(key []byte) (*Encryptor, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("%w: got %d bytes", ErrKeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("tunnel: aes: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("tunnel: gcm: %w", err)
	}
	return &Encryptor{aead: aead}, nil
}

// Seal encrypts plaintext and returns nonce (12 bytes) || ciphertext || tag (16 bytes).
// hdr (the 16-byte wire header) is passed as GCM additional authenticated data (AAD):
// it is authenticated but not encrypted. The caller prepends hdr to the returned bytes
// before writing to the wire.
func (e *Encryptor) Seal(hdr, plaintext []byte) ([]byte, error) {
	nonce := make([]byte, NonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("tunnel: generate nonce: %w", err)
	}
	// aead.Seal appends ciphertext+tag to dst; starting dst with nonce
	// places it at offset 0 so the result is nonce || ciphertext || tag.
	out := e.aead.Seal(nonce, nonce, plaintext, hdr)
	return out, nil
}

// Open decrypts and authenticates payload, which must be nonce || ciphertext || tag
// as returned by Seal. hdr must be the same header bytes used during Seal.
func (e *Encryptor) Open(hdr, payload []byte) ([]byte, error) {
	if len(payload) < NonceSize {
		return nil, fmt.Errorf("%w: payload too short (%d bytes)", ErrAuth, len(payload))
	}
	nonce := payload[:NonceSize]
	ct := payload[NonceSize:]
	plain, err := e.aead.Open(nil, nonce, ct, hdr)
	if err != nil {
		// cipher.AEAD returns a generic error; wrap with ErrAuth so callers
		// can use errors.Is(err, ErrAuth) rather than string matching.
		return nil, fmt.Errorf("%w: %v", ErrAuth, err)
	}
	return plain, nil
}
```

The nonce is freshly generated for each `Seal` call using `crypto/rand`. Deriving the nonce from the sequence number risks nonce reuse if the counter resets or if a session key is reused — a catastrophic failure in GCM that recovers the authentication key.

### Exercise 3: Replay-Window Detector

Create `replay.go`. The sliding window tracks which sequence numbers in `[maxSeq - 63, maxSeq]` have been received using a 64-bit bitmap.

```go
package tunnel

import (
	"errors"
	"fmt"
	"sync"
)

const windowSize = 64 // must be <= 64 (fits in a uint64 bitmap)

// ErrReplay is returned by ReplayWindow.Check for duplicate or out-of-window packets.
var ErrReplay = errors.New("tunnel: replayed or out-of-window packet")

// ReplayWindow is a thread-safe sliding-window duplicate sequence-number detector.
//
// Bitmap semantics: bit k (0 = LSB) represents the packet with sequence number
// maxSeq - k. The window covers [maxSeq - windowSize + 1, maxSeq].
type ReplayWindow struct {
	mu     sync.Mutex
	maxSeq uint64
	bitmap uint64
	seen   bool // false until the first packet is accepted
}

// Check returns nil if seq is new and within the sliding window.
// On success it marks seq as seen and advances the window if seq > maxSeq.
// It returns an error wrapping ErrReplay for duplicates and out-of-window packets.
func (w *ReplayWindow) Check(seq uint64) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if !w.seen {
		w.maxSeq = seq
		w.bitmap = 1 // bit 0: maxSeq is now seen
		w.seen = true
		return nil
	}

	if seq > w.maxSeq {
		delta := seq - w.maxSeq
		if delta >= windowSize {
			// Gap larger than the window; all prior entries are irrelevant.
			w.bitmap = 1
		} else {
			// Shift bitmap left by delta; set bit 0 for the new maxSeq.
			w.bitmap = (w.bitmap << delta) | 1
		}
		w.maxSeq = seq
		return nil
	}

	pos := w.maxSeq - seq
	if pos >= windowSize {
		return fmt.Errorf("%w: seq %d is %d positions behind window max %d",
			ErrReplay, seq, pos, w.maxSeq)
	}
	bit := uint64(1) << pos
	if w.bitmap&bit != 0 {
		return fmt.Errorf("%w: seq %d already received", ErrReplay, seq)
	}
	w.bitmap |= bit
	return nil
}
```

### Exercise 4: Test the Contract

Create `tunnel_test.go`. The tests are the verification — there is no `main` to eyeball.

```go
package tunnel

import (
	"crypto/rand"
	"errors"
	"fmt"
	"testing"
)

// ---- Header tests ----

func TestHeaderMarshalRoundTrip(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		h    Header
	}{
		{"data packet", Header{Seq: 1, Type: TypeData, BodyLen: 1400}},
		{"keepalive", Header{Seq: 42, Type: TypeKeepalive, BodyLen: 0}},
		{"max seq", Header{Seq: ^uint64(0), Type: TypeData, BodyLen: MaxBody}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := make([]byte, HeaderSize)
			tc.h.Marshal(b)
			got, err := UnmarshalHeader(b)
			if err != nil {
				t.Fatalf("UnmarshalHeader: %v", err)
			}
			if got != tc.h {
				t.Fatalf("round-trip mismatch: got %+v, want %+v", got, tc.h)
			}
		})
	}
}

func TestUnmarshalHeaderRejectsShortBuffer(t *testing.T) {
	t.Parallel()

	for _, n := range []int{0, 1, 15} {
		if _, err := UnmarshalHeader(make([]byte, n)); !errors.Is(err, ErrShortHeader) {
			t.Errorf("len=%d: err=%v, want ErrShortHeader", n, err)
		}
	}
}

func TestUnmarshalHeaderRejectsUnknownType(t *testing.T) {
	t.Parallel()

	b := make([]byte, HeaderSize)
	Header{Seq: 1, Type: 99, BodyLen: 0}.Marshal(b)
	if _, err := UnmarshalHeader(b); !errors.Is(err, ErrBadType) {
		t.Fatalf("err=%v, want ErrBadType", err)
	}
}

func TestUnmarshalHeaderRejectsOversizedBody(t *testing.T) {
	t.Parallel()

	b := make([]byte, HeaderSize)
	Header{Seq: 1, Type: TypeData, BodyLen: MaxBody + 1}.Marshal(b)
	if _, err := UnmarshalHeader(b); !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("err=%v, want ErrBodyTooLarge", err)
	}
}

// ---- Encryptor tests ----

func newTestEncryptor(t *testing.T) *Encryptor {
	t.Helper()
	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	enc, err := NewEncryptor(key)
	if err != nil {
		t.Fatal(err)
	}
	return enc
}

func TestEncryptorRoundTrip(t *testing.T) {
	t.Parallel()

	enc := newTestEncryptor(t)
	hdr := make([]byte, HeaderSize)
	Header{Seq: 7, Type: TypeData, BodyLen: 60}.Marshal(hdr)

	plaintext := []byte("hello from the VPN")
	sealed, err := enc.Seal(hdr, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	got, err := enc.Open(hdr, sealed)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(plaintext) {
		t.Fatalf("round-trip: got %q, want %q", got, plaintext)
	}
}

func TestEncryptorDetectsTamperedCiphertext(t *testing.T) {
	t.Parallel()

	enc := newTestEncryptor(t)
	hdr := make([]byte, HeaderSize)
	Header{Seq: 1, Type: TypeData, BodyLen: 5}.Marshal(hdr)

	sealed, _ := enc.Seal(hdr, []byte("hello"))
	sealed[len(sealed)-1] ^= 0xFF // flip the last byte of the GCM tag

	if _, err := enc.Open(hdr, sealed); !errors.Is(err, ErrAuth) {
		t.Fatalf("expected ErrAuth on tampered ciphertext, got: %v", err)
	}
}

func TestEncryptorDetectsTamperedAAD(t *testing.T) {
	t.Parallel()

	enc := newTestEncryptor(t)
	hdr := make([]byte, HeaderSize)
	Header{Seq: 5, Type: TypeData, BodyLen: 5}.Marshal(hdr)

	sealed, _ := enc.Seal(hdr, []byte("hello"))

	// Change the sequence number in the AAD after sealing.
	hdrTampered := make([]byte, HeaderSize)
	Header{Seq: 6, Type: TypeData, BodyLen: 5}.Marshal(hdrTampered)

	if _, err := enc.Open(hdrTampered, sealed); !errors.Is(err, ErrAuth) {
		t.Fatalf("expected ErrAuth on tampered AAD, got: %v", err)
	}
}

func TestEncryptorRejectsWrongKey(t *testing.T) {
	t.Parallel()

	key1 := make([]byte, KeySize)
	key2 := make([]byte, KeySize)
	rand.Read(key1) //nolint:errcheck // crypto/rand.Read never errors since Go 1.20
	rand.Read(key2) //nolint:errcheck
	enc1, _ := NewEncryptor(key1)
	enc2, _ := NewEncryptor(key2)

	hdr := make([]byte, HeaderSize)
	Header{Seq: 1, Type: TypeData, BodyLen: 3}.Marshal(hdr)

	sealed, _ := enc1.Seal(hdr, []byte("abc"))
	if _, err := enc2.Open(hdr, sealed); !errors.Is(err, ErrAuth) {
		t.Fatalf("wrong key: expected ErrAuth, got: %v", err)
	}
}

func TestNewEncryptorRejectsShortKey(t *testing.T) {
	t.Parallel()

	if _, err := NewEncryptor(make([]byte, 16)); !errors.Is(err, ErrKeySize) {
		t.Fatalf("err=%v, want ErrKeySize", err)
	}
}

// ---- ReplayWindow tests ----

func TestReplayWindowAcceptsFirstPacket(t *testing.T) {
	t.Parallel()

	var w ReplayWindow
	if err := w.Check(100); err != nil {
		t.Fatalf("first packet: %v", err)
	}
}

func TestReplayWindowRejectsReplay(t *testing.T) {
	t.Parallel()

	var w ReplayWindow
	if err := w.Check(10); err != nil {
		t.Fatal(err)
	}
	if err := w.Check(10); !errors.Is(err, ErrReplay) {
		t.Fatalf("duplicate seq 10: err=%v, want ErrReplay", err)
	}
}

func TestReplayWindowAcceptsOutOfOrderWithinWindow(t *testing.T) {
	t.Parallel()

	var w ReplayWindow
	w.Check(10) //nolint:errcheck
	w.Check(11) //nolint:errcheck
	w.Check(13) //nolint:errcheck
	// seq=12 arrived late but is within the window.
	if err := w.Check(12); err != nil {
		t.Fatalf("out-of-order within window: %v", err)
	}
}

func TestReplayWindowRejectsTooOld(t *testing.T) {
	t.Parallel()

	var w ReplayWindow
	w.Check(100) //nolint:errcheck
	// Advance the window far past seq=1.
	w.Check(200) //nolint:errcheck
	if err := w.Check(1); !errors.Is(err, ErrReplay) {
		t.Fatalf("seq=1 behind window of 200: err=%v, want ErrReplay", err)
	}
}

func TestReplayWindowLargeGapResetsWindow(t *testing.T) {
	t.Parallel()

	var w ReplayWindow
	w.Check(1) //nolint:errcheck
	// Jump more than windowSize ahead.
	if err := w.Check(200); err != nil {
		t.Fatalf("large jump: %v", err)
	}
	// seq=1 is now far outside the window.
	if err := w.Check(1); !errors.Is(err, ErrReplay) {
		t.Fatalf("seq=1 after large jump: err=%v, want ErrReplay", err)
	}
}

func TestReplayWindowSequentialPackets(t *testing.T) {
	t.Parallel()

	var w ReplayWindow
	for seq := uint64(1); seq <= 64; seq++ {
		if err := w.Check(seq); err != nil {
			t.Fatalf("seq=%d: unexpected error: %v", seq, err)
		}
	}
	// seq=1 is now 63 positions behind maxSeq=64, still within the window (pos=63 < 64).
	// Replaying it must be rejected.
	if err := w.Check(1); !errors.Is(err, ErrReplay) {
		t.Fatalf("replay of seq=1 after 64 packets: err=%v, want ErrReplay", err)
	}
}

// ExampleHeader_Marshal demonstrates the marshal/unmarshal round-trip.
func ExampleHeader_Marshal() {
	h := Header{Seq: 1, Type: TypeData, BodyLen: 1400}
	b := make([]byte, HeaderSize)
	h.Marshal(b)
	h2, _ := UnmarshalHeader(b)
	fmt.Printf("seq=%d type=%d body=%d\n", h2.Seq, h2.Type, h2.BodyLen)
	// Output:
	// seq=1 type=0 body=1400
}
```

Your turn: add `TestEncryptorKeepaliveRoundTrip` — call `enc.Seal(hdr, nil)` for a keepalive (empty plaintext) and verify that `enc.Open(hdr, sealed)` returns a zero-length slice without error.

### `cmd/demo/main.go`: Crypto Round-Trip Without Root

This demo exercises the exported API without creating a TUN device or network socket. Run it with `go run ./cmd/demo` from the module root.

```go
package main

import (
	"crypto/rand"
	"errors"
	"fmt"
	"log"

	"example.com/tunnel"
)

func main() {
	key := make([]byte, tunnel.KeySize)
	if _, err := rand.Read(key); err != nil {
		log.Fatal(err)
	}
	enc, err := tunnel.NewEncryptor(key)
	if err != nil {
		log.Fatal(err)
	}

	// Simulate a 60-byte IPv4 packet (version=4, IHL=5).
	plaintext := make([]byte, 60)
	plaintext[0] = 0x45

	hdr := tunnel.Header{Seq: 1, Type: tunnel.TypeData, BodyLen: uint32(len(plaintext))}
	hdrBytes := make([]byte, tunnel.HeaderSize)
	hdr.Marshal(hdrBytes)

	sealed, err := enc.Seal(hdrBytes, plaintext)
	if err != nil {
		log.Fatal(err)
	}

	// Total per-packet overhead: header(16) + nonce(12) + GCM tag(16) = 44 bytes.
	// For Ethernet pathMTU=1500: TUN MTU = 1500 - 28(UDP+IP) - 44 = 1428.
	wireSize := tunnel.HeaderSize + len(sealed)
	overhead := tunnel.HeaderSize + tunnel.NonceSize + 16
	fmt.Printf("plaintext %d bytes -> wire %d bytes (per-packet overhead %d)\n",
		len(plaintext), wireSize, overhead)

	recovered, err := enc.Open(hdrBytes, sealed)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("round-trip OK: %d bytes recovered\n", len(recovered))

	// Tamper with the GCM tag and confirm authentication failure.
	sealed[len(sealed)-1] ^= 0xFF
	if _, err := enc.Open(hdrBytes, sealed); errors.Is(err, tunnel.ErrAuth) {
		fmt.Println("tamper detected: ErrAuth")
	}

	// Replay window demonstration.
	var rw tunnel.ReplayWindow
	for _, seq := range []uint64{5, 10, 8, 10, 3} {
		if err := rw.Check(seq); err != nil {
			fmt.Printf("seq %2d REJECTED\n", seq)
		} else {
			fmt.Printf("seq %2d accepted\n", seq)
		}
	}
}
```

### Reference: The Full Relay Daemon

The code below requires `github.com/songgao/water` and root privileges. It is not part of the compiled and tested package above; it is presented as reference architecture to show how `header.go`, `crypto.go`, and `replay.go` compose into a real VPN daemon.

Add the dependency with `go get github.com/songgao/water` and run with `sudo`:

```bash
go get github.com/songgao/water
sudo go run ./cmd/vpnd -local :51820 -peer 203.0.113.1:51820 -key <64-hex-chars> -vip 10.0.0.1/30
```

`cmd/vpnd/main.go` (requires external module and root; not compiled offline):

```go
// Requires: github.com/songgao/water; run with sudo.
package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"example.com/tunnel"
	"github.com/songgao/water"
)

const (
	pathMTU      = 1500
	// tunMTU = pathMTU - UDP/IP overhead(28) - tunnel overhead(16+12+16=44)
	tunMTU       = pathMTU - 28 - 44 // 1428
	keepaliveIval = 25 * time.Second
)

func main() {
	localAddr := flag.String("local", ":51820", "local UDP listen address")
	peerAddr  := flag.String("peer", "", "peer UDP address (required)")
	keyHex    := flag.String("key", "", "32-byte AES-256 key, 64 hex chars (required)")
	vip       := flag.String("vip", "10.0.0.1/30", "virtual IP/prefix for the TUN device")
	flag.Parse()

	if *peerAddr == "" || *keyHex == "" {
		fmt.Fprintln(os.Stderr, "usage: vpnd -peer <addr> -key <hex>")
		os.Exit(1)
	}
	key, err := hex.DecodeString(*keyHex)
	if err != nil || len(key) != tunnel.KeySize {
		fmt.Fprintln(os.Stderr, "key must be 64 hex characters (32 bytes AES-256)")
		os.Exit(1)
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	iface, err := water.New(water.Config{DeviceType: water.TUN})
	if err != nil {
		log.Error("create TUN", "err", err)
		os.Exit(1)
	}
	defer iface.Close()

	ifName := iface.Name()
	log.Info("TUN created", "name", ifName)

	// Configure TUN interface: assign virtual IP, set MTU, bring it up.
	for _, args := range [][]string{
		{"link", "set", ifName, "mtu", fmt.Sprint(tunMTU)},
		{"addr", "add", *vip, "dev", ifName},
		{"link", "set", ifName, "up"},
	} {
		if out, err := exec.Command("ip", args...).CombinedOutput(); err != nil {
			log.Error("ip command failed", "args", args, "output", string(out))
			os.Exit(1)
		}
	}
	log.Info("TUN configured", "vip", *vip, "mtu", tunMTU)

	enc, err := tunnel.NewEncryptor(key)
	if err != nil {
		log.Error("encryptor", "err", err)
		os.Exit(1)
	}

	laddr, _ := net.ResolveUDPAddr("udp4", *localAddr)
	paddr, err := net.ResolveUDPAddr("udp4", *peerAddr)
	if err != nil {
		log.Error("resolve peer", "err", err)
		os.Exit(1)
	}
	conn, err := net.ListenUDP("udp4", laddr)
	if err != nil {
		log.Error("listen UDP", "err", err)
		os.Exit(1)
	}
	defer conn.Close()

	var (
		seq    atomic.Uint64
		replay tunnel.ReplayWindow
	)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go sendLoop(ctx, iface, conn, paddr, enc, &seq, log)
	go recvLoop(ctx, iface, conn, paddr, enc, &replay, log)
	go keepaliveLoop(ctx, conn, paddr, enc, &seq, log)

	<-ctx.Done()
	log.Info("shutting down")
}

func sendLoop(
	ctx context.Context,
	iface *water.Interface,
	conn *net.UDPConn,
	peer *net.UDPAddr,
	enc *tunnel.Encryptor,
	seq *atomic.Uint64,
	log *slog.Logger,
) {
	buf := make([]byte, 65535)
	hdrBuf := make([]byte, tunnel.HeaderSize)
	for ctx.Err() == nil {
		n, err := iface.Read(buf)
		if err != nil {
			log.Error("TUN read", "err", err)
			return
		}
		s := seq.Add(1)
		h := tunnel.Header{Seq: s, Type: tunnel.TypeData, BodyLen: uint32(n)}
		h.Marshal(hdrBuf)
		sealed, err := enc.Seal(hdrBuf, buf[:n])
		if err != nil {
			log.Error("seal", "err", err)
			continue
		}
		wire := make([]byte, tunnel.HeaderSize+len(sealed))
		copy(wire, hdrBuf)
		copy(wire[tunnel.HeaderSize:], sealed)
		if _, err := conn.WriteToUDP(wire, peer); err != nil {
			log.Error("UDP write", "err", err)
		}
	}
}

func recvLoop(
	ctx context.Context,
	iface *water.Interface,
	conn *net.UDPConn,
	peer *net.UDPAddr,
	enc *tunnel.Encryptor,
	replay *tunnel.ReplayWindow,
	log *slog.Logger,
) {
	buf := make([]byte, 65535)
	for ctx.Err() == nil {
		n, addr, err := conn.ReadFromUDP(buf)
		if err != nil {
			log.Error("UDP read", "err", err)
			return
		}
		if addr.String() != peer.String() {
			log.Warn("unexpected source", "addr", addr)
			continue
		}
		if n < tunnel.HeaderSize {
			log.Warn("packet too short", "bytes", n)
			continue
		}
		hdr, err := tunnel.UnmarshalHeader(buf[:n])
		if err != nil {
			log.Warn("bad header", "err", err)
			continue
		}
		if err := replay.Check(hdr.Seq); err != nil {
			log.Warn("replay rejected", "seq", hdr.Seq, "err", err)
			continue
		}
		if hdr.Type == tunnel.TypeKeepalive {
			continue
		}
		plain, err := enc.Open(buf[:tunnel.HeaderSize], buf[tunnel.HeaderSize:n])
		if err != nil {
			log.Warn("decrypt failed", "seq", hdr.Seq, "err", err)
			continue
		}
		if _, err := iface.Write(plain); err != nil {
			log.Error("TUN write", "err", err)
		}
	}
}

func keepaliveLoop(
	ctx context.Context,
	conn *net.UDPConn,
	peer *net.UDPAddr,
	enc *tunnel.Encryptor,
	seq *atomic.Uint64,
	log *slog.Logger,
) {
	ticker := time.NewTicker(keepaliveIval)
	defer ticker.Stop()
	hdrBuf := make([]byte, tunnel.HeaderSize)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s := seq.Add(1)
			h := tunnel.Header{Seq: s, Type: tunnel.TypeKeepalive, BodyLen: 0}
			h.Marshal(hdrBuf)
			sealed, err := enc.Seal(hdrBuf, nil)
			if err != nil {
				log.Error("keepalive seal", "err", err)
				continue
			}
			wire := make([]byte, tunnel.HeaderSize+len(sealed))
			copy(wire, hdrBuf)
			copy(wire[tunnel.HeaderSize:], sealed)
			if _, err := conn.WriteToUDP(wire, peer); err != nil {
				log.Error("keepalive write", "err", err)
			}
		}
	}
}
```

## Common Mistakes

### Omitting the Header from GCM AAD

Wrong: passing `nil` as the additional data to `aead.Seal` and `aead.Open`.

What happens: the sequence number is on the wire in plaintext but is not integrity-protected. An adversary can flip bits in the sequence number field without invalidating the GCM tag, breaking replay detection and potentially routing packets to the wrong stream.

Fix: always pass the serialized header bytes as the `additionalData` argument to both `Seal` and `Open`. The GCM tag then covers both the ciphertext and the header.

### Nonce Derived from the Sequence Number

Wrong:
```go
var nonce [12]byte
binary.BigEndian.PutUint64(nonce[4:], seq)
sealed := aead.Seal(nil, nonce[:], plaintext, hdr)
```

What happens: if the counter resets (reconnect, counter overflow, accidental reuse of session keys across connections), the same nonce/key pair is reused. GCM nonce reuse recovers the authentication key and exposes the plaintexts of both messages encrypted under the reused nonce/key.

Fix: generate each nonce with `io.ReadFull(rand.Reader, nonce)`. With a 96-bit random nonce the collision probability reaches 2^{-32} at 2^{32} encryptions under one key; rotate the key well before that threshold.

### Wrong MTU Arithmetic

Wrong: setting TUN MTU equal to the underlying network MTU (1500).

What happens: the kernel produces a 1500-byte IP packet. After adding 44 bytes of tunnel overhead and 28 bytes of UDP/IP header, the UDP datagram is 1572 bytes. The underlying link drops it (if the DF bit is set) or fragments it. Either way TCP stalls.

Fix: `tunMTU = pathMTU - 72` (28 for UDP/IP, 44 for tunnel). For Ethernet: `1500 - 72 = 1428`.

### String-Matching on Authentication Errors

Wrong:
```go
if err != nil && strings.Contains(err.Error(), "authentication") { ... }
```

Fix:
```go
if errors.Is(err, tunnel.ErrAuth) { ... }
```

The error string is not part of the API contract. Sentinel errors wrapped with `%w` are the stable, version-safe interface.

## Verification

The core package (`header.go`, `crypto.go`, `replay.go`) uses only the standard library and can be verified fully offline. Run from `~/go-exercises/tunnel`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. The `-race` flag is mandatory because `ReplayWindow` uses a mutex; the race detector confirms the lock protocol is complete.

The relay daemon (`cmd/vpnd`) cannot be verified offline; it requires `github.com/songgao/water` and a system with Linux TUN support. To verify it end-to-end, run two instances on separate hosts or network namespaces and confirm bidirectional ping across the tunnel.

## Summary

- A VPN tunnel reads raw IP packets from a TUN device, encrypts them with AES-256-GCM, and relays them over UDP; the peer reverses the process.
- The 16-byte wire header (Seq, Type, BodyLen) is transmitted in plaintext but is passed as GCM additional authenticated data, so it is integrity-protected without being encrypted.
- Per-packet overhead is 44 bytes (16 header + 12 nonce + 16 GCM tag); TUN MTU = pathMTU - 28 - 44 = 1428 for Ethernet.
- Each GCM nonce is freshly generated with `crypto/rand` to prevent nonce reuse, which is a catastrophic failure in GCM.
- A sliding-window replay detector (64-bit bitmap, window of 64) rejects duplicate and out-of-window sequence numbers in O(1) time with a single mutex.

## What's Next

Next: [NAT Traversal with STUN/TURN](../27-nat-traversal-stun-turn/27-nat-traversal-stun-turn.md).

## Resources

- [crypto/cipher - pkg.go.dev](https://pkg.go.dev/crypto/cipher) - `NewGCM`, `AEAD.Seal`, `AEAD.Open` signatures and semantics
- [crypto/aes - pkg.go.dev](https://pkg.go.dev/crypto/aes) - `NewCipher`; key must be 16, 24, or 32 bytes
- [RFC 5116: An Interface and Algorithms for Authenticated Encryption](https://datatracker.ietf.org/doc/html/rfc5116) - formal semantics of AEAD, AAD, and nonce requirements
- [WireGuard Whitepaper](https://www.wireguard.com/papers/wireguard.pdf) - canonical reference for header-as-AAD design and nonce handling in a production VPN
- [Linux kernel TUN/TAP documentation](https://www.kernel.org/doc/Documentation/networking/tuntap.txt) - IFF_NO_PI, device lifecycle, and MTU behavior
- [songgao/water](https://pkg.go.dev/github.com/songgao/water) - cross-platform TUN/TAP library used in the reference daemon
