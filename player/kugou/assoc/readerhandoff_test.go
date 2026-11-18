package assoc

// 本文件只测会话与读取者之间的交接（设计 §6.7「读取者」的输入与输出）：写入的 hash 新值覆盖读取者尚未取走的
// 旧值、空串忽略；结果通道容量 1，读取者发送前清空旧结果，只留最新一条。

import (
	"testing"
	"testing/synctest"
	"time"
)

func TestReaderNewHashOverridesPending(t *testing.T) {
	var g gate
	f := &fakeDeps{candsFn: func(_ int, hash string) ([]candidate, error) {
		if hash == "A" {
			g.wait()
		}
		return []candidate{cacheCand}, nil
	}}
	runReader(t, f, func(t *testing.T, r *Reader) {
		g.arm()
		defer g.release()
		r.SetHash("A")
		synctest.Wait() // 读取者阻塞在 A 的解析里
		r.SetHash("B")
		r.SetHash("C")
		g.release()
		wantKind(t, latest(t, r), "C", KindFound)
		wantParses(t, f, "A@0s", "C@0s")
	})
}

func TestReaderResultsKeepLatest(t *testing.T) {
	f := &fakeDeps{}
	runReader(t, f, func(t *testing.T, r *Reader) {
		r.SetHash("A")
		synctest.Wait()
		r.SetHash("B")
		synctest.Wait()
		if n := len(r.Results()); n != 1 {
			t.Fatalf("通道里有 %d 条结果，want 1", n)
		}
		wantKind(t, latest(t, r), "B", KindFound)
		noResult(t, r)
		wantParses(t, f, "A@0s", "B@0s")
	})
}

// 反例：空串不唤醒读取者、不触发确认（只有启动时那一次）、不读库、不发结果，也不覆盖尚未取走的 hash；
// 之后写入的非空 hash 照常处理。
func TestReaderEmptyHashIgnored(t *testing.T) {
	var g gate
	f := &fakeDeps{candsFn: func(_ int, hash string) ([]candidate, error) {
		if hash == "A" {
			g.wait()
		}
		return []candidate{cacheCand}, nil
	}}
	runReader(t, f, func(t *testing.T, r *Reader) {
		g.arm()
		defer g.release()
		r.SetHash("")
		time.Sleep(time.Hour)
		noResult(t, r)
		if n := len(confirmCalls(f)); n != 1 {
			t.Fatalf("确认 %d 次，want 只有启动时的 1 次", n)
		}
		wantParses(t, f)

		r.SetHash("A")
		synctest.Wait()
		r.SetHash("B")
		r.SetHash("") // 不清掉待处理的 B
		g.release()
		wantKind(t, latest(t, r), "B", KindFound)
		wantParses(t, f, "A@1h0m0s", "B@1h0m0s")
	})
}
