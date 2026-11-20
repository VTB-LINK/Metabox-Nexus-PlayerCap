package watchdog

// 本文件只测汽水的商店版门禁（issue #212）：EnsureInspector 先于「9229 已可用」的早退过门禁，activateInspector 在
// 注入之前自己再过一次；gatePID 读映像路径。门禁与 9229 的探测经包级变量换成桩；用到的 pid 都不是汽水，激活走不到
// OpenProcess。

import (
	"errors"
	"os"
	"testing"

	"Metabox-Nexus-PlayerCap/player/launch"
)

// noSodaPID 是不存在的进程号（Windows 的进程号是 4 的倍数且远小于它）。
const noSodaPID = 0x7FFFFFFC

// stubGate 换上门禁与 9229 探测的桩，返回两者被调用的次数。
func stubGate(t *testing.T, gateErr error, up bool) (gates, ups *int) {
	t.Helper()
	oldGate, oldUp := gateFn, upFn
	t.Cleanup(func() { gateFn, upFn = oldGate, oldUp })
	gates, ups = new(int), new(int)
	gateFn = func(int) error { *gates++; return gateErr }
	upFn = func() bool { *ups++; return up }
	return gates, ups
}

// TestEnsureInspectorGateFirst：门禁不过时原样返回它的错误，连 9229 是否已可用都不看；门禁通过且 9229 已可用时
// 返回 nil，是对照。
//
// 防的缺陷与变异（均实测变红）：去掉 EnsureInspector 开头的门禁（9229 已可用时返回 nil）；门禁挪到 9229 的探测之后。
func TestEnsureInspectorGateFirst(t *testing.T) {
	store := &launch.PackagedError{ExePath: `C:\Program Files\WindowsApps\Soda_2.1.0.0_x64__abc\SodaMusic.exe`}
	for _, gateErr := range []error{store, launch.ErrExeUnknown} {
		t.Run(gateErr.Error(), func(t *testing.T) {
			gates, ups := stubGate(t, gateErr, true)
			if err := EnsureInspector(noSodaPID); err != gateErr || *gates != 1 || *ups != 0 {
				t.Errorf("EnsureInspector = %v（门禁 %d 次、探测 9229 %d 次），期望原样返回门禁的错误、不探测", err, *gates, *ups)
			}
		})
	}
	t.Run("门禁通过、9229 已可用", func(t *testing.T) {
		gates, ups := stubGate(t, nil, true)
		if err := EnsureInspector(noSodaPID); err != nil || *gates != 1 || *ups != 1 {
			t.Errorf("EnsureInspector = %v（门禁 %d 次、探测 %d 次），期望 nil、各 1 次", err, *gates, *ups)
		}
	})
}

// TestActivateInspectorGate：注入本体自己过门禁，不过即原样返回、不打开命名映射与目标进程。
//
// 防的缺陷与变异（实测变红）：去掉 activateInspector 开头的门禁（返回的是打开命名映射的错误）。
func TestActivateInspectorGate(t *testing.T) {
	gates, _ := stubGate(t, launch.ErrExeUnknown, false)
	if err := activateInspector(noSodaPID); !errors.Is(err, launch.ErrExeUnknown) || *gates != 1 {
		t.Errorf("activateInspector = %v（门禁 %d 次），期望 launch.ErrExeUnknown", err, *gates)
	}
}

// TestGatePID：读得到映像路径且不在 WindowsApps 下才放行（本测试进程自己），读不到（进程不存在）按路径未知拒绝。
//
// 防的缺陷与变异（实测变红）：读不到路径时放行。
func TestGatePID(t *testing.T) {
	if err := gatePID(os.Getpid()); err != nil {
		t.Errorf("gatePID(本测试进程) = %v，期望 nil", err)
	}
	if err := gatePID(noSodaPID); !errors.Is(err, launch.ErrExeUnknown) {
		t.Errorf("gatePID(不存在的进程) = %v，期望 launch.ErrExeUnknown", err)
	}
}
