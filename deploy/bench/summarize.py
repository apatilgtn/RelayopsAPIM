"""Summarise cmd/bench results into a markdown comparison table."""
import glob
import json
import os
import sys

sys.stdout.reconfigure(encoding="utf-8")

d = sys.argv[1] if len(sys.argv) > 1 else "results"
rows = []
for path in sorted(glob.glob(os.path.join(d, "*.json"))):
    r = json.load(open(path))
    opens = [s for s in r["steps"] if s["mode"] == "open"]
    closed = [s for s in r["steps"] if s["mode"] == "closed"]
    at = {s["target_rps"]: s for s in opens}
    peak = max(closed, key=lambda s: s["achieved_rps"]) if closed else None
    rows.append((r["label"], r["max_sustained_rps"], at, peak))

rates = sorted({rate for _, _, at, _ in rows for rate in at})
print("| Gateway / scenario | Max sustained (p99 <= 50 ms) | " +
      " | ".join(f"p50 / p99 @ {r:,}" for r in rates) + " | Peak (closed loop) |")
print("|---|---|" + "---|" * len(rates) + "---|")
for label, sustained, at, peak in rows:
    cells = []
    for rate in rates:
        s = at.get(rate)
        if not s:
            cells.append("")
        elif not s["sustained"]:
            cells.append(f"✗ ({s['p99_ms']:.0f} ms, {s['error_percent']:.1f}% err)")
        else:
            cells.append(f"{s['p50_ms']:.2f} / {s['p99_ms']:.1f}")
    pk = f"{peak['achieved_rps']:,.0f} rps (p99 {peak['p99_ms']:.0f} ms)" if peak else ""
    print(f"| {label} | {sustained:,.0f} rps | " + " | ".join(cells) + f" | {pk} |")
