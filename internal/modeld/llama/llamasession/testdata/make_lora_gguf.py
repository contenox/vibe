#!/usr/bin/env python3
"""Generate a synthetic GGUF LoRA adapter for a llama.cpp base GGUF model.

Reads the base model's real tensor dims so shapes are guaranteed to satisfy
llama.cpp's adapter loader (src/llama-adapter.cpp):
  base ne[0] == lora_a.ne[0] (= n_in)
  base ne[1] == lora_b.ne[1] (= n_out)
  lora_a.ne[1] == lora_b.ne[0] (= rank)
gguf-py reverses numpy shape -> GGML ne, so for GGML ne [n_in, rank] / [rank, n_out]
the numpy arrays are (rank, n_in) and (n_out, rank) — i.e. standard PEFT A/B.

Usage: PYTHONPATH=<llama.cpp>/gguf-py python make_lora_gguf.py <base.gguf> <out.gguf> [scale]
"""
import sys
import numpy as np
import gguf

base_path = sys.argv[1]
out_path = sys.argv[2]
scale = float(sys.argv[3]) if len(sys.argv) > 3 else 0.05
RANK = 8
PROJ = ("attn_q", "attn_k", "attn_v", "attn_output")

reader = gguf.GGUFReader(base_path)
arch_field = reader.fields["general.architecture"]
arch = bytes(arch_field.parts[arch_field.data[0]]).decode()

# base tensor name -> GGML ne (raw reader .shape is in ne order)
suffixes = tuple(f".{p}.weight" for p in PROJ)
dims = {}
for t in reader.tensors:
    if t.name.endswith(suffixes):
        dims[t.name] = tuple(int(x) for x in t.shape)

rng = np.random.default_rng(0)
writer = gguf.GGUFWriter(out_path, arch)
writer.add_type(gguf.GGUFType.ADAPTER)
writer.add_string(gguf.Keys.Adapter.TYPE, "lora")
writer.add_float32(gguf.Keys.Adapter.LORA_ALPHA, float(RANK))  # alpha=rank -> alpha/rank=1

n = 0
for name, ne in dims.items():
    n_in, n_out = ne[0], ne[1]
    a = (rng.standard_normal((RANK, n_in)) * scale).astype(np.float32)   # ne [n_in, rank]
    b = (rng.standard_normal((n_out, RANK)) * scale).astype(np.float32)  # ne [rank, n_out]
    writer.add_tensor(f"{name}.lora_a", a)
    writer.add_tensor(f"{name}.lora_b", b)
    n += 1

writer.write_header_to_file()
writer.write_kv_data_to_file()
writer.write_tensors_to_file()
writer.close()
print(f"wrote {out_path}: arch={arch}, {n} adapted tensors ({n*2} lora tensors), scale={scale}")
