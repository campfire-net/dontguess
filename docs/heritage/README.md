# Heritage Docs

These documents are from the toolrank era (March 2026) — when DontGuess was a tool discovery engine. The codebase was 17K LOC Python. It has been retired.

**What transfers:**
- 4-layer value stack (value-function.md) — correctness gates everything
- Three feedback loops (product-vision.md §"The Three Loops") — fast/medium/slow cadence
- Behavioral signals over preferences (value-function.md §1) — measure outcomes, not ratings
- Semantic matching architecture (search-learning.md) — vector embeddings for similarity
- Validation methodology (usefulness-validation.md) — arena, canary, tournament
- Bootstrap economics (bootstrap-harness.md) — cold-start signal accumulation

**What doesn't transfer:**
- Tool-specific scoring heuristics (8 dimensions: token efficiency, schema quality, etc.)
- Registry crawlers (brew, apt, npm, pypi, etc.)
- Local environment scanning (PATH, man pages)
- The "escape velocity" metric as defined (behavioral > heuristic) — replaced by marketplace equilibrium
- Python codebase — new implementation is Go, native to campfire ecosystem

These docs are reference material, not specifications. The active specs live in `docs/convention/` and `docs/design/`.
