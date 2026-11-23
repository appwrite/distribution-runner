package lease

import (
	"bytes"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/appwrite/distribution-runner/agent/protocol"
)

var awkward = map[string]string{
	"EMPTY":      "",
	"SPACES":     "a b  c",
	"QUOTES":     `it's "double" and 'single'`,
	"ONLY_QUOTE": "'",
	"SHELL":      "$HOME `id` $(id) ; && || > /tmp/x",
	"NEWLINES":   "first\nsecond\n",
	"BACKSLASH":  `C:\path\'`,
	"UNICODE":    "héllo ✓",
}

func TestEnvironmentRoundTrip(t *testing.T) {
	encoded := EncodeEnvironment(awkward)

	decoded, err := ParseEnvironment(encoded)

	if err != nil {
		t.Fatalf("ParseEnvironment: %v", err)
	}
	if !maps.Equal(decoded, awkward) {
		t.Errorf("decoded = %q, want %q", decoded, awkward)
	}
}

func TestEnvironmentSourcedByShell(t *testing.T) {
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh on PATH")
	}
	file := filepath.Join(t.TempDir(), FileEnvironment)
	if err := os.WriteFile(file, EncodeEnvironment(awkward), 0o600); err != nil {
		t.Fatal(err)
	}
	names := slices.Sorted(maps.Keys(awkward))
	script := `. "$1"; shift; for name in "$@"; do eval "printf '%s\0' \"\$$name\""; done`

	output, err := exec.Command(shell, append([]string{"-c", script, "sh", file}, names...)...).Output()

	if err != nil {
		t.Fatalf("sourcing the environment failed: %v", err)
	}
	values := strings.Split(strings.TrimSuffix(string(output), "\x00"), "\x00")
	if len(values) != len(names) {
		t.Fatalf("got %d values, want %d: %q", len(values), len(names), output)
	}
	for index, name := range names {
		if values[index] != awkward[name] {
			t.Errorf("%s = %q, want %q", name, values[index], awkward[name])
		}
	}
}

func TestEnvironmentDerivedValuesWin(t *testing.T) {
	lease := protocol.Lease{
		Environment: protocol.Map[string]{VariableVersionName: "9.9.9", VariableCI: "false", "CUSTOM": "kept"},
		Version:     protocol.Version{Name: "1.0.0", Code: 3},
		Signing:     protocol.Signing{Password: "secret", Certificate: []byte("certificate"), TeamID: "TEAM"},
	}

	variables, err := Environment(lease)

	if err != nil {
		t.Fatalf("Environment: %v", err)
	}
	if variables[VariableVersionName] != "1.0.0" || variables[VariableVersionCode] != "3" || variables[VariableCI] != "true" || variables["CUSTOM"] != "kept" {
		t.Errorf("variables = %q", variables)
	}
	if encoded := EncodeEnvironment(variables); bytes.Contains(encoded, []byte("secret")) || bytes.Contains(encoded, []byte("certificate")) {
		t.Errorf("signing material reached the environment: %s", encoded)
	}
}

func TestEnvironmentRejectsInvalidNames(t *testing.T) {
	for _, name := range []string{"", "1ABC", "A-B", "A B", "A=B", "$A"} {
		_, err := Environment(protocol.Lease{Environment: protocol.Map[string]{name: "value"}})
		if err == nil {
			t.Errorf("Environment accepted name %q", name)
		}
	}
}

func TestParseEnvironmentRejectsMalformed(t *testing.T) {
	for _, input := range []string{
		"A='x'",
		"export A=x\n",
		"export A='x\n",
		"export 1A='x'\n",
		"declare A='x'\n",
	} {
		if _, err := ParseEnvironment([]byte(input)); err == nil {
			t.Errorf("ParseEnvironment accepted %q", input)
		}
	}
}

func TestBundleName(t *testing.T) {
	for name, valid := range map[string]bool{
		"com.example.app":        true,
		"com.example-app.widget": true,
		"io.app2.Extension":      true,
		"com.example_app":        false,
		".com.example":           false,
		"com/example":            false,
		"":                       false,
		"com.exämple":            false,
	} {
		if got := bundleName(name); got != valid {
			t.Errorf("bundleName(%q) = %v, want %v", name, got, valid)
		}
	}
}
