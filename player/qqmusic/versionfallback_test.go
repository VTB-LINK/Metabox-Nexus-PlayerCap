package qqmusic

// 本文件只测未知版本的距离回退：nearestKnownVersion / latestKnownVersion / parseVer。
// 这三个是纯函数（不碰内存），能本机跑；qqmusic 整包 Windows-only、CI 跑不了（§8），
// 而 fallback 选错会让整台机器的 QQ 音乐读到垃圾，值得有测试钉住选择逻辑。
//
// 版本表（knownVersions）当前：20.05 / 21.81 / 22.16 / 22.22 / 22.31 / 22.41。
// 加新版本后，标了「依赖最新版本」的用例需要跟着更新——这是刻意的，它钉的就是当前表。

import "testing"

func TestNearestKnownVersion(t *testing.T) {
	cases := []struct {
		name     string
		detected string
		want     string
	}{
		// 实证：Sentry 上报的 22.21，夹在 22.16(距 5) 与 22.22(距 1) 之间，取近的 22.22。
		// 此前写死 22.16 恰好赌的是远的一边。
		{"22.21 取更近的 22.22", "22.21", "22.22"},
		// 距离切换点两侧：22.26 离 22.22(距 4) 近于 22.31(距 5)；22.27 反过来。
		{"22.26 偏向 22.22", "22.26", "22.22"},
		{"22.27 偏向 22.31", "22.27", "22.31"},
		// 距离相等时取版本号更大的一侧（确定 tie-break）：22.19 到 22.16 / 22.22 都是 3。
		{"tie 取更大 22.22", "22.19", "22.22"},
		// 比最新已知还新：落到最新版，字符串模型跟得上。用动态期望，避免加版本后此用例僵死。
		{"比最新高 → 最新", "999.99", latestKnownVersion()},
		// 比最老已知还老：落到最老版。
		{"比最老低 → 20.05", "1.00", "20.05"},
		// 恰好命中已知版本：距离 0，返回自己（该分支实际不走 fallback，但函数应稳）。
		{"命中已知返回自己", "22.31", "22.31"},
		// 解析不了：退最新。
		{"无法解析 → 最新", "not-a-version", latestKnownVersion()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := nearestKnownVersion(c.detected); got != c.want {
				t.Errorf("nearestKnownVersion(%q) = %q, 期望 %q", c.detected, got, c.want)
			}
		})
	}
}

func TestLatestKnownVersion(t *testing.T) {
	// 当前表最新是 22.41。加新版本后此用例应跟着改——它钉的就是「表里最大的那个」。
	if got := latestKnownVersion(); got != "22.41" {
		t.Errorf("latestKnownVersion() = %q, 期望 22.41", got)
	}
}

func TestParseVer(t *testing.T) {
	cases := []struct {
		in    string
		want  int
		wantK bool
	}{
		{"22.21", 22021, true},
		{"20.05", 20005, true},
		{"22.41", 22041, true},
		{"22", 0, false},     // 无小数点
		{"22.x", 0, false},   // minor 非数字
		{"x.16", 0, false},   // major 非数字
		{"", 0, false},       // 空
		{"1.00", 1000, true}, // 边界
	}
	for _, c := range cases {
		got, ok := parseVer(c.in)
		if ok != c.wantK || (ok && got != c.want) {
			t.Errorf("parseVer(%q) = (%d, %v), 期望 (%d, %v)", c.in, got, ok, c.want, c.wantK)
		}
	}
}
