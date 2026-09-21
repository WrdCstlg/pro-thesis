"""OBS-LIVE-001 independent cross-check.

Observer-side verification: never trust verdict.json. Recompute the anomaly
claim from the raw normative history only.

Checks:
  C1  Every witness op_id cited by the oracle exists in the history.
  C2  Real-time stale-read scan, three variants printed side by side:
      v1  naive "newest completed write" heuristic (UNSOUND, convicts
          clean histories; kept only to document the original error).
      v2  the 2026-09-17 audit's proposed criterion,
          W_B.complete < W_A.complete < R.invoke (STILL UNSOUND:
          complete<complete is not real-time precedence).
      v3  sound criterion: R returning v is a hard violation iff the
          write W_B that produced v completed, and some write W_A on the
          key satisfies W_B.complete < W_A.invoke AND
          W_A.complete < R.invoke. Then W_B precedes W_A precedes R in
          every linearization, so R must return W_A's value or newer.
          v3 is conservative: it may miss violations, never invent them.
  C3  Reproduce the oracle's specific witness under the v3 criterion.

Usage: python crosscheck.py <run_dir>
"""

import json
import sys
import glob
import os


def load_history(path):
    recs = []
    with open(path, encoding="utf-8", errors="replace") as f:
        for line in f:
            line = line.strip()
            if line:
                recs.append(json.loads(line))
    return recs


def main(run_dir):
    hists = sorted(glob.glob(os.path.join(run_dir, "world-*", "history.jsonl")))
    assert hists, "no history found"
    recs = load_history(hists[0])
    print(f"history: {hists[0]}")
    print(f"records: {len(recs)}")

    # Witness op_ids come from the run's own verdict (the claim under test).
    # A clean run has no violations: C1/C3 are skipped, C2 still must find 0.
    wit = None
    vp = os.path.join(run_dir, "verdict.json")
    if os.path.exists(vp):
        with open(vp, encoding="utf-8") as f:
            verdict = json.load(f)
        if verdict.get("violations"):
            wit = verdict["violations"][0]["witness"]
    if wit is not None:
        print(f"verdict witness: key={wit['key']} op_ids={wit['op_ids']}")
    else:
        print("verdict witness: none (clean run); C1/C3 skipped, C2 must report 0")

    # Index completions and invocations by op_id.
    invoke, done = {}, {}
    for r in recs:
        oid = r.get("op_id")
        t = r.get("type")
        if oid is None:
            continue
        if t == "invoke":
            invoke[oid] = r
        elif t in ("ok", "fail", "info"):
            done[oid] = r

    # C1: witness ops present
    if wit is not None:
        w_old, w_new, w_read = wit["op_ids"]  # older write, newer write, stale read
        for oid in (w_old, w_new, w_read):
            assert oid in invoke and oid in done, f"witness op {oid} missing"
        print(f"C1 OK: witness op_ids {w_old}, {w_new}, {w_read} all present with invoke+completion")

    # t_ns origin: use the earliest invoke as t0 for readable relative times.
    t0 = min(r["t_ns"] for r in invoke.values())

    def ms(r):
        return (r["t_ns"] - t0) / 1e6

    # C2 (v1, UNSOUND; kept for comparison only): a read is flagged when its
    # value differs from the newest write COMPLETED before the read was invoked.
    # This ignores that the write which produced the returned value may have
    # overlapped that newer write and can therefore be linearized AFTER it.
    # Audit of 2026-09-17: v1 convicted clean histories (false positives).
    writes = {}
    for oid, r in done.items():
        if r.get("type") == "ok" and r.get("f") == "write":
            writes.setdefault(r.get("key"), []).append((r["t_ns"], r.get("value"), oid))
    v1_violations = []
    reads = 0
    for oid, r in done.items():
        if r.get("type") == "ok" and r.get("f") == "read":
            reads += 1
            key, val = r.get("key"), r.get("value")
            inv = invoke.get(oid)
            if inv is None:
                continue
            t_inv = inv["t_ns"]
            prior = [w for w in writes.get(key, []) if w[0] <= t_inv]
            if not prior:
                continue
            newest = max(prior)
            if newest[1] != val:
                v1_violations.append((oid, key, val, newest))
    print(f"C2 v1 (unsound, kept for reference): {len(v1_violations)} flagged of {reads} reads")

    # C2 (v2, the audit's proposed criterion; still unsound, kept for
    # comparison): flags when the producing write W_B completed before some
    # W_A completed, and W_A completed before R was invoked.
    #     W_B.complete < W_A.complete < R.invoke
    # FLAW: complete < complete is not real-time precedence. If W_A was
    # INVOKED before W_B completed, the two overlapped and W_A can be
    # linearized BEFORE W_B, making R's return value legal.
    producer = {}  # (key, value) -> (invoke_t, complete_t) of the write
    for oid, r in done.items():
        if r.get("type") == "ok" and r.get("f") == "write":
            iw = invoke.get(oid)
            if iw is not None:
                producer[(r.get("key"), r.get("value"))] = (iw["t_ns"], r["t_ns"])
    v2_violations = []
    for oid, r in done.items():
        if r.get("type") == "ok" and r.get("f") == "read":
            key, val = r.get("key"), r.get("value")
            inv = invoke.get(oid)
            if inv is None:
                continue
            wb = producer.get((key, val))
            if wb is None:
                continue
            for (ct, aval, aoid) in writes.get(key, []):
                if wb[1] < ct < inv["t_ns"]:
                    v2_violations.append(oid)
                    break
    print(f"C2 v2 (audit criterion, still unsound): {len(v2_violations)} flagged")

    # C2 (v3, sound): R returning v is a HARD violation iff the write W_B that
    # produced v completed AND there is a completed write W_A on the key with
    #     W_B.complete < W_A.invoke   and   W_A.complete < R.invoke
    # i.e. W_B strictly PRECEDES W_A in real time, and W_A strictly precedes
    # R. Every linearization then orders W_B < W_A < R, so R must return W_A's
    # value or newer. Conservative by construction: it can miss violations a
    # full search would catch, but it can never manufacture one.
    v3_violations = []
    unattributed = 0
    for oid, r in done.items():
        if r.get("type") == "ok" and r.get("f") == "read":
            key, val = r.get("key"), r.get("value")
            inv = invoke.get(oid)
            if inv is None:
                continue
            wb = producer.get((key, val))
            if wb is None:
                unattributed += 1  # value no completed write produced; cannot judge
                continue
            t_inv = inv["t_ns"]
            for aoid2, wa in done.items():
                if wa.get("type") == "ok" and wa.get("f") == "write" and wa.get("key") == key:
                    iwa = invoke.get(aoid2)
                    if iwa is None:
                        continue
                    if wb[1] < iwa["t_ns"] and wa["t_ns"] < t_inv:
                        v3_violations.append((oid, key, val, aoid2))
                        break
    violations = v3_violations
    print(f"C2 v3 (sound): {len(violations)} hard real-time stale read(s) of {reads} reads "
          f"({unattributed} read(s) returned a value no completed write produced — skipped)")

    # Phase census: bucket op completions by the in-history phase markers.
    phase_at = []  # (t_ns, phase)
    for r in recs:
        if r.get("type") == "info" and r.get("event") == "phase":
            phase_at.append((r["t_ns"], r.get("phase")))
    phase_at.sort()
    buckets = {}
    for r in recs:
        if r.get("type") == "info" and r.get("event") == "phase":
            continue
        ph = "PRE"
        for (t, name) in phase_at:
            if t <= r["t_ns"]:
                ph = name
            else:
                break
        buckets[ph] = buckets.get(ph, 0) + 1
    print(f"phase census (op records incl. invoke/completion): "
          + ", ".join(f"{k}={v}" for k, v in sorted(buckets.items())))
    n_phase = len(phase_at)
    n_info_ops = sum(1 for r in recs if r.get("type") == "info" and r.get("event") != "phase")
    print(f"info census: {n_info_ops} op-info record(s) + {n_phase} phase marker(s)")

    # C3: the oracle's witness, recomputed under the sound v3 criterion:
    # W_old.complete < W_new.invoke (strict real-time precedence between the
    # two writes) AND W_new.complete < R.invoke, with R returning W_old's
    # value.
    ok = True
    if wit is not None:
        wo, wn, rd = done[w_old], done[w_new], done[w_read]
        ir, iwn = invoke[w_read], invoke[w_new]
        print("C3 witness reconstruction (ms since first invoke):")
        print(f"  op {w_old} write {wit['key']} -> {wo['value']}  completed t+{ms(wo):.1f}")
        print(f"  op {w_new} write {wit['key']} -> {wn['value']}  invoked t+{ms(iwn):.1f}, completed t+{ms(wn):.1f}")
        print(f"  op {w_read} read  {wit['key']} -> {rd['value']}  invoked t+{ms(ir):.1f}, completed t+{ms(rd):.1f}")
        ok = (
            rd["value"] == wo["value"]
            and wo["t_ns"] < iwn["t_ns"]   # W_old strictly precedes W_new
            and wn["t_ns"] < ir["t_ns"]    # W_new strictly precedes the read
        )
        margin_ms = (ir["t_ns"] - wn["t_ns"]) / 1e6
        print(f"C3 {'OK' if ok else 'FAIL'}: sound real-time witness; "
              f"read-invoke minus newer-write-complete margin = {margin_ms:.2f} ms")
    if violations:
        print("first 5 hard violations (read_op, key, returned_value, newer_completed_write_op):")
        for v in violations[:5]:
            print("  ", v[0], v[1], v[2], v[3])
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main(sys.argv[1]))
