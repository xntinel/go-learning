# Secure Tokens: PASETO vs JWT — Concepts

Every backend ships bearer tokens, and almost nobody audits the parser. That is
the wrong thing to leave unaudited, because the verifier is the entire security
boundary: a token is exactly as trustworthy as the strictest thing your parser is
willing to accept. This lesson is not "how to mint a token." It is "how the
verifier decides what to trust, and why the JOSE/JWT design hands the verifier too
many ways to be fooled." You will build a PASETO v4.public access-token service, a
v4.local encrypted-session service with key rotation and context binding, and a
hardened `golang-jwt` verifier whose test suite proves the classic algorithm
attacks fail closed.

## Concepts

### The verifier is the security boundary, not the issuer

An attacker never sees your signing code; they attack your verification code. The
issuer can be flawless and you are still owned if the verifier accepts one token
it should have rejected. Design the verifier to *enumerate* precisely what is
acceptable and reject everything else: which cryptographic primitive, which key,
which issuer, which audience, which time window. "Fail closed" means an
unrecognized algorithm, a missing claim, an absent expiry, or an unknown key id
is an error, never a shrug. Reading a claim is not the same as enforcing it: if
your code calls `GetAudience()` but never asserts the value, the audience is
decoration, and a token minted for service A sails into service B.

### JWT's algorithm agility is the root footgun

A JWS/JWT carries its own algorithm in the header: `{"alg":"RS256"}`. That header
is attacker-controlled — it arrives with the token, before any signature has been
checked. A naive verifier reads `alg`, looks up the matching primitive, and calls
it with whatever key the `keyFunc` returns. Two canonical exploits follow.

RS256 to HS256 confusion. A service that verifies RSA-signed tokens holds an RSA
*public* key, which by definition is not secret. If the verifier lets the token
pick the primitive and its `keyFunc` returns key *bytes* (a PEM/JWK blob) rather
than a typed key, an attacker crafts a token with `alg:HS256` and signs it with
HMAC using the public key's own bytes as the shared secret. The verifier dutifully
runs HMAC-SHA256 with the public key as the "secret," the MAC matches, and a
forged token validates. The attacker needed nothing secret.

`alg:none`. The spec defines an unsecured token with no signature at all. A
verifier that honors the header will accept a token with `alg:none` and an empty
signature as valid. Modern libraries make this opt-in (in `golang-jwt/v5` the
`keyFunc` must return a special sentinel to permit it), but the class of bug is
"the token told the verifier how to check itself."

The mitigation is always the same: pin the acceptable algorithms *out of band*,
in verifier configuration, and never let the token choose the primitive. In
`golang-jwt` that is `WithValidMethods([]string{"RS256"})`, checked before the
`keyFunc` is even consulted.

### PASETO's thesis: versioned, non-negotiable cryptography

PASETO (Platform-Agnostic Security Tokens) removes the choice. A token begins with
a version and a purpose baked into the header as literal text: `v4.public.` or
`v4.local.`. There is exactly one primitive per version and purpose:

- `v4.public` uses Ed25519 signatures. The payload is signed and world-readable:
  integrity and authenticity, no confidentiality.
- `v4.local` uses XChaCha20-Poly1305 authenticated encryption. The payload is
  confidential and integrity-protected.

Because the library only implements the one primitive for each header, there is no
`alg` field to confuse, no downgrade path, and no `none`. The parser does not
negotiate; it calls `ParseV4Public` or `ParseV4Local`, and you supply the key.
The failure class that dominates JWT bug bounties simply does not exist.

### public vs local is a data-classification decision

Choosing between the two purposes is not a performance question; it is "who is
allowed to read the claims." Use `v4.public` when a resource server or the client
legitimately reads claims to make authorization decisions and you accept that the
base64url payload is readable by anyone holding the token — the same visibility a
signed JWT has. Use `v4.local` when the payload must stay server-confidential:
session state, internal identifiers, PII. It gives the same integrity guarantee
plus confidentiality. Never treat a `v4.public` (or a signed JWT) payload as
secret; signing proves who wrote it, not that others cannot read it.

### Footers are authenticated but not encrypted

Both purposes support a footer: extra bytes covered by the signature or MAC but
*not* encrypted, even in `v4.local`. Because the footer is authenticated it cannot
be tampered with, and because it is not encrypted it can be read before the token
is verified. That makes it the correct home for a key id (`kid`) that selects
which key to verify with, and for public routing metadata. It is the wrong home
for anything secret. A verifier reads the footer's `kid` with an "unsafe" footer
parse (unsafe only in the sense that the signature has not been checked yet),
picks the corresponding key from a keyring, and then verifies — at which point a
tampered footer makes the whole verification fail.

### Implicit assertions bind a token to out-of-band context

Every v4 sign/encrypt and parse call takes a trailing `implicit []byte`. This is
the *implicit assertion*: data that is authenticated (mixed into the signature or
MAC) but is **not** transmitted with the token. Sign with implicit `tenant=acme`
and the token only verifies when the verifier supplies the byte-identical
`tenant=acme`. It cryptographically binds the token to context — tenant, purpose,
a channel-binding value — without putting that context on the wire, so a token
that is valid in one context cannot be replayed in another. The rule is exact:
sign/encrypt and verify must pass byte-identical implicit assertions or
verification fails, and `nil` is a distinct value from empty. Use it deliberately
and consistently, or not at all.

### Expiry must be opted into, in both ecosystems

Time checks are never free. In PASETO, `NewParser()` preloads a `NotExpired`
rule, but `NewParserWithoutExpiryCheck()` preloads nothing — build a parser that
way and forget to add `NotExpired()`/`ValidAt()` and an expired token is accepted.
In `golang-jwt`, `exp` is validated only if present; a token with no `exp` never
expires unless you add `WithExpirationRequired()`. Both ecosystems also need an
explicit leeway policy: clocks on different hosts disagree, so `nbf`/`exp` checks
should tolerate a small skew (`WithLeeway` in `golang-jwt`, or verifying against a
deliberate reference time in PASETO). No leeway means intermittent, maddening
failures under normal clock drift.

### Registered claims are a contract to enforce

`iss`, `aud`, `sub`, `exp`, `nbf`, `iat`, `jti` are not decoration. Audience
scoping (`aud`) is what stops a token minted for one service being replayed at
another. `jti` (a unique token id) is what makes revocation and replay-tracking
possible. The point is enforcement: PASETO exposes these as rules
(`IssuedBy`, `ForAudience`, `ValidAt`, `Subject`) you attach to the parser;
`golang-jwt` exposes them as parser options (`WithIssuer`, `WithAudience`). Attach
them, do not merely read the values.

### Stateless tokens cannot be un-issued

A signed or encrypted bearer token is valid until it expires; nothing about JWT or
PASETO changes that. If you must revoke before expiry you need extra machinery: a
short TTL with a refresh token, a deny-list keyed by `jti`, or rotating the
signing key to invalidate everything at once. Token lifetime is therefore a
risk-budget decision, not a default. Short TTLs shrink the window an exfiltrated
token is useful; long TTLs shift the cost onto revocation infrastructure.

### Key management dominates real risk

The cryptography is the easy part; keys are where systems actually fail. Signing
and symmetric keys belong in a KMS or secret store, are loaded at startup (a
process that generates a fresh key each boot silently invalidates every token it
ever issued), are never logged, and are rotated through a keyring keyed by `kid`.
Rotation is the entire reason the `kid`-in-footer (PASETO) and `kid`-in-header
(JWT) patterns exist: the verifier must be able to hold several keys at once and
pick the right one per token. And the trust model matters: a symmetric secret
shared across services (HS256, or a shared `v4.local` key) means every holder can
*mint* tokens, not just verify them. For multi-party verification use asymmetric
keys — Ed25519 (`v4.public`), or RSA/ECDSA/EdDSA in JOSE — so only the issuer's
private key can sign.

### The trade-off, stated plainly

JWT wins on ubiquity and interoperability: it is what OIDC, API gateways, and
libraries in every language speak, and it has JWKS for public-key discovery. Its
cost is that safety is pushed onto correct configuration — get `WithValidMethods`
wrong and you have a critical vulnerability. PASETO wins on safe-by-default: it
deletes the entire alg/confusion/none bug class and there is far less to
misconfigure. Its cost is a narrower ecosystem and no standardized public-key
discovery equivalent to JWKS. If you must interoperate with an OIDC provider or a
gateway, you speak JWT and you harden the verifier. If you own both ends, PASETO
removes a whole category of ways to be wrong.

## Common Mistakes

### Letting the token choose the algorithm

Wrong: `jwt.Parse(tokenString, keyFunc)` with no `WithValidMethods`, and a
`keyFunc` that returns a key without inspecting `token.Method`. This is the exact
setup that enables RS256-to-HS256 confusion and `alg:none`.

Fix: pass `WithValidMethods([]string{"RS256"})` (or your one algorithm) so the
method is validated before the `keyFunc` runs, and return a correctly *typed* key
(`*rsa.PublicKey`, not raw bytes).

### Confusing the footer with the implicit assertion

Wrong: assuming the trailing `[]byte` argument to `V4Sign`/`V4Encrypt`/`ParseV4*`
is the footer. It is the implicit assertion. The footer is set separately with
`Token.SetFooter`.

Fix: keep them distinct. Footer = authenticated metadata carried *with* the token
(good for `kid`). Implicit assertion = authenticated context *not* carried with
the token (good for binding). Mixing them up causes silent verification failures
or metadata that never travels.

### Putting secrets in a footer, or thinking local footers are encrypted

Wrong: storing anything sensitive in a footer because "the token is encrypted."
Footers are authenticated plaintext in every version, including `v4.local`.

Fix: footers hold public routing data only. Confidential values go in the
encrypted payload of a `v4.local` token.

### Treating a v4.public (or signed JWT) payload as confidential

Wrong: putting PII or internal ids into a `v4.public` token because it is
"cryptographically protected."

Fix: signing gives integrity and authenticity, not confidentiality. Anyone with
the token can base64url-decode the claims. Use `v4.local` when the payload must
stay secret.

### Forgetting to opt into expiry

Wrong: `NewParserWithoutExpiryCheck()` with no `NotExpired()` rule, or a
`golang-jwt` parse without `WithExpirationRequired()` on tokens that might omit
`exp`. Expired or never-expiring tokens are then accepted.

Fix: add `NotExpired()`/`ValidAt(now)` in PASETO and `WithExpirationRequired()`
in `golang-jwt`, and set an explicit leeway.

### Reading claims without enforcing them

Wrong: calling `GetAudience()` / reading `MapClaims["aud"]` and never comparing
it. The audience is then informational, not enforced.

Fix: attach the rule (`ForAudience`, `IssuedBy`) / option (`WithAudience`,
`WithIssuer`) so a mismatch is an error.

### Mismatching the implicit assertion

Wrong: passing a different implicit assertion at verify time than at sign time —
or `nil` on one side and empty-non-nil on the other — which makes legitimate
tokens fail; or passing `nil` everywhere and losing the binding entirely.

Fix: derive the implicit assertion from the same source on both sides and pass
byte-identical values. Decide deliberately whether you want a binding at all.

### Mishandling keys

Wrong: hardcoding or logging keys; generating a fresh key per process with no
persistence (invalidating all outstanding tokens on restart); or having no `kid`
keyring, so rotation is impossible.

Fix: load keys from a KMS/secret store at startup, keep several in a `kid`-keyed
keyring, and rotate by adding a new key and retiring the old one.

### Sharing a symmetric secret across services

Wrong: HS256 with one shared secret (or one shared `v4.local` key) across
services that only need to *verify*. Any holder can then mint tokens.

Fix: use asymmetric keys (`v4.public`, or RS/ES/EdDSA in JOSE) so only the issuer
signs and everyone else verifies with the public key.

Next: [01-v4-public-token-service.md](01-v4-public-token-service.md)
