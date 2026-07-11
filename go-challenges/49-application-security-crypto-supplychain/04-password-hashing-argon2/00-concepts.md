# Password Hashing with Argon2 and bcrypt — Concepts

You own the credential store. That sentence is the whole lesson. When a database
is breached, the difference between "attacker replays every account tonight" and
"attacker spends a decade and gets nothing" is decided entirely by how you hashed
the passwords and which parameters you chose. The algorithm and its cost are not
an implementation detail you copy from a blog; they ARE a security control, and
like any control they must be tunable, storable, verifiable, and upgradable in
production without breaking existing logins. This file builds the mental model you
need for the three exercises that follow: an argon2id hasher that self-describes
its parameters, a bcrypt wrapper that calibrates cost and refuses to truncate
long passphrases, and a credential service that migrates a legacy table to
argon2id transparently as users authenticate.

## Concepts

### Slow by design: why a fast hash is the wrong tool

A cryptographic digest such as SHA-256 is engineered to be *fast* — gigabytes per
second, hardware-accelerated. That is exactly what you want for integrity checks
and exactly what you do not want for passwords. If you store `sha256(password)`,
an attacker with your table computes billions of guesses per second per GPU;
common passwords fall in seconds and the whole table is enumerable. Salting a
fast hash defeats precomputation (rainbow tables) but does nothing about raw
guess rate: each guess is still one cheap SHA-256.

Password hashing functions invert this deliberately. bcrypt makes each guess cost
a tunable amount of *CPU work*; argon2id makes each guess cost a tunable amount of
*CPU and memory*. The point is to make one verification cheap for you (a few
hundred milliseconds, once, at login) but ruinously expensive for an attacker
multiplied across billions of guesses. The cost parameter is the dial that sets
that ratio, and it is the security control you are actually configuring.

### Memory-hardness: why argon2id is the default recommendation

bcrypt is *CPU-hard*: an attacker who builds custom silicon (an ASIC) or rents a
rack of GPUs can parallelize guesses massively, because each guess needs almost no
memory. argon2id is *memory-hard*: each guess is forced to allocate and randomly
access a large block of RAM (tens of megabytes), and memory bandwidth is the one
resource that does not get dramatically cheaper on GPUs and ASICs. Filling 19 MiB
per guess, a million times in parallel, needs 19 TiB of fast memory — which prices
the attack out in a way pure CPU cost does not.

That is why argon2id (the hybrid variant, resistant to both side-channel and
time-memory trade-off attacks) is the current baseline recommendation for new
systems. bcrypt is not "wrong" — it is battle-tested, available everywhere,
present in FIPS-adjacent and constrained environments where argon2 may not be, and
its 30-year track record is itself a feature. The honest senior position: prefer
argon2id for new credentials; keep bcrypt where library maturity or platform
constraints demand it; and build the machinery to migrate from one to the other,
because you will.

### Parameter budgeting is an engineering decision, not a copy-paste

argon2id takes three knobs — memory (KiB), time (iterations), and parallelism
(threads) — and bcrypt takes one, a logarithmic cost. You do not pick these from a
blog post; you pick them against two budgets:

- A *latency* budget: how long may one verify take at peak? Roughly 250-500 ms per
  login is a common target. Faster leaves attacker headroom; slower degrades the
  login path and invites its own denial of service.
- A *memory* budget: argon2's memory cost is paid *per concurrent login*. At 19 MiB
  and 500 concurrent authentications you are holding ~9.5 GiB transiently. Set
  memory too high and a burst of logins becomes a memory-exhaustion DoS on your own
  service. More concurrent logins × more memory per hash = a capacity-planning
  problem you must size, not ignore.

OWASP publishes floors — argon2id at m=19456 KiB, t=2, p=1, and bcrypt cost at
least 10 — but a floor is a minimum, not an answer. The right values are whatever
maxes out your latency budget on *your* production hardware today, re-measured as
hardware gets faster. That is why one exercise builds a `CalibrateCost` helper:
cost is measured, not hardcoded.

### Self-describing hash strings: parameters live in the hash

Here is the design decision that makes everything else possible. Both formats
encode the algorithm, version, parameters, and salt *inside the stored string*:

```text
$argon2id$v=19$m=19456,t=2,p=1$c29tZXNhbHQ$aGFzaGJ5dGVz   (PHC format)
$2a$12$R9h/cIPz0gi.URNNX3kh2OPST9/PgBkqquzi.Ss7KIUgO2t0jWMUW   (bcrypt)
```

Because the parameters travel with the hash, verification is *stateless*: you do
not need to know how a hash was made, only to parse it. And parameter evolution
becomes possible without a schema migration. Raise your argon2 memory next quarter
and old hashes still verify against *their* embedded parameters; new hashes carry
the new ones; and a `NeedsRehash` check compares embedded parameters against
current policy to decide who to upgrade. Store the parameters in a separate column
instead, or hardcode them, and verification breaks the instant you tune anything —
because the code no longer knows which parameters produced which hash.

The PHC (Password Hashing Competition) string format is the standard layout for
argon2: `$argon2id$v=<version>$m=<memory>,t=<time>,p=<parallelism>$<b64salt>$<b64hash>`,
with the salt and hash in base64 using the standard alphabet and *no padding*
(Go's `base64.RawStdEncoding`). bcrypt has its own older `$2a$`/`$2b$` format that
embeds cost and salt the same way.

### Per-hash random salt

Every hash gets its own fresh, cryptographically-random salt, read from
`crypto/rand` — never `math/rand`, whose output is predictable and defeats the
whole purpose. The salt does two jobs: it defeats precomputation (an attacker
cannot build one rainbow table that works against every account), and it ensures
two users with the same password get different stored hashes, so a breach does not
reveal password reuse across accounts. The salt is not a secret; it is stored in
the clear, right there in the hash string. Its value comes from being *unique per
record*, not from being hidden.

### Constant-time comparison and timing oracles

When you hand-roll an argon2id verifier, the last step is comparing the recomputed
key against the stored key — and you must do it with `crypto/subtle.ConstantTimeCompare`,
not `==` or `bytes.Equal`. A naive byte comparison returns as soon as it finds a
mismatched byte, so its running time leaks *how many leading bytes matched*. An
attacker who can measure verify latency can then reconstruct the correct hash byte
by byte, turning your verify path into an oracle. `ConstantTimeCompare` always
examines every byte and takes the same time regardless of where the first
difference is (it does return early only on a length mismatch, which is why you
compare equal-length recomputed keys). bcrypt's `CompareHashAndPassword` already
does this internally; a hand-rolled argon2id verifier does not unless you make it.

### The bcrypt 72-byte limit: reject, do not truncate

bcrypt only ever reads the first 72 bytes of a password — a property of the
underlying Blowfish key schedule. For years libraries silently truncated, so
`"correct horse battery staple ...(long passphrase)"` and its first-72-byte prefix
hashed identically, an invisible footgun. Modern Go's `x/crypto/bcrypt` fixed this
(golang/go#36546): `GenerateFromPassword` now *rejects* a password longer than 72
bytes with `ErrPasswordTooLong` instead of truncating, forcing you to handle long
passphrases deliberately. `CompareHashAndPassword` stays lenient — it does not
length-check, so hashes created in the truncating era still verify — which means
the two paths are asymmetric: Generate rejects, Compare accepts. The naive
workaround, pre-hashing with SHA-256 to shrink the input, reintroduces a
truncation bug of its own: raw SHA-256 output can contain a NUL byte, and bcrypt
treats NUL as a string terminator, so `sha256(pw)` bytes can collide at the first
NUL. If you must pre-hash, base64-encode the digest first so there are no NULs.

### Upgrade-on-login: how a live table migrates

Parameters must increase over time and algorithms get superseded, so a production
hasher exposes a "needs rehash" check and the login path acts on it. On a
successful authentication — and only then, because that is the one moment you hold
the plaintext password legitimately — you check whether the stored hash is below
current policy (a lower bcrypt cost, weaker argon2 parameters, or a legacy
algorithm). If so, you recompute a fresh hash at current policy and re-store it.
No mass re-hash job (you cannot rehash a password you do not have), no forced
password reset, no downtime. Over weeks, as your active users log in, a table of
legacy bcrypt hashes silently becomes a table of argon2id hashes. The third
exercise builds exactly this: a `Service.Authenticate` that returns the verified
result plus an optional freshly-upgraded hash for the caller to persist.

### Pepper vs salt

A *salt* is per-record and public. A *pepper* is a single secret key, the same for
every record, applied before or during hashing (for example `HMAC-SHA256(pepper,
password)` fed into the password hash), and stored *outside* the database — in a
KMS, an HSM, or an environment variable the DB never sees. Its job is different
from salt's: a pepper defends against a *database-only* breach, because an attacker
with the table but not the pepper cannot even begin offline guessing. The
operational cost is real: rotating a pepper changes the input to every hash, so you
can only rotate it through the same upgrade-on-login machinery, keeping old and new
peppers around during the transition. Salt and pepper are complementary, not
alternatives.

### Failure modes to internalize

Logging the password or leaking it through timing; reusing a salt or using
`math/rand` for it; storing parameters apart from the hash so verification cannot
find them; treating any non-nil verify error as "server error" and failing open
(a mismatch is *invalid credentials*, not a 500); and sizing argon2 memory without
accounting for concurrent logins, turning your own auth endpoint into a
memory-exhaustion DoS. Each of these has burned a real production system.

## Common Mistakes

### Using a fast hash for passwords

Wrong: storing `sha256(salt || password)` because "it's cryptographic." It is
cryptographic and it is fast, so after a breach the entire table is brute-forceable
in hours. Fix: use bcrypt or argon2id, where the per-guess cost is a tunable dial,
and salting is just one of several defenses rather than the only one.

### Comparing argon2id output with == or bytes.Equal

Wrong: `if recomputed == stored` (or `bytes.Equal`) to finish a hand-rolled
argon2id verify. The comparison short-circuits on the first differing byte and
leaks match length through timing. Fix: `subtle.ConstantTimeCompare(recomputed,
stored) == 1` on equal-length inputs.

### Storing parameters in a separate column

Wrong: a `hashes` table with columns `hash`, `memory`, `time`, `parallelism`. The
moment you tune a parameter, old rows verify with the new column values and fail.
Fix: encode the parameters in the PHC string; the hash is self-describing and
verification reads the parameters that actually produced it.

### Salting with math/rand or a global constant

Wrong: `rand.Intn` from `math/rand`, or a single hardcoded salt for all users. The
former is predictable; the latter reintroduces rainbow tables and reveals password
reuse. Fix: a fresh salt per hash from `crypto/rand.Read`.

### Assuming bcrypt silently truncates at 72 bytes

Wrong: passing a long passphrase straight to `GenerateFromPassword` and ignoring
the error, so a real user's 80-character passphrase becomes a login failure
nobody diagnoses. Fix: handle `ErrPasswordTooLong` explicitly — reject with a
clear message, or pre-hash with a *base64-encoded* digest (never raw SHA-256
bytes, which can contain a NUL that bcrypt treats as a terminator).

### Hardcoding a cost from a blog post

Wrong: `bcrypt.GenerateFromPassword(pw, 12)` copied once and never revisited. Cost
that was painful in 2015 is trivial today. Fix: calibrate cost to your current
hardware and latency budget, store it in the hash, and raise it over time via
upgrade-on-login.

### Failing open on a verify error

Wrong: `if err != nil { return http.StatusInternalServerError }` for every error
from the compare, so a mismatch either 500s or, worse, some refactor lets it fall
through to "authorized." Fix: map `ErrMismatchedHashAndPassword` (via `errors.Is`)
to "invalid credentials" and treat only structural errors as server errors — and
never treat a non-nil error as success.

### Sizing argon2 memory without a concurrency budget

Wrong: cranking memory to 256 MiB "for security." A modest login burst then
allocates tens of gigabytes and the service falls over — a self-inflicted DoS. Fix:
size memory against peak concurrent logins × per-hash memory, and treat the login
endpoint as a resource you must capacity-plan.

### Never implementing a rehash path

Wrong: shipping a hasher with no "needs rehash" check, so a table of weak-cost or
legacy-algorithm hashes never improves even though users log in daily. Fix: expose
`NeedsRehash`/`NeedsUpgrade` and act on it after every successful authentication.

Next: [01-argon2id-phc-hasher.md](01-argon2id-phc-hasher.md)
