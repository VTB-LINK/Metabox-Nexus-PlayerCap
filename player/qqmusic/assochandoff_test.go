package qqmusic

// 本文件只测会话与读取者之间的交接（设计 §6.8「输入」「输出」）：Submit 覆盖读取者尚未取走的旧值；结果通道只留最新一条；
// 代次（gen，reopen）原样带回；未知不顶掉同一代次尚未取走的结果，代次不同时照常顶掉；读取者不存在（nil）时 Submit 与 Results
// 不 panic、不阻塞。在线歌曲有候选时按假内存里当前歌曲的记录认键（assocrecord_test.go）。

import (
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

// 读取者阻塞在启动时的关联表读取里，期间连续提交三首：放行之后只处理最后一首（前两首的歌词文件一次都不读），只发一条结果。
// 变异自证（实测）：Submit 不覆盖尚未取走的旧值，处理了第一首（结果为 gen 1）而红。
func TestAssocSubmitOverwritesUnprocessed(t *testing.T) {
	e := desiredEnv()
	e.mem.play("二", idURL(2), "002Two")
	var g gate
	e.beforeStart = g.arm
	e.fs.onRead = func(p string) {
		if p == testTablePath {
			g.wait()
		}
	}
	runAssocReader(t, e, func(t *testing.T, r *assocReader) {
		defer g.release()
		r.Submit(songKey{gen: 1, songMid: "003Missing"})
		r.Submit(songKey{gen: 2, songMid: "003Unusable"})
		r.Submit(songKey{gen: 3, songID: 2, name: "二"})
		e.fs.setHooks(nil, nil)
		g.release()
		wantDesired(t, latestResult(t, r), 3, srcAssoc, -500, false, "", "关联第一行|译文一", "关联第二行|译文二")
		noAssocResult(t, r)
		for _, p := range []string{lyrMissing, lyrBad} {
			wantTimes(t, "读 "+p, e.fs.readsOf(p), nil)
		}
	})
}

// 会话不取结果时，通道里只留最新一条；gen 原样带回（不要求递增）。
func TestAssocResultsKeepOnlyLatest(t *testing.T) {
	e := desiredEnv()
	e.mem.play("二", idURL(2), "002Two")
	runAssocReader(t, e, func(t *testing.T, r *assocReader) {
		r.Submit(songKey{gen: 42, songID: 1})
		synctest.Wait()
		r.Submit(songKey{gen: 7, songID: 2, name: "二"})
		wantDesired(t, latestResult(t, r), 7, srcAssoc, -500, false, "", "关联第一行|译文一", "关联第二行|译文二")
		noAssocResult(t, r)
		r.Submit(songKey{gen: 42, songID: 1})
		wantDesired(t, latestResult(t, r), 42, srcOnline, 0, false, "")
	})
}

// 未知不顶掉同一 gen 尚未取走的期望状态（会话对未知不动作，顶掉即丢了期望状态）；取走之后的未知照常发出。
// 变异自证（实测）：去掉这条保留规则，第一次取到的是未知而红。
func TestAssocUnknownKeepsUnconsumedResult(t *testing.T) {
	e := offsetEnv()
	e.fs.put(testTablePath, offsetTable(100))
	runAssocReader(t, e, func(t *testing.T, r *assocReader) {
		r.Submit(songKey{gen: 1, songID: 100, name: "百"})
		sleepUntil(e, 500*time.Millisecond)
		e.fs.put(testTablePath, offsetTable(200))
		e.fs.setHooks(nil, func(string) error { return errors.New("the process cannot access the file") })
		sleepUntil(e, 2*time.Second)
		wantDesired(t, latestResult(t, r), 1, srcAssoc, 100, false, "", assocLines...)
		sleepUntil(e, 3*time.Second)
		if got := latestResult(t, r); got.kind != resUnknown || got.gen != 1 {
			t.Fatalf("取走期望状态之后的读坏：结果 %+v，want gen 1 的未知", got)
		}
	})
}

// 未知只在代次完整相同时保留通道里的旧结果：通道里是（1，0）的期望状态，新到（1，0）的未知 → 保留；新到（1，1）的未知
// （重开之后）、（2，0）的未知（换歌之后）→ 顶掉，重开前残留的结果不留到重开之后。期望状态照常顶掉同一代次的期望状态。
// 变异自证（实测）：保留规则只比 gen，（1，1）一例取到的仍是（1，0）的期望状态而红。
func TestAssocUnknownKeepsSameGeneration(t *testing.T) {
	r := &assocReader{wake: make(chan struct{}, 1), results: make(chan assocResult, 1)}
	desired := assocResult{gen: 1, kind: resDesired, source: srcAssoc, lyricOffsetMs: 500}
	for _, c := range []struct {
		name string
		next assocResult
		want assocResult
	}{
		{"同代次的未知", assocResult{gen: 1, kind: resUnknown}, desired},
		{"重开之后的未知", assocResult{gen: 1, reopen: 1, kind: resUnknown}, assocResult{gen: 1, reopen: 1, kind: resUnknown}},
		{"换歌之后的未知", assocResult{gen: 2, kind: resUnknown}, assocResult{gen: 2, kind: resUnknown}},
		{"同代次的期望状态", assocResult{gen: 1, kind: resDesired, source: srcOnline}, assocResult{gen: 1, kind: resDesired, source: srcOnline}},
	} {
		r.send(desired)
		r.send(c.next)
		select {
		case got := <-r.Results():
			if got.gen != c.want.gen || got.reopen != c.want.reopen || got.kind != c.want.kind || got.source != c.want.source ||
				got.lyricOffsetMs != c.want.lyricOffsetMs {
				t.Errorf("%s：取到 %+v，want %+v", c.name, got, c.want)
			}
		default:
			t.Fatalf("%s：通道为空", c.name)
		}
	}
}

// 没有读取者（startAssocReader 返回假）时会话拿到 nil：Submit 不 panic，Results 返回 nil 通道，带 default 的接收不阻塞。
func TestAssocNilReader(t *testing.T) {
	var r *assocReader
	r.Submit(songKey{gen: 1})
	select {
	case res := <-r.Results():
		t.Fatalf("nil 读取者给出了结果 %+v", res)
	default:
	}
}
