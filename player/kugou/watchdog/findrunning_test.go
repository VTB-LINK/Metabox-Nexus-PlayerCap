package watchdog

// 本文件只测 findRunningKuGou：从进程快照里定位酷狗主进程，并取其 CEF 子进程的映像目录。

import (
	"errors"
	"strings"
	"testing"
)

func TestFindRunningSingleMain(t *testing.T) {
	m := newFakeMachine()
	got, err := findRunningKuGou(m.procs, m.deps().imagePath)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got.PID != stubMainPID || got.ExePath != stubExe {
		t.Fatalf("主进程 = (%d, %q)，应为 (%d, %q)", got.PID, got.ExePath, stubMainPID, stubExe)
	}
	// 4 个子进程在同一版本目录，去重为一个；父进程是主进程而映像名不是 KuGou.exe 的（KGService.exe）不算。
	if len(got.ChildDirs) != 1 || got.ChildDirs[0] != stubVerDir {
		t.Fatalf("ChildDirs = %q，应为 [%q]", got.ChildDirs, stubVerDir)
	}
}

func TestFindRunningNone(t *testing.T) {
	procs := []proc{{PID: 100, PPID: 4, Name: "explorer.exe"}, {PID: 200, PPID: 100, Name: "KuGouHelper.exe"}}
	_, err := findRunningKuGou(procs, newFakeMachine().deps().imagePath)
	wantKind(t, err, ErrConfirmNotReady)
	if !errors.Is(err, ErrNotRunning) {
		t.Fatalf("无酷狗进程时应同时包装 ErrNotRunning，实得 %v", err)
	}

	_, err = findRunningKuGou(nil, newFakeMachine().deps().imagePath)
	wantKind(t, err, ErrConfirmNotReady)
	if !errors.Is(err, ErrNotRunning) {
		t.Fatalf("空快照时应同时包装 ErrNotRunning，实得 %v", err)
	}
}

// 两个父进程都不是 KuGou.exe 的 KuGou.exe：按未就绪，不取 PID 最小（与 procscan.MainPID 不同，有意）。
// 主进程已退出、子进程成了孤儿时也落在这里（孤儿的父 PID 不在同名集合里）。
func TestFindRunningTwoMains(t *testing.T) {
	m := newFakeMachine()
	m.procs = append(m.procs, proc{PID: 9000, PPID: 100, Name: "KuGou.exe"})
	m.images[9000] = stubExe
	_, err := findRunningKuGou(m.procs, m.deps().imagePath)
	wantKind(t, err, ErrConfirmNotReady)
	if errors.Is(err, ErrNotRunning) {
		t.Fatalf("多主进程时不得包装 ErrNotRunning：%v", err)
	}

	orphan := newFakeMachine()
	orphan.procs = append(orphan.procs, proc{PID: 9001, PPID: 12345, Name: "KuGou.exe"})
	_, err = findRunningKuGou(orphan.procs, orphan.deps().imagePath)
	wantKind(t, err, ErrConfirmNotReady)
	if errors.Is(err, ErrNotRunning) {
		t.Fatalf("孤儿进程并存时不得包装 ErrNotRunning：%v", err)
	}
}

func TestFindRunningCaseInsensitive(t *testing.T) {
	m := newFakeMachine()
	for i := range m.procs {
		switch m.procs[i].PID {
		case stubMainPID:
			m.procs[i].Name = "KUGOU.EXE"
		case 7001, 7002:
			m.procs[i].Name = "kugou.exe"
		}
	}
	// 同一目录大小写不同的两种写法去重为一个，保留先出现的写法。
	m.images[7003] = `C:\PROGRAM FILES\KuGou\KGMusic\20.1.22.27795\KUGOU.EXE`
	got, err := findRunningKuGou(m.procs, m.deps().imagePath)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got.PID != stubMainPID {
		t.Fatalf("PID = %d，应为 %d", got.PID, stubMainPID)
	}
	if len(got.ChildDirs) != 1 || got.ChildDirs[0] != stubVerDir {
		t.Fatalf("ChildDirs = %q，应为 [%q]", got.ChildDirs, stubVerDir)
	}
}

func TestFindRunningImagePathFails(t *testing.T) {
	t.Run("主进程", func(t *testing.T) {
		m := newFakeMachine()
		m.imageErr[stubMainPID] = errors.New("access denied")
		_, err := findRunningKuGou(m.procs, m.deps().imagePath)
		wantKind(t, err, ErrConfirmNotReady)
		if errors.Is(err, ErrNotRunning) {
			t.Fatalf("路径查询失败时不得包装 ErrNotRunning：%v", err)
		}
	})
	t.Run("任一子进程", func(t *testing.T) {
		m := newFakeMachine()
		m.imageErr[7003] = errors.New("process exited")
		_, err := findRunningKuGou(m.procs, m.deps().imagePath)
		wantKind(t, err, ErrConfirmNotReady)
		if errors.Is(err, ErrNotRunning) {
			t.Fatalf("子进程路径查询失败时不得包装 ErrNotRunning：%v", err)
		}
	})
}

// 快照之后 PID 被别的程序复用：映像名不再是 KuGou.exe，按未就绪，不拿别人的目录当酷狗的。
func TestFindRunningPIDReused(t *testing.T) {
	t.Run("主进程", func(t *testing.T) {
		m := newFakeMachine()
		m.images[stubMainPID] = `C:\Windows\System32\notepad.exe`
		_, err := findRunningKuGou(m.procs, m.deps().imagePath)
		wantKind(t, err, ErrConfirmNotReady)
		if errors.Is(err, ErrNotRunning) {
			t.Fatalf("PID 复用时不得包装 ErrNotRunning：%v", err)
		}
	})
	t.Run("子进程", func(t *testing.T) {
		m := newFakeMachine()
		m.images[7002] = `C:\Other\app.exe`
		_, err := findRunningKuGou(m.procs, m.deps().imagePath)
		wantKind(t, err, ErrConfirmNotReady)
		if errors.Is(err, ErrNotRunning) {
			t.Fatalf("子进程 PID 复用时不得包装 ErrNotRunning：%v", err)
		}
	})
}

// 没有子进程不是 findRunningKuGou 的错误（Task 7 只要主进程映像），由 confirmWith 判未就绪。
func TestFindRunningNoChildren(t *testing.T) {
	procs := []proc{{PID: 100, PPID: 4, Name: "explorer.exe"}, {PID: stubMainPID, PPID: 100, Name: "KuGou.exe"}}
	got, err := findRunningKuGou(procs, newFakeMachine().deps().imagePath)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got.PID != stubMainPID || len(got.ChildDirs) != 0 {
		t.Fatalf("got = %+v，应为主进程 %d、无子进程目录", got, stubMainPID)
	}
}

// 子进程分在两个目录：两个都列出，由 confirmWith 判不支持。
func TestFindRunningChildDirsDiffer(t *testing.T) {
	m := newFakeMachine()
	m.images[7004] = stubInstall + `\20.1.10.1\KuGou.exe`
	got, err := findRunningKuGou(m.procs, m.deps().imagePath)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(got.ChildDirs) != 2 {
		t.Fatalf("ChildDirs = %q，应为两个目录", got.ChildDirs)
	}
}

// TestFindRunningKuGouHDTopology 复现酷狗 20.1.61.28014 真机进程树拓扑：
// 主进程 HD_KuGou.exe 派生 5 个 20.1.61.28014\KuGou.exe CEF 子进程，以及启动器交接瞬态、成环无主与双主进程情形。
func TestFindRunningKuGouHDTopology(t *testing.T) {
	const (
		hdPID     uint32 = 25940
		hdExe            = `C:\Program Files\KuGou\KGMusic\HD_KuGou.exe`
		hdVerDir         = `C:\Program Files\KuGou\KGMusic\20.1.61.28014`
		hdSubExe         = hdVerDir + `\KuGou.exe`
		launchExe        = `C:\Program Files\KuGou\KGMusic\KuGou.exe`
	)

	makeImageFn := func(m map[uint32]string) func(uint32) (string, error) {
		return func(pid uint32) (string, error) {
			if p, ok := m[pid]; ok {
				return p, nil
			}
			return "", errors.New("no such process")
		}
	}

	t.Run("真机拓扑：HD_KuGou.exe 主进程 + 5 个版本子目录 KuGou.exe 子进程", func(t *testing.T) {
		procs := []proc{
			{PID: 4, PPID: 0, Name: "System"},
			{PID: 3532, PPID: 4, Name: "explorer.exe"},
			{PID: hdPID, PPID: 3532, Name: HDTargetProcessName},
			{PID: 15876, PPID: hdPID, Name: TargetProcessName}, // 故意放入比主进程 PID 更小的子进程
			{PID: 26100, PPID: hdPID, Name: TargetProcessName},
			{PID: 26200, PPID: hdPID, Name: TargetProcessName},
			{PID: 26300, PPID: hdPID, Name: TargetProcessName},
			{PID: 26400, PPID: hdPID, Name: TargetProcessName},
			{PID: 27000, PPID: hdPID, Name: "KGService.exe"},
		}
		imgs := map[uint32]string{
			hdPID: hdExe,
			15876: hdSubExe,
			26100: hdSubExe,
			26200: hdSubExe,
			26300: hdSubExe,
			26400: hdSubExe,
		}
		got, err := findRunningKuGou(procs, makeImageFn(imgs))
		if err != nil {
			t.Fatalf("findRunningKuGou 失败: %v", err)
		}
		if got.PID != hdPID || got.ExePath != hdExe {
			t.Fatalf("主进程 = (%d, %q)，应为 (%d, %q)", got.PID, got.ExePath, hdPID, hdExe)
		}
		if len(got.ChildDirs) != 1 || got.ChildDirs[0] != hdVerDir {
			t.Fatalf("ChildDirs = %q，应为 [%q]", got.ChildDirs, hdVerDir)
		}
	})

	t.Run("大小写不敏感：hd_kugou.EXE + KUGOU.EXE", func(t *testing.T) {
		procs := []proc{
			{PID: hdPID, PPID: 3532, Name: "hd_kugou.EXE"},
			{PID: 26100, PPID: hdPID, Name: "KUGOU.EXE"},
		}
		imgs := map[uint32]string{
			hdPID: `C:\Program Files\KuGou\KGMusic\hd_kugou.EXE`,
			26100: hdVerDir + `\KUGOU.EXE`,
		}
		got, err := findRunningKuGou(procs, makeImageFn(imgs))
		if err != nil {
			t.Fatalf("findRunningKuGou 失败: %v", err)
		}
		if got.PID != hdPID || len(got.ChildDirs) != 1 || got.ChildDirs[0] != hdVerDir {
			t.Fatalf("got = %+v，应命中主进程 %d 及目录 %q", got, hdPID, hdVerDir)
		}
	})

	t.Run("启动器交接瞬态：父 KuGou.exe 与子 HD_KuGou.exe 并存 → ErrConfirmNotReady 且非 ErrNotRunning", func(t *testing.T) {
		for _, childImg := range []string{hdExe, `C:\Program Files\KuGou\KGMusic\hd_kugou.exe`} {
			procs := []proc{
				{PID: 1000, PPID: 3532, Name: TargetProcessName},
				{PID: hdPID, PPID: 1000, Name: HDTargetProcessName},
			}
			imgs := map[uint32]string{
				1000:  launchExe,
				hdPID: childImg,
			}
			_, err := findRunningKuGou(procs, makeImageFn(imgs))
			wantKind(t, err, ErrConfirmNotReady)
			if errors.Is(err, ErrNotRunning) {
				t.Fatalf("交接瞬态不得包装 ErrNotRunning：%v", err)
			}
		}
	})

	t.Run("父子成环（存在酷狗进程但无主进程）→ ErrConfirmNotReady 且非 ErrNotRunning", func(t *testing.T) {
		procs := []proc{
			{PID: 10, PPID: 11, Name: TargetProcessName},
			{PID: 11, PPID: 10, Name: HDTargetProcessName},
		}
		_, err := findRunningKuGou(procs, makeImageFn(nil))
		wantKind(t, err, ErrConfirmNotReady)
		if errors.Is(err, ErrNotRunning) {
			t.Fatalf("成环无主时存在酷狗进程，不得包装 ErrNotRunning：%v", err)
		}
	})

	t.Run("双独立主进程（KuGou.exe 与 HD_KuGou.exe 互不为父子）→ 报 2 个主进程且非 ErrNotRunning", func(t *testing.T) {
		procs := []proc{
			{PID: 1000, PPID: 3532, Name: TargetProcessName},
			{PID: hdPID, PPID: 3532, Name: HDTargetProcessName},
		}
		imgs := map[uint32]string{1000: launchExe, hdPID: hdExe}
		_, err := findRunningKuGou(procs, makeImageFn(imgs))
		wantKind(t, err, ErrConfirmNotReady)
		if errors.Is(err, ErrNotRunning) {
			t.Fatalf("双独立主进程时不得包装 ErrNotRunning：%v", err)
		}
		if !strings.Contains(err.Error(), "2") {
			t.Fatalf("错误应包含主进程数 2，实得 %v", err)
		}
	})
}

// TestFindRunningKuGouExactWhitelist 守住精确双进程名白名单红线：严禁子串/前缀/后缀匹配。
func TestFindRunningKuGouExactWhitelist(t *testing.T) {
	for _, okName := range []string{"KuGou.exe", "kugou.exe", "KUGOU.EXE", "HD_KuGou.exe", "hd_kugou.exe", "HD_KUGOU.EXE"} {
		if !isKuGouProcessName(okName) {
			t.Errorf("isKuGouProcessName(%q) = false，应为 true", okName)
		}
	}
	nearNames := []string{
		"KuGouConcept.exe",
		"Fanxing_KuGou.exe",
		"my_kugou.exe",
		"HD_KuGou2.exe",
		"HD_KuGou.exe.bak",
		"KuGou.exe.old",
		"KuGou",
		"",
	}
	for _, bad := range nearNames {
		if isKuGouProcessName(bad) {
			t.Errorf("isKuGouProcessName(%q) = true，应为 false（严禁模糊匹配）", bad)
		}
	}

	// 只有近似名进程在运行时，findRunningKuGou 必须视为未运行（ErrConfirmNotReady + ErrNotRunning）。
	var onlyNear []proc
	for i, name := range nearNames {
		if name == "" {
			continue
		}
		onlyNear = append(onlyNear, proc{PID: uint32(1000 + i), PPID: 4, Name: name})
	}
	_, err := findRunningKuGou(onlyNear, func(uint32) (string, error) {
		t.Fatalf("近似进程名不应触发 imagePath 查询")
		return "", nil
	})
	wantKind(t, err, ErrConfirmNotReady)
	if !errors.Is(err, ErrNotRunning) {
		t.Fatalf("仅有近似名进程时应报 ErrNotRunning，实得 %v", err)
	}

	// 与合法 HD_KuGou.exe 共存时，近似名进程不被计为主进程或 CEF 子进程。
	const (
		hdPID    uint32 = 25940
		hdExe           = `C:\Program Files\KuGou\KGMusic\HD_KuGou.exe`
		hdVerDir        = `C:\Program Files\KuGou\KGMusic\20.1.61.28014`
	)
	mixed := append([]proc{
		{PID: hdPID, PPID: 4, Name: HDTargetProcessName},
		{PID: 26001, PPID: hdPID, Name: TargetProcessName},
		{PID: 26002, PPID: hdPID, Name: "KuGouConcept.exe"},
		{PID: 26003, PPID: hdPID, Name: "HD_KuGou2.exe"},
	}, onlyNear...)
	got, err := findRunningKuGou(mixed, func(pid uint32) (string, error) {
		switch pid {
		case hdPID:
			return hdExe, nil
		case 26001:
			return hdVerDir + `\KuGou.exe`, nil
		default:
			t.Fatalf("不应查询非白名单进程 PID %d", pid)
			return "", nil
		}
	})
	if err != nil {
		t.Fatalf("findRunningKuGou 失败: %v", err)
	}
	if got.PID != hdPID || len(got.ChildDirs) != 1 || got.ChildDirs[0] != hdVerDir {
		t.Fatalf("got = %+v，应只认白名单进程", got)
	}
}
