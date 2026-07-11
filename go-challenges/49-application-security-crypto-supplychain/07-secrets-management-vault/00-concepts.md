# Secrets Management with Vault — Concepts

You own the secrets layer for a fleet of Go services. The interesting question is
not "which SDK do I import" — it is a custody problem. How does a process prove it
is allowed to have a database password without a secret already baked into its
image? How does every process get its *own* credential, short-lived, so a leaked
one is worthless in minutes? How do you rotate, audit, and revoke centrally
instead of redeploying every service when one credential leaks? This is the same
KEK/DEK envelope-encryption custody problem from earlier lessons, but externalized
to a dedicated broker. Vault is the reference implementation of that broker, and
this lesson treats it as such: machine identity via AppRole, ephemeral database
credentials with lease renewal and revocation, and a production-grade provider
that survives a Vault blip. Because a live Vault is an external dependency, the
runnable code drives an `httptest` fake that speaks Vault's HTTP wire protocol, and
the live paths sit behind a `//go:build integration` tag.

## Why a broker at all

Environment variables and config files give you *secret sprawl*. The same
long-lived database password is copied into every pod's environment, a CI secret
store, three developers' laptops, and an old Terraform state file. There is no
rotation story that does not involve a coordinated redeploy of everything that
holds a copy. There is no per-consumer identity: every process presents the same
password, so an audit log cannot tell you *which* pod read it. And there is no
revocation: when one copy leaks, the only remedy is to rotate the shared secret and
redeploy the world, which is so painful that in practice it never happens and the
leaked credential stays valid for months.

A broker centralizes the four things sprawl cannot give you: **issuance** (one
place mints credentials), **rotation** (the broker rotates the backing secret on a
schedule), **audit** (every read is attributed to an identity and logged), and
**revocation** (a single call kills a credential everywhere). Once issuance is
central, you can make credentials *dynamic*, which is the real payoff.

## Static vs dynamic secrets

A **static** secret is stored-and-read. You put a value into the key/value engine
and consumers read it back. It is still better than an env var because it is
centrally audited and access-controlled, but it is one shared value with a manual
rotation story.

A **dynamic** secret is *generated on demand*. When a service asks the database
secrets engine for credentials, Vault connects to the database as an admin, runs a
`CREATE ROLE` statement, and hands back a brand-new username and password unique to
that request. The credential is short-lived and carries a *lease*; when the lease
ends, Vault runs the corresponding `DROP ROLE` and the credential stops working.

This converts a long-lived shared password — huge blast radius, manual rotation,
present in dozens of places — into a per-process credential that dies in minutes. A
compromised pod leaks a credential that is *already expiring*, and the blast radius
of the leak is bounded to the lease TTL rather than "until someone notices." Static
KV secrets still have their place (a third-party API key you did not mint), but
dynamic secrets are the reason a broker earns its operational cost.

## The lease model

Every dynamic secret and every service token carries a **lease**: a `lease_id`, a
`lease_duration` (the TTL, in seconds), and a `renewable` flag. Vault guarantees the
credential is valid for the TTL. The consumer's obligation is the mirror image: it
must **renew before expiry** or the credential dies — potentially mid-request, in
the middle of a connection pool's lifetime.

The `lease_id` is prefixed by the path that issued it (for example
`database/creds/app-ro/AbC123...`). That prefix is not cosmetic: it lets an operator
revoke an entire subtree at once ("revoke everything issued under
`database/creds/app-ro`") during an incident. Expiry auto-revokes: when the TTL
elapses with no renewal, Vault runs the revocation (the `DROP ROLE`) itself.

## Renewal and the LifetimeWatcher

Renewal *extends* a lease, but only up to its `max_ttl`. After `max_ttl` the lease
cannot be extended at all and re-authentication (or a fresh fetch) is mandatory.
The Go client's `LifetimeWatcher` automates the renew loop: you hand it the
`*api.Secret`, and it sleeps until a grace-period threshold before expiry, then
renews, adding jitter so a fleet of pods that all started together do not stampede
Vault at the same instant (the "thundering herd").

The watcher exposes two channels. `RenewCh()` emits a `*api.RenewOutput` on each
successful renewal. `DoneCh()` fires exactly once, when the watcher stops renewing —
and here is the trap that catches everyone: **`DoneCh` firing with a `nil` error
does not mean "success, keep going." It means the lease can no longer be extended
(you hit `max_ttl`, or renewal is disabled) and you must re-authenticate or refetch
now.** Treating a nil-error `DoneCh` as "all good" leaves you using a credential
that is about to stop working.

`RenewBehavior` controls how the watcher reacts to a renewal *error*.
`RenewBehaviorIgnoreErrors` (the default, `iota` 0) keeps trying across transient
failures and is the re-auth-friendly choice; `RenewBehaviorErrorOnErrors` makes the
watcher give up and fire `DoneCh` on the first error; `RenewBehaviorRenewDisabled`
turns renewal off entirely.

## Explicit revocation and lease explosions

If you rely *only* on TTL expiry, every credential your service ever fetched sits
in Vault's lease table until its TTL elapses, and every dynamic database credential
corresponds to a real `CREATE ROLE` still present in the database. A service that
churns pods, or fetches credentials in a hot path, accumulates orphaned leases and
orphaned database users faster than they expire. Vault's revocation queue backs up;
the database's `pg_roles` (or equivalent) fills with dead users; eventually one or
both degrade. This "lease explosion" is the single most common operational failure
mode of a Vault deployment.

The fix is discipline on shutdown: a service must **revoke its leases when it stops
gracefully** — `client.Sys().Revoke(leaseID)` by id, or revoke-by-prefix for a
whole subtree — so the backing resources are torn down immediately instead of
lingering for a full TTL. Operators additionally bound lease counts with quotas.
Revoke-on-shutdown is cheap to implement and prevents the failure that most often
takes a Vault deployment down.

## Machine identity and the secret-zero problem

You cannot solve "how does my service get a secret" by shipping a secret in the
image — that just moves the problem to "how does the image's secret stay secret,"
and now it is in your registry, your CI logs, and your git history. This is the
**secret-zero** problem: bootstrapping trust without a pre-shared secret.

**AppRole** splits identity into two parts. The `role_id` is a non-sensitive,
relatively static identifier (think "username") that can live in config. The
`secret_id` is the sensitive half (think "password") and is short-lived and
frequently rotated. A workload presents both to `auth/approle/login` and receives a
service token.

**Response wrapping** delivers the `secret_id` without either party seeing the
other's plaintext — "secure introduction." A trusted orchestrator asks Vault to
wrap a fresh `secret_id` into a single-use, short-TTL *wrapping token*, hands only
that token to the workload, and the workload unwraps it exactly once to get the real
`secret_id`. If the wrapping token is intercepted, the interceptor either used it
(and the legitimate workload's unwrap fails, raising an alarm) or it expires
unused. In the Go client this is one option: `approle.WithWrappingToken()`.

**Kubernetes auth** removes the static secret entirely: the workload presents its
projected ServiceAccount JWT, Vault verifies it against the cluster's API, and there
is no `secret_id` to manage at all. AppRole is the portable, platform-agnostic
mechanism; Kubernetes auth is what you use when you are on Kubernetes.

## KV v1 vs v2 and the /data/ path rewrite

The KV **v2** engine versions secrets: it supports soft-delete, undelete, destroy,
and reading a specific version. To do that it stores the payload under an internal
`data/` segment. A secret you think of as `secret/myapp` actually lives at
`secret/data/myapp` on the wire, and its metadata at `secret/metadata/myapp`. This
is a classic footgun: a hand-rolled `client.Logical().Read("secret/myapp")` returns
`nil` (nothing is stored at that raw path), while `Read("secret/data/myapp")`
returns the value nested one level deeper under `data`. The `client.KVv2(mount)`
helper hides the rewrite and the nesting — use it for KV v2 rather than composing
the path yourself.

## Least privilege: policies, mounts, namespaces

A service's token should be scoped by **policy** to exactly the paths it needs and
nothing more. Secret engines are enabled at **mount paths** (`database/`,
`secret/`, `pki/`), so policy is expressed in terms of those paths. Enterprise
**namespaces** isolate tenants entirely. A broad or root token defeats the whole
point of centralizing custody: the reason to have a broker is to issue *narrow*
credentials, and a wide token throws that away.

## Failure modes to design for

Vault is a dependency, and dependencies fail. Design for it. Vault can be *sealed*
(it starts sealed and needs unsealing) or briefly unreachable during a leader
election — so a service must not treat Vault as a hard startup dependency that
crash-loops the pod; it needs retry with backoff and, for already-fetched values, a
bounded last-known-good fallback. Clock skew between the service and Vault can make
a lease look expired early or late. Caching a *dynamic* credential past its TTL uses
a credential Vault has already revoked. And the most embarrassing failure is
operational: logging the secret payload (or the whole `*api.Secret`) while
debugging, which writes the very value you centralized to protect into your log
aggregator. Remember too that Go cannot reliably zero secret memory — the garbage
collector may copy a `[]byte` before you overwrite it — so the defense is to
*minimize lifetime and scope*, not to trust scrubbing.

## The client is just HTTP

`api.Client` is a typed wrapper over Vault's REST API. `Auth().Login` is a `POST` to
`/v1/auth/approle/login`; `KVv2("secret").Get(ctx, "x")` is a `GET` to
`/v1/secret/data/x`; `Sys().Revoke(id)` is a `PUT` to `/v1/sys/leases/revoke`. That
is why the client is fully testable against an `httptest.Server` that returns the
right JSON shapes — no Vault process required — and why, in a bar-mode lesson, the
live paths that need a real Vault belong behind an `integration` build tag while the
default build and tests exercise the fake. Understanding that the client is
structured HTTP is what lets you test it deterministically in CI.

## Common Mistakes

### Authenticating with a root or long-lived static token in production

Wrong: shipping `VAULT_TOKEN=<root>` in the environment. That token is secret-zero,
never expires, and can do anything. Fix: use AppRole or Kubernetes machine identity
to obtain a short-TTL, policy-scoped token at startup.

### Baking secret-zero into the image

Wrong: putting the `secret_id` (or any bootstrap secret) into the container image or
repo. It leaks to your registry, CI logs, and git history. Fix: deliver the
`secret_id` via a response-wrapped token (`WithWrappingToken`) or use a
platform-injected identity (Kubernetes ServiceAccount JWT) so nothing static ships.

### Never renewing leases

Wrong: fetch a credential and use it forever. It expires mid-request or mid-pool.
Fix: start a `LifetimeWatcher`, or renew explicitly before the grace threshold.

### Treating a dynamic secret like a static one

Wrong: caching a dynamic credential indefinitely and using it past its lease TTL —
Vault has already revoked it, so calls start failing. Fix: bound every cached value
by its lease-derived expiry and never serve it past that instant.

### Not revoking leases on shutdown

Wrong: relying only on TTL expiry, so orphaned leases and dead database users pile
up until Vault or the database degrades. Fix: `Sys().Revoke(leaseID)` (or
revoke-by-prefix) on graceful shutdown.

### Reading a KV v2 secret at the wrong path

Wrong: `client.Logical().Read("secret/myapp")` returns a confusing `nil`. Fix: use
`client.KVv2("secret").Get(ctx, "myapp")`, or read the rewritten
`secret/data/myapp` path yourself and unwrap the nested `data`.

### Panicking on a nil *Secret

Wrong: `client.Logical().Read(path)` returns `(nil, nil)` for a missing path, and
dereferencing `secret.Data` panics. Fix: nil-check the `*api.Secret` before touching
`Data`. (Note the asymmetry: the KV v2 helper `Get` instead returns
`api.ErrSecretNotFound`, so classify that with `errors.Is`.)

### Misreading a nil-error DoneCh

Wrong: `LifetimeWatcher.DoneCh()` fires with a `nil` error and you read it as "keep
going." Fix: `DoneCh` firing at all means the lease can no longer be extended —
re-authenticate or refetch now.

### Ignoring Secret.Warnings

Wrong: dropping `secret.Warnings`. They surface deprecations and partial-success
conditions. Fix: log or surface them (without logging the payload).

### Making Vault a hard startup dependency

Wrong: crash-looping the pod when Vault is briefly unreachable. Fix: retry with
capped backoff, and serve last-known-good within a bounded staleness window so a
brief Vault blip does not take the whole service down.

### Logging the secret payload

Wrong: `log.Printf("got %+v", secret)` during debugging, writing the value to your
log aggregator. Fix: log the version, lease id, and expiry — never the value. Give
your secret type a redacting `String()` method.

### Importing the Vault SDK throughout business code

Wrong: `github.com/hashicorp/vault/api` imported in every package, so business logic
is untestable without a Vault and welded to one broker. Fix: define a
`SecretsProvider` interface, put the Vault SDK behind one implementation, and let
business code depend on the interface.

Next: [01-approle-kv-secret-loader.md](01-approle-kv-secret-loader.md)
