# Milestone 10 validation status

**Status: incomplete.** This record separates package tests from Docker
acceptance. It does not certify a release.

## Environment and code checked

- Source: `6e08b6b` on `test/end-to-end-validation`, based on `cf3b008`.
- Go: 1.27.2. GCC: 15.2.0.
- Docker CLI: 29.9.1. Docker Engine: 29.8.2 when first checked. Docker Compose:
  5.6.0.
- The Engine later stopped responding. The fresh-checkout demos did not run.
- The worktree contains ignored local binaries and databases. They were not used
  as acceptance evidence.

## Checks

| Check | Result | Evidence |
| --- | --- | --- |
| `go test -p 1 ./...` | Passed after the clock fix | All packages reported `ok`. |
| `go test -count=1 ./internal/recovery` | Passed | Focused package run after the fix. |
| `go test -race ./...` | Incomplete | The run was interrupted before a result. |
| `go vet ./...` | Not run after the fix | A chained check stopped during race tests. |
| `gofmt` and `git diff --check` | Passed for the source fix and smoke-script edit | The smoke script also passes `bash -n`. |
| `go mod verify` | Passed | All downloaded modules verified. |
| `govulncheck` | Not run | The command is not installed. Network access to install it was unavailable. |
| `go list -m all` | Blocked | DNS lookup for `proxy.golang.org` failed in the sandbox. |
| Live inspection and recovery smoke tests | Blocked | Docker Engine stopped responding before either script created resources. |
| Fresh temporary checkout | Not run | The temporary Go toolchain path disappeared during this session. |
| Autoheal and Docker restart-policy comparisons | Not run | No comparison harness or raw trial results exist. |

The recovery package test exposed a clock bug. Pre-dispatch checks used
`time.Now` even when the coordinator received an injected clock. The fix uses
the injected clock for cancellation and freshness timestamps. The focused test
and full Go test suite passed afterward. The full race run still needs a result.

The repository tests cover unit policy decisions, temporary SQLite files,
fake-Docker HTTP protocol behavior, and child-process lifecycle cases. These do
not replace fresh-checkout or real-Docker acceptance.

## Comparison status

No comparison measurements were collected. Do not infer that Aegis performs
better than Docker restart policies or Autoheal. The upstream
[Docker Autoheal README](https://github.com/willfarrell/docker-autoheal#how-to-use)
currently documents `latest` as a daily build and lists `1.1.0` as a released
image tag. The [Docker Hub tags page](https://hub.docker.com/r/willfarrell/autoheal/tags)
lists additional tags. Any comparison must record the pulled image digest, not
only the mutable `latest` tag.

The required comparison runner, repeated trials, independent application
request probe, and sanitized raw result file remain unfinished. Do not report a
comparison until that harness exists and the trials run.

## Release decision

There were no existing version tags. `RELEASE_NOTES.md` proposes `v0.1.0-alpha`
as a draft first preview. Do not tag or publish it until the fresh-checkout
acceptance path, race tests, vet, and repeated comparison experiments have
recorded results and remaining limits.
