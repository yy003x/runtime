// Package strictjson provides bounded, single-value JSON decoding for Runtime
// contracts and configuration documents.
package strictjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// ValidationError identifies document syntax, schema shape, or bounded-file
// shape errors supplied by a caller. Filesystem inspection/open/read failures
// deliberately remain ordinary errors so transports can classify them as
// internal failures.
type ValidationError struct {
	cause error
}

func (err *ValidationError) Error() string {
	if err == nil || err.cause == nil {
		return ""
	}
	return err.cause.Error()
}

func (err *ValidationError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.cause
}

// IsValidation reports whether err represents caller-controlled JSON document
// or regular-file shape validation.
func IsValidation(err error) bool {
	var target *ValidationError
	return errors.As(err, &target)
}

func validationError(err error) error {
	if err == nil || IsValidation(err) {
		return err
	}
	return &ValidationError{cause: err}
}

func validationErrorf(format string, args ...any) error {
	return validationError(fmt.Errorf(format, args...))
}

func Decode(reader io.Reader, maxBytes int64, target any) error {
	data, err := readDocument(reader, maxBytes)
	if err != nil {
		return err
	}
	return decodeDocument(data, target)
}

func readDocument(reader io.Reader, maxBytes int64) ([]byte, error) {
	if reader == nil {
		return nil, fmt.Errorf("JSON reader is required")
	}
	if maxBytes <= 0 {
		return nil, fmt.Errorf("JSON size limit must be positive")
	}
	data, err := io.ReadAll(io.LimitReader(reader, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read JSON: %w", err)
	}
	if int64(len(data)) > maxBytes {
		return nil, validationErrorf("JSON exceeds %d bytes", maxBytes)
	}
	if !utf8.Valid(data) {
		return nil, validationErrorf("JSON must be valid UTF-8")
	}
	if err := rejectDuplicateObjectNames(data); err != nil {
		return nil, validationError(err)
	}
	return data, nil
}

func decodeDocument(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		var invalidTarget *json.InvalidUnmarshalError
		if errors.As(err, &invalidTarget) {
			return fmt.Errorf("decode JSON: %w", err)
		}
		return validationErrorf("decode JSON: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return validationErrorf("JSON contains multiple values")
		}
		return validationErrorf("decode trailing JSON: %w", err)
	}
	return nil
}

// DecodeObject applies Decode's strictness and additionally requires the
// document root to be a non-null JSON object.
func DecodeObject(reader io.Reader, maxBytes int64, target any) error {
	data, err := readObjectDocument(reader, maxBytes)
	if err != nil {
		return err
	}
	return decodeDocument(data, target)
}

// DecodeObjectWithNullPolicy additionally rejects every explicit JSON null
// except paths accepted by allow. Omitting a field is unaffected.
func DecodeObjectWithNullPolicy(
	reader io.Reader,
	maxBytes int64,
	target any,
	allow func(path []string) bool,
) error {
	data, err := readObjectDocument(reader, maxBytes)
	if err != nil {
		return err
	}
	if err := RejectNulls(data, allow); err != nil {
		return err
	}
	return decodeDocument(data, target)
}

// DecodeObjectNoNulls rejects every explicit JSON null in a non-null object.
func DecodeObjectNoNulls(reader io.Reader, maxBytes int64, target any) error {
	return DecodeObjectWithNullPolicy(reader, maxBytes, target, nil)
}

func readObjectDocument(reader io.Reader, maxBytes int64) ([]byte, error) {
	data, err := readDocument(reader, maxBytes)
	if err != nil {
		return nil, err
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, validationErrorf("JSON root must be a non-null object")
	}
	return data, nil
}

func rejectDuplicateObjectNames(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var walkValue func() error
	walkValue = func() error {
		token, err := decoder.Token()
		if err != nil {
			return fmt.Errorf("decode JSON: %w", err)
		}
		delimiter, composite := token.(json.Delim)
		if !composite {
			return nil
		}
		switch delimiter {
		case '{':
			seen := make(map[string]struct{})
			for decoder.More() {
				token, err := decoder.Token()
				if err != nil {
					return fmt.Errorf("decode JSON object name: %w", err)
				}
				name, ok := token.(string)
				if !ok {
					return fmt.Errorf("decode JSON object name: expected string")
				}
				if _, exists := seen[name]; exists {
					return fmt.Errorf("JSON object contains duplicate field %q", name)
				}
				seen[name] = struct{}{}
				if err := walkValue(); err != nil {
					return err
				}
			}
			token, err := decoder.Token()
			if err != nil {
				return fmt.Errorf("decode JSON object: %w", err)
			}
			if token != json.Delim('}') {
				return fmt.Errorf("decode JSON object: expected closing delimiter")
			}
		case '[':
			for decoder.More() {
				if err := walkValue(); err != nil {
					return err
				}
			}
			token, err := decoder.Token()
			if err != nil {
				return fmt.Errorf("decode JSON array: %w", err)
			}
			if token != json.Delim(']') {
				return fmt.Errorf("decode JSON array: expected closing delimiter")
			}
		default:
			return fmt.Errorf("decode JSON: unexpected delimiter %q", delimiter)
		}
		return nil
	}
	if err := walkValue(); err != nil {
		return err
	}
	return nil
}

// RejectNulls applies a document-specific null policy after strict decoding.
// JSON null is otherwise silently accepted by encoding/json for many Go
// scalar, struct, slice, and map fields, which can diverge from a public JSON
// Schema. The callback receives the object/array path to each null value.
func RejectNulls(data []byte, allow func(path []string) bool) error {
	var document any
	if err := json.Unmarshal(data, &document); err != nil {
		return validationErrorf("decode JSON null policy: %w", err)
	}
	var walk func(any, []string) error
	walk = func(value any, path []string) error {
		switch current := value.(type) {
		case nil:
			if allow != nil && allow(append([]string(nil), path...)) {
				return nil
			}
			label := strings.Join(path, ".")
			if label == "" {
				label = "<root>"
			}
			return validationErrorf("JSON field %q must not be null", label)
		case map[string]any:
			names := make([]string, 0, len(current))
			for name := range current {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				if err := walk(
					current[name], append(path, name),
				); err != nil {
					return err
				}
			}
		case []any:
			for index, item := range current {
				if err := walk(
					item, append(path, fmt.Sprintf("[%d]", index)),
				); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(document, nil)
}

func ReadRegularFile(path string, maxBytes int64, target any) error {
	return readRegularFileWith(path, maxBytes, target, openRegularNoFollow)
}

// ReadRegularFileBytes returns a bounded snapshot of a regular, non-symlink
// file. Caller-controlled file shape errors are ValidationError values; actual
// Lstat/open/stat/read failures remain ordinary errors.
func ReadRegularFileBytes(path string, maxBytes int64) ([]byte, error) {
	return readRegularFileBytesWith(path, maxBytes, openRegularNoFollow)
}

func readRegularFileWith(
	path string,
	maxBytes int64,
	target any,
	openFile func(string) (*os.File, error),
) error {
	data, err := readRegularFileBytesWith(path, maxBytes, openFile)
	if err != nil {
		return err
	}
	if err := Decode(bytes.NewReader(data), maxBytes, target); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func readRegularFileBytesWith(
	path string,
	maxBytes int64,
	openFile func(string) (*os.File, error),
) ([]byte, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("file size limit must be positive")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, validationErrorf("%s must not be a symlink", path)
	}
	if !info.Mode().IsRegular() {
		return nil, validationErrorf("%s must be a regular file", path)
	}
	if info.Size() > maxBytes {
		return nil, validationErrorf("%s exceeds %d bytes", path, maxBytes)
	}
	file, err := openFile(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect opened %s: %w", path, err)
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		return nil, validationErrorf("%s changed while opening", path)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if int64(len(data)) > maxBytes {
		return nil, validationErrorf("%s exceeds %d bytes", path, maxBytes)
	}
	return data, nil
}

func openRegularNoFollow(path string) (*os.File, error) {
	fd, err := unix.Open(
		path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0,
	)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("open regular file %s", path)
	}
	return file, nil
}
