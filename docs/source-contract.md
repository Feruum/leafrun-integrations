# Source contract

This module acquires a project as standard `io/fs` data. It does not import the document core or run Typst. Core's CLI maps the independent source metadata into its own `ProjectSource` contract before creating a snapshot.

## Ownership

`Source.Open(context.Context)` returns a `SourceTree` rooted at `.` in the selected project directory. A successful tree owns its acquired copies until `Close` is called. Close is idempotent, revokes open files and releases only data owned by that operation. Original archives and repositories are not deleted. Keep the tree open for the entire read or capture.

`Origin` describes the source separately from content identity. It contains the source kind, location and selected source root, plus the requested Git ref and resolved commit or archive SHA-256 when applicable. `ProtectedPaths` records original native inputs that the caller must protect from output publication.

`NativeProtection` retains a canonical absolute `Path` and an eager native `fs.FileInfo` identity. These are publication guards, separate from exported project bytes and content validation. Ordinary directory guards protect the complete native directory; ordinary file identities also protect hardlink aliases after the original path is replaced or removed. The adapter closes its native handles before returning; the retained identity does not grant write access. Callers must preserve these guards when bridging to a renderer, without substituting later reopened paths. The archive guard describes the actual opened file whose accepted bytes supplied the archive digest. Local Git guards protect repository metadata and existing selected working files without requiring the working tree to mirror the resolved commit.

## Archives

```go
source := integrations.ArchiveSource{
    Path:       "./invoice.tar.gz",
    SourceRoot: "invoice",
    Options: integrations.ArchiveOptions{
        Limits: integrations.Limits{
            MaxFiles:      10_000,
            MaxTotalBytes: 512 << 20,
            MaxFileBytes:  128 << 20,
        },
    },
}
tree, err := source.Open(ctx)
if err != nil {
    return err
}
data, readErr := fs.ReadFile(tree.FS, "main.typ")
if err := errors.Join(readErr, tree.Close()); err != nil {
    return err
}
_ = data
```

The example uses `context`, `errors`, `io/fs` and the `github.com/leafrun/leafrun-integrations` package. When using core, let `CreateSnapshot` own the mapped tree's close callback instead of closing it before capture finishes.

`SourceRoot` defaults to `.`. It selects an existing directory after the complete archive has been validated, including entries outside the selected subtree. ZIP and TAR.GZ accept regular files and directories. Links and special entries are rejected. ZIP supports stored and DEFLATE entries. TAR.GZ supports one complete gzip member; additional members and compressed trailing bytes are rejected.

Archive bytes are copied into operation-owned storage first. Parsing and `ArchiveSHA256` use that same accepted compressed copy. Extraction rejects unsafe portable names, traversal, absolute/drive/UNC paths, duplicates and case collisions. Checksums, complete reads and actual byte quotas are mandatory.

Each zero limit selects the default: 10,000 regular files, 512 MiB expanded total and 128 MiB per file. Negative limits are invalid. TAR.GZ's total quota includes headers, metadata, padding and trailing decompressed bytes as well as file contents. `TempDir` selects an existing temporary parent; its explicitly selected aliases are resolved before allocating owned storage.

## Git

`GitSource{URL: repository, Ref: revision}` acquires a local repository, HTTPS URL or SSH URL/scp-style location. `Ref` is required and selects an unambiguous branch, tag or full commit. `SourceRoot` defaults to `.`. `GitOptions` supplies temporary storage, the same configurable extraction limits and an optional existing `SSHConfig` path.

```go
source := integrations.GitSource{
    URL:        repository,
    Ref:        revision,
    SourceRoot: "invoice",
}
tree, err := source.Open(ctx)
if err != nil {
    return err
}
commit := tree.Origin.Revision
data, readErr := fs.ReadFile(tree.FS, "main.typ")
if err := errors.Join(readErr, tree.Close()); err != nil {
    return err
}
_, _ = commit, data
```

Here `repository` and `revision` are caller-supplied strings. A successful origin retains the requested ref and complete resolved commit; moving the ref later does not change the retained tree.

The adapter resolves a named ref once, fetches that exact object and exports raw committed trees and blobs with native `ls-tree` and `cat-file`. It does not create a working checkout or apply hooks/filters. Submodules, LFS pointers, links and unsafe portable tree names are unsupported. Original ref and resolved full commit remain in provenance. A bare repository and missing or changed working files do not become project content dependencies.

Private HTTPS uses the configured native credential helper. SSH uses installed OpenSSH with its configured identities/agent and known hosts; `SSHConfig` can explicitly select an existing configuration file. Interactive prompts, diagnostic credential tracing and embedded HTTPS credentials are rejected or disabled. TLS and SSH host verification remain enabled. Error output, including wrapped native process stderr, is sanitized while the original process cause remains available through `Unwrap`.

Native `insteadOf` rewrites are expanded once without contacting the server. The effective location is validated and frozen for the operation, and HTTPS verification is enforced at that destination. Later native configuration changes cannot switch the accepted transport between resolving the ref and fetching the commit.

Cancellation terminates operation-owned process trees, waits for process/descriptor completion and then releases owned storage. On Windows, a child is created suspended and assigned to its Job Object before execution. Teardown verifies owned process and thread identities, suspends/rescans until the job is quiescent, then terminates it and waits on retained original process handles. Unix uses a process group. Transport tests use temporary local servers, credentials, keys and trust files rather than real account secrets. See the verification record for review and test status.

Linux Git execution requires kernel 5.3+ pidfds for identity-bound exit watches; unavailable support fails before a child starts. macOS uses native kqueue exit events. These watches await members even when descendants do not inherit output pipes, and do not install a subreaper or change the application's global signal behavior. The Linux regression harness uses a disposable test-only subreaper to reap its own fixture descendants.

## Errors

`SourceError.Kind` identifies options, archive, unsafe path, unsupported entry, limit, source-root, input or cleanup failures. `Unwrap` preserves the original cause. Use `errors.Is` for the category sentinels and context/checksum/filesystem causes. A failed acquisition includes any mandatory cleanup failure in its error; it does not return a usable partial tree.

See [the verification record](m02-verification.md) for evidence and remaining milestone checks.
