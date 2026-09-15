#!/usr/bin/env python3
"""Prepare the two explicit Go overlays used by the opt-in SDR caller tests."""
import argparse
import hashlib
import json
from pathlib import Path

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--output", type=Path, required=True)
args = parser.parse_args()
root = Path(__file__).resolve().parents[1]
out = args.output.resolve()
out.mkdir(parents=True, exist_ok=False)
replacements = {
    "lib/ffi/sdr_funcs.go": ("commDcid, ffi.GenerateSDR, os.RemoveAll", "commDcid, SDRRetryTestNative(sb), os.RemoveAll"),
    "lib/ffi/sdr_retry_itest.go": ("SDRRetryAdapterActive = false", "SDRRetryAdapterActive = true"),
}
overlay, evidence = {}, []
for name, (old, new) in replacements.items():
    source = root / name
    text = source.read_text()
    if text.count(old) != 1:
        raise SystemExit(f"refusing changed adapter boundary: {name}")
    target = out / source.name
    target.write_text(text.replace(old, new))
    overlay[str(source)] = str(target)
    evidence.append({"source": name, "sha256": hashlib.sha256(source.read_bytes()).hexdigest(), "replace": old, "with": new})
(out / "overlay.json").write_text(json.dumps({"Replace": overlay}, indent=2) + "\n")
(out / "adapter.json").write_text(json.dumps(evidence, indent=2) + "\n")
print(out / "overlay.json")
