# Portable Blob Storage with gocloud.dev — Concepts

Object storage is the one external dependency almost every backend service
touches, and it is also the one most likely to lock you to a single cloud. The
S3 SDK, the GCS SDK, and the Azure Blob SDK each have a different type for a
bucket, a different type for an object handle, a different error taxonomy, and a
different notion of a signed URL. Write your domain logic against any one of them
and you have welded that logic to a provider. `gocloud.dev/blob` (the Go Cloud
Development Kit, "Go CDK") is a ports-and-adapters seam placed exactly at the
object-storage boundary: your code talks to `*blob.Bucket`, and a driver — chosen
by a single URL string plus one blank import — connects that bucket to S3, GCS,
Azure, the local filesystem, or an in-memory fake. This mirrors this repository's
own hexagonal split, where the `domain` crate has zero cloud dependencies and a
provider-specific adapter crate does the cloud-specific work. The senior judgment
this lesson drills is the trade you are making and its hazards: you accept a
lowest-common-denominator API in exchange for provider independence and fast,
hermetic tests, and you must know exactly when and how to drop through the escape
hatches to reach a provider-specific feature without leaking that provider back
into your domain. Read this file once; each of the four exercises that follows is
an independent module that applies one slice of it.

## Concepts

### The portability model: schemes, URLOpeners, and the blank import

A driver registers a `URLOpener` for a URL scheme with the package's
`DefaultURLMux`. `s3blob` registers `s3://`, `gcsblob` registers `gs://`,
`azureblob` registers `azblob://`, `fileblob` registers `file://`, and `memblob`
registers `mem://`. The registration happens in the driver package's `init`
function, which is why you import the driver for its side effect only:

```go
import (
	"gocloud.dev/blob"
	_ "gocloud.dev/blob/s3blob" // registers the s3:// scheme
)

b, err := blob.OpenBucket(ctx, "s3://my-bucket?region=us-east-1")
```

`blob.OpenBucket(ctx, urlstr)` parses the scheme, finds the registered opener,
and returns a `*blob.Bucket`. The single most common failure with the Go CDK is
forgetting the blank import: the code compiles, then panics or errors at runtime
with a message about no driver being registered for the scheme, because nothing
ran the `init` that would have registered it. The URL is data — it can come from
config or an environment variable — so you can move a deployment from S3 to GCS
by changing one string and one import, with no other code change.

For tests you usually skip the URL indirection and call the driver's direct
constructor: `memblob.OpenBucket(nil)` returns an in-memory `*blob.Bucket` with
no error and no network, and `fileblob.OpenBucket(dir, opts)` returns one backed
by a local directory. Those constructors do not require the blank import because
you are naming the driver directly. The URL path is for production wiring; the
direct constructor is for hermetic tests.

### The lowest-common-denominator trade and the escape hatches

The portable surface deliberately omits provider-specific features. There is no
portable way to set an S3 storage class, an SSE-KMS key, a GCS object hold, or an
Azure access tier through `WriterOptions`, because those concepts do not exist on
every provider. This is the price of portability, and it is the right default:
most code does not need them. When you do, the Go CDK gives you typed escape
hatches rather than forcing a fork. `As()` on a bucket, reader, writer, or
attributes lets you assign the underlying provider-specific value to a pointer of
the concrete SDK type; `ErrorAs()` does the same for errors; and the
`BeforeWrite`, `BeforeRead`, `BeforeCopy`, and `BeforeSign` hooks on the option
structs let you mutate the provider's native request just before it is sent. The
discipline is where you use them: at the adapter edge, wrapped inside your
provider-specific package, so the domain keeps talking only to your narrow port.
Reach for `As()` in business logic and you have re-coupled that logic to one
cloud and thrown away the entire abstraction — the casual `As()` is the anti-use.

### The streaming and commit model: Close is the commit point

`NewWriter(ctx, key, opts)` returns a `*blob.Writer` that implements
`io.Writer`. For large objects the writer buffers and, under the hood, performs a
multipart upload governed by `WriterOptions.BufferSize` and `MaxConcurrency`.
The crucial fact is that the bytes you write are not durable until `Close()`
returns `nil`. `Close` is the commit point: it flushes the final buffer,
finalizes the multipart upload, and verifies any checksum. An error from `Close`
means the object did not land, and ignoring that error — or, worse, never calling
`Close` at all — silently loses or truncates data. Every write path must check
the error from `Close`, and the idiom is a named return with a deferred close
that assigns the error, or an explicit `if err := w.Close(); err != nil`.

`WriteAll(ctx, key, p, opts)` and `ReadAll(ctx, key)` are convenience wrappers
that buffer the entire object in memory. They are fine for small, bounded
payloads (a JSON manifest, a small config) and wrong for anything large: a
multi-gigabyte object through `ReadAll` is a multi-gigabyte allocation and an
out-of-memory waiting to happen. For large or unbounded objects use
`NewWriter`/`NewReader`, or the `Upload(ctx, key, r, opts)` and
`Download(ctx, key, w, opts)` helpers that stream between the bucket and an
`io.Reader`/`io.Writer`. For partial reads use
`NewRangeReader(ctx, key, offset, length, opts)` so you fetch only the bytes you
need instead of the whole object.

### Keys are a flat namespace, not a filesystem

A bucket is a flat map from string keys to bytes. There are no real directories,
no atomic rename, and no `filepath` semantics. The slash in `a/b/c.txt` is just a
byte in the key; hierarchy is a convention you simulate. `ListOptions.Prefix`
restricts a listing to keys beginning with a string, and `ListOptions.Delimiter`
(usually `"/"`) rolls up everything below the next delimiter into a single
pseudo-entry whose `IsDir` field is true and whose `Key` ends with the delimiter.
That is how you render one level of a "directory" without there being any
directory. Treating keys as OS paths — running them through `filepath.Join`
(which uses the OS separator and would break on Windows), expecting a rename, or
expecting a real folder to exist — is a category error. Deleting a key that does
not exist is not universally a no-op either: strict providers return a NotFound
error, so a robust delete either tolerates that code or checks existence first.

### Two listing shapes for two consumers

There are two listing APIs because there are two kinds of caller. The stateful
`List(opts)` returns a `*blob.ListIterator`; you call `Next(ctx)` in a loop until
it returns `io.EOF`, which is the normal termination sentinel, not an error to
log. This is the right shape for an in-process loop that runs to completion.

For an HTTP endpoint that must resume across separate requests without holding a
server-side cursor, use `ListPage(ctx, pageToken, pageSize, opts)`. It returns a
slice of at most `pageSize` objects and an opaque `nextPageToken []byte`. You
start from the sentinel `blob.FirstPageToken`, hand the returned token back to the
client, and receive it on the next request; a returned token of length zero means
the listing is exhausted. The token is opaque — do not parse it — and it is what
lets pagination be stateless. Ignoring the empty-token termination and looping on
the same token forever is a classic bug.

### Consistency is a per-provider property

S3 now offers strong read-after-write consistency for new objects, but overwrite
visibility, list-after-write visibility, and cross-provider guarantees still
vary. Do not assume a key you just wrote is immediately visible in a `List` on
every provider, and do not build correctness on the assumption that two clients
see the same object version at the same instant. When you need write-once or
compare-and-swap semantics, use the conditional-write primitives below rather
than relying on consistency timing.

### Conditional writes and the uniform error taxonomy

`WriterOptions.IfNotExist` makes a write succeed only if the key does not already
exist; a conflict surfaces as an error at `Write` or `Close`. That is the
primitive for write-once semantics — an idempotent artifact publish, a lock file,
a dedup guard. `Attributes.ETag` supports optimistic read-modify-write reasoning.
The portability payoff is `gcerrors`: `gcerrors.Code(err)` returns a
provider-independent `ErrorCode` from a taxonomy shared across every driver —
`NotFound`, `AlreadyExists`, `FailedPrecondition` (what an `IfNotExist` conflict
maps to), `PermissionDenied`, `Unimplemented`, `ResourceExhausted`,
`DeadlineExceeded`, and `Canceled`. `Code` also understands the standard library:
a wrapped `context.Canceled` maps to `Canceled` and a wrapped
`context.DeadlineExceeded` maps to `DeadlineExceeded`. Callers branch on the code
and behave identically regardless of provider. Comparing errors by string match
or by a provider-specific concrete type in the domain defeats exactly the
portability the library exists to give you.

### Signed URLs offload the data plane

A signed URL is a time-limited, method-scoped URL that lets a client upload or
download an object directly, so the bytes never pass through your service.
`SignedURL(ctx, key, opts)` takes a `SignedURLOptions` with an `Expiry` (default
`blob.DefaultSignedURLExpiry`, one hour), a `Method` (`GET`, `PUT`, or `DELETE`),
and an optional `ContentType`. This is a real cost and scalability lever: a
service that hands out signed URLs for multi-gigabyte uploads does not proxy
those gigabytes. But support is uneven and is itself a portability seam to reason
about, not assume: `memblob` returns `Unimplemented`, `fileblob` signs only if
you configure it with a `URLSigner` and stand up an endpoint to serve the signed
requests, and each cloud emits a differently shaped URL with different query
parameters. `fileblob.NewURLSignerHMAC(baseURL, secretKey)` gives you a
fully offline HMAC signer that is a faithful stand-in for the cloud presigners
for local development and tests.

### Security, multi-tenancy, and least privilege

Credentials come from the ambient environment (instance role, workload identity,
`AWS_*` variables) or from URL query parameters; prefer the ambient path and
grant least privilege. For multi-tenancy, `blob.PrefixedBucket(b, prefix)` wraps
a bucket so every key is transparently prefixed, isolating a tenant to a key
namespace, and `blob.SingleKeyBucket(b, key)` pins a wrapper to exactly one
object. These are ergonomic isolation, not a security boundary: a prefix wrapper
does not stop code that holds the underlying bucket from reaching another
tenant's keys. Back prefix isolation with IAM policies that actually deny
cross-tenant access.

### Testability as a design goal, and cost awareness

`memblob` and `fileblob` are in-process fakes that need no network and no
credentials, which is the whole reason to wrap `*blob.Bucket` behind your own
narrow interface: your domain depends on the interface, your unit tests inject a
memblob-backed adapter, and production injects an S3-backed one. That is the
ports-and-adapters win made concrete. Finally, remember that every operation is a
billed request against a real provider. List-heavy access patterns and
full-object reads are expensive; cache `Attributes` you will reuse, use ranged
reads for partial data, and avoid N+1 head-or-list loops. Portability does not
make the underlying operations free.

## Common Mistakes

### Not checking the error from Writer.Close

Wrong: `io.Copy(w, r)` then `w.Close()` with the error discarded, or the code
returning after the copy without closing at all. The write is buffered and only
committed on `Close`; discarding its error silently loses or truncates data on
exactly the failures you most need to catch.

Fix: always `if err := w.Close(); err != nil { return err }`, and on an error
mid-copy call `w.Close()` best-effort to abort before returning the copy error.

### Forgetting the blank driver import

Wrong: `blob.OpenBucket(ctx, "s3://…")` with no `_ "gocloud.dev/blob/s3blob"`.
The code compiles but fails at runtime because no opener registered the scheme.

Fix: blank-import the driver whose scheme you open. In tests, prefer the direct
constructor (`memblob.OpenBucket(nil)`), which needs no blank import.

### Leaking readers and buckets

Wrong: opening a `*blob.Reader` or a bucket and never closing it, leaking
connections and handles under load.

Fix: `defer r.Close()` on every reader and `defer b.Close()` (or a `Close` method
on your adapter) on every bucket.

### Treating keys as OS paths

Wrong: building keys with `filepath.Join`, expecting real directories, or
expecting an atomic rename. `filepath.Join` uses the OS separator and there is no
rename.

Fix: build keys with plain string concatenation and `"/"`, and navigate hierarchy
with `Prefix` + `Delimiter`, reading `IsDir` pseudo-entries.

### Buffering large objects with ReadAll/WriteAll

Wrong: `ReadAll` on a multi-gigabyte object, buffering it all in memory.

Fix: stream with `NewReader`/`NewWriter`, `Upload`/`Download`, or fetch a slice
with `NewRangeReader`.

### Assuming Delete of a missing key is a no-op

Wrong: `b.Delete(ctx, key)` and ignoring the error, assuming absence is fine.
Strict providers return `NotFound`.

Fix: tolerate `gcerrors.NotFound` explicitly, or check `Exists` first.

### Assuming IfNotExist and SignedURL behave identically everywhere

Wrong: relying on `SignedURL` in code that might run against `memblob`, or
assuming conditional-write semantics are identical across drivers.

Fix: treat both as capabilities that vary by driver; `memblob` returns
`Unimplemented` for `SignedURL`, and callers should branch on `gcerrors.Code`.

### Comparing errors by string or concrete type in the domain

Wrong: `strings.Contains(err.Error(), "NoSuchKey")` or a type switch on the S3
error type inside business logic.

Fix: `gcerrors.Code(err) == gcerrors.NotFound`. That is the whole point of the
portable taxonomy.

### Reaching for As() in domain code

Wrong: calling `bucket.As(&s3Client)` in business logic to set a storage class,
re-coupling the domain to S3.

Fix: keep `As()` and the `Before*` hooks inside the provider-specific adapter; the
domain sees only the narrow port.

Next: [01-portable-object-store.md](01-portable-object-store.md)
