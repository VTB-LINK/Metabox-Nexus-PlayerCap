package launch

// 本文件只测 Running：按映像名在 Toolhelp 快照里找全部同名进程、逐个读映像路径，读不到的保留为空串（issue #212）。
// 只读快照与本测试进程自己的映像路径，不打开别的进程以外的任何东西。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunning
//
// 防的缺陷与变异（均实测变红）：跳过读不到映像路径的进程（「[System Process]」那例为空）；映像名区分大小写；按前缀
// 而不是整名匹配；快照里的进程不分名字一律收进来。
func TestRunning(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Base(self)
	find := func(t *testing.T, n string) []Proc {
		t.Helper()
		ps, err := Running(n)
		if err != nil {
			t.Fatalf("Running(%q): %v", n, err)
		}
		return ps
	}
	hasSelf := func(ps []Proc) bool {
		for _, p := range ps {
			if int(p.PID) == os.Getpid() && samePath(p.ExePath, self) {
				return true
			}
		}
		return false
	}

	t.Run("本测试进程：在结果里且读出映像路径", func(t *testing.T) {
		if ps := find(t, name); !hasSelf(ps) {
			t.Errorf("Running(%q) = %+v，没有本进程 %d（%s）", name, ps, os.Getpid(), self)
		}
	})
	t.Run("映像名不分大小写", func(t *testing.T) {
		if ps := find(t, strings.ToUpper(name)); !hasSelf(ps) {
			t.Errorf("Running(%q) = %+v，没有本进程", strings.ToUpper(name), ps)
		}
	})
	t.Run("反例：名字的前缀不算", func(t *testing.T) {
		if ps := find(t, strings.TrimSuffix(name, ".exe")); hasSelf(ps) {
			t.Errorf("Running(去掉 .exe 的名字) 把本进程也算进来了：%+v", ps)
		}
	})
	t.Run("反例：没有这个名字的进程", func(t *testing.T) {
		if ps := find(t, "no-such-player-212.exe"); len(ps) != 0 {
			t.Errorf("Running = %+v，期望空", ps)
		}
	})
	// 系统空闲进程（PID 0）谁也打不开，读不到映像路径：它仍在结果里，路径为空串。
	t.Run("读不到映像路径的进程保留、路径为空", func(t *testing.T) {
		ps := find(t, "[System Process]")
		if len(ps) != 1 || ps[0].PID != 0 || ps[0].ExePath != "" {
			t.Errorf("Running(\"[System Process]\") = %+v，期望恰好 PID 0、路径为空", ps)
		}
	})
}
