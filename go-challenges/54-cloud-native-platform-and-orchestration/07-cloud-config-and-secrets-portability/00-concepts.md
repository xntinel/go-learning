# Portable Config and Secrets with gocloud — Concepts

Run the same Go service across AWS, GCP, Azure, local dev, and CI and you hit the
same two questions in every environment: where does configuration come from, and
how do I decrypt a secret. The naive answer grows three code paths and a pile of
`if os.Getenv("CLOUD") == ...` branches. The Go Cloud Development Kit
(`gocloud.dev`) answers it differently. Its `runtimevar` and `secrets` packages
each expose one small Go interface whose concrete backend is chosen by a URL
string at deploy time: `constant://` and `file://` locally and in CI,
`awsparamstore://` / `awssecretsmanager://` / `gcpruntimeconfig://` in production
for config (note: `runtimevar` ships no first-class Azure App Configuration
driver, so on Azure you reach for `blobvar://` over Blob Storage or an `httpvar://`
endpoint), and `base64key://` / `awskms://` / `gcpkms://` / `azurekeyvault://` for
crypto. Your business logic depends on a `*runtimevar.Variable` and a
`*secrets.Keeper`; the cloud binding is one line of wiring. This is exactly the
hexagonal shape this curriculum pushes: ports in the core, adapters at the edge,
selection by configuration.

This file is the conceptual foundation. Read it once and the three independent
exercises that follow — a typed portable config loader, a hot-reloading watcher,
and envelope-encrypted config — become variations on the same two ideas.

## Concepts

### The two ports: Variable and Keeper

`runtimevar.Variable` is a port for "a value that may change over time, sourced
from somewhere." `secrets.Keeper` is a port for "encrypt and decrypt against a
key-management system." Neither type names a cloud. You obtain a concrete one by
URL:

```go
v, err := runtimevar.OpenVariable(ctx, os.Getenv("CONFIG_URL"))
k, err := secrets.OpenKeeper(ctx, os.Getenv("KEEPER_URL"))
```

`CONFIG_URL` might be `file:///etc/app/config.json` on a laptop and
`awsparamstore://myapp/config?decoder=json` in production; `KEEPER_URL` might be
`base64key://<key>` in CI and `awskms://<key-id>?region=us-east-1` in prod. The
Go code does not change. Selection is a wiring and deployment concern, driven by
the URL scheme. The scheme is registered by a driver package you blank-import
(for example `_ "gocloud.dev/runtimevar/awsparamstore"`); import the driver you
actually deploy with, and keep the hermetic drivers (`constantvar`, `filevar`,
`localsecrets`) available so tests and local runs use the same interface.

### Bytes in, typed value out

Every runtimevar driver delivers raw `[]byte`. A `runtimevar.Decoder` turns those
bytes into `Snapshot.Value`. A decoder is a prototype object plus a decode
function: `runtimevar.NewDecoder(&Config{}, runtimevar.JSONDecode)` says "the
value is a `*Config`, produced by JSON-unmarshalling the bytes into a fresh
`Config`." There are predefined decoders — `runtimevar.StringDecoder` (value is
`*string`) and `runtimevar.BytesDecoder` (value is `*[]byte`) — but for a config
struct you build your own.

The decode function type is `func(context.Context, []byte, any) error`;
`runtimevar.JSONDecode` and `runtimevar.StringDecode` both satisfy it.
`Snapshot.Value` is typed `any`, so you must type-assert it. The prototype you
passed to `NewDecoder` determines the concrete type: pass `&Config{}` and the
value is `*Config`, not `Config`. Asserting to the wrong type is a runtime panic,
not a compile error, so match the assertion to the prototype exactly.

### Latest versus Watch: the central distinction

`Variable` has two read methods and confusing them is the most common mistake.

`Latest(ctx)` returns the most recent *good* snapshot. On the very first call it
blocks until a good value has ever arrived (or `ctx` is Done). It is safe to call
concurrently from many goroutines, so it is the method for the per-request hot
path. Its context rule is subtle: if a good value already exists, `Latest`
returns it even when `ctx` is already Done; only when no good value has *ever*
arrived and `ctx` is Done does it return the latest error explaining why (it
returns that error, not `ctx.Err()`). That is what makes it safe inside a
deadline-bounded handler — and it is why a cold start with a Done context can look
like a config failure.

`Watch(ctx)` returns only when the value *changes* (it also blocks on the first
call until the first value). It is explicitly *not* safe to call concurrently:
`Watch` is single-consumer. The pattern is one background `Watch` loop that
publishes each new snapshot, and `Latest` (or a cached pointer) everywhere else.

### Readiness versus liveness

`CheckHealth()` returns `nil` exactly when `Latest` would return a good value
without blocking. That maps cleanly onto a Kubernetes readiness probe: do not send
traffic to the pod until its config has loaded. Because `Latest` blocks until the
first good value, you must not call it naively inside a health handler — a probe
that blocks forever fails the pod. Use `CheckHealth` for readiness; it never
blocks.

### Hot reload without data races

To reload dynamic config — log level, feature flags, rate limits — without a
restart, run a single `Watch` loop in the background and publish each good
snapshot into an `atomic.Pointer[Config]`. Request handlers `Load()` the pointer
per request. The whole config object is swapped atomically, so no reader ever
observes a half-updated struct and the hot path takes no lock. A plain pointer
field written by the watcher and read by handlers is a data race that
`go test -race` will catch; `atomic.Pointer` (or a mutex-guarded pointer) is the
fix.

### Error taxonomy in the Watch loop

`runtimevar.ErrClosed` is a sentinel: `Watch` returns it once the `Variable` has
been `Close`d. That is the shutdown handshake — `Close` unblocks the in-flight
`Watch` with `ErrClosed`, and the loop should treat it as "exit cleanly," not as a
failure. Context cancellation is similar: `Watch` returns an error satisfying
`errors.Is(err, context.Canceled)`. Every *other* error — a file briefly missing
mid-write, a transient backend hiccup, a decode failure on a bad payload — is
transient: log it and continue watching. Conflating the two categories either
leaks a goroutine (treating `ErrClosed` as retryable) or kills reloading on a
hiccup (treating a transient error as fatal).

### Lifecycle and leaks

Every `Variable` and every `Keeper` must be `Close`d. Closing a `Variable`
releases the driver's polling goroutine or file watcher and unblocks any in-flight
`Watch` with `ErrClosed`. Forgetting `Close` leaks that background machinery.
Coordinate shutdown: do not call `Close` concurrently with a `Latest`/`Watch` you
are not prepared to see return `ErrClosed`, and do not rely on `Close` being safe
to call twice.

### Envelope encryption and secret-at-rest

Storing a plaintext database password in a parameter store or a config file is the
default failure. The envelope pattern stores *ciphertext* in the config source and
decrypts it just-in-time, in memory, with a Keeper. `runtimevar` composes this
directly: `runtimevar.DecryptDecode(keeper, post)` returns a decode function that
first hands the variable's bytes to `keeper.Decrypt` and only then passes the
plaintext to the `post` decoder. So

```go
dec := runtimevar.NewDecoder(&Config{}, runtimevar.DecryptDecode(keeper, runtimevar.JSONDecode))
```

yields a `*Config` from ciphertext-at-rest: the config store and the disk only
ever see encrypted bytes, and the plaintext exists solely as a decoded struct in
process memory. Decryption happens *before* parsing, so no plaintext-shaped
intermediate string is created for a log line to leak.

### Portability boundaries and trade-offs

A unified API is a lowest-common-denominator API. `filevar` polls and offers no
read-consistency guarantee: mid-write you can transiently see an empty or errored
value, and its `Options.WaitDuration` governs retry frequency after an error. Its
change latency is bounded by the file system's notification behavior, which
differs across platforms (on macOS, replacing a file with an empty one may not
fire a change). Cloud stores each have their own consistency, quota, and cost
model, and `Watch` latency depends on how often the driver polls the backend.
Portability buys you one code path; know what read-consistency and latency
guarantees you give up for it, and write tests that poll with a deadline rather
than sleep-then-assert-once.

### localsecrets is for dev and tests, not production

`localsecrets` is symmetric AES-256 using a 32-byte key held in-process. It is
ideal for tests and local development and terrible as a production KMS: no key
rotation, no audit trail, no HSM. `NewRandomKey()` returns a fresh `[32]byte` for
tests; `Base64Key(s)` decodes a URL-safe base64 string that must decode to exactly
32 bytes; a `base64key://<key>` URL embeds the key in the string, which is fine for
dev only. The whole value of the abstraction is that moving to production changes
only the URL — `base64key://...` becomes `awskms://...` — with no change to the
code that calls `Encrypt`/`Decrypt`.

### Context discipline

`OpenVariable`, `OpenKeeper`, `Latest`, `Watch`, `Encrypt`, and `Decrypt` all take
a `context.Context`. Bound them with timeouts so a slow backend cannot wedge a
request. Remember the `Latest` rule above: an already-Done context returns the
latest error immediately when there is no good value yet, so pass a live context
on cold start and reserve a Done context for the deliberate "do not block" case.

## Common Mistakes

### Type-asserting Snapshot.Value to the wrong type

Wrong: `cfg := snap.Value.(Config)` when the decoder prototype was `&Config{}`.
The value is `*Config`; asserting to `Config` panics at runtime.

Fix: assert to the pointer type the prototype implies — `cfg, ok := snap.Value.(*Config)` —
and handle `!ok` as an error rather than letting the bare assertion panic.

### Calling Watch concurrently or on the request path

Wrong: calling `Watch` from each request handler, or from several goroutines at
once. `Watch` is single-consumer and change-driven; concurrent callers block and
misbehave, and per-request `Watch` blocks until the *next* change.

Fix: one background `Watch` loop publishing into an `atomic.Pointer`; handlers
read the pointer (or call `Latest`, which is concurrency-safe).

### Treating ErrClosed as fatal, or every error as fatal

Wrong: `if err != nil { return err }` in the `Watch` loop, so a transient
mid-write read error crashes reloading; or logging `ErrClosed` as an error and
looping forever after shutdown.

Fix: branch on the error. `errors.Is(err, runtimevar.ErrClosed)` (or a cancelled
context) means exit; anything else means log and continue.

### Publishing config with a plain pointer field

Wrong: the watcher writes `r.cfg = newCfg` while handlers read `r.cfg`. That is a
data race and `go test -race` fails.

Fix: `atomic.Pointer[Config]` with `Store` in the watcher and `Load` in handlers,
or a mutex around both.

### Storing plaintext and decrypting after decode

Wrong: put a plaintext secret in the parameter store, decode to a struct, then
decrypt a field manually. Plaintext now sits at rest, and a decoded struct or a
log line can leak it.

Fix: store ciphertext and chain `runtimevar.DecryptDecode(keeper, runtimevar.JSONDecode)`
so decryption happens before parsing and plaintext lives only in memory.

### Forgetting to Close, or misusing localsecrets keys

Wrong: opening a `Variable`/`Keeper` and never `Close`ing it (leaking the polling
goroutine or watcher), or passing `Base64Key` a string that decodes to other than
32 bytes, or shipping a committed `base64key://` to production where there is no
rotation, audit, or HSM.

Fix: `defer v.Close()` / `defer k.Close()`; ensure the key is exactly 32 bytes;
use `localsecrets` only in dev and tests and switch the URL to a real KMS in prod.

Next: [01-typed-config-loader.md](01-typed-config-loader.md)
