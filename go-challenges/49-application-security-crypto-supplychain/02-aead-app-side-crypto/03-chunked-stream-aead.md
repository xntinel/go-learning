# Exercise 3: Chunked Streaming AEAD with Counter Nonces and Truncation Defense

A single `Seal` call caps out near 64 GiB, so a file upload or backup stream must
be encrypted chunk by chunk. But a naive sequence of independently-sealed chunks
is authentic per chunk and forgeable as a *stream*: an attacker can drop the tail
or reorder chunks and every remaining chunk still verifies. This exercise builds
the STREAM defense — a per-message random nonce prefix plus a counter, with each
chunk's index and a last-chunk flag bound as associated data — so reordering,
duplication, and truncation are all rejected.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
chunkstream/               independent module: example.com/chunkstream
  go.mod                   go 1.24
  chunkstream.go          type Stream; New(key, chunkSize); Encrypt, Decrypt
  cmd/
    demo/
      main.go             encrypt a multi-chunk buffer, tamper one chunk, detect
  chunkstream_test.go      round-trip, truncation, reorder, dup, tamper, nonce-distinct
```

- Files: `chunkstream.go`, `cmd/demo/main.go`, `chunkstream_test.go`.
- Implement: `New(key []byte, chunkSize int)` and a `Stream` with `Encrypt(payload) []byte` / `Decrypt(blob) []byte`; split the payload into fixed chunks, derive each chunk's 12-byte nonce from a per-message random prefix plus a big-endian counter, and bind `(chunkIndex, lastFlag)` as AAD.
- Test: round-trip across several chunks, an exact-multiple boundary, and empty input; dropping the final chunk fails (truncation); swapping two chunks fails (reorder); duplicating a chunk fails; a flipped byte fails; all chunk nonces in one `Encrypt` are distinct.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/49-application-security-crypto-supplychain/02-aead-app-side-crypto/03-chunked-stream-aead/cmd/demo
cd go-solutions/49-application-security-crypto-supplychain/02-aead-app-side-crypto/03-chunked-stream-aead
go mod edit -go=1.24
```

### The nonce: prefix plus counter

Each chunk needs a nonce that is unique within the message. The construction
here is a 4-byte random prefix, drawn once per `Encrypt` call, followed by an
8-byte big-endian counter that is simply the chunk index. GCM's nonce is 12
bytes, so `4 + 8` fills it exactly. The counter guarantees uniqueness *within* a
message — chunk 0, 1, 2, ... never collide — and the random prefix is
defense-in-depth against accidental key reuse across two messages.

Why is a 4-byte (32-bit) prefix acceptable? Because the real safety comes from
using a fresh key per stream. This lesson takes the key as given; the next lesson
(envelope encryption) derives a fresh data-encryption key for each object, so the
nonce prefix only has to be unique within one stream — which the counter already
guarantees — and the random prefix is a belt-and-suspenders guard. If you instead
reused one long-lived key across billions of streams, you would raise the prefix
to 8 bytes (or use XChaCha's 24-byte nonce) to keep the cross-stream birthday
bound negligible. The nonce need not be secret, and here it is fully
reconstructible at decrypt time from the stored prefix and the chunk position, so
nothing about it is transmitted per chunk.

### The AAD: binding order and termination

The nonce keeps chunks distinct, but distinctness alone does not stop reordering
or truncation — a reordered chunk still has a valid tag *for its own nonce*. The
fix is to authenticate each chunk's position and whether it is the last chunk.
Each chunk's associated data is 9 bytes: the 8-byte big-endian chunk index and a
1-byte flag that is 1 for the final chunk and 0 otherwise.

Now the three stream attacks fail cleanly:

- Reorder: a chunk moved to a different position is decrypted expecting a
  different index in its AAD, so `Open` fails.
- Truncation: drop the final chunk and the new last chunk was sealed with
  `last=0` but is now decrypted expecting `last=1`, so `Open` fails. This is the
  defense the classic GCM truncation attack cannot beat.
- Duplication: a duplicated chunk lands at a position whose index no longer
  matches its AAD, so `Open` fails.

### The framing

The blob is the 4-byte nonce prefix, then a sequence of length-prefixed frames:
each frame is a 4-byte big-endian length followed by that many sealed bytes
(ciphertext plus 16-byte tag). `Decrypt` walks the frames left to right,
counting the index as it goes, and treats the frame that leaves no bytes after it
as the last chunk. An empty payload still produces exactly one frame — an empty
last chunk — so the last-chunk flag always authenticates the end of the stream.

Create `chunkstream.go`:

```go
package chunkstream

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
)

// KeySize selects AES-256.
const KeySize = 32

// prefixSize is the per-message random nonce prefix; the remaining nonce bytes
// hold the big-endian chunk counter.
const prefixSize = 4

// aadSize is 8 bytes of chunk index plus 1 byte last-chunk flag.
const aadSize = 9

var (
	// ErrKeySize is returned by New for a wrong-length key.
	ErrKeySize = errors.New("chunkstream: key must be 32 bytes")
	// ErrChunkSize is returned by New for a non-positive chunk size.
	ErrChunkSize = errors.New("chunkstream: chunk size must be positive")
	// ErrShortBlob is returned by Decrypt when framing is truncated.
	ErrShortBlob = errors.New("chunkstream: blob truncated")
	// ErrOpen is returned by Decrypt when a chunk fails authentication,
	// including reorder, duplication, tamper, and truncation.
	ErrOpen = errors.New("chunkstream: authentication failed")
)

// Stream encrypts and decrypts payloads too large for a single Seal call, using
// per-chunk AES-256-GCM with counter nonces and index/last-flag associated data.
type Stream struct {
	aead      cipher.AEAD
	chunkSize int
}

// New builds a Stream from a 32-byte key and a positive plaintext chunk size.
func New(key []byte, chunkSize int) (*Stream, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("%w: got %d", ErrKeySize, len(key))
	}
	if chunkSize <= 0 {
		return nil, fmt.Errorf("%w: got %d", ErrChunkSize, chunkSize)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Stream{aead: aead, chunkSize: chunkSize}, nil
}

// nonce derives a chunk nonce from the per-message prefix and the chunk index.
func (s *Stream) nonce(prefix []byte, index uint64) []byte {
	nonce := make([]byte, s.aead.NonceSize())
	copy(nonce, prefix)
	binary.BigEndian.PutUint64(nonce[prefixSize:], index)
	return nonce
}

// chunkAAD encodes the chunk index and whether it is the last chunk.
func chunkAAD(index uint64, last bool) []byte {
	aad := make([]byte, aadSize)
	binary.BigEndian.PutUint64(aad[:8], index)
	if last {
		aad[8] = 1
	}
	return aad
}

// Encrypt splits payload into fixed-size chunks and seals each with a distinct
// counter nonce and index/last-flag AAD. Empty input yields one empty chunk.
func (s *Stream) Encrypt(payload []byte) ([]byte, error) {
	prefix := make([]byte, prefixSize)
	if _, err := rand.Read(prefix); err != nil {
		return nil, err
	}

	numChunks := 1
	if len(payload) > 0 {
		numChunks = (len(payload) + s.chunkSize - 1) / s.chunkSize
	}

	out := make([]byte, 0, prefixSize+len(payload)+numChunks*(4+s.aead.Overhead()))
	out = append(out, prefix...)

	for i := range numChunks {
		start := i * s.chunkSize
		end := min(start+s.chunkSize, len(payload))
		chunk := payload[start:end]
		last := i == numChunks-1
		sealed := s.aead.Seal(nil, s.nonce(prefix, uint64(i)), chunk, chunkAAD(uint64(i), last))

		var lenBuf [4]byte
		binary.BigEndian.PutUint32(lenBuf[:], uint32(len(sealed)))
		out = append(out, lenBuf[:]...)
		out = append(out, sealed...)
	}
	return out, nil
}

// Decrypt reverses Encrypt. Any reordered, duplicated, dropped, or tampered
// chunk fails with ErrOpen; malformed framing fails with ErrShortBlob.
func (s *Stream) Decrypt(blob []byte) ([]byte, error) {
	if len(blob) < prefixSize {
		return nil, ErrShortBlob
	}
	prefix, rest := blob[:prefixSize], blob[prefixSize:]

	var out []byte
	var index uint64
	for len(rest) > 0 {
		if len(rest) < 4 {
			return nil, ErrShortBlob
		}
		n := binary.BigEndian.Uint32(rest[:4])
		rest = rest[4:]
		if uint64(len(rest)) < uint64(n) {
			return nil, ErrShortBlob
		}
		sealed := rest[:n]
		rest = rest[n:]

		last := len(rest) == 0
		plaintext, err := s.aead.Open(nil, s.nonce(prefix, index), sealed, chunkAAD(index, last))
		if err != nil {
			return nil, fmt.Errorf("%w: chunk %d", ErrOpen, index)
		}
		out = append(out, plaintext...)
		index++
	}
	return out, nil
}
```

### The runnable demo

The demo encrypts a buffer that spans several chunks, decrypts it, then flips a
byte inside one chunk's ciphertext and shows detection. Sizes are deterministic;
the ciphertext is not, so the demo prints lengths and outcomes.

Create `cmd/demo/main.go`:

```go
package main

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"log"

	"example.com/chunkstream"
)

func main() {
	key := make([]byte, chunkstream.KeySize)
	if _, err := rand.Read(key); err != nil {
		log.Fatal(err)
	}
	s, err := chunkstream.New(key, 16) // 16-byte chunks
	if err != nil {
		log.Fatal(err)
	}

	payload := []byte("the quick brown fox jumps over the lazy dog, twice over")
	blob, err := s.Encrypt(payload)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("payload: %d bytes -> blob: %d bytes\n", len(payload), len(blob))

	got, err := s.Decrypt(blob)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("decrypted matches: %v\n", bytes.Equal(got, payload))

	// Flip a byte inside the framed chunks (past the 4-byte prefix).
	tampered := append([]byte(nil), blob...)
	tampered[len(tampered)-1] ^= 0x01
	if _, err := s.Decrypt(tampered); errors.Is(err, chunkstream.ErrOpen) {
		fmt.Println("tamper: rejected")
	}

	// Drop the final frame to simulate truncation.
	if _, err := s.Decrypt(dropLastFrame(blob)); errors.Is(err, chunkstream.ErrOpen) {
		fmt.Println("truncation: rejected")
	}
}

// dropLastFrame removes the last length-prefixed frame from a blob.
func dropLastFrame(blob []byte) []byte {
	rest := blob[4:] // skip nonce prefix
	lastStart := 4
	for len(rest) >= 4 {
		n := int(rest[0])<<24 | int(rest[1])<<16 | int(rest[2])<<8 | int(rest[3])
		frame := 4 + n
		if len(rest) < frame {
			break
		}
		if len(rest) == frame {
			return blob[:lastStart]
		}
		lastStart += frame
		rest = rest[frame:]
	}
	return blob
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
payload: 55 bytes -> blob: 139 bytes
decrypted matches: true
tamper: rejected
truncation: rejected
```

### Tests

The tests cover the round-trip (including a chunk-boundary exact multiple and
empty input) and each stream attack. `TestTruncation` drops the last frame and
requires `ErrOpen`, proving the last-chunk flag defeats truncation.
`TestReorder` swaps two frames; `TestDuplicate` repeats one; `TestTamper` flips a
byte; all must fail. `TestNoncesDistinct` calls the unexported `nonce` helper
directly (same-package test) to prove every counter nonce in a run is unique.

Create `chunkstream_test.go`:

```go
package chunkstream

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"testing"
)

func mustStream(t *testing.T, chunkSize int) *Stream {
	t.Helper()
	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	s, err := New(key, chunkSize)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestRoundTrip(t *testing.T) {
	t.Parallel()
	s := mustStream(t, 8)
	cases := []struct {
		name string
		size int
	}{
		{"empty", 0},
		{"partial", 3},
		{"exact-one-chunk", 8},
		{"exact-multiple", 24},
		{"several-with-tail", 30},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			payload := make([]byte, tc.size)
			if _, err := rand.Read(payload); err != nil {
				t.Fatal(err)
			}
			blob, err := s.Encrypt(payload)
			if err != nil {
				t.Fatalf("Encrypt: %v", err)
			}
			got, err := s.Decrypt(blob)
			if err != nil {
				t.Fatalf("Decrypt: %v", err)
			}
			if !bytes.Equal(got, payload) {
				t.Fatalf("round-trip mismatch for size %d", tc.size)
			}
		})
	}
}

// frames splits a blob into its nonce prefix and the list of sealed frames.
func frames(t *testing.T, blob []byte) (prefix []byte, out [][]byte) {
	t.Helper()
	prefix, rest := blob[:prefixSize], blob[prefixSize:]
	for len(rest) > 0 {
		n := binary.BigEndian.Uint32(rest[:4])
		out = append(out, append([]byte(nil), rest[4:4+n]...))
		rest = rest[4+n:]
	}
	return prefix, out
}

// reassemble rebuilds a blob from a prefix and a list of sealed frames.
func reassemble(prefix []byte, sealed [][]byte) []byte {
	out := append([]byte(nil), prefix...)
	for _, f := range sealed {
		var lenBuf [4]byte
		binary.BigEndian.PutUint32(lenBuf[:], uint32(len(f)))
		out = append(out, lenBuf[:]...)
		out = append(out, f...)
	}
	return out
}

func TestTruncation(t *testing.T) {
	t.Parallel()
	s := mustStream(t, 8)
	blob, err := s.Encrypt(make([]byte, 30)) // 4 chunks
	if err != nil {
		t.Fatal(err)
	}
	prefix, sealed := frames(t, blob)
	if len(sealed) < 2 {
		t.Fatalf("want multiple chunks, got %d", len(sealed))
	}
	truncated := reassemble(prefix, sealed[:len(sealed)-1])
	if _, err := s.Decrypt(truncated); !errors.Is(err, ErrOpen) {
		t.Fatalf("truncation: err = %v, want ErrOpen", err)
	}
}

func TestReorder(t *testing.T) {
	t.Parallel()
	s := mustStream(t, 8)
	blob, err := s.Encrypt(make([]byte, 30))
	if err != nil {
		t.Fatal(err)
	}
	prefix, sealed := frames(t, blob)
	sealed[0], sealed[1] = sealed[1], sealed[0]
	if _, err := s.Decrypt(reassemble(prefix, sealed)); !errors.Is(err, ErrOpen) {
		t.Fatalf("reorder: err = %v, want ErrOpen", err)
	}
}

func TestDuplicate(t *testing.T) {
	t.Parallel()
	s := mustStream(t, 8)
	blob, err := s.Encrypt(make([]byte, 30))
	if err != nil {
		t.Fatal(err)
	}
	prefix, sealed := frames(t, blob)
	dup := append([][]byte{sealed[0]}, sealed...) // repeat chunk 0 at the front
	if _, err := s.Decrypt(reassemble(prefix, dup)); !errors.Is(err, ErrOpen) {
		t.Fatalf("duplicate: err = %v, want ErrOpen", err)
	}
}

func TestTamper(t *testing.T) {
	t.Parallel()
	s := mustStream(t, 8)
	blob, err := s.Encrypt(make([]byte, 20))
	if err != nil {
		t.Fatal(err)
	}
	tampered := append([]byte(nil), blob...)
	tampered[len(tampered)-1] ^= 0x01
	if _, err := s.Decrypt(tampered); !errors.Is(err, ErrOpen) {
		t.Fatalf("tamper: err = %v, want ErrOpen", err)
	}
}

func TestShortBlob(t *testing.T) {
	t.Parallel()
	s := mustStream(t, 8)
	if _, err := s.Decrypt([]byte{0x00, 0x01}); !errors.Is(err, ErrShortBlob) {
		t.Fatalf("short prefix: err = %v, want ErrShortBlob", err)
	}
	// Valid prefix but a frame length that overruns the buffer.
	bad := []byte{0, 0, 0, 0, 0, 0, 0, 99}
	if _, err := s.Decrypt(bad); !errors.Is(err, ErrShortBlob) {
		t.Fatalf("overrun frame: err = %v, want ErrShortBlob", err)
	}
}

func TestNoncesDistinct(t *testing.T) {
	t.Parallel()
	s := mustStream(t, 8)
	prefix := []byte{0xaa, 0xbb, 0xcc, 0xdd}
	seen := make(map[string]bool)
	for i := range uint64(2048) {
		n := string(s.nonce(prefix, i))
		if seen[n] {
			t.Fatalf("duplicate nonce at index %d", i)
		}
		seen[n] = true
	}
}

func TestNewRejectsBadArgs(t *testing.T) {
	t.Parallel()
	if _, err := New(make([]byte, 8), 16); !errors.Is(err, ErrKeySize) {
		t.Fatalf("bad key: err = %v, want ErrKeySize", err)
	}
	if _, err := New(make([]byte, KeySize), 0); !errors.Is(err, ErrChunkSize) {
		t.Fatalf("bad chunk size: err = %v, want ErrChunkSize", err)
	}
}

func TestLoneNonFinalChunkFails(t *testing.T) {
	t.Parallel()
	s := mustStream(t, 8)
	blob, err := s.Encrypt(make([]byte, 20)) // 3 chunks
	if err != nil {
		t.Fatal(err)
	}
	prefix, sealed := frames(t, blob)
	if len(sealed) < 2 {
		t.Fatalf("want multiple chunks, got %d", len(sealed))
	}
	// Re-encode only the first (non-final) chunk as if it were the whole blob.
	// It was sealed with last=0 but is now decrypted as the sole, final chunk
	// (last=1), so its AAD no longer matches and Open must fail.
	alone := reassemble(prefix, sealed[:1])
	if _, err := s.Decrypt(alone); !errors.Is(err, ErrOpen) {
		t.Fatalf("lone non-final chunk: err = %v, want ErrOpen", err)
	}
}

func Example() {
	key := make([]byte, KeySize)
	for i := range key {
		key[i] = byte(i)
	}
	s, err := New(key, 4)
	if err != nil {
		panic(err)
	}
	blob, err := s.Encrypt([]byte("streamed payload"))
	if err != nil {
		panic(err)
	}
	got, err := s.Decrypt(blob)
	if err != nil {
		panic(err)
	}
	fmt.Printf("%s\n", got)
	// Output: streamed payload
}
```

`TestLoneNonFinalChunkFails` closes the last hole: it re-encodes just the first
frame of a multi-chunk stream as if it were the whole blob and asserts `Decrypt`
fails, proving a single surviving non-final chunk cannot masquerade as a complete
stream.

## Review

The stream is correct when a payload of any size round-trips and every one of the
four stream attacks — tamper, reorder, duplicate, truncate — is rejected with
`ErrOpen`. The truncation test is the one that distinguishes this construction
from naive chunking: because the final chunk is sealed with `last=1` and any
other chunk with `last=0`, dropping the tail leaves a chunk whose AAD no longer
matches what `Decrypt` reconstructs, so it fails instead of returning a
plausible-looking prefix of the data.

The mistakes to avoid are in the binding. If you forget the index in the AAD,
reorder and duplicate attacks pass. If you forget the last-chunk flag, truncation
passes — the single most-missed defense. If you derive the nonce from the index
without a per-message prefix, two messages under the same key reuse nonces; if
you derive it from the prefix without the counter, chunks within a message reuse
nonces. Both are catastrophic, which is why `TestNoncesDistinct` checks the
derivation directly. Finally, guard the framing: a corrupt length prefix must
yield `ErrShortBlob`, never a slice panic. Run `go test -race ./...` to confirm.

## Resources

- [crypto/cipher: AEAD, NewGCM](https://pkg.go.dev/crypto/cipher) — `Seal`/`Open` semantics and `NonceSize`.
- [encoding/binary: BigEndian](https://pkg.go.dev/encoding/binary#BigEndian) — `PutUint64`/`PutUint32` for canonical nonce and AAD encoding.
- [Online Authenticated-Encryption and the STREAM construction (Hoang, Reyhanitabar, Rogaway, Vizár)](https://eprint.iacr.org/2015/189) — the segmented-stream design this exercise implements.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-cipher-agility-aead-envelope.md](02-cipher-agility-aead-envelope.md) | Next: [../03-envelope-encryption-kek-dek/00-concepts.md](../03-envelope-encryption-kek-dek/00-concepts.md)
