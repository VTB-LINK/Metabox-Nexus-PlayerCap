package launch

// 本文件只测一件事：照 OBS Autostarter 插件的算法结束「PlayerCap」时，经 Detached 拉起的播放器不被连带结束
// （issue #127）。
//
// 结构与真实情形一致：测试进程 → 假 PlayerCap（测试二进制，按 envMode 拉起假播放器后一直运行）→ 假播放器。
// 测试进程照 Autostarter 的 QuitProcess 结束假 PlayerCap：先按父进程 ID（Toolhelp 快照的 th32ParentProcessID）
// 递归 TerminateProcess 它的全部子进程，再结束它本身。对照组用修复前的 exec.Command(...).Start()，假播放器应被
// 连带结束；实验组用 Detached，假播放器应存活。对照组不红，说明模拟的算法没有生效，实验组的存活也就说明不了问题。

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// TestAutostarterQuitSparesDetachedPlayer
//
// 防的缺陷与变异（实测变红）：Detached 内部改为直接 exec.Command(...).Start()（仍返回 detached=true），实验组的
// 假播放器被连带结束。
func TestAutostarterQuitSparesDetachedPlayer(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		mode    string
		survive bool
	}{{"direct", false}, {"detached", true}} {
		t.Run(c.mode, func(t *testing.T) {
			tmp := t.TempDir()
			player := fakePlayer(t, filepath.Join(tmp, "player", "player.exe"))
			selfRep, playerRep := filepath.Join(tmp, "playercap.json"), filepath.Join(tmp, "player.json")

			pc := exec.Command(self)
			pc.Env = append(os.Environ(), envRole+"=playercap", envMode+"="+c.mode,
				envPlayerExe+"="+player, envReport+"="+selfRep, envPlayerReport+"="+playerRep)
			pc.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW} // 测试进程没有控制台时不闪窗
			if err := pc.Start(); err != nil {
				t.Fatalf("启动假 PlayerCap: %v", err)
			}
			pch := own(t, pc.Process.Pid, self)

			pr := waitReport(t, selfRep, 30*time.Second)
			if pr.Err != "" {
				t.Fatalf("假 PlayerCap 拉起假播放器失败: %s", pr.Err)
			}
			if pr.Detached != (c.mode == "detached") {
				t.Fatalf("假 PlayerCap 报告 detached=%v", pr.Detached)
			}
			r := waitReport(t, playerRep, 30*time.Second)
			ph := own(t, r.PID, player)
			if under := r.PPID == pc.Process.Pid; under == c.survive {
				t.Fatalf("假播放器的父进程 %d、假 PlayerCap %d：父子关系与 %s 拉起不符", r.PPID, pc.Process.Pid, c.mode)
			}

			quitProcess(t, pch, uint32(pc.Process.Pid), map[string]bool{"player.exe": true, "cmd.exe": true, "conhost.exe": true})
			if !exited(pch, 5*time.Second) {
				t.Fatal("假 PlayerCap 没有被结束")
			}
			pc.Wait()
			if c.survive {
				if exited(ph, 1500*time.Millisecond) {
					t.Fatal("经 Detached 拉起的假播放器随假 PlayerCap 一并被结束了")
				}
			} else if !exited(ph, 5*time.Second) {
				t.Fatal("对照组：直接启动的假播放器没有被连带结束——模拟的算法没有生效")
			}
		})
	}
}

// quitProcess 照 Autostarter 的 QuitProcess 结束 h 所指的进程 pid：先按父进程 ID 递归结束它的全部子进程，再结束它。
//
// 只对这棵子树动手：子进程须不早于父进程创建（排除父进程 ID 被复用后挂上来的无关进程），映像名须在 allowed 里
// （否则用例失败、不结束它），测试进程自身永不结束。
func quitProcess(t *testing.T, h windows.Handle, pid uint32, allowed map[string]bool) {
	t.Helper()
	if pid == uint32(os.Getpid()) {
		t.Fatal("要结束的是测试进程自身")
	}
	created := creationTime(h)
	for _, e := range snapshot() {
		if e.ParentProcessID != pid || e.ProcessID == pid {
			continue
		}
		ch, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, e.ProcessID)
		if err != nil {
			continue // 已退出
		}
		if creationTime(ch) < created {
			windows.CloseHandle(ch)
			continue
		}
		if name := windows.UTF16ToString(e.ExeFile[:]); !allowed[strings.ToLower(name)] {
			windows.CloseHandle(ch)
			t.Fatalf("假 PlayerCap 的子树里有预料之外的进程 %s（%d），不结束它", name, e.ProcessID)
		}
		quitProcess(t, ch, e.ProcessID, allowed)
		windows.CloseHandle(ch)
	}
	windows.TerminateProcess(h, 1)
}

func creationTime(h windows.Handle) int64 {
	var c, e, k, u windows.Filetime
	if windows.GetProcessTimes(h, &c, &e, &k, &u) != nil {
		return 0
	}
	return c.Nanoseconds()
}
