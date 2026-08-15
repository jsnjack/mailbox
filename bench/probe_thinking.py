#!/usr/bin/env python3
"""
One-shot probe: find a way to get a parseable classification out of a REASONING
model through lemonade's OpenAI-compatible API.

Loads the target once (so it evicts your loaded model only once), then tries
several documented ways to suppress or survive chain-of-thought, and reports
which produce usable content. Restores the original model at the end.

  python3 probe_thinking.py [model] [--keep]
"""
import json
import os
import subprocess
import sys
import time
import urllib.request

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from prompt import CATEGORIZE_SYSTEM
import cases as synthetic

API = os.environ.get("LEMONADE_API", "http://localhost:13305/api/v1")
MODEL = next((a for a in sys.argv[1:] if not a.startswith("-")),
             "Qwen3.5-35B-A3B-GGUF")


def loaded():
    out = subprocess.run(["lemonade", "status"], capture_output=True,
                         text=True, timeout=30).stdout
    for line in out.splitlines():
        if "llamacpp" in line and line.strip():
            return line.split()[0]
    return None


def load(m):
    subprocess.run(["lemonade", "load", m], capture_output=True, text=True,
                   timeout=900)


def call(body, label):
    req = urllib.request.Request(f"{API}/chat/completions",
                                 data=json.dumps(body).encode(),
                                 headers={"Content-Type": "application/json"})
    t = time.perf_counter()
    try:
        with urllib.request.urlopen(req, timeout=600) as r:
            payload = json.load(r)
    except Exception as e:
        return {"label": label, "error": f"{type(e).__name__}: {e}"}
    el = time.perf_counter() - t
    msg = payload["choices"][0].get("message") or {}
    u = payload.get("usage") or {}
    return {
        "label": label,
        "secs": round(el, 1),
        "ctok": u.get("completion_tokens"),
        "message_keys": sorted(msg.keys()),
        "content": (msg.get("content") or "")[:120],
        "reasoning_len": len((msg.get("reasoning_content")
                              or msg.get("reasoning") or "")),
        "finish": payload["choices"][0].get("finish_reason"),
    }


def main():
    cases, _ = synthetic.load()
    item = cases[0]["item"]          # a Calendar case: "Accepted: ..."
    user = json.dumps([item])
    base = {"model": MODEL, "temperature": 0.0, "stream": False}

    attempts = [
        ("max_tokens=512 (current)", dict(base, max_tokens=512, messages=[
            {"role": "system", "content": CATEGORIZE_SYSTEM},
            {"role": "user", "content": user}])),
        ("max_tokens=2048", dict(base, max_tokens=2048, messages=[
            {"role": "system", "content": CATEGORIZE_SYSTEM},
            {"role": "user", "content": user}])),
        ("chat_template_kwargs enable_thinking=false", dict(
            base, max_tokens=64, chat_template_kwargs={"enable_thinking": False},
            messages=[{"role": "system", "content": CATEGORIZE_SYSTEM},
                      {"role": "user", "content": user}])),
        ("reasoning_effort=none", dict(base, max_tokens=64,
            reasoning_effort="none",
            messages=[{"role": "system", "content": CATEGORIZE_SYSTEM},
                      {"role": "user", "content": user}])),
        ("/no_think suffix", dict(base, max_tokens=64, messages=[
            {"role": "system", "content": CATEGORIZE_SYSTEM},
            {"role": "user", "content": user + " /no_think"}])),
        ("prefill assistant with '['", dict(base, max_tokens=64, messages=[
            {"role": "system", "content": CATEGORIZE_SYSTEM},
            {"role": "user", "content": user},
            {"role": "assistant", "content": "["}])),
    ]

    orig = loaded()
    print(f"currently loaded : {orig}")
    print(f"probing          : {MODEL}\n")
    if orig != MODEL:
        print(f"loading {MODEL} ...", flush=True)
        load(MODEL)
        call(dict(base, max_tokens=5, messages=[{"role": "user", "content": "hi"}]),
             "warmup")

    results = []
    try:
        for label, body in attempts:
            r = call(body, label)
            results.append(r)
            if r.get("error"):
                print(f"  {label:<44} ERROR {r['error'][:60]}", flush=True)
            else:
                ok = "OK  " if r["content"].strip() else "BLANK"
                print(f"  {label:<44} {ok} ctok={r['ctok']:<5} "
                      f"{r['secs']:>5}s reasoning={r['reasoning_len']:<5} "
                      f"finish={r['finish']}", flush=True)
                if r["content"].strip():
                    print(f"       content: {r['content']!r}")
                if r["message_keys"]:
                    print(f"       message keys: {r['message_keys']}")
    finally:
        if orig and orig != MODEL and "--keep" not in sys.argv:
            print(f"\nrestoring {orig} ...")
            load(orig)

    usable = [r for r in results if not r.get("error") and r["content"].strip()]
    print(f"\n{len(usable)}/{len(results)} attempts produced usable content")
    if usable:
        best = min(usable, key=lambda r: r["secs"])
        print(f"fastest usable: {best['label']}  ({best['secs']}s, "
              f"{best['ctok']} tokens)")
    else:
        print("none worked -- this model cannot be made to answer briefly here")
    with open(os.path.join(os.path.dirname(os.path.abspath(__file__)),
                           "probe_thinking.json"), "w") as fh:
        json.dump({"model": MODEL, "attempts": results}, fh, indent=2)


if __name__ == "__main__":
    main()
