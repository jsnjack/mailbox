# Benchmarking a local model

Mailbox's AI features — inbox tags, thread summaries, draft replies, translation
— are only as good as the model behind them. This benchmark tells you whether a
model you're considering is actually better than the one you're running, and
what it costs in memory and speed.

It runs entirely on your machine against [lemonade](https://lemonade-server.ai)
or any OpenAI-compatible endpoint. Nothing leaves it.

## Try a model in one command

```bash
cd bench
python3 genprompt.py ..            # once, and after any prompt change
python3 bench_models.py Qwen3.5-9B-GGUF
```

Name a model and it is compared against the baseline automatically — the
baseline runs first, and the difference is reported for you. That takes about
ten minutes for two models.

In a hurry? `--quick` skips summaries, drafts and translation. `--cat-limit 60`
cuts the tagging test to 60 emails spread across all ten categories.

Missing models are listed with their download size and skipped; add `--pull` to
fetch them.

## What you get

```
model                                RAM   pre   gen  ttft   raw%   wtd%  rep-R  rep-P  hard%
Gemma-4-E4B-it-GGUF  (baseline)      8.6   540  25.0  0.45   87.1   86.0  100.0   73.7   72.1
Qwen3.5-9B-GGUF                     15.5   372  14.8  0.65   89.7   92.4   96.4   77.1   78.0

vs baseline Gemma-4-E4B-it-GGUF:
  Qwen3.5-9B-GGUF     quality  +6.4pp   speed 0.59x   RAM 1.80x   +0.93pp per extra GB
```

The script prints a full explanation of every column underneath the table, so
you don't have to keep this page open. The short version:

| Column | Meaning |
| --- | --- |
| `RAM` | GB actually resident, weights **plus** the KV cache. Not the download size — expect 1.3–2.3× that. |
| `pre` | Prefill tok/s: how fast it **reads**. Drives summaries and translation. |
| `gen` | Generation tok/s: how fast it **writes**. Drives draft replies. |
| `ttft` | Seconds to the first visible token — what "feels" fast when a draft streams in. |
| `raw%` | Tagging accuracy, every test email counted once. |
| `wtd%` | Accuracy weighted by how often each category really arrives. |
| `rep-R` | "Needs reply" recall — how much of what needs an answer it catches. |
| `rep-P` | "Needs reply" precision — how much of what it flags really needs one. |
| `hard%` | Accuracy on the deliberately tricky cases. |

**Read `raw%` and `wtd%` together.** About 87% of real mail is automated
notifications, so a model that answered "Notification" to everything would score
~87% weighted and ~17% raw. Neither number alone is honest.

**`rep-P` is the one that decides whether you trust the tags.** A model can catch
every email that needs a reply and still be annoying, if a third of the badges it
shows are wrong.

## The current baseline

**Gemma-4-E4B** — 5.6 GB on disk, 8.6 GB resident. It is what mailbox's local
chain entry is set to, and every run compares against it.

It was chosen because it beat the previous default (granite-4.1-8b) on quality
while using **2.4 GB less memory** and reading 1.6× faster, with the same time to
first token. If you change the model in your config, change `BASELINE` at the top
of `bench/bench_models.py` to match.

## What we found

Seven models, 272 labelled emails, on a Ryzen AI 9 HX PRO 370 with a Radeon 890M:

| Model | RAM | wtd% | rep-P | gen | ttft |
| --- | --- | --- | --- | --- | --- |
| **Gemma-4-12B-MTP** | 11.3 | **96.2** | **96.2%** | 17.5 | 1.04 |
| Qwen3.5-9B | 15.5 | 92.4 | 77.1% | 14.8 | 0.65 |
| Qwen3.5-35B-A3B | 27.7 | 92.1 | 86.7% | 24.2 | 0.71 |
| **Gemma-4-E4B** *(baseline)* | **8.6** | 87.7 | 73.7% | **25.9** | 0.39 |
| granite-4.1-8b *(previous default)* | 11.0 | 83.2 | 63.6% | 14.7 | 0.38 |
| Qwen3-8B | 10.7 | 69.0 | 65.9% | 18.2 | 0.44 |
| LFM2-8B-A1B | 6.3 | 33.7 | 48.8% | 78.0 | 0.28 |

Four things worth knowing before you shop for a model:

**Bigger is not better.** Qwen3.5-35B needs 27.7 GB resident and still loses to a
12B model using 11.3 GB. Past a point you are paying memory for nothing.

**Download size is not memory.** llama.cpp preallocates the KV cache, so a 5.8 GB
file occupies 11 GB. A "fits in 10 GB" rule based on file sizes would have let
through a model needing 15.5 GB.

**The generation matters more than the parameter count.** Qwen3-8B scores 69%;
Qwen3.5-9B, same family and size class, scores 92%.

**Fast can be useless.** LFM2-8B-A1B wins every speed measurement and the memory
one, then gets a third of the tags right — and it is the only model whose answers
changed between identical runs.

## How it works, briefly

The tagging test is the app's own behaviour, not an approximation of it.
`genprompt.py` lifts the real prompt out of `internal/ai/assistant.go`, the
request is the same JSON array at temperature 0 that the background categoriser
sends, and replies are parsed by a port of mailbox's own tolerant parser — so a
model is scored on what mailbox would *actually store*, forgiving replies
included.

It is graded against 272 synthetic emails in `bench/cases.py`, covering all ten
categories, with 64 of them written to probe a specific boundary: an "invoice"
that has already been paid (Receipt, not Finance), a task-board milestone with
"Meeting" in the title (Notification, not Calendar), a thank-you note that ends a
thread (no tag, not Needs reply). They are invented — no real mail is used — but
the archetypes and traps come from what a real inbox contains.

Summaries, drafts and translation are also exercised. Translation is scored
automatically on whether specific facts survive; summaries and drafts are written
to `artifact.json` for a human (or a stronger model) to judge.

`bench/FINDINGS.md` records the results in more detail, including seven bugs
found in the harness itself — each of which produced a believable but wrong
number.

## Commands

| Command | What it does |
| --- | --- |
| `python3 bench_models.py MODEL` | Compare MODEL against the baseline |
| `python3 bench_models.py` | Baseline only |
| `python3 bench_models.py A B C` | Baseline plus several candidates |
| `--quick` | Tagging and speed only, no summaries or drafts |
| `--cat-limit N` | Use N emails, spread across all categories |
| `--pull` | Download any missing models first |
| `--solo` | Don't add the baseline |
| `--consistency` | Re-run 48 emails and report answers that changed |
| `--ab` | Compare prompt variants instead of models |
| `--selftest` | ~1 minute check that everything is wired up |
| `--remote-models NAME` | List models on a remote endpoint |

Remote endpoints work too: `python3 bench_models.py some-model@myproxy`. Aliases
come from your own `~/.config/mailbox/config.toml`, and API keys from the same
keyring entry mailbox uses.

## Two caveats

**It moves your loaded model.** lemonade holds one model at a time, so each
candidate evicts the last. Mailbox's local fallback points at whatever is loaded
mid-run. The originally-loaded model is restored at the end, including on Ctrl-C
— but don't start a sweep five minutes before you need the app.

**The numbers are yours, not universal.** Memory and speed depend on your GPU,
your quantisation and your context size. Quality transfers between machines;
performance does not.
