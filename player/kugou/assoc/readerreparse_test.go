package assoc

// 本文件只测确认之后对当前 hash 重新解析的时机（设计 §6.7「读取者」的自行节拍）：库文件的（大小, 修改时间）
// 与上次解析时不同且已稳定 1 秒；结果带待重试时每 5 秒；暂时失败按 1、2、4、8 秒退避（之后保持 8 秒），
// 一次成功或换 hash 即复位。其余情形不重新解析。

import (
	"errors"
	"fmt"
	"io/fs"
	"sync/atomic"
	"testing"
	"time"
)

// statAt 返回按时刻给出库文件（大小, 修改时间）的桩：at 落在 changes 的第 i 段（[changes[i-1], changes[i])）即第 i 个值。
func statAt(changes ...time.Duration) func(time.Duration) (int64, time.Time, error) {
	return func(at time.Duration) (int64, time.Time, error) {
		i := 0
		for i < len(changes) && at >= changes[i] {
			i++
		}
		return 1 << 20, time.Unix(int64(1000+i), 0), nil
	}
}

func TestReaderReparseOnStableMtime(t *testing.T) {
	cases := []struct {
		name string
		stat func(time.Duration) (int64, time.Time, error)
		want []string
	}{
		{"第 2 秒变、第 3 秒同", statAt(2 * time.Second), []string{"A@0s", "A@3s"}},
		{"只有大小变", func(at time.Duration) (int64, time.Time, error) {
			size := int64(1 << 20)
			if at >= 2*time.Second {
				size += 1024
			}
			return size, time.Unix(1000, 0), nil
		}, []string{"A@0s", "A@3s"}},
		{"变了两次、各稳定 1 秒以上", statAt(2*time.Second, 5*time.Second), []string{"A@0s", "A@3s", "A@6s"}},
		// 反例
		{"每秒都变", func(at time.Duration) (int64, time.Time, error) {
			return 1 << 20, time.Unix(int64(1000+at/time.Second), 0), nil
		}, []string{"A@0s"}},
		{"不变", statAt(), []string{"A@0s"}},
		{"变了又在 1 秒内变回", func(at time.Duration) (int64, time.Time, error) {
			if at >= 2*time.Second && at < 3*time.Second {
				return 1 << 20, time.Unix(2000, 0), nil
			}
			return 1 << 20, time.Unix(1000, 0), nil
		}, []string{"A@0s"}},
		{"stat 一直失败", func(time.Duration) (int64, time.Time, error) {
			return 0, time.Time{}, fs.ErrNotExist
		}, []string{"A@0s"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeDeps{statFn: c.stat}
			runReader(t, f, func(t *testing.T, r *Reader) {
				r.SetHash("A")
				wantKind(t, latest(t, r), "A", KindFound)
				time.Sleep(10 * time.Second)
				wantParses(t, f, c.want...)
			})
		})
	}
}

// 反例：stat 在解析时失败、之后恢复，恢复后的值稳定 1 秒即重新解析（库被替换后重新出现）；没有 hash 时不 stat。
func TestReaderReparseAfterStatRecovers(t *testing.T) {
	var stats atomic.Int32
	f := &fakeDeps{statFn: func(at time.Duration) (int64, time.Time, error) {
		stats.Add(1)
		if at < time.Hour+2*time.Second {
			return 0, time.Time{}, fs.ErrNotExist
		}
		return 1 << 20, time.Unix(1000, 0), nil
	}}
	runReader(t, f, func(t *testing.T, r *Reader) {
		time.Sleep(time.Hour)
		if n := stats.Load(); n != 0 {
			t.Fatalf("没有 hash 时 stat %d 次，want 0", n)
		}
		r.SetHash("A")
		latest(t, r)
		time.Sleep(10 * time.Second)
		wantParses(t, f, "A@1h0m0s", "A@1h0m3s")
	})
}

func TestReaderRetryEvery5s(t *testing.T) {
	t.Run("带待重试每 5 秒，某次不带即停", func(t *testing.T) {
		f := &fakeDeps{
			candsFn: func(int, string) ([]candidate, error) { return []candidate{assocCand, cacheCand}, nil },
			readFn: func(at time.Duration, path string) ([]byte, error) {
				if path == cachePath || path == assocCand.Path && at >= 10*time.Second {
					return []byte(testLRC), nil
				}
				return nil, fs.ErrNotExist
			},
		}
		runReader(t, f, func(t *testing.T, r *Reader) {
			r.SetHash("A")
			res := latest(t, r)
			if res.Kind != KindFound || res.Source != SourceCache || !res.Retry {
				t.Fatalf("结果 %+v，want 关联暂缺、取缓存、带待重试", res)
			}
			time.Sleep(30 * time.Second)
			wantParses(t, f, "A@0s", "A@5s", "A@10s")
			res = latest(t, r)
			if res.Kind != KindFound || res.Source != SourceAssociation || res.Retry {
				t.Fatalf("结果 %+v，want 关联出现后取关联、不带待重试", res)
			}
		})
	})
	t.Run("全部暂缺为没有，同样每 5 秒", func(t *testing.T) {
		f := &fakeDeps{candsFn: func(int, string) ([]candidate, error) { return []candidate{assocCand}, nil }}
		runReader(t, f, func(t *testing.T, r *Reader) {
			r.SetHash("A")
			res := latest(t, r)
			if res.Hash != "A" || res.Kind != KindNone || !res.Retry {
				t.Fatalf("结果 %+v，want KindNone 带待重试", res)
			}
			time.Sleep(12 * time.Second)
			wantParses(t, f, "A@0s", "A@5s", "A@10s")
		})
	})
	// 反例：不带待重试的「没有」与歌词都不重新解析。
	for name, cands := range map[string][]candidate{"没有": nil, "歌词": {cacheCand}} {
		t.Run("不带待重试的"+name, func(t *testing.T) {
			f := &fakeDeps{candsFn: func(int, string) ([]candidate, error) { return cands, nil }}
			runReader(t, f, func(t *testing.T, r *Reader) {
				r.SetHash("A")
				if res := latest(t, r); res.Retry {
					t.Fatalf("结果 %+v，want 不带待重试", res)
				}
				time.Sleep(time.Minute)
				wantParses(t, f, "A@0s")
			})
		})
	}
	// 反例：换 hash 后不再按上一首的待重试重新解析。
	t.Run("换 hash 不沿用", func(t *testing.T) {
		f := &fakeDeps{candsFn: func(_ int, hash string) ([]candidate, error) {
			if hash == "A" {
				return []candidate{assocCand}, nil
			}
			return []candidate{cacheCand}, nil
		}}
		runReader(t, f, func(t *testing.T, r *Reader) {
			r.SetHash("A")
			latest(t, r)
			time.Sleep(2 * time.Second)
			r.SetHash("B")
			wantKind(t, latest(t, r), "B", KindFound)
			time.Sleep(time.Minute)
			wantParses(t, f, "A@0s", "B@2s")
		})
	})
}

func TestReaderTransientBackoff(t *testing.T) {
	t.Run("一直暂时失败", func(t *testing.T) {
		f := &fakeDeps{candsFn: func(int, string) ([]candidate, error) { return nil, errTransientStub }}
		runReader(t, f, func(t *testing.T, r *Reader) {
			r.SetHash("A")
			wantKind(t, latest(t, r), "A", KindTransient)
			time.Sleep(30 * time.Second)
			wantParses(t, f, "A@0s", "A@1s", "A@3s", "A@7s", "A@15s", "A@23s")
			wantKind(t, latest(t, r), "A", KindTransient)
		})
	})
	t.Run("成功后复位", func(t *testing.T) {
		// 第 5 次成功（关联暂缺、取缓存，带待重试），5 秒后的第 6 次起又暂时失败：从 1 秒重新退避。
		f := &fakeDeps{candsFn: func(n int, _ string) ([]candidate, error) {
			if n == 5 {
				return []candidate{assocCand, cacheCand}, nil
			}
			return nil, errTransientStub
		}}
		runReader(t, f, func(t *testing.T, r *Reader) {
			r.SetHash("A")
			time.Sleep(15 * time.Second)
			res := latest(t, r)
			if res.Kind != KindFound || !res.Retry {
				t.Fatalf("15 s 时结果 %+v，want 第 5 次成功", res)
			}
			time.Sleep(8 * time.Second)
			wantParses(t, f, "A@0s", "A@1s", "A@3s", "A@7s", "A@15s", "A@20s", "A@21s", "A@23s")
		})
	})
	t.Run("换 hash 复位", func(t *testing.T) {
		f := &fakeDeps{candsFn: func(int, string) ([]candidate, error) { return nil, errTransientStub }}
		runReader(t, f, func(t *testing.T, r *Reader) {
			r.SetHash("A")
			time.Sleep(8 * time.Second) // A 的下一次在 15 s
			r.SetHash("B")
			wantKind(t, latest(t, r), "B", KindTransient)
			time.Sleep(4 * time.Second)
			wantParses(t, f, "A@0s", "A@1s", "A@3s", "A@7s", "B@8s", "B@9s", "B@11s")
		})
	})
	// 反例：读库返回三类以外的错误同样按暂时失败退避，不停用。
	t.Run("其他错误", func(t *testing.T) {
		f := &fakeDeps{candsFn: func(int, string) ([]candidate, error) { return nil, errors.New("unexpected") }}
		runReader(t, f, func(t *testing.T, r *Reader) {
			r.SetHash("A")
			wantKind(t, latest(t, r), "A", KindTransient)
			time.Sleep(4 * time.Second)
			wantParses(t, f, "A@0s", "A@1s", "A@3s")
		})
	})
	// 反例：包装了 errTransient 的错误不因外层另有包装而被当成别的。
	t.Run("多层包装", func(t *testing.T) {
		f := &fakeDeps{candsFn: func(int, string) ([]candidate, error) {
			return nil, fmt.Errorf("outer: %w", errTransientStub)
		}}
		runReader(t, f, func(t *testing.T, r *Reader) {
			r.SetHash("A")
			wantKind(t, latest(t, r), "A", KindTransient)
			time.Sleep(2 * time.Second)
			wantParses(t, f, "A@0s", "A@1s")
		})
	})
}
