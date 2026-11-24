// Maps lintdec JSON findings to editor ranges; kept free of the vscode module so it can be tested with node.
"use strict";

const levels = { error: 0, warning: 1, info: 2 }; // vscode.DiagnosticSeverity Error, Warning, Information

// toRanges: one {startLine, startCol, endLine, endCol, severity, message, rule, url} per finding, 0-based.
function toRanges(findings, text) {
  const lines = text.split("\n");
  return (findings || []).map((f) => {
    // A file edited during the run can be shorter than the finding's line.
    const first = Math.min(Math.max(0, (f.line || 1) - 1), Math.max(0, lines.length - 1));
    const last = Math.min(lines.length - 1, Math.max(first, (f.end_line || f.line) - 1));
    const url = (f.source || "").match(/https?:\/\/\S+/);
    const out = {
      severity: levels[f.severity] ?? 2,
      message: f.message + (f.source ? `\n${f.source.replace(/\s*https?:\/\/\S+/, "")}` : ""),
      rule: f.rule,
      url: url ? url[0] : undefined,
    };
    // Word-level findings carry the judged words; underline just those when they appear in the unit.
    if (f.span) {
      for (let i = first; i <= last; i++) {
        const k = lines[i].toLowerCase().indexOf(f.span.toLowerCase());
        if (k >= 0) return { ...out, startLine: i, startCol: k, endLine: i, endCol: k + f.span.length };
      }
    }
    const indent = lines[first].length - lines[first].trimStart().length;
    return { ...out, startLine: first, startCol: indent, endLine: last, endCol: lines[last].length };
  });
}

module.exports = { toRanges };
