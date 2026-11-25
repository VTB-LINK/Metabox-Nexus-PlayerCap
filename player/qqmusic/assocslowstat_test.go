package qqmusic

// 本文件只测关联表的 stat 很慢时节拍不饿死其余定时处理（设计 §6.8「节拍」）：stat 每次耗时 1.5 秒（如缓存目录在失联的
// 网络盘上）时，待重试、关联表的退避重读与本地认定照常进行。下一拍从这一拍处理完的时刻排起。

import (
	"errors"
	"testing"
	"time"
)

// slowTableStat 让关联表的每次 stat 耗时 1.5 秒（泡内假时钟），其余文件的 stat 不受影响。
func slowTableStat(e *readerEnv) {
	e.fs.onStat = func(p string) {
		if p == testTablePath {
			time.Sleep(1500 * time.Millisecond)
		}
	}
}

// 变异自证（实测）：下一拍仍从 stat 之前的时刻排起（修复前的写法），三例都红：重读暂缺的歌词 1 次；重读关联表 1 次；
// 本地认定 1 次。
func TestAssocSlowStatDoesNotStarveTimers(t *testing.T) {
	t.Run("待重试", func(t *testing.T) {
		e := newReaderEnv()
		e.fs.put(testTablePath, tableDat(assocItem{song: midURL("003Missing"), lyric: lyrMissing, byUser: true}))
		slowTableStat(e)
		e.mem.play("缺", midURL("003Missing"), "003Missing")
		runAssocReader(t, e, func(t *testing.T, r *assocReader) {
			r.Submit(songKey{gen: 1, songMid: "003Missing", name: "缺"})
			sleepUntil(e, 40*time.Second)
			if n := len(e.fs.readsOf(lyrMissing)); n < 4 {
				t.Errorf("40 秒内重读暂缺的歌词 %d 次，want 至少 4 次", n)
			}
		})
	})
	t.Run("关联表退避重读", func(t *testing.T) {
		e := newReaderEnv()
		e.fs.put(testTablePath, offsetTable(100))
		e.fs.readErr = func(p string) error {
			if p == testTablePath {
				return errors.New("the process cannot access the file")
			}
			return nil
		}
		slowTableStat(e)
		runAssocReader(t, e, func(t *testing.T, r *assocReader) {
			sleepUntil(e, 40*time.Second)
			if n := len(e.fs.readsOf(testTablePath)); n < 4 {
				t.Errorf("40 秒内重读关联表 %d 次，want 至少 4 次", n)
			}
		})
	})
	t.Run("本地认定", func(t *testing.T) {
		e := localEnv()
		slowTableStat(e)
		runAssocReader(t, e, func(t *testing.T, r *assocReader) {
			r.Submit(songKey{gen: 1, name: "一"})
			sleepUntil(e, 60*time.Second)
			if n := len(e.rm.callTimes()); n != len(localAttemptAt) {
				t.Errorf("60 秒内本地认定 %d 次，want %d 次", n, len(localAttemptAt))
			}
		})
	})
}
