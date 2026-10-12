package integrations

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestArchiveRejectsAbsoluteDirectoryAndUnsafeCharacters(t *testing.T) {
	for _, kind := range []string{"zip", "tar.gz"} {
		for _, name := range []string{"/", "//", ".//", "././/", "./../", "a?/", "a*/", "a|/", "a</", "a>/", "a\"/", "NUL.txt/", "conin$/", "CON .txt/", "COM1 .txt/"} {
			path, _ := writeArchive(t, kind, []archiveRecord{{name: name, mode: fs.ModeDir}})
			assertArchiveFailure(t, ArchiveSource{Path: path}, ErrUnsafePath)
		}
	}
}

func TestZIPRejectsUnusedCompressedTail(t *testing.T) {
	data := []byte("ok")
	var compressed bytes.Buffer
	compressor, err := flate.NewWriter(&compressed, flate.DefaultCompression)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := compressor.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := compressor.Close(); err != nil {
		t.Fatal(err)
	}
	compressed.WriteByte('x')
	var raw bytes.Buffer
	w := zip.NewWriter(&raw)
	f, err := w.CreateRaw(&zip.FileHeader{Name: "main.typ", Method: zip.Deflate, CRC32: crc32.ChecksumIEEE(data), UncompressedSize64: uint64(len(data)), CompressedSize64: uint64(compressed.Len())})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(compressed.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "tail.zip")
	replaceArchiveBytes(t, path, raw.Bytes())
	assertArchiveFailure(t, ArchiveSource{Path: path}, ErrArchive)
}

func TestZIPRejectsMissingLocalZIP64Sizes(t *testing.T) {
	var raw bytes.Buffer
	w := zip.NewWriter(&raw)
	data := []byte("ok")
	f, err := w.CreateRaw(&zip.FileHeader{Name: "main.typ", Method: zip.Store, CRC32: crc32.ChecksumIEEE(data), UncompressedSize64: 2, CompressedSize64: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	bytes := raw.Bytes()
	binary.LittleEndian.PutUint32(bytes[18:22], math.MaxUint32)
	path := filepath.Join(t.TempDir(), "local64.zip")
	replaceArchiveBytes(t, path, bytes)
	assertArchiveFailure(t, ArchiveSource{Path: path}, ErrArchive)
}

type probeContext struct {
	context.Context
	probe func()
}

func (c probeContext) Err() error { c.probe(); return c.Context.Err() }

func TestArchiveParsesAndHashesSameOwnedCapture(t *testing.T) {
	for _, kind := range []string{"zip", "tar.gz"} {
		path, raw := writeArchive(t, kind, []archiveRecord{{name: "main.typ", data: []byte("accepted bytes")}})
		_, replacement := writeArchive(t, kind, []archiveRecord{{name: "main.typ", data: []byte("replacement bytes")}})
		parent := t.TempDir()
		changed := false
		ctx := probeContext{Context: context.Background(), probe: func() {
			if changed {
				return
			}
			entries, err := os.ReadDir(parent)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if _, err := os.Stat(filepath.Join(parent, entry.Name(), "tree")); err == nil {
					replaceArchiveBytes(t, path, replacement)
					changed = true
				}
			}
		}}
		tree, err := (ArchiveSource{Path: path, Options: ArchiveOptions{TempDir: parent}}).Open(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = tree.Close() })
		if !changed {
			t.Fatal("fixture did not change original between capture and extraction")
		}
		data, err := fs.ReadFile(tree.FS, "main.typ")
		if err != nil || string(data) != "accepted bytes" {
			t.Fatalf("parsed changed external bytes: %q %v", data, err)
		}
		acceptedHash := sha256.Sum256(raw)
		if tree.Origin.ArchiveSHA256 != hex.EncodeToString(acceptedHash[:]) {
			t.Fatalf("digest not accepted capture: %+v", tree.Origin)
		}
	}
}

func TestZIPRejectsUnsafeLocalName(t *testing.T) {
	path, raw := writeArchive(t, "zip", []archiveRecord{{name: "aa/escape", data: []byte("payload")}})
	copy(raw[30:39], "../escape")
	replaceArchiveBytes(t, path, raw)
	assertArchiveFailure(t, ArchiveSource{Path: path}, ErrUnsafePath)
}

func TestZIPVerifiesDirectoryChecksum(t *testing.T) {
	path, raw := writeArchive(t, "zip", []archiveRecord{{name: "empty/", mode: fs.ModeDir}})
	central := bytes.Index(raw, []byte{'P', 'K', 1, 2})
	if central < 0 {
		t.Fatal("central header missing")
	}
	binary.LittleEndian.PutUint32(raw[central+16:central+20], 1)
	replaceArchiveBytes(t, path, raw)
	assertArchiveFailure(t, ArchiveSource{Path: path}, zip.ErrChecksum)
}

func TestZIPRejectsDamagedDeflateStreamOutsideSourceRoot(t *testing.T) {
	path, raw := writeArchive(t, "zip", []archiveRecord{{name: "selected/main.typ", data: []byte("ok")}, {name: "ignored", data: []byte("ignored payload")}})
	r, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	offset, err := r.File[1].DataOffset()
	if err != nil {
		t.Fatal(err)
	}
	raw[offset] ^= 0xff
	replaceArchiveBytes(t, path, raw)
	assertArchiveFailure(t, ArchiveSource{Path: path, SourceRoot: "selected"}, ErrArchive)
}

func TestZIPChecksDeclaredIntegerOverflow(t *testing.T) {
	var out bytes.Buffer
	w := zip.NewWriter(&out)
	_, err := w.CreateRaw(&zip.FileHeader{Name: "main.typ", Method: zip.Store, UncompressedSize64: math.MaxUint64, CompressedSize64: 0})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "overflow.zip")
	replaceArchiveBytes(t, path, out.Bytes())
	assertArchiveFailure(t, ArchiveSource{Path: path}, ErrLimit)
}

func TestArchivesAllowConventionalRootAndLeadingDotRecords(t *testing.T) {
	for _, kind := range []string{"zip", "tar.gz"} {
		path, _ := writeArchive(t, kind, []archiveRecord{{name: "./", mode: fs.ModeDir}, {name: "./docs/main.typ", data: []byte("hello")}})
		tree, err := (ArchiveSource{Path: path}).Open(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		data, readErr := fs.ReadFile(tree.FS, "docs/main.typ")
		closeErr := tree.Close()
		if readErr != nil || closeErr != nil || string(data) != "hello" || tree.Origin.SourceRoot != "." {
			t.Fatalf("root record: %q %v %v %+v", data, readErr, closeErr, tree.Origin)
		}
	}
}

func tarRecord(t *testing.T, name string, flag byte, payload []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	w := tar.NewWriter(&out)
	if err := w.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0600, Size: int64(len(payload)), Format: tar.FormatUSTAR}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	raw := append([]byte(nil), out.Bytes()[:out.Len()-1024]...)
	raw[156] = flag
	fixTarChecksum(raw[:512])
	return raw
}

func fixTarChecksum(block []byte) {
	for i := 148; i < 156; i++ {
		block[i] = ' '
	}
	var sum int64
	for _, b := range block {
		sum += int64(b)
	}
	copy(block[148:156], fmt.Sprintf("%06o\x00 ", sum))
}

func gzipTAR(t *testing.T, raw []byte) string {
	t.Helper()
	var out bytes.Buffer
	w := gzip.NewWriter(&out)
	if _, err := w.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "project.tar.gz")
	replaceArchiveBytes(t, path, out.Bytes())
	return path
}

func paxRecord(key, value string) []byte {
	data := key + "=" + value + "\n"
	length := len(data) + 2
	for {
		result := fmt.Sprintf("%d %s", length, data)
		if len(result) == length {
			return []byte(result)
		}
		length = len(result)
	}
}

func TestTARValidatesHiddenMetadataAndSupersededPaths(t *testing.T) {
	for _, record := range []struct {
		name string
		flag byte
		data []byte
		want error
	}{
		{name: "../metadata", flag: tar.TypeXHeader, data: paxRecord("path", "safe"), want: ErrUnsafePath},
		{name: "PaxHeaders/meta", flag: tar.TypeXHeader, data: append(paxRecord("path", "../unsafe"), paxRecord("path", "safe")...), want: ErrUnsafePath},
		{name: "PaxHeaders/meta", flag: tar.TypeXHeader, data: paxRecord("GNU.sparse.map", "0,1"), want: ErrUnsupportedEntry},
		{name: "././@LongLink", flag: tar.TypeGNULongName, data: []byte("../unsafe\x00"), want: ErrUnsafePath},
		{name: "././@LongLink", flag: tar.TypeGNULongLink, data: []byte("safe\x00"), want: ErrUnsupportedEntry},
		{name: "PaxHeaders/meta", flag: tar.TypeXHeader, data: []byte("99 invalid\n"), want: ErrArchive},
	} {
		metadata := tarRecord(t, record.name, record.flag, record.data)
		normal := tarRecord(t, "safe", tar.TypeReg, []byte("ok"))
		raw := append(metadata, normal...)
		raw = append(raw, make([]byte, 1024)...)
		path := gzipTAR(t, raw)
		assertArchiveFailure(t, ArchiveSource{Path: path}, record.want)
	}
}

func TestTARSupportsPAXAndGNULongNames(t *testing.T) {
	for _, format := range []tar.Format{tar.FormatPAX, tar.FormatGNU} {
		var raw bytes.Buffer
		w := tar.NewWriter(&raw)
		name := "docs/" + strings.Repeat("a", 120) + ".typ"
		if err := w.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0600, Size: 2, Format: format}); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte("ok")); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		path := gzipTAR(t, raw.Bytes())
		tree, err := (ArchiveSource{Path: path}).Open(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		data, readErr := fs.ReadFile(tree.FS, name)
		closeErr := tree.Close()
		if readErr != nil || closeErr != nil || string(data) != "ok" {
			t.Fatalf("long name: %q %v %v", data, readErr, closeErr)
		}
	}
}

func TestTARRejectsOrphanMetadata(t *testing.T) {
	for _, record := range []struct {
		flag byte
		data []byte
	}{
		{flag: tar.TypeXHeader, data: paxRecord("path", "safe")},
		{flag: tar.TypeGNULongName, data: []byte("safe\x00")},
	} {
		raw := tarRecord(t, "metadata", record.flag, record.data)
		path := gzipTAR(t, append(raw, make([]byte, 1024)...))
		assertArchiveFailure(t, ArchiveSource{Path: path}, ErrArchive)
	}
}

func TestTARCancelsBetweenPAXRecords(t *testing.T) {
	var metadata []byte
	for range 100 {
		metadata = append(metadata, paxRecord("comment", "some metadata")...)
	}
	raw := tarRecord(t, "metadata", tar.TypeXHeader, metadata)
	raw = append(raw, tarRecord(t, "main.typ", tar.TypeReg, []byte("ok"))...)
	path := gzipTAR(t, append(raw, make([]byte, 1024)...))
	parent := t.TempDir()
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &cancelChecks{Context: base, cancel: cancel, remaining: 60}
	tree, err := (ArchiveSource{Path: path, Options: ArchiveOptions{TempDir: parent}}).Open(ctx)
	if tree.Close != nil {
		_ = tree.Close()
	}
	if !errors.Is(err, context.Canceled) || tree.FS != nil {
		t.Fatalf("metadata cancellation: %+v %v", tree, err)
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 0 {
		t.Fatalf("metadata cancellation cleanup: %v %v", entries, err)
	}
}

func TestZIPSupportsSmallZIP64Records(t *testing.T) {
	for _, descriptor := range []bool{false, true} {
		var original bytes.Buffer
		w := zip.NewWriter(&original)
		data := []byte("ok")
		f, err := w.CreateRaw(&zip.FileHeader{Name: "main.typ", Method: zip.Store, CRC32: crc32.ChecksumIEEE(data), UncompressedSize64: 2, CompressedSize64: 2})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(data); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		reader, err := zip.NewReader(bytes.NewReader(original.Bytes()), int64(original.Len()))
		if err != nil {
			t.Fatal(err)
		}
		dataOffset, err := reader.File[0].DataOffset()
		if err != nil {
			t.Fatal(err)
		}
		local := append([]byte(nil), original.Bytes()[:dataOffset]...)
		central := append([]byte(nil), original.Bytes()[dataOffset+2:original.Len()-22]...)
		binary.LittleEndian.PutUint16(local[4:6], 45)
		binary.LittleEndian.PutUint32(local[18:22], math.MaxUint32)
		binary.LittleEndian.PutUint32(local[22:26], math.MaxUint32)
		binary.LittleEndian.PutUint16(local[28:30], 20)
		localExtra := make([]byte, 20)
		binary.LittleEndian.PutUint16(localExtra[:2], 1)
		binary.LittleEndian.PutUint16(localExtra[2:4], 16)
		if descriptor {
			binary.LittleEndian.PutUint16(local[6:8], 8)
			binary.LittleEndian.PutUint32(local[14:18], 0)
			binary.LittleEndian.PutUint16(central[8:10], 8)
		} else {
			binary.LittleEndian.PutUint64(localExtra[4:12], 2)
			binary.LittleEndian.PutUint64(localExtra[12:20], 2)
		}
		binary.LittleEndian.PutUint16(central[6:8], 45)
		binary.LittleEndian.PutUint32(central[20:24], math.MaxUint32)
		binary.LittleEndian.PutUint32(central[24:28], math.MaxUint32)
		binary.LittleEndian.PutUint32(central[42:46], math.MaxUint32)
		binary.LittleEndian.PutUint16(central[30:32], 28)
		centralExtra := make([]byte, 28)
		binary.LittleEndian.PutUint16(centralExtra[:2], 1)
		binary.LittleEndian.PutUint16(centralExtra[2:4], 24)
		binary.LittleEndian.PutUint64(centralExtra[4:12], 2)
		binary.LittleEndian.PutUint64(centralExtra[12:20], 2)
		local = append(local, localExtra...)
		local = append(local, data...)
		if descriptor {
			desc := make([]byte, 24)
			copy(desc, []byte{'P', 'K', 7, 8})
			binary.LittleEndian.PutUint32(desc[4:8], crc32.ChecksumIEEE(data))
			binary.LittleEndian.PutUint64(desc[8:16], 2)
			binary.LittleEndian.PutUint64(desc[16:24], 2)
			local = append(local, desc...)
		}
		central = append(central, centralExtra...)
		end := make([]byte, 56)
		copy(end, []byte{'P', 'K', 6, 6})
		binary.LittleEndian.PutUint64(end[4:12], 44)
		binary.LittleEndian.PutUint16(end[12:14], 45)
		binary.LittleEndian.PutUint16(end[14:16], 45)
		binary.LittleEndian.PutUint64(end[24:32], 1)
		binary.LittleEndian.PutUint64(end[32:40], 1)
		binary.LittleEndian.PutUint64(end[40:48], uint64(len(central)))
		binary.LittleEndian.PutUint64(end[48:56], uint64(len(local)))
		locator := make([]byte, 20)
		copy(locator, []byte{'P', 'K', 6, 7})
		binary.LittleEndian.PutUint64(locator[8:16], uint64(len(local)+len(central)))
		binary.LittleEndian.PutUint32(locator[16:20], 1)
		legacy := make([]byte, 22)
		copy(legacy, []byte{'P', 'K', 5, 6})
		binary.LittleEndian.PutUint16(legacy[8:10], math.MaxUint16)
		binary.LittleEndian.PutUint16(legacy[10:12], math.MaxUint16)
		binary.LittleEndian.PutUint32(legacy[12:16], math.MaxUint32)
		binary.LittleEndian.PutUint32(legacy[16:20], math.MaxUint32)
		raw := append(local, central...)
		raw = append(raw, end...)
		raw = append(raw, locator...)
		raw = append(raw, legacy...)
		path := filepath.Join(t.TempDir(), "zip64.zip")
		replaceArchiveBytes(t, path, raw)
		tree, err := (ArchiveSource{Path: path}).Open(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		read, readErr := fs.ReadFile(tree.FS, "main.typ")
		closeErr := tree.Close()
		if readErr != nil || closeErr != nil || !bytes.Equal(read, data) {
			t.Fatalf("ZIP64 descriptor=%v: %q %v %v", descriptor, read, readErr, closeErr)
		}
	}
}

type cancelRead struct {
	reader io.Reader
	cancel context.CancelFunc
}

func (r cancelRead) Read(data []byte) (int, error) {
	n, err := r.reader.Read(data)
	r.cancel()
	return n, err
}

func TestCopyBoundedEnforcesActualBytesAndChunkCancellation(t *testing.T) {
	var out bytes.Buffer
	n, err := copyBounded(context.Background(), &out, strings.NewReader("123456"), 4)
	if !errors.Is(err, ErrLimit) || n > 4 || out.Len() > 4 {
		t.Fatalf("actual limit: %d %q %v", n, out.String(), err)
	}
	out.Reset()
	ctx, cancel := context.WithCancel(context.Background())
	n, err = copyBounded(ctx, &out, cancelRead{reader: strings.NewReader("123456"), cancel: cancel}, 10)
	if !errors.Is(err, context.Canceled) || n != 0 || out.Len() != 0 {
		t.Fatalf("cancel between read/write: %d %q %v", n, out.String(), err)
	}
}

type cancelChecks struct {
	context.Context
	cancel    context.CancelFunc
	remaining int
}

func (c *cancelChecks) Err() error {
	c.remaining--
	if c.remaining <= 0 {
		c.cancel()
	}
	return c.Context.Err()
}

func TestArchiveCancellationDuringOwnedAcquisitionAndExtraction(t *testing.T) {
	for _, kind := range []string{"zip", "tar.gz"} {
		var records []archiveRecord
		for i := range 50 {
			records = append(records, archiveRecord{name: fmt.Sprintf("files/%02d.typ", i), data: bytes.Repeat([]byte("x"), 2048)})
		}
		path, _ := writeArchive(t, kind, records)
		for _, checks := range []int{4, 10, 40, 160} {
			base, cancel := context.WithCancel(context.Background())
			ctx := &cancelChecks{Context: base, cancel: cancel, remaining: checks}
			parent := t.TempDir()
			tree, err := (ArchiveSource{Path: path, Options: ArchiveOptions{TempDir: parent}}).Open(ctx)
			cancel()
			if !errors.Is(err, context.Canceled) || tree.FS != nil {
				if tree.Close != nil {
					_ = tree.Close()
				}
				t.Fatalf("%s cancel at %d: %+v %v", kind, checks, tree, err)
			}
			left, err := os.ReadDir(parent)
			if err != nil || len(left) != 0 {
				t.Fatalf("cancel resources: %v %v", left, err)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatal(err)
			}
		}
	}
}
