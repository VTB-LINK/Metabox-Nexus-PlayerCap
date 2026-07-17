package qqmusic

// 本文件只测 parseLRC 的标准 LRC 时间戳解析边界（分钟/秒/毫秒的位数）。
// 不测 QRC 解密（qrcdecrypt_test.go）、不测压缩型与 intentional blank —— 后两者是已知的
// 解析器能力缺口，但 QQ 音乐歌词库实测无此格式样本，按版本策略不投入，详见 commit 记录。

import "testing"

// TestParseLRCLongMinutes 钉死分钟位不限两位。
//
// 缺陷背景：正则原为 `\[(\d{2}):(\d{2})\.(\d{2,3})\]`，分钟位写死两位。时长超 99 分钟的
// 音频（长混音 / DJ 串烧 / 有声书类）行首是 `[100:05.00]`，整行匹配不上 → 被静默丢弃，
// 那之后的歌词全部消失且无任何日志。与歌词来源格式无关，任何长音频都会中。
//
// 变异自证：把分钟位改回 `\d{2}` 即红。
func TestParseLRCLongMinutes(t *testing.T) {
	// 100 分 05.00 秒 = 6005 秒
	got, _, _ := parseLRC("[100:05.00]LONG\n", 7200000)
	if len(got) != 1 {
		t.Fatalf("超 99 分钟的行必须能解析（否则整行静默丢弃），实得 %d 条", len(got))
	}
	if got[0].Time != 6005.0 {
		t.Errorf("Time 应为 6005.00（100*60+5），实得 %.2f", got[0].Time)
	}
	if got[0].Text != "LONG" {
		t.Errorf("Text 应为 LONG，实得 %q", got[0].Text)
	}
}

// TestParseLRCTimestampWidths 回归：常见位数组合的解析不得变化。
// 分钟位放宽后，秒（恰好两位）与毫秒（两位或三位）必须仍按原样工作。
func TestParseLRCTimestampWidths(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want float32
	}{
		{"两位分钟 + 两位毫秒（最常见）", "[02:04.45]X", 124.45},
		{"两位分钟 + 三位毫秒", "[00:14.640]X", 14.64},
		{"零分钟", "[00:05.00]X", 5.0},
		{"三位分钟", "[123:45.67]X", 7425.67},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _, _ := parseLRC(c.in+"\n", 600000)
			if len(got) != 1 {
				t.Fatalf("应解析出 1 条，实得 %d 条", len(got))
			}
			if got[0].Time != c.want {
				t.Errorf("Time 应为 %.2f，实得 %.2f", c.want, got[0].Time)
			}
			if got[0].Text != "X" {
				t.Errorf("Text 应为 X，实得 %q", got[0].Text)
			}
		})
	}
}
