#!/usr/bin/env python3
"""Score a lintdec --calibrate --threshold 0.50 --out jsonl run against a blind lab manifest."""

import json
import sys

DEFAULT_FLOORS = {
    "todo-no-owner": 0.9,
    "comment-history": 0.85,
    "flakey-test": 0.8,
    "comment-long": 0.8,
    "comment-tautology": 0.7,
    "comment-excuse": 0.7,
    "tautology-test": 0.7,
    "md-inanimate-actor": 0.6,
    "md-marketing": 0.85,
    "md-prose-list": 0.85,
    "md-list-not-parallel": 0.9,
    "md-step-not-command": 0.85,
    "md-no-purpose": 0.85,
}
VETO_CUTS = {"flakey-test": 0.3, "comment-excuse": 0.7}


def is_reportable(finding: dict) -> bool:
    rule = finding["rule"]
    floor = DEFAULT_FLOORS.get(rule, 0.75)
    cut = VETO_CUTS.get(rule, 0.5)
    veto = finding.get("veto")
    return finding["confidence"] >= floor and (veto is None or veto < cut)


def load_findings(path: str) -> list[dict]:
    with open(path) as f:
        return [json.loads(line) for line in f if line.strip()]


def match_target(findings: list[dict], rule: str, file_suffix: str, line: int) -> list[dict]:
    return [
        r for r in findings
        if r["rule"] == rule and r["file"].endswith(file_suffix) and r["line"] == line
    ]


def fmt_score(hit: dict) -> str:
    veto = hit.get("veto")
    vstr = f"{veto:.3f}" if veto is not None else "none"
    tag = "REP" if is_reportable(hit) else "SUP"
    return f"{tag} conf={hit['confidence']:.3f} veto={vstr}"


def score_rule(rule: str, spec: dict, findings: list[dict]) -> tuple[int, int, int, int]:
    loud_hits = loud_total = quiet_fps = quiet_total = 0
    for item in spec.get("loud", []):
        loud_total += 1
        hits = match_target(findings, rule, item["file"], item["line"])
        rep = [h for h in hits if is_reportable(h)]
        subsumed = [
            r["rule"] for r in findings
            if r["rule"] != rule and r["file"].endswith(item["file"])
            and r["line"] == item["line"] and is_reportable(r)
        ]
        status = fmt_score(hits[0]) if hits else "MISS (<0.50)"
        sub = f"  [also caught by: {', '.join(sorted(set(subsumed)))}]" if subsumed else ""
        if rep:
            loud_hits += 1
        print(f"  LOUD  {item['file']}:{item['line']:<3d} -> {status}{sub}")

    for item in spec.get("quiet", []):
        quiet_total += 1
        hits = match_target(findings, rule, item["file"], item["line"])
        rep = [h for h in hits if is_reportable(h)]
        if rep:
            quiet_fps += 1
            print(f"  QUIET {item['file']}:{item['line']:<3d} -> FP! {fmt_score(rep[0])}")
    return loud_hits, loud_total, quiet_fps, quiet_total


def main() -> int:
    if len(sys.argv) != 3:
        print("usage: score-lab.py <findings.jsonl> <manifest.json>", file=sys.stderr)
        return 2
    findings = load_findings(sys.argv[1])
    with open(sys.argv[2]) as f:
        manifest = json.load(f)

    tot_lh = tot_lt = tot_qfp = tot_qt = 0
    for rule, spec in sorted(manifest.items()):
        print(f"\n[{rule}]")
        lh, lt, qfp, qt = score_rule(rule, spec, findings)
        tot_lh += lh
        tot_lt += lt
        tot_qfp += qfp
        tot_qt += qt
        print(f"  => recall {lh}/{lt}, quiet FPs {qfp}/{qt}")

    print(f"\nTOTAL: loud recall {tot_lh}/{tot_lt}, quiet FPs {tot_qfp}/{tot_qt}")
    return 0 if (tot_lh == tot_lt and tot_qfp == 0) else 1


if __name__ == "__main__":
    raise SystemExit(main())
