# FP ledger for rubric tuning

Collected 2026-02-19 from the full active-repo lint (quack 83, home-server 16, dotagents 28, games 2 findings; NightsOut clean). Each entry: what fired, why it is a false positive or weak, the rubric gap, and the proposed patch. Lab plants proposed per class so the calibration set covers the fix.

## md-table-dense

### FP-1: working-document tables (the big one, ~90 findings)

Evidence:
- quack `.quack/v1-design/audits/*` (60 of the 60 quack hits are in v1-design): audit tables with columns like Agent | Evidence | What it shows, SRS requirement tables, RFC extension tables
- home-server ops docs (11): hardware inventory ("Board cell packs multiple distinct specs"), networking setup, LLM build notes
- dotagents skills references (19): `http-methods.md` (PATCH semantics cell), `richardson-model.md`, `spec-requirements.md`, mermaid shape tables, `api-versioning.md`, `severity.md`

Why FP: these are reference/working documents where the cell IS the content. The semantics of PATCH cannot be a short phrase; an audit's "What it shows" column exists to carry the reasoning. The rule has no medium or audience test, so it floods precisely the docs whose tables are load-bearing.

Rubric gap: md-jargon carries an audience clause ("operational and personal notes … legitimately use their domain's vocabulary"); md-table-dense has none.

Proposed patch (append to the rubric):
> Judge the file's role from its path and headings. Reference and working documents — API and protocol references, RFCs, SRS and audit records, design docs, hardware or network inventories, build notes — legitimately pack structured facts into cells, and their tables are the format, not a defect. Do not flag them. The target is reader-facing prose documents (public READMEs, tutorials, changelogs, user guides) where a cell carries a multi-sentence essay the reader must parse inline.

### FP-2: short two-clause cells

Evidence:
- games `README.md` protocol table: three rewrite rounds were needed before the table went clean. The last trigger was a ~15-word cell: "Join a room; a token from an earlier joined re-claims that seat and host role". Before that, an 8-item parallel enumeration with phase tags (`hit` … on your turn, `insurance` … in the insurance phase) was flagged despite the rubric already saying "do not flag cells whose length comes from many short parallel entries".

Why FP: the rubric's "multi-sentence content" trigger has no length floor. Two short clauses separated by a semicolon is not an essay, and a parallel enumeration with conditions is the reference form.

Proposed patch:
> A cell under ~25 words is not dense unless it packs three or more distinct facts. Parallel enumerations (a list of values, actions or options, each with at most one short qualifier) are the reference form of such tables and are never dense.

Lab plants to add (must stay quiet):
- a protocol reference table with semicolon two-clause cells and one parallel enumeration
- an audit table (Agent | Evidence | What it shows) with 2-3 sentence "What it shows" cells
- a hardware inventory table with multi-spec cells
Plants that must still fire:
- a public README table with a 4-sentence essay cell (the existing planted.md table)

## md-jargon

### FP-3: terms defined later in the same file

Evidence: quack `.quack/v1-design/rfc/extensions/slots.md:8` — the RFC Summary uses lease ledger / GenerationTask / fake clock; all are defined in the Design section of the same file. The rubric says "do not flag terms defined or referenced within the file", but the model only looks at the flagged paragraph.

Proposed patch:
> A term that is defined, introduced or explained anywhere in the same file — including later sections — is not jargon, even when the paragraph using it comes before the definition.

Lab plant: an RFC-shaped doc whose Summary uses a term that its Design section defines (must stay quiet), alongside the existing broad-audience plant.

### FP-4: a product's own component nouns in its own README

Evidence: quack `README.md:3` — "A distributor runs one session process per conversation, each driving an agent bundle on pi-durable". Distributor/agent bundle are the product's names for its parts, explained in the docs the README links to.

Proposed patch:
> A product or project's own names for its components are not jargon in that project's own documentation, even when the names are internal.

## comment-tautology

### FP-5: restates the name but adds a mechanism

Evidence: dotagents lab `bigfile.go:29` — `// Put stores a value, evicting the oldest slot.` on method `Put`. The first clause restates the name, but the comment adds the eviction behavior, which the name does not carry. Firing tautology here overlaps with comment-restates-name (the more precise rule) and loses the "adds information" test.

Proposed patch:
> If any clause of the comment states a mechanism, constraint or behavior the name does not state, the comment is not tautological, even if another clause restates the name. For pure name restatement with added detail, comment-restates-name is the rule.

## md-para-long (weak, not clear FP — review list)

- home-server ops READMEs (5 hits): operational setup notes where dense paragraphs are the habit. The rubric already requires "mixes multiple distinct points", and the messages cite that, so these are probably right — but the rule has no audience clause like md-jargon's. Keep for now; revisit if ops notes keep flooding.

## Not FPs (verified true, for the record)

- games `README.md` hard-wraps at 103/125/136/171: genuine fixed-width wrapping, fixed and verified clean.
- quack `docs/README.md` 7 para-long: genuine 200-word CLI walls.
- quack v1-design para-long (~27): genuine multi-point walls even in design docs.

## Secondary tuning note

Confidence is still ~0.95 for nearly everything (0.85-0.99 band, no spread). It works as a gate; it is useless as a sort key. If we ever sort by it in CI triage, add explicit spread anchors to the system prompt ("0.6 = pattern present but the unit could be intentional; 0.85+ only when a human would agree without rereading").

## Status of proposed rubric changes — APPLIED 2026-02-19

All five rubric patches are in rules.go. New quiet plants added: `references.md` (Q1-Q3 protocol/audit/inventory tables), README sections (Q4 terms-defined-later, Q5 project's own names), bigfile.go:29 (Q6, restated as a constraint the code does not state). Calibration: two fresh runs both 22/22 loud + 6/6 quiet. One bad plant caught by calibration: the original Q6 comment (`evicting the oldest slot`) was rightly flagged — the ring write is visible eviction — so the plant now carries a constraint the code does not state.
Residual: 4 borderline units (lab hard-wrapped paragraphs, the ParseFlags doc comment, one decision-record) flip between fresh runs at temperature 0 — GPU-batching nondeterminism confined to the borderline. 22/22 and the quiet plants are stable across runs.
Structural guards (in lint.go, enforced after the model verdict): a single-line unit can never be md-hard-wrap, and md-hard-wrap needs at least two mid-sentence line breaks — a definition of where the line breaks fall, not a size judgment; and findings anchor to the unit's real span, because the model's line numbers drift once the file is edited around the unit.

Judgment-only correction (2026-11-21): the numeric floors were removed — the 80-word code floor for md-para-long and the "N sentences/words/lines/parameters" anchors in the md-para-long, comment-long, complex-method, iface-god-method and md-table-dense rubrics. Standing rule is judgment, no math, and thresholds train the model to count instead of read. The md-para-long rubric now says "separate topic, where a reader must reorient" — a sentence or clause that develops the same point is one point. Re-calibrated 26/26 twice, quiet plants silent. Residual: a small class of dense single-topic paragraphs (a ~120-word one-idea paragraph) still flips between runs; that is the model's honest borderline, left to judgment rather than re-floored.
