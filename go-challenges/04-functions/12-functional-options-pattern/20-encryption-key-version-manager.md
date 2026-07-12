# Exercise 20: Encryption Key Rotator With Version Precedence and Deprecation

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A key-rotation manager holds several AES key versions at once: one active
version encrypts new data, and older versions must still decrypt data they
originally encrypted for a while after being rotated out. This module builds
that manager through options, checking that the active version genuinely
exists among the registered keys, and that the window during which a
rotated-out key can still decrypt never outlives the window during which its
material is kept in memory at all.

## What you'll build

```text
keyrotator/                      independent module: example.com/keyrotator
  go.mod                         go 1.24
  keyrotator.go                  Manager, Option, New, WithKey, WithActiveVersion,
                                  WithRingSize, WithDeprecationWindow,
                                  WithRetentionWindow, WithClock, Rotate, Sweep,
                                  Encrypt, Decrypt
  cmd/
    demo/
      main.go                    manual clock drives rotate, decrypt-while-deprecated, and sweep
  keyrotator_test.go              table test over option combos plus rotate/decrypt/sweep behavior
```

- Files: `keyrotator.go`, `cmd/demo/main.go`, `keyrotator_test.go`.
- Implement: `New(opts ...Option) (*Manager, error)` whose `Encrypt`/`Decrypt` use real AES-256-GCM, whose `Rotate` starts a deprecation clock on the outgoing version, and whose `Sweep` deletes key material once its retention window elapses.
- Test: every registration/validation combination, a full encrypt-rotate-decrypt-expire-sweep sequence against an injected clock, rotating to an unregistered version, and a concurrency check.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/12-functional-options-pattern/20-encryption-key-version-manager/cmd/demo
cd go-solutions/04-functions/12-functional-options-pattern/20-encryption-key-version-manager
go mod edit -go=1.24
```

### The active version must actually exist

`WithKey(version, material)` registers key material; `WithActiveVersion(version)`
picks which one encrypts new data. These are independent options — a caller
could name an active version that was never registered, or register keys
and forget to pick one at all. `New` checks both failure modes after every
option has run: no key registered under the chosen active version, or no
active version chosen at all. Neither option's closure can see the other's
argument, so this is, once again, a check that belongs in the constructor.

### Two clocks, two purposes

`Rotate(newVersion)` switches which version encrypts new data and starts a
deprecation clock on the version being replaced. From that moment, two
durations govern the outgoing version: `deprecationWindow` (how long it can
still decrypt data) and `retentionWindow` (how long its raw key material
stays in memory at all, for audit purposes, even after it can no longer
decrypt anything). The deprecation window must never exceed the retention
window — otherwise a key would be asked to decrypt something after its
material had already been deleted, which is a contradiction the constructor
rejects up front. `Sweep()` is the method that actually deletes material past
retention; `Decrypt` checks deprecation without needing a sweep to have run.

### Real AES-256-GCM, not a placeholder

`WithKey` requires exactly 32 bytes of material (AES-256) and `Encrypt`/`Decrypt`
use `crypto/aes` and `crypto/cipher`'s GCM mode for real, generating a fresh
random nonce per call via `crypto/rand`. This is what makes
`TestEncryptRotateDecrypt` a genuine round-trip test rather than a check
against a stub.

Create `keyrotator.go`:

```go
package keyrotator

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"fmt"
	"io"
	"sync"
	"time"
)

type versionEntry struct {
	material     []byte
	deprecated   bool
	deprecatedAt time.Time
}

// Manager is a concurrency-safe AES-256-GCM key rotator with version
// precedence and a two-tier deprecation/retention expiry.
type Manager struct {
	mu                sync.Mutex
	ringSize          int
	deprecationWindow time.Duration
	retentionWindow   time.Duration
	now               func() time.Time
	activeVersion     int
	activeVersionSet  bool
	keys              map[int]*versionEntry
}

// Option configures a Manager and may reject invalid input.
type Option func(*Manager) error

// New seeds defaults, applies opts in order, then validates the invariants
// no single option could see: the active version must exist among the
// registered keys, the registered key count must fit the ring size, and the
// deprecation window (when decryption stops) must not exceed the retention
// window (when key material is finally deleted).
func New(opts ...Option) (*Manager, error) {
	m := &Manager{
		ringSize:          5,
		deprecationWindow: 24 * time.Hour,
		retentionWindow:   7 * 24 * time.Hour,
		now:               time.Now,
		keys:              make(map[int]*versionEntry),
	}
	for _, opt := range opts {
		if err := opt(m); err != nil {
			return nil, err
		}
	}

	if len(m.keys) == 0 {
		return nil, fmt.Errorf("at least one key must be registered via WithKey")
	}
	if len(m.keys) > m.ringSize {
		return nil, fmt.Errorf("%d registered key versions exceed ring size %d", len(m.keys), m.ringSize)
	}
	if !m.activeVersionSet {
		return nil, fmt.Errorf("active version must be set via WithActiveVersion")
	}
	if _, ok := m.keys[m.activeVersion]; !ok {
		return nil, fmt.Errorf("active version %d not found among registered keys", m.activeVersion)
	}
	if m.deprecationWindow > m.retentionWindow {
		return nil, fmt.Errorf("deprecation window %s exceeds retention window %s", m.deprecationWindow, m.retentionWindow)
	}
	return m, nil
}

// WithKey registers a 32-byte AES-256 key under version (> 0).
func WithKey(version int, material []byte) Option {
	return func(m *Manager) error {
		if version <= 0 {
			return fmt.Errorf("version must be positive, got %d", version)
		}
		if len(material) != 32 {
			return fmt.Errorf("key material for version %d must be 32 bytes (AES-256), got %d", version, len(material))
		}
		cp := make([]byte, 32)
		copy(cp, material)
		m.keys[version] = &versionEntry{material: cp}
		return nil
	}
}

// WithActiveVersion selects which registered version encrypts new data.
func WithActiveVersion(version int) Option {
	return func(m *Manager) error {
		if version <= 0 {
			return fmt.Errorf("active version must be positive, got %d", version)
		}
		m.activeVersion = version
		m.activeVersionSet = true
		return nil
	}
}

// WithRingSize caps how many key versions the manager may hold at once.
func WithRingSize(n int) Option {
	return func(m *Manager) error {
		if n < 1 {
			return fmt.Errorf("ring size must be >= 1, got %d", n)
		}
		m.ringSize = n
		return nil
	}
}

// WithDeprecationWindow sets how long a rotated-out version still decrypts.
func WithDeprecationWindow(d time.Duration) Option {
	return func(m *Manager) error {
		if d <= 0 {
			return fmt.Errorf("deprecation window must be positive, got %s", d)
		}
		m.deprecationWindow = d
		return nil
	}
}

// WithRetentionWindow sets how long a rotated-out version's material is kept
// in memory at all, even once it can no longer decrypt.
func WithRetentionWindow(d time.Duration) Option {
	return func(m *Manager) error {
		if d <= 0 {
			return fmt.Errorf("retention window must be positive, got %s", d)
		}
		m.retentionWindow = d
		return nil
	}
}

// WithClock injects the clock used to time deprecation and retention.
func WithClock(now func() time.Time) Option {
	return func(m *Manager) error {
		if now == nil {
			return fmt.Errorf("clock is nil")
		}
		m.now = now
		return nil
	}
}

// ActiveVersion reports the version currently used to encrypt.
func (m *Manager) ActiveVersion() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.activeVersion
}

// Rotate switches the active version to newVersion, which must already be
// registered, and starts the deprecation clock on the version being
// replaced.
func (m *Manager) Rotate(newVersion int) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.keys[newVersion]; !ok {
		return fmt.Errorf("cannot rotate to unregistered version %d", newVersion)
	}
	if newVersion == m.activeVersion {
		return nil
	}
	old := m.keys[m.activeVersion]
	old.deprecated = true
	old.deprecatedAt = m.now()
	m.activeVersion = newVersion
	return nil
}

// Sweep deletes key material for any version whose retention window has
// elapsed since it was deprecated. Returns the versions removed.
func (m *Manager) Sweep() []int {
	m.mu.Lock()
	defer m.mu.Unlock()

	var removed []int
	now := m.now()
	for v, e := range m.keys {
		if e.deprecated && now.Sub(e.deprecatedAt) > m.retentionWindow {
			delete(m.keys, v)
			removed = append(removed, v)
		}
	}
	return removed
}

// Encrypt seals plaintext under the active version's key, returning the
// version used and the nonce-prefixed ciphertext.
func (m *Manager) Encrypt(plaintext []byte) (version int, ciphertext []byte, err error) {
	m.mu.Lock()
	active := m.keys[m.activeVersion]
	version = m.activeVersion
	m.mu.Unlock()

	gcm, err := newGCM(active.material)
	if err != nil {
		return 0, nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return 0, nil, fmt.Errorf("generate nonce: %w", err)
	}
	ciphertext = gcm.Seal(nonce, nonce, plaintext, nil)
	return version, ciphertext, nil
}

// Decrypt opens ciphertext with the given version's key. It fails if the
// version was never registered, was deprecated beyond the deprecation
// window, or was already swept past the retention window.
func (m *Manager) Decrypt(version int, ciphertext []byte) ([]byte, error) {
	m.mu.Lock()
	entry, ok := m.keys[version]
	if !ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("key version %d is not registered or was swept after retention expired", version)
	}
	if entry.deprecated && m.now().Sub(entry.deprecatedAt) > m.deprecationWindow {
		m.mu.Unlock()
		return nil, fmt.Errorf("key version %d was deprecated beyond the deprecation window", version)
	}
	material := entry.material
	m.mu.Unlock()

	gcm, err := newGCM(material)
	if err != nil {
		return nil, err
	}
	nonceSize := gcm.NonceSize()
	if len(ciphertext) < nonceSize {
		return nil, fmt.Errorf("ciphertext too short")
	}
	nonce, sealed := ciphertext[:nonceSize], ciphertext[nonceSize:]
	return gcm.Open(nil, nonce, sealed, nil)
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("new gcm: %w", err)
	}
	return gcm, nil
}
```

### The runnable demo

The demo encrypts under version 1, rotates to version 2, shows version 1
still decrypting right after rotation (it is deprecated but not yet past its
window), then advances the clock past deprecation and shows decryption
failing, and finally past retention and shows `Sweep` deleting it.

Create `cmd/demo/main.go`:

```go
package main

import (
	"bytes"
	"fmt"
	"time"

	"example.com/keyrotator"
)

func key(b byte) []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = b
	}
	return k
}

func main() {
	current := time.Unix(0, 0).UTC()
	clock := func() time.Time { return current }

	m, err := keyrotator.New(
		keyrotator.WithKey(1, key(0x01)),
		keyrotator.WithKey(2, key(0x02)),
		keyrotator.WithActiveVersion(1),
		keyrotator.WithDeprecationWindow(time.Hour),
		keyrotator.WithRetentionWindow(2*time.Hour),
		keyrotator.WithClock(clock),
	)
	if err != nil {
		panic(err)
	}

	version, ciphertext, err := m.Encrypt([]byte("top secret"))
	if err != nil {
		panic(err)
	}
	fmt.Printf("encrypted with version: %d\n", version)

	if err := m.Rotate(2); err != nil {
		panic(err)
	}
	fmt.Printf("active version after rotate: %d\n", m.ActiveVersion())

	plaintext, err := m.Decrypt(version, ciphertext)
	if err != nil {
		panic(err)
	}
	fmt.Printf("decrypted with deprecated v1 key: %s\n", plaintext)
	fmt.Printf("matches original: %t\n", bytes.Equal(plaintext, []byte("top secret")))

	current = current.Add(90 * time.Minute) // past deprecation, before retention
	_, err = m.Decrypt(version, ciphertext)
	fmt.Printf("decrypt after deprecation window: %v\n", err)

	current = current.Add(2 * time.Hour) // now past retention too
	removed := m.Sweep()
	fmt.Printf("swept versions: %v\n", removed)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
encrypted with version: 1
active version after rotate: 2
decrypted with deprecated v1 key: top secret
matches original: true
decrypt after deprecation window: key version 1 was deprecated beyond the deprecation window
swept versions: [1]
```

### Tests

`TestNewValidation` tables no-keys, unregistered active version, no active
version, ring-size overflow, and the deprecation/retention boundary (equal
is allowed, exceeding is not). `TestEncryptRotateDecrypt` drives the full
round trip against a fake clock, proving decryption still works right after
rotation and fails once the deprecation window passes.
`TestRotateToUnregisteredVersionFails` and `TestSweepRemovesOnlyPastRetention`
cover the remaining edges. `TestConcurrentEncrypt` runs `-race` over
concurrent encryptions.

Create `keyrotator_test.go`:

```go
package keyrotator

import (
	"bytes"
	"sync"
	"testing"
	"time"
)

func k(b byte) []byte {
	out := make([]byte, 32)
	for i := range out {
		out[i] = b
	}
	return out
}

func TestNewValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		opts    []Option
		wantErr bool
	}{
		{name: "no keys registered", opts: []Option{WithActiveVersion(1)}, wantErr: true},
		{name: "active version not registered", opts: []Option{
			WithKey(1, k(1)), WithActiveVersion(2),
		}, wantErr: true},
		{name: "active version not set", opts: []Option{WithKey(1, k(1))}, wantErr: true},
		{name: "valid single key", opts: []Option{WithKey(1, k(1)), WithActiveVersion(1)}},
		{name: "too many keys for ring size", opts: []Option{
			WithKey(1, k(1)), WithKey(2, k(2)), WithKey(3, k(3)),
			WithActiveVersion(1), WithRingSize(2),
		}, wantErr: true},
		{name: "deprecation window exceeds retention window", opts: []Option{
			WithKey(1, k(1)), WithActiveVersion(1),
			WithDeprecationWindow(2 * time.Hour), WithRetentionWindow(time.Hour),
		}, wantErr: true},
		{name: "deprecation window equal to retention window is allowed", opts: []Option{
			WithKey(1, k(1)), WithActiveVersion(1),
			WithDeprecationWindow(time.Hour), WithRetentionWindow(time.Hour),
		}},
		{name: "wrong key material length", opts: []Option{
			WithKey(1, []byte("too-short")), WithActiveVersion(1),
		}, wantErr: true},
		{name: "nil clock rejected", opts: []Option{
			WithKey(1, k(1)), WithActiveVersion(1), WithClock(nil),
		}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := New(tt.opts...)
			if tt.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestEncryptRotateDecrypt(t *testing.T) {
	t.Parallel()

	base := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	current := base

	m, err := New(
		WithKey(1, k(1)), WithKey(2, k(2)),
		WithActiveVersion(1),
		WithDeprecationWindow(time.Hour),
		WithRetentionWindow(2*time.Hour),
		WithClock(func() time.Time { return current }),
	)
	if err != nil {
		t.Fatal(err)
	}

	version, ciphertext, err := m.Encrypt([]byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if version != 1 {
		t.Fatalf("encrypted with version %d, want 1", version)
	}

	if err := m.Rotate(2); err != nil {
		t.Fatal(err)
	}
	if m.ActiveVersion() != 2 {
		t.Fatalf("ActiveVersion() = %d, want 2", m.ActiveVersion())
	}

	plaintext, err := m.Decrypt(version, ciphertext)
	if err != nil {
		t.Fatalf("decrypt with recently deprecated version: %v", err)
	}
	if !bytes.Equal(plaintext, []byte("hello")) {
		t.Fatalf("plaintext = %q, want hello", plaintext)
	}

	current = base.Add(90 * time.Minute) // past deprecation window
	if _, err := m.Decrypt(version, ciphertext); err == nil {
		t.Fatal("expected decrypt to fail past the deprecation window")
	}
}

func TestRotateToUnregisteredVersionFails(t *testing.T) {
	t.Parallel()

	m, err := New(WithKey(1, k(1)), WithActiveVersion(1))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Rotate(99); err == nil {
		t.Fatal("expected error rotating to an unregistered version")
	}
}

func TestSweepRemovesOnlyPastRetention(t *testing.T) {
	t.Parallel()

	base := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	current := base

	m, err := New(
		WithKey(1, k(1)), WithKey(2, k(2)),
		WithActiveVersion(1),
		WithDeprecationWindow(time.Hour),
		WithRetentionWindow(2*time.Hour),
		WithClock(func() time.Time { return current }),
	)
	if err != nil {
		t.Fatal(err)
	}

	if err := m.Rotate(2); err != nil {
		t.Fatal(err)
	}

	if got := m.Sweep(); len(got) != 0 {
		t.Fatalf("Sweep() = %v before retention elapsed, want empty", got)
	}

	current = base.Add(3 * time.Hour) // past retention
	removed := m.Sweep()
	if len(removed) != 1 || removed[0] != 1 {
		t.Fatalf("Sweep() = %v, want [1]", removed)
	}

	if _, err := m.Decrypt(1, []byte("anything")); err == nil {
		t.Fatal("expected decrypt of swept version to fail")
	}
}

func TestConcurrentEncrypt(t *testing.T) {
	t.Parallel()

	m, err := New(WithKey(1, k(1)), WithActiveVersion(1))
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := m.Encrypt([]byte("payload")); err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()
}
```

## Review

The rotator is correct when the active version is always a real, registered
key, and when a rotated-out version's decrypt eligibility never outlives its
storage eligibility. `Rotate` and `Sweep` are the two operations that turn
"deprecated" into "gone," each reading the same `deprecatedAt` timestamp
against a different window — this is the general pattern for any resource
with a soft-expiry phase (still usable, degraded) followed by a hard-expiry
phase (deleted): validate at construction that soft expiry can never outlive
hard expiry, then let two independent methods enforce each phase. Building
on real AES-256-GCM rather than a stub is what makes `TestEncryptRotateDecrypt`
prove something: the ciphertext produced under version 1 genuinely only
opens with version 1's key.

## Resources

- [pkg.go.dev: crypto/cipher (AEAD, GCM)](https://pkg.go.dev/crypto/cipher)
- [pkg.go.dev: crypto/aes](https://pkg.go.dev/crypto/aes)
- [NIST SP 800-57: Key Management Recommendations](https://csrc.nist.gov/pubs/sp/800/57/pt1/r5/final)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [19-event-store-compaction-policy.md](19-event-store-compaction-policy.md) | Next: [21-multi-tenant-router-isolation.md](21-multi-tenant-router-isolation.md)
