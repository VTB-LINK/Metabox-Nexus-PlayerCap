package qqmusic

// 本文件测 QQMusicMem 进程句柄的权限与生命周期（#233）：
//   - qqProcessAccess 恰为 PROCESS_QUERY_INFORMATION | PROCESS_VM_READ，不带写/注入/复制/终止权限；
//   - 用真实受限只读句柄打开当前测试进程，包内全部活跃 Win32 读/查询/复制调用均可用，写/分配/改保护均被内核拒绝；
//   - newRecordSource 从该受限只读句柄复制出的句柄独立存活，不受 QQMusicMem.Close 影响；
//   - QQMusicMem.Close 真正关闭内核句柄、清零 hProcess、幂等且 nil 安全，关闭后 ReadAllMetadata 返回错误；
//   - QQMusicMem.CheckValid 在目标进程退出后关闭句柄并清零（实测子进程为本测试二进制自重入，不启动外部播放器）；
//   - attachQQMusic / attachQQMusicPID 在全部未命中、失败与 panic 路径下均关闭已开句柄，且不跨 PID 污染字段。
//
// 真实源码行为变异自证（在当前 worktree 临时注入并还原，均实测红）：
//   - qqProcessAccess 改回 windows.PROCESS_ALL_ACCESS → TestProcHandleReadOnlyAccessOnSelf（权限断言与负向写/分配/改保护）与门禁均红；
//   - CheckValid 改回仅 m.hProcess = 0 不调 m.Close() → TestQQMusicMemCheckValidClosesExitedHandle（原句柄仍有效）与门禁均红；
//   - Close 去掉 windows.CloseHandle → TestQQMusicMemCloseIdempotentAndSafe（原句柄仍有效）与门禁均红；
//   - Close 去掉 m.hProcess = 0 → TestQQMusicMemCloseIdempotentAndSafe（字段未清零、ReadAllMetadata 未报错、二次 Close 误关复用句柄）与门禁均红；
//   - attachQQMusicPID 去掉 defer 关闭 → TestAttachQQMusicHandleLifecycle（多 PID 未命中/跨 PID/panic/真实本进程 4 个子用例）红；
//   - ReadAllMetadata 去掉 m.hProcess == 0 守卫 → TestQQMusicMemCloseIdempotentAndSafe 红。

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// 探测目标放包级：地址不随栈增长而变，供真实 ReadProcessMemory / VirtualQueryEx 与负向写测试使用。
var (
	procHandleProbeBytes            = [16]byte{'q', 'q', '-', 'r', 'o', '-', 'p', 'r', 'o', 'b', 'e', '!', 1, 2, 3, 4}
	procHandleProbeUint32    uint32 = 0x13572468
	procHandleProbePlayState uint32 = 3
	procHandleWriteTarget           = [4]byte{0x11, 0x22, 0x33, 0x44}

	procVirtualFreeExTest = modkernel32.NewProc("VirtualFreeEx")
)

const memReleaseTest = 0x8000

// TestProcHandleReadOnlyAccessOnSelf 验证只读权限常量与真实内核句柄的正/负向行为：
// 所有活跃读取与查询 API 均可用，且写内存、分配可执行页与修改页保护均被内核拒绝；
// 同时验证经该受限只读句柄复制出的 recordSource 独立于原句柄存活。
//
// 变异自证（实测）：mem.go 的 qqProcessAccess 改回 windows.PROCESS_ALL_ACCESS，
// 本例的权限常量断言与负向反例（WriteBytes / VirtualAllocEx / VirtualProtectEx 全部意外成功）均红，
// TestQQMusicProcHandleReadOnlyAndClosed 同红。
func TestProcHandleReadOnlyAccessOnSelf(t *testing.T) {
	const wantAccess uint32 = windows.PROCESS_QUERY_INFORMATION | windows.PROCESS_VM_READ
	if qqProcessAccess != wantAccess {
		t.Errorf("qqProcessAccess = %#x，期望 %#x", qqProcessAccess, wantAccess)
	}
	if qqProcessAccess != 0x0410 {
		t.Errorf("qqProcessAccess = %#x，期望 0x410", qqProcessAccess)
	}
	if qqProcessAccess == windows.PROCESS_ALL_ACCESS {
		t.Errorf("qqProcessAccess 不得为 PROCESS_ALL_ACCESS (%#x)", uint32(windows.PROCESS_ALL_ACCESS))
	}
	for _, tc := range []struct {
		name string
		bit  uint32
	}{
		{"PROCESS_VM_WRITE", windows.PROCESS_VM_WRITE},
		{"PROCESS_VM_OPERATION", windows.PROCESS_VM_OPERATION},
		{"PROCESS_DUP_HANDLE", windows.PROCESS_DUP_HANDLE},
		{"PROCESS_TERMINATE", windows.PROCESS_TERMINATE},
	} {
		if qqProcessAccess&tc.bit != 0 {
			t.Errorf("qqProcessAccess (%#x) 不得包含 %s (%#x)", qqProcessAccess, tc.name, tc.bit)
		}
	}

	pid := uint32(os.Getpid())
	h, err := windows.OpenProcess(qqProcessAccess, false, pid)
	if err != nil {
		t.Fatalf("OpenProcess(qqProcessAccess, false, self=%d) 失败: %v", pid, err)
	}
	m := &QQMusicMem{
		pid:            pid,
		hProcess:       uintptr(h),
		qqmusicDllBase: 0x6000_0000,
		version:        "22.82",
		offsets:        knownVersions["22.82"],
	}
	defer m.Close()

	// 1. EnumProcessModulesEx + GetModuleBaseNameW + GetModuleInformation
	_, _, _, k32Base, found := probeQQMusicModules(h)
	if found {
		t.Errorf("当前测试进程不应加载 QQMusic.dll")
	}
	if k32Base == 0 {
		t.Fatalf("probeQQMusicModules 未找到 kernel32.dll")
	}
	var minfo MODULEINFO
	retMod, _, errMod := procGetModuleInformation.Call(uintptr(h), k32Base, uintptr(unsafe.Pointer(&minfo)), uintptr(unsafe.Sizeof(minfo)))
	if retMod == 0 || minfo.SizeOfImage == 0 {
		t.Fatalf("GetModuleInformation(kernel32.dll) 失败: ret=%d size=%d err=%v", retMod, minfo.SizeOfImage, errMod)
	}

	// 2. QueryFullProcessImageNameW
	wantExe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable 失败: %v", err)
	}
	procQuery := modkernel32.NewProc("QueryFullProcessImageNameW")
	buf := make([]uint16, 512)
	size := uint32(len(buf))
	retQ, _, errQ := procQuery.Call(m.hProcess, 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)))
	if retQ == 0 || size == 0 {
		t.Fatalf("QueryFullProcessImageNameW 失败: ret=%d size=%d err=%v", retQ, size, errQ)
	}
	gotExe := syscall.UTF16ToString(buf[:size])
	if !strings.EqualFold(gotExe, wantExe) {
		t.Errorf("QueryFullProcessImageNameW = %q，期望与 %q 一致", gotExe, wantExe)
	}

	// 3. GetExitCodeProcess（经 CheckValid）
	if !m.CheckValid() {
		t.Fatalf("CheckValid() 对存活的本进程返回 false")
	}

	// 4. ReadProcessMemory：ReadBytes / ReadUint32 / readPlayState
	probeAddr := uintptr(unsafe.Pointer(&procHandleProbeBytes[0]))
	gotBytes := m.ReadBytes(probeAddr, uint32(len(procHandleProbeBytes)))
	if !bytes.Equal(gotBytes, procHandleProbeBytes[:]) {
		t.Errorf("ReadBytes = %x，期望 %x", gotBytes, procHandleProbeBytes[:])
	}
	if gotU32 := m.ReadUint32(uintptr(unsafe.Pointer(&procHandleProbeUint32))); gotU32 != procHandleProbeUint32 {
		t.Errorf("ReadUint32 = %#x，期望 %#x", gotU32, procHandleProbeUint32)
	}
	stateAddr := uintptr(unsafe.Pointer(&procHandleProbePlayState))
	mState := &QQMusicMem{
		hProcess:       m.hProcess,
		qqmusicDllBase: stateAddr - 0x10,
		offsets:        dllOffsets{PlayState: playStateSpec{Off: 0x10}},
	}
	if gotState, ok := mState.readPlayState(); !ok || gotState != procHandleProbePlayState {
		t.Errorf("readPlayState() = (%d, %v)，期望 (%d, true)", gotState, ok, procHandleProbePlayState)
	}

	// 5. VirtualQueryEx
	var mbi MEMORY_BASIC_INFORMATION
	retVQ, _, errVQ := procVirtualQueryEx.Call(m.hProcess, probeAddr, uintptr(unsafe.Pointer(&mbi)), uintptr(unsafe.Sizeof(mbi)))
	if retVQ == 0 || mbi.State != MEM_COMMIT {
		t.Fatalf("VirtualQueryEx 失败: ret=%d state=%#x err=%v", retVQ, mbi.State, errVQ)
	}

	// 6. DuplicateHandle（经 newRecordSource，使用真实的受限只读句柄而非 CurrentProcess 伪句柄）
	rec := newRecordSource(m)
	if rec == nil {
		t.Fatalf("newRecordSource(m) 使用受限只读句柄返回 nil")
	}
	recClosed := false
	defer func() {
		if !recClosed {
			rec.close()
		}
	}()
	recBuf := make([]byte, len(procHandleProbeBytes))
	if n, rerr := rec.mem.read(probeAddr, recBuf); rerr != nil || n != len(procHandleProbeBytes) || !bytes.Equal(recBuf, procHandleProbeBytes[:]) {
		t.Fatalf("rec.mem.read 失败: n=%d got=%x err=%v", n, recBuf, rerr)
	}
	if reg, qerr := rec.mem.query(probeAddr); qerr != nil || reg.state != windows.MEM_COMMIT {
		t.Fatalf("rec.mem.query 失败: reg=%+v err=%v", reg, qerr)
	}

	// 7. 负向权限反例：同一只读句柄不得写内存、分配内存或修改页保护
	writeAddr := uintptr(unsafe.Pointer(&procHandleWriteTarget[0]))
	origTarget := procHandleWriteTarget
	if m.WriteBytes(writeAddr, []byte{0xAA, 0xBB}) {
		procHandleWriteTarget = origTarget
		t.Errorf("只读句柄的 WriteBytes 意外成功")
	}
	if procHandleWriteTarget != origTarget {
		procHandleWriteTarget = origTarget
		t.Errorf("只读句柄改写了目标字节")
	}

	cave, _, errAlloc := procVirtualAllocEx.Call(m.hProcess, 0, 0x1000, MEM_COMMIT|MEM_RESERVE, PAGE_READWRITE)
	if cave != 0 {
		procVirtualFreeExTest.Call(m.hProcess, cave, 0, memReleaseTest)
		t.Errorf("只读句柄的 VirtualAllocEx 意外成功: addr=%#x", cave)
	} else if !errors.Is(errAlloc, windows.ERROR_ACCESS_DENIED) {
		t.Errorf("VirtualAllocEx 错误 = %v，期望 ERROR_ACCESS_DENIED", errAlloc)
	}

	var oldProt uint32
	retProt, _, errProt := procVirtualProtectEx.Call(m.hProcess, writeAddr, 4, PAGE_READWRITE, uintptr(unsafe.Pointer(&oldProt)))
	if retProt != 0 {
		procVirtualProtectEx.Call(m.hProcess, writeAddr, 4, uintptr(oldProt), uintptr(unsafe.Pointer(&oldProt)))
		t.Errorf("只读句柄的 VirtualProtectEx 意外成功")
	} else if !errors.Is(errProt, windows.ERROR_ACCESS_DENIED) {
		t.Errorf("VirtualProtectEx 错误 = %v，期望 ERROR_ACCESS_DENIED", errProt)
	}

	// 8. 生命周期独立性：关闭 m 后，rec 复制出的句柄仍有效；关闭 rec 后才报 ERROR_INVALID_HANDLE
	m.Close()
	clear(recBuf)
	if n, rerr := rec.mem.read(probeAddr, recBuf); rerr != nil || n != len(procHandleProbeBytes) || !bytes.Equal(recBuf, procHandleProbeBytes[:]) {
		t.Fatalf("m.Close() 之后 rec.mem.read 应仍成功: n=%d got=%x err=%v", n, recBuf, rerr)
	}
	rec.close()
	recClosed = true
	if _, rerr := rec.mem.read(probeAddr, recBuf); !errors.Is(rerr, windows.ERROR_INVALID_HANDLE) {
		t.Fatalf("rec.close() 之后再读: err=%v，期望 ERROR_INVALID_HANDLE", rerr)
	}
}

// TestQQMusicMemCloseIdempotentAndSafe 验证 Close 真正关闭内核句柄、清零字段、阻断后续元数据读取，
// 且重复调用不会误关系统复用的新句柄；对 nil 与零值实例安全。
func TestQQMusicMemCloseIdempotentAndSafe(t *testing.T) {
	pid := uint32(os.Getpid())
	h, err := windows.OpenProcess(qqProcessAccess, false, pid)
	if err != nil {
		t.Fatalf("OpenProcess 失败: %v", err)
	}
	m := &QQMusicMem{pid: pid, hProcess: uintptr(h), qqmusicDllBase: 0x1000}

	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil || code != 259 {
		t.Fatalf("Close 前 GetExitCodeProcess = (%d, %v)，期望 (259, nil)", code, err)
	}

	m.Close()
	if m.hProcess != 0 {
		t.Errorf("Close 后 m.hProcess = %#x，期望 0", m.hProcess)
	}
	if err := windows.GetExitCodeProcess(h, &code); !errors.Is(err, windows.ERROR_INVALID_HANDLE) {
		t.Errorf("Close 后对原句柄 GetExitCodeProcess = %v，期望 ERROR_INVALID_HANDLE", err)
	}
	if _, err := m.ReadAllMetadata(); err == nil {
		t.Errorf("Close 后 ReadAllMetadata() 返回 nil 错误，期望报错")
	}

	// 幂等性反例：关闭后立即新开句柄，重复 Close 不得误关系统可能复用的句柄值。
	for i := 0; i < 8; i++ {
		h1, err := windows.OpenProcess(qqProcessAccess, false, pid)
		if err != nil {
			t.Fatalf("第 %d 轮 OpenProcess(h1) 失败: %v", i, err)
		}
		m1 := &QQMusicMem{pid: pid, hProcess: uintptr(h1)}
		m1.Close()

		h2, err := windows.OpenProcess(qqProcessAccess, false, pid)
		if err != nil {
			t.Fatalf("第 %d 轮 OpenProcess(h2) 失败: %v", i, err)
		}
		m1.Close()

		var c2 uint32
		err2 := windows.GetExitCodeProcess(h2, &c2)
		_ = windows.CloseHandle(h2)
		if err2 != nil || c2 != 259 {
			t.Fatalf("第 %d 轮二次 Close 后新句柄失效: code=%d err=%v（可能未清零 hProcess 而误关复用句柄）", i, c2, err2)
		}
	}

	// nil 与零值接收者安全
	(*QQMusicMem)(nil).Close()
	(&QQMusicMem{}).Close()
}

// TestProcHandleExitHelper 为 TestQQMusicMemCheckValidClosesExitedHandle 提供短生命周期自重入子进程，
// 仅在环境变量 QQMUSIC_HANDLE_TEST_HELPER=1 时阻塞等待 stdin 关闭，避免触碰任何真实播放器进程。
func TestProcHandleExitHelper(t *testing.T) {
	if os.Getenv("QQMUSIC_HANDLE_TEST_HELPER") != "1" {
		return
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
}

// TestQQMusicMemCheckValidClosesExitedHandle 验证目标进程退出后 CheckValid 真正关闭内核句柄并清零 hProcess，
// 以及 nil / 零值 / 已失效句柄的边界行为。
func TestQQMusicMemCheckValidClosesExitedHandle(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestProcHandleExitHelper$")
	cmd.Env = append(os.Environ(), "QQMUSIC_HANDLE_TEST_HELPER=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe 失败: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动辅助子进程失败: %v", err)
	}
	waited := false
	t.Cleanup(func() {
		_ = stdin.Close()
		if !waited {
			_ = cmd.Wait()
		}
	})

	childPID := uint32(cmd.Process.Pid)
	h, err := windows.OpenProcess(qqProcessAccess, false, childPID)
	if err != nil {
		t.Fatalf("OpenProcess(child=%d) 失败: %v", childPID, err)
	}
	m := &QQMusicMem{pid: childPID, hProcess: uintptr(h)}
	t.Cleanup(func() { m.Close() })

	if !m.CheckValid() || m.hProcess != uintptr(h) {
		t.Fatalf("子进程存活期间 CheckValid 失败或句柄被改动: hProcess=%#x want=%#x", m.hProcess, uintptr(h))
	}

	if err := stdin.Close(); err != nil {
		t.Fatalf("关闭子进程 stdin 失败: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("等待辅助子进程退出失败: %v", err)
	}
	waited = true

	if m.CheckValid() {
		t.Fatalf("子进程退出后 CheckValid() 仍返回 true")
	}
	if m.hProcess != 0 {
		t.Errorf("子进程退出后 m.hProcess = %#x，期望 0", m.hProcess)
	}
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); !errors.Is(err, windows.ERROR_INVALID_HANDLE) {
		t.Errorf("CheckValid 返回 false 后原句柄仍有效: code=%d err=%v，期望 ERROR_INVALID_HANDLE", code, err)
	}

	// 再次检查与关闭均安全
	if m.CheckValid() {
		t.Errorf("二次 CheckValid() 期望 false")
	}
	m.Close()

	// nil / 零值 / 已失效句柄边界
	if (*QQMusicMem)(nil).CheckValid() {
		t.Errorf("nil.CheckValid() 期望 false")
	}
	if (&QQMusicMem{}).CheckValid() {
		t.Errorf("零值 CheckValid() 期望 false")
	}
	// Windows 内核句柄均按 4 字节对齐且伪句柄为负值，奇数 0x7FFFFFFD 恒为无效句柄，
	// 避免关闭真实句柄后复用其数值导致误伤并发打开的句柄。
	mInvalid := &QQMusicMem{hProcess: uintptr(0x7FFF_FFFD)}
	if mInvalid.CheckValid() {
		t.Errorf("已失效句柄 CheckValid() 期望 false")
	}
	if mInvalid.hProcess != 0 {
		t.Errorf("已失效句柄 CheckValid() 后 m.hProcess = %#x，期望 0", mInvalid.hProcess)
	}
}

// TestAttachQQMusicHandleLifecycle 验证 attachQQMusic / attachQQMusicPID 在全部探测路径下的句柄生命周期：
// 打开失败不关 0、未命中全部关闭、跨 PID 不污染字段、detectVersion panic 时关闭、真实本进程未命中后句柄失效。
func TestAttachQQMusicHandleLifecycle(t *testing.T) {
	t.Run("打开失败跳过且不关0句柄", func(t *testing.T) {
		var closed []windows.Handle
		ops := attachOps{
			openProcess: func(pid uint32) (windows.Handle, error) {
				if pid == 101 {
					return 0, windows.ERROR_ACCESS_DENIED
				}
				return 0, nil
			},
			closeHandle: func(h windows.Handle) error {
				closed = append(closed, h)
				return nil
			},
			probeModules: func(h windows.Handle) (uintptr, uintptr, uint32, uintptr, bool) {
				t.Errorf("打开失败不应进入 probeModules (h=%#x)", h)
				return 0, 0, 0, 0, false
			},
			detectVersion: func(m *QQMusicMem) {
				t.Errorf("打开失败不应进入 detectVersion")
			},
		}
		mem, err := attachQQMusic([]uint32{101, 102}, ops)
		if err == nil || mem != nil {
			t.Fatalf("期望返回错误与 nil，实得 mem=%v err=%v", mem, err)
		}
		if len(closed) != 0 {
			t.Errorf("打开失败不应调用 closeHandle，实得 %v", closed)
		}
	})

	t.Run("多PID均未命中时每个句柄恰关闭一次", func(t *testing.T) {
		handles := map[uint32]windows.Handle{201: 0x1001, 202: 0x1002, 203: 0x1003}
		closeCount := map[windows.Handle]int{}
		ops := attachOps{
			openProcess: func(pid uint32) (windows.Handle, error) {
				return handles[pid], nil
			},
			closeHandle: func(h windows.Handle) error {
				closeCount[h]++
				return nil
			},
			probeModules: func(h windows.Handle) (uintptr, uintptr, uint32, uintptr, bool) {
				if h == 0x1001 {
					// 模拟 EnumProcessModulesEx 失败
					return 0, 0, 0, 0, false
				}
				// 模拟模块表中无 QQMusic.dll
				return 0, 0, 0, 0x7600_0000, false
			},
			detectVersion: func(m *QQMusicMem) {
				t.Errorf("未命中不应进入 detectVersion")
			},
		}
		mem, err := attachQQMusic([]uint32{201, 202, 203}, ops)
		if err == nil || mem != nil {
			t.Fatalf("期望返回错误与 nil，实得 mem=%v err=%v", mem, err)
		}
		for pid, h := range handles {
			if closeCount[h] != 1 {
				t.Errorf("PID %d 的句柄 %#x 关闭次数 = %d，期望 1", pid, h, closeCount[h])
			}
		}
	})

	t.Run("未命中PID字段不污染后续命中PID", func(t *testing.T) {
		closeCount := map[windows.Handle]int{}
		detectCalled := 0
		ops := attachOps{
			openProcess: func(pid uint32) (windows.Handle, error) {
				if pid == 301 {
					return 0x2001, nil
				}
				return 0x2002, nil
			},
			closeHandle: func(h windows.Handle) error {
				closeCount[h]++
				return nil
			},
			probeModules: func(h windows.Handle) (uintptr, uintptr, uint32, uintptr, bool) {
				if h == 0x2001 {
					return 0, 0xAAAA_0000, 0x5000, 0xBBBB_0000, false
				}
				return 0xCCCC_0000, 0, 0, 0, true
			},
			detectVersion: func(m *QQMusicMem) {
				detectCalled++
				m.version = "22.82"
			},
		}
		mem, err := attachQQMusic([]uint32{301, 302}, ops)
		if err != nil || mem == nil {
			t.Fatalf("期望命中 PID 302，实得 mem=%v err=%v", mem, err)
		}
		if closeCount[0x2001] != 1 {
			t.Errorf("未命中句柄 0x2001 关闭次数 = %d，期望 1", closeCount[0x2001])
		}
		if closeCount[0x2002] != 0 {
			t.Errorf("命中句柄 0x2002 不应被关闭，实得关闭次数 %d", closeCount[0x2002])
		}
		if detectCalled != 1 || mem.pid != 302 || mem.hProcess != 0x2002 || mem.qqmusicDllBase != 0xCCCC_0000 {
			t.Errorf("返回实例字段不符: %+v (detectCalled=%d)", mem, detectCalled)
		}
		if mem.gfWrapperBase != 0 || mem.gfWrapperSize != 0 || mem.kernel32Base != 0 {
			t.Errorf("未命中 PID 字段污染了命中实例: gfBase=%#x gfSize=%#x k32Base=%#x",
				mem.gfWrapperBase, mem.gfWrapperSize, mem.kernel32Base)
		}
	})

	t.Run("detectVersion发生panic时仍关闭已开句柄", func(t *testing.T) {
		closeCount := map[windows.Handle]int{}
		ops := attachOps{
			openProcess: func(pid uint32) (windows.Handle, error) {
				return 0x3001, nil
			},
			closeHandle: func(h windows.Handle) error {
				closeCount[h]++
				return nil
			},
			probeModules: func(h windows.Handle) (uintptr, uintptr, uint32, uintptr, bool) {
				return 0xDDDD_0000, 0, 0, 0, true
			},
			detectVersion: func(m *QQMusicMem) {
				panic("detectVersion boom")
			},
		}
		var recovered any
		func() {
			defer func() { recovered = recover() }()
			_, _ = attachQQMusic([]uint32{401}, ops)
		}()
		if recovered != "detectVersion boom" {
			t.Fatalf("期望捕获 detectVersion panic，实得 %v", recovered)
		}
		if closeCount[0x3001] != 1 {
			t.Errorf("detectVersion panic 后句柄 0x3001 关闭次数 = %d，期望 1", closeCount[0x3001])
		}
	})

	t.Run("真实本进程未命中后内核句柄已关闭", func(t *testing.T) {
		var opened []windows.Handle
		ops := defaultAttachOps
		ops.openProcess = func(pid uint32) (windows.Handle, error) {
			h, err := defaultAttachOps.openProcess(pid)
			if err == nil && h != 0 {
				opened = append(opened, h)
			}
			return h, err
		}
		mem, err := attachQQMusic([]uint32{uint32(os.Getpid())}, ops)
		if err == nil || mem != nil {
			if mem != nil {
				mem.Close()
			}
			t.Fatalf("本进程无 QQMusic.dll，期望返回错误与 nil，实得 mem=%v err=%v", mem, err)
		}
		if len(opened) != 1 {
			t.Fatalf("期望打开 1 个真实句柄，实得 %d 个", len(opened))
		}
		var code uint32
		if err := windows.GetExitCodeProcess(opened[0], &code); !errors.Is(err, windows.ERROR_INVALID_HANDLE) {
			_ = windows.CloseHandle(opened[0])
			t.Errorf("attachQQMusic 返回错误后真实句柄仍有效: code=%d err=%v，期望 ERROR_INVALID_HANDLE", code, err)
		}
	})
}
