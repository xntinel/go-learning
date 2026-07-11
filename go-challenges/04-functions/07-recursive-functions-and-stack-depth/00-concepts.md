# Recursion, Stack Depth, and When to Go Iterative — Concepts

Recursion is not a syntax feature you reach for because it reads nicely. On a
backend it is a correctness-and-safety decision. You use it when you are walking
data you own and whose depth you trust — a config tree, a directory layout, an
internal dependency graph — because the code then mirrors the recurrence and is
easier to get right. You deliberately refuse it, or bound it hard, the moment the
shape of the input is controlled by someone else: a request body, a third-party
API payload, an uploaded manifest. Go makes this a real production concern
because of two facts that surprise engineers coming from other languages: Go
goroutine stacks grow on demand up to a hard cap, and Go performs no tail-call
optimization. Unbounded recursion over untrusted input is therefore a genuine
denial-of-service vector that crashes the whole process, not a recoverable bug.

This file is the conceptual foundation. Read it once and you have the model
behind all nine exercises: recursive walkers over trusted trees, the mechanical
conversion to an explicit depth-bounded stack, a streaming depth guard for
untrusted JSON, a recursive-descent parser, three-color cycle detection,
topological ordering, memoized closures, a generic fold, and the concrete
measurement of why a linear reduction belongs in a loop, not a recursion.

## The shape of a recursive function

A recursive function calls itself on a strictly smaller subproblem and has a base
case that returns without recursing. "Strictly smaller" is the load-bearing
phrase: each call must make measurable progress toward the base case, or the
recursion never terminates. For tree and graph work the base case is a leaf (a
file, a node with no children) or an already-visited node. The classic tree walk:

```go
func walk(fsys fs.FS, dir string) error {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			if err := walk(fsys, path.Join(dir, e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}
```

The function recurses only on directories; a file is the base case that returns
without recursing. The depth of the call stack equals the depth of the tree.
A missing or unreachable base case is the single root cause of stack overflow:
the frames pile up until the runtime kills the process.

## Go stacks: small, growable, but hard-capped

A goroutine starts with a small stack — a few KiB — and the runtime grows it on
demand by allocating a larger segment and copying the frames over. There is no
fixed stack size the way there is in C with a default 8 MiB thread stack, so a Go
recursion can go far deeper than intuition from fixed-stack languages suggests.
But "growable" is not "unbounded". There is a hard cap, set by
`runtime/debug.SetMaxStack`: the default is 1 GiB on 64-bit platforms and 250 MiB
on 32-bit. `SetMaxStack` returns the previous limit and is meant to be called
once at startup; lowering it is a way to make runaway recursion fail faster
during testing. Exceeding the cap is fatal.

## Stack overflow is fatal and recover cannot catch it

When a goroutine's stack exceeds the limit, the runtime prints
`runtime: goroutine stack exceeds ... limit` and aborts the entire program. This
is not a `panic` that a deferred `recover` can intercept — it is a runtime abort.
That single fact is why unbounded recursion over untrusted input is a security
problem and not merely a bug: an attacker who can send a deeply nested payload
can crash your process, and no amount of `defer func(){ recover() }()` will save
you. The only defense is to bound depth before the stack is exhausted. Build the
guard; do not plan to catch the failure.

## Go has no tail-call optimization

Some languages rewrite a "tail recursive" function — one whose recursive call is
the last thing it does, typically passing an accumulator — into a loop that
reuses a single frame. Go does not. An accumulator-passing `sumAcc(rest, acc+x)`
still allocates one stack frame per element, so it consumes O(depth) stack exactly
like naive recursion and offers no safety advantage. For a linear reduction over
a slice, the recursive version can overflow the stack on a large input while the
plain `for` loop uses O(1) stack. The lesson here is blunt: for linear reductions
the loop is strictly better; reserve recursion for genuinely branching structure
where the tree depth, not the total element count, drives stack usage.

## Depth and breadth are separate costs

Recursion depth drives stack usage; total node count drives time and heap. A
shallow-but-enormous tree (a million files two directories deep) is a heap and
time problem, not a stack problem, and iterating it recursively is fine for the
stack. A narrow-but-very-deep tree (a single chain a million nodes long) is a
stack problem regardless of how few total nodes it has. Each is defended
differently: bound total work for the wide case, bound depth for the deep case.
Conflating them leads to guards that protect against the wrong failure.

## Recursion is for depth you trust; bound depth you do not

The decision rule that a senior engineer applies: recurse freely over data whose
depth you control and can reason about — your own directory tree, a config file
you emit, an internal DAG built from your own service registry. The recurrence is
clearer than the manual stack, and the trusted depth means the hard cap will
never be approached. For depth you do not control, either bound it explicitly or
convert to iteration with a hard limit. "It has always been shallow in practice"
is not a bound; an attacker does not send you the inputs you tested with.

## Converting recursion to an explicit stack

Any recursion can be converted to iteration mechanically. Depth-first traversal
becomes a loop over an explicit LIFO stack of pending work items; breadth-first
becomes a FIFO queue. The transformation is the same every time: the arguments of
the recursive call become the fields of a frame struct pushed onto the stack, and
the function body becomes the loop body that pops a frame and pushes new ones. The
payoff is not speed — it is control. Because you own the stack, you can carry a
per-frame depth counter and refuse to push a frame past a limit, failing fast with
a sentinel error before memory is exhausted. The common bug in this conversion is
carrying the path but forgetting the depth, so the bound you added is never
actually enforced.

## Bounding untrusted structured input before materializing it

For untrusted JSON, XML, or nested filter expressions, the wrong move is to parse
the whole thing and then measure its depth — the out-of-memory or stack overflow
happens during the parse, before your check ever runs. The right move is to stream
tokens and count nesting as you go. `encoding/json`'s `Decoder.Token` returns one
token at a time; a `json.Delim` of `{` or `[` increments depth and `}` or `]`
decrements it. You reject a payload the instant depth crosses the limit, having
materialized only a constant amount of it. This is the standard middleware defense
against nested-JSON denial of service, and it generalizes to any tokenizable
format.

## Graph recursion needs visited state

A tree has no cycles, so plain recursion terminates. A graph can, so recursion
over a graph needs state to avoid looping forever. A single "visited" set is
enough to terminate — you never re-enter a node — but it cannot tell you *why* you
reached a node again. Cycle detection needs three colors: white (unvisited), gray
(on the current DFS stack, in progress), and black (fully explored). Encountering
a gray node means you followed a back edge to a node still on your own path — that
is a cycle. Encountering a black node just means a diamond: a node reachable by
two forward paths, which is not a cycle. The gray state is the difference between
"terminate safely" and "detect the cycle and report the path".

## Memoization turns exponential re-walks into linear work

When subgraphs are shared — a diamond where two paths reach the same node — naive
recursion recomputes that node's result once per path, and in the worst case the
number of recomputations is exponential in the graph's depth. Caching each node's
computed result in a memo map collapses this to linear work: each node is computed
once and its result reused. Memoization also makes recursion over a cyclic graph
terminate, because a revisit short-circuits to the cached (or in-progress) result
instead of recursing forever. The transitive-closure and dependency-analysis
exercises both depend on this.

## Recursive-descent parsing mirrors grammar precedence

A recursive-descent parser is a set of mutually recursive functions, one per
precedence level, that mirror the grammar: `parseExpr` handles the lowest-binding
operator (say `OR`), calls `parseTerm` for the next level (`AND`), which calls
`parseFactor` for the highest (comparisons and parenthesized subexpressions). The
structure reads almost exactly like the grammar it implements, which is why it is
the default hand-written parsing technique. But `parseFactor` recurses back into
`parseExpr` on an open parenthesis, so adversarially deep parentheses —
`((((((...))))))` — drive the recursion as deep as the input is nested. A parser
over untrusted expressions must carry a depth counter and reject input past a cap,
for exactly the same reason the JSON guard does.

## Common Mistakes

### Recursing without a reachable base case

Wrong: a function that calls itself on every input but has no base case, or one
whose base case the arguments never actually reach. The stack grows until the
process is killed. Fix: every recursive function needs a base case that returns
without recursing, and every recursive call must move strictly toward it. For tree
walks the base case is the leaf; for graph walks it is the already-visited node.

### Expecting recover to catch a stack overflow

Wrong: wrapping a deep recursion in `defer func(){ recover() }()` and assuming the
program survives a runaway. Stack exhaustion is a fatal runtime abort, not a
`panic`; `recover` never runs. Fix: bound depth up front. The guard is the only
defense, so build it before the recursion, not a rescue after it.

### Writing accumulator recursion expecting Go to optimize it

Wrong: writing `sumAcc(rest, acc+x)` "tail recursively" and assuming Go turns it
into a loop. Go has no tail-call optimization; the frames still pile up O(n). Fix:
for a linear reduction, write the `for` loop. It uses O(1) stack and cannot
overflow.

### Running recursion directly over attacker-controlled data

Wrong: walking a JSON document from a public API, or a user-uploaded manifest,
with an unbounded recursion. A nested payload exhausts the stack and crashes the
process. Fix: stream and bound the depth before materializing the structure, and
reject over-deep input with a sentinel error.

### Descending into a non-directory

Wrong: calling `fs.ReadDir` on a path that is a file, producing
`readdir: not a directory` and breaking single-file roots. Fix: check `IsDir`
before recursing, and handle the single-file root as an early return.

### Traversing a graph with no visited state, or the wrong state

Wrong: recursing over a graph with no visited set (infinite recursion on any
cycle), or using a plain visited set when you actually need to distinguish back
edges. Fix: use a visited set to terminate, and the gray-on-stack color when you
must detect the cycle, not merely avoid looping.

### Recomputing shared subtrees instead of memoizing

Wrong: recomputing a shared node's closure once per path that reaches it,
turning a diamond graph into exponential work. Fix: memoize each node's result in
a cache keyed by the node, so it is computed once and reused — which also makes
cyclic input terminate.

### Relying on map iteration order in graph algorithms

Wrong: iterating a `map[string][]string` graph in native (random) order, so the
detected cycle or the topological order changes run to run and tests flake. Fix:
iterate sorted keys and sorted neighbor lists, making the output deterministic and
assertable.

### Converting to an explicit stack but dropping the depth

Wrong: mechanically turning recursion into an explicit stack of paths but not
carrying the per-frame depth, so the whole point — the enforceable bound — is
never actually checked. Fix: the frame struct must carry `depth` alongside the
work item, and the loop must refuse to push past the limit.

### Materializing the whole structure just to measure it

Wrong: `json.Unmarshal` into `any` and then walking the result to compute depth —
the out-of-memory happens during the unmarshal, before your measurement runs. Fix:
stream tokens with `Decoder.Token` and count delimiters, so you reject the payload
having read only a constant slice of it.

Next: [01-recursive-fs-tree-walker.md](01-recursive-fs-tree-walker.md)
