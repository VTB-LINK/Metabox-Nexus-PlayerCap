package park

// 本文件只测 mainWindow 的句柄缓存：正结果与**负结果**都必须缓存，避免高频轮询空烧
// 全进程枚举。不测 park/unpark 的窗口操作本身（那需要真实窗口）。

import (
	"testing"
	"time"
)

// resetCache 把缓存清回初始态。测试专用——包内变量，同包 _test.go 可访问。
func resetCache() {
	cacheMu.Lock()
	cachedHwnd = 0
	missUntil = time.Time{}
	cacheMu.Unlock()
}

// TestMainWindowCachesNegativeResult 钉死负结果缓存：网易云没启动时，高频调用只应枚举一次。
//
// 缺陷背景：原实现只缓存正结果——findMainAny 返回 0 时 cachedHwnd 被写成 0，而下一轮
// `cachedHwnd != 0` 为假，于是又枚举一遍。「查无此进程」这个结果从不被缓存。而 findMainAny
// 要走 gopsutil 的 process.Processes()（枚举全进程 + 逐个 OpenProcess 取 Name），
// IsMainMinimized/IsMainForeground 各调一次即一轮两次，windowStateLoop 80ms 一轮且无任何
// 门控（注释明写「始终运行，不随订阅者启停」）——net 效果是网易云没开时全速空烧 CPU，
// 而这段时间恰恰什么都不需要做。
//
// 变异自证：去掉 mainWindow 里的 `time.Now().Before(missUntil)` 早退即红。
func TestMainWindowCachesNegativeResult(t *testing.T) {
	orig := findMainAnyFn
	defer func() { findMainAnyFn = orig; resetCache() }()

	calls := 0
	findMainAnyFn = func() uintptr { calls++; return 0 } // 模拟网易云未启动
	resetCache()

	// 模拟 windowStateLoop 的高频轮询（每轮两次：IsMainMinimized + IsMainForeground）
	for range 20 {
		if h := mainWindow(); h != 0 {
			t.Fatalf("网易云未启动时应返回 0，实得 %#x", h)
		}
	}

	if calls != 1 {
		t.Errorf("负结果应被缓存：20 次调用只该枚举 1 次，实得 %d 次（每次枚举 = 一遍全进程扫描）", calls)
	}
}

// TestMainWindowRetriesAfterTTL 反方向：负结果不能永久缓存，否则网易云启动后永远发现不了。
//
// 没有这条，「负结果缓存后再也不枚举」这种把 park 功能整个废掉的假修复能通过上面那条。
//
// **必须让 missTTL 自然过期**，不能手动改写 missUntil 来模拟——那样等于绕过 TTL 本身，
// 把 missTTL 改成 100 小时也照样绿（实测过，本测试的前身就是这个毛病）。故这里把 missTTL
// 设成极短值再真的等它过去。
func TestMainWindowRetriesAfterTTL(t *testing.T) {
	origFn, origTTL := findMainAnyFn, missTTL
	defer func() { findMainAnyFn, missTTL = origFn, origTTL; resetCache() }()

	calls := 0
	findMainAnyFn = func() uintptr { calls++; return 0 }
	missTTL = 20 * time.Millisecond
	resetCache()

	mainWindow() // 第 1 次：枚举，记下负结果
	mainWindow() // TTL 内：不该再枚举
	if calls != 1 {
		t.Fatalf("前置条件：TTL 内应只枚举 1 次，实得 %d", calls)
	}

	time.Sleep(40 * time.Millisecond) // 等 TTL 自然过期

	mainWindow()
	if calls != 2 {
		t.Errorf("负结果缓存自然过期后必须重新枚举（否则网易云启动后永远发现不了），实得枚举 %d 次", calls)
	}
}

// TestMainWindowNegativeCacheClearedOnFound 网易云启动后应能被发现，并从此走正结果缓存。
// 同样靠 TTL 自然过期，不手动改 missUntil。
func TestMainWindowNegativeCacheClearedOnFound(t *testing.T) {
	origFn, origTTL := findMainAnyFn, missTTL
	defer func() { findMainAnyFn, missTTL = origFn, origTTL; resetCache() }()

	calls := 0
	found := uintptr(0)
	findMainAnyFn = func() uintptr { calls++; return found }
	missTTL = 20 * time.Millisecond
	resetCache()

	mainWindow() // 未启动 → 负结果
	found = 0x1234
	// TTL 内仍返回 0（可接受的最长 missTTL 延迟）
	if h := mainWindow(); h != 0 {
		t.Errorf("TTL 内应仍用负缓存，实得 %#x", h)
	}
	if calls != 1 {
		t.Errorf("TTL 内不应重复枚举，实得 %d 次", calls)
	}

	time.Sleep(40 * time.Millisecond) // 等 TTL 自然过期
	if h := mainWindow(); h != 0x1234 {
		t.Errorf("TTL 过期后应枚举到窗口，实得 %#x", h)
	}
}

// TestMissTTLSane 守住 TTL 取值：太短则失去意义（80ms 轮询下仍高频枚举），
// 太长则网易云启动后迟迟发现不了。
func TestMissTTLSane(t *testing.T) {
	if missTTL < 500*time.Millisecond {
		t.Errorf("missTTL=%v 过短，压不住 80ms 轮询的枚举频率", missTTL)
	}
	if missTTL > 5*time.Second {
		t.Errorf("missTTL=%v 过长，网易云启动后要等这么久才被发现", missTTL)
	}
}
