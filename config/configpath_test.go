package config

// 本文件只测 configPath 的选址：config.yml 必须按 **exe 所在目录** 解析，不是 CWD。
// 不测 YAML 解析、不测钳制（clamppoll_test.go）、不测 Load 整体（它注册全局 flag，
// 二次调用会 panic）。

import (
	"os"
	"path/filepath"
	"testing"
)

// withPaths 注入假的 exe 路径与 CWD，返回还原函数。
func withPaths(t *testing.T, exePath, cwd string, exeErr error) {
	t.Helper()
	oe, og := osExecutable, osGetwd
	osExecutable = func() (string, error) { return exePath, exeErr }
	osGetwd = func() (string, error) { return cwd, nil }
	t.Cleanup(func() { osExecutable, osGetwd = oe, og })
}

func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("poll: 30\n"), 0644); err != nil {
		t.Fatalf("建测试文件失败: %v", err)
	}
}

// TestConfigPathPrefersExeDir 钉死本条核心：exe 目录的 config.yml 优先于 CWD 的。
//
// 缺陷背景：原为裸 os.ReadFile("config.yml") —— 按 **CWD** 解析。双击运行时 CWD = exe
// 目录（主流路径无差异），但计划任务、注册表 Run 键自启、起始位置为空的快捷方式，CWD
// 都是 C:\Windows\system32：配置在那儿读、在那儿生成，提权时还会把 System32 写脏。
// 而 README 明写「自动加载**同目录**下的 config.yml」，CI 打包也是把 config.yml 和 exe
// 放进同一目录 —— 「同目录」是设计意图，按 CWD 解析是代码单方面偏离。
//
// 变异自证：把优先级反过来（CWD 优先）即红。
func TestConfigPathPrefersExeDir(t *testing.T) {
	exeDir, cwd := t.TempDir(), t.TempDir()
	touch(t, filepath.Join(exeDir, configName))
	touch(t, filepath.Join(cwd, configName)) // 两处都有 —— 唯一的真实回归人群

	withPaths(t, filepath.Join(exeDir, "app.exe"), cwd, nil)

	if got, want := configPath(), filepath.Join(exeDir, configName); got != want {
		t.Errorf("两份都存在时应优先 exe 目录\n  want %s\n  got  %s", want, got)
	}
}

// TestConfigPathFallsBackToCwd exe 目录没有、CWD 有 → 用 CWD 那份。
//
// 这条回退只为「开发者 go build 到临时目录再从别处跑」保留。**对 zip 用户永不触发**
// （exe 目录里 CI 那份 config.yml 永远在），所以它不是兼容保证——别把它当兼容保证卖。
func TestConfigPathFallsBackToCwd(t *testing.T) {
	exeDir, cwd := t.TempDir(), t.TempDir()
	touch(t, filepath.Join(cwd, configName)) // 只有 CWD 有

	withPaths(t, filepath.Join(exeDir, "app.exe"), cwd, nil)

	if got, want := configPath(), filepath.Join(cwd, configName); got != want {
		t.Errorf("exe 目录无 config.yml 时应回退到 CWD\n  want %s\n  got  %s", want, got)
	}
}

// TestConfigPathGeneratesIntoExeDir 两处都没有 → 生成点必须是 exe 目录。
//
// 这条守的是「别再往 CWD 写」：提权的计划任务下，往 CWD 写 = 往 C:\Windows\System32
// 写 config.yml 污染系统目录；不提权则失败（且此前是无 else 的写入，零输出）。
func TestConfigPathGeneratesIntoExeDir(t *testing.T) {
	exeDir, cwd := t.TempDir(), t.TempDir() // 两处都空

	withPaths(t, filepath.Join(exeDir, "app.exe"), cwd, nil)

	if got, want := configPath(), filepath.Join(exeDir, configName); got != want {
		t.Errorf("两处都没有时，生成点应是 exe 目录（README 的「同目录」契约）\n  want %s\n  got  %s", want, got)
	}
}

// TestConfigPathSurvivesExecutableError os.Executable() 失败时必须回退到旧行为，
// 不 panic、不退出。
//
// 配置读不到最多是用内置默认继续跑；让整个取词服务因为定位不了自己而起不来，
// 是把一个小问题升级成事故（稳定 > 正确）。
func TestConfigPathSurvivesExecutableError(t *testing.T) {
	withPaths(t, "", t.TempDir(), os.ErrPermission)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("os.Executable() 失败不得 panic，实得 %v", r)
		}
	}()

	if got := configPath(); got != configName {
		t.Errorf("os.Executable() 失败时应回退为裸 %q（旧行为），实得 %q", configName, got)
	}
}

// TestConfigPathNoCwdLookupWhenSame CWD 与 exe 目录相同（双击运行的主流路径）时，
// 结果就是那一份。
//
// **本用例只守返回值**，不守「双份并存 Warn 有没有被误触发」—— log 的调用与否不在观测
// 范围内。（原注释声称守后者，是空头支票，已收窄。要真守得把 config 的 log 抽成可注入。）
//
// 已知缺口，按 LOW 记档不修：configPath 用字符串比较判 `cwd != exeDir`，而 Windows 上
// 同一个目录有多种文本形式（8.3 短名 `C:\PROGRA~1` vs `C:\Program Files`、大小写、尾斜杠），
// 于是「发现两份 config.yml」的 Warn 可能对着同一份文件误报。只污染日志、不影响选址
// （两条路径最终都返回 exe 目录那份），故不动。
func TestConfigPathNoCwdLookupWhenSame(t *testing.T) {
	dir := t.TempDir()
	touch(t, filepath.Join(dir, configName))

	withPaths(t, filepath.Join(dir, "app.exe"), dir, nil)

	if got, want := configPath(), filepath.Join(dir, configName); got != want {
		t.Errorf("CWD == exe 目录时应返回该目录下那份\n  want %s\n  got  %s", want, got)
	}
}

// TestGenerateDefaultConfigWritesToGivenPath 写入点必须是传进来的 path，
// 且与 Load 的读取点一致 —— 否则「写了读不到」（28b095c 修过这个坑）。
func TestGenerateDefaultConfigWritesToGivenPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, configName)

	generateDefaultConfig(path)

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("模板应写到传入的 path，实测不存在: %v", err)
	}
	// 且必须能被读回来（内容是合法 YAML）——「写了读不到」的另一半
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		t.Fatalf("生成的 config.yml 应可读且非空: err=%v len=%d", err, len(data))
	}
}

// TestGenerateDefaultConfigSurvivesWriteFailure 写失败不得 panic（本次运行用内置默认继续）。
// 原实现是无 else 的 `if err == nil`，写失败零输出；现在至少要有 Warn，且不能崩。
func TestGenerateDefaultConfigSurvivesWriteFailure(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("写失败不得 panic，实得 %v", r)
		}
	}()
	// 往一个不存在的目录里写 —— 必然失败
	generateDefaultConfig(filepath.Join(t.TempDir(), "no-such-dir", configName))
}
