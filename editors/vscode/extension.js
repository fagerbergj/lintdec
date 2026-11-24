"use strict";
const vscode = require("vscode");
const { execFile } = require("node:child_process");
const path = require("node:path");
const os = require("node:os");
const fs = require("node:fs");
const { toRanges } = require("./findings");

let diagnostics;
let status;
let log;
const running = new Map(); // uri -> true while a lint of that file is in flight

// binary: the configured path, else the usual install locations the extension host's PATH may lack.
function binary() {
  const configured = vscode.workspace.getConfiguration("lintdec").get("path");
  if (configured !== "lintdec") return configured;
  for (const p of [path.join(os.homedir(), ".local/bin/lintdec"), path.join(os.homedir(), "go/bin/lintdec"), "/opt/homebrew/bin/lintdec"]) {
    if (fs.existsSync(p)) return p;
  }
  return configured;
}

function lint(doc, manual) {
  if (doc.languageId !== "markdown" || doc.uri.scheme !== "file") {
    log.appendLine(`skip ${doc.uri.toString()}: language ${doc.languageId}, scheme ${doc.uri.scheme}`);
    if (manual) vscode.window.showInformationMessage("lintdec only checks saved Markdown files.");
    return;
  }
  if (running.get(doc.uri.toString())) return;
  const folder = vscode.workspace.getWorkspaceFolder(doc.uri);
  const cwd = folder ? folder.uri.fsPath : path.dirname(doc.uri.fsPath);
  const bin = binary();
  const args = ["--out", "json", "--quiet", path.relative(cwd, doc.uri.fsPath)];
  log.appendLine(`run ${bin} ${args.join(" ")} (cwd ${cwd})`);
  running.set(doc.uri.toString(), true);
  status.text = "$(sync~spin) lintdec";
  status.show();
  let finish;
  vscode.window.withProgress({ location: vscode.ProgressLocation.Window, title: `lintdec: checking ${path.basename(doc.uri.fsPath)}` }, () => new Promise((r) => (finish = r)));
  execFile(bin, args, { cwd, maxBuffer: 16 << 20 }, (err, stdout, stderr) => {
    running.delete(doc.uri.toString());
    finish();
    log.appendLine(`exit ${err ? err.code : 0}; ${(stderr || "").trim().split("\n").slice(-2).join(" | ")}`);
    // Exit 1 means findings; anything else non-zero is a real failure.
    if (err && err.code !== 1) {
      status.text = "$(error) lintdec";
      vscode.window.showWarningMessage(`lintdec failed: ${(stderr || err.message).trim().split("\n").pop()} (see Output → lintdec)`);
      return;
    }
    let findings = [];
    try {
      findings = JSON.parse(stdout || "null") || [];
    } catch (e) {
      log.appendLine(`bad JSON: ${e.message}`);
      status.text = "$(error) lintdec";
      return;
    }
    const list = toRanges(findings, doc.getText()).map((r) => {
      const d = new vscode.Diagnostic(new vscode.Range(r.startLine, r.startCol, r.endLine, r.endCol), r.message, r.severity);
      d.source = "lintdec";
      d.code = r.url ? { value: r.rule, target: vscode.Uri.parse(r.url) } : r.rule;
      return d;
    });
    diagnostics.set(doc.uri, list);
    status.text = `$(book) lintdec: ${list.length}`;
    if (manual) vscode.window.showInformationMessage(list.length ? `lintdec: ${list.length} finding${list.length > 1 ? "s" : ""} (see Problems)` : "lintdec: no findings");
  });
}

function activate(context) {
  diagnostics = vscode.languages.createDiagnosticCollection("lintdec");
  log = vscode.window.createOutputChannel("lintdec");
  status = vscode.window.createStatusBarItem(vscode.StatusBarAlignment.Left, 0);
  status.command = "lintdec.lintFile";
  const cfg = () => vscode.workspace.getConfiguration("lintdec");
  context.subscriptions.push(
    diagnostics,
    status,
    log,
    vscode.commands.registerCommand("lintdec.lintFile", () => {
      const ed = vscode.window.activeTextEditor;
      if (!ed) return vscode.window.showInformationMessage("lintdec: open a Markdown file in the editor first.");
      lint(ed.document, true);
    }),
    vscode.workspace.onDidSaveTextDocument((d) => cfg().get("runOnSave") && lint(d)),
    vscode.workspace.onDidOpenTextDocument((d) => cfg().get("runOnOpen") && lint(d)),
    vscode.workspace.onDidCloseTextDocument((d) => diagnostics.delete(d.uri)),
  );
}

module.exports = { activate, deactivate() {} };
