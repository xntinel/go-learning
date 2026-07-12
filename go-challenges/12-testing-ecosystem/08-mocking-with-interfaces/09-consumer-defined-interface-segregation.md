# Exercise 9: Own the Seam — Narrow Consumer Interfaces Over a Fat SDK Client

You cannot mock a third-party client cheaply if you try to mock its whole surface.
The fix is interface segregation: define your *own* narrow interface in the package
that consumes the client, capturing only the methods you actually call, let the fat
client satisfy it implicitly, and mock the small seam. This module gives you a
twelve-method object-store SDK you do not own and a service that uses exactly two of
those methods — and shows the double stays two lines because the interface does.

This module is fully self-contained: its own `go mod init`, all code inline, its own
demo and tests. No external dependencies.

## What you'll build

```text
blobsvc/                     independent module: example.com/blobsvc
  go.mod                     go 1.26
  sdk/
    objectstore.go           "third-party" fat *Client: Get, Put + 10 more methods (not owned)
  archive/
    archive.go               narrow consumer-defined ObjectStore (Get, Put); Archiver service
    archive_test.go          two-method double; compile-time assertions; roundtrip through the real client
  cmd/
    demo/
      main.go                runnable demo wiring the fat client through the narrow seam
```

- Files: `sdk/objectstore.go`, `archive/archive.go`, `archive/archive_test.go`, `cmd/demo/main.go`.
- Implement: a narrow `ObjectStore` interface (`Get`, `Put`) in the consumer package; an `Archiver` that namespaces keys and delegates to it.
- Test: a two-method hand-rolled double for the seam; `var _ ObjectStore = (*sdk.Client)(nil)` proving the fat client satisfies it; a roundtrip against the real client through the narrow interface.
- Verify: `go test -count=1 -race ./...`

### The fat SDK you do not own

Real object-store SDKs are wide: get, put, delete, list, copy, move, stat, ACLs,
presigned URLs, bucket info, health, close. You did not write it and cannot change
it. If your instinct is to generate a mock of `*sdk.Client` so you can substitute it
in tests, you are committing to a twelve-method double for a service that touches
two of them — every unused method is dead weight, and the mock must be regenerated
whenever the SDK adds a method you never call.

Create `sdk/objectstore.go`:

```go
package sdk

import (
	"context"
	"errors"
)

// ErrNoSuchKey is returned by Get when the key is absent.
var ErrNoSuchKey = errors.New("no such key")

// Client is a wide third-party object-store client. You do not own it and cannot
// shrink it; a service should not mock its whole surface.
type Client struct {
	bucket string
	data   map[string][]byte
}

// NewClient constructs the fat client.
func NewClient(bucket string) *Client {
	return &Client{bucket: bucket, data: make(map[string][]byte)}
}

// Get and Put are the two methods the Archiver actually uses.

func (c *Client) Get(_ context.Context, key string) ([]byte, error) {
	v, ok := c.data[key]
	if !ok {
		return nil, ErrNoSuchKey
	}
	return v, nil
}

func (c *Client) Put(_ context.Context, key string, data []byte) error {
	c.data[key] = data
	return nil
}

// The rest of the surface: real SDKs carry many more methods the Archiver never
// touches. They exist here only to make the interface-segregation point concrete.

func (c *Client) Delete(_ context.Context, key string) error {
	delete(c.data, key)
	return nil
}
func (c *Client) List(_ context.Context, prefix string) ([]string, error) {
	var keys []string
	for k := range c.data {
		if len(prefix) == 0 || (len(k) >= len(prefix) && k[:len(prefix)] == prefix) {
			keys = append(keys, k)
		}
	}
	return keys, nil
}
func (c *Client) Copy(_ context.Context, src, dst string) error   { return nil }
func (c *Client) Move(_ context.Context, src, dst string) error   { return nil }
func (c *Client) Stat(_ context.Context, key string) (int, error) { return len(c.data[key]), nil }
func (c *Client) SetACL(_ context.Context, key, acl string) error { return nil }
func (c *Client) Presign(_ context.Context, key string) (string, error) {
	return "https://example/" + key, nil
}
func (c *Client) BucketInfo(_ context.Context) (string, error) { return c.bucket, nil }
func (c *Client) Health(_ context.Context) error               { return nil }
func (c *Client) Close() error                                 { return nil }
```

### The narrow consumer-defined seam

The `archive` package declares `ObjectStore` with the two methods it needs, and
nothing else. Because Go interfaces are satisfied implicitly, `*sdk.Client` already
implements `ObjectStore` — the consumer never imports the SDK to *declare* the
interface, only at the composition root to *wire* the concrete client. The
`Archiver` namespaces keys under `archive/` and delegates. Note what is absent:
`archive` does not depend on `sdk` at all; the dependency arrow points inward, the
seam is owned by the consumer.

Create `archive/archive.go`:

```go
package archive

import "context"

// ObjectStore is the narrow, consumer-defined seam: exactly the two methods the
// Archiver uses. *sdk.Client satisfies it implicitly; so does a two-method double.
type ObjectStore interface {
	Get(ctx context.Context, key string) ([]byte, error)
	Put(ctx context.Context, key string, data []byte) error
}

// Archiver stores and retrieves documents under a namespaced key.
type Archiver struct {
	store ObjectStore
}

// New injects the narrow store through the constructor.
func New(store ObjectStore) *Archiver {
	return &Archiver{store: store}
}

func key(id string) string { return "archive/" + id }

// Save writes a document under its namespaced key.
func (a *Archiver) Save(ctx context.Context, id string, doc []byte) error {
	return a.store.Put(ctx, key(id), doc)
}

// Load reads a previously archived document.
func (a *Archiver) Load(ctx context.Context, id string) ([]byte, error) {
	return a.store.Get(ctx, key(id))
}
```

### The runnable demo

The demo is the composition root: it is the only place that imports `sdk`, and it
passes the fat `*sdk.Client` where an `ObjectStore` is expected — the implicit
satisfaction in action.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/blobsvc/archive"
	"example.com/blobsvc/sdk"
)

func main() {
	store := sdk.NewClient("my-bucket") // the fat client...
	arch := archive.New(store)          // ...wired through the narrow seam.
	ctx := context.Background()

	if err := arch.Save(ctx, "report", []byte("q3 numbers")); err != nil {
		fmt.Println("save:", err)
		return
	}
	data, err := arch.Load(ctx, "report")
	if err != nil {
		fmt.Println("load:", err)
		return
	}
	fmt.Printf("loaded %d bytes: %s\n", len(data), data)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
loaded 10 bytes: q3 numbers
```

### The two-line double and the tests

Because `ObjectStore` is two methods, `fakeStore` is two methods (plus a captured
argument to make it a spy). That is the whole payoff of segregation, stated as code:
had the `Archiver` depended on `*sdk.Client`, this double would need all twelve. Two
compile-time assertions pin the design — `var _ ObjectStore = (*sdk.Client)(nil)`
proves the real fat client satisfies the narrow seam, and the same for the double.
`TestSaveNamespacesKey` uses the spy to verify the key was namespaced and the data
passed through. `TestRoundTripThroughRealClient` wires the actual `*sdk.Client`
through the narrow interface and proves save-then-load works end to end — the seam
is honest, not just type-compatible.

Create `archive/archive_test.go`:

```go
package archive

import (
	"bytes"
	"context"
	"testing"

	"example.com/blobsvc/sdk"
)

// fakeStore is a two-method double for the two-method seam. It records the Put.
type fakeStore struct {
	putKey  string
	putData []byte
	getData []byte
}

func (f *fakeStore) Put(_ context.Context, key string, data []byte) error {
	f.putKey, f.putData = key, data
	return nil
}
func (f *fakeStore) Get(_ context.Context, key string) ([]byte, error) {
	return f.getData, nil
}

// Compile-time proof: both the fat real client and the tiny double satisfy the
// narrow consumer-defined seam.
var (
	_ ObjectStore = (*sdk.Client)(nil)
	_ ObjectStore = (*fakeStore)(nil)
)

func TestSaveNamespacesKey(t *testing.T) {
	t.Parallel()
	spy := &fakeStore{}
	arch := New(spy)

	doc := []byte("hello")
	if err := arch.Save(context.Background(), "doc1", doc); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if spy.putKey != "archive/doc1" {
		t.Fatalf("put key = %q, want archive/doc1", spy.putKey)
	}
	if !bytes.Equal(spy.putData, doc) {
		t.Fatalf("put data = %q, want %q", spy.putData, doc)
	}
}

func TestLoadReturnsStored(t *testing.T) {
	t.Parallel()
	spy := &fakeStore{getData: []byte("stored")}
	arch := New(spy)

	got, err := arch.Load(context.Background(), "doc1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !bytes.Equal(got, []byte("stored")) {
		t.Fatalf("Load = %q, want stored", got)
	}
}

func TestRoundTripThroughRealClient(t *testing.T) {
	t.Parallel()
	// The fat client, used through the narrow interface, actually works.
	arch := New(sdk.NewClient("bucket"))
	ctx := context.Background()

	if err := arch.Save(ctx, "r1", []byte("payload")); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := arch.Load(ctx, "r1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !bytes.Equal(got, []byte("payload")) {
		t.Fatalf("roundtrip = %q, want payload", got)
	}
}
```

## Review

The design is correct when the consumer owns a two-method interface and the fat SDK
satisfies it without the consumer depending on the SDK anywhere but the composition
root. The compile-time assertions are the load-bearing check: they prove the real
client fits the narrow seam, so the double you test against is a faithful stand-in,
and they prove the double fits too — all without importing the SDK into the
`archive` package's production code. The double stays two lines because the interface
is two methods; that is the generalization of the original lesson's rule that "a mock
is only as good as the interface it implements." The mistake to avoid is mocking a
type you do not own by its whole surface — define the seam you need and mock that.
Run `go test -race`.

## Resources

- [Effective Go: interfaces](https://go.dev/doc/effective_go#interfaces) — implicit interface satisfaction, the mechanism behind consumer-defined seams.
- [Go Code Review Comments: interfaces](https://go.dev/wiki/CodeReviewComments#interfaces) — "define interfaces where they are used," the consumer-side rule.
- [SOLID: the Interface Segregation Principle](https://en.wikipedia.org/wiki/Interface_segregation_principle) — narrow interfaces so clients depend only on what they use.

---

Back to [00-concepts.md](00-concepts.md) | Next: [10-sqlmock-repository-layer.md](10-sqlmock-repository-layer.md)
