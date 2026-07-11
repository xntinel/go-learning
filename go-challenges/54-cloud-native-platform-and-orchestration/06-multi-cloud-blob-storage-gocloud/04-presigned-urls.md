# Exercise 4: Presigning URLs to Offload the Data Plane

A signed URL lets a client upload or download an object directly, so multi-gigabyte
bytes never pass through your service. This exercise builds a signed-URL issuer on
`fileblob`'s HMAC signer — a fully offline stand-in for the S3, GCS, and Azure
presigners — and documents the portability gap where `memblob` cannot sign at all.

## What you'll build

```text
presigner/                     independent module: example.com/presigner
  go.mod                       go 1.26; requires gocloud.dev
  presign.go                   OpenSignedBucket (fileblob+HMAC); Issuer; DownloadURL; UploadURL
  cmd/
    demo/
      main.go                  issues a short-lived download URL and prints its parts
  presign_test.go              GET URL parses+carries params, PUT differs, memblob->Unimplemented
```

Files: `presign.go`, `cmd/demo/main.go`, `presign_test.go`.
Implement: `OpenSignedBucket` (a `fileblob` bucket configured with an HMAC `URLSigner`) and an `Issuer` with `DownloadURL` (GET) and `UploadURL` (PUT) that call `SignedURL`.
Test: a GET signed URL parses as a `net/url.URL` and carries `obj`, `expiry`, and `signature` query params; a PUT URL differs from the GET URL (different `method`); calling `SignedURL` on a `memblob` bucket returns an error whose `gcerrors.Code` is `gcerrors.Unimplemented`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/presigner/cmd/demo
cd ~/go-exercises/presigner
go mod init example.com/presigner
go get gocloud.dev/blob@latest
go mod edit -go=1.26
```

### Why signed URLs, and why this is a portability seam

The default data path proxies bytes: a client uploads to your service, your
service writes to the bucket. For large objects that is wasteful — you pay for
the bandwidth twice and tie up a request goroutine for the whole transfer.
`SignedURL(ctx, key, opts)` returns a time-limited, method-scoped URL the client
uses to talk to the storage provider directly; your service only hands out the
URL. `SignedURLOptions.Method` scopes it to `GET` (download), `PUT` (upload), or
`DELETE`, and `Expiry` bounds its lifetime (default `blob.DefaultSignedURLExpiry`,
one hour). A GET URL cannot be used to upload, and it stops working after it
expires.

But signing is exactly where the portable abstraction is thinnest, so it is a seam
to reason about rather than assume. `memblob` has no way to serve a URL, so its
`SignedURL` returns `gcerrors.Unimplemented`. `fileblob` can sign, but only if you
configure it with a `URLSigner` and stand up an HTTP endpoint that verifies and
serves the signed requests. Each cloud provider emits a differently shaped URL
with different query parameters. The lesson uses `fileblob`'s HMAC signer because
it is entirely offline and deterministic, which makes it a faithful teaching
stand-in for the cloud presigners without any network or credentials.

### The fileblob HMAC signer

`fileblob.NewURLSignerHMAC(baseURL, secretKey)` builds a signer that produces
URLs rooted at `baseURL` with an HMAC-SHA256 signature over the request. You pass
it to `fileblob.OpenBucket` through `Options.URLSigner`; from then on the bucket's
`SignedURL` returns signed URLs instead of an error. The signer encodes the
object key, expiry, and method as query parameters and appends a `signature`
parameter it can later verify in constant time — so a client cannot tamper with
the key or extend the expiry without invalidating the signature. The generated URL
carries these query parameters: `obj` (the object key), `expiry` (a Unix
timestamp), `method` (the HTTP method), an optional `contentType`, and
`signature`. The `baseURL` is the address of the endpoint you would run to serve
these requests; in a real deployment that endpoint calls `KeyFromURL` to validate
the signature and expiry before streaming the object.

Create `presign.go`:

```go
// presign.go
package presigner

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"gocloud.dev/blob"
	"gocloud.dev/blob/fileblob"
)

// OpenSignedBucket opens a fileblob bucket rooted at dir that can sign URLs
// rooted at base, using secret as the HMAC key. CreateDir makes the directory
// if it does not exist.
func OpenSignedBucket(dir string, base *url.URL, secret []byte) (*blob.Bucket, error) {
	signer := fileblob.NewURLSignerHMAC(base, secret)
	b, err := fileblob.OpenBucket(dir, &fileblob.Options{
		URLSigner: signer,
		CreateDir: true,
	})
	if err != nil {
		return nil, fmt.Errorf("open signed bucket at %q: %w", dir, err)
	}
	return b, nil
}

// Issuer hands out time-limited, method-scoped URLs so clients transfer bytes
// directly with the storage provider instead of through this service.
type Issuer struct {
	bucket *blob.Bucket
}

// NewIssuer wraps a bucket that supports signing.
func NewIssuer(b *blob.Bucket) *Issuer {
	return &Issuer{bucket: b}
}

// DownloadURL returns a GET URL for key that is valid for ttl.
func (i *Issuer) DownloadURL(ctx context.Context, key string, ttl time.Duration) (string, error) {
	u, err := i.bucket.SignedURL(ctx, key, &blob.SignedURLOptions{
		Method: "GET",
		Expiry: ttl,
	})
	if err != nil {
		return "", fmt.Errorf("sign GET %q: %w", key, err)
	}
	return u, nil
}

// UploadURL returns a PUT URL for key, pinned to contentType, valid for ttl.
func (i *Issuer) UploadURL(ctx context.Context, key, contentType string, ttl time.Duration) (string, error) {
	u, err := i.bucket.SignedURL(ctx, key, &blob.SignedURLOptions{
		Method:      "PUT",
		ContentType: contentType,
		Expiry:      ttl,
	})
	if err != nil {
		return "", fmt.Errorf("sign PUT %q: %w", key, err)
	}
	return u, nil
}
```

### The runnable demo

The demo opens a signed fileblob bucket in a temporary directory, writes one
object, and prints a short-lived download URL broken into its path and the object
it points at. Because the HMAC signature is not deterministic across runs (the
`expiry` timestamp changes), the demo prints only the stable parts.

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"os"
	"time"

	"example.com/presigner"
)

func main() {
	ctx := context.Background()
	dir, err := os.MkdirTemp("", "presign-demo-")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)

	base, _ := url.Parse("https://downloads.example.com/files")
	b, err := presigner.OpenSignedBucket(dir, base, []byte("demo-secret-key"))
	if err != nil {
		log.Fatal(err)
	}
	defer b.Close()

	if err := b.WriteAll(ctx, "release/notes.txt", []byte("hello"), nil); err != nil {
		log.Fatal(err)
	}

	iss := presigner.NewIssuer(b)
	signed, err := iss.DownloadURL(ctx, "release/notes.txt", 15*time.Minute)
	if err != nil {
		log.Fatal(err)
	}

	u, err := url.Parse(signed)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("host: %s\n", u.Host)
	fmt.Printf("path: %s\n", u.Path)
	fmt.Printf("object: %s\n", u.Query().Get("obj"))
	fmt.Printf("method: %s\n", u.Query().Get("method"))
	fmt.Printf("signed: %v\n", u.Query().Get("signature") != "")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
host: downloads.example.com
path: /files
object: release/notes.txt
method: GET
signed: true
```

### Tests

`TestDownloadURLParses` issues a GET URL and asserts it parses as a `net/url.URL`
carrying the `obj`, `expiry`, and `signature` query parameters — the proof the
signer ran. `TestUploadDiffersFromDownload` issues both a GET and a PUT URL for
the same key and asserts they differ in the `method` parameter, showing the URL is
method-scoped. `TestMemblobUnimplemented` calls `SignedURL` on a `memblob` bucket
and asserts the error's `gcerrors.Code` is `gcerrors.Unimplemented`, documenting
the portability gap directly in an assertion. An `Example` prints whether a signed
URL is method-scoped.

Create `presign_test.go`:

```go
// presign_test.go
package presigner

import (
	"context"
	"fmt"
	"net/url"
	"testing"
	"time"

	"gocloud.dev/blob"
	"gocloud.dev/blob/memblob"
	"gocloud.dev/gcerrors"
)

func newSignedIssuer(t *testing.T) *Issuer {
	t.Helper()
	base, err := url.Parse("https://files.example.com/d")
	if err != nil {
		t.Fatalf("parse base: %v", err)
	}
	b, err := OpenSignedBucket(t.TempDir(), base, []byte("test-secret"))
	if err != nil {
		t.Fatalf("OpenSignedBucket: %v", err)
	}
	t.Cleanup(func() { b.Close() })
	if err := b.WriteAll(t.Context(), "file.txt", []byte("x"), nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return NewIssuer(b)
}

func TestDownloadURLParses(t *testing.T) {
	t.Parallel()
	iss := newSignedIssuer(t)

	raw, err := iss.DownloadURL(t.Context(), "file.txt", 10*time.Minute)
	if err != nil {
		t.Fatalf("DownloadURL: %v", err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("signed URL does not parse: %v", err)
	}
	q := u.Query()
	for _, param := range []string{"obj", "expiry", "signature"} {
		if q.Get(param) == "" {
			t.Errorf("signed URL missing %q param: %s", param, raw)
		}
	}
	if q.Get("obj") != "file.txt" {
		t.Errorf("obj = %q, want file.txt", q.Get("obj"))
	}
}

func TestUploadDiffersFromDownload(t *testing.T) {
	t.Parallel()
	iss := newSignedIssuer(t)

	get, err := iss.DownloadURL(t.Context(), "file.txt", time.Minute)
	if err != nil {
		t.Fatalf("DownloadURL: %v", err)
	}
	put, err := iss.UploadURL(t.Context(), "file.txt", "text/plain", time.Minute)
	if err != nil {
		t.Fatalf("UploadURL: %v", err)
	}
	if get == put {
		t.Fatal("GET and PUT signed URLs are identical; they must be method-scoped")
	}

	gm := mustQuery(t, get).Get("method")
	pm := mustQuery(t, put).Get("method")
	if gm != "GET" || pm != "PUT" {
		t.Fatalf("methods = %q/%q, want GET/PUT", gm, pm)
	}
}

func TestMemblobUnimplemented(t *testing.T) {
	t.Parallel()
	b := memblob.OpenBucket(nil)
	t.Cleanup(func() { b.Close() })

	_, err := b.SignedURL(t.Context(), "file.txt", &blob.SignedURLOptions{Method: "GET"})
	if code := gcerrors.Code(err); code != gcerrors.Unimplemented {
		t.Fatalf("memblob SignedURL code = %v, want Unimplemented", code)
	}
}

func mustQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u.Query()
}

func Example() {
	ctx := context.Background()
	base, _ := url.Parse("https://files.example.com/d")
	b, _ := OpenSignedBucket("/tmp/presign-example", base, []byte("secret"))
	defer b.Close()
	_ = b.WriteAll(ctx, "f", []byte("x"), nil)

	iss := NewIssuer(b)
	get, _ := iss.DownloadURL(ctx, "f", time.Minute)
	u, _ := url.Parse(get)
	fmt.Println(u.Query().Get("method"))
	// Output: GET
}
```

## Review

The issuer is correct when a GET URL parses and carries the signer's `obj`,
`expiry`, and `signature` parameters, when GET and PUT URLs differ by their
`method`, and when the code treats signing as a capability that not every driver
has. The mistake to avoid is assuming `SignedURL` works everywhere:
`TestMemblobUnimplemented` pins `memblob`'s `gcerrors.Unimplemented` so a
deployment that swaps to a non-signing driver surfaces the gap as a checked error,
not a panic in production. Do not treat the HMAC secret as anything but a real
secret — a leaked key lets anyone mint valid URLs for any object. Do not hard-code
a long expiry "to be safe"; a signed URL is a bearer credential and its lifetime
is its blast radius, so scope it to the shortest window the client needs. The
signed URL is only half the system: a real deployment also runs the endpoint at
`baseURL` that validates the signature and expiry before serving bytes. Run
`go test -count=1 -race ./...`.

## Resources

- [gocloud.dev/blob/fileblob](https://pkg.go.dev/gocloud.dev/blob/fileblob) — `Options`, `NewURLSignerHMAC`, and `OpenBucket`.
- [gocloud.dev/blob SignedURL](https://pkg.go.dev/gocloud.dev/blob#Bucket.SignedURL) — `SignedURLOptions`, `Method`, `Expiry`, and `DefaultSignedURLExpiry`.
- [Go CDK How-To: Blob storage](https://gocloud.dev/howto/blob/) — the driver support matrix for signed URLs.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-conditional-writes-and-idempotency.md](03-conditional-writes-and-idempotency.md) | Next: [../07-cloud-config-and-secrets-portability/00-concepts.md](../07-cloud-config-and-secrets-portability/00-concepts.md)
