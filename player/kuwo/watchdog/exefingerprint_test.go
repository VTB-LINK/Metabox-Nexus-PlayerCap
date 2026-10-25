package watchdog

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A filename or copied version-directory layout is not sufficient authority to
// terminate a running process. No real installation is read; knownBuilds is
// replaced with fixtures whose hashes come from files written here.
func writeExe(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func useFixtureBuilds(t *testing.T) {
	t.Helper()
	original := knownBuilds
	t.Cleanup(func() { knownBuilds = original })
	knownBuilds = map[string]string{"w1": sha256Hex("W1-body"), "ug1": sha256Hex("UG1-body")}
}

func buildPath(root, dir, bin, content string, t *testing.T) string {
	return writeExe(t, filepath.Join(root, dir, "bin", bin), content)
}

func TestVerifiedBuildPassesRestartGate(t *testing.T) {
	useFixtureBuilds(t)
	root := t.TempDir()
	for _, exe := range []string{
		buildPath(root, "9.6.0.0_W1", "kwmusic.exe", "W1-body", t),
		buildPath(root, "9.6.0.0_UG1", "kwmusic.exe", "UG1-body", t),
	} {
		if err := verifyExecutable(exe); err != nil {
			t.Fatalf("verified build rejected %q: %v", exe, err)
		}
	}
	// Directory and filename case differences still pass: the file exists at the
	// literal path, and the structure comparison is Windows case-insensitive.
	mixed := writeExe(t, filepath.Join(root, "9.6.0.0_w1", "BIN", "KWMUSIC.EXE"), "W1-body")
	if err := verifyExecutable(mixed); err != nil {
		t.Fatalf("case variation rejected: %v", err)
	}
}

// Each case below is a valid W1 build with exactly one attribute wrong, so it
// isolates one guard. 变异自证（隔离副本，实测）：去掉文件名检查 → wrong filename
// 一例红；去掉父目录检查 → parent not bin 一例红；哈希比较改成恒真 → wrong hash
// 一例红。knownBuilds 成员检查与哈希比较重叠（未知后缀的 want 为空串，没有文件
// 哈希得到空串），单独去掉它仍被哈希拒绝，故那一条不独立变红。
func TestUnverifiedExecutableNeverPassesRestartGate(t *testing.T) {
	useFixtureBuilds(t)
	root := t.TempDir()
	for _, exe := range []string{
		"",
		TargetProcessName,
		filepath.Join("relative", "9.6.0.0_W1", "bin", TargetProcessName),                   // not absolute
		buildPath(root, "9.6.0.0_W1", "kwmusic.exe", "tampered", t),                         // wrong hash (only)
		buildPath(root, "9.6.0.0_X9", "kwmusic.exe", "W1-body", t),                          // unknown suffix, known hash
		buildPath(root, "9.6.0.0_W1", "launcher.exe", "W1-body", t),                         // wrong filename (only)
		buildPath(root, "9.7.0.0_W1", "kwmusic.exe", "W1-body", t),                          // wrong numeric version
		writeExe(t, filepath.Join(root, "9.6.0.0_W1", "lib", TargetProcessName), "W1-body"), // parent not bin (only)
		filepath.Join(root, "9.6.0.0_W1", "bin", TargetProcessName, ".."),                   // ".." collapses off the filename
		filepath.Join(root, "9.6.0.0_W1", "bin", "missing.exe"),                             // absent
	} {
		if err := verifyExecutable(exe); !errors.Is(err, ErrVersionUnsupported) {
			t.Fatalf("accepted %q: %v", exe, err)
		}
	}
}

// A path that only reduces to a valid layout once ".." is collapsed is refused as
// written. The strings keep the ".." (filepath.Join would clean it away), and the
// target file exists with the known hash, so only the ".." rule can reject them.
// 变异自证：去掉 verifyExecutable 开头的 ".." 检查，本用例变红（实测）。
func TestDotDotPathNeverPassesRestartGate(t *testing.T) {
	useFixtureBuilds(t)
	root := t.TempDir()
	buildPath(root, "9.6.0.0_W1", "kwmusic.exe", "W1-body", t)
	sep := string(filepath.Separator)
	good := root + sep + "9.6.0.0_W1" + sep + "bin" + sep + "kwmusic.exe"
	if err := verifyExecutable(good); err != nil {
		t.Fatalf("control rejected: %v", err)
	}
	for _, exe := range []string{
		root + sep + "other" + sep + ".." + sep + "9.6.0.0_W1" + sep + "bin" + sep + "kwmusic.exe",
		root + sep + "9.6.0.0_W1" + sep + "lib" + sep + ".." + sep + "bin" + sep + "kwmusic.exe",
		root + sep + "9.6.0.0_W1" + sep + "bin" + sep + "kwmusic.exe" + sep + ".." + sep + "kwmusic.exe",
		root + "/other/../9.6.0.0_W1/bin/kwmusic.exe", // forward slashes
	} {
		if err := verifyExecutable(exe); !errors.Is(err, ErrVersionUnsupported) {
			t.Fatalf("accepted %q: %v", exe, err)
		}
	}
}

func TestBuildSuffixParsesChannel(t *testing.T) {
	for dir, want := range map[string]string{"9.6.0.0_W1": "w1", "9.6.0.0_UG1": "ug1", "9.6.0.0_X9": "x9", "9.6.0.0_ug1": "ug1"} {
		if got, ok := buildSuffix(dir); !ok || got != want {
			t.Fatalf("%q: %q %v", dir, got, ok)
		}
	}
	for _, dir := range []string{"", "9.6.0.0", "9.6.0.0_", "9.6.0.1_w1", "bin", "x9.6.0.0_w1"} {
		if _, ok := buildSuffix(dir); ok {
			t.Fatalf("%q accepted", dir)
		}
	}
}
