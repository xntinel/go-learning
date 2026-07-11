# Embeddings and Vector Search with pgvector — Concepts

An embedding turns a piece of text into a dense vector of floats where geometric
proximity encodes semantic similarity: two sentences that mean nearly the same
thing land close together, two unrelated ones land far apart. That single
property is what powers semantic search, retrieval-augmented generation,
deduplication, clustering, and recommendation. The engineering question this
lesson answers is not "how do I call an embeddings API" — that is one HTTP
request — but "where do the vectors live, how do I search them at scale, and how
do I operate the pipeline that produces them over years as models change". The
senior answer, for a service that already runs Postgres, is usually pgvector:
keep the vectors next to the relational data they describe.

## Why Postgres instead of a dedicated vector database

The reflex is to reach for a managed vector database — Pinecone, Qdrant,
Weaviate — because "vectors need a vector database". For a team that already
operates Postgres, that reflex is often wrong, and the reason is colocation.
Embeddings almost never travel alone; each vector describes a row that also has
an owner, a tenant, a timestamp, an access-control flag, a soft-delete bit. If
the vectors live in a second system you get two sources of truth that must be
kept in sync: every insert becomes a distributed write, every delete becomes a
reconciliation job, and a crash between the two leaves orphans. Putting the
vector in a column of the same row you already store gives you transactional
upserts (the metadata and its embedding commit or roll back together), foreign
keys, joins, and — critically — filtered search ("nearest neighbours *belonging
to this tenant*") inside one query planner, one backup, one failover story, one
system you already know how to operate.

The cost is honesty about what a managed vector DB hides. With pgvector you own
the recall/latency/memory tuning: which index type (HNSW vs IVFFlat), the search
knob (`hnsw.ef_search` / `ivfflat.probes`), index build time and memory, and the
8 KB-page dimension limits described below. A managed service tunes those for
you and charges for it. The decision is the classic operational trade: one more
stateful service to run and reconcile, versus tuning work you take on inside a
database you already run. For most backend teams below the tens-of-millions-of-
vectors scale, colocation wins.

A note specific to an Anthropic-centric stack: Anthropic ships no embeddings
endpoint. Their guidance is to use a third-party embedder, and they point users
to Voyage AI. This lesson uses OpenAI's `text-embedding-3-*` family as the
concrete provider because its Go SDK is the one already wired into this chapter,
but the embedder sits behind a small interface precisely so the provider is
swappable — Voyage, Cohere, or a local model — without touching the storage or
search code.

## What a vector actually is, and what it is not

An embedding is only meaningful *relative to other vectors produced by the same
model at the same output dimension*. The number `0.031` in position 200 has no
standalone meaning; only the geometry of the whole vector, compared against
other vectors from the same model, carries information. Two consequences follow
and both are load-bearing. First, you cannot compare vectors across models: a
`text-embedding-3-small` vector and a `text-embedding-3-large` vector live in
different spaces, and their distance is noise, not similarity — with no error to
warn you. Second, you cannot compare vectors produced at different `Dimensions`
settings, because reducing the dimension changes the space. This is why the
model and the dimension are part of a vector's identity and must be recorded
alongside it.

## Distance metrics and pgvector operators

pgvector exposes three distance operators, and picking the wrong one silently
changes your rankings:

```text
<->   L2 / Euclidean distance
<=>   cosine distance
<#>   negative inner product   (returns -(a . b), so smaller is "more similar")
```

For unit-normalized vectors these collapse: cosine distance and negated inner
product rank identically, and L2 becomes a monotonic function of cosine, so the
top-k order is the same across all three. OpenAI's `text-embedding-3-*` returns
already-normalized vectors, so in practice the choice is mostly about which
*operator class* you build the index with — and the operator in your query MUST
match the index's operator class or the index is ignored entirely. Build with
`vector_cosine_ops` and query with `<=>`; mixing `vector_l2_ops` with a `<=>`
query gives a correct answer by sequential scan and a silently unused index.

## Distance is not similarity

pgvector returns and sorts by *distance*: smaller means closer, and results come
back in ascending order. A user-facing "score" is almost always a *similarity*
in `[0, 1]` where larger is better, which is a derived quantity. For cosine the
conversion is `similarity = 1 - distance`. For the negative-inner-product
operator, similarity is `-distance`. Reporting the raw distance to a caller as a
"relevance score" — so that your best match shows the *lowest* number — is one of
the most common bugs in vector-search code, and the compiler cannot catch it
because both are just `float64`.

## Column types and the 8 KB-page limit

pgvector offers several column types and they are not interchangeable:

- `vector(N)` stores `float32`, 4 bytes per dimension. Its HNSW and IVFFlat
  indexes are capped at **2000 dimensions**, because an index tuple must fit in a
  single 8 KB Postgres page and a 2001-dimension float32 vector no longer does.
- `halfvec(N)` stores `float16`, 2 bytes per dimension, so its indexes reach
  **4000 dimensions** at some precision cost.
- `sparsevec` (sparse) and `bit` (binary) exist for sparse and binary
  embeddings.

The practical bite: `text-embedding-3-large` is 3072 dimensions. Stored as a
plain `vector(3072)` you can hold it, but creating an HNSW index fails with
"column cannot have more than 2000 dimensions for hnsw index". The two fixes are
to store it as `halfvec(3072)` (indexable) or to reduce the model's output
dimension to `<= 2000` via the `Dimensions` parameter. Choosing the column type
from the dimension is a decision you should make in code, not discover in
production.

## ANN indexes are approximate, and both have a recall knob

Exact nearest-neighbour search is O(n): it scans every row. Approximate nearest
neighbour (ANN) indexes trade a little recall for a large latency win, and
pgvector ships two:

- **HNSW** (hierarchical navigable small world, a graph) gives high recall and
  fast queries, at the cost of slower, more memory-hungry builds. It does not
  need data present to build a good structure.
- **IVFFlat** (inverted file with flat lists, cluster-based) builds faster and
  uses less memory, but it must be built *after* representative data is loaded,
  because it learns its cluster centroids (the `lists` parameter) from the data
  present at build time. Building it on an empty or tiny table trains useless
  clusters and tanks recall.

Both trade recall for latency at query time through a knob: `hnsw.ef_search`
(how many candidates the graph search keeps) and `ivfflat.probes` (how many
clusters to scan). Higher means better recall and slower queries. Because the
results are approximate, top-k can miss a true neighbour; do not use ANN results
where exactness is a correctness requirement.

## Why an index gets silently skipped

An ANN index only engages for a query shaped exactly as
`ORDER BY column <op> $1 LIMIT k` with a matching operator class. Drop the
`LIMIT` and the planner cannot use the index — it must sort the whole table, so
it falls back to a sequential scan. Use an operator whose operator class you did
not index, and same result. Add a `WHERE` the planner thinks is cheaper to
satisfy first, and it may skip the index too. The scan is still *correct*, just
O(n); the only symptom is latency that grows with the table. `EXPLAIN` is the
tool that tells you whether the index was used, and reading it is part of
operating pgvector.

## pgx type registration: the trap that fails at query time

pgvector's `Vector` implements `driver.Valuer` and `sql.Scanner`, but pgx needs
the Postgres `vector` OID registered on the connection to encode and decode it.
The helper is `pgxvec.RegisterTypes(ctx, conn)`. With a single connection you
call it once; with a `pgxpool.Pool` you must wire it into
`Config.AfterConnect`, so every connection the pool opens — now and after a
reconnect — can handle vectors. Forget it and nothing fails at startup; you get
an "unable to encode" error the first time you pass a `pgvector.Vector` as a
query argument, often in production under load when the pool grows a new
connection. Registration also requires the `vector` extension to already exist,
so `AfterConnect` typically runs `CREATE EXTENSION IF NOT EXISTS vector` (an
idempotent no-op after the first) before registering.

## float64 in, float32 out

The OpenAI Go SDK returns each embedding as `[]float64`, but pgvector's `vector`
type is `float32`, and `pgvector.NewVector` takes `[]float32`. The conversion is
therefore mandatory and lossy, and it should be a deliberate, single, tested
step rather than something that happens implicitly at three call sites. Treating
the API's `[]float64` as if it could be stored directly is a compile error at
best and a silent per-element truncation at worst.

## Dimension reduction (Matryoshka)

`text-embedding-3-*` are Matryoshka embeddings: the `Dimensions` parameter tells
the API to return a shorter vector that is still useful, trading a little
accuracy for cheaper storage and faster search. Two rules come with it. A
truncated vector should be re-normalized (truncation changes its magnitude), and
the reduced dimension becomes part of the vector's identity — it must match the
column width and every other vector you compare against. You cannot mix 1536-d
and 512-d vectors in one column and expect meaningful distances.

## Batching and cost

Embedding one text per request wastes round-trips and money. The API accepts an
array of inputs and returns an array of results, each tagged with its `Index`,
so you batch many texts into one call and reassemble the outputs by index. But
each request has token and input-count limits, so a batcher needs a cap on
inputs per request and must preserve global order across batches using the
returned `Index`. Order preservation is not optional: your caller passed a list
and expects results aligned to it.

## The operational lifecycle: embeddings are versioned data

The deepest senior point is that an embedding pipeline is a data-engineering
problem, not an API call. Embeddings are model-versioned artifacts. Store the
model (and dimension) that produced each row. Make writes idempotent with
`ON CONFLICT ... DO UPDATE` so a re-run of a backfill does not duplicate rows.
And plan for the day the model changes: because vectors from a new model are
incomparable to the old ones, upgrading the embedder invalidates the entire
column, and you need a backfill/re-embed job that recomputes every vector before
the new ones can be searched against the old. A model column is what lets you
run the old and new spaces side by side during that migration.

## Filtered vector search is genuinely hard

Combining a metadata `WHERE` with ANN is one of the real reasons teams reach for
or away from Postgres as a vector store. Pre-filtering (apply the `WHERE`, then
search) can starve the ANN graph so it returns fewer than k results or misses
neighbours that were pruned early. Post-filtering (search, then apply the
`WHERE`) can under-return when the filter is selective, because the top-k it
searched were mostly filtered out. The levers are partial indexes (an index per
common filter value) and iterative scans (pgvector re-searching until it has
enough post-filter results). There is no free lunch here; knowing the failure
modes is the point.

## Common Mistakes

### Omitting LIMIT and silently scanning the whole table

Writing `ORDER BY embedding <=> $1` with no `LIMIT` means the planner cannot use
the ANN index; it sorts every row. The query is correct and O(n). Always pair
the distance `ORDER BY` with a `LIMIT k`.

### Operator-class mismatch

Building `CREATE INDEX ... USING hnsw (embedding vector_l2_ops)` but querying
with `<=>` (cosine). The index is ignored, and worse, if you *thought* you were
searching by cosine your rankings differ from your intent. The query operator
must match the index operator class.

### Forgetting pgxvec.RegisterTypes

Not calling `pgxvec.RegisterTypes` (or not wiring it into
`pgxpool.Config.AfterConnect`), then hitting "unable to encode" the first time a
`pgvector.Vector` is passed as an argument — at query time, under load, not at
startup.

### Indexing a 3072-d vector as plain vector

Storing `text-embedding-3-large` as `vector(3072)` and trying to build an HNSW
index, which fails with the 2000-dimension limit. Use `halfvec(3072)` or reduce
`Dimensions` to `<= 2000`.

### Treating the []float64 as storable

Assuming the SDK's `[]float64` can be handed to pgvector directly.
`pgvector.NewVector` takes `[]float32`; the conversion is required and lossy, so
make it explicit.

### Mixing models or dimensions in one column

Storing vectors from two models, or two `Dimensions` settings, in the same
column. Distances become meaningless and no error is raised. Record the model
and dimension per row and never mix.

### Reporting raw distance as a score

Showing the pgvector distance to a user as a "relevance score", so the best
match has the lowest number. Convert: `similarity = 1 - distance` for cosine.

### Not normalizing before inner product

Using the `<#>` inner-product operator on non-normalized vectors, or assuming
cosine and L2 give the same ranking on non-normalized data. Normalize, or use
cosine.

### Building IVFFlat on an empty table

Creating an IVFFlat index before loading representative data (or with a poor
`lists`), which trains bad centroids and tanks recall. IVFFlat wants the data
present first; HNSW does not.

### SELECT * including the embedding

Selecting the embedding column into list/search responses you never display,
shipping thousands of floats per row over the wire. Project only the columns the
caller needs.

### Trusting ANN results as exact

Using approximate top-k where correctness matters. `ef_search`/`probes` trade
recall for latency, and a true neighbour can be missed.

Next: [01-embedding-client.md](01-embedding-client.md)
