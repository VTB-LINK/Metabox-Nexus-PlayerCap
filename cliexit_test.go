package main

// 本文件只测一件事：命令行里有多余的词时，config.Load 报出那个词、输出用法并以 2 退出，与写错参数名时
// 一致，不再静默忽略它后面的参数（issue #92）。
//
// 退出发生在 Load 里（os.Exit），所以各例在子进程里跑：子进程是本测试二进制重入自身，TestMain 见到
// loadHelperEnv 即转入 loadHelperMain，以收到的命令行实参跑一遍真实的 config.Load，不跑任何用例，也不
// 运行服务本体。退出码与两个输出流都取自子进程。报错的文案见 config/extraarg_test.go。

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"Metabox-Nexus-PlayerCap/config"
)

// loadHelperEnv 非空时，测试二进制作为 helper 子进程运行，见 loadHelperMain。
const loadHelperEnv = "PLAYERCAP_LOAD_TEST_HELPER"

// loadHelperMain 是 helper 子进程的入口，不返回。Load 返回后把几项配置写到标准输出、以 0 退出；Load 自己
// 退出时走不到这里，标准输出为空。
func loadHelperMain() {
	// 测试二进制的 flag.CommandLine 上登记着 testing 包的 -test.* 参数。换成与出货程序启动时相同的一个：
	// flag 包初始化时 CommandLine 即 NewFlagSet(os.Args[0], ExitOnError)，其 Usage 转调包级的 flag.Usage。
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	flag.CommandLine.Usage = func() { flag.Usage() }
	cfg := config.Load(Version)
	fmt.Printf("loaded poll=%d offset=%d audio-normalize=%t\n", cfg.Poll, cfg.Offset, cfg.AudioNormalize)
	os.Exit(0)
}

// runLoad 在子进程里以 args 为命令行实参跑 config.Load，返回退出码与两个输出流。子进程的当前目录是一个
// 临时目录，里面只有一份 config.yml：版本键是当前的盖章值（Load 不迁移、不改写它），其余全取默认值。
func runLoad(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(exe), "config.yml")); err == nil {
		t.Fatalf("测试二进制所在的 %s 里有 config.yml，Load 会优先读它；把二进制放到没有 config.yml 的目录再跑", filepath.Dir(exe))
	}
	dir := t.TempDir()
	stamped := `config-version: "` + config.StampVersion(Version) + "\"\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yml"), []byte(stamped), 0644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(exe, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), loadHelperEnv+"=1")
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	var exitErr *exec.ExitError
	if err := cmd.Run(); err != nil && !errors.As(err, &exitErr) {
		t.Fatalf("运行 helper: %v", err)
	}
	return cmd.ProcessState.ExitCode(), out.String(), errOut.String()
}

// usageOf 返回 stderr 里自「Usage of」起的部分；没有用法时返回空串。
func usageOf(stderr string) string {
	i := strings.Index(stderr, "Usage of ")
	if i < 0 {
		return ""
	}
	return stderr[i:]
}

// TestLoadExitsOnExtraArgs 命令行里多余的词：报出其中第一个，随后输出用法，以 2 退出，标准输出为空（Load
// 没有返回，它后面的参数没有被悄悄采用或丢弃）。用法与退出码同写错参数名时 flag 包的处理逐字节相同。
//
// 变异自证（隔离的 worktree，实测）：
//   - Load 去掉对剩余实参的检查 → 13 例都以 0 退出并写出 loaded，红。同时不规整实参（即改动前的行为）→
//     另有「对照：合法的参数」红。
//   - 检查之后不退出、退出码改成 1 或 0 → 13 例红。
//   - 报错之后不输出用法、先输出用法再报错、报错写到标准输出、不报错只输出用法 → 13 例红。
//   - 点名最后一个多余的词 → 多余的词不止一个的 8 例红；报错里的词不按 %q 写 → 13 例红。
//   - 只是不规整实参（保留检查）→「开关的取值之后又多一个字面量」「多余的词在最后」与「对照：合法的
//     参数」红：点名的成了开关后面的字面量。
func TestLoadExitsOnExtraArgs(t *testing.T) {
	// 对照：写错参数名时 flag 包自己的处理。
	refCode, refOut, refErr := runLoad(t, "-nosuch", "1")
	refUsage := usageOf(refErr)
	if refCode != 2 || refOut != "" || !strings.Contains(refErr, "flag provided but not defined: -nosuch") || refUsage == "" {
		t.Fatalf("对照（写错参数名）：退出码 %d，stdout %q，stderr:\n%s", refCode, refOut, refErr)
	}

	cases := []struct {
		name string
		args []string
		word string // 报错里按 %q 写出的那个词
	}{
		{"漏了横线", []string{"offset", "-200"}, `"offset"`},
		{"漏了横线，前面有合法的参数", []string{"-poll", "50", "offset", "-200"}, `"offset"`},
		{"开关后面不是布尔字面量", []string{"-audio-normalize", "yes"}, `"yes"`},
		{"开关后面不是布尔字面量，其后还有参数", []string{"-audio-normalize", "on", "-poll", "50"}, `"on"`},
		{"每播放器的开关后面不是布尔字面量", []string{"-qqmusic-audio-normalize", "no", "-poll", "50"}, `"no"`},
		{"多余的词夹在参数之间", []string{"-poll", "50", "foo", "-offset", "1"}, `"foo"`},
		{"开关的取值之后又多一个字面量", []string{"-audio-normalize", "true", "false", "-poll", "50"}, `"false"`},
		{"等号写法之后的字面量", []string{"-audio-normalize=false", "true"}, `"true"`},
		{"多余的词在最后", []string{"-poll", "50", "-audio-normalize", "false", "bar"}, `"bar"`},
		{"-- 之后的内容", []string{"-poll", "50", "--", "-offset", "1"}, `"-offset"`},
		{"单个横线", []string{"-poll", "50", "-"}, `"-"`},
		{"空串", []string{"-poll", "50", ""}, `""`},
		{"带空格的词", []string{"a b", "-poll", "50"}, `"a b"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel() // 各例各起一个子进程、各用各的临时目录，不改本进程的当前目录
			code, stdout, stderr := runLoad(t, c.args...)
			if code != 2 {
				t.Errorf("退出码 %d，期望 2", code)
			}
			if stdout != "" {
				t.Errorf("Load 不该返回，标准输出却有 %q", stdout)
			}
			msg := "命令行里多余的词 " + c.word + "："
			at := strings.Index(stderr, msg)
			if at < 0 || strings.Count(stderr, "命令行里多余的词") != 1 {
				t.Errorf("标准错误里应恰有一处 %s\n%s", msg, stderr)
			}
			if usage := usageOf(stderr); usage != refUsage {
				t.Errorf("用法应与写错参数名时逐字节相同，实际:\n%s", usage)
			} else if at > strings.Index(stderr, usage) {
				t.Errorf("报错应在用法之前:\n%s", stderr)
			}
		})
	}

	// 对照：没有多余的词时 Load 照常返回，不输出用法。
	for _, c := range []struct {
		name string
		args []string
		want string
	}{
		{"对照：合法的参数", []string{"-audio-normalize", "false", "-poll", "50"}, "loaded poll=50 offset=-200 audio-normalize=false\n"},
		{"对照：没有参数", nil, "loaded poll=30 offset=-200 audio-normalize=true\n"},
		{"对照：末尾的 -- 之后没有内容", []string{"-poll", "50", "--"}, "loaded poll=50 offset=-200 audio-normalize=true\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			code, stdout, stderr := runLoad(t, c.args...)
			if code != 0 || stdout != c.want || usageOf(stderr) != "" || strings.Contains(stderr, "多余") {
				t.Errorf("退出码 %d，stdout %q（期望 0 与 %q），stderr:\n%s", code, stdout, c.want, stderr)
			}
		})
	}
}
