package integrations

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
	"sort"
)

type zipSpan struct{ start, end int64 }

// validateZIPLayout verifies local names and record boundaries as well as the
// central directory consumed by archive/zip. A central entry cannot hide an
// unsafe local name, overlapping data, or a malformed directory stream.
func validateZIPLayout(ctx context.Context, file *os.File, size int64, reader *zip.Reader) error {
	start, length, count, err := zipDirectory(file, size)
	if err != nil {
		return sourceErr(ErrorArchive, "ZIP directory", "", err)
	}
	if count != uint64(len(reader.File)) {
		return sourceErr(ErrorArchive, "ZIP directory count", "", zip.ErrFormat)
	}
	position := start
	var spans []zipSpan
	for _, entry := range reader.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		var fixed [46]byte
		if position > start+length-46 {
			return sourceErr(ErrorArchive, "ZIP central header", entry.Name, io.ErrUnexpectedEOF)
		}
		if _, err := file.ReadAt(fixed[:], position); err != nil {
			return sourceErr(ErrorArchive, "ZIP central header", entry.Name, err)
		}
		if !bytes.Equal(fixed[:4], []byte{'P', 'K', 1, 2}) {
			return sourceErr(ErrorArchive, "ZIP central header", entry.Name, zip.ErrFormat)
		}
		nameLen := int64(binary.LittleEndian.Uint16(fixed[28:30]))
		extraLen := int64(binary.LittleEndian.Uint16(fixed[30:32]))
		commentLen := int64(binary.LittleEndian.Uint16(fixed[32:34]))
		variableLen := nameLen + extraLen + commentLen
		if variableLen > start+length-position-46 {
			return sourceErr(ErrorArchive, "ZIP central fields", entry.Name, io.ErrUnexpectedEOF)
		}
		variable := make([]byte, variableLen)
		if _, err := file.ReadAt(variable, position+46); err != nil {
			return sourceErr(ErrorArchive, "ZIP central fields", entry.Name, err)
		}
		if string(variable[:nameLen]) != entry.Name {
			return sourceErr(ErrorArchive, "ZIP central name", entry.Name, zip.ErrFormat)
		}
		localOffset, err := zipLocalOffset(fixed, variable[nameLen:nameLen+extraLen])
		if err != nil || localOffset > math.MaxInt64 {
			if err == nil {
				err = zip.ErrFormat
			}
			return sourceErr(ErrorArchive, "ZIP local offset", entry.Name, err)
		}
		span, err := validateZIPLocal(file, start, int64(localOffset), entry)
		if err != nil {
			return err
		}
		spans = append(spans, span)
		position += 46 + variableLen
	}
	if position != start+length {
		return sourceErr(ErrorArchive, "ZIP central tail", "", zip.ErrFormat)
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	var cursor int64
	for _, span := range spans {
		if span.start != cursor {
			return sourceErr(ErrorArchive, "ZIP local records", "", errors.New("overlapping or unreferenced local data"))
		}
		cursor = span.end
	}
	if cursor != start {
		return sourceErr(ErrorArchive, "ZIP local tail", "", errors.New("unreferenced data before central directory"))
	}
	return nil
}

// zipDirectory bounds the end record and ZIP64 locator before walking any
// central entry. Only a single standalone ZIP container is accepted.
func zipDirectory(file io.ReaderAt, size int64) (start, length int64, count uint64, err error) {
	if size < 22 {
		return 0, 0, 0, zip.ErrFormat
	}
	tail := make([]byte, min(size, 22+65535))
	if _, err := file.ReadAt(tail, size-int64(len(tail))); err != nil {
		return 0, 0, 0, err
	}
	index := -1
	for i := len(tail) - 22; i >= 0; i-- {
		if bytes.Equal(tail[i:i+4], []byte{'P', 'K', 5, 6}) && i+22+int(binary.LittleEndian.Uint16(tail[i+20:i+22])) == len(tail) {
			index = i
			break
		}
	}
	if index < 0 {
		return 0, 0, 0, zip.ErrFormat
	}
	end := tail[index : index+22]
	endOffset := size - int64(len(tail)) + int64(index)
	if binary.LittleEndian.Uint16(end[4:6]) != 0 || binary.LittleEndian.Uint16(end[6:8]) != 0 || binary.LittleEndian.Uint16(end[8:10]) != binary.LittleEndian.Uint16(end[10:12]) {
		return 0, 0, 0, zip.ErrFormat
	}
	count = uint64(binary.LittleEndian.Uint16(end[10:12]))
	dirLength := uint64(binary.LittleEndian.Uint32(end[12:16]))
	dirStart := uint64(binary.LittleEndian.Uint32(end[16:20]))
	if count == 0xffff || dirLength == 0xffffffff || dirStart == 0xffffffff {
		var locator [20]byte
		if endOffset < 20 {
			return 0, 0, 0, zip.ErrFormat
		}
		if _, err := file.ReadAt(locator[:], endOffset-20); err != nil {
			return 0, 0, 0, err
		}
		if !bytes.Equal(locator[:4], []byte{'P', 'K', 6, 7}) || binary.LittleEndian.Uint32(locator[4:8]) != 0 || binary.LittleEndian.Uint32(locator[16:20]) != 1 {
			return 0, 0, 0, zip.ErrFormat
		}
		offset := binary.LittleEndian.Uint64(locator[8:16])
		if offset > uint64(endOffset-20) || uint64(endOffset-20)-offset < 56 {
			return 0, 0, 0, zip.ErrFormat
		}
		var extended [56]byte
		if _, err := file.ReadAt(extended[:], int64(offset)); err != nil {
			return 0, 0, 0, err
		}
		recordSize := binary.LittleEndian.Uint64(extended[4:12])
		if !bytes.Equal(extended[:4], []byte{'P', 'K', 6, 6}) || recordSize < 44 || recordSize != uint64(endOffset-20)-offset-12 || binary.LittleEndian.Uint32(extended[16:20]) != 0 || binary.LittleEndian.Uint32(extended[20:24]) != 0 || binary.LittleEndian.Uint64(extended[24:32]) != binary.LittleEndian.Uint64(extended[32:40]) {
			return 0, 0, 0, zip.ErrFormat
		}
		count = binary.LittleEndian.Uint64(extended[32:40])
		dirLength = binary.LittleEndian.Uint64(extended[40:48])
		dirStart = binary.LittleEndian.Uint64(extended[48:56])
		endOffset = int64(offset)
	}
	if dirStart > uint64(endOffset) || dirLength != uint64(endOffset)-dirStart {
		return 0, 0, 0, zip.ErrFormat
	}
	return int64(dirStart), int64(dirLength), count, nil
}

func zipLocalOffset(header [46]byte, extra []byte) (uint64, error) {
	offset := uint64(binary.LittleEndian.Uint32(header[42:46]))
	if binary.LittleEndian.Uint16(header[34:36]) != 0 {
		return 0, zip.ErrFormat
	}
	needOffset := offset == 0xffffffff
	for len(extra) > 0 {
		if len(extra) < 4 {
			return 0, zip.ErrFormat
		}
		id, length := binary.LittleEndian.Uint16(extra[:2]), int(binary.LittleEndian.Uint16(extra[2:4]))
		if length > len(extra)-4 {
			return 0, zip.ErrFormat
		}
		field := extra[4 : 4+length]
		if id == 1 && needOffset {
			for _, value := range []uint32{binary.LittleEndian.Uint32(header[24:28]), binary.LittleEndian.Uint32(header[20:24])} {
				if value == 0xffffffff {
					if len(field) < 8 {
						return 0, zip.ErrFormat
					}
					field = field[8:]
				}
			}
			if len(field) < 8 {
				return 0, zip.ErrFormat
			}
			offset = binary.LittleEndian.Uint64(field[:8])
			needOffset = false
		}
		extra = extra[4+length:]
	}
	if needOffset {
		return 0, zip.ErrFormat
	}
	return offset, nil
}

func validateZIPLocal(file io.ReaderAt, centralStart, offset int64, entry *zip.File) (zipSpan, error) {
	if offset < 0 || offset > centralStart-30 || entry.CompressedSize64 > math.MaxInt64 {
		return zipSpan{}, sourceErr(ErrorArchive, "ZIP local bounds", entry.Name, zip.ErrFormat)
	}
	var header [30]byte
	if _, err := file.ReadAt(header[:], offset); err != nil {
		return zipSpan{}, sourceErr(ErrorArchive, "ZIP local header", entry.Name, err)
	}
	if !bytes.Equal(header[:4], []byte{'P', 'K', 3, 4}) {
		return zipSpan{}, sourceErr(ErrorArchive, "ZIP local header", entry.Name, zip.ErrFormat)
	}
	nameLen := int64(binary.LittleEndian.Uint16(header[26:28]))
	extraLen := int64(binary.LittleEndian.Uint16(header[28:30]))
	dataOffset := offset + 30 + nameLen + extraLen
	if dataOffset > centralStart || entry.CompressedSize64 > uint64(centralStart-dataOffset) {
		return zipSpan{}, sourceErr(ErrorArchive, "ZIP compressed bounds", entry.Name, io.ErrUnexpectedEOF)
	}
	name := make([]byte, nameLen)
	if _, err := file.ReadAt(name, offset+30); err != nil {
		return zipSpan{}, sourceErr(ErrorArchive, "ZIP local name", entry.Name, err)
	}
	if _, err := portablePath(string(name), entry.Mode().IsDir(), true); err != nil {
		return zipSpan{}, sourceErr(ErrorUnsafePath, "ZIP local name", string(name), err)
	}
	if string(name) != entry.Name || binary.LittleEndian.Uint16(header[6:8]) != entry.Flags || binary.LittleEndian.Uint16(header[8:10]) != entry.Method {
		return zipSpan{}, sourceErr(ErrorArchive, "ZIP header mismatch", entry.Name, zip.ErrFormat)
	}
	extra := make([]byte, extraLen)
	if _, err := file.ReadAt(extra, offset+30+nameLen); err != nil {
		return zipSpan{}, sourceErr(ErrorArchive, "ZIP local extra", entry.Name, err)
	}
	compressedSize, uncompressedSize, err := zipLocalSizes(header, extra)
	if err != nil {
		return zipSpan{}, sourceErr(ErrorArchive, "ZIP local ZIP64 sizes", entry.Name, err)
	}
	if entry.Flags&8 == 0 {
		if binary.LittleEndian.Uint32(header[14:18]) != entry.CRC32 {
			return zipSpan{}, sourceErr(ErrorArchive, "ZIP local CRC", entry.Name, zip.ErrChecksum)
		}
		if compressedSize != entry.CompressedSize64 || uncompressedSize != entry.UncompressedSize64 {
			return zipSpan{}, sourceErr(ErrorArchive, "ZIP local size", entry.Name, zip.ErrFormat)
		}
	}
	end := dataOffset + int64(entry.CompressedSize64)
	if entry.Flags&8 != 0 {
		var first [4]byte
		if _, err := file.ReadAt(first[:], end); err != nil {
			return zipSpan{}, sourceErr(ErrorArchive, "ZIP descriptor", entry.Name, err)
		}
		if bytes.Equal(first[:], []byte{'P', 'K', 7, 8}) {
			end += 4
		}
		zip64 := entry.CompressedSize64 >= 0xffffffff || entry.UncompressedSize64 >= 0xffffffff || binary.LittleEndian.Uint32(header[18:22]) == 0xffffffff || binary.LittleEndian.Uint32(header[22:26]) == 0xffffffff
		length := int64(12)
		if zip64 {
			length = 20
		}
		if end > centralStart-length {
			return zipSpan{}, sourceErr(ErrorArchive, "ZIP descriptor bounds", entry.Name, io.ErrUnexpectedEOF)
		}
		descriptor := make([]byte, length)
		if _, err := file.ReadAt(descriptor, end); err != nil {
			return zipSpan{}, sourceErr(ErrorArchive, "ZIP descriptor", entry.Name, err)
		}
		if binary.LittleEndian.Uint32(descriptor[:4]) != entry.CRC32 {
			return zipSpan{}, sourceErr(ErrorArchive, "ZIP descriptor CRC", entry.Name, zip.ErrChecksum)
		}
		compressed, uncompressed := uint64(binary.LittleEndian.Uint32(descriptor[4:8])), uint64(binary.LittleEndian.Uint32(descriptor[8:12]))
		if zip64 {
			compressed = binary.LittleEndian.Uint64(descriptor[4:12])
			uncompressed = binary.LittleEndian.Uint64(descriptor[12:20])
		}
		if compressed != entry.CompressedSize64 || uncompressed != entry.UncompressedSize64 {
			return zipSpan{}, sourceErr(ErrorArchive, "ZIP descriptor size", entry.Name, zip.ErrFormat)
		}
		end += length
	}
	return zipSpan{start: offset, end: end}, nil
}

func zipLocalSizes(header [30]byte, extra []byte) (compressed, uncompressed uint64, err error) {
	compressed = uint64(binary.LittleEndian.Uint32(header[18:22]))
	uncompressed = uint64(binary.LittleEndian.Uint32(header[22:26]))
	needCompressed, needUncompressed := compressed == 0xffffffff, uncompressed == 0xffffffff
	for len(extra) > 0 {
		if len(extra) < 4 {
			return 0, 0, zip.ErrFormat
		}
		id, length := binary.LittleEndian.Uint16(extra[:2]), int(binary.LittleEndian.Uint16(extra[2:4]))
		if length > len(extra)-4 {
			return 0, 0, zip.ErrFormat
		}
		field := extra[4 : 4+length]
		if id == 1 {
			if needUncompressed {
				if len(field) < 8 {
					return 0, 0, zip.ErrFormat
				}
				uncompressed = binary.LittleEndian.Uint64(field[:8])
				field = field[8:]
				needUncompressed = false
			}
			if needCompressed {
				if len(field) < 8 {
					return 0, 0, zip.ErrFormat
				}
				compressed = binary.LittleEndian.Uint64(field[:8])
				needCompressed = false
			}
		}
		extra = extra[4+length:]
	}
	if needCompressed || needUncompressed {
		return 0, 0, zip.ErrFormat
	}
	return compressed, uncompressed, nil
}
