package watchdog

// 本文件只测 killAndPatch：酷狗启动之后、结束它之前，重新定位安装并与修补前预选的 exe、libcef.dll 比较，
// 不同或定位出错就不结束酷狗、不触发 helper。

import (
	"errors"
	"path/filepath"
	"testing"
)

// TestKillAndPatchRechecksInstall 钉死「先确认、再结束」：EnsurePatched 在酷狗没运行时按注册表预选安装、弹 UAC，
// 等到的只是「任意 KuGou.exe 出现」。主播打开的若是另一份安装，照旧结束它、给预选那份打补丁、再启动预选那份，
// 就把主播的酷狗换掉了。路径比较 Clean 后不分大小写。kill 与 trigger 一律是桩，不结束任何进程。
//
// 变异自证（均实测变红）：去掉比较；只比 exe 不比 libcef.dll；重新定位出错时照旧比较、修补；路径比较区分大小写；
// 先结束酷狗、后确认；重新定位的错误按 %w 包装（会被认作 ErrInstallNotFound）。
func TestKillAndPatchRechecksInstall(t *testing.T) {
	const (
		exe = `C:\Program Files\KuGou\KGMusic\KuGou.exe`
		lc  = `C:\Program Files\KuGou\KGMusic\20.1.22.27795\libcef.dll`
	)
	locateErr := errors.Join(ErrInstallNotFound, errors.New("stub"))
	cases := []struct {
		name               string
		gotExe, gotLc      string
		locateErr          error
		triggerErr         error
		wantPatch          bool // 是否结束酷狗并触发 helper
		wantChanged        bool // 错误是否为 ErrInstallChanged
		wantErr            bool // 是否返回错误
		wantNotInstallMiss bool // 错误不得被认作 ErrInstallNotFound（否则 Start 按找不到安装分流）
	}{
		{name: "相同 → 结束并修补", gotExe: exe, gotLc: lc, wantPatch: true},
		{name: "只差大小写与未 Clean 的写法 → 视为相同", gotExe: `c:\program files\kugou\kgmusic\.\KUGOU.EXE`, gotLc: `C:\PROGRAM FILES\KuGou\KGMusic\20.1.22.27795\\libcef.dll`, wantPatch: true},
		{name: "反例：exe 不同（另一份安装）→ 不结束、不修补", gotExe: `D:\Apps\KuGou\KuGou.exe`, gotLc: lc, wantChanged: true, wantErr: true},
		{name: "反例：libcef.dll 不同（同一安装的另一版本目录）→ 不结束、不修补", gotExe: exe, gotLc: `C:\Program Files\KuGou\KGMusic\20.1.23.1\libcef.dll`, wantChanged: true, wantErr: true},
		{name: "反例：重新定位出错（一并返回的路径恰与预选相同）→ 不结束、不修补，且不被认作找不到安装", gotExe: exe, gotLc: lc, locateErr: locateErr, wantChanged: true, wantErr: true, wantNotInstallMiss: true},
		{name: "反例：重新定位出错、路径为空 → 不结束、不修补", locateErr: locateErr, wantChanged: true, wantErr: true, wantNotInstallMiss: true},
		{name: "相同而 helper 修补失败 → 已结束酷狗，返回修补错误", gotExe: exe, gotLc: lc, triggerErr: errors.New("ERROR: x"), wantPatch: true, wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var calls []string
			locate := func() (string, string, error) {
				calls = append(calls, "locate")
				return c.gotExe, c.gotLc, c.locateErr
			}
			kill := func() { calls = append(calls, "kill") }
			trigger := func() error { calls = append(calls, "trigger"); return c.triggerErr }

			err := killAndPatch(exe, lc, locate, kill, trigger)

			want := []string{"locate"}
			if c.wantPatch {
				want = append(want, "kill", "trigger")
			}
			if len(calls) != len(want) {
				t.Fatalf("调用序列 = %v，应为 %v", calls, want)
			}
			for i := range want {
				if calls[i] != want[i] {
					t.Fatalf("调用序列 = %v，应为 %v", calls, want)
				}
			}
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v，应%s出错", err, map[bool]string{true: "", false: "不"}[c.wantErr])
			}
			if got := errors.Is(err, ErrInstallChanged); got != c.wantChanged {
				t.Fatalf("errors.Is(err, ErrInstallChanged) = %v，应为 %v（err = %v）", got, c.wantChanged, err)
			}
			if c.wantNotInstallMiss && errors.Is(err, ErrInstallNotFound) {
				t.Fatalf("重新定位出错的错误不得被认作 ErrInstallNotFound：%v", err)
			}
		})
	}
}

// TestKillAndPatchLocatesByRunningProcess 钉死修补前重新确认安装须按进程定位成功：生产的 locate 是
// locateRunningInstall，进程来源出错时原样返回错误、不退回注册表。反例：冷启动按注册表预选 A，主播启动的是 B，
// 重新快照时 B 的某个子进程恰好退出、映像路径取不到；若退回注册表就又得到 A，比较通过，结束 B、修补并启动 A。
// 经 stubLocate 注入进程、注册表与安装根，不查询本机，kill 与 trigger 是桩。
//
// 变异自证（均实测变红）：locateRunningInstall 在进程来源出错时退回注册表（即改用 FindKuGouInstall）；
// 进程来源出错时返回 nil 错误。
func TestKillAndPatchLocatesByRunningProcess(t *testing.T) {
	setup := func(t *testing.T) (a, b string) {
		root := t.TempDir()
		a, b = filepath.Join(root, "A"), filepath.Join(root, "B")
		mkInstall(t, a, "20.1.22.27795")
		mkInstall(t, b, "20.1.22.27795")
		return a, b
	}
	regA := func(a string) map[string]string {
		return map[string]string{"AppPath": a, "KuGou8": filepath.Join(a, "20.1.22.27795")}
	}
	running := func(dir string) runningKuGou {
		return runningKuGou{PID: 100, ExePath: filepath.Join(dir, "KuGou.exe"), ChildDirs: []string{filepath.Join(dir, "20.1.22.27795")}}
	}
	imageGone := errors.Join(ErrConfirmNotReady, errors.New("query the image path of KuGou.exe (PID 7001): access denied"))

	cases := []struct {
		name      string
		run       func(a, b string) runningKuGou
		runErr    error
		wantPatch bool
	}{
		{"进程来源给出与预选相同的安装 → 照常修补", func(a, _ string) runningKuGou { return running(a) }, nil, true},
		{"反例：进程来源给出另一份安装 B → 不结束、不修补", func(_, b string) runningKuGou { return running(b) }, nil, false},
		{"反例：进程来源出错、注册表指向预选的 A → 不退回注册表，不结束、不修补", func(_, b string) runningKuGou { return running(b) }, imageGone, false},
		{"反例：进程来源出错且没有返回内容 → 不结束、不修补", func(_, _ string) runningKuGou { return runningKuGou{} }, imageGone, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, b := setup(t)
			stubLocate(t, c.run(a, b), c.runErr, regA(a))
			var calls []string
			err := killAndPatch(filepath.Join(a, "KuGou.exe"), filepath.Join(a, "20.1.22.27795", "libcef.dll"), locateRunningInstall,
				func() { calls = append(calls, "kill") },
				func() error { calls = append(calls, "trigger"); return nil })
			if c.wantPatch {
				if err != nil || len(calls) != 2 {
					t.Fatalf("err = %v，调用 = %v；应无错误并依次结束、触发", err, calls)
				}
				return
			}
			if !errors.Is(err, ErrInstallChanged) || len(calls) != 0 {
				t.Fatalf("err = %v，调用 = %v；应为 ErrInstallChanged 且不结束、不触发", err, calls)
			}
		})
	}

	t.Run("locateRunningInstall：进程来源出错时原样返回错误、路径为空", func(t *testing.T) {
		a, b := setup(t)
		stubLocate(t, running(b), imageGone, regA(a))
		exe, lc, err := locateRunningInstall()
		if !errors.Is(err, ErrConfirmNotReady) || exe != "" || lc != "" {
			t.Fatalf("locateRunningInstall = (%q, %q, %v)，应为空路径与进程来源的错误", exe, lc, err)
		}
	})
}
