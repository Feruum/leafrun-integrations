# Source contract

This module acquires a project as standard `io/fs` data. It does not import the document core or run Typst. Core's CLI maps the independent source metadata into its own `ProjectSource` contract before creating a snapshot.

## Ownership

`Source.Open(context.Context)` returns a `SourceTree` rooted at `.` in the selected project directory. A successful tree owns its acquired copies until `Close` is called. Close is idempotent, revokes open files and releases only data owned by that operation. Original archives and repositories are not deleted. Keep the tree open for the entire read or capture.

`Origin` describes the source separately from content identity. It contains the source kind, location and selected source root, plus the requested Git ref and resolved commit or archive SHA-256 when applicable. `ProtectedPaths` records original native inputs that the caller must protect from output publication.

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

## Errors

`SourceError.Kind` identifies options, archive, unsafe path, unsupported entry, limit, source-root, input or cleanup failures. `Unwrap` preserves the original cause. Use `errors.Is` for the category sentinels and context/checksum/filesystem causes. A failed acquisition includes any mandatory cleanup failure in its error; it does not return a usable partial tree.

See [the verification record](m02-verification.md) for evidence and remaining milestone checks.
