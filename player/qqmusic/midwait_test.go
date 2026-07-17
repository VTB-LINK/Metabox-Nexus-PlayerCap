package qqmusic

// 本文件只测 midWaitGate：v20.05 换歌时等 songMid 就绪的**有界**等待窗。
//
// 两个方向都要钉住，缺一个就退化成一个 bug 换另一个 bug：
//   - 等窗内必须继续等 → 否则 songMid 只是慢一拍就被当成没有，退回「上一首歌词滚在新歌上」；
//   - 等窗尽了必须停 → 否则 songMid 持久为空时（本地文件播放，两条提取路径都不成立）
//     这首歌一个事件都不发，overlay 整首停在上一首——同一个症状换了个触发源。

import (
	"testing"
	"time"
)

func TestMidWaitGateWaitsThenGivesUp(t *testing.T) {
	var g midWaitGate
	t0 := time.Now()
	const window = 500 * time.Millisecond

	// 第一次见到这首歌：开窗，且应继续等
	started, keepWaiting := g.wait("歌A", t0, window)
	if !started {
		t.Error("第一次等某首歌应报 started（调用方据此清掉上一首歌词，停止错滚）")
	}
	if !keepWaiting {
		t.Error("刚开窗就该继续等")
	}

	// 窗内再问：不是新开始，但仍该等
	started, keepWaiting = g.wait("歌A", t0.Add(100*time.Millisecond), window)
	if started {
		t.Error("同一首歌的后续轮询不应再报 started（否则会反复清歌词）")
	}
	if !keepWaiting {
		t.Error("等窗内（100ms < 500ms）应继续等")
	}

	// 窗尽：必须停止等待，让调用方认账并发标题
	if _, keepWaiting = g.wait("歌A", t0.Add(600*time.Millisecond), window); keepWaiting {
		t.Error("等窗用尽后必须停止等待——songMid 可能持久为空（本地文件），无界等会让整首歌不发任何事件")
	}
}

func TestMidWaitGateRestartsForNewSong(t *testing.T) {
	var g midWaitGate
	t0 := time.Now()
	const window = 500 * time.Millisecond

	g.wait("歌A", t0, window)
	// 歌 A 的窗已尽
	if _, keep := g.wait("歌A", t0.Add(600*time.Millisecond), window); keep {
		t.Fatal("前置条件：歌A 的窗应已尽")
	}

	// 换到歌 B：必须重新开窗，不能继承歌 A 已经过期的 deadline
	started, keepWaiting := g.wait("歌B", t0.Add(600*time.Millisecond), window)
	if !started {
		t.Error("换新歌应重新开窗并报 started")
	}
	if !keepWaiting {
		t.Error("新歌刚开窗就该继续等——继承了上一首的过期 deadline 会让新歌一次都不等")
	}
}

func TestMidWaitGateResetReopensWindow(t *testing.T) {
	var g midWaitGate
	t0 := time.Now()
	const window = 500 * time.Millisecond

	g.wait("歌A", t0, window)
	g.reset() // songMid 就绪（或已放弃），认领了这首歌

	// 同名歌再次进入（例如单曲循环重播）应重新开窗
	if started, _ := g.wait("歌A", t0.Add(10*time.Second), window); !started {
		t.Error("reset 后同一首歌再次等待应重新开窗")
	}
}
