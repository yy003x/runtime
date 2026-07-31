package strictjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
)

func TestDecodeIsBoundedAndStrict(t *testing.T) {
	var target struct {
		Name string `json:"name"`
	}
	if err := Decode(strings.NewReader(`{"name":"ok"}`), 64, &target); err != nil {
		t.Fatal(err)
	}
	if target.Name != "ok" {
		t.Fatalf("name=%q", target.Name)
	}
	for _, input := range []string{
		`{"name":"ok","extra":true}`,
		`{"name":"one"} {"name":"two"}`,
		`{"name":"one","name":"two"}`,
	} {
		if err := Decode(strings.NewReader(input), 64, &target); err == nil {
			t.Fatalf("Decode(%q) returned nil", input)
		}
	}
	if err := Decode(strings.NewReader(`{"name":"too large"}`), 4, &target); err == nil {
		t.Fatal("oversized JSON was accepted")
	}
	invalidUTF8 := append([]byte(`{"name":"`), 0xff)
	invalidUTF8 = append(invalidUTF8, []byte(`"}`)...)
	if err := Decode(bytes.NewReader(invalidUTF8), 64, &target); err == nil {
		t.Fatal("invalid UTF-8 JSON was accepted")
	}
	var rawTarget struct {
		Schema json.RawMessage `json:"schema"`
	}
	if err := Decode(
		strings.NewReader(`{"schema":{"maximum":1e400}}`),
		64, &rawTarget,
	); err != nil {
		t.Fatalf("RawMessage large number was rejected: %v", err)
	}
	var nestedTarget struct {
		Nested struct {
			Value int `json:"value"`
		} `json:"nested"`
	}
	err := Decode(
		strings.NewReader(`{"nested":{"value":1,"value":2}}`),
		64, &nestedTarget,
	)
	if err == nil || !strings.Contains(err.Error(), "duplicate field") {
		t.Fatalf("nested duplicate error=%v", err)
	}
}

func TestDecodeObjectRequiresStrictNonNullObject(t *testing.T) {
	var target struct {
		Name     string  `json:"name"`
		Optional *string `json:"optional,omitempty"`
	}
	if err := DecodeObject(
		strings.NewReader(`{"name":"ok"}`), 64, &target,
	); err != nil {
		t.Fatal(err)
	}
	if target.Name != "ok" {
		t.Fatalf("name=%q", target.Name)
	}
	if err := DecodeObject(
		strings.NewReader(`{"name":"ok","optional":null}`), 64, &target,
	); err != nil {
		t.Fatalf("DecodeObject rejected nested null: %v", err)
	}
	for _, input := range []string{
		`null`,
		`[]`,
		`"value"`,
		`{"name":"one","name":"two"}`,
		`{"name":"ok","unknown":true}`,
		`{"name":"one"} {"name":"two"}`,
	} {
		if err := DecodeObject(
			strings.NewReader(input), 64, &target,
		); err == nil {
			t.Fatalf("DecodeObject(%q) returned nil", input)
		}
	}
	if err := DecodeObject(
		strings.NewReader(`{"name":"too large"}`), 4, &target,
	); err == nil {
		t.Fatal("oversized JSON object was accepted")
	}
}

func TestDecodeObjectNullPolicies(t *testing.T) {
	type nested struct {
		Value *string `json:"value,omitempty"`
	}
	var target struct {
		Name   string    `json:"name,omitempty"`
		Nested nested    `json:"nested,omitempty"`
		Items  []*string `json:"items,omitempty"`
	}
	for _, input := range []string{
		`{"name":null}`,
		`{"nested":null}`,
		`{"nested":{"value":null}}`,
		`{"items":[null]}`,
	} {
		if err := DecodeObjectNoNulls(
			strings.NewReader(input), 128, &target,
		); err == nil || !strings.Contains(err.Error(), "must not be null") {
			t.Fatalf("DecodeObjectNoNulls(%q) error=%v", input, err)
		}
	}
	if err := DecodeObjectNoNulls(
		strings.NewReader(`{"nested":{}}`), 128, &target,
	); err != nil {
		t.Fatalf("omitted fields were rejected: %v", err)
	}
	if err := DecodeObjectWithNullPolicy(
		strings.NewReader(`{"nested":{"value":null}}`), 128, &target,
		func(path []string) bool {
			return strings.Join(path, ".") == "nested.value"
		},
	); err != nil {
		t.Fatalf("allowed null path was rejected: %v", err)
	}
}

func TestRejectNullsUsesDocumentPolicy(t *testing.T) {
	document := []byte(`{
		"nullable":{"value":null},
		"required":{"nested":"ok"},
		"items":[1,null]
	}`)
	err := RejectNulls(document, func(path []string) bool {
		return strings.Join(path, ".") == "nullable.value"
	})
	if err == nil || !strings.Contains(err.Error(), "items.[1]") {
		t.Fatalf("error=%v", err)
	}
	if err := RejectNulls(document, func(path []string) bool {
		switch strings.Join(path, ".") {
		case "nullable.value", "items.[1]":
			return true
		default:
			return false
		}
	}); err != nil {
		t.Fatal(err)
	}
}

func TestReadRegularFilePinsTheInspectedFile(t *testing.T) {
	for _, testCase := range []struct {
		name string
		swap func(string, string) error
	}{
		{
			name: "symlink",
			swap: func(current, replacement string) error {
				return os.Symlink(replacement, current)
			},
		},
		{
			name: "regular_file",
			swap: func(current, replacement string) error {
				return os.Rename(replacement, current)
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "runtime.json")
			replacement := filepath.Join(root, "replacement.json")
			if err := os.WriteFile(
				path, []byte(`{"name":"original"}`), 0o600,
			); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(
				replacement, []byte(`{"name":"replacement"}`), 0o600,
			); err != nil {
				t.Fatal(err)
			}
			var target struct {
				Name string `json:"name"`
			}
			err := readRegularFileWith(
				path, 1024, &target,
				func(current string) (*os.File, error) {
					if err := os.Remove(current); err != nil {
						return nil, err
					}
					if err := testCase.swap(
						current, replacement,
					); err != nil {
						return nil, err
					}
					return openRegularNoFollow(current)
				},
			)
			if err == nil {
				t.Fatal("inspected file swap was accepted")
			}
			if target.Name != "" {
				t.Fatalf("swapped content was decoded: %#v", target)
			}
		})
	}
}

func TestValidationErrorsDistinguishDocumentsFromReaderIO(t *testing.T) {
	for _, input := range []string{
		`{"name":`,
		`{"name":"one","name":"two"}`,
		`{"name":"ok","unknown":true}`,
		`{"name":"one"} {"name":"two"}`,
	} {
		var target struct {
			Name string `json:"name"`
		}
		err := Decode(strings.NewReader(input), 128, &target)
		if err == nil || !IsValidation(err) {
			t.Fatalf("input=%q error=%v, want ValidationError", input, err)
		}
	}

	injected := errors.New("injected reader failure")
	var target struct {
		Name string `json:"name"`
	}
	err := Decode(iotest.ErrReader(injected), 128, &target)
	if err == nil || IsValidation(err) || !errors.Is(err, injected) {
		t.Fatalf("reader error=%v, want unclassified injected error", err)
	}
	if err := Decode(strings.NewReader(`{"name":"ok"}`), 128, nil); err == nil ||
		IsValidation(err) {
		t.Fatalf("invalid decode target error=%v, want unclassified error", err)
	}
}

func TestReadRegularFileBytesClassifiesShapeButNotIO(t *testing.T) {
	root := t.TempDir()
	regular := filepath.Join(root, "input.json")
	if err := os.WriteFile(regular, []byte(`{"ok":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, "directory")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(root, "symlink.json")
	if err := os.Symlink(regular, symlink); err != nil {
		t.Fatal(err)
	}
	oversized := filepath.Join(root, "oversized.json")
	if err := os.WriteFile(oversized, []byte(strings.Repeat("x", 65)), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{directory, symlink, oversized} {
		if _, err := ReadRegularFileBytes(path, 64); err == nil ||
			!IsValidation(err) {
			t.Fatalf("path=%s error=%v, want ValidationError", path, err)
		}
	}

	missing := filepath.Join(root, "missing.json")
	if _, err := ReadRegularFileBytes(missing, 64); err == nil ||
		IsValidation(err) || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing error=%v, want unclassified not-exist", err)
	}

	injected := errors.New("injected open failure")
	if _, err := readRegularFileBytesWith(
		regular, 64,
		func(string) (*os.File, error) { return nil, injected },
	); err == nil || IsValidation(err) || !errors.Is(err, injected) {
		t.Fatalf("open error=%v, want unclassified injected error", err)
	}

	if _, err := readRegularFileBytesWith(
		regular, 64,
		func(path string) (*os.File, error) {
			return os.OpenFile(path, os.O_WRONLY, 0)
		},
	); err == nil || IsValidation(err) {
		t.Fatalf("read error=%v, want unclassified I/O error", err)
	}
}
