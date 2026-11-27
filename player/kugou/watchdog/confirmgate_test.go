package watchdog

// 本文件只测确认闸门的前四步（设计 §6.2 第 1～4 步）：主进程、版本、子进程目录下的 libcef.dll、补丁指纹；
// 以及任一步不通过时不触碰数据目录。数据目录与歌词目录的选择在 confirmdatadir_test.go、confirmlyricdir_test.go。

import (
	"errors"
	"testing"
)

func TestConfirmHappyPath(t *testing.T) {
	m := newFakeMachine()
	got, err := confirmWith(m.deps())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	want := Confirmed{
		PID:        stubMainPID,
		ExePath:    stubExe,
		Version:    stubVersion,
		LibcefPath: stubLibcef,
		DataDir:    stubDataDir,
		LyricDir:   stubLyricDir,
	}
	if got != want {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
	if len(m.patchCalls) != 1 || m.patchCalls[0] != stubLibcef {
		t.Fatalf("checkPatch 调用 = %q，应只以子进程目录下的 libcef.dll 调用一次", m.patchCalls)
	}
}

func TestConfirmSnapshotFails(t *testing.T) {
	m := newFakeMachine()
	m.snapErr = errors.New("snapshot failed")
	_, err := confirmWith(m.deps())
	wantKind(t, err, ErrConfirmNotReady)
	wantNoDBAccess(t, m)
}

func TestConfirmNoMainProcess(t *testing.T) {
	m := newFakeMachine()
	m.procs = m.procs[:2] // 只剩 System 与 explorer
	_, err := confirmWith(m.deps())
	wantKind(t, err, ErrConfirmNotReady)
	wantNoDBAccess(t, m)
}

func TestConfirmEmptyVersion(t *testing.T) {
	m := newFakeMachine()
	m.version = map[string]string{}
	_, err := confirmWith(m.deps())
	wantKind(t, err, ErrConfirmUnsupported)
	wantNoDBAccess(t, m)
	if len(m.patchCalls) != 0 {
		t.Fatalf("版本读不出却查了补丁：%q", m.patchCalls)
	}
}

func TestConfirm10x(t *testing.T) {
	m := newFakeMachine()
	m.version[pathKey(stubExe)] = "10.2.1.1234"
	_, err := confirmWith(m.deps())
	wantKind(t, err, ErrConfirmUnsupported)
	wantNoDBAccess(t, m)
}

// 版本只读主进程的映像：子进程映像的版本读得出、主进程读不出，仍不支持。
func TestConfirmVersionFromMainImageOnly(t *testing.T) {
	m := newFakeMachine()
	m.version = map[string]string{pathKey(stubVerDir + `\KuGou.exe`): stubVersion}
	_, err := confirmWith(m.deps())
	wantKind(t, err, ErrConfirmUnsupported)
}

func TestConfirmNoChildren(t *testing.T) {
	m := newFakeMachine()
	m.procs = m.procs[:3] // System、explorer、主进程
	_, err := confirmWith(m.deps())
	wantKind(t, err, ErrConfirmNotReady)
	wantNoDBAccess(t, m)
	if len(m.patchCalls) != 0 {
		t.Fatalf("没有子进程却查了补丁：%q", m.patchCalls)
	}
}

func TestConfirmChildDirsDiffer(t *testing.T) {
	m := newFakeMachine()
	other := stubInstall + `\20.1.10.1`
	m.images[7004] = other + `\KuGou.exe`
	m.existing[pathKey(other+`\libcef.dll`)] = true // 两个目录都有 libcef.dll 也不行
	_, err := confirmWith(m.deps())
	wantKind(t, err, ErrConfirmUnsupported)
	wantNoDBAccess(t, m)
	if len(m.patchCalls) != 0 {
		t.Fatalf("子进程目录不一致却查了补丁：%q", m.patchCalls)
	}
}

func TestConfirmNoLibcefInChildDir(t *testing.T) {
	m := newFakeMachine()
	delete(m.existing, pathKey(stubLibcef))
	_, err := confirmWith(m.deps())
	wantKind(t, err, ErrConfirmUnsupported)
	wantNoDBAccess(t, m)
}

func TestConfirmNotFullyPatched(t *testing.T) {
	m := newFakeMachine()
	m.allPatched, m.canAutoFix = false, true
	_, err := confirmWith(m.deps())
	wantKind(t, err, ErrConfirmUnsupported)
	wantNoDBAccess(t, m)
}

func TestConfirmUnknownFingerprint(t *testing.T) {
	m := newFakeMachine()
	m.allPatched, m.canAutoFix = false, false
	_, err := confirmWith(m.deps())
	wantKind(t, err, ErrConfirmUnsupported)
	wantNoDBAccess(t, m)

	// canAutoFix 为假而 allPatched 为真（CheckPatchStatus 不会这样返回）：仍不支持，不只看 allPatched。
	m = newFakeMachine()
	m.allPatched, m.canAutoFix = true, false
	_, err = confirmWith(m.deps())
	wantKind(t, err, ErrConfirmUnsupported)
}

func TestConfirmCheckPatchReadError(t *testing.T) {
	m := newFakeMachine()
	m.allPatched, m.canAutoFix, m.patchErr = true, true, errors.New("read Patch1: EOF")
	_, err := confirmWith(m.deps())
	wantKind(t, err, ErrConfirmNotReady)
	wantNoDBAccess(t, m)
}

// 子进程目录里没有 libcef.dll，而注册表 KuGou8 指向的另一目录里有一份已打补丁的：
// 那只是磁盘上的一份文件，不能证明是当前进程加载的，仍不支持，且不拿它去查补丁。
func TestConfirmDoesNotFallBackToRegistryLibcef(t *testing.T) {
	m := newFakeMachine()
	const kugou8Libcef = stubInstall + `\20.1.30.1\libcef.dll`
	delete(m.existing, pathKey(stubLibcef))
	m.existing[pathKey(kugou8Libcef)] = true
	_, err := confirmWith(m.deps())
	wantKind(t, err, ErrConfirmUnsupported)
	for _, p := range m.patchCalls {
		if pathKey(p) == pathKey(kugou8Libcef) {
			t.Fatalf("以注册表 KuGou8 的 libcef.dll 调用了 checkPatch")
		}
	}
	if len(m.patchCalls) != 0 {
		t.Fatalf("子进程目录无 libcef.dll 却查了补丁：%q", m.patchCalls)
	}
}

// TestConfirmHDMainProcess 覆盖酷狗 20.1.61.28014 主进程为 HD_KuGou.exe、CEF 子进程为 20.1.61.28014\KuGou.exe 的确认流程，
// 以及外层启动器交接给 HD_KuGou.exe 的瞬态拦截。
func TestConfirmHDMainProcess(t *testing.T) {
	const (
		hdPID    uint32 = 25940
		hdExe           = stubInstall + `\HD_KuGou.exe`
		hdVer           = "20.1.61.28014"
		hdVerDir        = stubInstall + `\` + hdVer
		hdLibcef        = hdVerDir + `\libcef.dll`
		hdSubExe        = hdVerDir + `\KuGou.exe`
	)

	t.Run("HD_KuGou.exe 主进程 + 5 个版本子目录 KuGou.exe 子进程 → Confirm 通过", func(t *testing.T) {
		m := newFakeMachine()
		m.procs = []proc{
			{PID: 4, PPID: 0, Name: "System"},
			{PID: 3532, PPID: 4, Name: "explorer.exe"},
			{PID: hdPID, PPID: 3532, Name: HDTargetProcessName},
			{PID: 15876, PPID: hdPID, Name: TargetProcessName},
			{PID: 26100, PPID: hdPID, Name: TargetProcessName},
			{PID: 26200, PPID: hdPID, Name: TargetProcessName},
			{PID: 26300, PPID: hdPID, Name: TargetProcessName},
			{PID: 26400, PPID: hdPID, Name: TargetProcessName},
			{PID: 27000, PPID: hdPID, Name: "KGService.exe"},
		}
		m.images = map[uint32]string{
			hdPID: hdExe,
			15876: hdSubExe,
			26100: hdSubExe,
			26200: hdSubExe,
			26300: hdSubExe,
			26400: hdSubExe,
		}
		m.version = map[string]string{pathKey(hdExe): hdVer}
		m.existing[pathKey(hdLibcef)] = true
		m.holders[pathKey(stubDB)] = []uint32{5555, hdPID}

		got, err := confirmWith(m.deps())
		if err != nil {
			t.Fatalf("confirmWith 失败: %v", err)
		}
		want := Confirmed{
			PID:        hdPID,
			ExePath:    hdExe,
			Version:    hdVer,
			LibcefPath: hdLibcef,
			DataDir:    stubDataDir,
			LyricDir:   stubLyricDir,
		}
		if got != want {
			t.Fatalf("got  %+v\nwant %+v", got, want)
		}
		if len(m.patchCalls) != 1 || m.patchCalls[0] != hdLibcef {
			t.Fatalf("checkPatch 调用 = %q，应只以 %q 调用一次", m.patchCalls, hdLibcef)
		}
	})

	t.Run("启动器交接瞬态（父 KuGou.exe + 子 HD_KuGou.exe）→ ErrConfirmNotReady 且不触碰补丁与数据库", func(t *testing.T) {
		m := newFakeMachine()
		m.procs = []proc{
			{PID: 4, PPID: 0, Name: "System"},
			{PID: 3532, PPID: 4, Name: "explorer.exe"},
			{PID: 1000, PPID: 3532, Name: TargetProcessName},
			{PID: hdPID, PPID: 1000, Name: HDTargetProcessName},
		}
		m.images = map[uint32]string{
			1000:  stubExe,
			hdPID: hdExe,
		}
		_, err := confirmWith(m.deps())
		wantKind(t, err, ErrConfirmNotReady)
		wantNoDBAccess(t, m)
		if len(m.patchCalls) != 0 {
			t.Fatalf("交接瞬态不应检查补丁：%q", m.patchCalls)
		}
	})
}
