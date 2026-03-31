#!/usr/bin/env python3
"""cf-embed: embedding service as a campfire convention.

Self-installs all-MiniLM-L6-v2 ONNX model on first run.
Joins a campfire and responds to embed requests with 384-dim vectors.

Usage:
  # Serve on a campfire (long-running):
  python3 cmd/embed/main.py serve --campfire <id-or-alias>

  # One-shot embed (for testing / CLI use):
  python3 cmd/embed/main.py embed "migrate campfire app to SDK 0.13"
"""

import argparse
import json
import os
import subprocess
import sys
import time
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


def cf(*args, input_data=None):
    """Run a cf command and return stdout."""
    cmd = ["cf"] + list(args)
    result = subprocess.run(
        cmd, capture_output=True, text=True, input=input_data
    )
    if result.returncode != 0:
        print(f"cf error: {result.stderr.strip()}", file=sys.stderr)
    return result.stdout.strip()


def serve(campfire_id, sess, tok):
    """Join campfire and respond to embed requests."""
    print(f"Embedding service starting on campfire {campfire_id[:12]}...", file=sys.stderr)

    # Read cursor: start from now (don't replay old messages).
    cursor = str(int(time.time() * 1e9))

    print("Listening for embed requests (tag: embed:request)...", file=sys.stderr)

    while True:
        # Poll for new messages with the embed:request tag.
        raw = cf("read", campfire_id, "--tag", "embed:request",
                 "--after", cursor, "--json")
        if not raw or raw == "[]":
            time.sleep(0.5)
            continue

        try:
            messages = json.loads(raw)
        except json.JSONDecodeError:
            time.sleep(0.5)
            continue

        for msg in messages:
            msg_id = msg.get("id", "")
            # Update cursor to avoid reprocessing.
            ts = msg.get("timestamp", 0)
            if isinstance(ts, (int, float)) and str(int(ts)) > cursor:
                cursor = str(int(ts))

            try:
                payload = msg.get("payload", {})
                if isinstance(payload, str):
                    payload = json.loads(payload)

                texts = payload.get("texts", [])
                text = payload.get("text", "")
                if text and not texts:
                    texts = [text]

                if not texts:
                    continue

                vectors = embed_batch(sess, tok, texts)

                response = json.dumps({
                    "vectors": vectors,
                    "model": "all-MiniLM-L6-v2",
                    "dim": 384,
                })

                cf("send", campfire_id,
                   "--tag", "embed:response",
                   "--reply-to", msg_id,
                   "--fulfills", msg_id,
                   "--payload", response)

                print(f"  embedded {len(texts)} text(s) for {msg_id[:8]}", file=sys.stderr)

            except Exception as e:
                print(f"  error processing {msg_id[:8]}: {e}", file=sys.stderr)


def main():
    parser = argparse.ArgumentParser(description="Embedding service on campfire")
    sub = parser.add_subparsers(dest="command")

    serve_cmd = sub.add_parser("serve", help="Long-running campfire service")
    serve_cmd.add_argument("--campfire", required=True, help="Campfire ID or alias")

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

    elif args.command == "serve":
        serve(args.campfire, sess, tok)


if __name__ == "__main__":
    main()
