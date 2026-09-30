# Contributing

Contributions are welcome. The most useful areas are:

- **Language multipliers** — corrections or additions to `COMPLEXITY_MAP`
- **Change type rules** — new path patterns for generated/build output
- **Region rates** — new regions or updated market rates
- **Bug fixes** — incorrect calculations, edge cases, display issues

## Setup

Requires Go 1.25+.

```bash
go test ./...
```

## Running

```bash
go run . /path/to/repo --detail
```

## Releasing

Pushing a `v*` tag (e.g. `git tag v1.0.0 && git push origin v1.0.0`) makes GitHub Actions cross-compile static binaries for Linux, macOS and Windows (amd64 and arm64) and attach them, with a `SHA256SUMS` file, to a new GitHub release. Every push and pull request also builds all targets and uploads them as workflow artifacts.

## Guidelines

- Keep the tool dependency-light: pure Go (`CGO_ENABLED=0`), with `go-git` as the only direct dependency
- Multiplier changes should include a brief rationale in the comment
- Run `gofmt` and `go test ./...` before opening a pull request
- Open an issue before large refactors

## Reporting Issues

Please include the version (`human-vs-ai --version`), OS, and the output of the failing command.
