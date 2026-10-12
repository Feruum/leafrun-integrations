package integrations

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type archiveRecord struct {
	name     string
	data     []byte
	mode     fs.FileMode
	typeflag byte
	link     string
	format   tar.Format
}

func writeArchive(t *testing.T, kind string, records []archiveRecord) (string, []byte) {
	t.Helper()
	var data bytes.Buffer
	switch kind {
	case "zip":
		w := zip.NewWriter(&data)
		for _, record := range records {
			h := &zip.FileHeader{Name: record.name, Method: zip.Deflate}
			mode := record.mode
			if mode == 0 {
				mode = 0600
			}
			h.SetMode(mode)
			f, err := w.CreateHeader(h)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.Write(record.data); err != nil {
				t.Fatal(err)
			}
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	case "tar.gz":
		gz := gzip.NewWriter(&data)
		w := tar.NewWriter(gz)
		for _, record := range records {
			flag := record.typeflag
			if flag == 0 {
				flag = tar.TypeReg
			}
			if record.mode.IsDir() {
				flag = tar.TypeDir
			}
			h := &tar.Header{Name: record.name, Typeflag: flag, Linkname: record.link, Mode: 0600, Size: int64(len(record.data)), Format: record.format}
			if flag != tar.TypeReg {
				h.Size = 0
			}
			if err := w.WriteHeader(h); err != nil {
				t.Fatal(err)
			}
			if _, err := w.Write(record.data); err != nil {
				t.Fatal(err)
			}
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown test kind %s", kind)
	}
	path := filepath.Join(t.TempDir(), "project."+kind)
	raw := append([]byte(nil), data.Bytes()...)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return path, raw
}

func TestArchiveSourceProvidesRetainedStandardTree(t *testing.T) {
	for _, kind := range []string{"zip", "tar.gz"} {
		t.Run(kind, func(t *testing.T) {
			records := []archiveRecord{
				{name: "project/main.typ", data: []byte("#include \"Résumé/data.json\"")},
				{name: "project/Résumé/data.json", data: []byte(`{"title":"A source"}`)},
				{name: "project/Résumé/", mode: fs.ModeDir | 0700},
				{name: "project/figure.svg", data: []byte("<svg/>")},
				{name: "project/font bytes.otf", data: []byte{0, 1, 255, 42}},
				{name: "outside.txt", data: []byte("not selected")},
			}
			path, raw := writeArchive(t, kind, records)
			canonical := canonicalArchivePath(t, path)
			tempParent := t.TempDir()
			tree, err := (ArchiveSource{Path: path, SourceRoot: "project", Options: ArchiveOptions{TempDir: tempParent}}).Open(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := tree.Close(); err != nil {
					t.Error(err)
				}
			})
			if tree.Origin.Kind != kind || tree.Origin.SourceRoot != "project" {
				t.Fatalf("origin: %+v", tree.Origin)
			}
			digest := sha256.Sum256(raw)
			if tree.Origin.ArchiveSHA256 != hex.EncodeToString(digest[:]) {
				t.Fatalf("digest: %s", tree.Origin.ArchiveSHA256)
			}
			if tree.Origin.Location != canonical {
				t.Fatalf("location: %q", tree.Origin.Location)
			}
			if len(tree.ProtectedPaths) != 1 || tree.ProtectedPaths[0] != canonical {
				t.Fatalf("protected paths: %v", tree.ProtectedPaths)
			}
			if err := os.WriteFile(path, []byte("changed external archive"), 0600); err != nil {
				t.Fatal(err)
			}
			for _, record := range records[:5] {
				if record.mode.IsDir() {
					continue
				}
				data, err := fs.ReadFile(tree.FS, strings.TrimPrefix(record.name, "project/"))
				if err != nil || !bytes.Equal(data, record.data) {
					t.Fatalf("%s: %q, %v", record.name, data, err)
				}
			}
			if _, err := fs.Stat(tree.FS, "outside.txt"); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("outside subtree: %v", err)
			}
			if _, err := tree.FS.Open("../outside.txt"); !errors.Is(err, fs.ErrInvalid) {
				t.Fatalf("invalid fs path: %v", err)
			}
			entries, err := fs.ReadDir(tree.FS, ".")
			if err != nil || len(entries) != 4 {
				t.Fatalf("root directory: %v %v", entries, err)
			}
			f, err := tree.FS.Open("main.typ")
			if err != nil {
				t.Fatal(err)
			}
			if err := tree.Close(); err != nil {
				t.Fatal(err)
			}
			if err := tree.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := f.Read(make([]byte, 1)); !errors.Is(err, fs.ErrClosed) {
				t.Fatalf("retained file after close: %v", err)
			}
			if _, err := fs.ReadFile(tree.FS, "main.typ"); !errors.Is(err, fs.ErrClosed) {
				t.Fatalf("tree after close: %v", err)
			}
			left, err := os.ReadDir(tempParent)
			if err != nil || len(left) != 0 {
				t.Fatalf("owned cleanup: %v, %v", left, err)
			}
			if data, err := os.ReadFile(path); err != nil || string(data) != "changed external archive" {
				t.Fatalf("original retained: %q %v", data, err)
			}
		})
	}
}

func TestArchiveSourceRejectsUnsafePathsAndCollisions(t *testing.T) {
	unsafe := []string{"/absolute", "../escape", "a/../b", "a/./b", "a//b", `C:/drive`, `C:drive`, `\\server\share`, `a\b`, "a:stream", "NUL", "con.txt", "AUX", "com¹.txt", "lpt9", "trail. ", "dot.", "space ", "bad\x01", "bad\x7f", "bad\x85", "bad\xff"}
	for _, kind := range []string{"zip", "tar.gz"} {
		for _, name := range unsafe {
			t.Run(kind+"/"+name, func(t *testing.T) {
				if kind == "tar.gz" && !strings.Contains(name, "\xff") && strings.Contains(name, "\x00") {
					t.Skip("tar writer cannot encode NUL")
				}
				path, _ := writeArchive(t, kind, []archiveRecord{{name: "selected/main.typ", data: []byte("ok")}, {name: name, data: []byte("bad")}})
				assertArchiveFailure(t, ArchiveSource{Path: path, SourceRoot: "selected"}, ErrUnsafePath)
			})
		}
		cases := [][]archiveRecord{
			{{name: "a", data: []byte("1")}, {name: "a", data: []byte("2")}},
			{{name: "a", data: []byte("1")}, {name: "./a", data: []byte("2")}},
			{{name: "a/", mode: fs.ModeDir}, {name: "a/", mode: fs.ModeDir}},
			{{name: "Résumé/a", data: []byte("1")}, {name: "résumé/b", data: []byte("2")}},
			{{name: "K/a", data: []byte("1")}, {name: "K/b", data: []byte("2")}},
			{{name: "σ", data: []byte("1")}, {name: "ς", data: []byte("2")}},
			{{name: "a", data: []byte("1")}, {name: "a/b", data: []byte("2")}},
			{{name: "a/b", data: []byte("1")}, {name: "a", data: []byte("2")}},
			{{name: "a/", mode: fs.ModeDir}, {name: "A", data: []byte("2")}},
		}
		for i, records := range cases {
			t.Run(kind+"/collision/"+string(rune('a'+i)), func(t *testing.T) {
				path, _ := writeArchive(t, kind, records)
				assertArchiveFailure(t, ArchiveSource{Path: path}, ErrUnsafePath)
			})
		}
	}
}

func assertArchiveFailure(t *testing.T, source ArchiveSource, want error) {
	t.Helper()
	parent := t.TempDir()
	source.Options.TempDir = parent
	tree, err := source.Open(context.Background())
	if err == nil {
		if tree.Close != nil {
			_ = tree.Close()
		}
		t.Fatal("unsafe source accepted")
	}
	if !errors.Is(err, want) {
		t.Fatalf("error %v does not wrap %v", err, want)
	}
	if tree.FS != nil || tree.Close != nil {
		t.Fatalf("failed operation returned a tree: %+v", tree)
	}
	entries, readErr := os.ReadDir(parent)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("failed operation retained resources: %v %v", entries, readErr)
	}
	if _, statErr := os.Stat(source.Path); statErr != nil {
		t.Fatalf("original archive removed: %v", statErr)
	}
}

func TestArchiveSourceRejectsLinksAndSpecialRecords(t *testing.T) {
	for _, mode := range []fs.FileMode{fs.ModeSymlink, fs.ModeNamedPipe, fs.ModeSocket, fs.ModeDevice, fs.ModeDevice | fs.ModeCharDevice} {
		path, _ := writeArchive(t, "zip", []archiveRecord{{name: "special", mode: mode, data: []byte("target")}})
		assertArchiveFailure(t, ArchiveSource{Path: path}, ErrUnsupportedEntry)
	}
	for _, flag := range []byte{tar.TypeSymlink, tar.TypeLink, tar.TypeFifo, tar.TypeChar, tar.TypeBlock} {
		path, _ := writeArchive(t, "tar.gz", []archiveRecord{{name: "special", typeflag: flag, link: "target"}})
		assertArchiveFailure(t, ArchiveSource{Path: path}, ErrUnsupportedEntry)
	}
}

func TestArchiveSourceLimitsAndRootValidation(t *testing.T) {
	for _, kind := range []string{"zip", "tar.gz"} {
		path, _ := writeArchive(t, kind, []archiveRecord{{name: "project/a", data: []byte("1234")}, {name: "project/b", data: []byte("5678")}})
		for _, limits := range []Limits{{MaxFiles: 1}, {MaxFileBytes: 3}, {MaxTotalBytes: 7}} {
			assertArchiveFailure(t, ArchiveSource{Path: path, Options: ArchiveOptions{Limits: limits}}, ErrLimit)
		}
		for _, root := range []string{"missing", "project/a"} {
			assertArchiveFailure(t, ArchiveSource{Path: path, SourceRoot: root}, ErrSourceRoot)
		}
		for _, root := range []string{"../project", "/project", `project\a`, "project/../project", "project/", "./project"} {
			assertArchiveFailure(t, ArchiveSource{Path: path, SourceRoot: root}, ErrSourceRoot)
		}
		for _, limits := range []Limits{{MaxFiles: -1}, {MaxFileBytes: -1}, {MaxTotalBytes: -1}} {
			assertArchiveFailure(t, ArchiveSource{Path: path, Options: ArchiveOptions{Limits: limits}}, ErrOptions)
		}
	}
}

func TestArchiveSourceCancellationBeforeAcquisition(t *testing.T) {
	path, _ := writeArchive(t, "zip", []archiveRecord{{name: "main.typ", data: []byte("ok")}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	parent := t.TempDir()
	tree, err := (ArchiveSource{Path: path, Options: ArchiveOptions{TempDir: parent}}).Open(ctx)
	if !errors.Is(err, context.Canceled) || tree.FS != nil {
		t.Fatalf("cancel: %+v, %v", tree, err)
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 0 {
		t.Fatalf("cancel cleanup: %v %v", entries, err)
	}
}

func replaceArchiveBytes(t *testing.T, path string, raw []byte) {
	t.Helper()
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestArchiveSourceRejectsZIPCorruption(t *testing.T) {
	t.Run("CRC", func(t *testing.T) {
		var data bytes.Buffer
		w := zip.NewWriter(&data)
		f, err := w.CreateHeader(&zip.FileHeader{Name: "main.typ", Method: zip.Store})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(f, "unique project data"); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		raw := append([]byte(nil), data.Bytes()...)
		index := bytes.Index(raw, []byte("unique project data"))
		if index < 0 {
			t.Fatal("fixture data missing")
		}
		raw[index] ^= 1
		path := filepath.Join(t.TempDir(), "project.zip")
		replaceArchiveBytes(t, path, raw)
		assertArchiveFailure(t, ArchiveSource{Path: path}, zip.ErrChecksum)
	})
	t.Run("local header", func(t *testing.T) {
		path, raw := writeArchive(t, "zip", []archiveRecord{{name: "main.typ", data: []byte("ok")}})
		raw[0] = 0
		replaceArchiveBytes(t, path, raw)
		assertArchiveFailure(t, ArchiveSource{Path: path}, ErrArchive)
	})
	t.Run("truncation", func(t *testing.T) {
		path, raw := writeArchive(t, "zip", []archiveRecord{{name: "main.typ", data: []byte("ok")}})
		replaceArchiveBytes(t, path, raw[:len(raw)-12])
		assertArchiveFailure(t, ArchiveSource{Path: path}, ErrArchive)
	})
}

func TestArchiveSourceRejectsGZIPCorruptionAndTrailingData(t *testing.T) {
	for _, kind := range []string{"missing footer", "bad footer", "another member", "compressed tail"} {
		t.Run(kind, func(t *testing.T) {
			path, raw := writeArchive(t, "tar.gz", []archiveRecord{{name: "main.typ", data: []byte("ok")}})
			switch kind {
			case "missing footer":
				raw = raw[:len(raw)-8]
			case "bad footer":
				raw[len(raw)-8] ^= 1
			case "another member":
				raw = append(raw, raw...)
			case "compressed tail":
				raw = append(raw, 'x')
			}
			replaceArchiveBytes(t, path, raw)
			assertArchiveFailure(t, ArchiveSource{Path: path}, ErrArchive)
		})
	}
	for _, kind := range []string{"bad checksum", "truncated payload", "nonzero tar tail", "expanded padding limit"} {
		t.Run(kind, func(t *testing.T) {
			path, raw := writeArchive(t, "tar.gz", []archiveRecord{{name: "main.typ", data: []byte("ok")}})
			gz, err := gzip.NewReader(bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			expanded, err := io.ReadAll(gz)
			if err != nil {
				t.Fatal(err)
			}
			if err := gz.Close(); err != nil {
				t.Fatal(err)
			}
			options := ArchiveOptions{}
			switch kind {
			case "bad checksum":
				expanded[0] ^= 1
			case "truncated payload":
				expanded = expanded[:513]
			case "nonzero tar tail":
				expanded = append(expanded, 'x')
			case "expanded padding limit":
				expanded = append(expanded, make([]byte, 4096)...)
				options.Limits.MaxTotalBytes = 2048
			}
			var out bytes.Buffer
			w := gzip.NewWriter(&out)
			if _, err := w.Write(expanded); err != nil {
				t.Fatal(err)
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			replaceArchiveBytes(t, path, out.Bytes())
			want := ErrArchive
			if kind == "expanded padding limit" {
				want = ErrLimit
			}
			assertArchiveFailure(t, ArchiveSource{Path: path, Options: options}, want)
		})
	}
}
