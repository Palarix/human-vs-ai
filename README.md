# human-vs-ai

A Git repository analyzer that estimates the human replacement effort for a codebase and compares it with either a transparent frontier-model token estimate or known AI spend. Human operator time is included only when explicitly supplied.

---

## Quickstart

Download the archive for your platform from the [latest release](https://github.com/palarix/human-vs-ai/releases/latest) (Linux, macOS and Windows on amd64 and arm64), unpack it, and put `human-vs-ai` on your `PATH`. It is a single static binary with no runtime dependencies — not even `git`.

On macOS, a binary downloaded through a browser is quarantined because it isn't signed. Clear that with `xattr -d com.apple.quarantine human-vs-ai`.

Or build from source with Go 1.25+:

```bash
go install github.com/palarix/human-vs-ai@latest
```

From a source checkout, `make build` creates a local static binary and `make check` runs formatting checks, vet, and race-enabled tests.

Then:

```bash
# Summary only — US market, Senior developer (defaults)
human-vs-ai /path/to/your/repo

# Full per-commit breakdown
human-vs-ai /path/to/your/repo --detail

# Western Europe rates, junior developer, with detail
human-vs-ai /path/to/your/repo --eu --junior --detail

# US rates, principal engineer
human-vs-ai /path/to/your/repo --us --principal

# Asia rates, senior developer
human-vs-ai /path/to/your/repo --asia --senior

# Conservative/high estimate and a 3-person capacity projection
human-vs-ai /path/to/your/repo --profile high --team-size 3

# Use known AI spend and operator time instead of relying on cost estimates
human-vs-ai /path/to/your/repo --ai-cost-total 1200 --operator-hours 80
```

Per-commit output (with `--detail`):

```
2026-03-10   • LOC:549    | Mult:1.3  | HC:$2,838.51
             ├─ .ts: 768
             └─ std: 98%  test: 2%
```

Summary (always printed):

```
┌─────────────────────────┬────────────────────────────────┐
│ EFFORT ESTIMATE         │ US · Senior · base             │
├─────────────────────────┼────────────────────────────────┤
│ Commits                 │ 48  (+0 merges skipped)        │
│ LOC Added               │ 16,693                         │
│ Human replacement       │ $79,067.78                     │
│ AI scenario             │ Sol-level · medium effort      │
│ Tokens / weighted LOC   │ 2,500                          │
│ AI tokens (estimated)   │ 49.4M                          │
│ AI cost (estimated)     │ $133.38                        │
│ AI cost / weighted LOC  │ $0.0068                        │
│ Operator labor          │ not provided                   │
│ Tool-cost savings       │ 99.8%                          │
├─────────────────────────┼────────────────────────────────┤
│ HUMAN SENSITIVITY       │ low–high assumptions           │
├─────────────────────────┼────────────────────────────────┤
│ Human cost range        │ $49,000–$132,000               │
├─────────────────────────┼────────────────────────────────┤
│ EFFORT & SPAN           │ 2026-02-20 → 2026-03-11        │
├─────────────────────────┼────────────────────────────────┤
│ Repository span         │ 20.0 days                      │
│ Human equivalent        │ 791 hrs / 99 workdays          │
└─────────────────────────┴────────────────────────────────┘
```

---

## Parameters

### Region `(default: --us)`

Sets the base hourly rate for the human team.

| Flag     | Region         | Rate    |
| -------- | -------------- | ------- |
| `--us`   | US             | $100/hr |
| `--eu`   | Western Europe | $75/hr  |
| `--east` | Eastern Europe | $45/hr  |
| `--asia` | Asia           | $30/hr  |

### Seniority `(default: --senior)`

Adjusts both hourly rate and coding velocity relative to the Senior baseline.
Principal engineers cost more per unit of output because their rate premium (`1.75x`) exceeds their productivity gain (`1.35x`). Junior is the reverse — their productivity drop (`0.7x`) is worse than their rate discount (`0.6x`), making them slightly more expensive per line.

| Flag          | Rate mult | LOC/hr mult | Effective LOC/hr (US Senior base) |
| ------------- | --------- | ----------- | --------------------------------- |
| `--junior`    | 0.6x      | 0.7x        | 17.5                              |
| `--senior`    | 1.0x      | 1.0x        | 25.0                              |
| `--principal` | 1.75x     | 1.35x       | 33.75                             |

Region and seniority flags compose freely. The effective hourly rate is `region_rate × seniority_rate_mult`.

### Output `(default: summary only)`

| Flag        | Effect                                             |
| ----------- | -------------------------------------------------- |
| `--detail`  | Show per-commit breakdown before the summary table |
| `--version` | Print the version and exit                         |

### Estimation

| Option                            | Effect                                                        |
| --------------------------------- | ------------------------------------------------------------- |
| `--profile low\|base\|high`       | Select an assumption profile; default is `base`               |
| `--team-size N`                   | Show a perfect-parallel capacity floor for an explicit team   |
| `--ai-cost-total AMOUNT`          | Replace estimated model cost with known subscription/API spend |
| `--operator-hours HOURS`          | Include known human operator time at the selected hourly rate  |

The built-in profiles deliberately vary the most consequential assumptions:

| Profile | Baseline LOC/hr | Deletion weight |
| ------- | --------------: | --------------: |
| `low`   | 40              | 0.05            |
| `base`  | 25              | 0.10            |
| `high`  | 15              | 0.20            |

“Low” and “high” refer to the resulting cost estimate. They are sensitivity bounds, not confidence intervals.

---

## The Math

### LOC Delta

Additions are the primary effort signal. In the base profile, deletions are weighted at `0.1x`—removing code takes some thought, but it is not equivalent to writing new code. The sensitivity profiles vary this weight.

```
delta = additions + (deletions × 0.1)
```

Root commits are diffed against an empty tree. Merge commits and empty diffs are skipped. Commits reachable from merged branches are still analyzed, but edits made only while resolving a merge conflict can be omitted.

Line counts come from a minimal line diff of each commit against its parent. Rename detection prevents file moves and project-wide renames from being priced as complete rewrites. Binary files (a NUL byte in the first 8000 bytes, the same test Git uses) count as zero lines.

### Language Complexity Multiplier

Each file extension is assigned a complexity weight reflecting how much human effort a line represents relative to the baseline. Unknown extensions use `1.0`.

```
file_delta_i = additions_i + (deletions_i × deletion_weight)
```

Representative values:

| Language         | Multiplier | Rationale                                  |
| ---------------- | ---------- | ------------------------------------------ |
| Markdown, JSON   | 0.2 – 0.3  | Declarative, no logic                      |
| CSS/SCSS, HTML   | 0.5 – 0.6  | Structural, low cognitive load             |
| Ruby, Python, Go | 1.0        | Baseline — expressive, readable            |
| TypeScript       | 1.3        | Type system overhead over plain JS         |
| Java, Swift, C#  | 1.3 – 1.4  | Verbose OOP, boilerplate                   |
| C                | 1.6        | Manual memory, no abstractions             |
| Scala, Haskell   | 1.6 – 2.0  | Type theory, functional abstractions       |
| C++              | 2.0        | Templates, OOP complexity, manual memory   |
| Rust             | 2.2        | Borrow checker + lifetimes make it hardest |
| Lock files       | 0.05       | Machine-generated, nearly free             |

### Change Type Multiplier

Files are also classified by path. Tests, documentation, and configuration remain visible in the breakdown, but they do not receive a second effort discount: their language multiplier already represents their artifact type. Only automated output is discounted.

```
file_effort_i = file_delta_i × language_mult_i × type_mult_i
```

| Type         | Multiplier | Detection                                         |
| ------------ | ---------- | ------------------------------------------------- |
| `generated`  | 0.0        | Build output, minified bundles, maps, binary assets |
| `dep-update` | 0.1        | Lock files, `package.json`, `go.mod`, etc.        |
| `docs`       | 1.0        | `.md`, `.txt`, `.rst`, `.adoc`                    |
| `config`     | 1.0        | `.yml`, `.yaml`, `.toml`, `.json`, `.tf`, etc.    |
| `test`       | 1.0        | Test filenames and conventional test directories  |
| `std`        | 1.0        | Everything else                                   |

### Human Cost per Commit

```
effort_units = Σ(file_effort_i)
human_hours = effort_units / (base_loc_per_hour × seniority_loc_mult)
human_cost = human_hours × hourly_rate
```

The base profile uses 25 baseline LOC/hour. Because this and the multipliers are assumptions rather than measurements, the summary also reports low-to-high sensitivity results.

### AI Token and Cost Estimate

```
total_tokens = effort_units × 2,500
token_mix = 30% uncached input + 50% cached input + 20% output
model_cost = Σ(tokens_by_type × price_by_type)
```

The default represents a medium-effort frontier coding-agent workflow, including repository reads, tool results, retries, and reasoning—not merely the tokens visible in generated code. It uses [OpenAI's published GPT-6 Sol rates](https://platform.openai.com/pricing) of `$2.00/M` uncached input tokens, `$0.20/M` cached input tokens, and `$10.00/M` output tokens. These assumptions produce approximately `$0.00675` per weighted LOC.

Token reconstruction from Git is necessarily approximate. `--ai-cost-total` replaces the dollar estimate with known subscription or API spend while retaining the token estimate for reference.

Human operator labor is not guessed. Supply `--operator-hours` to include it; otherwise the output explicitly says that it was not provided and labels the percentage as tool-cost savings rather than total savings.

### Effort and Repository Span

The tool reports human-equivalent effort separately from the elapsed repository span:

```
working_days = human_hours / 8
repository_span = last_analyzed_commit - first_analyzed_commit
```

Repository span is not treated as active development time, so the tool does not claim an AI speed multiplier. With `--team-size`, it additionally shows a capacity floor that assumes perfect parallelization and a five-day work week; it is not a delivery forecast.

---

## Caveats & Limitations

- **LOC is a proxy, not the truth.** It correlates with effort but ignores architecture, debugging, meetings, and research time.
- **The base rate of 25 baseline LOC/hour is an assumption.** It represents production effort rather than typing speed and should be interpreted alongside the sensitivity range.
- **AI usage cannot be recovered from Git.** The token model is a transparent scenario, not reconstructed billing. Supply actual AI spend where possible.
- **Seniority multipliers are opinionated.** The `rate_mult > loc_mult` design for `--principal` reflects the market reality that senior engineers command a premium that outpaces their raw velocity advantage.
- **The tool only walks the current HEAD.** It does not account for work done on branches that were never merged.
- **Merge commits are skipped.** Reachable branch commits are analyzed, but merge-only conflict-resolution changes may be omitted.
- **Team projections are capacity floors.** They assume perfect parallelism and ignore coordination costs and indivisible work.
