#!/usr/bin/env python3
"""Score a lintdec --calibrate --threshold 0.50 --out jsonl run against a blind lab manifest."""

import json
import re
import sys
from pathlib import Path

# Mirror of veto.go ruleFloors / ruleVetoCuts / vetoConfidence; synced at runtime if veto.go is nearby.
DEFAULT_FLOORS = {
    "todo-no-owner": 0.9,
    "comment-history": 0.85,
    "flakey-test": 0.8,
    "comment-long": 0.8,
    "comment-tautology": 0.7,
    "tautology-test": 0.7,
    "md-inanimate-actor": 0.6,
    "md-prose-list": 0.85,
    "md-list-not-parallel": 0.9,
    "md-no-purpose": 0.85,
}
VETO_CUTS = {"flakey-test": 0.3}
DEFAULT_FLOOR = 0.75
DEFAULT_VETO_CUT = 0.5


def find_veto_go() -> Path | None:
    for parent in Path(__file__).resolve().parents:
        candidate = parent / "veto.go"
        if candidate.is_file():
            return candidate
    return None


def load_veto_config() -> tuple[dict[str, float], dict[str, float], float, str]:
    floors, cuts, default_cut = dict(DEFAULT_FLOORS), dict(VETO_CUTS), DEFAULT_VETO_CUT
    veto_go = find_veto_go()
    if veto_go is None:
        print("warning: veto.go not found in ancestor dirs; using frozen fallback floors/cuts", file=sys.stderr)
        return floors, cuts, default_cut, "fallback mirror"
    text = veto_go.read_text()
    missing = []
    for var_name, target in (("ruleFloors", floors), ("ruleVetoCuts", cuts)):
        m = re.search(rf"var\s+{var_name}\s*=\s*map\[string\]float64\{{([^}}]*)\}}", text, re.S)
        if m:
            target.clear()
            for rid, val in re.findall(r'"([^"]+)"\s*:\s*([0-9.]+)', m.group(1)):
                target[rid] = float(val)
        else:
            missing.append(var_name)
    cm = re.search(r"const\s+vetoConfidence\s*=\s*([0-9.]+)", text)
    if cm:
        default_cut = float(cm.group(1))
    else:
        missing.append("vetoConfidence")
    if missing:
        print(f"warning: {', '.join(missing)} not found in {veto_go}; using fallback values for those", file=sys.stderr)
        return floors, cuts, default_cut, f"veto.go + fallback ({', '.join(missing)})"
    return floors, cuts, default_cut, "veto.go"


def is_reportable(finding: dict, floors: dict[str, float], cuts: dict[str, float], default_cut: float) -> bool:
    rule = finding["rule"]
    floor = floors.get(rule, DEFAULT_FLOOR)
    cut = cuts.get(rule, default_cut)
    veto = finding.get("veto")
    return finding["confidence"] >= floor and (veto is None or veto < cut)


REQUIRED_FINDING_FIELDS = ("rule", "file", "line", "confidence")


def load_findings(path: str) -> list[dict]:
    rows = []
    try:
        with open(path) as f:
            for idx, raw in enumerate(f, 1):
                line = raw.strip()
                if not line:
                    continue
                try:
                    obj = json.loads(line)
                except json.JSONDecodeError as err:
                    raise ValueError(f"{path}:{idx}: invalid JSON: {err}") from err
                if not isinstance(obj, dict) or any(k not in obj for k in REQUIRED_FINDING_FIELDS):
                    req = ", ".join(REQUIRED_FINDING_FIELDS)
                    raise ValueError(f"{path}:{idx}: finding needs {req} (use --out jsonl, not a filtered capture or --out json)")
                rows.append(obj)
    except OSError as err:
        raise ValueError(str(err)) from err
    return rows


def load_manifest(path: str) -> object:
    try:
        with open(path) as f:
            return json.load(f)
    except OSError as err:
        raise ValueError(str(err)) from err
    except json.JSONDecodeError as err:
        raise ValueError(f"{path}: invalid JSON: {err}") from err


def matches_file(finding_file: str, suffix: str) -> bool:
    norm = finding_file.replace("\\", "/")
    want = suffix.replace("\\", "/").removeprefix("./").lstrip("/")
    return norm == want or norm.endswith("/" + want)


def match_target(findings: list[dict], rule: str, file_suffix: str, line: int) -> list[dict]:
    return [
        r for r in findings
        if r["rule"] == rule
        and matches_file(r["file"], file_suffix)
        and r["line"] <= line <= r.get("end_line", r["line"])
    ]


def fmt_score(hit: dict, floors: dict[str, float], cuts: dict[str, float], default_cut: float) -> str:
    veto = hit.get("veto")
    vstr = f"{veto:.3f}" if veto is not None else "none"
    tag = "REP" if is_reportable(hit, floors, cuts, default_cut) else "SUP"
    return f"{tag} conf={hit['confidence']:.3f} veto={vstr}"


def score_rule(
    rule: str,
    spec: dict,
    findings: list[dict],
    floors: dict[str, float],
    cuts: dict[str, float],
    default_cut: float,
) -> tuple[int, int, int, int]:
    loud_hits = loud_total = quiet_fps = quiet_total = 0
    for item in spec["loud"]:
        loud_total += 1
        hits = match_target(findings, rule, item["file"], item["line"])
        rep = [h for h in hits if is_reportable(h, floors, cuts, default_cut)]
        subsumed = [
            r["rule"] for r in findings
            if r["rule"] != rule
            and matches_file(r["file"], item["file"])
            and r["line"] <= item["line"] <= r.get("end_line", r["line"])
            and is_reportable(r, floors, cuts, default_cut)
        ]
        status = fmt_score(hits[0], floors, cuts, default_cut) if hits else "NO HIT at this line"
        sub = f"  [also caught by: {', '.join(sorted(set(subsumed)))}]" if subsumed else ""
        if rep:
            loud_hits += 1
        print(f"  LOUD  {item['file']}:{item['line']:<3d} -> {status}{sub}")

    for item in spec.get("quiet", []):
        quiet_total += 1
        hits = match_target(findings, rule, item["file"], item["line"])
        rep = [h for h in hits if is_reportable(h, floors, cuts, default_cut)]
        if rep:
            quiet_fps += 1
            print(f"  QUIET {item['file']}:{item['line']:<3d} -> FP! {fmt_score(rep[0], floors, cuts, default_cut)}")
        else:
            print(f"  QUIET {item['file']}:{item['line']:<3d} -> ok")
    return loud_hits, loud_total, quiet_fps, quiet_total


def validate_manifest(manifest: object) -> str | None:
    if not isinstance(manifest, dict) or not manifest:
        return "manifest must be a non-empty JSON object mapping rule ids to {loud, quiet}"
    for rule, spec in sorted(manifest.items()):
        if not isinstance(spec, dict) or not isinstance(spec.get("loud"), list) or not spec["loud"]:
            return f"manifest: {rule!r} needs a non-empty 'loud' list"
        for key in ("loud", "quiet"):
            items = spec.get(key, [])
            if not isinstance(items, list):
                return f"manifest: {rule!r}.{key} must be a list"
            for item in items:
                if not isinstance(item, dict) or not isinstance(item.get("file"), str) or not isinstance(item.get("line"), int):
                    return f"manifest: {rule!r}.{key} entries must have string 'file' and int 'line'"
    return None


def main() -> int:
    if len(sys.argv) != 3:
        print("usage: score-lab.py <findings.jsonl> <manifest.json>", file=sys.stderr)
        return 2
    try:
        findings = load_findings(sys.argv[1])
        manifest = load_manifest(sys.argv[2])
    except ValueError as err:
        print(f"error: {err}", file=sys.stderr)
        return 2
    if err := validate_manifest(manifest):
        print(f"error: {err}", file=sys.stderr)
        return 2

    floors, cuts, default_cut, source = load_veto_config()
    applied = ", ".join(
        f"{r}(floor={floors.get(r, DEFAULT_FLOOR):.2f}, cut={cuts.get(r, default_cut):.2f})"
        for r in sorted(manifest)
    )
    print(f"scoring: {source} (default floor={DEFAULT_FLOOR:.2f}, cut={default_cut:.2f}; {applied})")
    tot_lh = tot_lt = tot_qfp = tot_qt = 0
    for rule, spec in sorted(manifest.items()):
        print(f"\n[{rule}]")
        lh, lt, qfp, qt = score_rule(rule, spec, findings, floors, cuts, default_cut)
        tot_lh += lh
        tot_lt += lt
        tot_qfp += qfp
        tot_qt += qt
        print(f"  => recall {lh}/{lt}, quiet FPs {qfp}/{qt}")

    print(f"\nTOTAL: loud recall {tot_lh}/{tot_lt}, quiet FPs {tot_qfp}/{tot_qt}")
    return 0 if (tot_lh == tot_lt and tot_qfp == 0) else 1


if __name__ == "__main__":
    raise SystemExit(main())
