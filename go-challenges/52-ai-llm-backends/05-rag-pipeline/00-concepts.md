# Building a RAG Pipeline — Concepts

Retrieval-augmented generation is sold as a model trick. It is not. It is a
data-and-plumbing subsystem with exactly one non-deterministic call at the very
end, and that is precisely where a senior backend engineer earns their keep. The
shape of the pipeline is fixed: `chunk -> embed -> store -> retrieve -> re-rank
-> assemble -> ground -> validate`. Every stage upstream of the model is ordinary
backend engineering you can unit-test, benchmark, and reason about with
invariants: chunk boundaries, byte-offset reconstruction, cosine math, top-k
ordering, token budgets, citation validation. Only the final completion is
stochastic. Answer quality is dominated by retrieval quality, not by the model,
so the engineering leverage lives in the retrieval and grounding layers — and all
of it is deterministic code you own. This file is the conceptual foundation for
the three independent exercises that follow: a document chunker, a vector
retriever with MMR re-ranking, and a grounding stage with a citation guardrail.

## Concepts

### RAG is a subsystem, not a prompt

Draw the boundary correctly and the design falls out. The corpus is chunked once
(offline), each chunk is embedded into a vector and stored, and at query time you
embed the question, retrieve nearest chunks, re-rank them for diversity, pack the
highest-value ones under a token budget, render a grounded prompt, call the model,
and validate the answer's citations against what you actually retrieved. Exactly
one of those steps — the completion — is non-deterministic. The rest is a
retrieval subsystem you can gate with a normal test suite. When answers are bad,
the instinct is to swap models or tune the prompt; the higher-leverage move is
almost always to fix retrieval, because a model cannot ground an answer on a
passage it never received.

### Chunking trade-offs

Chunk size is a genuine trade-off, not a default. Too large and a single
embedding averages several topics together — the vector points nowhere in
particular and similarity search degrades — while the chunk wastes context budget
when retrieved. Too small and you fragment meaning: a sentence loses the
paragraph that gave it context, and cross-sentence reasoning becomes impossible.
Overlap between adjacent chunks preserves boundary context (a fact split across a
chunk edge survives in at least one chunk) at the cost of duplication and storage.
The right default is to chunk on semantic boundaries first — paragraphs, then
sentences, then words — and only fall back to a hard cut when a single segment
exceeds the limit. Critically, store the byte offsets `[start, end)` of every
chunk. Offsets let you cite the exact source span and reconstruct the original
document, which turns "the model said X" into "the model said X, from characters
1400-1750 of doc-42."

A note on units: character and word counts are only a proxy for model tokens. The
real tokenizer differs per model and per language, so any offline token estimate
(this lesson uses roughly four characters per token) is deliberately conservative.
Budget with headroom rather than trusting the estimate.

### Embedding-space geometry: similarity vs distance

Similarity search is cosine geometry, and the single most expensive mistake is
confusing cosine *similarity* with cosine *distance*. Cosine similarity ranges
from -1 (opposite) through 0 (orthogonal) to 1 (identical); larger is closer.
pgvector's `<=>` operator returns cosine *distance*, which for normalized vectors
is `1 - similarity`; smaller is closer. The nearest-neighbour query therefore
sorts ascending:

```
ORDER BY embedding <=> $1 ASC LIMIT $2
```

Write `DESC` and you silently return the *worst* matches — a bug that passes code
review because the query still runs and still returns rows. Convert back to a
score with `1 - (embedding <=> $1)` when you need a comparable similarity.
Similarity is only meaningful within one embedding model and one dimension: you
cannot mix models or compare across a dimension change, and switching models means
re-embedding the entire corpus.

### Retrieval is lossy; top-k is not best-k

Pure cosine top-k has a failure mode: the k nearest vectors are often near-
duplicates of each other. If the three closest chunks all paraphrase the same
sentence, you have spent three context slots on one fact and covered none of the
rest of the question. Maximal marginal relevance (MMR) fixes this with a single
knob, lambda: it greedily selects the chunk that maximizes
`lambda * relevance - (1 - lambda) * max_similarity_to_already_selected`. At
`lambda = 1` MMR reduces to pure relevance; below 1 it penalizes a candidate that
looks too much like something already chosen, trading a little relevance for
coverage. Beyond MMR, a reranking stage — a cross-encoder, or the model itself
scoring candidates — on top of vector recall is where the large real-world gains
come from; Anthropic's contextual retrieval work reports on the order of 49-67%
fewer retrieval failures from contextual embeddings plus reranking.

### Context budgeting

The context window is a hard constraint and every token costs money and latency.
Never blindly concatenate all retrieved chunks into the prompt — that both risks
overflowing the window and pays for thousands of tokens that add nothing. Instead
pack greedily by score under a fixed token budget: take the highest-scoring chunk
that fits, then the next, and drop the rest. Leave headroom for the system prompt,
the question, and the answer's own `max_tokens`. Because the token estimate is
approximate, budget conservatively.

### Grounding and hallucination control

Retrieval succeeding does not guarantee a faithful answer. The model can ignore
the context, or cite a source that was never retrieved. Two mechanisms make the
answer auditable. First, a grounded system prompt instructs the model to answer
*only* from the provided context and to say it does not know otherwise — with an
explicit "insufficient context" path when nothing clears the relevance threshold,
so the pipeline never forces a confident hallucination. Second, numbered source
markers `[1]..[n]` in the prompt let the model cite, and a citation guardrail
parses the answer's markers and flags any index that does not map to a retrieved
chunk. A fabricated `[9]` against three sources is the difference between a demo
and something you can put in front of users.

### Failure modes to design for

The failure modes are ordinary backend failure modes. Empty or low-recall
retrieval should return "insufficient context," not a forced answer. A stale index
after documents change produces confidently wrong answers, so embeddings must be
versioned and regenerated when the corpus or the model changes. Embedding-model
dimension drift silently invalidates every stored vector. The embedding and
completion calls hit a provider with rate limits and timeouts, so batch the
embedding requests and apply context deadlines. And when you move OpenAI
embeddings (which the Go SDK returns as `[]float64`) into pgvector (which wants
`[]float32`), convert — and normalize if your operator or index assumes unit
vectors — at the boundary.

### Separation for testability and cost

The design that makes all of this maintainable is a clean seam between the pure
pipeline and the two network calls. Chunking, ranking, assembly, and citation
validation are network-free and race-tested in CI; the two external calls
(`Embeddings.New` and `Messages.New`) live behind `//go:build online` in separate
files. The default gate compiles and tests the pure pipeline — fast,
deterministic, and free — while the online path is exercised only when you opt in
with `-tags online` and a live key. This is not a testing gimmick; it is how you
keep a RAG service cheap to run in CI and cheap to reason about.

## Common Mistakes

### Treating `<=>` as similarity and ordering DESC

pgvector's `<=>` is cosine *distance* — smaller means closer. Ordering descending
returns the least relevant rows. Order ascending (`ORDER BY embedding <=> $1 ASC
LIMIT k`) and convert to a score with `1 - distance` if you need one.

### Fixed-size character chunking that cuts mid-word

Cutting every N characters severs sentences and words and destroys meaning at the
boundary. Split on semantic boundaries first (paragraph, then sentence, then word)
and add an overlap window; keep byte offsets so citations point at real spans.

### Concatenating every retrieved chunk into the prompt

Dumping all retrieved chunks blows the context window or pays for thousands of
wasted tokens. Enforce a token budget and greedily pack the highest-scoring chunks
that fit; drop the rest.

### Returning near-duplicate chunks

Pure cosine top-k crowds the context with paraphrases of one fact. Apply MMR (or a
reranker) so the packed context is diverse and actually covers the question.

### Assuming retrieval success means a faithful answer

The model can ignore the context or cite sources that were never retrieved. Emit
numbered markers and run a citation guardrail that flags any cited index outside
the retrieved set, returning a wrapped sentinel error.

### Forcing an answer when recall is empty

An empty or below-threshold retrieval, answered anyway, produces confident
hallucination. Add a threshold plus an explicit "insufficient context" fallback
that needs no model call at all.

### Mixing embedding models or dimensions without re-embedding

Distances are only comparable within one model and dimension. Version the
embedding model with the stored vectors and re-embed the whole corpus on any model
or dimension change.

### Feeding `[]float64` straight into pgvector

OpenAI's Go SDK returns `[]float64`; pgvector wants `[]float32`. Convert (and
normalize if your operator assumes unit vectors) at the boundary, or you get type
errors or skewed cosine values.

### Estimating tokens with `len(s)`

Character and word counts approximate but do not equal the model tokenizer. Budget
conservatively, with headroom for the system prompt, question, and answer
`max_tokens`.

### Making the whole pipeline require live keys

If chunk/rank/assemble/validate all need an API key, CI is slow, flaky, and
expensive. Keep them pure and offline; gate the embed and complete calls behind
`//go:build online`.

Next: [01-chunker.md](01-chunker.md)
