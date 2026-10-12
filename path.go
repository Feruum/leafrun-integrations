package integrations

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"unicode"
	"unicode/utf8"
)

// portablePath never cleans away traversal components. Only conventional leading
// ./ prefixes and a directory's final slash are normalized for archive records.
func portablePath(name string, directory, archive bool) (string, error) {
	if name == "" || strings.HasPrefix(name, "/") {
		return "", errors.New("empty or absolute path")
	}
	if !utf8.ValidString(name) {
		return "", errors.New("path is not UTF-8")
	}
	if strings.ContainsAny(name, "\\:") {
		return "", errors.New("backslash or colon in path")
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "", errors.New("control character in path")
		}
	}
	if archive {
		for strings.HasPrefix(name, "./") {
			name = strings.TrimPrefix(name, "./")
		}
		if strings.HasPrefix(name, "/") {
			return "", errors.New("empty leading path component")
		}
		if directory {
			name = strings.TrimSuffix(name, "/")
		}
	}
	if name == "." || (archive && directory && name == "") {
		return ".", nil
	}
	if !fs.ValidPath(name) {
		return "", errors.New("path is not a clean relative logical path")
	}
	for _, component := range strings.Split(name, "/") {
		if strings.ContainsAny(component, `<>"|?*`) {
			return "", errors.New("Windows-unsafe character in path")
		}
		if strings.HasSuffix(component, ".") || strings.HasSuffix(component, " ") {
			return "", errors.New("trailing dot or space in path")
		}
		stem, _, _ := strings.Cut(component, ".")
		stem = strings.ToUpper(strings.TrimRight(stem, " "))
		switch stem {
		case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
			return "", errors.New("Windows reserved device in path")
		}
		if len(stem) >= 4 && (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) {
			suffix := strings.TrimPrefix(strings.TrimPrefix(stem, "COM"), "LPT")
			if len([]rune(suffix)) == 1 && strings.ContainsRune("123456789¹²³", []rune(suffix)[0]) {
				return "", errors.New("Windows reserved device in path")
			}
		}
	}
	return name, nil
}

// foldPath gives every Unicode simple-fold cycle one representative, including
// Kelvin-sign and final-sigma variants that lowercasing alone would miss.
func foldPath(name string) string {
	var result strings.Builder
	for _, r := range name {
		minimum := r
		for next := unicode.SimpleFold(r); next != r; next = unicode.SimpleFold(next) {
			if next < minimum {
				minimum = next
			}
		}
		result.WriteRune(minimum)
	}
	return result.String()
}

type registeredPath struct {
	name      string
	directory bool
	explicit  bool
}

type pathRegistry map[string]registeredPath

// add distinguishes automatically created ancestors from actual directory
// records. An implicit ancestor may receive its first explicit directory entry.
func (r pathRegistry) add(name string, directory bool) error {
	if name == "." && !directory {
		return errors.New("root entry is not a directory")
	}
	parts := strings.Split(name, "/")
	for i := range parts {
		current := strings.Join(parts[:i+1], "/")
		last := i == len(parts)-1
		key := foldPath(current)
		previous, exists := r[key]
		if exists {
			if previous.name != current {
				return fmt.Errorf("case-insensitive collision between %q and %q", previous.name, current)
			}
			if !previous.directory || (last && (!directory || previous.explicit)) {
				return fmt.Errorf("duplicate or file/directory conflict at %q", current)
			}
			if last {
				previous.explicit = true
				r[key] = previous
			}
			continue
		}
		r[key] = registeredPath{name: current, directory: !last || directory, explicit: last}
	}
	return nil
}
