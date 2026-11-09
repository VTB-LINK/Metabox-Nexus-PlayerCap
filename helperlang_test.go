package main

// 本文件只测一件事：两种 helper 子进程模式按系统显示语言设定界面语言，规则与主进程相同（issue #143）。
// 此前 main 在 SetLanguage 之前就转入 helper 分支，helper 进程的语言恒为零值（中文），它写进 stderr 或
// 结果文件、由主进程打进日志的文案在英文系统上夹中文。
//
// 子进程是本测试二进制重入自身：TestMain 见到 helperLangEnv 即转入 helperLangMain，先把语言设成与系统显示
// 语言相反的一种，再以取值所指的 helper 参数跑真实的 main()。main 按系统显示语言重设了，输出才是期望的语言；
// 没有重设，输出就是相反的那一种。因此两例在中文与英文系统上都能区分。
//
// 两例都停在参数或控制行的校验：音频 helper 读到一行未知命令、随即见 stdin EOF，不激活任何捕获；kugou helper
// 的主进程 PID 不合法，只把原因写进结果文件即退出，不打开任何进程、不碰 libcef.dll。
//
// 变异自证（实测）：main 里的 SetLanguage 挪到两个 helper 分支之后 → 两例红。

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"Metabox-Nexus-PlayerCap/i18n"
)

// helperLangEnv 非空时，测试二进制作为 helper 子进程运行，取值是交给 main 的 helper 参数，见 helperLangMain。
const helperLangEnv = "PLAYERCAP_HELPERLANG_TEST_HELPER"

// helperLangMain 是 helper 子进程的入口，不返回：语言先设成与系统显示语言相反的一种，再以 mode 加上收到的
// 命令行实参跑 main()。
func helperLangMain(mode string) {
	if i18n.DetectSystemLanguage() == i18n.Chinese {
		i18n.SetLanguage(i18n.English)
	} else {
		i18n.SetLanguage(i18n.Chinese)
	}
	os.Args = append([]string{os.Args[0], mode}, os.Args[1:]...)
	main()
	os.Exit(0)
}

// runHelperMode 在子进程里以 mode 加 args 跑 main()，stdin 为其标准输入，env 追加进环境，返回其标准错误。
func runHelperMode(t *testing.T, mode, stdin string, env []string, args ...string) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, args...)
	cmd.Env = append(append(os.Environ(), helperLangEnv+"="+mode), env...)
	cmd.Stdin = strings.NewReader(stdin)
	var errOut bytes.Buffer
	cmd.Stderr = &errOut
	var exitErr *exec.ExitError
	if err := cmd.Run(); err != nil && !errors.As(err, &exitErr) {
		t.Fatalf("运行 helper: %v", err)
	}
	return errOut.String()
}

// TestHelperModesFollowSystemLanguage 两种 helper 写出的文案是系统显示语言的那一种：音频 helper 写 stderr 的
// 诊断，kugou helper 写进结果文件的原因。
func TestHelperModesFollowSystemLanguage(t *testing.T) {
	t.Cleanup(func() { i18n.SetLanguage(i18n.Chinese) })
	i18n.SetLanguage(i18n.DetectSystemLanguage())

	t.Run("audio-capture-helper", func(t *testing.T) {
		stderr := runHelperMode(t, "--audio-capture-helper", "bogus\n", nil)
		want := fmt.Sprintf(i18n.T("[helper] ignoring invalid control line: %v"), fmt.Sprintf(i18n.T("unknown control command %q"), "bogus"))
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr 里没有 %q:\n%s", want, stderr)
		}
	})

	t.Run("kugou-patch-helper", func(t *testing.T) {
		// 结果文件写在 helper 的 os.TempDir()，文件名带令牌。令牌每次随机，按它在同一目录里找文件，用完删掉。
		// 不靠设 TMP/TEMP 把它引到本例的临时目录：以 SYSTEM 运行时（CI 的 runner）os.TempDir 经 GetTempPath2
		// 固定返回 C:\Windows\SystemTemp，不看这两个变量。子进程与本进程身份相同，取到的目录也相同。
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			t.Fatal(err)
		}
		token := hex.EncodeToString(b[:])
		// 列目录后按文件名筛，不用 filepath.Glob：临时目录的路径里若有方括号，会被当成模式。
		tokenFiles := func() ([]string, error) {
			entries, err := os.ReadDir(os.TempDir())
			var files []string
			for _, e := range entries {
				if strings.Contains(e.Name(), token) {
					files = append(files, filepath.Join(os.TempDir(), e.Name()))
				}
			}
			return files, err
		}
		t.Cleanup(func() {
			files, _ := tokenFiles()
			for _, f := range files {
				os.Remove(f)
			}
		})
		runHelperMode(t, "--kugou-patch-helper", "", nil, `C:\nonexistent\libcef.dll`, "x", token)
		files, err := tokenFiles()
		if err != nil || len(files) != 1 {
			t.Fatalf("临时目录里带本次令牌的应只有结果文件: %v %v", files, err)
		}
		got, err := os.ReadFile(files[0])
		if err != nil {
			t.Fatal(err)
		}
		if want := "ERROR: " + fmt.Sprintf(i18n.T("invalid main process PID: %q"), "x"); string(got) != want {
			t.Errorf("结果文件 = %q，期望 %q", got, want)
		}
	})
}
