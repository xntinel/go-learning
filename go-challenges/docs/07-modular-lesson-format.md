# 07 — Modular Lesson Format (runbook)

How to convert a capstone chapter from one big `.md` per lesson into the
modular, self-contained-per-exercise format. Followed for chapters 39 and 40;
repeat verbatim for 41-47 or any other chapter. Written so another agent can
execute it without further context.

## 1. End state (the convention)

For each lesson directory `NN-lesson-slug/`:

```
NN-lesson-slug/
  00-concepts.md        concepts as PROSE only; no buildable code; no index
  01-<slug>.md          one self-contained, independently-gating Go module
  02-<slug>.md          per exercise
  ...
```

Rules that define "done":

- `00-concepts.md`: the richest conceptual content (all theory needed to do the
  exercises) as prose + a `## Common Mistakes` section. NO `Create`/`Append` Go
  markers (tiny inline illustrative snippets only). NO index / no list of links
  to exercises. Ends with one line: `Next: [01-<slug>.md](01-<slug>.md)`.
- Each exercise file is its OWN fully self-contained, independent Go module:
  its own `go mod init example.com/<slug>`, all the code it needs duplicated
  inline (no exercise imports another), its own `cmd/demo/main.go`, its own
  `*_test.go`. It gates ALONE.
- Each exercise file, in this exact order:
  1. `# Exercise N: <Title>`
  2. a short teaching intro (1-3 sentences)
  3. `## What you'll build` — FIRST a ```text tree of files + key symbols, then
     four lines: `Files:`, `Implement:`, `Test:`, `Verify:`. NO "Prerequisites".
  4. rich EXPLANATION prose (the why/how, design reasoning, trade-offs, failure
     modes, a walkthrough of the key code). Explanation quality is the priority;
     explain an opaque mechanism at/before its code, never only after.
  5. the code (with `Create` markers, TABS not spaces)
  6. `cmd/demo/main.go` with an Expected-output block matching a real run
  7. the `*_test.go`
  8. `## Review` — plain prose: common mistakes + how to confirm correctness.
     NO checklist.
  9. `## Resources` — 2-4 real, authoritative internet links for this subtopic
     (verify each resolves with WebSearch/WebFetch before citing).
  10. a prev/next nav footer linking `00-concepts.md` and adjacent exercises.
- Banned everywhere: README files, any index/lesson-map, `## Your turn`,
  verification checklists, the word "Prerequisites", emojis/decorative symbols
  (math arrows and relational-algebra operators are allowed). English only.

## 2. Decomposition rule (how many exercises)

Read the current single file. Its sections are usually a mix of:

- incremental build-steps of ONE package (types -> core -> tests -> demo) —
  these are NOT independent; consolidate them into ONE self-contained core
  module.
- genuinely independent features (a distinct concept subsection / Common
  Mistake of its own) — each becomes its own self-contained module.

Drop nothing. "Tests" and "Demo" pseudo-exercises fold into the module they
test/demonstrate (every module ships its own test + demo). Convert any
"Your turn" prompt into a real, passing test.

## 3. Execution: one agent per lesson

Spawn one general-purpose agent per lesson, in parallel. Put EVERY requirement
in the agent's INITIAL prompt — see §6 (subagents discount mid-run messages).
Give each agent: the lesson path, the topic, the §1 convention, the §2 rule, the
gate recipe (§4), and a pointer to an already-converted lesson as the format
template (e.g. `01-write-ahead-log/00-concepts.md` + one exercise file). Tell
each agent: do NOT edit `go.md`, do NOT create a README/index, delete the old
single file, and self-gate every exercise to PASS before returning.

## 4. Gate model

Each exercise is its own module; the gate tool already maps one markdown file ->
one module. Gate each exercise file alone:

```bash
cd challenges/go/<chapter-dir>
WD=$(mktemp -d)
for f in <lesson-dir>/[0-9][0-9]-*.md; do
  [ "$(basename "$f")" = "00-concepts.md" ] && continue
  r=$(env GOTOOLCHAIN=auto python3 ../../tools/go_gate_append.py "$f" "$WD/$(basename "$f" .md)" 2>&1 | tail -1)
  echo "$r <- $f"
done
```

- `GOTOOLCHAIN=auto` is required (lessons declare `go 1.26`; it downloads the
  toolchain).
- External-module lessons (e.g. one importing `golang.org/x/term`): add
  `GOFLAGS=-mod=mod` to the env so `go` fetches the dependency.
- Cross-module dependencies must be removed: if a lesson used
  `replace ../otherlesson`, each exercise instead BUNDLES its own minimal copy
  of what it needs, so every module is standalone (no `replace`).

## 5. Final consolidation (the coordinator does this, not the agents)

After all per-lesson agents finish:

1. Update the index `go.md` rows to point at each lesson's `00-concepts.md`:

   ```bash
   perl -i -pe 's{(<chapter-dir>/(\d{2}-[a-z0-9-]+))/\2\.md}{$1/00-concepts.md}g' challenges/go/go.md
   ```
   (The pattern only matches the old `dir/dir.md` form, so `00-concepts.md` rows
   and other chapters are left alone.)

2. Fix cross-lesson "Next" links that still point at the deleted single files:

   ```bash
   cd challenges/go/<chapter-dir>
   for f in */[0-9][0-9]-*.md */00-concepts.md; do
     [ -f "$f" ] || continue
     perl -i -pe 's{\.\./(\d{2}-[a-z0-9-]+)/\1\.md}{../$1/00-concepts.md}g' "$f"
   done
   ```
   (A link to the NEXT chapter, e.g. `../../41-.../01-....md`, has dir != file
   slug, so it is correctly left untouched.)

3. Verify, from the chapter dir:
   ```bash
   # every lesson has 00-concepts.md and no old single file
   for d in [0-9][0-9]-*; do
     [ -f "$d/00-concepts.md" ] || echo "NO-CONCEPTS $d"
     [ -f "$d/$d.md" ] && echo "OLD-FILE-LEFT $d"
   done
   # no README/index
   find . -iname 'readme*' -o -iname 'index*'
   # no banned emojis (arrows are fine)
   grep -rloP '[\x{2600}-\x{27BF}\x{1F000}-\x{1FAFF}\x{2705}\x{2714}\x{2B50}\x{25B6}\x{26A0}]' */*.md
   # no leaked tool-call XML fragments (agents sometimes trail </content> or
   # </invoke> at EOF; the GATE DOES NOT CATCH THIS since it only extracts Go
   # blocks). Must print nothing; clean with:
   #   grep -rlE '^</?(content|invoke|parameter|function_calls)>$' */*.md | while IFS= read -r f; do
   #     perl -i -ne 'print unless m{^</?(content|invoke|parameter|function_calls)>\s*$}' "$f"; done
   grep -rlE '^</?(content|invoke|parameter|function_calls)>$' */*.md
   # no broken local links
   for f in */*.md; do grep -oE '\]\(([0-9A-Za-z._/-]+\.md)\)' "$f" | sed -E 's/\]\(//;s/\)//' | while read l; do
     case "$l" in /*) t="$l";; *) t="$(dirname "$f")/$l";; esac
     [ -f "$t" ] || echo "BROKEN $f -> $l"
   done; done
   ```

4. Spot re-gate a few modules across the chapter (including any external-module
   one with `GOFLAGS=-mod=mod`) to confirm the on-disk state still PASSes.

5. Save a memory note (engram + MEMORY.md) recording the chapter is converted,
   the module counts, and any special cases.

## 6. Process gotchas

- Subagents treat a mid-run `SendMessage` (delivered as a "coordinator" message
  with no user authority) as NON-authoritative and may revert it. Put ALL
  requirements in the agent's INITIAL prompt. If you must change the spec
  mid-stream, spawn a FRESH agent with the new spec as its task rather than
  messaging a running one.
- Have agents NOT touch `go.md` — concurrent edits to one file race. The
  coordinator updates `go.md` once at the end.
- The new-diagnostics noise about files "within module .../scratchpad/..." is
  from the agents' temporary gate workdirs, not the lessons. Ignore it.
- `zsh` aborts a `for f in glob1 glob2` loop when `glob2` has no match; the gate
  loop runs correctly because the single-digit glob covers all files, but if you
  hand-run a two-glob loop, guard with `[ -f "$f" ] || continue` (as above).

## 7. Reference

The first converted lesson is the gold template:
`challenges/go/39-capstone-database-engine/01-write-ahead-log/` —
`00-concepts.md` (prose) + `01..07-*.md` (one module each). Match its shape.
