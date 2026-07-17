package server

// 本文件只测 evaluateGroup 的选址确定性：谁该拿到根 overlay。
// 不测 switchTo/holding 倒计时/超时释放，也不测 resolvePlayer（resolveplayer_test.go）。

import (
	"testing"
	"time"
)

// TestEvaluateGroupIsDeterministic 钉死本条核心：**同一份状态必须永远给出同一个 target**。
//
// 缺陷背景：evaluateGroup 用 `for name, ps := range states` 收集 activeNames，两个调用方
// 都取 activeNames[0]。lastPlaying 仍 active 时它会被覆盖，但 lastPlaying **失配**时
// （最后播放的那个进程退出了，另外两个还在放）activeNames[0] 就是最终答案 —— 而 map
// 迭代序是随机的。放大器：进程不在的播放器每 ~2s 重发 waiting_process，每条 status_update
// 都触发重新评估 → 硬币每 2s 重掷 → 结果一变就 switchTo → 全量重放 → OBS 根 overlay 硬切。
// 实测重掷结果改变概率 0.37，期望硬切间隔 ~5.4s。
//
// 变异自证：去掉 evaluateGroup 里的 sort 即红（map 序随机，50 次里必然出现不同答案）。
func TestEvaluateGroupIsDeterministic(t *testing.T) {
	now := time.Now()
	// kugou 更晚开始播 → 它该赢；qqmusic 是失配的 lastPlaying（进程已退 → idle）
	build := func() map[string]*playerState {
		return map[string]*playerState{
			"cloudmusicv3": {status: "playing", activated: true, activeAt: now.Add(-2 * time.Minute)},
			"kugou":        {status: "playing", activated: true, activeAt: now.Add(-10 * time.Second)},
			"qqmusic":      {status: "idle"},
		}
	}

	first, _ := evaluateGroup(build())
	if len(first) != 2 {
		t.Fatalf("前置条件：应有 2 个活跃播放器，实得 %v", first)
	}
	for range 50 {
		got, _ := evaluateGroup(build())
		if got[0] != first[0] {
			t.Fatalf("同一份状态必须给出同一个 target —— 实得 %q 与 %q 不一致（map 序随机 = OBS 每 ~5s 硬切一次）",
				got[0], first[0])
		}
	}
}

// TestEvaluateGroupPrefersMostRecent 钉死判据：最近**开始播放**的排第一。
//
// 判据是 activeAt 而不是「配置顺序」——这是作者本来就声明的语义（796d69c 的 body：
// 「切到 priorLastPlaying（焦点竞争，最后播放者优先）」，README 与 config 模板也都写
// 「切换到最后一个普通播放器」）。lastPlaying 是这个判据的标量版，activeAt 是它的全序推广。
//
// 变异自证：把排序方向反过来（升序）即红。
func TestEvaluateGroupPrefersMostRecent(t *testing.T) {
	now := time.Now()
	states := map[string]*playerState{
		"cloudmusicv3": {status: "playing", activated: true, activeAt: now.Add(-2 * time.Minute)},
		"kugou":        {status: "playing", activated: true, activeAt: now.Add(-10 * time.Second)},
	}
	got, _ := evaluateGroup(states)
	if got[0] != "kugou" {
		t.Errorf("最近开始播放的应排第一（kugou -10s vs cloudmusicv3 -2min），实得 %q", got[0])
	}

	// 反过来：让 cloudmusicv3 更近 —— 防「永远返回 kugou」这种把测试蒙过去的写法
	states["cloudmusicv3"].activeAt = now
	got, _ = evaluateGroup(states)
	if got[0] != "cloudmusicv3" {
		t.Errorf("activeAt 更近的应排第一，实得 %q", got[0])
	}
}

// TestEvaluateGroupCollectsAllActive 排序不得丢成员。
//
// 这条守的是一个比原缺陷更严重的假修复：把遍历改成「按预设 order 遍历」。order 里漏掉的
// 名字会整个从 activeNames 消失 —— 漏掉唯一活跃者即 len==0 → clearActivePlayer → OBS 空屏。
// 排序只重排已收集到的名字，不可能丢。
func TestEvaluateGroupCollectsAllActive(t *testing.T) {
	now := time.Now()
	states := map[string]*playerState{
		"cloudmusicv3": {status: "playing", activated: true, activeAt: now},
		"kugou":        {status: "loading", activated: true, activeAt: now.Add(-time.Second)},
		"qqmusic":      {status: "paused", activated: true},
		"newplayer":    {status: "playing", activated: true, activeAt: now.Add(-5 * time.Second)},
	}
	got, hasHolding := evaluateGroup(states)

	if len(got) != 3 {
		t.Errorf("playing/loading 都算活跃，应收集 3 个（含未在任何预设顺序表里的 newplayer），实得 %v", got)
	}
	seen := map[string]bool{}
	for _, n := range got {
		seen[n] = true
	}
	for _, want := range []string{"cloudmusicv3", "kugou", "newplayer"} {
		if !seen[want] {
			t.Errorf("%q 活跃却没被收集 —— 漏掉唯一活跃者会导致 OBS 空屏", want)
		}
	}
	if !hasHolding {
		t.Error("paused+activated 的播放器应记为 holding（排序不得影响 hasHolding）")
	}
}

// TestEvaluateGroupTieBreakIsStable 同刻进入活跃（几乎只出现在测试里）也必须确定。
func TestEvaluateGroupTieBreakIsStable(t *testing.T) {
	now := time.Now()
	build := func() map[string]*playerState {
		return map[string]*playerState{
			"kugou":        {status: "playing", activated: true, activeAt: now},
			"cloudmusicv3": {status: "playing", activated: true, activeAt: now},
			"qqmusic":      {status: "playing", activated: true, activeAt: now},
		}
	}
	first, _ := evaluateGroup(build())
	for range 50 {
		got, _ := evaluateGroup(build())
		if got[0] != first[0] {
			t.Fatalf("activeAt 相同时也必须确定，实得 %q 与 %q 不一致", got[0], first[0])
		}
	}
}

// TestActiveAtOnlyOnEnteringActive activeAt 只在**进入**活跃态时打，loading→playing
// 是同一轮活跃内的转换，不得重打。
//
// 否则它退化成「谁最后发的事件」而不是「谁最近开始播放」：一个从头播到尾的播放器会因为
// 中途的状态事件不断刷新 activeAt，抢走真正后开始播的那个。
//
// 变异自证：把 activeAt 的赋值移到 `if !wasActive` 之外即红。
func TestActiveAtOnlyOnEnteringActive(t *testing.T) {
	r := &Router{}
	ps := &playerState{status: "idle"}
	var last string

	r.updatePlayerState(ps, "loading", "kugou", "普通", &last)
	enteredAt := ps.activeAt
	if enteredAt.IsZero() {
		t.Fatal("idle→loading 应打 activeAt")
	}

	time.Sleep(2 * time.Millisecond)
	r.updatePlayerState(ps, "playing", "kugou", "普通", &last)
	if !ps.activeAt.Equal(enteredAt) {
		t.Errorf("loading→playing 属同一轮活跃，不得重打 activeAt（否则退化成「谁最后发事件」）\n  进入时 %v\n  现在   %v",
			enteredAt, ps.activeAt)
	}

	// 但真正离开再回来必须重打
	time.Sleep(2 * time.Millisecond)
	r.updatePlayerState(ps, "idle", "kugou", "普通", &last)
	time.Sleep(2 * time.Millisecond)
	r.updatePlayerState(ps, "playing", "kugou", "普通", &last)
	if !ps.activeAt.After(enteredAt) {
		t.Error("idle 之后重新 playing 是新一轮活跃，必须重打 activeAt")
	}
}
