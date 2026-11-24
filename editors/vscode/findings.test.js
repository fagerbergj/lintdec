"use strict";
const test = require("node:test");
const assert = require("node:assert");
const { toRanges } = require("./findings");

const doc = "# Title\n\nThe cache ⟦x⟧ no.\nA knowledge base that cannot forget will rot.\n\n- one\n- two";

test("word-level findings underline only the judged words", () => {
  const [r] = toRanges([{ rule: "md-inanimate-actor", severity: "info", line: 4, end_line: 4, span: "forget", message: "m", source: "Paper: quote https://arxiv.org/abs/1" }], doc);
  assert.deepStrictEqual([r.startLine, r.startCol, r.endLine, r.endCol], [3, 29, 3, 35]);
  assert.strictEqual(r.url, "https://arxiv.org/abs/1");
  assert.strictEqual(r.message, "m\nPaper: quote");
});

test("unit findings cover the unit from its first word to its last line", () => {
  const [r] = toRanges([{ rule: "md-list-not-parallel", severity: "info", line: 6, end_line: 7, message: "m" }], doc);
  assert.deepStrictEqual([r.startLine, r.startCol, r.endLine, r.endCol], [5, 0, 6, 5]);
});

test("a finding past the end of an edited file stays in range", () => {
  const [r] = toRanges([{ rule: "md-prose-list", severity: "info", line: 9, end_line: 12, message: "m" }], "# T\n\nshort line\n");
  assert.deepStrictEqual([r.startLine, r.endLine], [3, 3]);
});

test("a span missing from the text falls back to the unit", () => {
  const [r] = toRanges([{ rule: "md-wordy-phrase", severity: "warning", line: 4, span: "in order to", message: "m" }], doc);
  assert.deepStrictEqual([r.startLine, r.endLine, r.severity], [3, 3, 1]);
});
