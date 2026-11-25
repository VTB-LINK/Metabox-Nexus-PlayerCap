package qqmusic

// 本文件只测 takeResult：换歌拍、补取块与每拍从读取者取结果都经它（设计 §6.8）。它不等待：没有读取者、通道里没有结果时立即
// 返回；每次至多取一条交给 lyricState.apply，过时、未知与指纹不变的结果取走后不要求重发。接线由 assocwiring_test.go 守。

import (
	"testing"
	"time"
)

// takeWant 在 1 秒内调用一次 takeResult（阻塞即判失败，不挂住测试），断言是否要求重发与给出的行。
func takeWant(t *testing.T, s *lyricState, ch <-chan assocResult, songHasIDs, changed bool, want ...string) {
	t.Helper()
	type out struct {
		lines   []lyricLine
		changed bool
	}
	done := make(chan out, 1)
	go func() {
		l, c := takeResult(s, ch, songHasIDs, stateOffsetSec)
		done <- out{l, c}
	}()
	select {
	case o := <-done:
		checkOutput(t, "takeResult", o.lines, o.changed, changed, want)
	case <-time.After(time.Second):
		t.Fatal("takeResult 阻塞了：轮询线程上取结果不得等待")
	}
}

// 反例：没有读取者（Results 为 nil 通道）与通道里没有结果，都立即返回、不要求重发，状态不变。
func TestTakeResultNothingToTake(t *testing.T) {
	var s lyricState
	s.reset(1)
	s.setOnlineRaw(shiftFixture())
	takeWant(t, &s, nil, true, false)
	takeWant(t, &s, make(chan assocResult, 1), true, false)
	wantSource(t, &s, srcNone, "") // 没有结果，什么也没给出
	refreshWant(t, &s, true, true, onlineAt0...)
}

// 有结果即交给 apply，给出折算后的行；每次只取一条，其余留在通道里供下一拍。
func TestTakeResultTakesOneAndApplies(t *testing.T) {
	var s lyricState
	s.reset(1)
	s.setOnlineRaw(shiftFixture())
	ch := make(chan assocResult, 2)
	ch <- desiredRes(1, srcAssoc, -2000, assocFixture("A"), "A")
	ch <- onlineRes(1, -1000)
	takeWant(t, &s, ch, true, true, assocADelayed2s...)
	wantSource(t, &s, srcAssoc, "")
	if len(ch) != 1 {
		t.Fatalf("一次应只取一条，通道里剩 %d 条", len(ch))
	}
	takeWant(t, &s, ch, true, true, onlineDelayed1s...)
	wantSource(t, &s, srcOnline, "")
}

// 反例：过时 gen、未知、指纹不变的结果都被取走（不留到下一拍），不要求重发，已给出的来源不变。
func TestTakeResultIgnoredResultsAreConsumed(t *testing.T) {
	var s lyricState
	s.reset(2)
	s.setOnlineRaw(shiftFixture())
	refreshWant(t, &s, true, true, onlineAt0...)
	ch := make(chan assocResult, 1)
	for _, res := range []assocResult{
		desiredRes(1, srcAssoc, 0, assocFixture("A"), "A"), // 上一首（或编号落位之前）的
		{gen: 2, kind: resUnknown},
		onlineRes(2, 0), // 与已给出的相同
	} {
		ch <- res
		takeWant(t, &s, ch, true, false)
		if len(ch) != 0 {
			t.Fatalf("结果 %+v 应被取走", res)
		}
		wantSource(t, &s, srcOnline, "")
	}
}

// 来源变化时打一条 Info（来源名经 i18n.T 注入，测试在中文下跑）；只改偏移、来源不变时照常要求重发而不打。
func TestTakeResultLogsSourceChange(t *testing.T) {
	logs := captureAssocLog(t)
	var s lyricState
	s.reset(1)
	s.setOnlineRaw(shiftFixture())
	refreshWant(t, &s, true, true, onlineAt0...)
	ch := make(chan assocResult, 1)
	ch <- onlineRes(1, -1000)
	takeWant(t, &s, ch, true, true, onlineDelayed1s...)
	if n := countLog(logs(), "*", "歌词来源改为"); n != 0 {
		t.Fatalf("来源未变不应打来源变化的日志，实得 %d 条：\n%s", n, logs())
	}
	ch <- desiredRes(1, srcAssoc, -2000, assocFixture("A"), "A")
	takeWant(t, &s, ch, true, true, assocADelayed2s...)
	ch <- onlineRes(1, 0)
	takeWant(t, &s, ch, true, true, onlineAt0...)
	for _, want := range []string{"歌词来源改为「QQ 音乐关联」：2 行", "歌词来源改为「QQ 音乐在线歌词」：2 行"} {
		if n := countLog(logs(), "*", want); n != 1 {
			t.Fatalf("应有 1 条 %q，实得 %d 条：\n%s", want, n, logs())
		}
	}
}

// 重开之前残留在通道里的结果：通道里是（1，0）的关联 A，会话已 reopened(1)。takeResult 取走它、不要求重发、不打来源变化的
// 日志，显示仍是在线。对照：（1，1）的同一份关联被收，打一条来源变化。
// 变异自证（实测）：apply 只比 gen，残留的 A 被收、打了日志而红。
func TestTakeResultRejectsResidueAfterReopen(t *testing.T) {
	logs := captureAssocLog(t)
	var s lyricState
	s.reset(1)
	s.setOnlineRaw(shiftFixture())
	refreshWant(t, &s, true, true, onlineAt0...)
	ch := make(chan assocResult, 1)
	ch <- desiredRes(1, srcAssoc, -2000, assocFixture("A"), "A")
	s.reopened(1)
	takeWant(t, &s, ch, true, false)
	if len(ch) != 0 {
		t.Fatal("残留的结果应被取走")
	}
	wantSource(t, &s, srcOnline, "")
	if n := countLog(logs(), "*", "歌词来源改为"); n != 0 {
		t.Fatalf("残留的结果被拒，不应打来源变化的日志，实得 %d 条：\n%s", n, logs())
	}
	ch <- atReopen(desiredRes(1, srcAssoc, -2000, assocFixture("A"), "A"), 1)
	takeWant(t, &s, ch, true, true, assocADelayed2s...)
	if n := countLog(logs(), "*", "歌词来源改为「QQ 音乐关联」：2 行"); n != 1 {
		t.Fatalf("（1，1）的关联被收，应打 1 条来源变化，实得 %d 条：\n%s", n, logs())
	}
}

// 换歌拍编号缺位时会话先以 refresh 登记「无」已给出：读取者随后给出的「无」（本地文件未认定）不再要求重发，免得在编号还在
// 补取时多发一条 index:-1；本地文件的歌词到达照常发出。反例：不登记时同一条「无」要求重发。
func TestTakeResultNoneAfterClaimWithoutIDs(t *testing.T) {
	var s lyricState
	s.reset(1)
	refreshWant(t, &s, false, true) // 换歌拍的登记：行为空
	ch := make(chan assocResult, 1)
	ch <- noneRes(1)
	takeWant(t, &s, ch, false, false)
	ch <- localRes(1)
	takeWant(t, &s, ch, false, true, localLDelayed1s...)
	wantSource(t, &s, srcLocal, localFlac)

	var bare lyricState
	bare.reset(1)
	ch <- noneRes(1)
	takeWant(t, &bare, ch, false, true)
}
