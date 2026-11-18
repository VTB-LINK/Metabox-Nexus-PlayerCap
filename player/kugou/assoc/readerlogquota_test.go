package assoc

// 本文件只测读取者的日志条数（设计 §4 反目标「日志刷屏」、§6.8）：确认通过一条 Info；停用与放弃各一条 Warn；
// 暂时失败与确认未就绪的 Detail 同一原因每首歌至多一条、每首歌至多 maxDetailsPerSong 条，换歌后重新计。

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/player/kugou/watchdog"
)

// countLog 返回 out 中格式串 format（按当前语言）去掉首个占位符及其后内容的前缀出现的次数。
func countLog(out, format string) int {
	prefix, _, _ := strings.Cut(i18n.T(format), "%")
	return strings.Count(out, prefix)
}

const (
	logEnabled   = "Local lyric association enabled (KuGou %s, data directory %s)"
	logDisabled  = "Local lyric association disabled: %v"
	logTransient = "Reading the KuGou lyric database failed; will retry: %v"
	logNotReady  = "Local lyric association not confirmed yet: %v"
)

func TestReaderLogsBounded(t *testing.T) {
	t.Run("确认通过一条 Info", func(t *testing.T) {
		out := captureLog(t, func() {
			runReader(t, &fakeDeps{}, func(t *testing.T, r *Reader) {
				for _, h := range []string{"A", "B", "C"} {
					r.SetHash(h)
					latest(t, r)
				}
				time.Sleep(time.Minute)
			})
		})
		if n := countLog(out, logEnabled); n != 1 {
			t.Fatalf("启用日志 %d 条，want 1：\n%s", n, out)
		}
		if strings.Contains(out, "第一行") {
			t.Fatalf("日志里有歌词内容：\n%s", out)
		}
	})
	t.Run("未就绪到放弃：一条 Detail、一条 Warn", func(t *testing.T) {
		out := captureLog(t, func() {
			f := &fakeDeps{confirmFn: func(int) (watchdog.Confirmed, error) { return watchdog.Confirmed{}, errNotReadyStub }}
			runReader(t, f, func(t *testing.T, r *Reader) {
				time.Sleep(time.Hour) // 启动即确认，之后每 10 秒一次，30 次放弃
			})
		})
		if n, m := countLog(out, logNotReady), countLog(out, logDisabled); n != 1 || m != 1 {
			t.Fatalf("未就绪 %d 条、停用 %d 条，want 各 1：\n%s", n, m, out)
		}
	})
	t.Run("暂时失败：同一原因每首歌一条", func(t *testing.T) {
		out := captureLog(t, func() {
			f := &fakeDeps{candsFn: func(int, string) ([]candidate, error) { return nil, errTransientStub }}
			runReader(t, f, func(t *testing.T, r *Reader) {
				r.SetHash("A")
				time.Sleep(time.Hour)
				r.SetHash("B")
				time.Sleep(time.Minute)
			})
		})
		if n := countLog(out, logTransient); n != 2 {
			t.Fatalf("暂时失败日志 %d 条，want 两首歌各 1：\n%s", n, out)
		}
	})
	t.Run("暂时失败：原因各不相同时每首歌封顶", func(t *testing.T) {
		out := captureLog(t, func() {
			f := &fakeDeps{candsFn: func(n int, _ string) ([]candidate, error) {
				return nil, fmt.Errorf("%w: database size changed from %d to %d", errTransient, n, n+1)
			}}
			runReader(t, f, func(t *testing.T, r *Reader) {
				r.SetHash("A")
				time.Sleep(time.Hour)
			})
		})
		if n := countLog(out, logTransient); n != maxDetailsPerSong {
			t.Fatalf("暂时失败日志 %d 条，want %d：\n%s", n, maxDetailsPerSong, out)
		}
	})
	t.Run("停用一条 Warn", func(t *testing.T) {
		out := captureLog(t, func() {
			f := &fakeDeps{confirmFn: func(int) (watchdog.Confirmed, error) { return watchdog.Confirmed{}, errUnsupportedStub }}
			runReader(t, f, func(t *testing.T, r *Reader) {
				for _, h := range []string{"A", "B", "C"} {
					r.SetHash(h)
					latest(t, r)
				}
			})
		})
		if n := countLog(out, logDisabled); n != 1 {
			t.Fatalf("停用日志 %d 条，want 1：\n%s", n, out)
		}
	})
}
