# M02 source adapter verification

Implementation is in progress on `feature/lea-12-source-adapters`. Evidence is added only after the corresponding checks run. The adapters are independent of `leafrun-document-core`; they return standard `io/fs` trees for core to capture.

## Baseline

The checkout preserves the upstream Git history through `a6492ffb81166d2515ac78e2cf04e9425022aebb`. The module uses Go 1.27.0, `golang.org/x/crypto v0.58.0` for ephemeral SSH test fixtures, and `golang.org/x/sys v0.49.0` for Windows process control. The development workspace lives outside both repositories and is not part of delivery.

The local environment has Git 2.47.1 and OpenSSH 9.5p2. The native tool check in CI was exercised successfully on Windows. CI requires Git and OpenSSH on Linux, Windows and macOS; missing tools cause failure. Linux additionally runs the race detector.

## Acceptance evidence

| Area | Required behavior | Evidence |
| --- | --- | --- |
| ZIP | Retained tree, portable paths, CRC, actual quotas, no duplicate/case collisions | Spec and quality reviews approved through `436baae`; exact native three-platform CI passed |
| TAR.GZ | Regular files/directories, header checksums, complete gzip stream, actual quotas | Effective PAX framing, precedence, deletion and quotas independently verified; spec and quality reviews approved through `436baae` |
| Archive provenance | Digest of the same accepted compressed bytes; protected original archive | Canonical path/alias and accepted-copy mutation regressions pass locally |
| Source selection | Validate the complete archive before selecting a subtree | Unsafe/corrupt entries outside selected root are rejected in local tests |
| Cleanup | Canonical owned temporary storage; cancellation and failed extraction leave no owned copies | Alias retargeting, tracked-reader revocation and cleanup-cause tests pass; bounded FIFO/substitution/cancellation and descriptor checks pass with Linux race detector at `436baae` |
| Git revision | Required unambiguous ref; full resolved commit; movement cannot alter accepted inputs | Pending |
| Git objects | Raw tree/blobs without checkout/hooks/filters; submodules and LFS rejected | Pending |
| HTTPS authentication | Native configured helper, local TLS fixture, temporary credentials, no prompts | Pending |
| SSH authentication | Native OpenSSH, temporary keys/known hosts, auth and host-key failures | Pending |
| Diagnostics | Transport credentials redacted; original process cause available through Unwrap | Pending |
| Git cancellation | Entire process tree terminated and descriptors closed before owned cleanup | Pending |
| Native CI | Tests, vet, build on Linux/Windows/macOS; Linux race detector | Archive phase passed at `436baae`; final Git phase pending |

## Delivery

Prepare a draft pull request into upstream `develop` when that branch exists. Keep the related core feature stacked on M01 and test both exact feature commits in a temporary workspace. Do not ship a workspace or local replacement, merge, release, or mark Linear Done before review and acceptance. After review, release integrations `v0.1.0` before pinning and testing the core dependency with `GOWORK=off`.

## Archive implementation and review

Commit `44ae720b873cc63eaf1d2abb4215ec91dbc9b556` introduced the standalone contracts and ZIP/TAR.GZ adapters. Native Windows tests, vet and build passed, as did Linux amd64/macOS arm64 test compilation. [Initial native CI](https://github.com/Feruum/leafrun-integrations/actions/runs/38311471819) exposed Windows/macOS test assertions comparing lexical temporary paths to the intentionally canonical origin and protection paths.

Independent spec review also reproduced malformed ZIP central extra-field framing, invalid recognized PAX numeric records, and ignored global PAX semantics. In particular, a global size of two overriding a one-byte legacy header silently lost a byte. Commit `83e0e310810aabdab45764b253a2043e7bdef9e5` validates all central extra records and recognized numeric metadata, applies global/local/base PAX precedence and deletion, and uses the effective payload offsets and sizes for extraction and quotas. Permanent regressions also cover a 513-byte effective payload followed by another record. The provenance assertions now use canonical paths and explicit selected-alias tests.

The final full native Windows run at that commit passed (`go test ./... -count=1 -v -timeout=5m`, 11.823 seconds), as did vet, build and Linux/macOS test compilation. [Repeat native CI](https://github.com/Feruum/leafrun-integrations/actions/runs/38313557702) passed all Linux, Windows and macOS jobs, including tests, vet, build, mandatory Git/OpenSSH checks and the Linux race detector. The same independent spec reviewer approved the fix after running the original regressions, additional effective-framing/precedence/quota/collision/cleanup checks, and the full existing suite against a frozen copy (6.134 seconds).

Fresh quality review found an additional cancellation issue on native Linux: opening an archive FIFO blocked before the regular-file check, and context cancellation could not release the owned temporary workspace. A bounded independent test reproduced this against `83e0e31`. The same reviewer's parser-mutation and concurrent-close checks passed on Windows and with the Linux race detector.

Commit `436baae46b89e55fdd1a4f490671eaaeb600eee1` uses nonblocking Unix acquisition and validates the opened handle before copying. Bounded subprocess regressions cover FIFO rejection, pathname substitution in both directions and cancellation after open; they recorded RED to GREEN without retaining blocked readers. Parent verification with exact Go 1.27.0 passed full Windows tests (4.069 seconds), full Linux race tests (8.007 seconds), Linux vet and build. The same spec reviewer approved the five-file acquisition fix after independent pinned-version tests. [Exact native CI for the fix](https://github.com/Feruum/leafrun-integrations/actions/runs/38316179945) passed all three platforms, including Linux race checks.

Quality re-review approved that exact commit: the original FIFO reproduction passed without an external opener; 200 FIFO rejections and 400 cancellations after open/stat showed no descriptor growth. Permanent acquisition, concurrent revocation and parser probes passed under the pinned Linux race detector (focused suite 5.831 seconds), with Linux vet also passing. The independent full native Windows suite passed (5.897 seconds), as did vet. The separate core native-read transition is recorded in core's verification file. Earlier supplementary local container runs used Go 1.27.1; they are additional evidence, not the pinned-version checks. Milestone delivery and acceptance remain pending.
