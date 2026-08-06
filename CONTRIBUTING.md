# Contributing to procman

Thank you for your interest in contributing to `procman`! This document covers
the basics of getting set up and how changes are tracked.

## Prerequisites

- **Go 1.24 or later** — the module targets `go 1.24`.
- A platform with working `os/exec` support (Linux, macOS, or Windows).

## Getting started

```sh
git clone https://github.com/mainvec/procman.git
cd procman
go test ./...
```

## Running tests

```sh
# Full test suite
go test ./...

# Race detector (recommended for concurrency-related changes)
go test ./... -race -count=1

# Cross-platform build checks
GOOS=linux   go build ./...
GOOS=windows go build ./...
```

## How changes are tracked

Every change — feature, bug fix, performance improvement, or chore — starts
with a GitHub issue. If one does not exist yet, [open one][new-issue] before
writing code.

### Branch naming

Branches are named after the issue:

| Type       | Pattern              |
|------------|----------------------|
| Feature    | `feat/NNN-slug`      |
| Bug fix    | `fix/NNN-slug`       |
| Performance| `perf/NNN-slug`      |
| Chore      | `chore/NNN-slug`     |

`NNN` is the issue number (zero-padded to 3 digits in plan files only).
`slug` is a short lowercase, hyphen-separated description.

### Commit messages

Follow [Conventional Commits][conventional-commits] and reference the issue:

```
feat: add restart policy support (#42)
fix: handle nil process state on windows (#42)
perf: reduce event loop allocations (#42)
chore: update dependencies (#42)
```

### Pull requests

- Open a PR from your branch into `main`.
- Include `Closes #NNN` on its own line in the PR body so the issue
  auto-closes on merge.
- PR titles follow the same Conventional Commits format as commit messages.
- Make sure `go test ./...` and `go vet ./...` pass before requesting review.

## Code style

- Run `gofmt` before committing.
- Run `go vet ./...` and fix any warnings.
- Follow the patterns established in the existing code — functional options for
  configuration, platform-specific build tags for OS-specific code.

## Reporting bugs

[Open a bug report][new-issue] with:

- Your operating system and Go version.
- A minimal reproduction.
- Expected and actual behavior.

## License

By contributing, you agree that your contributions are licensed under the
project's [MIT License](LICENSE).

[new-issue]: https://github.com/mainvec/procman/issues/new
[conventional-commits]: https://www.conventionalcommits.org/