# Contributing

Contributions are welcome. The most useful areas are:

- **Language multipliers** — corrections or additions to `COMPLEXITY_MAP`
- **Change type rules** — new path patterns for generated/build output
- **Region rates** — new regions or updated market rates
- **Bug fixes** — incorrect calculations, edge cases, display issues

## Setup

```bash
gem install bundler
bundle install
```

## Running

```bash
bundle exec ruby human-vs-ai /path/to/repo --detail
```

## Guidelines

- Keep the script self-contained in a single file — no additional runtime dependencies beyond `rugged`
- Multiplier changes should include a brief rationale in the comment
- Open an issue before large refactors

## Reporting Issues

Please include the Ruby version (`ruby --version`), OS, and the output of the failing command.
