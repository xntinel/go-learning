export const meta = {
  name: 'go-fix-gofmt',
  description: 'Targeted parallel fixes: gofmt-clean the flagged capstone lessons + generate the 1 missing + reconcile seccomp package names',
  phases: [{ title: 'Fix' }],
}

const STANDARD = 'challenges/go/AGENT-ORDER-go-quality.md'

function safe(p) { return p.replace(/[^A-Za-z0-9]/g, '_') }
function gateCmd(path) {
  return `export GOTOOLCHAIN=auto && python3 challenges/tools/go_gate_append.py ${path} challenges/go/.verify/fix/${safe(path)}`
}

// 15 gofmt-fail lessons
const GOFMT = [
  'challenges/go/43-capstone-stream-processing-engine/07-sink-connectors/07-sink-connectors.md',
  'challenges/go/44-capstone-http2-implementation/06-full-http2-server/06-full-http2-server.md',
  'challenges/go/45-capstone-distributed-key-value-store/05-read-repair/05-read-repair.md',
  'challenges/go/45-capstone-distributed-key-value-store/07-client-protocol/07-client-protocol.md',
  'challenges/go/46-capstone-concurrency-deep-dive/06-software-transactional-memory/06-software-transactional-memory.md',
  'challenges/go/47-capstone-systems-and-kernel/02-ebpf-tracing/02-ebpf-tracing.md',
  'challenges/go/47-capstone-systems-and-kernel/03-netlink-socket/03-netlink-socket.md',
  'challenges/go/47-capstone-systems-and-kernel/05-io-uring-integration/05-io-uring-integration.md',
  'challenges/go/47-capstone-systems-and-kernel/07-ptrace-syscall-tracer/07-ptrace-syscall-tracer.md',
  'challenges/go/47-capstone-systems-and-kernel/08-raw-socket-packet-capture/08-raw-socket-packet-capture.md',
  'challenges/go/47-capstone-systems-and-kernel/09-custom-network-protocol-stack/09-custom-network-protocol-stack.md',
  'challenges/go/47-capstone-systems-and-kernel/10-go-language-server-lsp/10-go-language-server-lsp.md',
  'challenges/go/47-capstone-systems-and-kernel/12-go-to-wasm-compiler/12-go-to-wasm-compiler.md',
  'challenges/go/47-capstone-systems-and-kernel/13-interactive-debugger-ptrace/13-interactive-debugger-ptrace.md',
]
// seccomp has BOTH gofmt diffs AND a two-package-names-in-one-dir bug
const SECCOMP = 'challenges/go/47-capstone-systems-and-kernel/06-seccomp-filter/06-seccomp-filter.md'
const MISSING = 'challenges/go/46-capstone-concurrency-deep-dive/07-concurrent-btree/07-concurrent-btree.md'

function gofmtPrompt(md, extra) {
  return `Work from repo root. ONE Go lesson markdown has gofmt-dirty code blocks. Fix ONLY the formatting: make every Go code block gofmt-clean (tabs for indentation, correct struct-field/comment alignment, no trailing whitespace). Do NOT change logic, identifiers, or prose. Keep no emojis, English.

Lesson: ${md}
${extra || ''}
Method: run the gate, read which files gofmt flags, open the matching code block in the .md, and reformat exactly as gofmt would (you can extract a block to a temp .go file and run \`gofmt\` on it to see the canonical form, then mirror it back into the .md block). Re-run the gate until gofmt is clean:
${gateCmd(md)}
For a mode "bar" lesson the build/test may FAIL on external/Linux deps — that is fine; ONLY gofmt must be clean (no "gofmt:" token in the output). Return the final gate tail.`
}

function generatePrompt(md) {
  return `Work from repo root. Read ${STANDARD} in full, then rewrite this ONE capstone lesson to the canonical standard (§5 shape: real *_test.go + Example//Output + cmd/demo, TAB-indented Go, no emojis, English): ${md}
This is a CAPSTONE (mode "bar"): substantial realistic code; it may not build offline (Linux/cgo/external module). Apply the capstone bar — gofmt-clean + go vet on extractable code; build/test deferred. Research authoritative sources before writing; never invent APIs. Remove the banned \`bloom_level:\` marker. Self-check with:
${gateCmd(md)}
Ensure gofmt is clean. Return the final gate tail.`
}

phase('Fix')
const tasks = []
for (const md of GOFMT) {
  const label = md.split('/').slice(-1)[0]
  tasks.push(() => agent(gofmtPrompt(md), { phase: 'Fix', label: `gofmt:${label}`, model: 'sonnet', agentType: 'general-purpose' }))
}
tasks.push(() => agent(gofmtPrompt(SECCOMP, 'ALSO: this lesson currently declares TWO different package names in the same directory (e.g. `package seccomp` in bpf.go and `package compiler` in compiler.go). That is a real defect — reconcile every file in the same package directory to ONE consistent package name so the package compiles as a unit.'), { phase: 'Fix', label: 'fix:06-seccomp-filter', model: 'sonnet', agentType: 'general-purpose' }))
tasks.push(() => agent(generatePrompt(MISSING), { phase: 'Fix', label: 'gen:07-concurrent-btree', model: 'sonnet', agentType: 'general-purpose' }))

const results = await parallel(tasks)
log(`Fix wave done: ${results.filter(Boolean).length}/${tasks.length} agents returned`)
return { count: tasks.length, returned: results.filter(Boolean).length }
