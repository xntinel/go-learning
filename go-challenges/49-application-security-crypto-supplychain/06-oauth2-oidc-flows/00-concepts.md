# OAuth2 and OIDC Authentication Flows — Concepts

A senior backend engineer almost never writes the underlying crypto for OAuth2,
but wires the flows constantly: the login redirect, the callback, the
service-to-service token, and the single most consequential line of code in the
whole system — the moment a resource server decides whether to trust an inbound
token. Getting that decision wrong is how teams get breached. This file is the
conceptual foundation for three independent exercises: the browser-facing
authorization-code-with-PKCE flow, offline OIDC ID-token verification, and the
machine-to-machine client-credentials grant that most backend OAuth2 code
actually is.

Every exercise replaces the network dependency with an `httptest` fake or a
locally minted JWT, so the flow *logic* is what gets tested, not a live identity
provider. These lessons import `golang.org/x/oauth2` and
`github.com/coreos/go-oidc/v3` — external modules — so the offline gate cannot
build them. The bar is different (see the last exercise's note): real verified
APIs, honest concepts, and prose that matches the code exactly.

## Concepts

### Authorization is not authentication

OAuth2 is a *delegated authorization* protocol. Its deliverable is an access
token: a credential that authorizes the bearer to call a resource server on some
user's behalf, within some scope. It says nothing, by contract, about *who* the
user is. The access token is opaque to the client — the client is not supposed to
parse it or draw identity conclusions from it. It is a bearer ticket for an API.

OpenID Connect (OIDC) is a thin authentication layer bolted on top of OAuth2. Its
deliverable is the ID token: a signed JWT that asserts *who authenticated*, issued
to *your* client (its audience is your `client_id`), meant to be verified and
consumed by the client, not forwarded to an API. So there are two tokens with two
audiences and two purposes. Conflating them is a category error with real
consequences:

- Using an access token to identify the user (`token.AccessToken` as a user id,
  or decoding it to read `sub`) trusts a value whose format and contents the
  authorization server may change at will, and which was never meant to carry
  identity to the client.
- Sending an ID token to a resource server as if it were an access token hands
  the API a credential scoped to the wrong audience; a correct API rejects it,
  and a lax one now trusts a token minted for a different purpose.

The mental model: **access token → resource server (authorization); ID token →
client (authentication).**

### Why the authorization code flow exists, and why PKCE closes its last gap

The authorization code flow exists so that the token never travels through the
browser or the URL. The user agent only ever carries a short-lived, single-use
*authorization code*; the client redeems that code for tokens over a direct
back-channel HTTPS call to the token endpoint. The older implicit flow, which
returned the access token in the URL fragment, is dead precisely because that
fragment leaked into history, logs, and referrers.

The residual threat is code interception: on a mobile or SPA client, a malicious
app registered for the same custom-scheme redirect can grab the code as it comes
back. PKCE (Proof Key for Code Exchange, RFC 7636) closes this. At authorize
time the client generates a high-entropy random `code_verifier`, and sends only
its SHA-256 hash — the `code_challenge`, with `code_challenge_method=S256`. At
token time the client presents the raw `code_verifier`; the authorization server
re-hashes it and checks it matches the challenge it stored. A stolen code is
useless without the verifier, which never left the client.

Crucially, PKCE is now recommended for **all** clients — including confidential
server-side web backends — by the OAuth 2.0 Security Best Current Practice
(RFC 9700), not just public/mobile clients. Treating PKCE as optional for a web
backend is outdated advice. In `golang.org/x/oauth2` the two halves are two
distinct options that are easy to swap by mistake:
`oauth2.S256ChallengeOption(verifier)` belongs on `AuthCodeURL` (it sends the
challenge), and `oauth2.VerifierOption(verifier)` belongs on `Exchange` (it sends
the verifier). Swapping them silently disables PKCE.

### state and nonce solve two different problems, and both are mandatory

`state` is a CSRF token. It binds the callback to the *browser session that
started the flow*. Your `/login` handler generates a random `state`, remembers it
tied to that browser (a cookie-keyed server-side session), and on the callback
rejects any request whose `state` does not match. Without it, an attacker can
trick a victim's browser into completing a login the attacker initiated (login
CSRF / session fixation). Compare `state` in constant time.

`nonce` is an OIDC replay defense. It binds the returned *ID token* to that same
authorization request. The client puts a random `nonce` on the authorize URL; the
IdP echoes it into the ID token's `nonce` claim; the client checks it matches on
return. This prevents an attacker from injecting a previously captured ID token.
The trap: **go-oidc verifies the signature, issuer, audience, and expiry for you,
but it does NOT verify the nonce.** The `IDToken.Nonce` field is exposed
specifically so *you* compare it to the value you stored. Forgetting that check
silently disables replay protection.

### ID token verification is a fixed checklist

Verifying an ID token is not "decode and read the claims." It is a checklist, and
skipping any one item turns the token into an attacker-controllable blob:

1. **Signature** valid against the IdP's published keys (JWKS).
2. **Algorithm** is an *expected asymmetric* algorithm. Never `alg: none`. Never
   accept a symmetric alg (HS256) where an asymmetric one (RS256/ES256) is
   expected — that is the JOSE alg-confusion footgun, where an attacker signs an
   HS256 token using the *public* key as the HMAC secret. Pin
   `oidc.Config.SupportedSigningAlgs`.
3. **Issuer** (`iss`) matches the discovered issuer string exactly.
4. **Audience** (`aud`) contains your `client_id`.
5. **Expiry / issued-at** within a small clock skew.
6. **Nonce** matches the value you stored (your code, not the library).

The JWT header's `kid` selects which JWKS key verifies the signature.

### UserInfo is not a shortcut for identity

The UserInfo endpoint returns claims over a network call authorized by the access
token. It is convenient, but it is not, by itself, integrity-protected the way a
*verified* ID token is: a compromised or man-in-the-middled transport, or code
that trusts the response without checking its provenance, has no signed assertion
to fall back on. The correct pattern is: verify the ID token first to establish
identity, then use UserInfo only to *fetch additional* claims. If you must trust a
UserInfo response standalone, request a signed (JWT) UserInfo response and verify
it, or bind it via `at_hash`. `at_hash` is the leftmost half of the SHA-256 of the
access token, base64url-encoded, carried in the ID token; verifying it proves the
ID token and the access token were issued together (go-oidc's
`IDToken.VerifyAccessToken`).

### JWKS rotation and caching

Production verification pulls the IdP's public keys from its JWKS endpoint.
go-oidc's `RemoteKeySet` fetches and caches them and refetches on an unknown
`kid`, which is how it survives key rotation. Two failure modes bracket the
correct behavior: a verifier that refetches JWKS on *every* request (or on every
unknown kid with no rate limit) is a self-inflicted DoS vector; one that caches
keys *forever* breaks every login the moment the IdP rotates its signing key.
`StaticKeySet` holds a fixed set of `crypto.PublicKey` values — perfect for tests
and for pinned keys, wrong for a production IdP that rotates. The verification
exercise uses `StaticKeySet` precisely because it needs to be offline and
deterministic.

### Token lifecycle: let a TokenSource own refresh

Access tokens are deliberately short-lived. For user flows a refresh token
(obtained with `access_type=offline` / a consent prompt on some IdPs) mints new
access tokens without re-prompting the user. For service flows the client simply
re-runs the client-credentials grant. Either way, the correct concurrency-safe
pattern is an `oauth2.TokenSource`: `oauth2.ReuseTokenSource` memoizes a token and
transparently refreshes only when it has expired. Scattering manual
`token.Expiry` checks and hand-rolled refresh across handlers invites races and
duplicate refreshes; a single shared token source does not. `token.Valid()`
already accounts for a small early-expiry buffer, so callers never have to.

### Client credentials: service identity, no user, no ID token

The client-credentials grant is the flow most backend OAuth2 code actually is: a
service authenticating *as itself* to another API. There is no user, no browser,
and no ID token — only an access token representing the service. `AuthStyle`
controls how the client sends its id and secret: `AuthStyleInHeader` uses HTTP
Basic, `AuthStyleInParams` puts them in the POST body. The zero value
auto-detects by trying one and retrying, which works but costs an extra failed
round trip and produces a confusing 401 in the IdP logs the first time; pinning
`AuthStyle` to what your IdP expects avoids that. `EndpointParams` carries extra
fields many IdPs require, such as `audience` or a resource indicator.

### Redirect URI handling is a security boundary

The authorization server must exact-match the `redirect_uri` against what was
registered — no substring, no trailing-slash or case tricks, no open-redirect
parameters. On your side, the login session that holds `state`, `nonce`, and the
PKCE `verifier` must be bound to the specific browser (a cookie-keyed
server-side entry, not a global variable), **single-use**, and **TTL-bounded** —
just as the authorization code itself is single-use and short-lived. A ledger
that enforces single-use and expiry defeats both CSRF and replay of an
intercepted code.

## Common Mistakes

### Storing state/nonce/verifier somewhere not bound to the browser session

Wrong: keeping the PKCE verifier, state, or nonce in a package-level variable, or
in a cookie with no integrity protection. Concurrent logins collide, and the CSRF
guarantee evaporates.

Fix: store them server-side in a ledger keyed by a random session id that lives in
an `HttpOnly`, `Secure`, `SameSite` cookie; make each entry single-use and
TTL-bounded.

### Not validating state, or comparing it non-constant-time

Wrong: skipping the `state` check on callback, or `if state == sess.state`,
leaving login CSRF open and leaking timing.

Fix: reject a missing or mismatched `state`; compare with
`subtle.ConstantTimeCompare`.

### Assuming go-oidc validates the nonce

Wrong: calling `verifier.Verify` and trusting the result, believing the nonce was
checked. It was not.

Fix: after `Verify` succeeds, compare `idToken.Nonce` to the nonce you stored for
this session; reject on mismatch.

### Using the access token as identity, or trusting UserInfo without an ID token

Wrong: reading identity out of `token.AccessToken`, or calling UserInfo and
trusting it without ever verifying the signed ID token.

Fix: verify the ID token to establish identity; use UserInfo only for extra
claims, and bind via `at_hash` if you rely on it.

### Accepting alg:none or an unexpected algorithm

Wrong: leaving `SupportedSigningAlgs` unset and accepting whatever the token
claims, enabling `alg: none` and RS/HS alg-confusion.

Fix: pin `oidc.Config.SupportedSigningAlgs` to the exact asymmetric algorithms
your IdP uses (e.g. `[]string{oidc.RS256}`).

### Disabling checks to make a test pass

Wrong: setting `SkipClientIDCheck`, `SkipExpiryCheck`, `SkipIssuerCheck`, or
`InsecureSkipSignatureCheck` and shipping it. Each one deletes a checklist item.

Fix: keep every check on in production; drive `oidc.Config.Now` with a fixed clock
to test expiry deterministically instead of skipping it.

### Swapping the two PKCE options

Wrong: passing `oauth2.S256ChallengeOption` to `Exchange`, or
`oauth2.VerifierOption` to `AuthCodeURL`. Either swap silently breaks PKCE — the
server never gets the challenge, or never gets the verifier.

Fix: `S256ChallengeOption` on `AuthCodeURL`; `VerifierOption` on `Exchange`.

### Hand-rolling token refresh

Wrong: checking `token.Expiry` and refetching by hand in each handler, racing
concurrent refreshes.

Fix: build one `oauth2.TokenSource` (`ReuseTokenSource`, or
`clientcredentials.Config.TokenSource`) and share it; it refreshes exactly once,
concurrency-safe.

Next: [01-authcode-pkce-flow.md](01-authcode-pkce-flow.md)
