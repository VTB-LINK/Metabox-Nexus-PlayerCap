package cdp

import (
	"errors"
	"testing"
)

const testEndpoint = "http://127.0.0.1:9224/json"

func netsongPage(dir string) target {
	return target{Type: "page", URL: "file:///C:/Program%20Files%20(x86)/kuwo/kuwomusic/" + dir + "/bin/html/webdata/netsong/index.html", WebSocketDebuggerURL: "ws://127.0.0.1:9224/devtools/page/12"}
}

func TestTargetSelectionAcceptsChannelSuffixes(t *testing.T) {
	for dir, want := range map[string]string{"9.6.0.0_W1": "w1", "9.6.0.0_UG1": "ug1", "9.6.0.0_X9": "x9", "9.6.0.0_abcdefghijklmnop": "abcdefghijklmnop"} {
		good := netsongPage(dir)
		got, build, err := selectTarget([]target{good}, testEndpoint)
		if err != nil || got != good.WebSocketDebuggerURL || build != want {
			t.Fatalf("%s rejected: %q %q %v", dir, got, build, err)
		}
	}
}

func TestTargetSelectionRejectsForeignAndRemoteTargets(t *testing.T) {
	good := netsongPage("9.6.0.0_W1")
	for _, mutate := range []func(*target){
		func(t *target) { t.Type = "browser" },
		func(t *target) { t.URL = "https://www.kuwo.cn/" },
		func(t *target) { t.URL = "file:///C:/other/index.html" },
		func(t *target) { t.URL = "file://remote/C:/9.6.0.0_W1/bin/html/webdata/netsong/index.html" },
		// Valid suffix, but the rest of the path is not the netsong document.
		func(t *target) { t.URL = "file:///C:/Kuwo/9.6.0.0_W1/bin/html/webdata/other/index.html" },
		func(t *target) { t.URL = "file:///C:/Kuwo/9.6.0.0_W1/bin/html/webdata/netsong/index.htm" },
		func(t *target) {
			t.URL = "file:///C:/Kuwo/9.6.0.0_W1/bin/html/webdata/netsong/../../../../x/index.html"
		},
		func(t *target) { t.WebSocketDebuggerURL = "ws://example.com:9224/devtools/page/12" },
		func(t *target) { t.WebSocketDebuggerURL = "ws://127.0.0.1:9223/devtools/page/12" },
		func(t *target) { t.WebSocketDebuggerURL = "ws://user@127.0.0.1:9224/devtools/page/12" },
		func(t *target) { t.WebSocketDebuggerURL = "ws://127.0.0.1:9224/other" },
	} {
		bad := good
		mutate(&bad)
		if _, _, err := selectTarget([]target{bad}, testEndpoint); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("accepted %+v: %v", bad, err)
		}
	}
	if _, _, err := selectTarget(nil, testEndpoint); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("empty target list: %v", err)
	}
}

// A netsong page of any other build directory is a version rejection, not a
// connection failure.
// 变异自证（隔离副本，实测）：buildDirPattern 的数字部分放宽为 `[0-9.]+` → 9.6.0.1_W1 红；
// 后缀长度上限改为 `+` → 17 字符一例红；去掉「netsong 目录只出现一次」→ 嵌套一例红；
// 改回 strings.ToLower → KELVIN SIGN 一例红；无可选页面时不报 ErrVersionUnsupported → 首例红。
func TestTargetSelectionRejectsUnverifiedBuildDirectory(t *testing.T) {
	for _, dir := range []string{
		"9.6.0.1_W1", "9.7.0.0_W1", "10.6.0.0_W1", "19.6.0.0_W1", "9.6.0.0", "9.6.0.0_",
		"9.6.0.0_abcdefghijklmnopq", "9.6.0.0_w%EF%BC%91", "9.6.0.0_w%C3%A9", "9.6.0.0_W-1", "9.6.0.0_W_1",
		// U+212A KELVIN SIGN lowers to ASCII "k" under strings.ToLower.
		"9.6.0.0_%E2%84%AA",
		"9a6a0a0_W1", "x9.6.0.0_W1", "9.6.0.0_W1x/..", "9.6.0.0_W1/../9.7.0.0_W1",
		"9.6.0.0_W%2F1", "9.6.0.0_W1/bin/html/webdata/netsong/9.6.0.0_UG1",
	} {
		_, _, err := selectTarget([]target{netsongPage(dir)}, testEndpoint)
		if !errors.Is(err, ErrVersionUnsupported) {
			t.Fatalf("%s: %v", dir, err)
		}
	}
	// "/" inside the suffix splits the directory; the remaining part is not 9.6.0.0_<s>.
	_, _, err := selectTarget([]target{netsongPage("9.6.0.0_W/1")}, testEndpoint)
	if !errors.Is(err, ErrVersionUnsupported) {
		t.Fatalf("split suffix: %v", err)
	}
	// An eligible page still wins over an unverified one; ordering does not matter.
	good, bad := netsongPage("9.6.0.0_UG1"), netsongPage("9.7.0.0_UG1")
	for _, list := range [][]target{{good, bad}, {bad, good}} {
		got, build, err := selectTarget(list, testEndpoint)
		if err != nil || got != good.WebSocketDebuggerURL || build != "ug1" {
			t.Fatalf("mixed targets: %q %q %v", got, build, err)
		}
	}
	// A foreign debugger serving an unverified directory is not evidence about Kuwo.
	bad.WebSocketDebuggerURL = "ws://example.com:9224/devtools/page/12"
	if _, _, err := selectTarget([]target{bad}, testEndpoint); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("foreign debugger classified as version: %v", err)
	}
}

func TestTargetSelectionPrefersIndexPage(t *testing.T) {
	other := netsongPage("9.6.0.0_W1")
	other.URL = "file:///C:/Kuwo/9.6.0.0_W1/bin/html/webdata/netsong/song.html"
	other.WebSocketDebuggerURL = "ws://127.0.0.1:9224/devtools/page/1"
	index := netsongPage("9.6.0.0_W1")
	for _, list := range [][]target{{other, index}, {index, other}} {
		if got, _, err := selectTarget(list, testEndpoint); err != nil || got != index.WebSocketDebuggerURL {
			t.Fatalf("index not preferred: %q %v", got, err)
		}
	}
}

// 变异自证（隔离副本，实测）：bridgeVersionPattern 去掉 ^ 与 $、数字部分放宽为 `[0-9.]+`、
// 后缀长度上限改为 `+`，均红。
func TestBridgeBuildAcceptsOnlyVerifiedNumericVersion(t *testing.T) {
	for version, want := range map[string]string{"MUSIC_9.6.0.0_W1": "W1", "MUSIC_9.6.0.0_UG1": "UG1", "MUSIC_9.6.0.0_X9": "X9", "MUSIC_9.6.0.0_abcdefghijklmnop": "abcdefghijklmnop"} {
		if got, ok := bridgeBuild(version); !ok || got != want {
			t.Fatalf("%q: %q %v", version, got, ok)
		}
	}
	for _, version := range []string{
		"", "MUSIC_9.6.0.1_W1", "MUSIC_9.7.0.0_W1", "MUSIC_10.6.0.0_W1", "MUSIC_19.6.0.0_W1", "MUSIC_9.6.0.0",
		"MUSIC_9.6.0.0_", "MUSIC_9.6.0.0_abcdefghijklmnopq", "MUSIC_9.6.0.0_W１", "MUSIC_9.6.0.0_Wé",
		"MUSIC_9.6.0.0_W/1", "MUSIC_9.6.0.0_..", "MUSIC_9.6.0.0_W1/..", "music_9.6.0.0_W1", "Music_9.6.0.0_W1",
		"MUSIC_9a6a0a0_W1", "MUSIC_9.6.0.0__W1", "MUSIC_9.6.0.0_W1_X", "MUSIC_9.6.0.0_W-1", "xMUSIC_9.6.0.0_W1",
		" MUSIC_9.6.0.0_W1", "MUSIC_9.6.0.0_W1 ", "MUSIC_9.6.0.0_W1\n", "MUSIC_9.6.0.0_W1\x00",
	} {
		if got, ok := bridgeBuild(version); ok {
			t.Fatalf("%q accepted as %q", version, got)
		}
	}
}
