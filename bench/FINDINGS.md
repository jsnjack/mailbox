# Findings: choosing a local model for mailbox's AI features

Measured on an AMD Ryzen AI 9 HX PRO 370 (Strix Point, Radeon 890M iGPU, 61 GB
RAM, ~31.7 GB GTT), lemonade 11.5.2 with the llamacpp/vulkan backend. Test set:
170 synthetic labelled emails (`cases.py`), production prompt and protocol
(`genprompt.py`).

---

## 1. Metrics: read two numbers, never one

**raw accuracy** treats all 170 cases equally. **inbox-weighted accuracy**
weights each class by how often it actually arrives (`INBOX_MIX`: ~87%
Notification, measured once from a real mailbox).

They disagree, and the disagreement is the point:

- A model that blindly answers `Notification` to everything scores **~17% raw**
  but **~87% weighted**. Weighted alone is gameable by a degenerate model.
- granite-4.1-8b scored **80.0% raw / 63.4% weighted** — weighted *below* raw,
  meaning its errors concentrate in the class that matters most.

Also track **`Needs reply` precision** and **no-tag recall**. A false "Needs
reply" badge on "Thanks! Much appreciated." erodes trust faster than a missing
tag on a newsletter, and neither aggregate captures it.

### Noise floor

At 170 cases, one case = **0.6pp raw**. But Notification has only 30 cases while
carrying 87% of the weight, so one Notification case = **2.9pp weighted**.
Repeat runs at temperature 0 differ by ~1 case (llama.cpp is not bit
deterministic). **Treat weighted deltas under ~3pp as noise.**

---

## 2. Prompt work: +18.4pp weighted, from wording alone

Five variants A/B'd on granite-4.1-8b over the same 170 cases:

| variant | raw | weighted | notif | reply-P | no-tag | `[]` |
|---|---|---|---|---|---|---|
| production | 80.0 | 63.4 | 60.0 | 74.3% | 33.3 | 13 |
| +shape | 82.9 | 69.8 | 66.7 | 70.3% | 8.3 | 0 |
| +shape+notify | 84.1 | 81.3 | 80.0 | 73.0% | 0.0 | 0 |
| +shape+ack | 83.5 | 64.2 | 60.0 | 70.0% | 41.7 | 0 |
| **+all three** | **88.2** | **81.8** | **80.0** | **77.8%** | 41.7 | 0 |

All three are now shipped in `internal/ai/assistant.go`.

**The three rules, and why each was needed:**

1. **Output shape.** The prompt said "use an empty string `""`" for no match; a
   small model rendered that as an empty array `[]`. 13 of 170 emails came back
   as `[]` — including a dev newsletter, a firmware notice and a **resignation
   letter** — and mailbox stores that as no tag. Spelling out that `[]` is never
   valid took it to 0.
2. **Notification precedence.** The prompt called Notification "the catch-all"
   but only said it *loses* to Security and Newsletter — never that it *beats*
   Receipt/Finance/Calendar/Needs reply. So automated mail leaked: "installation
   finished" → Receipt, "spend alert" → Finance, "X requested time off" → Needs
   reply. Notification recall 60% → 80%.
3. **Acknowledgements.** Needs "an OPEN question"; excludes thanks, closings,
   congratulations, no-action FYIs, own sent mail.

### The most important lesson: tweaks interact, and are model-specific

**Neither rule 2 nor rule 3 works alone.** Rule 2 alone drives no-tag recall to
**0%** — told automated mail is Notification, the model tags everything. Rule 3
alone does nothing for the dominant class. Together each repairs the other's
collateral damage. Testing one at a time would have rejected both.

And they do not transfer. Two further candidates (`RECEIPT_TWEAK`,
`SELF_TWEAK`, still in `bench_models.py`, unshipped) helped granite by about one
noisy case and **cost Qwen3.5 5.6pp weighted**. A tweak that fixes a weaker
model can constrain a stronger one that never had the problem.

### Where prompt work stops paying

After all three rules, **Notification still costs 17.4 of the 18.2 weighted
points** granite loses. Remaining misses cluster as: no-tag → Needs reply (×6),
and Receipt acting as a magnet (4 false positives). Two of the 20 remaining
misses are arguably mislabelled by the test set, not the model, so the realistic
ceiling here is ~89–90%. Further gains have to come from the model.

---

## 3. Reasoning models: the trap

Qwen3.5-35B-A3B initially scored **7.1% raw** — the floor. It had not answered
badly; it had not answered at all. All 170 replies had **blank content** with
chain-of-thought in a side field.

Measured, one email, same prompt:

| approach | result | tokens | time |
|---|---|---|---|
| `max_tokens=512` | **blank**, truncated mid-thought | 512 | 24.2s |
| `max_tokens=2048` | correct | 772 | **31.5s** |
| `chat_template_kwargs{enable_thinking:false}` | correct | 4 | **0.4s** |
| `reasoning_effort=none` | correct | 4 | **0.3s** |
| `/no_think` suffix | blank | 64 | 4.5s |
| prefill assistant with `[` | correct | 5 | 0.8s |

Raising the budget "works" at ~80× the cost. Disabling thinking gives the same
answer in 0.3s. Classification is an argmax, not a deliberation — which is why
the prompt already pins temperature 0.

**Shipped as a result** (`internal/ai/`):
- `Options.AllowReasoning`, defaulting to false, so `Assistant.stream` suppresses
  for every op unless one opts in — it sends both `reasoning_effort:"none"` and
  `chat_template_kwargs.enable_thinking:false`, because servers honour different
  keys and both are ignored when unknown. (First shipped as the inverse,
  `SuppressReasoning`, set only on `Categorize`; section 7 explains why that was
  wrong.)
- A streaming `thinkFilter` (`think.go`) that drops inline `<think>` blocks for
  every provider and every op. Stripping after the fact is not enough: the
  stream renders incrementally, so reasoning would appear in the compose window
  before `</think>` arrived. The filter withholds text that might be a partial
  tag, so a tag split across SSE chunks cannot leak.
- `Categorize` now returns a diagnostic error on an empty reply instead of
  failing opaquely.

---

## 4. Model comparison so far

Best variant each, 170 cases:

| | granite-4.1-8b Q5 | Qwen3.5-35B-A3B |
|---|---|---|
| raw | 87.6% | **90.0%** |
| **weighted** | 78.9% | **90.0%** |
| hard cases | 77.1% | **88.6%** |
| Needs-reply recall | **100%** | 92.9% |
| no-tag recall | 41.7% | **100%** |
| Notification | 76.7% | **90.0%** |
| Newsletter | **90.9%** | 72.7% |
| generation | 14.5 tok/s | **23.6 tok/s** |
| categorise / email | **0.52s** | 0.67s |
| translation facts | 92.3%, 0 leaks | 92.3%, 0 leaks |
| on disk | **5.8 GB** | 21.6 GB |

**Generative quality**, graded against a fixed rubric:

- **Drafts: Qwen clearly better (5/5 vs 3.5/5).** granite inverted the ask in
  both reps — told to *ask about parking*, it wrote "let us know if you have any
  specific requirements regarding parking", making the client state
  requirements. Qwen asked about availability. Qwen was also 1.8× faster and
  read like a person wrote it.
- **Summaries: effectively tied** (granite 3.75, Qwen 3.5 of 5), Qwen 2× faster
  and tighter. Different failure modes: Qwen rep0 invented "the client agreed",
  which never happened (rep1 was correct, so variance not bias); granite
  consistently drifted "40 sessions/day" into "40 sessions affected" and dropped
  a fact in rep1.

**Prefill and generation are independent axes.** Between these two models prefill
looked constant (~330 tok/s each) while generation separated them (14.5 vs 23.6),
which suggested prefill was size-independent. **The 7-model sweep in section 6
disproved that**: prefill ranges 229–1064 tok/s, a 4.6× spread, and the
best-quality model (Gemma-4-12B-MTP, 229) is the *slowest* reader of all. Since
summaries and translation are input-bound, a model can beat the baseline on
generation and still be slower at the two features a user waits for. Never infer
prefill from a two-model sample.

---

## 5. Harness bugs found (all fixed) — read this before trusting a number

Every one of these produced a plausible-looking wrong result:

1. **Malformed payload.** A cache-busting `<!--nonce-->` was appended *after* the
   JSON array while the prompt promised "the user message is a JSON array".
   Cost 12.3 weighted points on its own and inflated the `[]` count from 13 to
   21 — nearly credited to the prompt tweak instead.
2. **Prefill measured a cache hit.** The nonce sat at the *end* of the prompt and
   llama.cpp caches by common *prefix*, so a warm rerun reported 5756 tok/s
   against a true ~340. The nonce now goes first.
3. **Reasoning models scored as bad classifiers.** Fixed by detecting blank
   content plus chain-of-thought and reporting the run **INVALID** rather than
   publishing a score.
4. **`[]` was invisible.** It parsed as mode `scalar`, hiding the single largest
   failure shape. Now its own `empty_array` mode.
5. **max_tokens=32** on the category call, far too small for a reasoning model.

Lesson: when a model scores near the floor, suspect the harness first.

---

## 6. Model sweep: 7 models, 272 cases

Corrected translation column (see the stale-fact bug below): 100% for granite,
Gemma-4-E4B, Qwen3.5-9B, Gemma-4-12B-MTP and Qwen3.5-35B; 91.7% for LFM2 and
Qwen3-8B.

| model | RAM | wtd% | raw% | reply-R | reply-P | hard% | gen | TTFT | flip |
|---|---|---|---|---|---|---|---|---|---|
| granite-4.1-8b Q5 *(baseline)* | 11.0 | 83.2 | 84.6 | 100% | 63.6% | 71.2 | 14.7 | 0.38 | 0% |
| Gemma-4-E4B | **8.6** | 87.7 | 89.7 | 100% | 73.7% | 76.3 | **25.9** | 0.39 | 0% |
| LFM2-8B-A1B | **6.3** | 33.7 | 45.2 | 75.0% | 48.8% | 30.5 | **78.0** | **0.28** | 4.2% |
| Qwen3-8B | 10.7 | 69.0 | 77.2 | 96.4% | 65.9% | 55.9 | 18.2 | 0.44 | 0% |
| Qwen3.5-9B | 15.5 | 92.4 | 89.7 | 96.4% | 77.1% | 78.0 | 14.8 | 0.65 | 0% |
| **Gemma-4-12B-MTP** | 11.3 | **96.2** | **94.9** | 89.3% | **96.2%** | **89.8** | 17.5 | 1.04 | 0% |
| Qwen3.5-35B-A3B | 27.7 | 92.1 | 91.2 | 92.9% | 86.7% | 84.7 | 24.2 | 0.71 | 0% |

Pareto frontier (max quality, min RAM): **LFM2 (6.3/33.7) → Gemma-4-E4B
(8.6/87.7) → Gemma-4-12B-MTP (11.3/96.2)**. Every other model is dominated.

### Conclusions

1. **granite is dominated at every footprint.** Gemma-4-E4B beats it on quality
   *and* uses 2.4 GB less; Gemma-4-12B-MTP beats it by 13.0pp weighted at the
   same 11 GB. Replace it either way.
2. **Bigger is not better, and 21 GB buys nothing.** Qwen3.5-35B-A3B needs
   27.7 GB resident and still loses to Gemma-4-12B-MTP by 4.1pp at 11.3 GB. An
   earlier estimate that "21 GB buys +10.4pp" was right against granite and
   wrong as advice — it priced the jump against the wrong alternative because
   the middle of the range had not been tested.
3. **Generation beats parameter count.** Qwen3-8B (dense, older) 69.0% vs
   Qwen3.5-9B (newer) 92.4%. Same family, same size class, 23pp apart.
4. **A tiny active-parameter MoE fails this task.** LFM2-8B-A1B (~1B active) is
   the fastest to first token (0.28s), fastest generating (78 tok/s) and
   lightest (6.3 GB) — and scores 33.7%, half of granite. It is also the only
   model that wobbles on a repeat pass (4.2%).
5. **File size is not RAM.** llama-server runs with `--ctx-size 32768` and
   llama.cpp preallocates the KV cache, so resident footprint is 1.28×–2.25× the
   GGUF: granite 5.8 GB file → 11.0 GB resident, Qwen3.5-9B 6.9 → 15.5.
   A "≤10 GB" rule expressed in file sizes would have admitted models needing
   15 GB and rejected none of them.
6. **TTFT and throughput are independent axes.** LFM2 wins both and is useless;
   Gemma-4-12B-MTP has the best quality and the worst TTFT (1.04s vs 0.38s).
   TTFT only matters where output streams to a human, so it should not
   disqualify a model used by the background categorizer.
7. **Recall and precision moved in opposite directions all sweep.** granite has
   100% `Needs reply` recall and 63.6% precision — 16 false alarms in 44 claims.
   Gemma-4-12B-MTP has 89.3% recall and 96.2% precision: 1 false alarm in 26.
   Reporting recall alone would have ranked granite top on the tag that matters
   most.

### Harness bug #6: a stale fixture silently killed a metric

Every model scored exactly 92.3% on translation, which was the tell. The privacy
sweep renamed the Dutch email's signature to "Elena Novak" but left the fact list
asserting "marieke de vries" — an unfindable fact, so all seven models lost the
same point and the metric could not discriminate. Fixed, with a comment tying the
fact list to the body. General lesson: **an identical score across very different
models is a bug signal, not a finding.**

---

## 7. Thinking is the default, so suppression must be too

**5 of the 7 models surveyed think by default** — Gemma-4-E4B, Qwen3-8B,
Qwen3.5-9B, Gemma-4-12B-MTP and Qwen3.5-35B-A3B. Only granite-4.1-8b and
LFM2-8B-A1B do not. That makes thinking-suppression a baseline requirement, not
an accommodation for one model.

Measured on Gemma-4-E4B, the model selected, same draft prompt:

| | latency | tokens | quality |
|---|---|---|---|
| thinking on | **12.7s** | 324 | correct |
| thinking off | **2.4s** | 55 | correct |

**5.3× slower for no quality gain**, on the most interactive feature in the app.

The first implementation set suppression only on `Categorize`, which would have
left every draft, summary and translation thinking. `Options` was therefore
inverted to `AllowReasoning`, defaulting to **false**: `Assistant.stream` is the
single gate all 13 ops call through, so suppression is now the default everywhere
and an op must deliberately opt in. A new op cannot accidentally inherit
13-second drafts.

### Harness bug #7: the detector was reactive (fixed)

The bench originally suppressed thinking only *reactively*, when a reply came
back blank. With a 512-token budget a thinking model often finishes deliberating
and answers correctly — just 15× slower — so nothing triggered and the cost was
invisible: a selftest run paid 14–18s per email while reporting 8/8 correct.

The sweep results were NOT affected, because its warmup call uses
`max_tokens=5`, which a thinking model cannot satisfy, so suppression engaged
before any measurement; log ordering confirms `(reasoning model detected)`
precedes the `speed` line for all five thinking models.

The harness now suppresses unconditionally on every call, exactly as
`Assistant.stream` does. Same selftest afterwards: 0.3s per email instead of
14–18s, and the latch, the retry and the detection branch all disappeared.
