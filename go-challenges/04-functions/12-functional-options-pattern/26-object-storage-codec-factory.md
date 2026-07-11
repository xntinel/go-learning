# Exercise 26: Object Storage Client With Compression and Encryption Codec Options

**Nivel: Intermedio** — validacion rapida (un test corto).

An S3-like storage client that lets a caller pick a compression codec and an
encryption algorithm independently has to guard two different kinds of
mistakes: a single option receiving garbage (an unsupported algorithm, a
key of the wrong length) and two options that are each individually valid
but never allowed to combine. This module builds that client with
functional options and validates both.

## What you'll build

```text
objstore/                        independent module: example.com/object-storage-codec-factory
  go.mod                         go 1.24
  objstore.go                    Client, StoredObject, Option, New, WithCompression,
                                  WithEncryption, Compression, Encryption, Put
  cmd/
    demo/
      main.go                    stores an object with zstd+aes256gcm, then shows a rejected combo
  objstore_test.go                option-validation table, size math, and accessor tests
```

- Files: `objstore.go`, `cmd/demo/main.go`, `objstore_test.go`.
- Implement: a `Client` built by `New(bucket string, opts ...Option) (*Client, error)` whose `WithEncryption` validates its key length immediately and whose `New` rejects the combination of gzip compression with chacha20poly1305 encryption after every option has run.
- Test: every option-validation case including both allowed and disallowed codec/algorithm combinations, the simulated stored-size math for a compression ratio plus an encryption tag, and the plain accessors.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/objstore/cmd/demo
cd ~/go-exercises/objstore
go mod init example.com/object-storage-codec-factory
go mod edit -go=1.24
```

### Two kinds of invalid, two places to catch them

`WithEncryption("aes256gcm", key)` can be rejected the moment it runs: the
algorithm and its key both arrive in the same call, so the option closure
has everything it needs to check the key is exactly the required length.
The gzip/chacha20poly1305 incompatibility is different — `WithCompression`
only ever sees the compression codec, and `WithEncryption` only ever sees
the encryption algorithm, so neither closure can know what the other one
set. That check has to wait until `New` has applied every option, exactly
the same constructor-boundary shape used for every other cross-field
invariant in this chapter: seed defaults, apply options in order, validate
the combination once nothing can change it further.

### Simulated compression and encryption

`Put` does not actually compress or encrypt bytes — that would pull in real
codec and cipher libraries this module has no need for. Instead it applies
the configured compression codec's ratio to the plaintext size (a fixed
numerator/denominator per codec) and then adds a fixed authentication-tag
overhead for the configured encryption algorithm. The math is simple enough
to assert exactly in a test, which is the point: the exercise is about
validating the *configuration*, and the simulated size calculation gives
that configuration an observable, testable effect.

Create `objstore.go`:

```go
package objstore

import "fmt"

// compressionRatios maps a compression codec to the numerator/denominator
// pair used to simulate its effect on stored size.
var compressionRatios = map[string][2]int{
	"none": {1, 1},
	"gzip": {3, 5},
	"zstd": {1, 2},
}

// encryptionTagOverhead maps an encryption algorithm to the fixed
// authentication-tag overhead it adds to stored size.
var encryptionTagOverhead = map[string]int{
	"none":             0,
	"aes256gcm":        16,
	"chacha20poly1305": 16,
}

// requiredKeyLen is the key length, in bytes, every non-"none" encryption
// algorithm in this client requires.
const requiredKeyLen = 32

// Client is a storage client for a single bucket with a fixed compression
// codec and encryption algorithm.
type Client struct {
	bucket        string
	compression   string
	encryptionAlg string
	encryptionKey []byte
}

// Option configures a Client and may reject invalid input.
type Option func(*Client) error

// StoredObject describes an object as the client would have written it.
type StoredObject struct {
	Key           string
	Compression   string
	Encryption    string
	PlaintextSize int
	StoredSize    int
}

// New builds a Client for bucket, seeding defaults ("none" compression,
// "none" encryption) and then applying opts. It is the single validation
// boundary for cross-field rules that no single option can see on its own:
// certain compression/encryption combinations are incompatible.
func New(bucket string, opts ...Option) (*Client, error) {
	if bucket == "" {
		return nil, fmt.Errorf("bucket name is required")
	}

	c := &Client{
		bucket:        bucket,
		compression:   "none",
		encryptionAlg: "none",
	}
	for _, opt := range opts {
		if err := opt(c); err != nil {
			return nil, err
		}
	}

	if c.compression == "gzip" && c.encryptionAlg == "chacha20poly1305" {
		return nil, fmt.Errorf("incompatible combination: gzip compression with chacha20poly1305 encryption")
	}
	return c, nil
}

// WithCompression sets the compression codec ("none", "gzip", or "zstd").
func WithCompression(codec string) Option {
	return func(c *Client) error {
		if _, ok := compressionRatios[codec]; !ok {
			return fmt.Errorf("unsupported compression codec: %q", codec)
		}
		c.compression = codec
		return nil
	}
}

// WithEncryption sets the encryption algorithm ("none", "aes256gcm", or
// "chacha20poly1305") and its key. Any algorithm other than "none" requires
// a key exactly requiredKeyLen bytes long.
func WithEncryption(algorithm string, key []byte) Option {
	return func(c *Client) error {
		if _, ok := encryptionTagOverhead[algorithm]; !ok {
			return fmt.Errorf("unsupported encryption algorithm: %q", algorithm)
		}
		if algorithm != "none" && len(key) != requiredKeyLen {
			return fmt.Errorf("encryption key for %s must be %d bytes, got %d", algorithm, requiredKeyLen, len(key))
		}
		c.encryptionAlg = algorithm
		c.encryptionKey = key
		return nil
	}
}

// Compression reports the configured compression codec.
func (c *Client) Compression() string { return c.compression }

// Encryption reports the configured encryption algorithm.
func (c *Client) Encryption() string { return c.encryptionAlg }

// Put simulates writing data under key, returning the object as the codec
// and encryption settings would have transformed it: compression scales the
// plaintext size by its ratio, and encryption (if any) adds a fixed
// authentication-tag overhead on top.
func (c *Client) Put(key string, data []byte) StoredObject {
	ratio := compressionRatios[c.compression]
	stored := len(data) * ratio[0] / ratio[1]
	stored += encryptionTagOverhead[c.encryptionAlg]

	return StoredObject{
		Key:           key,
		Compression:   c.compression,
		Encryption:    c.encryptionAlg,
		PlaintextSize: len(data),
		StoredSize:    stored,
	}
}
```

### The runnable demo

The demo stores one object with zstd compression and AES-256-GCM encryption,
prints the simulated size math, and then shows that gzip combined with
chacha20poly1305 is rejected at construction.

Create `cmd/demo/main.go`:

```go
package main

import (
	"bytes"
	"fmt"

	"example.com/object-storage-codec-factory"
)

func main() {
	key := bytes.Repeat([]byte{0x42}, 32)

	client, err := objstore.New(
		"orders-archive",
		objstore.WithCompression("zstd"),
		objstore.WithEncryption("aes256gcm", key),
	)
	if err != nil {
		panic(err)
	}

	payload := bytes.Repeat([]byte("x"), 1000)
	obj := client.Put("orders/2026-07-05.json", payload)

	fmt.Printf("compression: %s\n", obj.Compression)
	fmt.Printf("encryption: %s\n", obj.Encryption)
	fmt.Printf("plaintext size: %d\n", obj.PlaintextSize)
	fmt.Printf("stored size: %d\n", obj.StoredSize)

	_, err = objstore.New(
		"orders-archive",
		objstore.WithCompression("gzip"),
		objstore.WithEncryption("chacha20poly1305", key),
	)
	fmt.Printf("incompatible combo rejected: %t\n", err != nil)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
compression: zstd
encryption: aes256gcm
plaintext size: 1000
stored size: 516
incompatible combo rejected: true
```

### Tests

`TestNewValidation` tables both option-level failures (unsupported codec,
unsupported algorithm, wrong key length) and the cross-field combination
check, including the case where gzip is paired with the *other* algorithm
(aes256gcm) and the incompatibility does not apply. `TestPutAppliesCompressionAndEncryptionOverhead`
asserts the exact stored-size arithmetic for zstd plus AES-256-GCM.
`TestPutWithNoCompressionOrEncryption` proves the defaults leave size
unchanged. `TestAccessors` covers the plain getters.

Create `objstore_test.go`:

```go
package objstore

import (
	"bytes"
	"testing"
)

func validKey() []byte { return bytes.Repeat([]byte{0x01}, requiredKeyLen) }

func TestNewValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		bucket  string
		opts    []Option
		wantErr bool
	}{
		{name: "defaults only", bucket: "b"},
		{name: "empty bucket", bucket: "", wantErr: true},
		{name: "unsupported compression", bucket: "b", opts: []Option{WithCompression("brotli")}, wantErr: true},
		{name: "unsupported encryption", bucket: "b", opts: []Option{WithEncryption("rot13", nil)}, wantErr: true},
		{name: "short encryption key", bucket: "b", opts: []Option{WithEncryption("aes256gcm", []byte("short"))}, wantErr: true},
		{name: "no key needed for none", bucket: "b", opts: []Option{WithEncryption("none", nil)}},
		{
			name:    "gzip with chacha20poly1305 is incompatible",
			bucket:  "b",
			opts:    []Option{WithCompression("gzip"), WithEncryption("chacha20poly1305", validKey())},
			wantErr: true,
		},
		{
			name:   "gzip with aes256gcm is allowed",
			bucket: "b",
			opts:   []Option{WithCompression("gzip"), WithEncryption("aes256gcm", validKey())},
		},
		{
			name:   "zstd with chacha20poly1305 is allowed",
			bucket: "b",
			opts:   []Option{WithCompression("zstd"), WithEncryption("chacha20poly1305", validKey())},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := New(tt.bucket, tt.opts...)
			if tt.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestPutAppliesCompressionAndEncryptionOverhead(t *testing.T) {
	t.Parallel()

	client, err := New("b", WithCompression("zstd"), WithEncryption("aes256gcm", validKey()))
	if err != nil {
		t.Fatal(err)
	}

	obj := client.Put("k", bytes.Repeat([]byte("x"), 1000))
	if obj.PlaintextSize != 1000 {
		t.Fatalf("PlaintextSize = %d, want 1000", obj.PlaintextSize)
	}
	// zstd ratio 1/2 -> 500, plus a 16-byte aes256gcm tag -> 516.
	if obj.StoredSize != 516 {
		t.Fatalf("StoredSize = %d, want 516", obj.StoredSize)
	}
}

func TestPutWithNoCompressionOrEncryption(t *testing.T) {
	t.Parallel()

	client, err := New("b")
	if err != nil {
		t.Fatal(err)
	}

	obj := client.Put("k", bytes.Repeat([]byte("x"), 100))
	if obj.StoredSize != 100 {
		t.Fatalf("StoredSize = %d, want 100 (no compression, no encryption overhead)", obj.StoredSize)
	}
}

func TestAccessors(t *testing.T) {
	t.Parallel()

	client, err := New("b", WithCompression("gzip"), WithEncryption("aes256gcm", validKey()))
	if err != nil {
		t.Fatal(err)
	}
	if client.Compression() != "gzip" {
		t.Fatalf("Compression() = %q, want gzip", client.Compression())
	}
	if client.Encryption() != "aes256gcm" {
		t.Fatalf("Encryption() = %q, want aes256gcm", client.Encryption())
	}
}
```

## Review

The client is correct when an invalid value never survives past the option
that received it, and an invalid *combination* of otherwise-valid values
never survives past `New`. Those are genuinely different failure classes:
`WithEncryption` alone can never know whether the compression codec its
sibling option set will turn out to be incompatible, so pushing the
combination check into the option would either miss it (if compression is
set second) or require options to run in a specific order, which functional
options are explicitly designed not to require. Validating combinations in
the constructor, once every option has had its turn, is what keeps option
order irrelevant while still catching every invalid configuration before a
`Client` value ever escapes `New`.

## Resources

- [Dave Cheney: Functional options for friendly APIs](https://dave.cheney.net/2014/10/17/functional-options-for-friendly-apis)
- [AWS S3 server-side encryption](https://docs.aws.amazon.com/AmazonS3/latest/userguide/serv-side-encryption.html)
- [crypto/cipher AEAD](https://pkg.go.dev/crypto/cipher#AEAD)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [25-endpoint-health-checker.md](25-endpoint-health-checker.md) | Next: [27-user-session-storage-backend.md](27-user-session-storage-backend.md)
