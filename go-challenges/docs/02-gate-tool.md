# 02 — The Gate (ground truth)

`challenges/tools/go_gate_append.py` is the model-independent arbiter of whether
a lesson is real. It assembles the lesson's Go code blocks into a temporary
module and runs the §14.1 gate: `gofmt -l`, `go vet`, `go build`,
`go test -race`. Treat its verdict as truth — never a generator's self-report.

## Usage

```bash
export GOTOOLCHAIN=auto
python3 challenges/tools/go_gate_append.py <lesson.md> <workdir-inside-repo>
```

- Run from the repo root.
- `<workdir>` MUST be inside the repo (the sandbox rejects `/tmp`). Convention:
  `challenges/go/.verify/<chapter>/<lesson>/`. Delete it after.
- Go is 1.23.3 system-wide, but `GOTOOLCHAIN=auto` auto-downloads go1.26 from a
  `go 1.26` go.mod (first run pulls the toolchain; later runs are cached).

Output is one line:
- `PASS` — gofmt clean AND vet/build/test all exit 0.
- `FAIL  <detail>` — e.g. `FAIL  gofmt:server.go | build:foo.go:9:2: ...`.
  Detail order: gofmt diffs, then the first failing of build/vet/test.
- `NOGO  (no Create/Append markers or // headers)` — no assemblable code found.

## How it assembles files (the markers it honors)

The gate scans the markdown for file markers and ```go``` blocks, in document
order, and writes real files. Recognized ways to name a file:

1. **A marker line before a ```go``` block:**
   - `Create \`path.go\`:` or `Save as \`path.go\`:` -> start a new file at path.
   - `Append to \`path.go\`:` or `Add to \`path.go\`:` -> concatenate onto it.
     A bare basename in an Append/Add marker resolves to the unique
     previously-created file with that basename.
2. **A comment header as the FIRST line of the block:**
   - `// path/file.go` -> that block IS that file (header line is stripped).
   - `// go.mod` -> the block supplies the module path (not written as .go).
3. **The module path** comes from a `module <path>` line (a go.mod block) or a
   `go mod init <path>` command line; else defaults to `example.com/lesson`.
   The real module path is used verbatim — a wrong path in the lesson (e.g. a
   stray `.md` suffix) correctly FAILS the gate instead of being masked.

**Blocks with neither a marker nor a header are illustrative and skipped.** Use
plain ```` ``` ```` fences (or `text`/`bash`) for Wrong-examples and for any
deliberately-buggy intermediate code you do NOT want assembled.

### Gotchas (learned the hard way)

- **"Replace `file`:" is NOT a recognized marker** — it is ignored. If you show
  a buggy version then a corrected one, do one of:
  - make the buggy block illustrative ("Write …" / plain fence, no `Create`
    path) and `Create` the fixed version; OR
  - use `Create \`same/path.go\`` for BOTH — a second `Create` of the same path
    OVERWRITES (last wins), so the fixed block becomes the file.
- A `// path.go` header line means the block becomes that file — so don't start
  an illustrative snippet with a `// something.go` comment unless you mean it.
- Every Go block that IS assembled must be a complete compilable unit for its
  file: a block that starts with `func …` and no `package` clause, captured
  under a `Create`, produces `expected 'package', found 'func'`.
- The gate runs `go test -race` — concurrency lessons must be race-free, not
  just compile.
- For mode "bar" lessons (cgo/external/Linux-only), the gate will `FAIL` on
  build with `no required module provides package …` or undefined Linux
  syscalls on darwin. That is EXPECTED; judge gofmt cleanliness on the
  extractable code instead (see below).

## Fast mechanical sweep (for bar-mode and bulk checks)

For many bar-mode lessons, skip LLM verifiers and run the gate yourself in
parallel, classifying gofmt-fails (real defects) vs build-only-fails (expected
offline):

```bash
sweep() {
  md="$1"; slug=$(echo "$md" | sed 's|[^A-Za-z0-9]|_|g')
  out=$(GOTOOLCHAIN=auto python3 challenges/tools/go_gate_append.py "$md" \
        "challenges/go/.verify/sweep/$slug" 2>&1 | tail -3)
  if   echo "$out" | grep -q "^PASS";    then echo "PASS|$md"
  elif echo "$out" | grep -qi "gofmt:";  then echo "GOFMT|$md|$out"   # real defect
  else echo "BUILDONLY|$md"; fi                                       # expected offline-bar
}
export -f sweep
find challenges/go/<chapters> -name '*.md' -path '*/0*' | xargs -P8 -I{} bash -c 'sweep "$@"' _ {}
```

A `GOFMT` result is a real, fixable defect; `BUILDONLY` is a clean OFFLINE-BAR.

## Split lessons (multi-file): go_gate_lesson.py

Newer lessons are split across a directory instead of one big `.md`:

```
NN-name/
  NN-name.md      home index (H1, intro, links, Summary, What's Next, Resources)
  concepts.md     ## Concepts + ## Common Mistakes (no assembled code)
  ex-01-...md      one file per exercise: "## What this teaches" + "## The code"
  ex-02-...md      (Create/Append code blocks live here)
  ...
```

There are two exercise styles, and `go_gate_lesson.py` auto-detects which:

- **Shared-module** — the exercises build ONE module incrementally (`ex-01`
  `Create`s files, `ex-02` `Append`s, ...). The lesson is gated as one unit by
  concatenating all `.md` in sorted order.
- **Independent-exercise** — each `ex-NN` file is its own self-contained module
  (its own `go mod init` + code + `*_test.go` + `cmd/demo`), so a learner can
  build/run/test any exercise on its own without the others. Detected when more
  than one `ex-NN` file contains a `go mod init`; each exercise is then gated
  SEPARATELY and the lesson passes only if all do. (Lesson 02 uses this style.)

Gate the whole lesson with `challenges/tools/go_gate_lesson.py` either way:

```bash
GOTOOLCHAIN=auto python3 challenges/tools/go_gate_lesson.py challenges/go/NN-chapter/NN-name
```

Output is the same single line (`PASS` / `FAIL <detail>` / `NOGO`). The sorted
concatenation is exactly the old single-file document, so the assembly and verdict
are identical to gating the un-split lesson. The splitter that produces this layout
from a single file is `challenges/tools/split_go_lesson.py`; the
non-negotiable is the sorted-order rule so that `ex-NN` files assemble in sequence.

## Related fallback gates

- `challenges/tools/go_gate_subdir.py`, `go_gate_marker.py` — legacy assemblers,
  kept only for the rare lesson the primary marks NOGO. Prefer `go_gate_append.py`.
