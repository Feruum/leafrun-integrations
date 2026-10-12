package integrations

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// GitOptions controls native Git acquisition. SSHConfig optionally selects an
// existing OpenSSH configuration file; its keys remain native OpenSSH inputs.
type GitOptions struct {
	TempDir   string
	Limits    Limits
	SSHConfig string
}

// GitSource acquires a branch, tag or full commit from HTTPS, SSH or a local
// repository. Ref is required. SourceRoot defaults to an existing "." directory.
// Open resolves a named ref once and exports raw blobs from that exact commit.
type GitSource struct {
	URL        string
	Ref        string
	SourceRoot string
	Options    GitOptions
}

func (s GitSource) Open(ctx context.Context) (tree SourceTree, err error) {
	if err = ctx.Err(); err != nil {
		return SourceTree{}, err
	}
	limits, err := s.Options.Limits.normalized()
	if err != nil {
		return SourceTree{}, err
	}
	if err = validateGitRef(s.Ref); err != nil {
		return SourceTree{}, sourceErr(ErrorOptions, "ref", s.Ref, err)
	}
	location, local, err := gitLocation(s.URL)
	if err != nil {
		return SourceTree{}, sourceErr(ErrorOptions, "repository", redactTransport(s.URL), err)
	}
	selected := s.SourceRoot
	if selected == "" {
		selected = "."
	}
	if _, err = portablePath(selected, true, false); err != nil {
		return SourceTree{}, sourceErr(ErrorSourceRoot, "select", selected, err)
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
	runner, err := newGitRunner(w, s.Options)
	if err != nil {
		return SourceTree{}, sourceErr(ErrorInput, "native git", location, err)
	}
	var protection []NativeProtection
	var workingDirectory string
	if local {
		protection, workingDirectory, err = protectLocalGit(ctx, runner, location)
		if err != nil {
			return SourceTree{}, err
		}
	}
	commit, err := resolveGitRef(ctx, runner, location, s.Ref)
	if err != nil {
		return SourceTree{}, err
	}
	if err = acquireGitCommit(ctx, runner, location, commit); err != nil {
		return SourceTree{}, err
	}
	if err = w.root.Mkdir("tree", 0700); err != nil {
		return SourceTree{}, err
	}
	root, err := w.root.OpenRoot("tree")
	if err != nil {
		return SourceTree{}, err
	}
	entries, extractErr := exportGitTree(ctx, runner, root, commit, limits)
	if err = errors.Join(extractErr, root.Close()); err != nil {
		return SourceTree{}, err
	}
	info, err := w.root.Lstat(path.Join("tree", selected))
	if err != nil {
		return SourceTree{}, sourceErr(ErrorSourceRoot, "select", selected, err)
	}
	if !info.IsDir() || info.Mode().Type() != fs.ModeDir {
		return SourceTree{}, sourceErr(ErrorSourceRoot, "select", selected, errors.New("source root is not an ordinary directory"))
	}
	filesystem, err := w.tree(path.Join("tree", selected))
	if err != nil {
		return SourceTree{}, err
	}
	if workingDirectory != "" {
		protection = append(protection, protectWorkingGitFiles(ctx, workingDirectory, selected, entries)...)
	}
	if err = ctx.Err(); err != nil {
		return SourceTree{}, err
	}
	return SourceTree{FS: filesystem, Origin: SourceOrigin{Kind: "git", Location: location, SourceRoot: selected, Ref: s.Ref, Revision: commit}, NativeProtection: protection, Close: w.close}, nil
}

func validOID(oid string) bool {
	if len(oid) != 40 && len(oid) != 64 {
		return false
	}
	_, err := hex.DecodeString(oid)
	return err == nil
}

func validateGitRef(ref string) error {
	if validOID(ref) {
		return nil
	}
	name := strings.TrimPrefix(strings.TrimPrefix(ref, "refs/heads/"), "refs/tags/")
	if ref == "" || name == "" || strings.HasPrefix(ref, "-") || strings.HasPrefix(name, "-") || ref == "HEAD" || ref == "@" || strings.HasPrefix(ref, "refs/") && name == ref {
		return errors.New("expected a branch, tag or full commit ID")
	}
	if strings.ContainsAny(ref, " ~^:?*[\\\x00\r\n\t") || strings.Contains(ref, "..") || strings.Contains(ref, "@{") || strings.HasSuffix(ref, ".") || strings.HasPrefix(ref, "/") || strings.HasSuffix(ref, "/") || strings.Contains(ref, "//") {
		return errors.New("revision expressions and unsafe ref names are unsupported")
	}
	for _, component := range strings.Split(ref, "/") {
		if strings.HasPrefix(component, ".") || strings.HasSuffix(component, ".lock") {
			return errors.New("invalid ref component")
		}
	}
	for _, r := range ref {
		if r < 32 || r == 127 {
			return errors.New("control character in ref")
		}
	}
	return nil
}

func gitLocation(input string) (string, bool, error) {
	if input == "" || strings.HasPrefix(input, "-") || strings.ContainsAny(input, "\x00\r\n") {
		return "", false, errors.New("invalid repository location")
	}
	// Native absolute paths are recognized before scp syntax, including Windows.
	if !filepath.IsAbs(input) && strings.Contains(input, ":") {
		if strings.Contains(input, "://") {
			u, err := url.Parse(input)
			if err != nil {
				return "", false, errors.New("invalid repository URL")
			}
			if u.Scheme != "https" && u.Scheme != "ssh" {
				return "", false, errors.New("only HTTPS and SSH URLs are supported")
			}
			if u.Hostname() == "" || strings.HasPrefix(u.Hostname(), "-") || u.Opaque != "" || u.RawQuery != "" || u.Fragment != "" || u.Path == "" {
				return "", false, errors.New("invalid repository URL")
			}
			if u.User != nil {
				if _, exists := u.User.Password(); exists || u.Scheme == "https" {
					return "", false, errors.New("embedded HTTPS credentials or SSH passwords are unsupported")
				}
			}
			return input, false, nil
		}
		left, right, _ := strings.Cut(input, ":")
		if strings.Contains(left, "/") || strings.Contains(left, "\\") || strings.Contains(right, ":") || left == "" || right == "" || strings.HasPrefix(right, "-") || strings.ContainsAny(left, " \t") {
			return "", false, errors.New("unsafe SSH repository form")
		}
		if _, host, ok := strings.Cut(left, "@"); ok {
			left = host
		}
		if left == "" || strings.HasPrefix(left, "-") {
			return "", false, errors.New("unsafe SSH hostname")
		}
		return input, false, nil
	}
	absolute, err := filepath.Abs(input)
	if err != nil {
		return "", true, err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", true, err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", true, err
	}
	if !info.IsDir() {
		return "", true, errors.New("local repository is not a directory")
	}
	return resolved, true, nil
}

type gitRunner struct {
	binary string
	dir    string
	env    []string
	config []string
}

func newGitRunner(w *ownedWorkspace, options GitOptions) (*gitRunner, error) {
	binary, err := exec.LookPath("git")
	if err != nil {
		return nil, err
	}
	r := &gitRunner{binary: binary, dir: w.root.Name(), env: gitEnvironment(os.Environ())}
	settings := []string{"core.hooksPath=", "core.askPass=", "core.attributesFile=", "core.excludesFile=", "gc.auto=0", "maintenance.auto=false", "fetch.writeCommitGraph=false", "fetch.recurseSubmodules=false", "submodule.recurse=false", "credential.interactive=false", "credential.trace=false", "credential.traceSecrets=false", "credential.traceMsAuth=false", "credential.debug=false", "trace2.normalTarget=0", "trace2.eventTarget=0", "trace2.perfTarget=0", "http.sslVerify=true", "http.followRedirects=false", "transfer.fsckObjects=true", "fetch.fsckObjects=true", "uploadpack.packObjectsHook=", "protocol.version=2"}
	ssh, err := exec.LookPath("ssh")
	if err != nil {
		return nil, fmt.Errorf("native OpenSSH required: %w", err)
	}
	command := quoteSSHArgument(filepath.ToSlash(ssh)) + " -oBatchMode=yes -oStrictHostKeyChecking=yes -oNumberOfPasswordPrompts=0 -oLogLevel=ERROR"
	if options.SSHConfig != "" {
		config, err := filepath.Abs(options.SSHConfig)
		if err != nil {
			return nil, err
		}
		command += " -F " + quoteSSHArgument(filepath.ToSlash(config))
	}
	r.env = append(r.env, "GIT_SSH_COMMAND="+command, "GIT_SSH_VARIANT=ssh")
	for _, setting := range settings {
		r.config = append(r.config, "-c", setting)
	}
	return r, nil
}
func (r *gitRunner) command(args ...string) *exec.Cmd {
	cmd := exec.Command(r.binary, append(append([]string(nil), r.config...), args...)...)
	cmd.Dir = r.dir
	cmd.Env = r.env
	return cmd
}
func (r *gitRunner) output(ctx context.Context, args ...string) (string, error) {
	var b bytes.Buffer
	err := runNative(ctx, r.command(args...), func(reader io.Reader) error { _, err := copyBounded(ctx, &b, reader, 1<<20); return err })
	return b.String(), err
}
func (r *gitRunner) objects(args ...string) *exec.Cmd {
	return r.command(append([]string{"--git-dir=" + filepath.Join(r.dir, "objects.git")}, args...)...)
}

func (r *gitRunner) verifyTransport(location string) {
	if strings.HasPrefix(location, "https://") {
		// URL-scoped native configuration otherwise outranks http.sslVerify.
		r.config = append(r.config, "-c", "http."+location+".sslVerify=true", "-c", "http."+location+".followRedirects=false")
	}
}

func resolveGitRef(ctx context.Context, r *gitRunner, location, ref string) (string, error) {
	r.verifyTransport(location)
	patterns := []string{ref}
	if !strings.HasPrefix(ref, "refs/") {
		patterns = []string{"refs/heads/" + ref, "refs/tags/" + ref, "refs/tags/" + ref + "^{}"}
	} else if strings.HasPrefix(ref, "refs/tags/") {
		patterns = append(patterns, ref+"^{}")
	}
	data, err := r.output(ctx, append([]string{"ls-remote", "--", location}, patterns...)...)
	if err != nil {
		return "", sourceErr(ErrorInput, "resolve git ref", redactTransport(location), err)
	}
	refs := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(data), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) != 2 || !validOID(parts[0]) {
			return "", sourceErr(ErrorInput, "resolve git ref", ref, errors.New("invalid native Git ref response"))
		}
		for _, pattern := range patterns {
			if parts[1] == pattern {
				refs[pattern] = strings.ToLower(parts[0])
			}
		}
	}
	if validOID(ref) {
		if len(refs) != 0 {
			return "", sourceErr(ErrorOptions, "ref", ref, errors.New("commit ID is ambiguous with a named ref"))
		}
		return strings.ToLower(ref), nil
	}
	if strings.HasPrefix(ref, "refs/heads/") {
		if oid := refs[ref]; oid != "" {
			return oid, nil
		}
	} else if strings.HasPrefix(ref, "refs/tags/") {
		if oid := refs[ref+"^{}"]; oid != "" {
			return oid, nil
		}
		if oid := refs[ref]; oid != "" {
			return oid, nil
		}
	} else {
		branch, tag := refs["refs/heads/"+ref], refs["refs/tags/"+ref]
		if branch != "" && tag != "" {
			return "", sourceErr(ErrorOptions, "ref", ref, errors.New("ambiguous branch and tag; use refs/heads/ or refs/tags/"))
		}
		if branch != "" {
			return branch, nil
		}
		if peeled := refs["refs/tags/"+ref+"^{}"]; peeled != "" {
			return peeled, nil
		}
		if tag != "" {
			return tag, nil
		}
	}
	return "", sourceErr(ErrorInput, "resolve git ref", ref, errors.New("requested branch or tag is missing or inaccessible"))
}

func acquireGitCommit(ctx context.Context, r *gitRunner, location, commit string) error {
	r.verifyTransport(location)
	format := "sha1"
	if len(commit) == 64 {
		format = "sha256"
	}
	if _, err := r.output(ctx, "init", "--bare", "--template=", "--object-format="+format, filepath.Join(r.dir, "objects.git")); err != nil {
		return sourceErr(ErrorInput, "init git objects", "", err)
	}
	err := runNative(ctx, r.objects("fetch", "--no-tags", "--depth=1", "--no-write-fetch-head", "--recurse-submodules=no", "--no-auto-maintenance", "--", location, commit), func(reader io.Reader) error { _, err := io.Copy(io.Discard, reader); return err })
	if err != nil {
		return sourceErr(ErrorInput, "fetch resolved git commit", redactTransport(location), err)
	}
	var kind bytes.Buffer
	err = runNative(ctx, r.objects("cat-file", "-t", commit), func(reader io.Reader) error { _, err := copyBounded(ctx, &kind, reader, 64); return err })
	if err != nil {
		return sourceErr(ErrorInput, "read resolved git commit", commit, err)
	}
	if strings.TrimSpace(kind.String()) != "commit" {
		return sourceErr(ErrorInput, "read resolved git commit", commit, errors.New("resolved object is not a commit"))
	}
	return nil
}

type gitEntry struct {
	name      string
	oid       string
	directory bool
}

func gitRecord(reader *bufio.Reader, delimiter byte) ([]byte, error) {
	var result []byte
	for {
		part, err := reader.ReadSlice(delimiter)
		if len(result)+len(part) > 64<<10 {
			return nil, sourceErr(ErrorLimit, "git record", "", errors.New("Git metadata record exceeds 64 KiB"))
		}
		result = append(result, part...)
		if !errors.Is(err, bufio.ErrBufferFull) {
			return result, err
		}
	}
}

func exportGitTree(ctx context.Context, r *gitRunner, root *os.Root, commit string, limits Limits) ([]gitEntry, error) {
	registry := make(pathRegistry)
	var entries []gitEntry
	var count int64
	metadataLimit := int64(math.MaxInt64)
	if limits.MaxFiles <= (math.MaxInt64-1024)/2 {
		metadataLimit = limits.MaxFiles*2 + 1024
	}
	err := runNative(ctx, r.objects("ls-tree", "-r", "-t", "-z", "--full-tree", commit), func(input io.Reader) error {
		reader := bufio.NewReader(input)
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			record, err := gitRecord(reader, 0)
			if errors.Is(err, io.EOF) && len(record) == 0 {
				return nil
			}
			if err != nil {
				return err
			}
			header, name, ok := bytes.Cut(record[:len(record)-1], []byte{'\t'})
			parts := strings.Fields(string(header))
			if !ok || len(parts) != 3 || !validOID(parts[2]) {
				return errors.New("invalid ls-tree record")
			}
			directory := parts[0] == "040000" && parts[1] == "tree"
			if !directory && (parts[0] != "100644" && parts[0] != "100755" || parts[1] != "blob") {
				return sourceErr(ErrorUnsupportedEntry, "git tree", string(name), fmt.Errorf("Git mode %s/type %s is unsupported (links and submodules are not followed)", parts[0], parts[1]))
			}
			clean, err := portablePath(string(name), directory, false)
			if err != nil {
				return sourceErr(ErrorUnsafePath, "git tree", string(name), err)
			}
			for _, part := range strings.Split(clean, "/") {
				if foldPath(part) == foldPath(".git") {
					return sourceErr(ErrorUnsupportedEntry, "git tree", clean, errors.New("repository metadata is not exportable"))
				}
			}
			if err := registry.add(clean, directory); err != nil {
				return sourceErr(ErrorUnsafePath, "git tree", clean, err)
			}
			if !directory {
				if count >= limits.MaxFiles {
					return sourceErr(ErrorLimit, "git count", clean, errors.New("regular file count exceeded"))
				}
				count++
			}
			// Bound metadata as well as blobs, including hostile directory-only trees.
			if int64(len(entries)) >= metadataLimit {
				return sourceErr(ErrorLimit, "git tree", clean, errors.New("Git entry count exceeded"))
			}
			entries = append(entries, gitEntry{name: clean, oid: parts[2], directory: directory})
		}
	})
	if err != nil {
		return nil, sourceErr(ErrorInput, "list git tree", commit, err)
	}
	writer := treeWriter{root: root}
	var requests strings.Builder
	for _, entry := range entries {
		if entry.directory {
			if err := writer.ensureDirectories(ctx, entry.name); err != nil {
				return nil, sourceErr(ErrorUnsafePath, "git directory", entry.name, err)
			}
		} else {
			requests.WriteString(entry.oid)
			requests.WriteByte('\n')
		}
	}
	cmd := r.objects("cat-file", "--batch")
	cmd.Stdin = strings.NewReader(requests.String())
	var total int64
	err = runNative(ctx, cmd, func(input io.Reader) error {
		reader := bufio.NewReader(input)
		for _, entry := range entries {
			if entry.directory {
				continue
			}
			header, err := gitRecord(reader, '\n')
			if err != nil {
				return err
			}
			parts := strings.Fields(string(header))
			if len(parts) != 3 || parts[0] != entry.oid || parts[1] != "blob" {
				return sourceErr(ErrorInput, "git blob", entry.name, errors.New("object is missing or is not a blob"))
			}
			size, err := strconv.ParseInt(parts[2], 10, 64)
			if err != nil || size < 0 {
				return errors.New("invalid Git blob size")
			}
			allowance := min(limits.MaxFileBytes, limits.MaxTotalBytes-total)
			if size > allowance {
				return sourceErr(ErrorLimit, "git blob", entry.name, errors.New("blob byte allowance exceeded"))
			}
			blob := &io.LimitedReader{R: reader, N: size}
			prefix := make([]byte, min(size, 1024))
			if _, err = io.ReadFull(blob, prefix); err != nil {
				return err
			}
			if bytes.HasPrefix(prefix, []byte("version https://git-lfs.github.com/spec/v1\n")) || bytes.HasPrefix(prefix, []byte("version https://git-lfs.github.com/spec/v1\r\n")) {
				return sourceErr(ErrorUnsupportedEntry, "git blob", entry.name, errors.New("Git LFS pointers are unsupported"))
			}
			written, err := writer.writeEntry(ctx, preparedEntry{name: entry.name, size: size}, io.MultiReader(bytes.NewReader(prefix), blob), allowance)
			if err != nil {
				kind := ErrorInput
				if errors.Is(err, fs.ErrExist) {
					kind = ErrorUnsafePath
				}
				return sourceErr(kind, "export git blob", entry.name, err)
			}
			if written != size {
				return io.ErrUnexpectedEOF
			}
			total += written
			if delimiter, err := reader.ReadByte(); err != nil || delimiter != '\n' {
				return errors.New("invalid cat-file blob terminator")
			}
		}
		if _, err := reader.ReadByte(); !errors.Is(err, io.EOF) {
			return errors.New("unexpected cat-file output")
		}
		return nil
	})
	return entries, err
}
