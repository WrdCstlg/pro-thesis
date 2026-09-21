"""Artifact forensics for the adversarial conformance suite.

WHY THIS EXISTS SEPARATELY FROM THE SUITE

Every probe in scripts/adversarial.ps1 has one rule: never believe what the
system says about itself. `verdict.json` is the system's OPINION. `history.jsonl`
and the raw world files are the EVIDENCE. This module only ever reads evidence,
so a defect in the reporting path cannot hide from it.

Each function returns (ok: bool, detail: str). A detail is written for a reader
who does not already know what the check was for.
"""

import json
import os
import sys
import glob


def _read_jsonl(path):
    """Yield parsed records, and count what would not parse.

    A line that does not parse is reported rather than skipped: a truncated or
    corrupt history is a fact about the run, and silently dropping it is how a
    checker ends up reporting "clean" over a file it could not read.
    """
    bad = 0
    with open(path, "r", encoding="utf-8", errors="replace") as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            try:
                yield json.loads(line)
            except Exception:
                bad += 1
    if bad:
        print(f"  note: {bad} unparseable line(s) in {os.path.basename(path)}", file=sys.stderr)


def history_stats(path):
    """Independent census of a history: what was invoked, what completed, per key."""
    stats = {
        "records": 0, "invoke": 0, "ok": 0, "fail": 0, "info": 0,
        "keys": {}, "op_ids": set(),
    }
    for r in _read_jsonl(path):
        stats["records"] += 1
        t = r.get("type", "")
        if t in stats:
            stats[t] += 1
        if t == "invoke":
            k = r.get("key")
            if k:
                stats["keys"][k] = stats["keys"].get(k, 0) + 1
        oid = r.get("op_id")
        if oid is not None:
            stats["op_ids"].add(oid)
    return stats


def check_witness_is_real(run_dir):
    """A6: every witness op_id a violation cites must EXIST in the history it cites.

    This is the check that catches an oracle citing evidence it did not have. A
    violation naming op ids that never appear in the history is not a witness; it
    is a number. The verdict is the claim under test, so it is read here only to
    extract the claim -- never to decide whether the claim is true.
    """
    verdict_p = os.path.join(run_dir, "verdict.json")
    if not os.path.exists(verdict_p):
        return True, "no verdict.json (run produced no verdict); nothing to cross-check"
    with open(verdict_p, encoding="utf-8") as f:
        verdict = json.load(f)
    violations = verdict.get("violations") or []
    if not violations:
        return True, "verdict claims no violations; nothing to cross-check"

    problems, checked = [], 0
    for v in violations:
        w = v.get("witness") or {}
        op_ids = w.get("op_ids") or []
        key = w.get("key")
        if not op_ids:
            continue
        # Find the world whose history backs this violation. The verdict does not
        # name one, so every world in the run is a candidate and the claim holds
        # if ANY of them contains the cited ops -- the weakest reading, chosen so
        # a pass here is never an artefact of guessing the wrong file.
        found_in = None
        for hist in sorted(glob.glob(os.path.join(run_dir, "world-*", "history.jsonl"))):
            st = history_stats(hist)
            if all(o in st["op_ids"] for o in op_ids):
                found_in = hist
                if key and key not in st["keys"]:
                    problems.append(
                        f"{v.get('oracle')}: cites key {key!r}, which no operation in "
                        f"{os.path.basename(os.path.dirname(hist))} touches"
                    )
                break
        checked += 1
        if found_in is None:
            problems.append(
                f"{v.get('oracle')}: witness op_ids {op_ids} appear in NO history in this run"
            )
    if problems:
        return False, "; ".join(problems)
    return True, f"{checked} violation witness(es) verified present in the history they cite"


def check_no_violation(run_dir, oracle=None):
    """A1: assert NO oracle (or a named one) reported a violation, read from results.

    Read from each world's own result.json rather than the run verdict, because a
    verdict that omits a violation is exactly one of the failures worth catching.
    """
    hits = []
    for res_p in sorted(glob.glob(os.path.join(run_dir, "world-*", "result.json"))):
        with open(res_p, encoding="utf-8") as f:
            res = json.load(f)
        for o in res.get("oracles") or []:
            if o.get("status") == "violated" and (oracle is None or o.get("oracle") == oracle):
                hits.append(f"{os.path.basename(os.path.dirname(res_p))}:{o.get('oracle')}")
    if hits:
        return False, "violation(s) reported: " + ", ".join(hits)
    n = len(glob.glob(os.path.join(run_dir, "world-*", "result.json")))
    return True, f"{n} world(s), no {oracle or 'oracle'} violation"


def count_reproductions(run_dir, oracle):
    """How many worlds in this run reported a violation from `oracle`, and how many ran."""
    worlds = sorted(glob.glob(os.path.join(run_dir, "world-*")))
    judged, hits = 0, 0
    for w in worlds:
        res_p = os.path.join(w, "result.json")
        if not os.path.exists(res_p):
            continue           # world did not complete; not evidence either way
        judged += 1
        with open(res_p, encoding="utf-8") as f:
            res = json.load(f)
        for o in res.get("oracles") or []:
            if o.get("oracle") == oracle and o.get("status") == "violated":
                hits += 1
                break
    return hits, judged, len(worlds)


def check_canonical_bytes(paths):
    """A5: every world file must survive decode -> re-encode BYTE-IDENTICALLY.

    World files are content-addressed by SHA-256 over their exact bytes, so a
    re-encode that differs by one space breaks every world_hash at once -- and it
    breaks them in someone else's clone, long after the mistake.

    The canonical form is: sorted keys, no spaces after separators, LF only, and
    no non-ASCII escaping beyond what JSON requires.
    """
    problems, n = [], 0
    for p in paths:
        with open(p, "rb") as f:
            raw = f.read()
        n += 1
        if b"\r" in raw:
            problems.append(f"{os.path.basename(p)}: contains CR; canonical form is LF only")
            continue
        try:
            doc = json.loads(raw.decode("utf-8"))
        except Exception as e:
            problems.append(f"{os.path.basename(p)}: does not parse as UTF-8 JSON: {e}")
            continue
        again = json.dumps(doc, sort_keys=True, separators=(",", ":"),
                           ensure_ascii=False).encode("utf-8")
        if again != raw.rstrip(b"\n"):
            # Report the first difference, because "bytes differ" on a 700-byte
            # document is not a debuggable message.
            a, b = raw.rstrip(b"\n"), again
            i = next((i for i in range(min(len(a), len(b))) if a[i] != b[i]), min(len(a), len(b)))
            problems.append(
                f"{os.path.basename(p)}: re-encode differs at byte {i}: "
                f"stored {a[max(0,i-20):i+20]!r} vs canonical {b[max(0,i-20):i+20]!r}"
            )
    if problems:
        return False, "; ".join(problems)
    return True, f"{n} world file(s) round-trip byte-identically"


def main():
    cmd = sys.argv[1]
    if cmd == "witness":
        ok, detail = check_witness_is_real(sys.argv[2])
    elif cmd == "no-violation":
        oracle = sys.argv[3] if len(sys.argv) > 3 else None
        ok, detail = check_no_violation(sys.argv[2], oracle)
    elif cmd == "count":
        hits, judged, total = count_reproductions(sys.argv[2], sys.argv[3])
        print(f"{hits} {judged} {total}")
        return 0
    elif cmd == "canonical":
        paths = []
        for pat in sys.argv[2:]:
            paths.extend(glob.glob(pat, recursive=True))
        ok, detail = check_canonical_bytes(paths)
    elif cmd == "stats":
        st = history_stats(sys.argv[2])
        st["op_ids"] = len(st["op_ids"])
        print(json.dumps(st, sort_keys=True))
        return 0
    else:
        print(f"unknown command {cmd!r}", file=sys.stderr)
        return 2
    print(("OK  " if ok else "FAIL ") + detail)
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
