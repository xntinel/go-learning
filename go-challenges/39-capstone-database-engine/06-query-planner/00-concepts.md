# 6. Query Planner and Executor — Concepts

A query engine is the half of a database that turns a declarative request ("give me these rows") into an executable plan and then runs it. This lesson builds that engine from the ground up in the volcano (iterator) model: a typed nullable value, an operator interface, the physical operators (sequential and index scan, filter, projection, sort, limit, hash join, nested-loop join, group-by aggregation, top-N, and sort-merge join), a cost-based planner that lowers a logical plan to physical operators while pushing predicates down, and a rule-based optimizer that rewrites the logical plan. The conceptual hard parts are three: SQL's three-valued NULL logic, which infects every comparison and every join; the blocking (pipeline-breaking) operators that must materialize their whole input before emitting a row; and proving that a plan rewrite is semantics-preserving by reasoning about the operator tree rather than the data. Read this file once and you will have the vocabulary for every exercise, each of which builds one piece as an independent, self-contained Go module.

## Concepts

### The Volcano Model: Open, Next, Close

The volcano model, named after Graefe's 1994 evaluation system, is the iterator pattern applied to relational operators. Every operator — scan, filter, join, aggregate — exposes the same tiny interface of four methods. `Init` (the classic "Open") acquires resources: it rewinds a child, allocates a build-side hash table, or drains and sorts an input. `Next` returns exactly one output tuple, or a nil tuple to signal end-of-stream; it does the minimum work needed to produce that one row. `Close` releases resources regardless of how many rows were actually consumed, so a `LIMIT 1` over a billion-row scan frees its file handle promptly. `Schema` reports the ordered list of output columns, which lets the planner resolve column references at plan time instead of re-deriving them while rows flow.

Execution is pull-based: the consumer calls `Next` on the root operator until it returns nil, and each operator pulls from its children in turn. The whole plan is a tree of cooperating iterators driven entirely from the top. Because each `Next` produces one row with no buffering, most operators run in O(1) state and allocate nothing after `Init`. The clarity of one-tuple-at-a-time is exactly why the model is the standard teaching vehicle, even though production engines layer optimizations on top of it.

### Typed Values and SQL NULL

SQL is built on three-valued logic because every column can be NULL, and NULL is not a value but the absence of one. A runtime value therefore carries its own type tag and its own nullability: an integer, a float, a string, a boolean, or the distinguished NULL. Any arithmetic or comparison that touches a NULL yields NULL, never a concrete result. In a boolean context NULL is "unknown," which a filter treats as "do not pass," but unknown is a third truth value distinct from false, and the distinction matters the moment NULL meets AND and OR (see the three-valued-logic section below). The first exercise builds this value type and the comparison routine that orders NULL before every concrete value, because a deterministic total order is what sort and merge-join depend on even though SQL itself leaves NULL ordering implementation-defined.

### Schemas and Tuples

A schema is an ordered list of column definitions, each a name, an optional table qualifier, and a kind. A tuple is a slice of values in schema order — a row. Operators advertise their output schema so that a projection can extract a column by integer index rather than by string lookup at runtime, and so the planner can resolve a column reference like `users.id` to a position before any data moves. Resolving by name at plan time and by index at run time is the small design choice that makes projection and predicate pushdown cheap: the rewrite manipulates names, the executor manipulates indices.

A subtle correctness rule lives here: a scan must return a copy of each stored row, never a pointer into its backing array. Operators that hold tuples across `Next` calls — most importantly the build side of a hash join — would otherwise alias into the scan's storage and see their retained rows overwritten by the next read. Cloning at the scan boundary makes every emitted tuple independent.

### Logical vs Physical Plans

A logical plan says *what* to compute in relational-algebra terms: scan a table, select (σ) rows satisfying a predicate, project (π) a set of columns, join (⋈) two inputs, group and aggregate. A physical plan says *how*: which scan (sequential vs index), which join algorithm (nested-loop vs hash vs sort-merge), which aggregation strategy. One logical plan corresponds to many physical plans that return identical results at wildly different costs. The separation matters because two distinct kinds of transformation act on the two levels. Rule-based rewrites — predicate pushdown, projection pushdown, join reordering, constant folding — map logical plan to logical plan and must be semantics-preserving: the result set is unchanged for every possible input. Cost-based selection then maps logical plan to physical plan using a cost model fed by catalog statistics such as row counts, selectivity estimates, and index availability. CMU's database course draws exactly this line between the optimizer, which plans, and the execution engine, which runs operators.

### Pull vs Push, and Pipeline-Breakers

The volcano model is a pull engine: control flows top-down as the consumer calls `Next`, and data flows bottom-up as return values. Its dual is a push engine, where each operator, on producing a tuple, calls a consume callback on its parent, so control and data both flow bottom-up. Push pipelines, as used by HyPer and DuckDB's morsel model, keep the hot tuple in registers across several operators with no per-tuple virtual call into a child, which is friendlier to the CPU and to code generation; pull is simpler to write, debug, and reason about, and composes cleanly under one operator interface, which is why it remains the teaching model.

Independent of pull versus push, operators split into two classes. Streaming (pipelined) operators emit one output per input without buffering the whole input: sequential scan, filter, projection, nested-loop join, limit, and the merge phase of a sort-merge join. Pipeline-breakers (blocking operators) must consume their entire input before emitting the first output: sort, the build side of a hash join, hash aggregation (GROUP BY), and a top-N built from a heap. A pipeline-breaker is where the engine materializes state, and therefore where memory pressure and spilling to disk appear. Knowing which operators block tells you a plan's memory profile at a glance: a chain of streaming operators runs in O(1) state, while each blocking operator adds an O(rows) buffer (or O(k) for top-N).

### Iterator vs Vectorized vs Compiled Execution

Three execution models trade simplicity for throughput. The iterator (volcano) model processes one tuple per `Next` call; it is simple and memory-frugal but pays a virtual call and a branch per operator per tuple, so interpreter overhead dominates on large scans. Vectorized execution, pioneered by MonetDB/X100 and used by DuckDB, amortizes that overhead by passing a batch — a vector of, say, 1024 values — through each `Next`; the inner loop over the batch is tight, branch-predictable, and SIMD-friendly, so per-tuple dispatch cost falls by orders of magnitude while the interface stays recognizably volcano-shaped. Compiled execution, as in HyPer, goes further and generates machine code for an entire pipeline, fusing operators so a tuple flows through filter, project, and aggregate with no interface boundary at all. The trade-off is engineering cost and compile latency: interpreted iterators start instantly and are trivial to extend; vectorization needs columnar batches and per-type kernels; compilation needs a code generator and a JIT. This lesson uses the iterator model precisely because its one-tuple-at-a-time clarity makes the protocol and the operator boundaries explicit.

### Join Algorithms and When Each Wins

Three equi-join algorithms cover the practical space. Nested-loop join rescans the inner relation once per outer row, costing O(|R|·|S|) comparisons; it is the only option for non-equi conditions such as ranges or arbitrary predicates, and it wins when one side is tiny or when an index turns the inner scan into a point lookup (index nested-loop join). Hash join builds an in-memory hash table on the smaller (build) side, then probes it with the larger side, costing O(|R|+|S|) for equi-joins; it is the usual winner when the build side fits in memory, and its costs are the blocking build phase, unordered output, and degradation to a partitioned (grace) hash join with disk spilling when the build side overflows. Sort-merge join sorts both inputs on the join key and merges them in O(|R| log|R| + |S| log|S|), dropping to O(|R|+|S|) when the inputs already arrive sorted — from an index scan or a prior merge join — and it wins on pre-sorted inputs, when the query needs output ordered by the join key, or when neither side fits a hash table but both fit through an external sort. All three satisfy the same operator interface, so the planner swaps among them on cost alone.

### Cost-Based Planning and Predicate Pushdown

The planner consults a catalog for row-count estimates and makes two cost-driven choices. First, scan strategy: if a usable index exists for an equality predicate on the WHERE column, choose an index scan (a point lookup); otherwise choose a sequential scan. Second, join strategy: if the build-side table is estimated below a threshold of rows, use a hash join; otherwise fall back to nested-loop. Predicate pushdown then splits a conjunctive WHERE clause — its ANDed conjuncts — into per-table sub-expressions, and pushes any sub-expression that references only one table down to that table's scan, so rows are discarded before they ever reach the join. The matching design choices in the code are: split the conjunction, check whether every table a conjunct references is available in a given subtree, push it if so, and re-assemble the residual cross-table conjuncts as a filter above the join.

### Pushdown as Semantics-Preserving Rewrites

Predicate pushdown and projection pushdown are the two highest-value rule-based rewrites. Predicate pushdown moves a filter as close to the scan as possible so rows are discarded before an expensive join; it is valid through an inner join because σ(R ⋈ S) with a single-table predicate on R equals σ(R) ⋈ S — the conjuncts are ANDed regardless of where they sit. It is not generally valid to push a predicate into the null-supplying side of an outer join, because a row the filter removes there would otherwise have survived NULL-padded, changing the cardinality; a correct optimizer therefore pushes only through inner joins. Projection pushdown prunes the columns a subtree emits down to the set actually referenced above it — the final projection plus every predicate and join-key column — and because operators resolve columns by name, dropping unreferenced columns cannot change the result. Both rewrites shrink the data volume flowing up the tree without changing the answer, which is exactly what "semantics-preserving" means and what a good optimizer test verifies by comparing the result sets of the original and rewritten plans.

### Three-Valued Logic in Predicates and Joins

A SQL predicate evaluates to TRUE, FALSE, or UNKNOWN, where UNKNOWN arises from any comparison involving NULL. A WHERE or HAVING clause keeps a row only when the predicate is TRUE; UNKNOWN behaves like FALSE for filtering but is not the same truth value, which is why `NULL AND FALSE = FALSE` (FALSE dominates a conjunction) while `NULL AND TRUE = UNKNOWN`, and symmetrically `NULL OR TRUE = TRUE` (TRUE dominates a disjunction) while `NULL OR FALSE = UNKNOWN`. Evaluating AND with a plain Go `&&` over a "NULL is false" coercion gets `NULL AND FALSE` right by luck but `NULL AND TRUE` wrong, leaking rows past the filter; the evaluator must special-case NULL before combining.

The same rule governs joins. An equi-join match requires the key comparison to be TRUE, and `NULL = NULL` is UNKNOWN, never TRUE, so a NULL-keyed row matches nothing — not even another NULL-keyed row. A correct hash join must therefore never insert a NULL build key into the table and never probe with a NULL key; a correct sort-merge join must skip NULL keys during the merge. Such rows still surface as the NULL-padded side of an outer join, but never as a join match. Getting this wrong is a classic correctness bug: hashing NULL to a sentinel makes NULL keys collide and join, silently producing rows that SQL says cannot exist. The complementary obligation is outer-join padding: a LEFT join must preserve every left row, emitting the ones with no match NULL-padded on the right; a RIGHT join preserves the probe side symmetrically.

## Common Mistakes

### Forgetting Three-Valued Logic in AND and OR

Evaluating `AND` as `left.ToBool() && right.ToBool()` over a "NULL coerces to false" rule gives `NULL AND FALSE = false`, which happens to be correct, but `NULL AND TRUE = false`, which is wrong — the answer must be UNKNOWN (NULL). Rows that should be suppressed then pass the filter. The fix is to check for NULL before combining: FALSE dominates a conjunction (`NULL AND FALSE = FALSE`), TRUE dominates a disjunction (`NULL OR TRUE = TRUE`), and every other NULL-involving combination is UNKNOWN. The expression evaluator handles these cases explicitly before falling through to a plain comparison.

### Hashing NULL Join Keys to a Sentinel

Hashing a NULL key to a fixed sentinel and inserting or probing it like any other key makes every NULL build key share a bucket and every NULL probe key find them, so NULL-keyed rows match one another. SQL says `NULL = NULL` is UNKNOWN, never TRUE, so those rows must never join, and the result silently gains rows that should not exist. The fix is to skip NULL keys entirely: never insert a build row whose key is NULL, and never probe with a NULL key. A NULL-keyed row can still appear as the NULL-padded side of an outer join, but never as a match, and the sort-merge join applies the same rule during the merge.

### Not Reinitializing the Inner Operator in a Nested-Loop Join

Opening the inner operator once and calling `Next` on it for every outer row drains it after the first outer row, so the join produces at most as many rows as the outer relation. A nested-loop join must rewind the inner relation for each new outer row by calling its `Init` again whenever the inner reaches end-of-stream, so each outer row sees the full inner relation.

### Building the Hash Table on the Larger Side

Always building on the left (or always on the probe) input ignores size: joining a million-row table with a hundred-row table by building a million-entry hash table wastes memory and time, when building the hundred-entry table and probing it a million times is far cheaper. The fix is to compare estimated row counts and build on the smaller side — but only for inner joins, because for an outer join the preserved side is fixed by the join type and the output column order is build-then-probe, so swapping would change the semantics.

### Pushing a Cross-Table Predicate Down to One Scan

Pushing a join predicate such as `users.id = orders.user_id` down to the `users` scan fails because `orders.user_id` is not in the `users` schema: the column resolves to nothing, the comparison yields NULL, and every `users` row is discarded. Only a conjunct whose referenced tables are all available in a subtree may be pushed into it; cross-table conjuncts must stay above the join as a residual filter. Splitting the WHERE clause into conjuncts and checking table availability per conjunct is what keeps pushdown correct.

### Returning Aliased Tuples from a Scan

Returning a direct pointer to a row stored in the table's backing array means any operator that retains tuples across `Next` calls — the build side of a hash join, a sort buffer — aliases into that storage, and the next read overwrites values it still holds. The scan must return a clone whose value slice is an independent copy, so every emitted tuple stands on its own.

---

Next: [01-value-and-three-valued-logic.md](01-value-and-three-valued-logic.md)
