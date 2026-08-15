#!/usr/bin/env python3
"""
Benchmark models for mailbox's AI jobs -- local lemonade models AND remote
OpenAI-compatible endpoints (proteus) side by side -- then hand the raw outputs
to Claude for grading.

FIDELITY
  * categorisation uses the app's PRODUCTION system prompt, extracted from
    internal/ai/assistant.go by genprompt.py -- not a paraphrase
  * same protocol: JSON array in/out, temperature 0, one email per call
  * same item format: "From: <who> / Subject: <subj> / <snippet>"
  * same tolerant parsing: Python port of parseCategories + MatchCategory, so a
    model is scored on what mailbox would ACTUALLY store
  * 272 synthetic labelled emails (bench/cases.py) whose archetypes, category
    boundaries and decoys are modelled on a real inbox, but which contain no
    real names, senders or content -- committed and safe to share.

TARGET SYNTAX
  granite-4.1-8b-GGUF-Q5_K_M          local lemonade model
  local-qwen@proteus                  remote model on the proteus endpoint
  some-model@https://host/v1          any OpenAI-compatible endpoint
  Remote API keys come from $MAILBOX_AI_KEY, else the keyring
  (service "mailbox-ai", attribute username=<endpoint>) -- same place mailbox
  itself reads them. Never printed.

MISSING LOCAL MODELS
  A preflight lists which requested lemonade models are not downloaded and how
  many GB they need. Nothing is downloaded unless you pass --pull; without it
  those models are skipped and the rest still run.

USAGE
  python3 genprompt.py ~/workspace/mailbox      # once, and after prompt edits
  python3 bench_models.py --selftest            # ~1 min, no eviction
  python3 bench_models.py --quick               # speed + all 272 categories
  python3 bench_models.py                       # adds translation/summary/draft
  python3 bench_models.py --remote-models proteus          # what's on proteus
  python3 bench_models.py granite-4.1-8b-GGUF-Q5_K_M local-qwen@proteus
  python3 bench_models.py --pull Gemma-4-26B-A4B-it-MTP-GGUF
  --ab            run categorisation twice per model: the production prompt and
                  the SHAPE_TWEAK variant, and report the delta. Use this to
                  find out whether an under-tagging model is a prompt problem
                  rather than a model problem. Doubles categorisation time.
  --reps N        summary/draft repetitions (default 2)
  --cat-limit N   cap categorisation cases

Then give Claude artifact.json.

CAVEAT lemonade is Max Models/Type=1: each LOCAL load evicts the previous model,
so your mailbox local fallback points at whatever is loaded mid-run. The
originally-loaded model is restored in a finally block, including on Ctrl-C.
Remote targets evict nothing.
"""

import glob
import json
import os
import re
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from collections import Counter, defaultdict

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
try:
    from prompt import CATEGORIZE_SYSTEM, EMAIL_CATEGORIES
except ImportError:
    sys.exit("prompt.py missing -- run: python3 genprompt.py ~/workspace/mailbox")
import cases as synthetic

LEMONADE_API = os.environ.get("LEMONADE_API", "http://localhost:13305/api/v1")


def config_endpoints():
    """
    Endpoint aliases read from the user's OWN mailbox config
    (~/.config/mailbox/config.toml, [ai].endpoint plus every [[ai.chain]]),
    keyed by the first label of the host: https://proteus.example/v1 -> "proteus".

    Derived at runtime on purpose -- no private or internal hostname is baked
    into this committed file. Add more with
        BENCH_ENDPOINTS="name=https://host/v1,other=https://host2/v1"
    """
    out = {}
    try:
        import tomllib
        with open(os.path.expanduser("~/.config/mailbox/config.toml"), "rb") as fh:
            ai = (tomllib.load(fh).get("ai") or {})
    except Exception:
        ai = {}
    eps = ([ai["endpoint"]] if ai.get("endpoint") else []) + \
          [c["endpoint"] for c in (ai.get("chain") or []) if c.get("endpoint")]
    for ep in eps:
        host = urllib.parse.urlsplit(ep).hostname or ""
        alias = host.split(".")[0]
        if alias and alias not in out:
            out[alias] = ep
    for pair in filter(None, os.environ.get("BENCH_ENDPOINTS", "").split(",")):
        if "=" in pair:
            k, v = pair.split("=", 1)
            out[k.strip()] = v.strip()
    return out


ENDPOINT_ALIASES = {"lemonade": LEMONADE_API, **config_endpoints()}
OUTDIR = os.path.dirname(os.path.abspath(__file__))
ARTIFACT = os.path.join(OUTDIR, "artifact.json")
TIMEOUT = 900
# A model download that cannot finish in this long is a broken mirror, not a
# big file: bound it so one slow pull cannot block a whole sweep.
PULL_TIMEOUT = 1800

# The model mailbox is configured to use. Every run puts it first and reports
# the others as deltas against it, so "is this candidate better?" is answerable
# without remembering last week's numbers. Change it here when the app's
# [[ai.chain]] local entry changes.
BASELINE = "Gemma-4-E4B-it-GGUF"

DEFAULT_TARGETS = [BASELINE]

# Inbox mix used for the frequency-WEIGHTED score. Per-class recall shows where
# a model is weak; this shows what you would actually experience day to day.
# NB a model that blindly answers "Notification" scores ~87% weighted and ~30%
# raw -- always read both, plus "Needs reply" recall.
REAL_FREQ = synthetic.INBOX_MIX


def sample_across_classes(cases, n):
    """
    Take n cases that still cover every category, instead of the first n.

    The set is grouped by class, so cases[:60] is 60 Notifications and a capped
    run silently reports 0% on "Needs reply" — which reads as a model failure
    rather than a missing sample. Round-robin keeps the proportions roughly
    intact and guarantees each class appears while it can.
    """
    if n >= len(cases):
        return cases
    buckets = defaultdict(list)
    for c in cases:
        buckets[c["label"]].append(c)
    # largest class first, so the mix stays close to the real distribution
    order = sorted(buckets, key=lambda k: -len(buckets[k]))
    out, i = [], 0
    while len(out) < n:
        added = False
        for label in order:
            if i < len(buckets[label]):
                out.append(buckets[label][i])
                added = True
                if len(out) == n:
                    break
        if not added:
            break
        i += 1
    return sorted(out, key=lambda c: c["id"])


def load_cases():
    """The labelled set from cases.py -> (cases, meta)."""
    return synthetic.load()


# ---------------------------------------------------------------- targets

def keyring_key(endpoint):
    """Same lookup mailbox uses: service mailbox-ai, username=<endpoint>."""
    if os.environ.get("MAILBOX_AI_KEY"):
        return os.environ["MAILBOX_AI_KEY"]
    try:
        r = subprocess.run(["secret-tool", "lookup", "service", "mailbox-ai",
                            "username", endpoint],
                           capture_output=True, text=True, timeout=20)
        return r.stdout.strip() or None
    except Exception:
        return None


# ------------------------------------------------------------ prompt A/B
#
# The production prompt tells the model: "If none of these clearly applies, use
# an empty string "" for that email." A small model renders that as an EMPTY
# ARRAY [] instead of a one-element array [""] -- and once it is emitting [] it
# does so even for textbook-obvious mail (a dev newsletter, a firmware notice, a
# resignation letter). mailbox stores that as no tag, so the email silently
# arrives untagged.
#
# SHAPE_TWEAK pins the output shape without touching any category definition, so
# an A/B measures exactly one thing: does spelling out "[]" is never valid
# recover those emails? If it does, this is a prompt fix rather than a reason to
# change model.
SHAPE_TWEAK = (
    " OUTPUT SHAPE, follow exactly: your reply is a JSON array with exactly as "
    "many elements as the input array -- never fewer. An empty array [] is NEVER "
    "a valid reply. When no category applies to an email, that email's element is "
    "the empty string \"\", so one email with no category is [\"\"] and never []. "
    "For one email the reply is one element, e.g. [\"Notification\"] or [\"\"]."
)

# Measured on granite-4.1-8b over the 170-case set: Notification recall of 66.7%
# costs 29.0 of the 30.3 weighted points lost -- every other class combined costs
# 1.3. Notification is ~87% of a real inbox, so it is the ONLY class whose recall
# moves experienced quality.
#
# The production prompt calls Notification "the catch-all", but only states that
# it loses to Security and Newsletter. It never says Notification BEATS Receipt,
# Finance, Calendar or Needs reply -- and that is exactly where the misses go:
# "installation finished" and "power cycle completed" read as Receipt, "spend
# alert" reads as Finance, "X requested time off" reads as Needs reply.
NOTIFY_TWEAK = (
    " Precedence for automated mail: a message generated by a service about the "
    "recipient's own account, servers, projects, files or usage is "
    "\"Notification\" even when it reports a COMPLETED action (an install, a "
    "backup, a power cycle, a firmware update -- these are NOT \"Receipt\", "
    "which requires a purchase, payment or physical delivery), even when it "
    "mentions spend, quota or usage (NOT \"Finance\", which requires money owed "
    "or an account balance), even when its subject contains a word like meeting "
    "or sync, and even when it names a person who did something (NOT \"Needs "
    "reply\" -- no human is waiting on an answer from the recipient)."
)

# Second-biggest failure, and the most irritating in practice: granite tags 8 of
# 12 no-action emails as "Needs reply", including "Thanks! Much appreciated." and
# the user's own sent mail. Only ~0.5 weighted points, but a false "Needs reply"
# badge is exactly the error that makes someone stop trusting the tag.
ACK_TWEAK = (
    " \"Needs reply\" requires an OPEN question or request still waiting on the "
    "recipient. It is NOT a thank-you or acknowledgement (\"Thanks!\", "
    "\"Received\", \"Much appreciated\"), NOT a closing pleasantry that ends a "
    "thread, NOT a congratulation, NOT an informational FYI the sender says needs "
    "no action, and NOT a message the recipient sent themselves. Those get \"\"."
)

# NOTE: SHAPE_TWEAK, NOTIFY_TWEAK and ACK_TWEAK above are now SHIPPED in
# internal/ai/assistant.go, so genprompt.py already includes them in
# CATEGORIZE_SYSTEM. They are kept here only as a record of what was measured --
# appending them again would duplicate them. New candidates go below.

# --- candidate 4: Receipt is the biggest remaining magnet ---------------------
# 6 of the 20 remaining misses land on Receipt: an unpaid "invoice available" and
# a "tax invoice is available" (both Finance), a Russian verification code
# (Security), a coach "order" (Travel), a locker notice and a Dutch service-start
# confirmation (both Notification), and a human "Received, thanks" (no tag).
# NOTIFY_TWEAK told the model what Receipt requires; this tells it what Receipt
# is NOT, which is the framing that actually moved Notification.
RECEIPT_TWEAK = (
    " \"Receipt\" covers only money already spent or goods already in transit to "
    "the recipient. An invoice that is available, issued, due or unpaid is "
    "\"Finance\" -- only an invoice confirmed PAID is a \"Receipt\". A "
    "verification or confirmation code is \"Security\". A ticket, booking or "
    "travel order is \"Travel\". The words received, confirmation or order "
    "appearing in a message written by a person do not by themselves make it a "
    "\"Receipt\"."
)

# --- candidate 5: the model is never told who the user is ---------------------
# The largest remaining bucket is 6 no-action emails tagged "Needs reply", and
# two of them are mail the user SENT. ACK_TWEAK already says own sent mail gets
# "" -- but the model cannot apply that rule, because nothing in the prompt or in
# the "From: ... / Subject: ... / snippet" item says which address is the
# recipient's. This is a missing-context bug, not a wording bug. In production
# aiwork/worker.go knows the account email and could interpolate it here.
BENCH_OWN_ADDRESS = "yauhen@example.com"   # matches the own-sent cases in cases.py
SELF_TWEAK = (
    f" The recipient of every email you classify is {BENCH_OWN_ADDRESS}. An email "
    f"whose From is that same address was written BY the recipient, so nobody is "
    f"waiting on a reply from them: it gets \"\"."
)

PROMPT_VARIANTS = {
    "shipped": CATEGORIZE_SYSTEM,
    "shipped+receipt": CATEGORIZE_SYSTEM + RECEIPT_TWEAK,
    "shipped+self": CATEGORIZE_SYSTEM + SELF_TWEAK,
    "shipped+both": CATEGORIZE_SYSTEM + RECEIPT_TWEAK + SELF_TWEAK,
}

# Reasoning models emit chain-of-thought. Some inline it in content between
# <think> tags; some return it in a side field and leave content empty. Both
# must be handled or the model scores 0 for reasons that have nothing to do with
# its judgement.
THINK_RE = re.compile(r"<\s*(think|thinking|reasoning)\s*>.*?<\s*/\s*\1\s*>",
                      re.S | re.I)
# Generous enough that a reasoning preamble can finish and still reach the
# answer. A bare label needs ~8 tokens; a thinking model can need hundreds.
CAT_MAX_TOKENS = 512


class Target:
    def __init__(self, spec):
        self.spec = spec
        self.empty = 0            # replies whose content was blank
        self.reasoning_only = 0   # ...but which carried chain-of-thought
        if "@" in spec:
            self.model, ep = spec.rsplit("@", 1)
            self.base = ENDPOINT_ALIASES.get(ep, ep)
            self.kind = "lemonade" if self.base == LEMONADE_API else "remote"
        else:
            self.model, self.base, self.kind = spec, LEMONADE_API, "lemonade"
        self.key = keyring_key(self.base) if self.kind == "remote" else None
        self.label = spec

    def __repr__(self):
        return f"<{self.kind} {self.model} @ {self.base}>"


def post(target, messages, max_tokens, temp):
    # Thinking is suppressed on every call, because internal/ai does the same:
    # Options.AllowReasoning defaults to false in Assistant.stream. Measured on
    # Qwen3.5-35B-A3B, one classification costs 772 tokens and 31.5s with
    # thinking and 4 tokens and 0.3s without, for the same answer. An earlier
    # version of this script only suppressed *reactively*, after a reply came
    # back blank -- so a model that thinks and then answers correctly, just 15x
    # slower, was never detected and the cost stayed invisible. Both keys are
    # sent because servers differ in which one they honour, and both are ignored
    # when unknown.
    payload_out = {"model": target.model, "messages": messages,
                   "max_tokens": max_tokens, "temperature": temp,
                   "stream": False,
                   "chat_template_kwargs": {"enable_thinking": False},
                   "reasoning_effort": "none"}
    body = json.dumps(payload_out).encode()
    headers = {"Content-Type": "application/json"}
    if target.key:
        headers["Authorization"] = f"Bearer {target.key}"
    req = urllib.request.Request(f"{target.base}/chat/completions", data=body,
                                headers=headers)
    t = time.perf_counter()
    with urllib.request.urlopen(req, timeout=TIMEOUT) as r:
        payload = json.load(r)
    el = time.perf_counter() - t
    u = payload.get("usage", {}) or {}
    msg = (payload["choices"][0].get("message") or {})
    txt = THINK_RE.sub("", msg.get("content") or "").strip()
    if not txt:
        target.empty += 1
        if (msg.get("reasoning_content") or msg.get("reasoning") or "").strip():
            target.reasoning_only += 1
    return (txt, u.get("prompt_tokens", 0), u.get("completion_tokens", 0), el)


def ask(target, prompt, max_tokens, temp, nonce="", system=None):
    # The nonce goes FIRST. llama.cpp caches by common PREFIX, so a nonce at the
    # end leaves the whole long prompt cached and the prefill measurement reads a
    # cache hit rather than real prompt processing -- that is how one run
    # reported 5756 tok/s against ~340 on a cold cache.
    msgs = ([{"role": "system", "content": system}] if system else []) + \
           [{"role": "user", "content": prompt if not nonce
             else f"<!--{nonce}-->\n{prompt}"}]
    return post(target, msgs, max_tokens, temp)


def remote_models(ep):
    base = ENDPOINT_ALIASES.get(ep, ep)
    key = keyring_key(base)
    req = urllib.request.Request(f"{base}/models")
    if key:
        req.add_header("Authorization", f"Bearer {key}")
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            data = json.load(r)
    except urllib.error.HTTPError as e:
        sys.exit(f"{base}/models -> HTTP {e.code}. "
                 f"{'403 usually means the VPN is not up.' if e.code == 403 else ''}")
    except Exception as e:
        sys.exit(f"{base} unreachable: {type(e).__name__}: {e} (VPN up?)")
    ids = [m.get("id") for m in data.get("data", [])]
    print(f"{len(ids)} models on {base}"
          f"{' (key: present)' if key else ' (no key found)'}")
    for i in ids:
        print(f"  {i}@{ep if ep in ENDPOINT_ALIASES else base}")


# ------------------------------------------------------- lemonade inventory

def lemonade_inventory():
    """-> {name: {"downloaded": bool, "size_gb": float}} from `lemonade list`."""
    inv = {}
    try:
        out = subprocess.run(["lemonade", "list"], capture_output=True,
                             text=True, timeout=120).stdout
    except Exception:
        return inv
    for line in out.splitlines():
        f = line.split()
        if len(f) >= 3 and f[1] in ("Yes", "No"):
            try:
                size = float(f[2])
            except ValueError:
                size = 0.0
            inv[f[0]] = {"downloaded": f[1] == "Yes", "size_gb": size}
    return inv


def backend_warnings():
    """Surface backends that are missing or need an update before use."""
    warn = []
    try:
        out = subprocess.run(["lemonade", "backends"], capture_output=True,
                             text=True, timeout=120).stdout
    except Exception:
        return warn
    for line in out.splitlines():
        if "llamacpp" in line or line.strip().startswith(("vulkan", "rocm", "cpu")):
            if "update_required" in line:
                warn.append(f"backend update required: {' '.join(line.split()[:2])}")
    return warn


def preflight(targets, do_pull):
    """Report/resolve missing local models. Returns the runnable target list."""
    local = [t for t in targets if t.kind == "lemonade"]
    remote = [t for t in targets if t.kind == "remote"]
    runnable = list(remote)

    for t in remote:
        print(f"  remote   {t.model:<34} @ {t.base} "
              f"{'(key present)' if t.key else '(NO KEY -- may 401)'}")

    if not local:
        return runnable
    inv = lemonade_inventory()
    missing = []
    for t in local:
        info = inv.get(t.model)
        if info is None:
            print(f"  local    {t.model:<34} NOT IN REGISTRY -- skipping")
            continue
        if info["downloaded"]:
            print(f"  local    {t.model:<34} ready ({info['size_gb']:.1f} GB)")
            runnable.append(t)
        else:
            missing.append((t, info["size_gb"]))

    if missing:
        total = sum(s for _, s in missing)
        print(f"\n  {len(missing)} model(s) not downloaded, {total:.1f} GB total:")
        for t, s in missing:
            print(f"    {t.model:<36} {s:.1f} GB")
        if not do_pull:
            print("  -> skipped. Re-run with --pull to download them.")
        else:
            for t, s in missing:
                print(f"  pulling {t.model} ({s:.1f} GB) ...", flush=True)
                try:
                    # Bounded on purpose. Registry entries do not all resolve to
                    # the same mirror: a HuggingFace pull ran at ~1100 MB/min
                    # while a ModelScope-hosted one crawled at ~0.5 MB/s, and with
                    # no timeout it blocked an entire 8-model sweep for hours
                    # before a single model was benchmarked. One slow mirror
                    # should cost that model, not the run.
                    r = subprocess.run(["lemonade", "pull", t.model],
                                       capture_output=True, text=True,
                                       timeout=PULL_TIMEOUT)
                except subprocess.TimeoutExpired:
                    print(f"    TIMED OUT after {PULL_TIMEOUT // 60} min -- "
                          f"skipping {t.model} (slow mirror?)")
                    continue
                if r.returncode == 0:
                    print(f"    done: {t.model}")
                    runnable.append(t)
                else:
                    err = (r.stderr or r.stdout).strip().splitlines()
                    print(f"    FAILED: {err[-1] if err else 'unknown error'}")
    for w in backend_warnings():
        print(f"  ! {w}")
    # keep the caller's ordering
    order = {t.spec: i for i, t in enumerate(targets)}
    return sorted(runnable, key=lambda t: order[t.spec])


# ------------------------------------------------------------- real memory
#
# File size is a bad proxy for footprint: it ignores the KV cache, which is what
# actually bites on a long thread, and RAM is a selection axis in its own right on
# a laptop. The iGPU has no dedicated VRAM to speak of (512 MiB carve-out), so
# weights land in GTT — system memory mapped for the GPU — and both counters have
# to be summed.

def _gpu_bytes():
    total, found = 0, False
    for card in sorted(glob.glob("/sys/class/drm/card*/device")):
        for name in ("mem_info_vram_used", "mem_info_gtt_used"):
            try:
                with open(os.path.join(card, name)) as fh:
                    total += int(fh.read().strip())
                found = True
            except (OSError, ValueError):
                pass
    return total if found else None


def gpu_gb():
    b = _gpu_bytes()
    return round(b / (1 << 30), 2) if b is not None else None


def unload_all():
    """Clean slate so a model's own footprint can be attributed to it."""
    subprocess.run(["lemonade", "unload"], capture_output=True, text=True,
                   timeout=TIMEOUT)


def load_local(model):
    subprocess.run(["lemonade", "load", model], capture_output=True,
                   text=True, timeout=TIMEOUT)


# --------------------------------------------------------- time to first token
#
# Perceived speed in a UI is TTFT, not throughput: a draft that starts streaming
# in 0.4s reads as instant at 14 tok/s, while one that starts at 3s reads as
# broken at 25 tok/s. It needs a streaming request, which the scoring path
# deliberately does not use, so it is measured separately.

def measure_ttft(target, prompt, max_tokens=120):
    payload = {"model": target.model, "stream": True, "max_tokens": max_tokens,
               "temperature": 0.3,
               "messages": [{"role": "user", "content": prompt}]}
    payload["reasoning_effort"] = "none"
    payload["chat_template_kwargs"] = {"enable_thinking": False}
    headers = {"Content-Type": "application/json"}
    if target.key:
        headers["Authorization"] = f"Bearer {target.key}"
    req = urllib.request.Request(f"{target.base}/chat/completions",
                                 data=json.dumps(payload).encode(),
                                 headers=headers)
    t0 = time.perf_counter()
    try:
        with urllib.request.urlopen(req, timeout=TIMEOUT) as r:
            for raw in r:
                line = raw.decode("utf-8", "replace").strip()
                if not line.startswith("data:"):
                    continue
                data = line[5:].strip()
                if data == "[DONE]":
                    break
                try:
                    d = json.loads(data)
                except json.JSONDecodeError:
                    continue
                ch = (d.get("choices") or [{}])[0]
                # Only real content counts. A reasoning model streaming
                # chain-of-thought leaves content empty, and waiting for the
                # visible answer is exactly what the user experiences.
                if ((ch.get("delta") or {}).get("content") or "").strip():
                    return round(time.perf_counter() - t0, 3)
    except Exception:
        return None
    return None


def loaded_model():
    try:
        out = subprocess.run(["lemonade", "status"], capture_output=True,
                             text=True, timeout=30).stdout
        for line in out.splitlines():
            if "llamacpp" in line and line.strip():
                return line.split()[0]
    except Exception:
        pass
    return None


# ------------------------------------------------------- production parity

def match_category(s):
    """Port of ai.MatchCategory (internal/ai/assistant.go)."""
    s = s.strip().strip("\"'`.,:;!").strip()
    if not s:
        return ""
    for c in EMAIL_CATEGORIES:
        if s.lower() == c.lower():
            return c
    ls, found = s.lower(), ""
    for c in EMAIL_CATEGORIES:
        if c.lower() in ls:
            if found:
                return ""          # two named -> ambiguous -> no tag
            found = c
    if found:
        return found
    return {"marketing": "Newsletter", "promotion": "Newsletter",
            "promotions": "Newsletter", "promo": "Newsletter",
            "invitation": "Calendar", "invite": "Calendar",
            "meeting": "Calendar", "alert": "Notification",
            "alerts": "Notification"}.get(ls, "")


def _first_json_array(raw):
    start = raw.find("[")
    if start < 0:
        return ""
    depth, in_str, esc = 0, False, False
    for i in range(start, len(raw)):
        ch = raw[i]
        if in_str:
            if esc:
                esc = False
            elif ch == "\\":
                esc = True
            elif ch == '"':
                in_str = False
            continue
        if ch == '"':
            in_str = True
        elif ch == "[":
            depth += 1
        elif ch == "]":
            depth -= 1
            if depth == 0:
                return raw[start:i + 1]
    return ""


def parse_one(raw):
    """Port of ai.parseCategories for the n=1 call mailbox issues.
    -> (stored_label, mode) with mode = json / nested / scalar / fail."""
    arr = _first_json_array(raw)
    if arr:
        try:
            v = json.loads(arr)
            if isinstance(v, list) and not v:
                # well-formed but ZERO elements: mailbox stores no tag. Tracked
                # separately because it is a shape failure, not a judgement.
                return "", "empty_array"
            if isinstance(v, list) and v:
                if isinstance(v[0], str):
                    return match_category(v[0]), "json"
                if isinstance(v[0], list) and v[0] and isinstance(v[0][0], str):
                    return match_category(v[0][0]), "nested"
        except json.JSONDecodeError:
            pass
    stripped = raw.strip().strip("`").strip()
    if stripped:
        return match_category(stripped), "scalar"
    return "", "fail"


# ---------------------------------------------------------- translation set

TRANS_CASES = [
    {"lang": "Dutch",
     "text": """Beste Yauhen,

Hartelijk dank voor je bericht van afgelopen dinsdag. Wij hebben je voorstel
intern besproken en zijn enthousiast, maar er zijn twee openstaande punten.

Ten eerste vragen wij ons af of de voorgestelde levertermijn van zes weken
haalbaar is, gezien de vakantieperiode in augustus. Ten tweede willen we meer
inzicht in de kosten voor onderhoud na oplevering, aangezien dit niet in de
begroting van EUR 12.500 is opgenomen.

Zou je volgende week donderdag om 14:00 tijd hebben voor een videogesprek?

Met vriendelijke groet,
Elena Novak""",
     "facts": [["six weeks", "6 weeks"], ["august"],
               ["12,500", "12.500", "12500"], ["thursday"],
               # Must match the signature in the email body above. When these
               # drifted apart the fact became unfindable and every model lost
               # exactly one point, pinning translation at 92.3% for all of them
               # and silently destroying the metric's ability to discriminate.
               ["14:00", "2 pm", "2:00 pm", "2pm"], ["elena novak"]],
     "leak": ["hartelijk", "vriendelijke", "levertermijn", "begroting",
              "aangezien", "videogesprek", "openstaande"]},
    {"lang": "German",
     "text": """Guten Tag Herr Janssen,

vielen Dank für Ihre Anfrage. Leider müssen wir den Termin am 3. September
verschieben, da unser Techniker erkrankt ist. Wir schlagen stattdessen Montag,
den 8. September um 11:30 Uhr vor.

Die Kosten belaufen sich unverändert auf 4.800 EUR netto. Bitte bestätigen Sie
den neuen Termin bis Freitag.

Mit freundlichen Grüßen
Andreas Keller""",
     "facts": [["3 september", "september 3", "3rd of september"],
               ["8 september", "september 8", "8th of september"],
               ["11:30"], ["4,800", "4.800", "4800"], ["friday"],
               ["andreas keller"], ["monday"]],
     "leak": ["vielen dank", "freundlichen", "grüßen", "techniker",
              "unverändert", "bestätigen", "stattdessen", "erkrankt"]},
]

SUM_THREAD = """From: elena@client.example  (Tue 5 Aug, 09:12)
Subject: Co-browsing widget rollout

Hi Yauhen, our support team started using the widget on Monday. Two issues:
session handoff drops when an agent refreshes the console mid-session, and the
widget does not load at all in Safari 17. Volume is about 40 sessions a day so
this is blocking our full rollout.

From: you@example.com  (Tue 5 Aug, 14:40)

Thanks Elena. The refresh issue is a stale websocket token that isn't renewed
on reconnect -- we have a fix in the session broker, targeting Thursday. Safari
17 is new to us, can you send a console log?

From: elena@client.example  (Wed 6 Aug, 08:05)

Log attached. Also our security team wants confirmation that session recordings
are stored in the EU before we expand beyond the pilot. Can you confirm the
region, and whether the Thursday fix covers Safari too?

From: you@example.com  (Wed 6 Aug, 16:30)

Recordings are in eu-west-1, I'll send the DPA appendix. The Thursday fix is
the websocket token only; Safari 17 needs a separate patch, realistically early
next week. I'd suggest keeping the pilot at 40 sessions until both land."""

SUM_GROUND_TRUTH = (
    "Two defects: (1) session handoff drops on mid-session console refresh, cause "
    "= stale websocket token not renewed on reconnect; (2) widget does not load in "
    "Safari 17. Websocket fix targets Thursday; Safari 17 needs a separate patch, "
    "early next week. Client security team needs confirmation recordings are stored "
    "in the EU -- they are in eu-west-1, DPA appendix to follow. Pilot stays at ~40 "
    "sessions/day until both fixes land. Anything else is invented.")

SUM_PROMPT = ("Summarize this email thread in exactly three bullet points. Be "
              "factual and specific. Output only the bullets.\n\n" + SUM_THREAD)

DRAFT_PROMPT = """Write a reply to this email. Be professional and concise.
Confirm we can do the earlier date, ask about parking, and mention the invoice
will follow separately. Output only the email body, no subject line.

--- original message ---
From: Elena Novak <elena@client.example>

Hi Yauhen,

Could we move the onsite workshop from 18 September to 11 September? Our
training room is only free that week. We can host up to 12 people and lunch is
included. Let me know if that works.

Best,
Elena"""

DRAFT_REQUIREMENTS = (
    "Must do exactly three things: (a) confirm 11 September works, (b) ask about "
    "parking, (c) say the invoice will follow separately. Must read as an email "
    "body: no subject line, no meta-preamble like 'Here is a draft'. Must not "
    "invent specifics (prices, times, attendee names) that were not given.")


# -------------------------------------------------------------- measurement

def measure_speed(target, nonce):
    prefill = gen = None
    try:
        _, ptok, _, el = ask(target, "Reply with the single word OK.\n\n"
                             + SUM_THREAD * 6, 1, 0.1, nonce + "-pre")
        prefill = round(ptok / el, 1) if el > 0 else None
    except Exception:
        pass
    try:
        _, ptok, ctok, el = ask(
            target, "Write a detailed paragraph explaining how memory bandwidth "
                    "limits LLM inference speed on integrated GPUs.", 150, 0.7,
            nonce + "-gen")
        gen = round(ctok / max(el - (ptok / prefill if prefill else 0), 1e-6), 1)
    except Exception:
        pass
    return prefill, gen


def run_categories(target, nonce, cases, system=None):
    system = system or CATEGORIZE_SYSTEM
    per_cat = defaultdict(lambda: {"n": 0, "hit": 0})
    per_diff = defaultdict(lambda: {"n": 0, "hit": 0})
    per_lang = defaultdict(lambda: {"n": 0, "hit": 0})
    modes, misses, secs = Counter(), [], []
    stored_by_id = {}          # for the consistency pass
    hits = 0
    empty0, reas0 = target.empty, target.reasoning_only
    for k, c in enumerate(cases):
        try:
            # NO nonce here: the prompt promises the user message IS a JSON
            # array, and appending a cache-busting comment after it made the
            # payload malformed -- which is itself enough to make a small model
            # answer with a degenerate []. Cache-busting is unnecessary anyway,
            # since every one of the cases is a different string.
            raw, _, _, el = ask(target, json.dumps([c["item"]]), CAT_MAX_TOKENS,
                                0.0, system=system)
        except Exception as e:
            misses.append({"id": c["id"], "expected": c["label"],
                           "stored": f"ERROR {type(e).__name__}", "raw": ""})
            per_cat[c["label"]]["n"] += 1
            per_diff[c["difficulty"]]["n"] += 1
            continue
        secs.append(el)
        stored, mode = parse_one(raw)
        modes[mode] += 1
        ok = stored == c["label"]
        hits += ok
        stored_by_id[c["id"]] = stored
        per_cat[c["label"]]["n"] += 1
        per_cat[c["label"]]["hit"] += ok
        per_diff[c["difficulty"]]["n"] += 1
        per_diff[c["difficulty"]]["hit"] += ok
        lang = c.get("lang", "en")
        per_lang[lang]["n"] += 1
        per_lang[lang]["hit"] += ok
        if not ok:
            misses.append({"id": c["id"], "expected": c["label"],
                           "stored": stored, "raw": raw.strip()[:160],
                           "difficulty": c["difficulty"],
                           "probe": c.get("probe"), "item": c["item"][:130]})
        if (k + 1) % 40 == 0:
            print(f"        {k+1}/{len(cases)} ...", flush=True)

    n = len(cases)
    recall = {k: (v["hit"] / v["n"] if v["n"] else 0.0) for k, v in per_cat.items()}
    tw = sum(REAL_FREQ.get(k, 0) for k in recall)
    weighted = (sum(REAL_FREQ.get(k, 0) * r for k, r in recall.items()) / tw
                if tw else 0.0)
    # A model that returns blank content is not a model that answered wrongly.
    # Report the run as INVALID rather than publishing a meaningless score --
    # otherwise a truncated reasoning model looks like a terrible classifier.
    blank = target.empty - empty0
    invalid = None
    if n and blank / n > 0.2:
        invalid = (f"{blank}/{n} replies had blank content"
                   + (f" ({target.reasoning_only - reas0} carried chain-of-thought "
                      f"instead) -- reasoning model: raise CAT_MAX_TOKENS or "
                      f"disable thinking" if target.reasoning_only > reas0
                      else " -- model returned nothing; scores below are not "
                           "about its judgement"))
    return {
        "n": n,
        "INVALID": invalid,
        "blank_replies": blank,
        "empty_array_replies": modes.get("empty_array", 0),
        "accuracy_pct": round(100.0 * hits / n, 1) if n else 0.0,
        "weighted_accuracy_pct": round(100.0 * weighted, 1),
        "needs_reply_recall_pct": round(100.0 * recall.get("Needs reply", 0.0), 1),
        "per_category": {k: {"n": v["n"], "correct": v["hit"],
                             "recall_pct": round(100.0 * recall[k], 1)}
                         for k, v in sorted(per_cat.items())},
        "per_difficulty": {k: {"n": v["n"], "correct": v["hit"],
                               "accuracy_pct": round(100.0 * v["hit"] / v["n"], 1)
                               if v["n"] else 0.0}
                           for k, v in sorted(per_diff.items())},
        "per_language": {k: {"n": v["n"], "correct": v["hit"],
                             "accuracy_pct": round(100.0 * v["hit"] / v["n"], 1)
                             if v["n"] else 0.0}
                         for k, v in sorted(per_lang.items())},
        "stored_by_id": stored_by_id,
        "parse_modes": dict(modes),
        "median_secs": round(sorted(secs)[len(secs) // 2], 2) if secs else None,
        "misses": misses,
    }


def run_consistency(target, cases, system, first_pass, n=48):
    """
    Re-run a subset and count answers that changed. This matters more than it
    sounds: mailbox CACHES a category per message, so a run-to-run flip is not
    averaged away — a wrong tag sticks until someone runs "Re-categorize inbox".
    Temperature is already 0; llama.cpp is still not bit-deterministic, so the
    question is how much each model wobbles.
    """
    sub = cases[:n]
    flips = []
    for c in sub:
        try:
            raw, _, _, _ = ask(target, json.dumps([c["item"]]), CAT_MAX_TOKENS,
                               0.0, system=system)
        except Exception:
            continue
        stored, _ = parse_one(raw)
        was = first_pass.get(c["id"])
        if was is not None and stored != was:
            flips.append({"id": c["id"], "first": was, "second": stored,
                          "expected": c["label"]})
    return {"n": len(sub), "flips": len(flips),
            "flip_pct": round(100.0 * len(flips) / len(sub), 1) if sub else 0.0,
            "examples": flips[:6]}


def run_translation(target, nonce):
    out, kept, total, leaks = [], 0, 0, 0
    for case in TRANS_CASES:
        p = (f"Translate this {case['lang']} email into natural English. Output "
             f"only the translation.\n\n{case['text']}")
        try:
            txt, _, _, el = ask(target, p, 600, 0.2, f"{nonce}-tr{case['lang']}")
        except Exception as e:
            out.append({"lang": case["lang"], "text": f"ERROR {e}"})
            total += len(case["facts"])
            continue
        low, missing = txt.lower(), []
        for forms in case["facts"]:
            total += 1
            if any(f in low for f in forms):
                kept += 1
            else:
                missing.append(forms[0])
        leaked = [w for w in case["leak"] if w in low]
        leaks += len(leaked)
        out.append({"lang": case["lang"], "secs": round(el, 1), "text": txt,
                    "missing_facts": missing, "leaked_words": leaked})
    return {"facts_kept_pct": round(100.0 * kept / total, 1) if total else 0.0,
            "leaked_word_count": leaks, "outputs": out}


def run_generative(target, nonce, prompt, mx, temp, reps, tag):
    outs = []
    for r in range(reps):
        try:
            txt, _, ctok, el = ask(target, prompt, mx, temp, f"{nonce}-{tag}{r}")
            outs.append({"rep": r, "secs": round(el, 1), "tokens": ctok,
                         "text": txt})
        except Exception as e:
            outs.append({"rep": r, "error": f"{type(e).__name__}: {e}"})
    return outs


def bench(target, idx, args, cases):
    nonce = f"r{idx}-{int(time.time())}"
    rec = {"model": target.model, "target": target.spec, "kind": target.kind,
           "endpoint": target.base}
    base_mem = None
    if target.kind == "lemonade":
        unload_all()            # attribute the footprint to this model alone
        base_mem = gpu_gb()
    t0 = time.perf_counter()
    if target.kind == "lemonade":
        load_local(target.model)
    try:
        ask(target, "hi", 5, 0.1, nonce + "-warm")
    except Exception as e:
        rec["error"] = f"unreachable: {type(e).__name__}: {e}"
        print(f"      SKIP: {rec['error']}", flush=True)
        return rec
    rec["load_s"] = round(time.perf_counter() - t0, 1)
    resident = gpu_gb() if base_mem is not None else None

    pre, gen = measure_speed(target, nonce)
    rec["speed"] = {"prefill_tok_s": pre, "gen_tok_s": gen}
    note = " (incl. network)" if target.kind == "remote" else ""
    print(f"      speed     prefill={pre or 0:.0f}  gen={gen or 0:.1f} tok/s{note}",
          flush=True)

    # Read again after measure_speed pushed ~2.5k tokens through. Note llama.cpp
    # PRE-ALLOCATES the KV cache at load time from the context size, so the
    # at-load figure already includes it and this second read usually matches --
    # which is why the reported peak is a max(), not a difference. GTT is also
    # shared with the desktop compositor and any browser, so treat these as ±few
    # hundred MB, and as an upper bound on what the model needs.
    after = gpu_gb() if base_mem is not None else None
    at_load = (round(resident - base_mem, 2)
               if (resident is not None and base_mem is not None) else None)
    peak = (round(max(resident or 0, after or 0) - base_mem, 2)
            if base_mem is not None else None)
    rec["memory_gb"] = {
        "baseline_gb": base_mem, "at_load_gb": at_load, "peak_gb": peak,
        "file_gb": None,   # filled from the registry by preflight callers
        "note": "GTT+VRAM delta over an unloaded baseline; llama.cpp preallocates "
                "the KV cache at load, and GTT is shared with the desktop",
    }
    if peak is not None:
        print(f"      memory    {at_load} GB at load, {peak} GB peak "
              f"(weights + preallocated context)", flush=True)

    rec["ttft_s"] = measure_ttft(target, DRAFT_PROMPT)
    print(f"      ttft      {rec['ttft_s']}s to first visible token", flush=True)

    variants = PROMPT_VARIANTS if args["ab"] else {"shipped": CATEGORIZE_SYSTEM}
    baseline = next(iter(variants))          # first entry is the reference
    for vname, vsystem in variants.items():
        r = run_categories(target, nonce, cases, vsystem)
        if len(variants) > 1:
            rec.setdefault("categorisation_variants", {})[vname] = r
        if vname == baseline:
            rec["categorisation"] = r
        if r.get("INVALID"):
            print(f"      !! INVALID [{vname}]: {r['INVALID']}", flush=True)
        print(f"      cat[{vname:<10}] {r['accuracy_pct']}% raw, "
              f"{r['weighted_accuracy_pct']}% weighted, "
              f"reply={r['needs_reply_recall_pct']}%, "
              f"blank/[]={r['blank_replies']}+{r['empty_array_replies']}",
              flush=True)
    if args["consistency"]:
        rec["consistency"] = run_consistency(
            target, cases, variants[baseline],
            rec["categorisation"].get("stored_by_id", {}))
        cy = rec["consistency"]
        print(f"      consistency {cy['flips']}/{cy['n']} answers changed on a "
              f"repeat pass ({cy['flip_pct']}%)", flush=True)

    if args["ab"]:
        v = rec["categorisation_variants"]
        base = v.get(baseline)
        if base:
            print(f"\n      {'variant':<14}{'raw%':>7}{'wtd%':>7}{'Δwtd':>7}"
                  f"{'notif%':>8}{'reply-P':>9}{'notag%':>8}{'[]':>4}")
            for name, r in v.items():
                pc = r.get("per_category", {})
                fp = sum(1 for m in r.get("misses", [])
                         if m["stored"] == "Needs reply")
                tp = pc.get("Needs reply", {}).get("correct", 0)
                prec = 100 * tp / (tp + fp) if tp + fp else 0
                print(f"      {name:<14}{r['accuracy_pct']:>7}"
                      f"{r['weighted_accuracy_pct']:>7}"
                      f"{r['weighted_accuracy_pct'] - base['weighted_accuracy_pct']:>+7.1f}"
                      f"{pc.get('Notification', {}).get('recall_pct', 0):>8}"
                      f"{prec:>8.1f}%"
                      f"{pc.get('', {}).get('recall_pct', 0):>8}"
                      f"{r['empty_array_replies']:>4}", flush=True)
            best = max(v.items(), key=lambda kv: kv[1]["weighted_accuracy_pct"])
            print(f"      best weighted: {best[0]} "
                  f"({best[1]['weighted_accuracy_pct']}%)\n", flush=True)
    c = rec["categorisation"]

    if not args["quick"]:
        rec["translation"] = run_translation(target, nonce)
        t = rec["translation"]
        print(f"      translate {t['facts_kept_pct']}% facts, "
              f"{t['leaked_word_count']} leaks", flush=True)
        rec["summaries"] = run_generative(target, nonce, SUM_PROMPT, 300, 0.3,
                                          args["reps"], "sum")
        rec["drafts"] = run_generative(target, nonce, DRAFT_PROMPT, 400, 0.4,
                                       args["reps"], "dr")
        print(f"      generated {len(rec['summaries'])} summaries + "
              f"{len(rec['drafts'])} drafts for Claude", flush=True)
    rec["thinking_disabled"] = True   # always, mirroring internal/ai
    return rec


# ---------------------------------------------------------------- selftest

def selftest():
    m = loaded_model()
    if not m:
        sys.exit("could not determine the loaded model; is the server running?")
    t = Target(m)
    cases, meta = load_cases()
    print(f"loaded model : {m}")
    print(f"ground truth : {meta}")
    print(f"prompt       : {len(CATEGORIZE_SYSTEM)} chars, "
          f"{len(EMAIL_CATEGORIES)} categories\n")
    n = f"self-{int(time.time())}"
    sub, hits = cases[:8], 0
    for i, c in enumerate(sub):
        raw, _, _, el = ask(t, json.dumps([c["item"]]), CAT_MAX_TOKENS, 0.0,
                            system=CATEGORIZE_SYSTEM)
        stored, mode = parse_one(raw)
        ok = stored == c["label"]
        hits += ok
        print(f"  [{'ok ' if ok else 'MISS'}] want={c['label']:<13} "
              f"got={stored or '(none)':<13} mode={mode:<7} {el:.1f}s  "
              f"raw={raw.strip()[:44]!r}")
    print(f"\n{hits}/{len(sub)} on a sample. Plumbing fine; nothing evicted.")


# ------------------------------------------------------------------ artifact

def write_artifact(rows, args, meta):
    doc = {
        "purpose": "Claude grades summaries + drafts by reading this; the script "
                   "already scored speed, categorisation and translation.",
        "hardware": "AMD Ryzen AI 9 HX PRO 370, Radeon 890M iGPU, 61 GB RAM",
        "fidelity": {
            "prompt_source": "internal/ai/assistant.go via genprompt.py",
            "prompt_chars": len(CATEGORIZE_SYSTEM),
            "protocol": "JSON array in/out, temperature 0, one email per call",
            "parsing": "Python port of parseCategories + MatchCategory",
            "cat_max_tokens": CAT_MAX_TOKENS,
            "ground_truth": meta,
            "prompt_variants_tested": sorted(
                rows[0].get("categorisation_variants", {}).keys()) if rows else [],
            "shape_tweak": SHAPE_TWEAK,
            "note": "remote targets' tok/s include network latency; local "
                    "targets are bounded by iGPU memory bandwidth",
        },
        "real_inbox_distribution": REAL_FREQ,
        "grading_notes": {
            "summary_ground_truth": SUM_GROUND_TRUTH,
            "summary_asked_for": "exactly three factual bullets, no preamble",
            "draft_requirements": DRAFT_REQUIREMENTS,
        },
        "reps": args["reps"],
        "models": rows,
    }
    with open(ARTIFACT, "w", encoding="utf-8") as fh:
        json.dump(doc, fh, indent=2, ensure_ascii=False)

    # prefill tps belongs here: summaries and translation are input-bound, so a
    # model can have better gen tps than the baseline and still be slower at the
    # two features the user actually waits for.
    hdr = (f"{'model':<34}{'RAM':>6}{'pre':>6}{'gen':>6}{'ttft':>6}{'raw%':>7}"
           f"{'wtd%':>7}{'rep-R':>7}{'rep-P':>7}{'hard%':>7}{'trans%':>7}"
           f"{'flip%':>6}")
    print("\n" + hdr + "\n" + "-" * len(hdr))
    base = None
    for r in rows:
        if r.get("error"):
            print(f"{r.get('target','?'):<32}  {r['error'][:70]}")
            continue
        s, c = r.get("speed", {}), r.get("categorisation", {})
        t, d = r.get("translation", {}), c.get("per_difficulty", {})
        m, cy = r.get("memory_gb", {}), r.get("consistency", {})
        fp = sum(1 for x in c.get("misses", []) if x["stored"] == "Needs reply")
        tp = c.get("per_category", {}).get("Needs reply", {}).get("correct", 0)
        prec = 100.0 * tp / (tp + fp) if tp + fp else 0.0
        name = r["target"] + ("  (baseline)" if r["target"] == BASELINE else "")
        print(f"{name:<34}{m.get('peak_gb') or 0:>6.1f}"
              f"{s.get('prefill_tok_s') or 0:>6.0f}"
              f"{s.get('gen_tok_s') or 0:>6.1f}{r.get('ttft_s') or 0:>6.2f}"
              f"{c.get('accuracy_pct',0):>7}{c.get('weighted_accuracy_pct',0):>7}"
              f"{c.get('needs_reply_recall_pct',0):>7}{prec:>7.1f}"
              f"{d.get('hard',{}).get('accuracy_pct',0):>7}"
              f"{t.get('facts_kept_pct',0):>7}"
              f"{cy.get('flip_pct', 0):>6}")
        if base is None:
            base = r
    # Ratios against the first model, which is the baseline by convention.
    if base and len(rows) > 1:
        bw = base.get("categorisation", {}).get("weighted_accuracy_pct") or 1
        bg = base.get("speed", {}).get("gen_tok_s") or 1
        bm = base.get("memory_gb", {}).get("peak_gb") or 1
        print(f"\nvs baseline {base['target']}:")
        for r in rows[1:]:
            if r.get("error"):
                continue
            w = r.get("categorisation", {}).get("weighted_accuracy_pct") or 0
            g = r.get("speed", {}).get("gen_tok_s") or 0
            mm = r.get("memory_gb", {}).get("peak_gb") or 0
            per_gb = (w - bw) / (mm - bm) if abs(mm - bm) >= 1.0 else None
            print(f"  {r['target']:<32} quality {w - bw:+5.1f}pp   "
                  f"speed {g / bg:4.2f}x   RAM {mm / bm:4.2f}x"
                  + (f"   {per_gb:+.2f}pp per extra GB" if per_gb else ""))
    ran = rows[0].get("categorisation", {}).get("n", meta["scored"]) if rows else meta["scored"]
    if ran < meta["scored"]:
        print(f"\n  NOTE: capped at {ran} of {meta['scored']} cases. The sample "
              f"covers every category, but\n  wtd% is computed from very few "
              f"Notification cases and will swing. Use a full run\n  before "
              f"trusting a comparison.")
    print(f"""
what the columns mean ({ran} labelled emails)

  RAM     GB resident while the model is loaded — weights plus the KV cache
          llama.cpp preallocates. This is NOT the download size; expect 1.3-2.3x.
  pre     prefill tok/s: how fast it READS. Drives thread summaries and
          translation, which are long-input, short-output.
  gen     generation tok/s: how fast it WRITES. Drives draft replies.
  ttft    seconds to the first visible token. This is what "feels" fast when a
          draft streams into the compose window; throughput is not.

  raw%    accuracy over all cases, each counted once.
  wtd%    accuracy weighted by how often each category actually arrives (~87%
          Notification). Closer to what you experience — but read it WITH raw%:
          a model answering "Notification" to everything scores ~87% here and
          ~17% raw.
  rep-R   "Needs reply" recall: of the mail that needs an answer, how much it
          catches.
  rep-P   "Needs reply" precision: of the mail it tags, how much really needs an
          answer. A model can hold 100% recall while a third of its badges are
          false, so this is the one that decides whether you trust the tag.
  hard%   accuracy on cases that probe a specific boundary rule — the ones weak
          models fail first.
  trans%  key facts (dates, amounts, names) surviving a Dutch and a German
          translation.
  flip%   answers that changed on a repeat pass. Categories are cached per
          message, so a flip sticks until you re-categorise.
""")
    print(f"artifact -> {ARTIFACT} ({os.path.getsize(ARTIFACT)/1024:.0f} KB)")
    print("Give Claude artifact.json to grade the summaries and drafts.")


def main():
    argv = sys.argv[1:]
    if "--list" in argv:
        subprocess.run(["lemonade", "list", "--downloaded"])
        return
    if "--remote-models" in argv:
        i = argv.index("--remote-models")
        remote_models(argv[i + 1] if i + 1 < len(argv) else "proteus")
        return
    if "--selftest" in argv:
        selftest()
        return

    def flagval(name, default):
        return argv[argv.index(name) + 1] if name in argv and \
            argv.index(name) + 1 < len(argv) else default

    args = {"quick": "--quick" in argv, "ab": "--ab" in argv,
            "consistency": "--consistency" in argv,
            "reps": int(flagval("--reps", 2))}
    catlimit = flagval("--cat-limit", None)
    consumed = {flagval("--reps", None), catlimit}
    specs = [a for a in argv if not a.startswith("--") and a not in consumed] \
        or DEFAULT_TARGETS
    # Naming a model means "compare this with what I run today", so the baseline
    # goes in front unless it is already there or explicitly waived.
    if "--solo" not in argv and BASELINE not in specs:
        specs = [BASELINE] + specs
    targets = [Target(s) for s in specs]

    cases, meta = load_cases()
    if catlimit:
        cases = sample_across_classes(cases, int(catlimit))

    print(f"category set : {len(cases)} emails, {meta['source']}")
    print(f"prompt       : production, {len(CATEGORIZE_SYSTEM)} chars")
    print("preflight:")
    targets = preflight(targets, "--pull" in argv)
    if not targets:
        sys.exit("\nnothing runnable -- see preflight above")

    need_restore = any(t.kind == "lemonade" for t in targets)
    original = loaded_model() if need_restore else None
    if original:
        print(f"\nloaded now   : {original} (will be restored)")
    print()

    rows = []
    try:
        for i, t in enumerate(targets):
            print(f"[{i+1}/{len(targets)}] {t.spec}  [{t.kind}]", flush=True)
            rows.append(bench(t, i, args, cases))
    except KeyboardInterrupt:
        print("\ninterrupted -- writing partial artifact")
    finally:
        if original:
            print(f"\nrestoring {original} ...")
            load_local(original)
    write_artifact(rows, args, meta)


if __name__ == "__main__":
    main()
