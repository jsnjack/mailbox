# bench

A benchmark for choosing the local model behind mailbox's AI features.

**Usage and results: [../docs/BENCHMARK.md](../docs/BENCHMARK.md).**

```bash
python3 genprompt.py ..                      # once, and after any prompt change
python3 bench_models.py Qwen3.5-9B-GGUF      # compare a model with the baseline
```

| File | What |
| --- | --- |
| `bench_models.py` | the harness |
| `cases.py` | 272 synthetic labelled emails |
| `genprompt.py` | lifts the production prompt out of `internal/ai/assistant.go` |
| `probe_thinking.py` | one-shot probe for getting a reasoning model to answer briefly |
| `FINDINGS.md` | measured results, and the harness bugs found along the way |

`prompt.py`, `artifact.json` and `probe_thinking.json` are generated and
gitignored.

`cases.py` must stay synthetic. Do not author it with real mail open — an
earlier version was, and reproduced 524 phrases from a real mailbox while a
name scan reported it clean.
