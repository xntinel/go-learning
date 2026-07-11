# Redis Streams and Consumer Groups — Concepts

Redis Streams is the pragmatic middle ground a backend team reaches for when it
already runs Redis and needs durable, replayable, load-balanced message
consumption without standing up Kafka or NATS. The senior question is never "how
do I call XADD"; it is "what delivery guarantee do I actually get, and what must
I build on top to make it safe." The short answer: Redis Streams gives you
at-least-once delivery and an explicit, per-consumer Pending Entries List, and
almost everything else you associate with a managed queue — redelivery timeouts,
dead-lettering, exactly-once — is code you own. This file lays out the model, the
guarantee, and the four hard decisions (crash recovery, poison handling, memory,
idempotency) that the three exercises then make concrete.

## The log, not the pipe

A Redis Stream is an append-only log of entries, each with a unique, ordered ID
and a set of field/value pairs. That is a different data structure from the two
other Redis primitives teams reach for. Pub/Sub is fire-and-forget: a message is
delivered to whoever is subscribed at that instant and then gone — no
persistence, no replay, no record that anyone processed it. A list used as a
queue (`LPUSH`/`BRPOP`) does persist, but a popped element is removed from the
list immediately, so there is no per-consumer tracking, no way for a second
independent reader to see the same item, and no replay. A Stream keeps every
entry until you trim it, addresses entries by ID, supports range scans, and — via
consumer groups — tracks exactly which entries each consumer has taken but not
yet acknowledged. Choose Streams when you need replay, fan-out to several
independent consumer groups, and load-balanced-but-tracked consumption within a
group.

Entry IDs are the backbone. An ID looks like `1692632639151-0`:
milliseconds-since-epoch, a hyphen, then a per-millisecond sequence number. IDs
are monotonically increasing and, when you append with the special ID `*`, the
server assigns the next one for you. Because IDs are ordered you get total
ordering and range queries (`XRANGE stream - +`). A handful of special IDs carry
the whole semantics of group reads, and confusing them is the most common
beginner error: `>` means "new messages never delivered to any consumer in this
group"; `0` (or a range `0`..`+`) re-reads *this consumer's own* pending
entries; `$` means "only entries added after now" and is used with the
non-group `XREAD`, not with groups.

## Consumer groups: fan-out between groups, load-balance within one

A consumer group is a named cursor over a stream plus a set of named consumers.
The delivery rule has two halves. Within a single group, each new entry is handed
to exactly one consumer — this is the competing-consumers pattern, and it is how
you scale horizontally: run N worker processes with the same group name but
distinct consumer names, and the stream's throughput is spread across them. Across
groups, each group receives the full stream independently — this is fan-out, and
it is how an `orders` stream can drive both a `fulfilment` group and an
`analytics` group without either seeing the other's acknowledgements. Adding a
consumer to a group increases parallelism; adding a group adds an independent
subscriber.

Creating the group is idempotent only if you handle one specific error.
`XGROUP CREATE stream group id MKSTREAM` creates the stream (if missing) and the
group in one call, but if the group already exists it returns a `BUSYGROUP`
error. On every restart after the first, that call will "fail" — and the correct
behavior is to treat `BUSYGROUP` as success and carry on. The start ID matters:
`0` replays the entire existing stream to the new group; `$` starts the group at
"now", so pre-existing entries are never delivered to it.

## The guarantee is at-least-once, and the PEL is why

`XREADGROUP GROUP g c COUNT n STREAMS s >` does two things atomically: it
delivers up to `n` never-before-delivered entries to consumer `c`, and it records
each of those entries in `c`'s Pending Entries List (PEL). An entry in the PEL is
owned by that consumer and invisible to its peers, but it is *not* removed from
the stream and it is *not* considered done. It stays pending until the consumer
calls `XACK stream group id`, which removes it from the PEL. That is the entire
contract: between delivery and `XACK`, the entry is neither redelivered to a peer
nor lost.

Two consequences follow, and they define the rest of the lesson. First, if a
consumer crashes after reading but before acknowledging, its PEL entries sit
there — owned by a consumer that no longer exists — forever. Redis has no
visibility timeout. Unlike SQS or JetStream, it will never automatically re-queue
an un-acked message after some interval. Recovery is your job. Second, because
recovery means the same entry can be delivered a second time (to whoever reclaims
it), processing must be idempotent: applying the same event twice must not double
a balance or send two emails. At-least-once plus reclaim equals
"exactly-once-more-delivered eventually"; only idempotency turns that into
correct behavior.

## Crash recovery: XAUTOCLAIM, XCLAIM, and min-idle-time

Turning at-least-once into a durable worker means writing the recovery path
yourself. There are two moving parts. On startup, a consumer re-reads its *own*
PEL — the entries it took but never acked before the last restart — by calling
`XREADGROUP ... STREAMS s 0` with the literal ID `0` instead of `>`. That returns
the consumer's pending entries (not new ones) so it can finish them. On an
ongoing basis, a consumer reclaims entries stranded by *other*, crashed consumers.

`XAUTOCLAIM` is the workhorse for that second job. It scans a group's PEL from a
cursor and, in one call, transfers ownership of every entry idle longer than a
`min-idle-time` to the calling consumer, returning the claimed messages, a next
cursor, and (at the protocol level) a list of IDs that were trimmed out from under
the PEL. You page with the cursor: start at `0`, feed the returned cursor into the
next call, and stop when it comes back as `0-0`. `XCLAIM` is the targeted
alternative: you first learn specific IDs from `XPENDING`, then claim exactly
those. Use `XAUTOCLAIM` for a periodic janitor that sweeps everything stale; use
`XCLAIM` when you already know which IDs to move.

`min-idle-time` is the safety interlock. "Idle" is the time since the entry was
last delivered or claimed. Requiring a minimum idle before reclaiming prevents two
*live* consumers from stealing each other's in-flight work: a message being
actively processed has a small idle time and is not eligible. Set it comfortably
above your worst-case processing time (for example, several times the p99 handler
latency). Claiming resets the entry's idle time to zero and — crucially —
increments its delivery counter.

## Poison messages and the dead-letter stream

Every delivery bumps a counter. Each `XREADGROUP` delivery and each `XCLAIM`/
`XAUTOCLAIM` (without the `JUSTID` option) increments an entry's delivery count,
surfaced as `RetryCount` in `XPENDING`/`XPENDINGEXT`. This is what makes a poison
message dangerous: a payload whose handler always fails is never acked, so it is
reclaimed again and again, its delivery count climbing forever while it starves
throughput — the janitor keeps re-serving the one message that can never succeed.
Redis will not dead-letter it for you.

The fix is a max-deliveries ceiling. When an entry's delivery count crosses a
threshold, route it off the hot path: `XADD` it to a separate dead-letter stream
with the original ID, the error, and the attempt count in its fields, then `XACK`
the original so it leaves the source PEL. Because the ack removes it from the PEL,
a later reclaim pass cannot find it again, so the routing is naturally idempotent:
the poison entry lands in the dead-letter stream exactly once and stops consuming
worker time. The routing decision itself — "deliveries at or above max means
dead-letter, else retry" — is a pure function of two integers and is the easiest
thing in the whole system to unit-test at its boundary.

## Memory and trimming

A stream lives in RAM and grows without bound unless you cap it. Acknowledging an
entry does *not* free its memory — `XACK` only clears the PEL; the entry stays in
the stream. Trimming is what reclaims memory, and you trim on write with `MAXLEN`
(cap by entry count) or `MINID` (drop entries older than an ID, i.e. time-based).
Prefer approximate trimming: `XADD ... MAXLEN ~ 100000` lets Redis drop whole
macro-nodes when convenient instead of doing exact, O(N)-ish work on every append.
Exact `MAXLEN 100000` is correct but slower under load.

Trimming has a sharp edge that interacts with the PEL: it can evict an entry that
is still pending in some slow group. `MAXLEN`/`MINID` do not consult any group's
PEL — an entry trimmed while pending is gone from the stream even though a
consumer still "owed" work on it, and `XAUTOCLAIM` reports its ID as deleted. So
size the cap against worst-case consumer lag: the cap must comfortably exceed how
far behind your slowest group can fall, or you will silently drop messages that
were pending.

## Blocking reads, batching, and connection pools

`XREADGROUP` can long-poll. With `BLOCK <ms>` and no new entries, the call parks
until an entry arrives or the timeout elapses (returning a nil reply, surfaced in
go-redis as `redis.Nil`). `BLOCK 0` blocks forever — convenient in a script,
dangerous in a service, because it pins a connection from the pool for as long as
the stream is quiet, and under many consumers that exhausts the pool. `COUNT`
batches: reading `COUNT 50` delivers up to fifty entries at once, which lowers
per-message overhead but also means up to fifty entries sit in one PEL if that
consumer dies mid-batch. Tune `COUNT` and `BLOCK` against latency, pool pressure,
and how much in-flight work you are willing to strand on a single crash. (In the
tests here we pass a negative `Block`, which go-redis translates to "omit the
BLOCK argument entirely" — a non-blocking read — so the assertions never depend on
blocking semantics.)

## Observability

`XPENDING stream group` gives a summary: total pending, the lowest and highest
pending IDs, and a per-consumer pending count — enough to spot a consumer that has
stopped acking. `XPENDING` in extended form (`XPENDINGEXT` in go-redis) returns
one record per pending entry with its consumer, idle time, and delivery count —
the signal for finding stuck or repeatedly-failing entries. `XINFO STREAM`,
`XINFO GROUPS`, and `XINFO CONSUMERS` expose length, last-delivered ID, and group
lag. Alert on growing PEL size and on individual entries with a high idle time or
delivery count; both mean a consumer is stuck.

## go-redis specifics

These exercises use `github.com/redis/go-redis/v9`. Every command returns a typed
command value whose `.Result()` yields `(value, error)` and whose `.Err()` yields
just the error. `XReadGroup` returns an `*XStreamSliceCmd` whose result is a
`[]redis.XStream` (one per stream, each holding `[]redis.XMessage`);
`XMessage.Values` is a `map[string]interface{}`, and because Redis is untyped,
every value comes back as a *string* — you decode numbers and times deliberately.
`redis.Nil` is a sentinel meaning "no data / empty reply" and must be
distinguished from a real error with `errors.Is(err, redis.Nil)`. `XAutoClaim`
returns `(messages []redis.XMessage, cursor string, err error)` — note go-redis's
typed `XAutoClaim` discards the deleted-IDs third element the protocol returns, so
if you need to know which pending IDs were trimmed away you must reconcile against
the stream yourself. Group creation is `XGroupCreateMkStream`, whose `BUSYGROUP`
error you detect by string (`strings.Contains(err.Error(), "BUSYGROUP")`) because
there is no dedicated typed error.

## Common Mistakes

### Assuming exactly-once and building a non-idempotent handler

Wrong: writing a handler with side effects (increment a balance, charge a card,
send an email) as if each entry arrives exactly once. The first crash triggers a
reclaim, the entry is delivered again, and the side effect happens twice.

Fix: make processing idempotent — dedupe on the entry ID or a business key, use
`INSERT ... ON CONFLICT DO NOTHING`, or make the operation naturally repeatable.
At-least-once is the contract; idempotency is what makes it safe.

### Never acknowledging, or acking the wrong ID

Wrong: processing an entry and forgetting `XACK`, or acking an ID from a different
stream. The PEL grows without bound, memory and lag climb, and everything "looks"
fine because processing is happening.

Fix: `XACK stream group id` exactly the entries you finished, in the same stream
and group you read from. Monitor `XPENDING` count — a steadily rising pending
count with healthy throughput means missing acks.

### Expecting an automatic redelivery timeout

Wrong: assuming that, like SQS's visibility timeout, Redis re-queues an un-acked
entry after a while, and therefore writing no recovery path.

Fix: there is no timeout. Build the recovery path: on startup re-read your own PEL
with ID `0`, and run a periodic `XAUTOCLAIM` janitor gated by `min-idle-time`. A
single crashed consumer strands its in-flight entries permanently otherwise.

### Reclaiming with too small a min-idle-time

Wrong: `XAUTOCLAIM ... 0` (or a min-idle far below processing time), so live
consumers steal each other's actively-processing entries and process them twice.

Fix: set `min-idle-time` well above worst-case processing latency. Only entries
that have plausibly been abandoned should be eligible.

### Using `>` to re-read your own pending entries

Wrong: expecting `XREADGROUP ... STREAMS s >` to return your un-acked entries
after a restart. `>` only ever returns brand-new, never-delivered entries.

Fix: read with the literal ID `0` (`STREAMS s 0`) to recover this consumer's own
PEL; use `>` only for new work.

### No poison ceiling

Wrong: reclaiming failed entries forever with no maximum. A permanently-failing
payload is re-served endlessly, its delivery count climbing, starving the group.

Fix: read `RetryCount` from `XPENDINGEXT`, and once it reaches a threshold, `XADD`
the entry to a dead-letter stream and `XACK` the original.

### Unbounded or exactly-trimmed streams

Wrong: omitting `MAXLEN`/`MINID` (RAM balloons) or using exact `MAXLEN N` under
high write load (slower trims).

Fix: cap with approximate trimming — `MAXLEN ~ N` — on `XADD`, so Redis trims
efficiently by macro-node.

### Trimming without accounting for lag

Wrong: setting `MAXLEN` near the size of the working set, so trimming evicts
entries still pending in a slow group's PEL — those messages are lost from the
stream even though they were "pending".

Fix: size the cap far above the worst-case lag of your slowest consumer group.
Trimming ignores the PEL.

### BLOCK 0 on a shared client

Wrong: `XREADGROUP ... BLOCK 0` on a pooled client. Each idle consumer pins a
connection forever; many consumers exhaust the pool.

Fix: use a bounded block (for example five seconds) and loop, or a non-blocking
read when you are draining.

### Crashing on BUSYGROUP at boot

Wrong: treating any error from `XGROUP CREATE` as fatal, so the worker crashes on
every boot after the first (the group already exists).

Fix: detect `BUSYGROUP` and treat it as success; only other errors are fatal.

### Forgetting values come back as strings

Wrong: storing an int in an entry and reading `Values["amount"].(int)` — it is a
string, and the assertion panics or the parse silently misbehaves.

Fix: decode deliberately — `strconv.ParseInt(v.(string), 10, 64)` — and validate,
returning a wrapped sentinel error on malformed input.

### Ignoring entries deleted out from under the PEL

Wrong: assuming everything `XAUTOCLAIM` should have claimed comes back as a
message. Entries trimmed away while pending are reported as deleted IDs, and
go-redis's typed `XAutoClaim` drops that list — leaving phantom pending
references.

Fix: be aware that reclaim can encounter already-deleted entries; reconcile PEL
IDs against the live stream when it matters, and do not assume the claimed set
equals the pending set.

Next: [01-consumer-group-worker.md](01-consumer-group-worker.md)
