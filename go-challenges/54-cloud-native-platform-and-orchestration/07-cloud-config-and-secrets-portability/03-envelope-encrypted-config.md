# Exercise 3: Portable Secrets with Keeper Encrypt/Decrypt and DecryptDecode

Secrets should never sit in a config store or on disk in plaintext. This exercise
wires two things together: a portable secrets helper that encrypts and decrypts
payloads through a `secrets.Keeper`, and an encrypted-config variable whose stored
bytes are ciphertext, decrypted by the Keeper and only *then* JSON-decoded via
`runtimevar.DecryptDecode` chained with `runtimevar.JSONDecode`. The result is a
typed `*Config` produced from secret-at-rest ciphertext, with plaintext living
only in process memory.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
envelopeconfig/              independent module: example.com/envelopeconfig
  go.mod                     require gocloud.dev
  secretbox.go               type SecretBox over secrets.Keeper; Config; NewEncryptedDecoder; EncryptedLoader; sentinel errors
  cmd/
    demo/
      main.go                base64key keeper, encrypt config, load it back typed
  secretbox_test.go          round-trip, tampered ciphertext, integrated encrypted loader
```

Files: `secretbox.go`, `cmd/demo/main.go`, `secretbox_test.go`.
Implement: a `SecretBox` wrapping a `*secrets.Keeper` with `Encrypt`/`Decrypt`/`Close`; a `Config` struct; `NewEncryptedDecoder(k)` chaining `DecryptDecode` with `JSONDecode`; and an `EncryptedLoader` that yields a typed `*Config` from ciphertext.
Test: round-trip `Decrypt(Encrypt(pt)) == pt` with ciphertext distinct from plaintext; a tampered-ciphertext `Decrypt` returning `ErrDecrypt` via `errors.Is`; and the integrated loader decoding an encrypted JSON config to the right struct.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/54-cloud-native-platform-and-orchestration/07-cloud-config-and-secrets-portability/03-envelope-encrypted-config/cmd/demo
cd go-solutions/54-cloud-native-platform-and-orchestration/07-cloud-config-and-secrets-portability/03-envelope-encrypted-config
go get gocloud.dev@latest
```

### Why decryption belongs inside the decoder

The tempting but wrong design is to store plaintext, decode it to a struct, then
decrypt a field. That leaves plaintext at rest in the config store and creates a
decoded value a log line can leak. The envelope pattern inverts the order: store
ciphertext, and decrypt just-in-time, before parsing. `runtimevar` expresses this
as decoder composition. `runtimevar.DecryptDecode(keeper, post)` returns a decode
function that first calls `keeper.Decrypt` on the variable's raw bytes and only
then hands the resulting plaintext to `post`. So

```go
dec := runtimevar.NewDecoder(&Config{}, runtimevar.DecryptDecode(k, runtimevar.JSONDecode))
```

builds a decoder whose input is ciphertext and whose output is a `*Config`. The
config store and the disk only ever hold encrypted bytes; the plaintext exists
solely as the decoded struct, and no plaintext-shaped intermediate is created for
anything to leak.

`SecretBox` is the portable secrets port. It wraps a `*secrets.Keeper` and exposes
`Encrypt`/`Decrypt`, wrapping the Keeper's errors with package sentinels
(`ErrEncrypt`, `ErrDecrypt`) so callers can branch with `errors.Is`. The Keeper
itself is chosen by URL exactly like a Variable: `localsecrets.NewKeeper` (or a
`base64key://` URL) in tests and dev, `awskms://` / `gcpkms://` /
`azurekeyvault://` in production, with no change to `Encrypt`/`Decrypt`.
`localsecrets` is symmetric AES-256 with a 32-byte in-process key: fine for tests,
never a production KMS — it has no rotation, no audit, and no HSM. `NewRandomKey`
gives a fresh key for tests; `Base64Key` decodes a URL-safe base64 string that
must be exactly 32 bytes.

Because `secrets` uses authenticated encryption, `Decrypt` of tampered or garbage
ciphertext fails rather than returning wrong plaintext. The test asserts that,
which is why `Decrypt` wraps the failure in `ErrDecrypt`.

Create `secretbox.go`:

```go
package envelopeconfig

import (
	"context"
	"errors"
	"fmt"

	"gocloud.dev/runtimevar"
	"gocloud.dev/secrets"
)

// Sentinel errors let callers branch on failure category with errors.Is.
var (
	ErrEncrypt        = errors.New("secretbox: encrypt failed")
	ErrDecrypt        = errors.New("secretbox: decrypt failed")
	ErrUnexpectedType = errors.New("secretbox: unexpected snapshot type")
)

// Config is the sensitive application configuration stored as ciphertext.
type Config struct {
	DBPassword string `json:"db_password"`
	APIKey     string `json:"api_key"`
}

// SecretBox is a portable encrypt/decrypt port over a secrets.Keeper. The
// concrete KMS is chosen by how the Keeper was opened (localsecrets, awskms, ...).
type SecretBox struct {
	k *secrets.Keeper
}

// NewSecretBox wraps an already-opened Keeper.
func NewSecretBox(k *secrets.Keeper) *SecretBox {
	return &SecretBox{k: k}
}

// Encrypt returns ciphertext for plaintext.
func (b *SecretBox) Encrypt(ctx context.Context, plaintext []byte) ([]byte, error) {
	ct, err := b.k.Encrypt(ctx, plaintext)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrEncrypt, err)
	}
	return ct, nil
}

// Decrypt returns the plaintext for ciphertext. Authenticated encryption means a
// tampered or garbage ciphertext fails here rather than returning wrong bytes.
func (b *SecretBox) Decrypt(ctx context.Context, ciphertext []byte) ([]byte, error) {
	pt, err := b.k.Decrypt(ctx, ciphertext)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDecrypt, err)
	}
	return pt, nil
}

// Close releases the Keeper.
func (b *SecretBox) Close() error {
	return b.k.Close()
}

// NewEncryptedDecoder decodes ciphertext into a *Config: the Keeper decrypts the
// raw bytes, then JSONDecode parses the plaintext. Plaintext never touches disk.
func NewEncryptedDecoder(k *secrets.Keeper) *runtimevar.Decoder {
	return runtimevar.NewDecoder(&Config{}, runtimevar.DecryptDecode(k, runtimevar.JSONDecode))
}

// EncryptedLoader reads typed config from a variable whose bytes are ciphertext.
type EncryptedLoader struct {
	v *runtimevar.Variable
}

// NewEncryptedLoader wraps a Variable built with NewEncryptedDecoder.
func NewEncryptedLoader(v *runtimevar.Variable) *EncryptedLoader {
	return &EncryptedLoader{v: v}
}

// Load returns the latest decrypted, typed configuration.
func (l *EncryptedLoader) Load(ctx context.Context) (*Config, error) {
	snap, err := l.v.Latest(ctx)
	if err != nil {
		return nil, fmt.Errorf("load latest config: %w", err)
	}
	cfg, ok := snap.Value.(*Config)
	if !ok {
		return nil, fmt.Errorf("%w: got %T", ErrUnexpectedType, snap.Value)
	}
	return cfg, nil
}

// Close releases the backing variable.
func (l *EncryptedLoader) Close() error {
	return l.v.Close()
}
```

### The runnable demo

The demo uses a deterministic `base64key://` keeper so its output is reproducible.
It encrypts a JSON config, feeds the ciphertext to a `constant://`-style in-memory
variable via `constantvar.NewBytes` with the encrypted decoder, and loads it back
as a typed `*Config`. Swapping the `base64key://...` URL for `awskms://...` is the
only change needed to run against a real KMS.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"example.com/envelopeconfig"

	"gocloud.dev/runtimevar/constantvar"
	"gocloud.dev/secrets"
	_ "gocloud.dev/secrets/localsecrets"
)

// Deterministic 32-byte key (URL-safe base64) for a reproducible demo only.
const keyURL = "base64key://AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	keeper, err := secrets.OpenKeeper(ctx, keyURL)
	if err != nil {
		log.Fatalf("open keeper: %v", err)
	}
	box := envelopeconfig.NewSecretBox(keeper)
	defer box.Close()

	plain := `{"db_password":"s3cr3t","api_key":"ak_live_123"}`
	ciphertext, err := box.Encrypt(ctx, []byte(plain))
	if err != nil {
		log.Fatalf("encrypt: %v", err)
	}
	fmt.Printf("stored bytes are ciphertext: %v\n", string(ciphertext) != plain)

	// The config store holds only ciphertext; the decoder decrypts before parsing.
	v := constantvar.NewBytes(ciphertext, envelopeconfig.NewEncryptedDecoder(keeper))
	loader := envelopeconfig.NewEncryptedLoader(v)
	defer loader.Close()

	cfg, err := loader.Load(ctx)
	if err != nil {
		log.Fatalf("load: %v", err)
	}
	fmt.Printf("db_password: %s\n", cfg.DBPassword)
	fmt.Printf("api_key: %s\n", cfg.APIKey)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
stored bytes are ciphertext: true
db_password: s3cr3t
api_key: ak_live_123
```

### Tests

`TestRoundTrip` proves the secrets port: it encrypts plaintext, asserts the
ciphertext differs from the plaintext, and asserts `Decrypt(Encrypt(pt)) == pt`.
`TestDecryptTampered` proves authenticated encryption: decrypting garbage returns
`ErrDecrypt` via `errors.Is` rather than silent wrong bytes. `TestEncryptedLoader`
proves the integration: it encrypts a JSON config, feeds the ciphertext to
`constantvar.NewBytes` with `NewEncryptedDecoder`, and asserts `Load` returns the
right typed struct — decryption then parsing, all inside the decoder.

Create `secretbox_test.go`:

```go
package envelopeconfig

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"

	"gocloud.dev/runtimevar/constantvar"
	"gocloud.dev/secrets/localsecrets"
)

func newTestBox(t *testing.T) *SecretBox {
	t.Helper()
	key, err := localsecrets.NewRandomKey()
	if err != nil {
		t.Fatalf("NewRandomKey: %v", err)
	}
	box := NewSecretBox(localsecrets.NewKeeper(key))
	t.Cleanup(func() { box.Close() })
	return box
}

func TestRoundTrip(t *testing.T) {
	t.Parallel()
	box := newTestBox(t)
	ctx := context.Background()

	plaintext := []byte("correct horse battery staple")
	ciphertext, err := box.Encrypt(ctx, plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if bytes.Equal(ciphertext, plaintext) {
		t.Fatal("ciphertext equals plaintext; encryption did nothing")
	}

	got, err := box.Decrypt(ctx, ciphertext)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("Decrypt round-trip = %q, want %q", got, plaintext)
	}
}

func TestDecryptTampered(t *testing.T) {
	t.Parallel()
	box := newTestBox(t)

	_, err := box.Decrypt(context.Background(), []byte("this is not valid ciphertext"))
	if !errors.Is(err, ErrDecrypt) {
		t.Fatalf("Decrypt error = %v, want ErrDecrypt", err)
	}
}

func TestEncryptedLoader(t *testing.T) {
	t.Parallel()
	key, err := localsecrets.NewRandomKey()
	if err != nil {
		t.Fatalf("NewRandomKey: %v", err)
	}
	keeper := localsecrets.NewKeeper(key)
	t.Cleanup(func() { keeper.Close() })

	box := NewSecretBox(keeper)
	ctx := context.Background()
	plain := `{"db_password":"s3cr3t","api_key":"ak_live_123"}`
	ciphertext, err := box.Encrypt(ctx, []byte(plain))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	v := constantvar.NewBytes(ciphertext, NewEncryptedDecoder(keeper))
	loader := NewEncryptedLoader(v)
	t.Cleanup(func() { loader.Close() })

	cfg, err := loader.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DBPassword != "s3cr3t" {
		t.Errorf("DBPassword = %q, want %q", cfg.DBPassword, "s3cr3t")
	}
	if cfg.APIKey != "ak_live_123" {
		t.Errorf("APIKey = %q, want %q", cfg.APIKey, "ak_live_123")
	}
}

func Example() {
	key, _ := localsecrets.NewRandomKey()
	box := NewSecretBox(localsecrets.NewKeeper(key))
	defer box.Close()

	ctx := context.Background()
	ciphertext, _ := box.Encrypt(ctx, []byte("hello"))
	plaintext, _ := box.Decrypt(ctx, ciphertext)
	fmt.Println(string(plaintext))
	// Output: hello
}
```

## Review

The design is correct when ciphertext is all that is ever stored and the plaintext
appears only as a decoded struct. `TestEncryptedLoader` confirms the chained
decoder decrypts before it parses: the variable holds ciphertext, yet `Load`
returns a populated `*Config`. `TestRoundTrip` and `TestDecryptTampered` confirm
the secrets port: encryption changes the bytes, the round-trip recovers them, and a
tampered ciphertext fails closed with `ErrDecrypt` instead of yielding garbage.

The mistakes to avoid are the ones the tests exist to prevent. Do not store
plaintext and decrypt after decoding — chain `DecryptDecode(keeper, JSONDecode)` so
decryption precedes parsing and no plaintext touches the config store. Do not
ignore a `Decrypt` error and use the bytes anyway; authenticated encryption
guarantees failure on tampering, and the sentinel lets you branch. Do not ship a
committed `base64key://` to production — `localsecrets` has no rotation, audit, or
HSM, so change the URL to a real KMS (`awskms://`, `gcpkms://`,
`azurekeyvault://`) there, and make sure any `Base64Key` string decodes to exactly
32 bytes. Run `go test -count=1 -race ./...` to confirm the round-trip and the
integrated loader together.

## Resources

- [`gocloud.dev/secrets`](https://pkg.go.dev/gocloud.dev/secrets) — `OpenKeeper`, `Keeper.Encrypt`/`Decrypt`/`Close`.
- [`gocloud.dev/secrets/localsecrets`](https://pkg.go.dev/gocloud.dev/secrets/localsecrets) — `NewKeeper`, `NewRandomKey`, `Base64Key`, and the `base64key://` scheme.
- [`runtimevar.DecryptDecode`](https://pkg.go.dev/gocloud.dev/runtimevar#DecryptDecode) — composing a Keeper with a post-decoder for encrypted config.
- [Go CDK: secrets how-to](https://gocloud.dev/howto/secrets/) — encrypting and decrypting portably across KMS backends.

---

Back to [02-hot-reload-config-watcher.md](02-hot-reload-config-watcher.md) | Next: [../08-redis-distributed-cache/00-concepts.md](../08-redis-distributed-cache/00-concepts.md)
