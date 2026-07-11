# 05 — Quality Criteria (the rubric)

**This is the most important file. Quality of content is the only judge.** The
gate proves code compiles; this rubric judges whether the *content* is correct,
deep, honest, and senior-grade — and how to push it from "passes" to "excellent".

Source of truth: `challenges/CURRICULUM-METHODOLOGY.md` §6, §7, §14.1 (A-E),
§15, §16. A lesson that fails any A-E criterion or the gate is a **rewrite
candidate, not a fix-later item**.

---

## The A-E rubric (mandatory — every lesson must satisfy all five)

**A. Internal consistency.** Prose ↔ code ↔ output ↔ intro all match. Every "the
code does X" claim is verified against the code, not memory. The intro's promise
is implemented. One coherent narrative (no "there is a bug" → "no bug" pivots).
No leftovers from prior versions (Resources on a different topic, comments
describing old code).

**B. Correctness and realism.** No stdlib reinvention — if the code duplicates
`url.Values`, `strings.Contains`, `http.Error`, etc., rewrite over the stdlib.
Every technical claim verified against authoritative docs, not memory (e.g.
`strings.ToLower` is Unicode-aware; `golang.org/x/text` is an external module,
not stdlib; `Retry-After` is a response header). A real `cmd/<name>/main.go`
demonstrable with real commands. Tests assert the actual contract
(`errors.Is(err, sentinel)`, not `msg != ""`); `httptest` for HTTP;
`t.Parallel()`, `t.Context()`, `-race` where applicable. No Go-1.22 `i := i` /
`tc := tc` redundancy.

**C. Code hygiene.** No dead imports, no `var _ = X` hacks, no identical
`if/else` branches, no dead code. `gofmt`/`go vet`/`go build` clean;
`golangci-lint` clean when available.

**D. Navigation and metadata.** File is `NN-name/NN-name.md` (the `NN-` prefix is
mandatory; its absence breaks inbound links). Every link (What's Next,
cross-refs) resolves to an existing file. Resources: 3-5 entries, on-topic, real.

**E. Extensions and homework.** Any suggested test/exercise must be possible with
the code shown. Prefer including the test in the lesson over leaving it as
homework.

### The network rule (part of B)

No network in `go test`. The default path must be hermetic. Any smoke that hits
the network goes behind `//go:build online` (or `testing.Short()`), never
required in CI. Fix a flaky test by removing the network, not documenting it.
Use `httptest`, `fstest.MapFS`, `iotest.TestReader`.

---

## Artifact quality (§6 — domain-invariant)

- Every artifact checks out against ground truth; show **real results only**
  (real program output, a derivation that re-derives, a claim that traces to a
  source). **Never fabricate output, a result, or a citation** — invented FAIL
  lines or fake line numbers (`state_test.go:51`) are a hard failure.
- Idiomatic, current style; prefer the form that clarifies.
- Wrong approaches are always labeled and placed beside the correct one.
- Remove gratuitous noise; justify a genuinely sharp edge in one line at the
  point of use.
- Pin the toolchain to current stable (`go 1.26`); safety/error handling carries
  a one-line justification at the call site.

## Length is NOT a gate — content quality is (§7)

A lesson is exactly as long as its content needs. **Never trim/cut/fold to hit a
number.** A long lesson full of valuable, correct material is a good lesson. The
only length-adjacent defect is **bloat, which is a quality defect**:

- Redundancy: do not say the same thing twice (prose and again in the artifact,
  or across two paragraphs). Remove the *duplication*, not the content.
- Filler: cut anything that serves no learning objective.
- Two-things-in-one is a *structure* defect → split into two sequenced lessons,
  not because of size.

Completeness and correctness always outrank brevity. The verifier flags
redundancy/filler/incoherence — never length.

## Self-consistency (§15 — the second pass)

The gate answers "does it work?"; §15 answers "is the prose honest about what
works?". For every lesson:
- Every identifier the prose names (method/field/function/package) must exist in
  the code.
- Every "Expected output" block must match a real captured run, byte-for-byte.
- Every "this is a bug" claim must map to a test that FAILS on the buggy code and
  PASSES on the fix. If the test passes on the buggy code, the prose is wrong.
- Every cross-reference resolves; every Resource URL resolves (200/3xx, not 404).
- Every technical claim ("`x/text` is stdlib", "Retry-After is a request header")
  verified against the authoritative source.

A divergence is a failure even if the gate passes.

---

## How to MAINTAIN and INCREASE content quality

The bar is not "compiles + passes A-E". That is the floor. To keep raising
quality, judge each lesson on these depth dimensions and push it up a level.

### Quality levels (target: Excellent)

- **Floor (acceptable):** gate PASS, A-E satisfied, §15 clean. Real test +
  Example + demo. Correct but possibly shallow.
- **Good:** Concepts explain the real "why" (semantics, not just syntax); the
  artifact is realistic, not a toy; Common Mistakes are real traps tied to THIS
  code; trade-offs named.
- **Excellent (the target):** teaches a senior backend something they would
  actually apply — failure modes under load/concurrency, the cost model, when
  NOT to use the technique, how the stdlib/real libraries actually implement it,
  and an artifact substantial enough to resemble production code. The reader
  finishes able to make a design decision, not just reproduce a snippet.

### Depth checklist (push every lesson toward "yes")

- [ ] **Real "why", not just "how".** Concepts cover semantics, the hard part,
      trade-offs, and failure modes — not a paraphrase of the syntax.
- [ ] **Senior-backend relevance.** The artifact maps to something real
      (a server, a pool, a codec, a scheduler), not an artificial toy. Would a
      senior engineer recognize this problem from production?
- [ ] **Correctness under stress.** Where it matters: concurrency lessons pass
      `-race` with a real contended test; error paths are exercised; edge cases
      (empty/oversized/Unicode/cancellation) are covered, not only the happy
      ASCII path.
- [ ] **Cost / performance awareness.** Allocation, copies, lock scope, big-O,
      or syscall cost named where it shapes the design.
- [ ] **When NOT to use it.** The lesson states the limits and the alternative,
      so the reader can make a trade-off, not just follow a recipe.
- [ ] **Grounded in real implementations.** Reflects how the stdlib or a named
      library (`net/http`, `grpc`, `bbolt`, `slog`) actually does it — verified,
      not imagined.
- [ ] **Honest verification.** Tests assert real contracts (`errors.Is`/`%w`),
      outputs are captured from real runs, claims trace to sources.
- [ ] **One idea, fully taught.** If it teaches two, split it.

### Raising a passing lesson (concrete moves)

1. Deepen `## Concepts` into the failure modes and trade-offs a senior cares
   about; add the cost model.
2. Make the artifact less toy-like — closer to a real component, with the edge
   cases handled.
3. Strengthen tests: add the contended/`-race`, the boundary, the
   cancellation/timeout, the error-path assertions.
4. Replace any "from memory" claim with a verified one and cite the page.
5. Add a "when not to" / alternative paragraph.
6. Cut redundancy and filler (bloat is a quality defect) — without cutting
   content.

---

## Judging procedure (per lesson)

1. Run the gate (`02-gate-tool.md`). PASS or clean OFFLINE-BAR is the floor.
2. Score A-E with **evidence** — cite the exact line/quote/output for each
   verdict. Never a single pass/fail; criterion-by-criterion.
3. Run the §15 self-consistency read.
4. Apply the depth checklist; decide Floor / Good / Excellent.
5. Record concrete failures (not "looks fine"); fix to the level of the gold
   01-12 base lessons; re-gate to green.

This is exactly what the engine's adversarial verifier does
(`03-engine-and-methodology.md`) — but the criteria here apply equally to a
human pass, a new lesson, or an audit of existing content.
