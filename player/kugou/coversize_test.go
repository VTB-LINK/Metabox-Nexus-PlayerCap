package kugou

// 本文件只测封面 URL 请求的尺寸档（issue #109）：getPlayInfo 给的 /stdmusic/120/ 换成 800 档
// （normalizeKugouCoverURL），以及封面晚到时按换档后的值比较、送进 coverCh 的是 800 档（offerLateCover）。

import "testing"

const (
	kugouCover120 = "http://imge.kugou.com/stdmusic/120/20211220/20211220112203398944.jpg"
	kugouCover800 = "http://imge.kugou.com/stdmusic/800/20211220/20211220112203398944.jpg"
)

// 变异自证（实测）：替换范围扩到整串（去掉对 ? 与 # 的截断）→「查询串里」「片段里」「查询串之前」三例红
// （末一例靠幂等检查）；去掉替换 →「120 档」「https」「查询串之前」三例红。
func TestNormalizeKugouCoverURL(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"120 档换成 800 档", kugouCover120, kugouCover800},
		{"https 同样换", "https://imge.kugou.com/stdmusic/120/20211220/a.jpg", "https://imge.kugou.com/stdmusic/800/20211220/a.jpg"},
		{"已是 800 档不变", kugouCover800, kugouCover800},
		{"空串", "", ""},
		{"文件名里的 120 不动", "http://imge.kugou.com/stdmusic/800/20211220/120.jpg", "http://imge.kugou.com/stdmusic/800/20211220/120.jpg"},
		{"文件名里的 stdmusic_120 不动", "http://imge.kugou.com/stdmusic/800/20211220/stdmusic_120_a.jpg", "http://imge.kugou.com/stdmusic/800/20211220/stdmusic_120_a.jpg"},
		{"段不完整（1200）不动", "http://imge.kugou.com/stdmusic/1200/20211220/a.jpg", "http://imge.kugou.com/stdmusic/1200/20211220/a.jpg"},
		{"段不完整（xstdmusic）不动", "http://imge.kugou.com/xstdmusic/120/20211220/a.jpg", "http://imge.kugou.com/xstdmusic/120/20211220/a.jpg"},
		{"段尾没有斜杠不动", "http://imge.kugou.com/stdmusic/120", "http://imge.kugou.com/stdmusic/120"},
		{"没见过的尺寸段（240）不动", "http://imge.kugou.com/stdmusic/240/20211220/a.jpg", "http://imge.kugou.com/stdmusic/240/20211220/a.jpg"},
		{"没见过的路径（softhead/120）不动", "http://imge.kugou.com/softhead/120/20211220/a.jpg", "http://imge.kugou.com/softhead/120/20211220/a.jpg"},
		{"查询串里的不动", "http://imge.kugou.com/stdmusic/800/a.jpg?from=/stdmusic/120/", "http://imge.kugou.com/stdmusic/800/a.jpg?from=/stdmusic/120/"},
		{"片段里的不动", "http://imge.kugou.com/stdmusic/800/a.jpg#/stdmusic/120/", "http://imge.kugou.com/stdmusic/800/a.jpg#/stdmusic/120/"},
		{"查询串之前的照换、查询串里的不动", "http://imge.kugou.com/stdmusic/120/a.jpg?from=/stdmusic/120/", "http://imge.kugou.com/stdmusic/800/a.jpg?from=/stdmusic/120/"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := normalizeKugouCoverURL(c.in)
			if got != c.want {
				t.Errorf("normalizeKugouCoverURL(%q) = %q，应为 %q", c.in, got, c.want)
			}
			if again := normalizeKugouCoverURL(got); again != got {
				t.Errorf("再调用一次应不变：%q → %q", got, again)
			}
		})
	}
}

// 变异自证（实测）：比较改用原样的 raw（`raw == "" || raw == current`）→「换歌那一轮」例红；
// 送进 coverCh 的改回 raw → 「VIP 首播封面晚到」例红；去掉 `cover == ""` 的判断 → 「封面还没到」例红。
func TestOfferLateCover(t *testing.T) {
	cases := []struct {
		name        string
		current     string
		raw         string
		wantCurrent string
		wantChanged bool
	}{
		{"VIP 首播封面晚到：送进去的是 800 档", "", kugouCover120, kugouCover800, true},
		{"换歌那一轮：current 已是 800 档，读数仍是 120 档，不算更新", kugouCover800, kugouCover120, kugouCover800, false},
		{"读数本就是 800 档且与 current 相同", kugouCover800, kugouCover800, kugouCover800, false},
		{"封面还没到（读数为空）", kugouCover800, "", kugouCover800, false},
		{"封面还没到且 current 也为空", "", "", "", false},
		{"曲中换了另一张图", kugouCover800, "http://imge.kugou.com/stdmusic/120/20211221/b.jpg", "http://imge.kugou.com/stdmusic/800/20211221/b.jpg", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ch := make(chan string, 1)
			gotCurrent, gotChanged := offerLateCover(ch, c.current, c.raw)
			if gotCurrent != c.wantCurrent || gotChanged != c.wantChanged {
				t.Fatalf("offerLateCover(%q, %q) = (%q, %v)，应为 (%q, %v)",
					c.current, c.raw, gotCurrent, gotChanged, c.wantCurrent, c.wantChanged)
			}
			select {
			case sent := <-ch:
				if !c.wantChanged {
					t.Errorf("没有更新时不应往 coverCh 送东西，实得 %q", sent)
				} else if sent != c.wantCurrent {
					t.Errorf("送进 coverCh 的应为 %q，实得 %q", c.wantCurrent, sent)
				}
			default:
				if c.wantChanged {
					t.Error("有更新时应把 URL 送进 coverCh")
				}
			}
		})
	}
}

// coverCh 满或为 nil 时不阻塞、照旧更新 current（与抽出前的 select default 同义）。
func TestOfferLateCoverNeverBlocks(t *testing.T) {
	full := make(chan string, 1)
	full <- "旧值"
	if got, changed := offerLateCover(full, "", kugouCover120); got != kugouCover800 || !changed {
		t.Errorf("coverCh 满时应照旧返回 (%q, true)，实得 (%q, %v)", kugouCover800, got, changed)
	}
	if v := <-full; v != "旧值" {
		t.Errorf("coverCh 满时不应替换缓冲里的值，实得 %q", v)
	}
	if got, changed := offerLateCover(nil, "", kugouCover120); got != kugouCover800 || !changed {
		t.Errorf("coverCh 为 nil 时应照旧返回 (%q, true)，实得 (%q, %v)", kugouCover800, got, changed)
	}
}
