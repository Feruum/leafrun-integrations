package integrations

import (
	"archive/zip"
	"bufio"
	"compress/flate"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// ArchiveOptions controls extraction of ZIP and TAR.GZ sources.
type ArchiveOptions struct {
	TempDir string
	Limits  Limits
}

// ArchiveSource acquires ZIP or TAR.GZ bytes from Path into owned storage.
// SourceRoot defaults to "." and must name an existing ordinary directory.
type ArchiveSource struct {
	Path       string
	SourceRoot string
	Options    ArchiveOptions
}

func (s ArchiveSource) Open(ctx context.Context) (tree SourceTree, err error) {
	if err = ctx.Err(); err != nil {
		return SourceTree{}, err
	}
	limits, err := s.Options.Limits.normalized()
	if err != nil {
		return SourceTree{}, err
	}
	sourceRoot := s.SourceRoot
	if sourceRoot == "" {
		sourceRoot = "."
	}
	if _, err = portablePath(sourceRoot, true, false); err != nil {
		return SourceTree{}, sourceErr(ErrorSourceRoot, "select", sourceRoot, err)
	}
	w, err := newOwnedWorkspace(ctx, s.Options.TempDir)
	if err != nil {
		return SourceTree{}, sourceErr(ErrorInput, "allocate", s.Options.TempDir, err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, w.close())
			tree = SourceTree{}
		}
	}()
	location, digest, err := captureArchive(ctx, w, s.Path)
	if err != nil {
		return SourceTree{}, err
	}
	if err = w.root.Mkdir("tree", 0700); err != nil {
		return SourceTree{}, sourceErr(ErrorInput, "create tree", "", err)
	}
	root, err := w.root.OpenRoot("tree")
	if err != nil {
		return SourceTree{}, sourceErr(ErrorInput, "open tree", "", err)
	}
	kind, extractErr := extractArchive(ctx, w, root, limits)
	err = errors.Join(extractErr, root.Close())
	if err != nil {
		return SourceTree{}, err
	}
	selected := path.Join("tree", sourceRoot)
	info, err := w.root.Lstat(selected)
	if err != nil {
		return SourceTree{}, sourceErr(ErrorSourceRoot, "select", sourceRoot, err)
	}
	if !info.IsDir() || info.Mode().Type() != fs.ModeDir {
		return SourceTree{}, sourceErr(ErrorSourceRoot, "select", sourceRoot, errors.New("source root is not an ordinary directory"))
	}
	filesystem, err := w.tree(selected)
	if err != nil {
		return SourceTree{}, sourceErr(ErrorSourceRoot, "open", sourceRoot, err)
	}
	if err = ctx.Err(); err != nil {
		return SourceTree{}, err
	}
	return SourceTree{FS: filesystem, Origin: SourceOrigin{Kind: kind, Location: location, SourceRoot: sourceRoot, ArchiveSHA256: digest}, ProtectedPaths: []string{location}, Close: w.close}, nil
}

func sourceErr(kind ErrorKind, op, name string, err error) error {
	return &SourceError{Kind: kind, Op: op, Path: name, Err: err}
}

func (l Limits) normalized() (Limits, error) {
	if l.MaxFiles < 0 || l.MaxTotalBytes < 0 || l.MaxFileBytes < 0 {
		return Limits{}, sourceErr(ErrorOptions, "limits", "", errors.New("limits must not be negative"))
	}
	if l.MaxFiles == 0 {
		l.MaxFiles = DefaultMaxFiles
	}
	if l.MaxTotalBytes == 0 {
		l.MaxTotalBytes = DefaultMaxTotalBytes
	}
	if l.MaxFileBytes == 0 {
		l.MaxFileBytes = DefaultMaxFileBytes
	}
	return l, nil
}

// copyBounded checks actual bytes and cancellation before each write. Reading one
// byte beyond the remaining allowance detects excess without writing it.
func copyBounded(ctx context.Context, dst io.Writer, src io.Reader, limit int64) (int64, error) {
	buffer := make([]byte, 32<<10)
	var total int64
	emptyReads := 0
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		size := int64(len(buffer))
		remaining := limit - total
		if remaining < size {
			size = remaining + 1
		}
		n, readErr := src.Read(buffer[:size])
		if n < 0 || n > int(size) {
			return total, errors.New("invalid reader byte count")
		}
		if int64(n) > remaining {
			return total, sourceErr(ErrorLimit, "read", "", errors.New("expanded byte allowance exceeded"))
		}
		if err := ctx.Err(); err != nil {
			return total, err
		}
		if n > 0 {
			emptyReads = 0
			written, writeErr := dst.Write(buffer[:n])
			if written < 0 || written > n {
				return total, errors.New("invalid writer byte count")
			}
			total += int64(written)
			if writeErr != nil {
				return total, writeErr
			}
			if written != n {
				return total, io.ErrShortWrite
			}
		} else {
			emptyReads++
			if emptyReads >= 100 && readErr == nil {
				return total, io.ErrNoProgress
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return total, nil
			}
			return total, readErr
		}
	}
}

// captureArchive hashes the same owned bytes that are later parsed. It never
// reopens the external archive between hashing and extraction.
func captureArchive(ctx context.Context, w *ownedWorkspace, original string) (location, digest string, err error) {
	absolute, err := filepath.Abs(original)
	if err != nil {
		return "", "", sourceErr(ErrorInput, "resolve", original, err)
	}
	location, err = filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", "", sourceErr(ErrorInput, "resolve", original, err)
	}
	input, err := openArchiveInput(ctx, location)
	if err != nil {
		return "", "", sourceErr(ErrorInput, "open", location, err)
	}
	defer func() { err = errors.Join(err, input.Close()) }()
	output, err := w.root.OpenFile("archive", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", "", sourceErr(ErrorInput, "capture", location, err)
	}
	defer func() { err = errors.Join(err, output.Close()) }()
	hash := sha256.New()
	if _, err = copyBounded(ctx, io.MultiWriter(output, hash), input, math.MaxInt64); err != nil {
		return "", "", sourceErr(ErrorInput, "capture", location, err)
	}
	return location, hex.EncodeToString(hash.Sum(nil)), nil
}

func extractArchive(ctx context.Context, w *ownedWorkspace, root *os.Root, limits Limits) (kind string, err error) {
	file, err := w.root.Open("archive")
	if err != nil {
		return "", sourceErr(ErrorInput, "open capture", "", err)
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	var signature [4]byte
	if _, err = file.ReadAt(signature[:], 0); err != nil {
		return "", sourceErr(ErrorArchive, "identify", "", err)
	}
	switch {
	case signature[0] == 'P' && signature[1] == 'K':
		return "zip", extractZIP(ctx, file, root, limits)
	case signature[0] == 0x1f && signature[1] == 0x8b:
		return "tar.gz", extractTARGZIP(ctx, file, w, root, limits)
	default:
		return "", sourceErr(ErrorArchive, "identify", "", errors.New("expected ZIP or TAR.GZ data"))
	}
}

type preparedEntry struct {
	name      string
	directory bool
	size      int64
}

func prepareEntry(registry pathRegistry, name string, directory bool, size int64, count *int64, limits Limits) (preparedEntry, error) {
	clean, err := portablePath(name, directory, true)
	if err != nil {
		return preparedEntry{}, sourceErr(ErrorUnsafePath, "validate", name, err)
	}
	if err := registry.add(clean, directory); err != nil {
		return preparedEntry{}, sourceErr(ErrorUnsafePath, "validate", name, err)
	}
	if size < 0 {
		return preparedEntry{}, sourceErr(ErrorArchive, "size", name, errors.New("negative declared size"))
	}
	if directory && size != 0 {
		return preparedEntry{}, sourceErr(ErrorUnsupportedEntry, "validate", name, errors.New("directory has a data payload"))
	}
	if !directory {
		if *count >= limits.MaxFiles {
			return preparedEntry{}, sourceErr(ErrorLimit, "count", name, errors.New("regular file count exceeded"))
		}
		*count++
		if size > limits.MaxFileBytes || size > limits.MaxTotalBytes {
			return preparedEntry{}, sourceErr(ErrorLimit, "size", name, errors.New("declared file size exceeded"))
		}
	}
	return preparedEntry{name: clean, directory: directory, size: size}, nil
}

type treeWriter struct {
	root        *os.Root
	directories map[string]bool
}

func (w *treeWriter) writeEntry(ctx context.Context, entry preparedEntry, reader io.Reader, allowance int64) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if entry.directory {
		if err := w.ensureDirectories(ctx, entry.name); err != nil {
			return 0, err
		}
		return copyBounded(ctx, io.Discard, reader, 0)
	}
	if err := w.ensureDirectories(ctx, path.Dir(entry.name)); err != nil {
		return 0, err
	}
	file, err := w.root.OpenFile(entry.name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return 0, err
	}
	written, copyErr := copyBounded(ctx, file, reader, allowance)
	return written, errors.Join(copyErr, file.Close())
}

// Exact logical ancestors may be reused. Every other directory is created
// exclusively so native Unicode normalization or short-name aliases cannot
// silently merge different archive prefixes.
func (w *treeWriter) ensureDirectories(ctx context.Context, name string) error {
	if w.directories == nil {
		w.directories = map[string]bool{".": true}
	}
	if name == "." {
		return nil
	}
	var current string
	for _, component := range strings.Split(name, "/") {
		if err := ctx.Err(); err != nil {
			return err
		}
		current = path.Join(current, component)
		if w.directories[current] {
			continue
		}
		if err := w.root.Mkdir(current, 0700); err != nil {
			return err
		}
		w.directories[current] = true
	}
	return nil
}

func extractZIP(ctx context.Context, file *os.File, root *os.Root, limits Limits) error {
	info, err := file.Stat()
	if err != nil {
		return sourceErr(ErrorArchive, "stat", "", err)
	}
	reader, err := zip.NewReader(file, info.Size())
	if err != nil && !(errors.Is(err, zip.ErrInsecurePath) && reader != nil) {
		return sourceErr(ErrorArchive, "open ZIP", "", err)
	}
	registry := make(pathRegistry)
	prepared := make([]preparedEntry, 0, len(reader.File))
	var count int64
	for _, entry := range reader.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		mode := entry.Mode()
		if (mode.Type() != 0 && mode.Type() != fs.ModeDir) || entry.ExternalAttrs&0x400 != 0 || entry.Flags&1 != 0 {
			return sourceErr(ErrorUnsupportedEntry, "validate ZIP", entry.Name, errors.New("only ordinary files and directories are supported"))
		}
		if entry.UncompressedSize64 > math.MaxInt64 {
			return sourceErr(ErrorLimit, "size", entry.Name, errors.New("ZIP size overflows int64"))
		}
		validated, err := prepareEntry(registry, entry.Name, mode.IsDir(), int64(entry.UncompressedSize64), &count, limits)
		if err != nil {
			return err
		}
		prepared = append(prepared, validated)
	}
	if err := validateZIPLayout(ctx, file, info.Size(), reader); err != nil {
		return err
	}
	reader.RegisterDecompressor(zip.Deflate, func(raw io.Reader) io.ReadCloser {
		buffered := bufio.NewReader(raw)
		return &completeDeflateReader{ReadCloser: flate.NewReader(buffered), buffered: buffered}
	})
	var total int64
	writer := treeWriter{root: root}
	for i, entry := range reader.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		// zip.File.Open skips all stream/CRC checks for names ending in '/'.
		// Preserve the header and offsets but open directory streams as files.
		checked := *entry
		checked.Name = strings.TrimSuffix(checked.Name, "/")
		stream, err := checked.Open()
		if err != nil {
			return sourceErr(ErrorArchive, "open ZIP entry", entry.Name, err)
		}
		allowance := min(limits.MaxFileBytes, limits.MaxTotalBytes-total)
		written, copyErr := writer.writeEntry(ctx, prepared[i], stream, allowance)
		closeErr := stream.Close()
		if cause := errors.Join(copyErr, closeErr); cause != nil {
			return sourceErr(ErrorArchive, "extract ZIP", entry.Name, cause)
		}
		if written != prepared[i].size {
			return sourceErr(ErrorArchive, "extract ZIP", entry.Name, fmt.Errorf("read %d bytes; header declared %d", written, prepared[i].size))
		}
		total += written
	}
	return nil
}

type completeDeflateReader struct {
	io.ReadCloser
	buffered  *bufio.Reader
	finishErr error
}

func (r *completeDeflateReader) Read(data []byte) (int, error) {
	if r.finishErr != nil {
		return 0, r.finishErr
	}
	n, err := r.ReadCloser.Read(data)
	if errors.Is(err, io.EOF) {
		if _, tailErr := r.buffered.ReadByte(); !errors.Is(tailErr, io.EOF) {
			if tailErr == nil {
				tailErr = zip.ErrFormat
			}
			err = tailErr
		}
		r.finishErr = err
	}
	return n, err
}
