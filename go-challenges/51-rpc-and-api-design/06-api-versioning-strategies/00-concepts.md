# API Versioning Strategies — Concepts

Versioning is not a routing detail; it is a contract-management discipline. The
expensive part of an API version is never shipping v2 — it is carrying v1 and v2
*simultaneously* for years while you migrate clients you do not control. A senior
owns that whole lifecycle: choosing an axis that matches who consumes the API and
how much leverage you have over them, making most change additive so it needs no
new version at all, and running a disciplined, machine-readable deprecation path
so clients can automate their own migration instead of discovering breakage in
production. This file is the conceptual foundation for the three independent
exercises that follow: two client-facing versioning axes side by side, an
automatable deprecation-and-sunset program, and the in-band schema evolution you
should reach for before either.

## The three axes are orthogonal

There are three places you can put a version, and they solve different problems.

*URI-path versioning* (`/v1/orders`, `/v2/orders`) is the most discoverable and
curl-friendly, and it keeps the version in the cache key so shared caches stay
correct with no extra work. Its cost is conceptual and organizational: a URI is
supposed to name a *resource*, and `/v1/orders` and `/v2/orders` are the same
order in two dresses. It forces every client to rewrite URLs on a bump, and it
tempts teams to bump the *whole* API to v2 because one endpoint changed —
multiplying the maintenance surface for a one-line diff.

*Media-type / header versioning* (`Accept: application/vnd.acme.v2+json`) keeps
URLs stable and versions the *representation* rather than the resource. The URL
`/orders` never changes; the client asks for the shape it understands. The cost:
the version is invisible in a browser address bar and in most access logs, it is
awkward to test by hand, and it is a cache landmine — a response whose body
varies by `Accept` MUST send `Vary: Accept` or a shared cache/CDN will serve one
version's body to another version's client.

*In-band schema evolution* changes nothing about routing at all. You evolve one
JSON (or protobuf) contract compatibly so old and new clients share it. This is
the axis you should reach for first, and most of the time it is the only one you
need.

## The senior default: do not create a new version

A new major version is an admission that you are breaking the contract, and the
true price of that admission is the multi-year window in which you run N versions
at once. So the default posture is: *do not version.* Most changes can be made
additive and both backward- and forward-compatible. When you genuinely must
break, version the *smallest thing that changed* — one representation, one
endpoint — not the entire surface.

Backward and forward compatibility are distinct and worth separating in your
head. *Backward* compatibility means a new server can still read old clients'
requests. *Forward* compatibility means old clients can still read a new server's
responses. The tolerant-reader pattern — ignore fields you do not recognize — is
what buys forward compatibility: an old client handed a payload with new keys
keeps working because it drops what it does not understand. Strict decoding
deliberately gives up forward compatibility in exchange for catching first-party
bugs, which is why it belongs on internal boundaries and never on the public edge.

## What "additive" means, precisely

Additive means: add optional fields; never remove a field a client reads; never
rename a field a client writes. Removing a field breaks every reader that
dereferences it. Renaming is a remove plus an add, so it breaks writers of the
old name and readers expecting it — a rename is safe only behind an alias/dual-read
window where the server accepts both names for a deprecation period. In JSON,
`omitempty` plus a tolerant reader carries almost all real-world evolution: you
add a field, old clients ignore it, new clients default it when it is absent.

The canonical non-JSON case is protobuf, and its rules are stricter because the
wire format identifies fields by *number*, not name. The field number is the wire
identity; the name is a source-level label you may change freely. The hard rules:
never reuse a retired field number, never renumber a field to "rename" it, mark
removed numbers and names `reserved` so no future edit reintroduces them, add only
optional fields, and change a field's type only when the encodings are
wire-compatible (int32 to int64 parses, but truncates values above INT32_MAX). A
reused number silently corrupts data for old readers, which is why `buf breaking`
exists to mechanically reject it. This is also why gRPC/Connect services version
the *package* (`foo.v1` to `foo.v2`) rather than a URL path.

## A deprecation must be machine-readable

A deprecation announced only in a blog post or a changelog is not a program; it
is a hope. Clients cannot automate against prose. The HTTP standards give you
three machine-readable signals, and using them lets a client detect and schedule
its own migration.

RFC 9745 defines the `Deprecation` response header, which announces that a
resource is or will be deprecated. Its value is a Structured-Fields *Date* item,
serialized as an at-sign followed by the integer Unix seconds, e.g.
`Deprecation: @1767225600`. RFC 8594 defines the `Sunset` header, which announces
*when* the resource will stop responding; its value is an HTTP-date (IMF-fixdate,
always in GMT), e.g. `Sunset: Wed, 01 Jul 2026 00:00:00 GMT`. The two formats are
different on purpose: one is a Structured-Fields integer date, the other is the
classic HTTP date used by `Date`, `Expires`, and `Last-Modified`. Alongside them
a deprecation link relation (`Link: <docs>; rel="deprecation"`) points clients at
migration guidance they can follow programmatically.

There is an ordering invariant you must not violate: the Sunset instant MUST NOT
be earlier than the Deprecation instant (RFC 9745). Conceptually, deprecation
without a sunset is a warning with no deadline; the sunset is the enforceable
removal date. After it passes, requests should *fail closed* — return `410 Gone`
with a problem body — not silently keep working, because a version that "still
works after sunset" trains clients to ignore your sunset entirely.

## Cache and negotiation correctness

The single most common way to break header/media-type versioning is to forget
`Vary: Accept`. When the response body is a function of the `Accept` header, a
shared cache that keys only on the URL will cache whichever version it saw first
and hand it to everyone. `Vary: Accept` tells the cache the `Accept` header is
part of the key. URI-path versioning sidesteps this entirely because the version
is already in the URL and therefore in the cache key — one of its genuine
advantages.

## Deprecation is an operational program, not a code change

Emitting the headers is the easy 10%. The other 90% is operational. You need
per-version and per-client traffic metrics to know when it is actually safe to
sunset — sunsetting a version with live traffic you cannot see is exactly how you
take down a paying customer. You need staged timelines (announce, then emit
deprecation headers, then scheduled brownouts, then sunset) so clients have
runway. And you need real communication to consumers you may not control. Treat
every breaking change as a multi-quarter program with dashboards on per-version,
per-client traffic — not a one-line diff.

## Common Mistakes

### Bumping the whole API because one endpoint changed

Wrong: a single field changes on one resource, so the team ships `/v2` for the
entire surface and now maintains two copies of everything.

Fix: version the smallest thing that changed — one representation via media type,
or better, evolve that one field additively so nothing needs a new version.

### Treating a JSON rename or removal as safe

Wrong: renaming `customer` to `customer_id`, or deleting a field "nobody uses,"
in place. Removing breaks readers; renaming breaks writers of the old name.

Fix: additive-only. Add the new field, keep reading the old name for a
deprecation window (a tolerant `UnmarshalJSON` that accepts both), and drop the
old one only after traffic to it is zero.

### Reusing a protobuf field number

Wrong: removing a field and later assigning its number `7` to a new field, so old
readers decode new bytes into the wrong field. Or renumbering a field to rename it.

Fix: names are free to change; numbers are forever. Mark removed numbers and
names `reserved` and let `buf breaking` enforce it in CI.

### Emitting Deprecation and Sunset in the same format

Wrong: writing the Sunset date as `@<unix>` too, or the Deprecation value as an
HTTP-date. Clients then cannot parse one of them.

Fix: `Deprecation` is `@<unix-seconds>` (Structured-Fields Date); `Sunset` is an
HTTP-date in GMT (`http.TimeFormat`). They are deliberately different.

### Sunset earlier than Deprecation, or sunsetting blind

Wrong: shipping a `Sunset` before the `Deprecation` instant, or hard-cutting a
version with no dashboard showing who still calls it.

Fix: enforce the ordering invariant at construction time, and never schedule a
sunset without per-client traffic visibility.

### Header versioning without Vary: Accept

Wrong: varying the body by `Accept` but omitting `Vary: Accept`, so a CDN caches
the v2 body and serves it to v1 clients.

Fix: always send `Vary: Accept` on any response whose content is negotiated.

### DisallowUnknownFields on the public edge

Wrong: calling `Decoder.DisallowUnknownFields` on public ingress, so every
additive change a partner makes to their request becomes a 400 and forward
compatibility is destroyed.

Fix: tolerant decoding on the public edge; strict decoding only on internal
boundaries where an unexpected key really is a bug.

### Assuming ParseMediaType tolerates garbage

Wrong: a handler that feeds a raw `Accept` value into `mime.ParseMediaType` and
does not handle the error path, so a malformed header returns 500 instead of
degrading to a default version.

Fix: on a parse error, fall back to the configured default version and serve.

Next: [01-uri-vs-header-version-routing.md](01-uri-vs-header-version-routing.md)
