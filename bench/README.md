# bench — local model benchmark for mailbox's AI features

Picks the **fastest model that still holds quality** for the three jobs mailbox
actually does: inbox categorisation, thread summaries, and AI draft replies
(plus translation). Speed alone is a trap — fast and wrong is worse than slow
and right — so quality is measured, not assumed.

## Split of labour

| Measured by the script (objective) | Graded by a strong model reading `artifact.json` |
| --- | --- |
| prefill tok/s (summaries, translation) | summary faithfulness and coverage |
| generation tok/s (draft replies) | invented facts / hallucination |
| categorisation accuracy over 170 labelled emails | draft tone, did it do every ask |
| per-category recall, per-difficulty accuracy | meta-preamble, padding |
| translation: specific facts that must survive | consistency across repetitions |
| untranslated source-language leakage | |

No local model judges another local model, and there is no dependency on a
remote judge being reachable.

## Fidelity

The categorisation test is not a paraphrase of what the app does — it is what
the app does:

- the **production system prompt**, extracted from `internal/ai/assistant.go`
  by `genprompt.py` (re-run it whenever that prompt changes)
- the same protocol: JSON array in and out, `temperature 0`, one email per call,
  matching `internal/aiwork/worker.go`
- the same item format: `From: <who> / Subject: <subj> / <snippet>`
- the same tolerant parsing: a Python port of `parseCategories` + `MatchCategory`,
  so a model is scored on what mailbox would **actually store**, including the
  alias fixups (`marketing`→Newsletter, `invite`→Calendar, `alert`→Notification)

## The two accuracy numbers

Always read both. A model that blindly answers `Notification` to everything
scores **≈17% raw** but **≈87% inbox-weighted**, because automated notifications
genuinely dominate a developer inbox. Weighted accuracy tells you what you would
experience; raw and per-class accuracy tell you whether the model is
discriminating at all. `Needs reply` recall is the number that costs you most
when it drops — that is the category with real action value.

## Test set

`cases.py` — 170 synthetic labelled emails, 10 categories, tagged
`easy`/`med`/`hard`, with 38 **probes**: regression tests for one specific
boundary rule each, e.g.

- a completed task-board milestone whose title contains the word "Meeting"
  (→ Notification, not Calendar)
- a public webinar with a date promoted to a mailing list (→ Newsletter)
- an "invoice" for a purchase already paid (→ Receipt, not Finance)
- the user's own sent reply mentioning a "discount code" (→ no tag)
- a security-themed dependency digest (→ Notification, not Security)

Everything in it is invented — no real senders, names, or content — so it is
safe to commit and share. The archetypes and decoys are modelled on patterns
found in a real inbox, several of which a live categoriser got wrong.
Multilingual cases (Dutch, German, Russian) are deliberate: small models degrade
noticeably on non-English input.

Add your own cases freely. Every extra case sharpens the ranking, because all
models see the identical set — the comparison is paired.

## Usage

```bash
python3 genprompt.py ~/workspace/mailbox   # required once; writes prompt.py
python3 bench_models.py --selftest         # ~1 min, uses the LOADED model only
python3 bench_models.py --quick            # speed + all 170 categories
python3 bench_models.py                    # adds translation, summaries, drafts
```

Targets can be local or remote:

```bash
python3 bench_models.py granite-4.1-8b-GGUF-Q5_K_M      # local lemonade
python3 bench_models.py some-model@myproxy              # alias from your config
python3 bench_models.py some-model@https://host/v1      # explicit endpoint
python3 bench_models.py --remote-models myproxy         # list what's available
```

Remote endpoint aliases are read at runtime from your own
`~/.config/mailbox/config.toml` (`[ai].endpoint` and each `[[ai.chain]]`), keyed
by the first label of the hostname — nothing internal is hardcoded here. Add
more with `BENCH_ENDPOINTS="name=https://host/v1"`. API keys come from
`$MAILBOX_AI_KEY`, else the keyring (`service mailbox-ai`,
`username=<endpoint>`) — the same place mailbox reads them. Keys are never
printed or written to the artifact.

Missing local models are listed with their download size and **skipped**; pass
`--pull` to download them. Backends needing an update are flagged too.

## Caveat: it evicts your loaded model

lemonade runs with `Max Models/Type = 1`, so each local load evicts the previous
one — while a run is in progress, mailbox's local fallback points at whatever is
loaded. The originally-loaded model is restored in a `finally` block, including
on Ctrl-C. Remote targets evict nothing.

## Files

| File | Committed | What |
| --- | --- | --- |
| `bench_models.py` | yes | the harness |
| `cases.py` | yes | 170 synthetic labelled emails |
| `genprompt.py` | yes | extracts the production prompt from the Go source |
| `prompt.py` | no (generated) | output of `genprompt.py` |
| `artifact.json` | no (generated) | raw outputs + scores, for grading |
| `realcases.py`, `pool.json` | **no (private)** | optional ground-truth set built
  from real mail; enable with `--private`. Contains real senders and subjects —
  gitignored, keep it that way. |
