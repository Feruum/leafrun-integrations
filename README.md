# Leafrun integrations

Acquire a project tree from an external source without depending on a document engine. M02 implements [LEA-15 and LEA-16](https://linear.app/bulnik/issue/LEA-12) as a Go 1.27.0 module. The document core owns immutable snapshots and rendering; this module returns standard `io/fs` data, provenance and cleanup.

## Development

Work takes place in `feature/lea-12-source-adapters`. The first release is pending review; the feature does not create OAuth connections or synchronize local changes back to a service.

The archive and Git adapters have passed independent spec and quality review and native Linux, Windows and macOS CI. Shared core rendering integration and milestone delivery remain pending; see the verification record for the current status.

```sh
git clone https://github.com/Feruum/leafrun-integrations.git
cd leafrun-integrations
git switch feature/lea-12-source-adapters
GOWORK=off go test ./... -count=1 -v
GOWORK=off go vet ./...
GOWORK=off go build ./...
# Linux additionally:
GOWORK=off go test -race ./... -count=1
```

In PowerShell, set `$env:GOWORK = 'off'` before those Go commands. Native Git and OpenSSH are mandatory tools in the milestone CI. Tests run on Linux, Windows and macOS; Linux also runs the race detector. See [the verification record](docs/m02-verification.md) for completed checks and pending acceptance areas.

The Git adapter's Linux process exit watches require kernel 5.3 or newer with pidfd support. An unavailable native capability returns an error before starting a child. Folder and archive capture do not require these Git process watches.

## Archive API

`ArchiveSource{Path: "./invoice.zip"}` and `ArchiveSource{Path: "./invoice.tar.gz", SourceRoot: "invoice"}` return a retained project tree through `Open(context.Context)`. The source root defaults to `.`. Read the returned `FS` while the tree remains open, then call its idempotent `Close` and handle cleanup errors.

ZIP and TAR.GZ extraction accepts regular files and directories. Unsafe paths, links, special records, duplicates, case collisions, corrupt or incomplete streams and exceeded limits cause failure and owned cleanup. The whole archive is validated before selecting a subtree. ZIP supports stored and DEFLATE entries; TAR.GZ supports one complete gzip member and rejects additional compressed members or trailing bytes. The original archive remains an external input; its accepted compressed bytes determine `Origin.ArchiveSHA256`.

Default limits are 10,000 regular files, 512 MiB expanded total and 128 MiB per file. TAR.GZ's total includes tar headers, metadata and padding. Go callers can override these through `ArchiveOptions.Limits`; negative limits are invalid. See [the source contract](docs/source-contract.md) for a complete usage example, ownership, options and typed errors.

## Git API

`GitSource{URL: repository, Ref: revision}` selects a required unambiguous branch, tag or full commit. HTTPS, SSH/scp-style locations and local repositories use installed native Git. `SourceRoot` selects a committed subdirectory; `GitOptions` configures temporary storage, extraction limits and an optional existing OpenSSH configuration file.

The adapter reads raw committed objects without a checkout, hooks or filters. It rejects submodules, LFS pointers, links and unsafe names. Private access uses existing credential helpers and OpenSSH configuration/agents with interaction disabled and verification enabled. Native process causes remain inspectable, with credential-bearing diagnostics sanitized. Sources retain original native protection identities for the renderer, including archives and local repository files; these guards are separate from project content.

## Architecture and delivery

This module never imports [leafrun-document-core](https://github.com/leafrun/leafrun-document-core). Core's CLI maps provider metadata into its `ProjectSource` contract and passes every provider to the same snapshot renderer. Development and CI use a temporary workspace with exact feature commits; neither a workspace nor local replacement is shipped.

Changes use English and git flow; see [CONTRIBUTING.md](CONTRIBUTING.md). Draft pull requests target upstream `develop` when it exists. Merge and release wait for review. Release integrations `v0.1.0` before pinning that version in core and checking core with `GOWORK=off`.

Cloud storage, OAuth, bidirectional synchronization and collaboration remain later work. The [public roadmap](https://github.com/leafrun/leafrun/blob/main/docs/roadmap.md) and [repository map](https://github.com/leafrun/leafrun/blob/main/docs/repositories.md) explain the broader build order.

No project license has been selected yet.
