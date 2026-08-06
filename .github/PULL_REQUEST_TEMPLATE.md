<!-- Reference the issue this PR closes on its own line. -->
Closes #NNN

## Summary

<!-- Brief description of what this PR changes and why. -->

## Checklist

- [ ] Issue exists for this change (#NNN)
- [ ] Branch follows naming convention (`feat/`, `fix/`, `perf/`, or `chore/`)
- [ ] Commit messages follow [Conventional Commits](https://www.conventionalcommits.org/) and reference the issue (`(#NNN)`)
- [ ] `gofmt` and `go vet ./...` are clean
- [ ] `go test ./...` passes
- [ ] `go test ./... -race -count=1` passes (for concurrency-related changes)
- [ ] Cross-platform build checked if platform-specific code was touched:
  - [ ] `GOOS=linux go build ./...`
  - [ ] `GOOS=windows go build ./...`