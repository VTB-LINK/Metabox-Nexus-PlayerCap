package watchdog

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows/registry"
)

const spotifyExe = `C:\Users\a b\AppData\Roaming\Spotify\Spotify.exe`

func TestHasToken(t *testing.T) {
	cases := []struct {
		cmd, flag string
		want      bool
	}{
		{`"` + spotifyExe + `" --autostart --remote-debugging-port=9223`, DebugFlag, true},
		{`"` + spotifyExe + `" --remote-debugging-port=9222`, DebugFlag, false},
		{`"` + spotifyExe + `" --remote-debugging-port=92230`, DebugFlag, false},
		{`"` + spotifyExe + `" x--remote-debugging-port=9223`, DebugFlag, false},
		{`"` + spotifyExe + `"	--disable-backgrounding-occluded-windows`, keepaliveMarker, true},
		{`"` + spotifyExe + `" --disable-backgrounding-occluded-windows-x`, keepaliveMarker, false},
		{``, DebugFlag, false},
	}
	for _, c := range cases {
		if got := hasToken(c.cmd, c.flag); got != c.want {
			t.Errorf("hasToken(%q, %q) = %v, want %v", c.cmd, c.flag, got, c.want)
		}
	}
}

func TestLaunchFlagsCarryDebugAndKeepalive(t *testing.T) {
	joined := " " + strings.Join(LaunchFlags, " ") + " "
	for _, f := range []string{DebugFlag, keepaliveMarker, "--disable-renderer-backgrounding", "--disable-background-timer-throttling", "--disable-features=CalculateNativeWinOcclusion"} {
		if !hasToken(joined, f) {
			t.Errorf("LaunchFlags lacks %s", f)
		}
	}
	if len(LaunchFlags) != 5 {
		t.Errorf("LaunchFlags has %d items, want 5", len(LaunchFlags))
	}
}

func TestDecideRestart(t *testing.T) {
	main := func(debug, keep bool) scan {
		return scan{PIDs: []int{1, 2}, MainPID: 1, ExePath: spotifyExe, HasDebugFlag: debug, HasKeepalive: keep}
	}
	cases := []struct {
		name      string
		sc        scan
		portUp    bool
		restarted bool
		want      verdict
	}{
		{"both flags, port up", main(true, true), true, false, verdictReady},
		{"both flags, port down", main(true, true), false, false, verdictPortNotReady},
		{"both flags, port up, restarted", main(true, true), true, true, verdictReady},
		{"debug only, port up (user's case)", main(true, false), true, false, verdictRestart},
		{"debug only, port down", main(true, false), false, false, verdictRestart},
		{"keepalive only, port down", main(false, true), false, false, verdictRestart},
		{"keepalive only, port up", main(false, true), true, false, verdictRestart},
		{"no flags (incl. 9222), port down", main(false, false), false, false, verdictRestart},
		{"no flags, port up", main(false, false), true, false, verdictRestart},
		{"cmdline unreadable, port up", scan{PIDs: []int{1}}, true, false, verdictReady},
		{"cmdline unreadable, port down", scan{PIDs: []int{1}}, false, false, verdictNoMainPID},
		{"cmdline unreadable, port down, restarted", scan{PIDs: []int{1}}, false, true, verdictNoMainPID},
		{"restarted, debug only, port up", main(true, false), true, true, verdictKeep},
		{"restarted, debug only, port down", main(true, false), false, true, verdictKeep},
		{"restarted, keepalive only, port up", main(false, true), true, true, verdictKeep},
		{"restarted, no flags, port down (user relaunched)", main(false, false), false, true, verdictRestart},
		{"restarted, keepalive only, port down", main(false, true), false, true, verdictRestart},
	}
	for _, c := range cases {
		if got := decideRestart(c.sc, c.portUp, c.restarted); got != c.want {
			t.Errorf("%s: decideRestart = %v, want %v", c.name, got, c.want)
		}
	}
}

// fakeEnv 替换 EnsureDebugMode 依赖的进程与端口操作，记录调用。
type fakeEnv struct {
	scans       []scan // 依次返回；用完后重复最后一个
	scanErr     error
	portUp      bool
	supported   bool
	supportsErr error
	kills       int
	starts      int
	startArgs   []string
	exitAfter   bool // kill 之后扫描返回「无进程」
	startPortUp bool // start 之后端口通
	afterStart  scan // start 之后的扫描结果
}

func (f *fakeEnv) install(t *testing.T) {
	t.Helper()
	oldScan, oldPort, oldSup, oldKill, oldStart := scanFn, portUpFn, supportsFlagFn, killFn, startFn
	restartedThisRun.Store(false)
	keptMissingWarned.Store(false)
	t.Cleanup(func() {
		scanFn, portUpFn, supportsFlagFn, killFn, startFn = oldScan, oldPort, oldSup, oldKill, oldStart
		restartedThisRun.Store(false)
		keptMissingWarned.Store(false)
	})
	killed := false
	started := false
	scanFn = func() (scan, error) {
		if f.scanErr != nil {
			return scan{}, f.scanErr
		}
		if started {
			return f.afterStart, nil
		}
		if killed && f.exitAfter {
			return scan{}, nil
		}
		sc := f.scans[0]
		if len(f.scans) > 1 {
			f.scans = f.scans[1:]
		}
		return sc, nil
	}
	portUpFn = func() bool {
		if started {
			return f.startPortUp
		}
		return f.portUp
	}
	supportsFlagFn = func(string) (bool, error) { return f.supported, f.supportsErr }
	killFn = func() error { f.kills++; killed = true; return nil }
	startFn = func(exe string, args []string) error {
		f.starts++
		started = true
		f.startArgs = args
		return nil
	}
}

func spotifyScan(debug, keep bool) scan {
	return scan{PIDs: []int{10, 11}, MainPID: 10, ExePath: spotifyExe, HasDebugFlag: debug, HasKeepalive: keep}
}

func TestEnsureDebugModeDebugOnlyPortUpRestartsWithLaunchFlags(t *testing.T) {
	f := &fakeEnv{scans: []scan{spotifyScan(true, false)}, portUp: true, supported: true, exitAfter: true, startPortUp: true, afterStart: spotifyScan(true, true)}
	f.install(t)
	if err := EnsureDebugMode(); err != nil {
		t.Fatalf("EnsureDebugMode = %v, want nil", err)
	}
	if f.kills != 1 || f.starts != 1 {
		t.Fatalf("kills=%d starts=%d, want 1/1", f.kills, f.starts)
	}
	if strings.Join(f.startArgs, " ") != strings.Join(LaunchFlags, " ") {
		t.Errorf("started with %v, want LaunchFlags", f.startArgs)
	}
	if !restartedThisRun.Load() {
		t.Error("restartedThisRun not set after restart")
	}
	// 重启后参数齐全：后续调用不再杀。
	for i := 0; i < 3; i++ {
		if err := EnsureDebugMode(); err != nil {
			t.Fatalf("call %d after restart = %v", i, err)
		}
	}
	if f.kills != 1 {
		t.Errorf("kills=%d after restart, want 1", f.kills)
	}
}

func TestEnsureDebugModeNoKill(t *testing.T) {
	cases := []struct {
		name    string
		f       fakeEnv
		wantErr error // nil 表示要求返回 nil
		anyErr  bool  // 只要求非 nil
	}{
		{name: "both flags, port up", f: fakeEnv{scans: []scan{spotifyScan(true, true)}, portUp: true, supported: true}},
		{name: "both flags, port down", f: fakeEnv{scans: []scan{spotifyScan(true, true)}, supported: true}, wantErr: ErrPortNotReady},
		{name: "not running", f: fakeEnv{scans: []scan{{}}, supported: true}, wantErr: ErrNotRunning},
		{name: "scan fails", f: fakeEnv{scanErr: errors.New("boom"), supported: true}, anyErr: true},
		{name: "cmdline unreadable, port up", f: fakeEnv{scans: []scan{{PIDs: []int{1}, ExePath: spotifyExe}}, portUp: true, supported: true}},
		{name: "cmdline unreadable, port down", f: fakeEnv{scans: []scan{{PIDs: []int{1}, ExePath: spotifyExe}}, supported: true}, anyErr: true},
		{name: "debug only, version unsupported", f: fakeEnv{scans: []scan{spotifyScan(true, false)}, portUp: true, supported: false}, wantErr: ErrVersionUnsupported},
		{name: "no flags, version check fails", f: fakeEnv{scans: []scan{spotifyScan(false, false)}, supported: true, supportsErr: errors.New("cannot read")}, wantErr: ErrVersionUnsupported},
		{name: "debug 9222 only, version unsupported", f: fakeEnv{scans: []scan{spotifyScan(false, false)}, supported: false}, wantErr: ErrVersionUnsupported},
		{name: "no exe path", f: fakeEnv{scans: []scan{{PIDs: []int{1}, MainPID: 1, HasDebugFlag: true}}, portUp: true, supported: true}, anyErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := c.f
			f.install(t)
			err := EnsureDebugMode()
			switch {
			case c.anyErr:
				if err == nil {
					t.Error("want error, got nil")
				}
			case c.wantErr == nil:
				if err != nil {
					t.Errorf("want nil, got %v", err)
				}
			default:
				if !errors.Is(err, c.wantErr) {
					t.Errorf("want %v, got %v", c.wantErr, err)
				}
			}
			if f.kills != 0 || f.starts != 0 {
				t.Errorf("kills=%d starts=%d, want 0/0", f.kills, f.starts)
			}
			if restartedThisRun.Load() {
				t.Error("restartedThisRun set without a restart")
			}
		})
	}
}

func TestEnsureDebugModeRestartReasons(t *testing.T) {
	for _, sc := range []scan{spotifyScan(true, false), spotifyScan(false, true), spotifyScan(false, false)} {
		t.Run(fmt.Sprintf("debug=%v keep=%v", sc.HasDebugFlag, sc.HasKeepalive), func(t *testing.T) {
			f := &fakeEnv{scans: []scan{sc}, supported: true, exitAfter: true, startPortUp: true, afterStart: spotifyScan(true, true)}
			f.install(t)
			if err := EnsureDebugMode(); err != nil {
				t.Fatalf("EnsureDebugMode = %v", err)
			}
			if f.kills != 1 || f.starts != 1 {
				t.Errorf("kills=%d starts=%d, want 1/1", f.kills, f.starts)
			}
		})
	}
}

// 重启后新实例主进程仍缺保活标记（命令行被改写）：第二次起不再杀，只按端口返回。
func TestEnsureDebugModeDoesNotKillTwiceWhenMarkerStillMissing(t *testing.T) {
	f := &fakeEnv{scans: []scan{spotifyScan(true, false)}, portUp: true, supported: true, exitAfter: true, startPortUp: true, afterStart: spotifyScan(true, false)}
	f.install(t)
	if err := EnsureDebugMode(); err != nil {
		t.Fatalf("first call = %v", err)
	}
	for i := 0; i < 5; i++ {
		if err := EnsureDebugMode(); err != nil {
			t.Fatalf("call %d = %v, want nil (port up)", i, err)
		}
	}
	if f.kills != 1 || f.starts != 1 {
		t.Errorf("kills=%d starts=%d, want 1/1", f.kills, f.starts)
	}
	if !keptMissingWarned.Load() {
		t.Error("keptMissingWarned not set")
	}

	// 端口随后掉了、主进程仍带调试口：返回 ErrPortNotReady，仍不杀。
	f.startPortUp = false
	if err := EnsureDebugMode(); !errors.Is(err, ErrPortNotReady) {
		t.Errorf("port down after restart = %v, want ErrPortNotReady", err)
	}
	if f.kills != 1 {
		t.Errorf("kills=%d, want 1", f.kills)
	}
}

// 重启过之后用户自己不带参数重开（无调试口、端口不通）：这个实例对我们没用，照常重启。
func TestEnsureDebugModeRestartsAgainAfterPlainRelaunch(t *testing.T) {
	f := &fakeEnv{scans: []scan{spotifyScan(false, false)}, supported: true, exitAfter: true, startPortUp: true, afterStart: spotifyScan(true, true)}
	f.install(t)
	restartedThisRun.Store(true) // 本次运行早先已重启过一次
	if err := EnsureDebugMode(); err != nil {
		t.Fatalf("EnsureDebugMode = %v", err)
	}
	if f.kills != 1 || f.starts != 1 {
		t.Errorf("kills=%d starts=%d, want 1/1", f.kills, f.starts)
	}
}

func TestPatchAutostartValue(t *testing.T) {
	q := `"` + spotifyExe + `"`
	all := " " + strings.Join(LaunchFlags, " ")
	cases := []struct {
		name, in, want string
		changed        bool
	}{
		{"user's value: debug flag only", q + " --autostart --minimized --remote-debugging-port=9223",
			q + " --autostart --minimized --remote-debugging-port=9223 --disable-backgrounding-occluded-windows --disable-renderer-backgrounding --disable-background-timer-throttling --disable-features=CalculateNativeWinOcclusion", true},
		{"no flags", q + " --autostart", q + " --autostart" + all, true},
		{"bare path", q, q + all, true},
		{"already complete", q + " --autostart" + all, q + " --autostart" + all, false},
		{"already complete, different order", q + " --disable-features=CalculateNativeWinOcclusion --disable-background-timer-throttling --disable-renderer-backgrounding --disable-backgrounding-occluded-windows --remote-debugging-port=9223",
			q + " --disable-features=CalculateNativeWinOcclusion --disable-background-timer-throttling --disable-renderer-backgrounding --disable-backgrounding-occluded-windows --remote-debugging-port=9223", false},
		{"other debug port is not ours", q + " --remote-debugging-port=9222", q + " --remote-debugging-port=9222" + all, true},
		{"substring of a longer port", q + " --remote-debugging-port=92230", q + " --remote-debugging-port=92230" + all, true},
		{"merge into existing disable-features", q + " --disable-features=Foo,Bar --autostart",
			q + " --disable-features=Foo,Bar,CalculateNativeWinOcclusion --autostart " + strings.Join(LaunchFlags[:4], " "), true},
		{"existing disable-features already has it", q + " --disable-features=Foo,CalculateNativeWinOcclusion",
			q + " --disable-features=Foo,CalculateNativeWinOcclusion " + strings.Join(LaunchFlags[:4], " "), true},
		{"feature name only as a prefix of another", q + " --disable-features=CalculateNativeWinOcclusionX",
			q + " --disable-features=CalculateNativeWinOcclusionX,CalculateNativeWinOcclusion " + strings.Join(LaunchFlags[:4], " "), true},
		{"empty disable-features list", q + " --disable-features=",
			q + " --disable-features=CalculateNativeWinOcclusion " + strings.Join(LaunchFlags[:4], " "), true},
		{"two disable-features: merge into the last", q + " --disable-features=CalculateNativeWinOcclusion --disable-features=Foo",
			q + " --disable-features=CalculateNativeWinOcclusion --disable-features=Foo,CalculateNativeWinOcclusion " + strings.Join(LaunchFlags[:4], " "), true},
		{"tabs and double spaces preserved", q + "\t--autostart  --remote-debugging-port=9223",
			q + "\t--autostart  --remote-debugging-port=9223 " + strings.Join(LaunchFlags[1:], " "), true},
		{"non-ASCII path preserved", `"C:\Users\主播 甲\Spotify.exe" --disable-features=Foo`,
			`"C:\Users\主播 甲\Spotify.exe" --disable-features=Foo,CalculateNativeWinOcclusion ` + strings.Join(LaunchFlags[:4], " "), true},
		// 路径里恰有与参数同形的片段：路径逐字不变，参数追加在参数区。
		{"flag-shaped text inside the quoted path", `"C:\Apps --disable-features=Foo bar\Spotify.exe" --autostart`,
			`"C:\Apps --disable-features=Foo bar\Spotify.exe" --autostart` + all, true},
		{"our flag inside the quoted path is not ours", `"C:\x --remote-debugging-port=9223 y\Spotify.exe"`,
			`"C:\x --remote-debugging-port=9223 y\Spotify.exe"` + all, true},
		{"unquoted path with no args", `C:\Spotify\Spotify.exe`, `C:\Spotify\Spotify.exe` + all, true},
		{"unterminated quote: malformed value is left alone", `"C:\Spotify\Spotify.exe --autostart`, `"C:\Spotify\Spotify.exe --autostart`, false},
		{"leading spaces", `  ` + q + ` --autostart`, `  ` + q + ` --autostart` + all, true},
		{"empty or blank value is left alone", `   `, `   `, false},
		{"empty value", ``, ``, false},
	}
	for _, c := range cases {
		got, changed := patchAutostartValue(c.in)
		if got != c.want || changed != c.changed {
			t.Errorf("%s:\n in   %q\n got  %q (%v)\n want %q (%v)", c.name, c.in, got, changed, c.want, c.changed)
		}
		// 幂等：再补一次不应有任何改动。
		if again, ch := patchAutostartValue(got); ch || again != got {
			t.Errorf("%s: second patch changed the value to %q", c.name, again)
		}
	}
}

func TestLastTokenWithPrefix(t *testing.T) {
	cases := []struct {
		s          string
		start, end int
	}{
		{"", -1, -1},
		{"--disable-features=A", 0, 20},
		{"x --disable-features=A y", 2, 22},
		{"--disable-features=A --disable-features=B", 21, 41},
		{"x--disable-features=A", -1, -1},
		{"  ", -1, -1},
	}
	for _, c := range cases {
		s, e := lastTokenWithPrefix(c.s, disableFeaturesPrefix)
		if s != c.start || e != c.end {
			t.Errorf("lastTokenWithPrefix(%q) = %d,%d, want %d,%d", c.s, s, e, c.start, c.end)
		}
	}
}

// tempRunKey 在 HKCU 下建一个本测试专用的临时键，测试结束删除。绝不碰真实的 Run 键。
func tempRunKey(t *testing.T) string {
	t.Helper()
	path := fmt.Sprintf(`Software\MetaboxPlayerCapTest\SpotifyAutostart-%d`, time.Now().UnixNano())
	k, _, err := registry.CreateKey(registry.CURRENT_USER, path, registry.ALL_ACCESS)
	if err != nil {
		t.Fatalf("create temp key: %v", err)
	}
	k.Close()
	t.Cleanup(func() {
		if err := registry.DeleteKey(registry.CURRENT_USER, path); err != nil {
			t.Errorf("delete temp key: %v", err)
		}
		_ = registry.DeleteKey(registry.CURRENT_USER, `Software\MetaboxPlayerCapTest`)
	})
	return path
}

func readValue(t *testing.T, path, name string) (string, bool) {
	t.Helper()
	k, err := registry.OpenKey(registry.CURRENT_USER, path, registry.QUERY_VALUE)
	if err != nil {
		t.Fatalf("open temp key: %v", err)
	}
	defer k.Close()
	v, _, err := k.GetStringValue(name)
	if err != nil {
		return "", false
	}
	return v, true
}

func TestPatchAutostartAtPatchesExistingValue(t *testing.T) {
	path := tempRunKey(t)
	orig := `"` + spotifyExe + `" --autostart --minimized --remote-debugging-port=9223`
	k, err := registry.OpenKey(registry.CURRENT_USER, path, registry.SET_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	if err := k.SetStringValue("Spotify", orig); err != nil {
		t.Fatal(err)
	}
	k.Close()

	patchAutostartAt(path, "Spotify")
	want, _ := patchAutostartValue(orig)
	if got, ok := readValue(t, path, "Spotify"); !ok || got != want {
		t.Fatalf("value = %q (exists=%v), want %q", got, ok, want)
	}
	// 再修补一次不变。
	patchAutostartAt(path, "Spotify")
	if got, _ := readValue(t, path, "Spotify"); got != want {
		t.Errorf("second patch changed value to %q", got)
	}
}

// 原值是 REG_EXPAND_SZ（路径里带 %APPDATA%）时，修补后仍须是 REG_EXPAND_SZ，且 %APPDATA% 原样保留。
func TestPatchAutostartAtKeepsExpandStringType(t *testing.T) {
	path := tempRunKey(t)
	orig := `"%APPDATA%\Spotify\Spotify.exe" --autostart --minimized`
	k, err := registry.OpenKey(registry.CURRENT_USER, path, registry.SET_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	if err := k.SetExpandStringValue("Spotify", orig); err != nil {
		t.Fatal(err)
	}
	k.Close()

	patchAutostartAt(path, "Spotify")

	rk, err := registry.OpenKey(registry.CURRENT_USER, path, registry.QUERY_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	defer rk.Close()
	got, typ, err := rk.GetStringValue("Spotify")
	if err != nil {
		t.Fatal(err)
	}
	if typ != registry.EXPAND_SZ {
		t.Errorf("value type = %d, want REG_EXPAND_SZ (%d)", typ, registry.EXPAND_SZ)
	}
	if want, _ := patchAutostartValue(orig); got != want {
		t.Errorf("value = %q, want %q", got, want)
	}
}

func TestPatchAutostartAtDoesNotCreateMissingValue(t *testing.T) {
	path := tempRunKey(t)
	patchAutostartAt(path, "Spotify")
	if v, ok := readValue(t, path, "Spotify"); ok {
		t.Errorf("value created: %q", v)
	}
}

func TestPatchAutostartAtMissingKeyIsNoop(t *testing.T) {
	path := fmt.Sprintf(`Software\MetaboxPlayerCapTest\Missing-%d`, time.Now().UnixNano())
	patchAutostartAt(path, "Spotify")
	if k, err := registry.OpenKey(registry.CURRENT_USER, path, registry.QUERY_VALUE); err == nil {
		k.Close()
		_ = registry.DeleteKey(registry.CURRENT_USER, path)
		t.Error("missing key was created")
	}
}
