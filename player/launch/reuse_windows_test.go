package launch

// 本文件只测两件事（Codex 审查提出，issue #127）：
//   - 中转 cmd.exe 的进程句柄被持有，它的 PID 不会被复用：播放器记着的父进程 ID 就是这个 PID，被 PlayerCap 之后
//     启动的子进程复用并存活到 OBS 退出时，Autostarter 会沿 PlayerCap → 该子进程 → 播放器递归结束播放器；
//   - dir 为空时按本进程实际的当前目录判断能否经 cmd 转交：cmd 不接受 UNC 的当前目录，会改用 Windows 目录。

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// childless 起一个没有子进程、会活一阵的进程（测试二进制扮演 hang 角色），返回它的 PID；用例结束时结束它。
// holdRelay 的旧句柄判定看「有没有存活的子进程」，用本测试进程自己的 PID 会因为它可能有别的子进程而不稳定。
func childless(t *testing.T) int {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	c := exec.Command(self)
	c.Env = append(os.Environ(), envRole+"=hang", envReport+"="+filepath.Join(t.TempDir(), "hang.json"))
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Process.Kill(); c.Wait() })
	return c.Process.Pid
}

// heldPIDs 返回某播放器路径下持有的全部句柄所指进程的 PID。
func heldPIDs(exe string) (pids []uint32) {
	heldMu.Lock()
	defer heldMu.Unlock()
	for _, h := range held[strings.ToLower(exe)] {
		if id, err := windows.GetProcessId(h); err == nil {
			pids = append(pids, id)
		}
	}
	return pids
}

func containsPID(pids []uint32, pid uint32) bool {
	for _, p := range pids {
		if p == pid {
			return true
		}
	}
	return false
}

// TestDetachedHoldsRelayHandle
//
// 防的缺陷与变异（实测变红）：Detached 不调用 holdRelay（中转 cmd 的进程对象随 cmd.Wait 释放，PID 可被复用）——
// 持有列表里没有播放器的父进程，按该 PID 也打不开进程。
func TestDetachedHoldsRelayHandle(t *testing.T) {
	tmp := t.TempDir()
	player := fakePlayer(t, filepath.Join(tmp, "player", "player.exe"))
	rep := filepath.Join(tmp, "player.json")
	t.Setenv(envRole, "player") // cmd.exe 与 start 把环境变量原样传给假播放器
	t.Setenv(envReport, rep)
	t.Setenv(envLinger, lingerCap.String())

	detached, err := Detached(player, []string{"--remote-debugging-port=9222"}, "")
	if err != nil || !detached {
		t.Fatalf("Detached = (%v, %v)，期望 (true, nil)", detached, err)
	}
	r := waitReport(t, rep, 30*time.Second)
	own(t, r.PID, player)
	relay := uint32(r.PPID)

	if !containsPID(heldPIDs(player), relay) {
		t.Errorf("播放器的父进程（中转 cmd，PID %d）的句柄不在持有列表里，它的 PID 可被复用", relay)
	}

	// 中转进程早已退出；只要有句柄在，进程对象就还在，按 PID 仍能打开。
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, relay)
	if err != nil {
		t.Fatalf("按 PID %d 打不开中转进程：它的进程对象已被释放，PID 可被复用：%v", relay, err)
	}
	defer windows.CloseHandle(h)
	if !exited(h, 0) {
		t.Errorf("中转进程 %d 还没有退出", relay)
	}
}

// TestHoldRelayKeepsHandleOfLivePlayer（Codex 第三轮提出）
//
// 防的缺陷与变异（实测变红）：同一播放器再次拉起时无条件换掉旧句柄——上一个实例若还活着（例如网易云的
// taskkill 失败而 EnsureDebugMode 照常往下拉起），它记着的中转 PID 又可被复用；永远不关闭旧句柄——重启不成功的
// 播放器会让句柄无限累积。播放器还活着时旧句柄必须保留，播放器退出之后再拉起时必须关闭。
func TestHoldRelayKeepsHandleOfLivePlayer(t *testing.T) {
	tmp := t.TempDir()
	player := fakePlayer(t, filepath.Join(tmp, "player", "player.exe"))
	rep := filepath.Join(tmp, "player.json")
	t.Setenv(envRole, "player")
	t.Setenv(envReport, rep)
	t.Setenv(envLinger, lingerCap.String())
	if detached, err := Detached(player, nil, ""); err != nil || !detached {
		t.Fatalf("Detached = (%v, %v)，期望 (true, nil)", detached, err)
	}
	r := waitReport(t, rep, 30*time.Second)
	ph := own(t, r.PID, player)
	relay := uint32(r.PPID)

	second := childless(t)
	holdRelay(player, second) // 同一播放器再次拉起，而上一个实例还活着
	got := heldPIDs(player)
	if !containsPID(got, relay) || !containsPID(got, uint32(second)) {
		t.Fatalf("上一个实例还活着，旧中转（%d）与新中转（%d）的句柄都应在，得到 %v", relay, second, got)
	}

	windows.TerminateProcess(ph, 1)
	if !exited(ph, 5*time.Second) {
		t.Fatal("假播放器没有退出")
	}
	third := childless(t)
	holdRelay(player, third) // 播放器已退出后再次拉起
	if got := heldPIDs(player); len(got) != 1 || got[0] != uint32(third) {
		t.Errorf("播放器已退出，旧句柄应全部关闭、只剩最新的 %d，得到 %v", third, got)
	}
}

// TestHoldRelayPerPlayer（Codex 第二轮提出）
//
// 防的缺陷与变异（实测变红）：句柄不按播放器分槽（一个全局槽或全局上限）——播放器 A 反复重启会换掉仍在运行的
// 播放器 B 的句柄，B 的中转 PID 又可被复用；路径大小写不同被当成两个播放器。只比较持有的句柄值的集合，不依赖
// 内部的键格式。
func TestHoldRelayPerPlayer(t *testing.T) {
	const a, b = `C:\Apps\A\player.exe`, `C:\Apps\B\player.exe`
	pid := childless(t)
	snap := func() map[windows.Handle]bool {
		heldMu.Lock()
		defer heldMu.Unlock()
		m := map[windows.Handle]bool{}
		for _, hs := range held {
			for _, h := range hs {
				m[h] = true
			}
		}
		return m
	}
	added := func(after, before map[windows.Handle]bool) (r []windows.Handle) {
		for h := range after {
			if !before[h] {
				r = append(r, h)
			}
		}
		return r
	}
	base := snap()
	holdRelay(a, pid)
	afterA := snap()
	holdRelay(b, pid)
	afterB := snap()
	ha, hb := added(afterA, base), added(afterB, afterA)
	if len(ha) != 1 || len(hb) != 1 || !afterB[ha[0]] {
		t.Fatalf("A、B 应各占一个槽、互不替换，得到 A 新增 %d 个、B 新增 %d 个，A 的句柄保留=%v", len(ha), len(hb), len(ha) == 1 && afterB[ha[0]])
	}

	holdRelay(a, pid) // 播放器 A 再次拉起
	afterA2 := snap()
	if !afterA2[hb[0]] {
		t.Errorf("播放器 A 再次拉起时，播放器 B 的句柄被换掉了")
	}
	if afterA2[ha[0]] || len(added(afterA2, afterB)) != 1 {
		t.Errorf("A 再次拉起后应以一个新句柄换掉自己的旧句柄")
	}

	holdRelay(strings.ToUpper(a), pid) // 大小写不同的同一路径是同一个播放器
	afterA3 := snap()
	if !afterA3[hb[0]] || len(added(afterA3, afterA2)) != 1 || len(added(afterA2, afterA3)) != 1 {
		t.Errorf("路径大小写不同应是同一个播放器：A 的句柄应被换成一个新的，B 的不动")
	}
}

// TestHoldRelayReleasesReplaced
//
// 防的缺陷与变异（实测变红）：换掉旧句柄时不关闭它（重启不成功的播放器会让 Detached 反复被调用，句柄无限累积）。
// 同一播放器反复拉起 50 次，本进程的句柄数不得随之增长。
func TestHoldRelayReleasesReplaced(t *testing.T) {
	const exe = `C:\Apps\Leak\player.exe`
	pid := childless(t)
	holdRelay(exe, pid) // 先占好这个槽
	before := handleCount(t)
	for range 50 {
		holdRelay(exe, pid)
	}
	if grew := handleCount(t) - before; grew > 3 {
		t.Errorf("同一播放器拉起 50 次，句柄数增长了 %d：被换掉的旧句柄没有关闭", grew)
	}
}

func handleCount(t *testing.T) int {
	t.Helper()
	var n uint32
	r, _, err := procGetProcessHandleCount.Call(uintptr(windows.CurrentProcess()), uintptr(unsafe.Pointer(&n)))
	if r == 0 {
		t.Fatalf("GetProcessHandleCount: %v", err)
	}
	return int(n)
}

var procGetProcessHandleCount = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetProcessHandleCount")

// TestInheritedUNCWorkingDirectoryFallsBack
//
// 防的缺陷与变异（实测变红）：command 只按传入的 dir 判断、不看继承的当前目录——网易云与 Spotify 都传空 dir，
// 本进程的当前目录是 UNC 时仍经 cmd 中转，播放器的工作目录变成 C:\Windows 却不打退路 Warn。
func TestInheritedUNCWorkingDirectoryFallsBack(t *testing.T) {
	const exe = `C:\Apps\player.exe`
	for _, c := range []struct {
		name      string
		wd        string
		wdErr     error
		dir       string
		wantShell bool
	}{
		{"空 dir、当前目录是 UNC → 退路", `\\server\share\PlayerCap`, nil, "", false},
		{"空 dir、当前目录是斜杠写法的 UNC → 退路", `//server/share/PlayerCap`, nil, "", false},
		{"空 dir、当前目录是本地目录 → 中转", `C:\PlayerCap`, nil, "", true},
		{"给了本地 dir、当前目录是 UNC → 中转（cmd 的当前目录取 dir）", `\\server\share\PlayerCap`, nil, `C:\Apps`, true},
		{"给了 UNC dir、当前目录是本地目录 → 退路", `C:\PlayerCap`, nil, `\\server\share\Apps`, false},
		{"取不到当前目录 → 按可转交处理", "", errors.New("no cwd"), "", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			old := getwd
			getwd = func() (string, error) { return c.wd, c.wdErr }
			t.Cleanup(func() { getwd = old })

			cmd, viaShell := command(context.Background(), exe, nil, c.dir)
			if viaShell != c.wantShell {
				t.Errorf("viaShell = %v，期望 %v", viaShell, c.wantShell)
			}
			if cmd.Dir != c.dir {
				t.Errorf("cmd.Dir = %q，期望原样的 %q（退路也不得把继承的目录写进 Dir）", cmd.Dir, c.dir)
			}
		})
	}
}
