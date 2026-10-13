//go:build windows

package session

// 真机测试：量一拍探测的耗时与 CPU（设计 §4.4）。默认跳过，PLAYERCAP_LIVE=1 才跑：它对本机真实的音频服务做
// 会话枚举（只读，不开音频流、不注册回调），结果随本机的端点与会话数而变，不能进常规的 go test ./...。
//
//	$env:PLAYERCAP_LIVE='1'; go test ./audiocap/session/ -run TestLiveTickCost -v -count=1

import (
	"os"
	"runtime"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"Metabox-Nexus-PlayerCap/audiocap/procscan"
)

// processCPU 返回本测试进程自创建以来的内核态与用户态 CPU 时间之和。测试期间进程里只有本 goroutine 在做事，
// 故可当作探测本身的 CPU。
func processCPU(t *testing.T) time.Duration {
	t.Helper()
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(windows.CurrentProcess(), &creation, &exit, &kernel, &user); err != nil {
		t.Fatalf("GetProcessTimes: %v", err)
	}
	ft := func(f windows.Filetime) time.Duration {
		return time.Duration(uint64(f.HighDateTime)<<32|uint64(f.LowDateTime)) * 100
	}
	return ft(kernel) + ft(user)
}

func TestLiveTickCost(t *testing.T) {
	if os.Getenv("PLAYERCAP_LIVE") != "1" {
		t.Skip("真机测试，设 PLAYERCAP_LIVE=1 才跑")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := windows.CoInitializeEx(0, windows.COINIT_MULTITHREADED); err != nil {
		t.Fatalf("CoInitializeEx: %v", err)
	}
	defer windows.CoUninitialize()

	e := NewCOMEnumerator()
	defer e.Close()
	if _, err := e.enumerate(); err != nil { // 首次：建立端点与会话管理器缓存，不计入
		t.Fatalf("首次枚举: %v", err)
	}

	const ticks = 20
	var enumWall, snapWall time.Duration
	var endpoints, sessions, failures int
	cpu0 := processCPU(t)
	for i := 0; i < ticks; i++ {
		t0 := time.Now()
		eps, err := e.enumerate()
		enumWall += time.Since(t0)
		if err != nil {
			t.Fatalf("枚举: %v", err)
		}
		endpoints, sessions = len(eps), 0
		for _, ep := range eps {
			sessions += len(ep.sessions)
			if ep.err != nil {
				failures++
			}
		}
		t1 := time.Now()
		if _, err := procscan.Snapshot(); err != nil {
			t.Fatalf("进程快照: %v", err)
		}
		snapWall += time.Since(t1)
		time.Sleep(100 * time.Millisecond)
	}
	cpu := processCPU(t) - cpu0
	t.Logf("端点 %d 个、会话 %d 个、端点枚举失败 %d 次；每拍平均（缓存命中）：会话枚举 %v、进程快照 %v、进程 CPU %v（含两者，不含睡眠）",
		endpoints, sessions, failures, enumWall/ticks, snapWall/ticks, cpu/ticks)

	// 端点缓存重建（每 endpointRefresh 一次）的代价：每拍强制重建。
	var refreshWall time.Duration
	cpu1 := processCPU(t)
	for i := 0; i < ticks; i++ {
		e.listedAt = time.Time{}
		t0 := time.Now()
		if _, err := e.enumerate(); err != nil {
			t.Fatalf("重建后枚举: %v", err)
		}
		refreshWall += time.Since(t0)
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("端点缓存每次重建（含当拍枚举）：墙钟 %v、进程 CPU %v", refreshWall/ticks, (processCPU(t)-cpu1)/ticks)
}
