package server

import "testing"

// TestHasSubscribersRootFollowsActive：仅 root sub 时，HasSubscribers 对当前 active 家返回 true、
// 对非 active 家返回 false（「只连 root 也要抓 active」的关键判据）。
func TestHasSubscribersRootFollowsActive(t *testing.T) {
	h := newAudioHub()
	root := &audioSub{ch: make(chan []byte, 8), player: ""}
	h.addSub(root)
	h.onActivePlayerChanged("kugou")
	if !h.HasSubscribers("kugou") {
		t.Fatal("仅 root sub 时 active=kugou 应为 true")
	}
	if h.HasSubscribers("wesing") {
		t.Fatal("非 active 应为 false")
	}
}

// TestBroadcastDropOldestAndRouting：active=kugou；root sub(cap1) + wesing per-player sub(cap1)。
// 连发两帧 kugou → root 通道满则丢旧留新（保最新 "b"）；wesing per-player sub 不收 kugou 帧。
func TestBroadcastDropOldestAndRouting(t *testing.T) {
	h := newAudioHub()
	h.onActivePlayerChanged("kugou")
	root := &audioSub{ch: make(chan []byte, 1), player: ""}
	per := &audioSub{ch: make(chan []byte, 1), player: "wesing"}
	h.addSub(root)
	h.addSub(per)
	h.BroadcastAudioFrame("kugou", []byte("a")) // → root(active)
	h.BroadcastAudioFrame("kugou", []byte("b")) // 通道满 → 丢旧留新
	if got := <-root.ch; string(got) != "b" {
		t.Fatalf("root 应保最新 b, got %s", got)
	}
	if len(per.ch) != 0 {
		t.Fatal("wesing per-player 不该收到 kugou 帧")
	}
}

// TestBroadcastPerPlayerReceivesOwn：per-player sub 只收自己那家的帧，收不到别家的。
func TestBroadcastPerPlayerReceivesOwn(t *testing.T) {
	h := newAudioHub()
	h.onActivePlayerChanged("kugou") // active 是 kugou，验证 per-player 路由与 active 无关
	wesing := &audioSub{ch: make(chan []byte, 4), player: "wesing"}
	h.addSub(wesing)
	h.BroadcastAudioFrame("kugou", []byte("k"))  // 别家，不该收
	h.BroadcastAudioFrame("wesing", []byte("w")) // 自己家，应收
	if len(wesing.ch) != 1 {
		t.Fatalf("wesing per-player 应只收到 1 帧, got %d", len(wesing.ch))
	}
	if got := <-wesing.ch; string(got) != "w" {
		t.Fatalf("wesing 应收到自己那家 w, got %s", got)
	}
}

// TestRootFollowsActiveSwitch：active 切换后，root 跟随新 active、不再收旧 active 的帧。
func TestRootFollowsActiveSwitch(t *testing.T) {
	h := newAudioHub()
	h.onActivePlayerChanged("kugou")
	root := &audioSub{ch: make(chan []byte, 4), player: ""}
	h.addSub(root)

	h.BroadcastAudioFrame("kugou", []byte("k1"))  // active=kugou → 收
	h.onActivePlayerChanged("wesing")             // 切到 wesing
	h.BroadcastAudioFrame("kugou", []byte("k2"))  // 旧 active → 不收
	h.BroadcastAudioFrame("wesing", []byte("w1")) // 新 active → 收

	if got := <-root.ch; string(got) != "k1" {
		t.Fatalf("切换前 root 应收 kugou k1, got %s", got)
	}
	if got := <-root.ch; string(got) != "w1" {
		t.Fatalf("切换后 root 应跟随收 wesing w1（跳过旧 active kugou）, got %s", got)
	}
	if len(root.ch) != 0 {
		t.Fatal("root 不该收到切换后的旧 active(kugou) 帧")
	}

	// 切换后 HasSubscribers 也应跟随
	if !h.HasSubscribers("wesing") {
		t.Fatal("切换后 root 应把 wesing 计为有订阅")
	}
	if h.HasSubscribers("kugou") {
		t.Fatal("切换后旧 active kugou 应无订阅")
	}
}

// TestHasSubscribersPerPlayerRegardlessOfActive：per-player sub 无论 active 是谁都算有订阅。
func TestHasSubscribersPerPlayerRegardlessOfActive(t *testing.T) {
	h := newAudioHub()
	h.onActivePlayerChanged("kugou")
	h.addSub(&audioSub{ch: make(chan []byte, 1), player: "wesing"})
	if !h.HasSubscribers("wesing") {
		t.Fatal("存在 wesing per-player sub 时应为 true（与 active 无关）")
	}
}

// TestAnySubscribers：登记/注销后 AnySubscribers 正确反映 subs 是否为空。
func TestAnySubscribers(t *testing.T) {
	h := newAudioHub()
	if h.AnySubscribers() {
		t.Fatal("新建 hub 应无订阅者")
	}
	s := &audioSub{ch: make(chan []byte, 1), player: ""}
	h.addSub(s)
	if !h.AnySubscribers() {
		t.Fatal("addSub 后应有订阅者")
	}
	h.removeSub(s)
	if h.AnySubscribers() {
		t.Fatal("removeSub 后应无订阅者")
	}
}

// TestBroadcastNoSubscribersNoPanic：无订阅者时 Broadcast 不阻塞、不 panic。
func TestBroadcastNoSubscribersNoPanic(t *testing.T) {
	h := newAudioHub()
	h.onActivePlayerChanged("kugou")
	h.BroadcastAudioFrame("kugou", []byte("x")) // 无订阅者，直接返回
	h.BroadcastAudioFrame("wesing", nil)        // 空 payload 也不该 panic
}

// TestHasRootSubscriber：仅 per-player sub 时为 false；有 root sub 时为 true（预热门控判据）。
func TestHasRootSubscriber(t *testing.T) {
	h := newAudioHub()
	if h.HasRootSubscriber() {
		t.Fatal("新建 hub 应无 root 订阅")
	}
	per := &audioSub{ch: make(chan []byte, 1), player: "wesing"}
	h.addSub(per)
	if h.HasRootSubscriber() {
		t.Fatal("仅 per-player sub 不应算 root 订阅")
	}
	root := &audioSub{ch: make(chan []byte, 1), player: ""}
	h.addSub(root)
	if !h.HasRootSubscriber() {
		t.Fatal("有 root sub 应为 true")
	}
	h.removeSub(root)
	if h.HasRootSubscriber() {
		t.Fatal("移除 root sub 后应为 false")
	}
}

// TestActivePlayer：ActivePlayer 如实返回 onActivePlayerChanged 写入的缓存（空闲空帧门控判据）。
func TestActivePlayer(t *testing.T) {
	h := newAudioHub()
	if h.ActivePlayer() != "" {
		t.Fatalf("新建 hub active 应为空, got %q", h.ActivePlayer())
	}
	h.onActivePlayerChanged("kugou")
	if h.ActivePlayer() != "kugou" {
		t.Fatalf("应为 kugou, got %q", h.ActivePlayer())
	}
	h.onActivePlayerChanged("")
	if h.ActivePlayer() != "" {
		t.Fatalf("清空后应为空, got %q", h.ActivePlayer())
	}
}

// TestBroadcastEmptyPlayerRoutesRootOnly：player="" 的空闲空帧只命中 root 订阅，per-player 订阅收不到
// （管理器在 active=="" 时推空帧的路由前提）。
func TestBroadcastEmptyPlayerRoutesRootOnly(t *testing.T) {
	h := newAudioHub()
	h.onActivePlayerChanged("") // 无活跃
	root := &audioSub{ch: make(chan []byte, 1), player: ""}
	per := &audioSub{ch: make(chan []byte, 1), player: "wesing"}
	h.addSub(root)
	h.addSub(per)
	h.BroadcastAudioFrame("", []byte("idle")) // 空闲空帧
	if got := <-root.ch; string(got) != "idle" {
		t.Fatalf("root 应收到空闲空帧, got %q", got)
	}
	if len(per.ch) != 0 {
		t.Fatal("per-player 订阅不该收到 player=\"\" 的空闲空帧")
	}
}
