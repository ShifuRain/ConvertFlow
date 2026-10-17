# Contributing

Thanks for helping. This project is small; keep changes focused.

## Ground rules

- Test-driven: write a failing test first, then the code. Bug fixes come with a regression test.
- Keep security properties intact: no shell, no free-form FFmpeg arguments (all FFmpeg command lines are built in `internal/ffmpeg/args.go` from validated options), no new network egress in the worker, no new dependencies without a reason. Go dependencies must be permissive (`scripts/check-licenses.sh`).
- Documentation is part of the change. `scripts/check-docs.sh` must pass: every `CONVERTFLOW_*` variable in `internal/config/config.go` is in `docs/operations.md`, every API route is in `docs/api.md`, all relative links resolve.
- Do not add claims of compliance or certification to the docs; follow the wording in `docs/grundschutz-mapping.md`.

## Running the checks

```bash
make test                  # go vet + go test (builds the UI first)
scripts/test-db.sh         # full suite against a throwaway PostgreSQL container (CONTAINER_RUNTIME=podman)
scripts/check-docs.sh      # documentation consistency
scripts/check-pinned-digests.sh
tests/container/hardening.sh   # needs built images; see docs/containers.md
```

Only rootless podman is verified as the container runtime; say so when you test something else.

## Commits and sign-off

One logical change per commit with a one-line subject. A `Signed-off-by:` line (Developer Certificate of Origin, `git commit -s`) is welcome but not required. By contributing you agree that your contribution is licensed under the Apache License 2.0 (see `LICENSE`).

## Reporting security problems

Not through issues: see `SECURITY.md`.
