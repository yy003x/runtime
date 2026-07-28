package command

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDecodeCommandProfile(t *testing.T) {
	profile, err := Decode(strings.NewReader(`{
		"binary":"codex",
		"args":["-c","model_reasoning_effort=xhigh","${EXTRA_ARG}"],
		"env":{"CODEX_HOME":"${HOME}/.codex-aip","REMOVE_ME":null},
		"transport":"tty",
		"prompt_delivery":"argv"
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if profile.Binary != "codex" || len(profile.Args) != 3 || profile.Env["REMOVE_ME"] != nil {
		t.Fatalf("profile=%#v", profile)
	}
	for _, input := range []string{
		`{"binary":"codex","transport":"tty","prompt_delivery":"argv","model":"forbidden"}`,
		`{"binary":"","transport":"tty","prompt_delivery":"argv"}`,
		`{"binary":"codex","transport":"tty","prompt_delivery":"argv","args":["${BAD-NAME}"]}`,
		`{"binary":"codex","transport":"tty","prompt_delivery":"argv","env":{"BAD=NAME":"value"}}`,
		`{"binary":"codex","transport":"tmux","prompt_delivery":"stdin"}`,
	} {
		if _, err := Decode(strings.NewReader(input)); err == nil {
			t.Fatalf("Decode(%s) returned nil", input)
		}
	}
}

func TestLoadDirUsesFilenameIDsAndRejectsSymlinks(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "cc.json"), []byte(`{"binary":"claude","transport":"tty","prompt_delivery":"manual"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "cx.json"), []byte(`{"binary":"codex","args":["--help"],"transport":"tty","prompt_delivery":"argv"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	catalog, err := LoadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := catalog.IDs(); !reflect.DeepEqual(got, []string{"cc", "cx"}) {
		t.Fatalf("ids=%v", got)
	}
	profile, ok := catalog.Get("cx")
	if !ok || profile.Binary != "codex" {
		t.Fatalf("profile=%#v ok=%v", profile, ok)
	}

	linkRoot := t.TempDir()
	target := filepath.Join(linkRoot, "target.json")
	if err := os.WriteFile(target, []byte(`{"binary":"codex","transport":"tty","prompt_delivery":"manual"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(linkRoot, "cx.json")); err == nil {
		if _, err := LoadDir(linkRoot); err == nil {
			t.Fatal("symlink command profile was accepted")
		}
	}
}

func TestCatalogRejectsFixedNamespaceConflict(t *testing.T) {
	if _, err := NewCatalog(
		map[string]Profile{"command": {
			Binary: "codex", Transport: TransportTTY, PromptDelivery: PromptManual,
		}},
		"command",
	); err == nil || !strings.Contains(err.Error(), "fixed namespace") {
		t.Fatalf("error=%v", err)
	}
}
