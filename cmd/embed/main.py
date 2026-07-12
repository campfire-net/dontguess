#!/usr/bin/env python3
"""dg-embed: one-shot embedding CLI (all-MiniLM-L6-v2, 384-dim).

Self-installs the all-MiniLM-L6-v2 ONNX model on first run and embeds text to
L2-normalized 384-dim vectors. Invoked by the dense embedder
(pkg/matching/dense_embedder.go) as `python3 cmd/embed/main.py embed --json ...`
and usable standalone for testing.

Nostr-first (dontguess-ed2, design §3.10): the former campfire `serve` mode — a
live `cf read`/`cf send` client that joined a campfire and answered embed
requests — has been RETIRED. dontguess is campfire-free; the operator calls this
script directly as a subprocess embedder, no message bus.

Usage:
  python3 cmd/embed/main.py embed "migrate app to SDK 0.13"
  python3 cmd/embed/main.py embed --json "text one" "text two"
"""

import argparse
import json
import subprocess
import sys
from pathlib import Path

import numpy as np
import onnxruntime as ort
from tokenizers import Tokenizer

MODEL_DIR = Path.home() / ".local" / "lib" / "embed" / "all-MiniLM-L6-v2"
MODEL_URL = "https://huggingface.co/sentence-transformers/all-MiniLM-L6-v2/resolve/main/onnx/model.onnx"
TOKENIZER_URL = "https://huggingface.co/sentence-transformers/all-MiniLM-L6-v2/resolve/main/tokenizer.json"


def ensure_model():
    """Download model and tokenizer if not cached."""
    MODEL_DIR.mkdir(parents=True, exist_ok=True)
    model_path = MODEL_DIR / "model.onnx"
    tokenizer_path = MODEL_DIR / "tokenizer.json"

    for path, url in [(model_path, MODEL_URL), (tokenizer_path, TOKENIZER_URL)]:
        if not path.exists():
            print(f"Downloading {path.name}...", file=sys.stderr)
            subprocess.check_call(["curl", "-sL", "-o", str(path), url])
            print(f"  saved to {path}", file=sys.stderr)

    return model_path, tokenizer_path


def load_model(model_path, tokenizer_path):
    """Load ONNX session and tokenizer."""
    sess = ort.InferenceSession(
        str(model_path),
        providers=["CPUExecutionProvider"],
    )
    tok = Tokenizer.from_file(str(tokenizer_path))
    tok.enable_padding(length=128)
    tok.enable_truncation(max_length=128)
    return sess, tok


def embed(sess, tok, text):
    """Embed a single text string. Returns 384-dim float list."""
    encoded = tok.encode(text)
    input_ids = np.array([encoded.ids], dtype=np.int64)
    attention_mask = np.array([encoded.attention_mask], dtype=np.int64)
    token_type_ids = np.zeros_like(input_ids)

    outputs = sess.run(
        None,
        {
            "input_ids": input_ids,
            "attention_mask": attention_mask,
            "token_type_ids": token_type_ids,
        },
    )

    # Mean pooling over token embeddings, masked by attention.
    token_embeddings = outputs[0]  # (1, seq_len, 384)
    mask = attention_mask[..., np.newaxis].astype(np.float32)
    summed = (token_embeddings * mask).sum(axis=1)
    counts = mask.sum(axis=1).clip(min=1e-9)
    pooled = summed / counts

    # L2 normalize.
    norm = np.linalg.norm(pooled, axis=1, keepdims=True).clip(min=1e-9)
    normalized = (pooled / norm)[0]

    return normalized.tolist()


def embed_batch(sess, tok, texts):
    """Embed multiple texts. Returns list of 384-dim float lists."""
    if not texts:
        return []

    encodings = tok.encode_batch(texts)
    input_ids = np.array([e.ids for e in encodings], dtype=np.int64)
    attention_mask = np.array([e.attention_mask for e in encodings], dtype=np.int64)
    token_type_ids = np.zeros_like(input_ids)

    outputs = sess.run(
        None,
        {
            "input_ids": input_ids,
            "attention_mask": attention_mask,
            "token_type_ids": token_type_ids,
        },
    )

    token_embeddings = outputs[0]
    mask = attention_mask[..., np.newaxis].astype(np.float32)
    summed = (token_embeddings * mask).sum(axis=1)
    counts = mask.sum(axis=1).clip(min=1e-9)
    pooled = summed / counts

    norms = np.linalg.norm(pooled, axis=1, keepdims=True).clip(min=1e-9)
    normalized = pooled / norms

    return normalized.tolist()


def main():
    parser = argparse.ArgumentParser(description="One-shot embedding CLI (all-MiniLM-L6-v2)")
    sub = parser.add_subparsers(dest="command")

    embed_cmd = sub.add_parser("embed", help="One-shot embed")
    embed_cmd.add_argument("text", nargs="+", help="Text(s) to embed")
    embed_cmd.add_argument("--json", action="store_true", dest="as_json", help="Output as JSON")

    args = parser.parse_args()

    if not args.command:
        parser.print_help()
        sys.exit(1)

    model_path, tokenizer_path = ensure_model()
    sess, tok = load_model(model_path, tokenizer_path)
    print(f"Model loaded: {model_path}", file=sys.stderr)

    if args.command == "embed":
        if len(args.text) == 1:
            vec = embed(sess, tok, args.text[0])
            if args.as_json:
                print(json.dumps({"vector": vec, "model": "all-MiniLM-L6-v2", "dim": 384}))
            else:
                print(f"[{len(vec)}-dim] {vec[:5]}...")
        else:
            vecs = embed_batch(sess, tok, args.text)
            if args.as_json:
                print(json.dumps({"vectors": vecs, "model": "all-MiniLM-L6-v2", "dim": 384}))
            else:
                for i, v in enumerate(vecs):
                    print(f"[{i}] [{len(v)}-dim] {v[:5]}...")


if __name__ == "__main__":
    main()
