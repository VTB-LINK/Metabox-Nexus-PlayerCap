package assoc

// 本文件只测歌词目录在确认时还不存在、之后酷狗才建出并写入缓存的情形（watchdog.Confirm 不因目录不存在而
// 置空 LyricDir）：拼在歌词目录下的相对路径候选读不到即暂缺、结果带待重试，文件出现后的下一次重试取到。

import (
	"io/fs"
	"slices"
	"sync"
	"testing"
	"time"
)

func TestReaderRelativeCandidateAppearsLater(t *testing.T) {
	var mu sync.Mutex
	var reads []string
	f := &fakeDeps{ // 候选为默认的 [cacheCand]，相对路径 a.lrc
		readFn: func(at time.Duration, path string) ([]byte, error) {
			mu.Lock()
			reads = append(reads, path)
			mu.Unlock()
			if at >= 5*time.Second {
				return []byte(testLRC), nil
			}
			return nil, fs.ErrNotExist
		},
	}
	runReader(t, f, func(t *testing.T, r *Reader) {
		r.SetHash("A")
		if res := latest(t, r); res.Hash != "A" || res.Kind != KindNone || !res.Retry {
			t.Fatalf("文件尚不存在：结果 %+v，want KindNone 带待重试", res)
		}
		time.Sleep(5 * time.Second)
		res := latest(t, r)
		if res.Hash != "A" || res.Kind != KindFound || res.Source != SourceCache || res.Retry || len(res.Lines) != 2 {
			t.Fatalf("文件出现后：结果 %+v，want 取到缓存、不带待重试", res)
		}
		wantParses(t, f, "A@0s", "A@5s")
		mu.Lock()
		defer mu.Unlock()
		if !slices.Equal(reads, []string{cachePath, cachePath}) {
			t.Fatalf("读了 %q，want 两次都读拼在歌词目录下的 %q", reads, cachePath)
		}
	})
}
