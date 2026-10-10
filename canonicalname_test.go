package main

// 本文件只测一件事：ensureCanonicalName 按自身的文件名走哪条分支（issue #71）。
//
// 做法：把本测试二进制复制到临时目录、取不同的文件名，以 ensure 角色运行（见 relaunchwait_test.go
// 的 relaunchHelperMain）。角色把环境变量改成 leaf 后调 ensureCanonicalName，它启动的复制品因此是
// 一个 leaf：把参数写到标准输出，然后读标准输入而阻塞，用例写入一行它才结束。不运行服务本体。
// 三个标准流都是管道或文件、直接交给子进程，原进程一退出 cmd.Wait 就返回，与 leaf 是否还在无关。
//
// 等价变异（实测仍绿，不算缺口）：去掉「文件名已是标准名即返回」的判断，或把它改成区分大小写。
// 那时复制的目标就是正在运行的自身，os.Create 失败，函数照样返回。
//
// 没有覆盖的：取不到自身路径、打不开自身这两条提前返回。

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/i18n"
)

// ensureRun 是一次「把测试二进制复制为某个文件名、以 ensure 角色运行」的现场。
type ensureRun struct {
	cmd        *exec.Cmd
	stdin      *os.File      // 写端；leaf 读到一行才结束
	stdout     *bufio.Reader // 原进程与 leaf 共用的标准输出
	stderrPath string
	exited     chan struct{} // 原进程退出后关闭
}

// startEnsure 把本测试二进制复制为 dir 下的 name，以 role 运行。用例结束时放开 leaf，并等原进程与
// leaf 都退出（标准输出读到 EOF），临时目录才清得掉、不留残留进程。
func startEnsure(t *testing.T, dir, name, role string, args ...string) *ensureRun {
	t.Helper()
	exe := filepath.Join(dir, name)
	copyTestBinary(t, exe)

	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stderrPath := filepath.Join(t.TempDir(), "stderr.txt")
	errFile, err := os.Create(stderrPath)
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(exe, args...)
	cmd.Env = append(os.Environ(), relaunchHelperEnv+"="+role)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, errFile
	err = cmd.Start()
	inR.Close()
	outW.Close()
	errFile.Close()
	if err != nil {
		t.Fatalf("启动 %s: %v", name, err)
	}

	r := &ensureRun{cmd: cmd, stdin: inW, stdout: bufio.NewReader(outR), stderrPath: stderrPath, exited: make(chan struct{})}
	go func() {
		cmd.Wait()
		close(r.exited)
	}()
	t.Cleanup(func() {
		inW.Close()
		select {
		case <-r.exited:
		case <-time.After(helperSelfTimeout):
			cmd.Process.Kill()
			<-r.exited
		}
		io.Copy(io.Discard, r.stdout)
		outR.Close()
	})
	return r
}

// waitExit 等原进程退出并返回它的退出码；d 之内没有退出则返回 false。
func (r *ensureRun) waitExit(d time.Duration) (int, bool) {
	select {
	case <-r.exited:
		return r.cmd.ProcessState.ExitCode(), true
	case <-time.After(d):
		return 0, false
	}
}

func (r *ensureRun) stderr() string {
	b, _ := os.ReadFile(r.stderrPath)
	return string(b)
}

// readLines 从标准输出读 n 行。
func (r *ensureRun) readLines(t *testing.T, n int) []string {
	t.Helper()
	var lines []string
	for len(lines) < n {
		line, err := r.stdout.ReadString('\n')
		if err != nil {
			t.Fatalf("标准输出读到 %q 就结束了（%v）；标准错误：%s", append(lines, line), err, r.stderr())
		}
		lines = append(lines, strings.TrimRight(line, "\n"))
	}
	return lines
}

// rest 把标准输出读到 EOF：原进程与 leaf 都已退出。
func (r *ensureRun) rest() string {
	b, _ := io.ReadAll(r.stdout)
	return string(b)
}

// assertCanonicalCopy 断言 dir 下有标准文件名的普通文件，内容与本测试二进制相同。
func assertCanonicalCopy(t *testing.T, dir string) {
	t.Helper()
	fi, err := os.Lstat(filepath.Join(dir, canonicalExeName))
	if err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("%s 下没有 %s 的复制品（%v）", dir, canonicalExeName, err)
	}
	want, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, canonicalExeName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("复制品的内容与原文件不同（%d 字节，原文件 %d 字节）", len(got), len(want))
	}
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// TestEnsureCanonicalNameWaitsUnderOtherName 断言以非标准文件名启动时：同目录出现标准文件名的
// 复制品，参数原样传到；复制品还在运行时原进程没有退出；复制品结束后原进程以它的退出码退出。
//
// 变异（均实测为红）：这条分支改回启动后即以 0 退出（issue #71 的缺陷）；不复制内容就去启动。
func TestEnsureCanonicalNameWaitsUnderOtherName(t *testing.T) {
	const name = "PlayerCap-dev-0f3a9c1.exe"
	dir := t.TempDir()
	extra := []string{"-addr", "127.0.0.1:1", "a b", ""}
	r := startEnsure(t, dir, name, "ensure", append([]string{"echo", "3"}, extra...)...)

	// leaf 把参数写回来：复制品已启动，正读标准输入而阻塞。
	got := r.readLines(t, len(extra))
	for i, a := range extra {
		if want := fmt.Sprintf("%q", a); got[i] != want {
			t.Errorf("复制品收到的第 %d 个参数是 %s，期望 %s", i+1, got[i], want)
		}
	}
	select {
	case <-r.exited:
		t.Fatalf("复制品还在运行，原进程已退出（退出码 %d）", r.cmd.ProcessState.ExitCode())
	default:
	}
	assertCanonicalCopy(t, dir)
	if names := dirNames(t, dir); len(names) != 2 {
		t.Errorf("目录里应只有原文件与复制品，实有 %v", names)
	}

	fmt.Fprintln(r.stdin, "go")
	code, ok := r.waitExit(helperSelfTimeout)
	if !ok {
		t.Fatal("复制品结束后原进程没有退出")
	}
	if code != 3 {
		t.Errorf("原进程退出码 %d，期望沿用复制品的 3；标准错误：%s", code, r.stderr())
	}
	if rest := r.rest(); rest != "stdin=\"go\"\n" {
		t.Errorf("复制品的后续输出 %q", rest)
	}
}

// TestEnsureCanonicalNameDoesNotWaitUnderLegacyName 断言以 legacyExeName（不分大小写）启动时不
// 等待：复制品读标准输入而阻塞，用例此时什么都没写，原进程须自己先以 0 退出；之后放开复制品，
// 它照常跑完。
//
// 变异（均实测为红）：去掉这条例外、一律等待；这条分支改成等复制品结束再以 0 退出；文件名比较
// 区分大小写（大写一例红）；这条分支不传命令行参数；不复制内容就去启动。前三条表现为原进程
// 10 秒内不退出。
func TestEnsureCanonicalNameDoesNotWaitUnderLegacyName(t *testing.T) {
	for _, name := range []string{legacyExeName, strings.ToUpper(legacyExeName)} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			r := startEnsure(t, dir, name, "ensure", "echo", "3", "a b")

			code, ok := r.waitExit(10 * time.Second)
			if !ok {
				t.Fatal("原进程在等复制品结束，旧文件名应当不等待")
			}
			if code != 0 {
				t.Errorf("原进程退出码 %d，期望 0；标准错误：%s", code, r.stderr())
			}
			assertCanonicalCopy(t, dir)

			if got := r.readLines(t, 1); got[0] != `"a b"` {
				t.Errorf("复制品收到的参数是 %s，期望 \"a b\"", got[0])
			}
			fmt.Fprintln(r.stdin, "go")
			if rest := r.rest(); rest != "stdin=\"go\"\n" {
				t.Errorf("复制品没有照常跑完，后续输出 %q", rest)
			}
		})
	}
}

// TestEnsureCanonicalNameReturnsUnderCanonicalName 断言文件名已是标准名时函数直接返回：不复制
// （目录里只有它自己）、不启动（标准输出里没有 leaf 的输出）、不记日志。
//
// 守不住「提前返回」那一句本身，见文件头的等价变异。
func TestEnsureCanonicalNameReturnsUnderCanonicalName(t *testing.T) {
	dir := t.TempDir()
	r := startEnsure(t, dir, canonicalExeName, "ensure", "echo", "3", "a b")

	code, ok := r.waitExit(10 * time.Second)
	if !ok || code != helperExitEnsureReturned {
		t.Fatalf("ensureCanonicalName 没有返回：已退出 %v，退出码 %d；标准错误：%s", ok, code, r.stderr())
	}
	if out := r.rest(); out != "ensure-returned\n" {
		t.Errorf("标准输出 %q，期望只有 ensure-returned", out)
	}
	if names := dirNames(t, dir); len(names) != 1 {
		t.Errorf("目录里应只有它自己，实有 %v", names)
	}
	if s := r.stderr(); s != "" {
		t.Errorf("不应有日志，实有 %q", s)
	}
}

// TestEnsureCanonicalNameFallsBack 断言复制品起不来时函数返回、进程继续：复制失败（标准名的位置
// 是个目录）时不记日志、不去启动；复制成功而启动失败（命令行超长）时记一条告警，两条分支都如此。
//
// 变异（均实测为红）：启动失败也以 0 退出（改动前的写法）；旧文件名分支不检查启动错误、以 0 退出；
// 复制失败后仍去启动。
func TestEnsureCanonicalNameFallsBack(t *testing.T) {
	// 告警文案里 %v 之前的部分，随界面语言取。
	warn := fmt.Sprintf(strings.SplitN(i18n.T("Failed to start %s: %v; continuing under the current filename"), "%v", 2)[0], canonicalExeName)

	expectReturned := func(t *testing.T, r *ensureRun) {
		t.Helper()
		code, ok := r.waitExit(10 * time.Second)
		if !ok || code != helperExitEnsureReturned {
			t.Fatalf("ensureCanonicalName 没有返回：已退出 %v，退出码 %d；标准错误：%s", ok, code, r.stderr())
		}
		if out := r.rest(); out != "ensure-returned\n" {
			t.Errorf("标准输出 %q，期望只有 ensure-returned", out)
		}
	}

	t.Run("复制失败", func(t *testing.T) {
		dir := t.TempDir()
		blocker := filepath.Join(dir, canonicalExeName)
		if err := os.Mkdir(blocker, 0o755); err != nil {
			t.Fatal(err)
		}
		r := startEnsure(t, dir, "PlayerCap-dev.exe", "ensure", "echo", "3", "a b")

		expectReturned(t, r)
		if fi, err := os.Lstat(blocker); err != nil || !fi.IsDir() {
			t.Errorf("标准名位置上的目录被动过（%v）", err)
		}
		if s := r.stderr(); strings.Contains(s, canonicalExeName) {
			t.Errorf("复制失败应当不记日志，实有 %q", s)
		}
	})

	for _, name := range []string{"PlayerCap-dev.exe", legacyExeName} {
		t.Run("启动失败/"+name, func(t *testing.T) {
			dir := t.TempDir()
			r := startEnsure(t, dir, name, "ensure-longarg", "echo", "3", "a b")

			expectReturned(t, r)
			assertCanonicalCopy(t, dir)
			if s := r.stderr(); !strings.Contains(s, warn) {
				t.Errorf("没有记启动失败的告警（应含 %q），标准错误：%s", warn, s)
			}
		})
	}
}
