package wesing

// 本文件只测音译的等待（waitRoma）：只在 pending 时等，等也有上限。

import "testing"

// 这段等待卡在宣告之前，白等一次就是整首歌的 status_update / song_info_update / all_lyrics 晚一秒。
// 真机（2026-10-09）：切到不带音译的歌之后音译轨还留着上一首的行，旧实现见音译轨在、配上 0 行就等。
//
// 变异自证：把 waitRoma 里的 `!pending ||` 去掉（配上 0 行就等），「不是 pending」一例等了 5 次而红；
// 把 `try >= romaWaitTries` 去掉（一直 pending 就一直等），「一直 pending」一例超过上限而红。
func TestWaitRomaOnlyWhenPending(t *testing.T) {
	type step struct {
		attached int
		pending  bool
	}
	cases := []struct {
		name      string
		steps     []step // 第 i 次 load 的返回；用完之后一直返回最后一项
		want      int
		wantWaits int
	}{
		{"头一次就配上", []step{{68, false}}, 68, 0},
		{"不是 pending（没有音译轨、是别的歌的、有行而一行没配上）：不等", []step{{0, false}}, 0, 0},
		{"pending，第三次读到了", []step{{0, true}, {0, true}, {113, false}}, 113, 2},
		{"pending，后来发现有行而一行没配上：停", []step{{0, true}, {0, false}}, 0, 1},
		{"一直 pending：等满上限为止", []step{{0, true}}, 0, romaWaitTries},
	}
	for _, c := range cases {
		loads, waits := 0, 0
		load := func() (int, bool) {
			i := loads
			if i >= len(c.steps) {
				i = len(c.steps) - 1
			}
			loads++
			return c.steps[i].attached, c.steps[i].pending
		}
		wait := func() {
			waits++
			if waits > romaWaitTries*4 {
				panic("waitRoma 没有上限") // 不设上限的变异会在这里停下，而不是把测试挂死
			}
		}
		got := func() (n int) {
			defer func() {
				if r := recover(); r != nil {
					n = -1
				}
			}()
			return waitRoma(load, wait)
		}()
		if got != c.want || waits != c.wantWaits {
			t.Errorf("%s：配上 %d 行、等了 %d 次，want %d 行、%d 次", c.name, got, waits, c.want, c.wantWaits)
		}
	}
}
