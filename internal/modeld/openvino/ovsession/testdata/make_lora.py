#!/usr/bin/env python3
"""Generate a synthetic LoRA safetensors adapter for a Qwen2-0.5B OpenVINO model.

Pure stdlib + numpy (no torch/peft). Used by the dynamic-LoRA smoke test
(genai_lora_test.go) to prove the adapter actually changes generation.

Tensor names MUST be canonical PEFT
(`base_model.model.model.layers.N.self_attn.q_proj.lora_A.weight`): OpenVINO
GenAI's prefix detection keys off the full name to map adapter tensors onto the
model's MatMul nodes. Shorter names (e.g. `model.layers.N...`) load but match
zero nodes — OpenVINO logs "unused LoRA tensors" and generation is unchanged
(empirically verified against OpenVINO GenAI 2026.2).

Targets attention projections (q/k/v/o) across all layers. MODE_DYNAMIC LoRA
applies even to int4 (u4) weight-compressed models because the dynamic transform
inserts on activations, not weights.

Usage: python make_lora.py <out.safetensors> [scale]   (scale default 0.05)
"""
import json
import struct
import sys
import numpy as np

HIDDEN = 896
LAYERS = 24
KV = 128          # num_key_value_heads(2) * head_dim(64)
RANK = 8
# Projection -> (out_features, in_features)
PROJ = {
    "q_proj": (HIDDEN, HIDDEN),
    "k_proj": (KV, HIDDEN),
    "v_proj": (KV, HIDDEN),
    "o_proj": (HIDDEN, HIDDEN),
}

out_path = sys.argv[1] if len(sys.argv) > 1 else "lora.safetensors"
scale = float(sys.argv[2]) if len(sys.argv) > 2 else 0.05
rng = np.random.default_rng(0)

tensors = {}  # name -> np.ndarray(float32)
for i in range(LAYERS):
    for proj, (out_f, in_f) in PROJ.items():
        # Canonical PEFT naming: this is what OpenVINO's prefix detection matches
        # against the model's MatMul nodes (verified: shorter names match 0 nodes).
        base = f"base_model.model.model.layers.{i}.self_attn.{proj}"
        # lora_A: [rank, in], lora_B: [out, rank]. Non-trivial B so delta != 0.
        a = (rng.standard_normal((RANK, in_f)) * scale).astype(np.float32)
        b = (rng.standard_normal((out_f, RANK)) * scale).astype(np.float32)
        tensors[f"{base}.lora_A.weight"] = np.ascontiguousarray(a)
        tensors[f"{base}.lora_B.weight"] = np.ascontiguousarray(b)
        # PEFT-style alpha scalar (scale = alpha/rank). alpha=rank -> scale 1.0.
        tensors[f"{base}.alpha"] = np.array([float(RANK)], dtype=np.float32)

# Build safetensors: 8-byte header length + JSON header + concatenated data.
header = {}
blob = bytearray()
for name, arr in tensors.items():
    data = arr.tobytes(order="C")
    start = len(blob)
    blob.extend(data)
    header[name] = {
        "dtype": "F32",
        "shape": list(arr.shape),
        "data_offsets": [start, len(blob)],
    }
header["__metadata__"] = {"format": "pt", "generated_by": "contenox smoke-test"}

header_bytes = json.dumps(header, separators=(",", ":")).encode("utf-8")
with open(out_path, "wb") as f:
    f.write(struct.pack("<Q", len(header_bytes)))
    f.write(header_bytes)
    f.write(bytes(blob))

print(f"wrote {out_path}: {len(tensors)} tensors, {8 + len(header_bytes) + len(blob)} bytes")
