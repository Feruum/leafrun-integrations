# Contributing

Use English for source comments, documentation, commit messages, and pull request titles and descriptions. Unicode filenames are welcome in fixtures that test source handling.

## Git flow

- `main` contains released code and `develop` is the integration branch.
- Start `feature/<issue>-<description>` from `develop`; submit a draft pull request into `develop` while work is being verified.
- If upstream `develop` does not exist, preserve the prepared feature branch until a maintainer creates it. Do not merge a feature directly into `main`.
- Start `release/<version>` from `develop` after review and acceptance. Merge the release into `main`, tag that merge, and merge release changes back into `develop`.
- Start urgent `hotfix/<version>` branches from `main` and integrate them into both `main` and `develop`.

Use merge commits to preserve feature and release history. Do not rewrite shared branches. M02 prepares the first adapters release, `v0.1.0`; merging, tagging and releasing require maintainer review.

## Module boundary

Source adapters provide standard `io/fs` trees, provenance and owned resource cleanup. They must remain independent of `leafrun-document-core`. The core CLI adapts their results to its own source contract.

Do not commit a development `go.work` file or local `replace` directive. Related feature branches may be tested together with a temporary workspace outside their repositories; record the exact commits used. After adapters are released, the core pins their version and is checked with `GOWORK=off`.

## Verification

Use Go 1.27.0, native Git and native OpenSSH. Tests must use ephemeral local authentication fixtures rather than real credentials. Missing mandatory tools are test failures.

```sh
GOWORK=off go test ./... -count=1 -v
GOWORK=off go vet ./...
GOWORK=off go build ./...
```

On Linux, also run `GOWORK=off go test -race ./... -count=1`. The related core branch verifies the complete pipeline with Typst 0.15.1 on Linux, Windows and macOS. Keep cancellation, safe extraction and required cleanup checks enabled.

No project license has been selected yet.
