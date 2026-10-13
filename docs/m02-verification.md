# M02 source adapter verification

Standalone adapters are implemented and independently reviewed on `feature/lea-12-source-adapters`. Shared core integration and milestone delivery remain in progress. Evidence is added only after the corresponding checks run. The adapters are independent of `leafrun-document-core`; they return standard `io/fs` trees for core to capture.

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
| Git revision | Required unambiguous ref; full resolved commit; movement cannot alter accepted inputs | Branch/tag/full SHA-1 and SHA-256 cases passed; both review stages approved `e28c912` |
| Git objects | Raw tree/blobs without checkout/hooks/filters; submodules and LFS rejected | Raw committed inputs and unsupported object regressions passed, including changed/missing working files and bare repositories |
| HTTPS authentication | Native configured helper, local TLS fixture, temporary credentials, no prompts | Real local native helper/TLS/authentication fixtures passed; effective transport rewrites and TLS enforcement independently reviewed |
| SSH authentication | Native OpenSSH, temporary keys/known hosts, auth and host-key failures | Real OpenSSH fixtures passed with temporary keys, authentication rejection and unknown/changed host-key checks |
| Diagnostics | Transport credentials redacted; original process cause available through Unwrap | Redaction and native `exec.ExitError` cause checks passed in permanent and independent tests |
| Git cancellation | Entire process tree terminated and descriptors closed before owned cleanup | Original and late-spawned Windows descendants, Unix non-pipe descendants and retained leader/identity barriers passed; both reviews approved `e28c912` |
| Native CI | Tests, vet, build on Linux/Windows/macOS; Linux race detector | Archive phase passed at `436baae`; exact final Git phase passed at `e28c912` |

## Delivery

Prepare a draft pull request into upstream `develop` when that branch exists. Keep the related core feature stacked on M01 and test both exact feature commits in a temporary workspace. Do not ship a workspace or local replacement, merge, release, or mark Linear Done before review and acceptance. After review, release integrations `v0.1.0` before pinning and testing the core dependency with `GOWORK=off`.

## Archive implementation and review

Commit `44ae720b873cc63eaf1d2abb4215ec91dbc9b556` introduced the standalone contracts and ZIP/TAR.GZ adapters. Native Windows tests, vet and build passed, as did Linux amd64/macOS arm64 test compilation. [Initial native CI](https://github.com/Feruum/leafrun-integrations/actions/runs/38311471819) exposed Windows/macOS test assertions comparing lexical temporary paths to the intentionally canonical origin and protection paths.

Independent spec review also reproduced malformed ZIP central extra-field framing, invalid recognized PAX numeric records, and ignored global PAX semantics. In particular, a global size of two overriding a one-byte legacy header silently lost a byte. Commit `83e0e310810aabdab45764b253a2043e7bdef9e5` validates all central extra records and recognized numeric metadata, applies global/local/base PAX precedence and deletion, and uses the effective payload offsets and sizes for extraction and quotas. Permanent regressions also cover a 513-byte effective payload followed by another record. The provenance assertions now use canonical paths and explicit selected-alias tests.

The final full native Windows run at that commit passed (`go test ./... -count=1 -v -timeout=5m`, 11.823 seconds), as did vet, build and Linux/macOS test compilation. [Repeat native CI](https://github.com/Feruum/leafrun-integrations/actions/runs/38313557702) passed all Linux, Windows and macOS jobs, including tests, vet, build, mandatory Git/OpenSSH checks and the Linux race detector. The same independent spec reviewer approved the fix after running the original regressions, additional effective-framing/precedence/quota/collision/cleanup checks, and the full existing suite against a frozen copy (6.134 seconds).

Fresh quality review found an additional cancellation issue on native Linux: opening an archive FIFO blocked before the regular-file check, and context cancellation could not release the owned temporary workspace. A bounded independent test reproduced this against `83e0e31`. The same reviewer's parser-mutation and concurrent-close checks passed on Windows and with the Linux race detector.

Commit `436baae46b89e55fdd1a4f490671eaaeb600eee1` uses nonblocking Unix acquisition and validates the opened handle before copying. Bounded subprocess regressions cover FIFO rejection, pathname substitution in both directions and cancellation after open; they recorded RED to GREEN without retaining blocked readers. Parent verification with exact Go 1.27.0 passed full Windows tests (4.069 seconds), full Linux race tests (8.007 seconds), Linux vet and build. The same spec reviewer approved the five-file acquisition fix after independent pinned-version tests. [Exact native CI for the fix](https://github.com/Feruum/leafrun-integrations/actions/runs/38316179945) passed all three platforms, including Linux race checks.

Quality re-review approved that exact commit: the original FIFO reproduction passed without an external opener; 200 FIFO rejections and 400 cancellations after open/stat showed no descriptor growth. Permanent acquisition, concurrent revocation and parser probes passed under the pinned Linux race detector (focused suite 5.831 seconds), with Linux vet also passing. The independent full native Windows suite passed (5.897 seconds), as did vet. The separate core native-read transition is recorded in core's verification file. Earlier supplementary local container runs used Go 1.27.1; they are additional evidence, not the pinned-version checks. Milestone delivery and acceptance remain pending.

## Git implementation and review

Commit `c0378fde04e21843cf30c2b803c478803b1353c8` introduced native Git acquisition, real local private HTTPS/OpenSSH fixtures, process-tree handling and eager publication guard metadata. Local pinned Windows checks and Linux race checks passed. Independent spec review then reproduced two gaps: destination-scoped TLS verification could be disabled after an `insteadOf` rewrite, and a linked worktree's ordinary `.git` indirection file lacked a frozen native guard.

[Native CI at that production tree](https://github.com/Feruum/leafrun-integrations/actions/runs/38320622119) also failed Windows/macOS guard path assertions and Windows descendant exit checks. The guard assertions compared canonical paths with lexical temporary aliases. Retaining original Windows process handles independently reproduced a production teardown gap: Job Object active counts could reach zero before those handles signaled actual exit.

Fix `8f77960a1b6f82a79508dbb3aed827954c4022ca` validates and freezes the effective native transport, enforces TLS at that destination, retains linked-worktree `.git`-file identity and strengthens Windows teardown. It verifies owned process/thread identities, suspends and rescans until quiescent, terminates the job and waits every retained original process handle before descriptors and owned storage are released. Permanent regressions cover trusted and unsafe rewrites, native config changes between ref resolution and fetch, canonical temporary aliases, retained original children and late-spawned descendants.

The implementer's full Windows tests passed in 231.977 seconds, with vet/build exit 0. The pinned Linux Go 1.27.0 race suite passed in 38.853 seconds, with vet/build exit 0. Darwin and Windows test cross-compilation/build passed. Parent independently reran the complete pinned Linux race suite (36.764 seconds), vet and build successfully.

The same independent spec reviewer approved the exact fix after both original scratch reproductions passed on Windows and Linux, Windows scoped native/rewrite/guard/late-descendant checks passed (40.569 seconds), and full pinned Linux race checks (36.559 seconds), vet and build passed. [Exact native CI](https://github.com/Feruum/leafrun-integrations/actions/runs/38322956308) passed all Linux, Windows and macOS jobs (1 minute 37 seconds), including mandatory native tools and Linux race checks.

Fresh quality review then independently reproduced two remaining gaps at that head: Unix group termination could return while non-pipe-holding descendants were still alive, and stripping whitespace from native `--show-toplevel` output corrupted a legal local repository root ending in a space. Bounded native Linux reproductions are available; fixes and independent rechecks are in progress. Hosted success does not constitute final quality approval or milestone acceptance.

Commit `32ef02a38ad3a4c385a6df788fc7c078c79f9627` preserves native path whitespace and adds exit barriers for the original Unix process group. The original leader remains waitable until group teardown completes, preventing reuse of its group identifier during signaling. Linux uses pidfds and requires kernel 5.3 or newer; macOS uses kqueue exit notifications. The library does not install a global subreaper or change signal handling. Full pinned local Windows tests (219.603 seconds), Linux race tests (59.828 seconds), vet/build and cross-compilation passed. Parent pinned Linux race verification passed independently (57.855 seconds).

[Exact native CI for that change](https://github.com/Feruum/leafrun-integrations/actions/runs/38326260663) passed Windows and Linux but exposed macOS native process-boundary failures. Test-only commits `0bc4730` and `6fd0aab` add deterministic regressions before the fix. [Native RED evidence](https://github.com/Feruum/leafrun-integrations/actions/runs/38327240744) confirms that an absent macOS process is reported as EIO, an exit-watch registration racing the same original process's exit returns ESRCH, and signaling a zombie-only group returns EPERM. The final native fix and both independent review approvals remain pending. These failed probes are evidence of the defect, not an acceptance claim.

Scoped fix `e28c912c49420900c341638848a3ff160e3e2179` distinguishes a native zero-record process response from an I/O failure, revalidates the original birth/group identity after exit-watch registration races, and avoids signaling completed groups. The macOS zombie-only signaling race is accepted only after a fresh group snapshot proves no live member remains. Original exit barriers, late-member recapture, cleanup failures and native error causes remain intact; mismatched identities and mixed native failures have permanent regressions. Full local pinned Windows tests passed (215.849 seconds), with vet/build exit 0. Full pinned Linux race tests passed (58.137 seconds), with vet/build exit 0. Darwin test compilation, vet/build and Windows cross-build passed. Exact native macOS verification and both review rechecks remain required.

Parent independently reran the entire pinned Linux race suite against that exact production head (56.452 seconds), followed by vet and build; all exited successfully. The feature head is published for native three-platform verification.

[Exact native CI at `e28c912`](https://github.com/Feruum/leafrun-integrations/actions/runs/38328561966) passed Linux, Windows and macOS in 2 minutes 2 seconds, including the actual macOS regressions, mandatory Git/OpenSSH checks and the Linux race detector. Independent spec and quality rechecks still precede final acceptance.

The same spec reviewer approved `e28c912` after its unchanged original descendant/trailing-space probes and native authentication, rewrite, frozen-guard, cause-preservation and cleanup cases passed independently under pinned Linux race checks (58.637 seconds). Quality re-review is in progress; milestone delivery and acceptance remain pending.

The same quality reviewer approved that exact head after copying its two original scratch reproductions unchanged and confirming their hashes. The trailing-space and 15-round, 32-descendant probes passed. Independent pinned Linux race tests covering those boundaries and native TLS/SSH, frozen rewrites, guards and process teardown passed (43.645 seconds); Windows native lifetime checks passed (5.491 seconds). The full final process-control changes were reviewed, with no remaining actionable findings. Shared core integration, user review and release are separate milestone gates.
