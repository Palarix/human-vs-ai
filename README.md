# human-vs-ai

A git repository analyzer that estimates what a codebase would have cost to build with a human engineering team, versus what it actually cost to build with AI agents. Useful for quantifying AI ROI on a per-project basis.

---

## Quickstart

Download the archive for your platform from the [latest release](https://github.com/palarix/human-vs-ai/releases/latest) (Linux, macOS and Windows on amd64 and arm64), unpack it, and put `human-vs-ai` on your `PATH`. It is a single static binary with no runtime dependencies — not even `git`.

On macOS, a binary downloaded through a browser is quarantined because it isn't signed. Clear that with `xattr -d com.apple.quarantine human-vs-ai`.

Or build from source with Go 1.25+:

```bash
go install github.com/palarix/human-vs-ai@latest
```

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
```

Per-commit output (with `--detail`):

```
2026-03-10   • LOC:549    | Mult:1.3  | HC:$2,838.51   | AIC:$4.36
             ├─ .ts: 768
             └─ std: 98%  test: 2%
```

Summary (always printed):

```
┌─────────────────────────┬────────────────────────────────┐
│ FINAL ROI ANALYSIS      │ US · Senior ($100/hr)          │
├─────────────────────────┼────────────────────────────────┤
│ Commits                 │ 48  (+0 merges skipped)        │
│ LOC Added               │ 16,693                         │
│ Human Investment        │ $79,067.78                     │
│ AI Agent Cost           │ $189.41                        │
│ Net AI Savings          │ 99.8%                          │
├─────────────────────────┼────────────────────────────────┤
│ BUILD SPEED             │ 2026-02-20 → 2026-03-11        │
├─────────────────────────┼────────────────────────────────┤
│ AI build window         │ 20 days                        │
│ Human equiv             │ 790 hrs / 99 working days      │
│ Team size               │ 1 committer                    │
│ Solo developer          │ 27.7 months                    │
├─────────────────────────┼────────────────────────────────┤
│ For reference           │                                │
├─────────────────────────┼────────────────────────────────┤
│ 3-person team           │ 9.2 months                     │
│ 5-person team           │ 5.6 months                     │
│ 10-person team          │ 2.8 months                     │
├─────────────────────────┼────────────────────────────────┤
│ AI speed vs team        │ 41.8x faster                   │
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

---

## The Math

### LOC Delta

Only additions count toward the effort signal. Deletions are weighted at `0.1x` — removing code takes some thought, but it is not equivalent to writing new code.

```
delta = additions + (deletions × 0.1)
```

Merge commits and empty diffs are skipped entirely.

Line counts come from a **minimal** line diff of each commit against its parent, without rename detection, so they match `git log --numstat --minimal --no-renames` exactly. Binary files (a NUL byte in the first 8000 bytes, the same test git uses) count as zero lines. Plain `git diff` can report somewhat higher numbers on large rewrites, because its default algorithm trades minimality for speed.

### Language Complexity Multiplier

Each file extension is assigned a complexity weight reflecting how many lines per hour a developer can realistically produce in that language. The commit-level multiplier is a **LOC-weighted average** across all patches — not a simple patch count average — so a 200-line Rust change dominates over a 5-line markdown edit.

```
lang_mult = Σ(loc_i × complexity_i) / Σ(loc_i)
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

Within a commit, files are classified by path pattern into change types. The type multiplier is also **LOC-weighted**, so a mixed commit (60% standard, 30% tests, 10% docs) gets a proportionally blended score rather than a binary label.

```
type_mult = Σ(loc_i × type_mult_i) / Σ(loc_i)
```

| Type         | Multiplier | Detection                                         |
| ------------ | ---------- | ------------------------------------------------- |
| `dep-update` | 0.05       | Lock files, `package.json`, `go.mod`, etc.        |
| `docs`       | 0.3        | `.md`, `.txt`, `.rst`, `.adoc`                    |
| `config`     | 0.4        | `.yml`, `.yaml`, `.toml`, `.json`, `.tf`, etc.    |
| `test`       | 0.6        | Paths matching `_spec.`, `_test.`, `.test.`, etc. |
| `std`        | 1.0        | Everything else                                   |

### Human Cost per Commit

```
effective_loc_per_hour = BASE_LOC_PER_HOUR × seniority_loc_mult / lang_mult
human_cost = (delta / effective_loc_per_hour) × hourly_rate × type_mult
```

`BASE_LOC_PER_HOUR = 25` is calibrated for deliberate, production-quality new code — not typing speed. This is the right baseline for estimating what AI output _would have cost_ a human team to produce, not how fast code can be typed.

### AI Cost per Commit

```
oversight_mins = AI_HUMAN_OVERSIGHT_MINS × lang_mult × type_mult
ai_cost = AI_BASE_COST_PER_COMMIT + (oversight_mins / 60) × hourly_rate
```

AI cost models two components:

- **API cost:** a flat `$0.05` per commit covering token usage
- **Human oversight:** 2 minutes of engineer review time, scaled by complexity and change type — harder code warrants more careful review

### Build Speed

Human equivalent hours are derived from cost, then projected onto calendar time:

```
human_hours  = total_human_cost / hourly_rate
working_days = human_hours / 8
team_size    = unique committer emails in repo history
calendar_days (team) = (working_days / team_size) × (7/5)
```

The `7/5` factor converts working days to calendar days accounting for weekends. The primary speed comparison uses the **actual team size** detected from unique committer emails — so a 3-person team is compared against the 3-person human estimate, not a hypothetical solo developer. Reference projections for other team sizes are shown below it.

The speed multiplier is:

```
speed_mult = calendar_days (actual team) / actual_build_window
```

---

## Caveats & Limitations

- **LOC is a proxy, not the truth.** It correlates with effort but ignores architecture, debugging, meetings, and research time.
- **`BASE_LOC_PER_HOUR = 25` is deliberately conservative.** It reflects the cost of _producing_ the output, not the pace at which a developer types. Studies on production code quality suggest 10–50 net new LOC/day is realistic for complex systems; 25/hr (200/day) is generous.
- **AI API costs are likely underestimated.** The `$0.05` flat rate does not account for long context windows, retries, failed attempts, or multi-model pipelines. Treat it as a lower bound.
- **Seniority multipliers are opinionated.** The `rate_mult > loc_mult` design for `--principal` reflects the market reality that senior engineers command a premium that outpaces their raw velocity advantage.
- **The tool only walks the current HEAD.** It does not account for work done on branches that were never merged.
- **Merge commits are skipped.** Only linear commits with a single parent are analyzed.
