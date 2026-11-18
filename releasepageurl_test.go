package main

// 本文件只测 releasePageURL：自动更新失败时提示的手动下载地址（#210）。

import "testing"

func TestReleasePageURL(t *testing.T) {
	tests := []struct {
		name   string
		prefix string
		tag    string
		want   string
	}{
		{name: "gitee", prefix: "https://gitee.com/VTB-LINK/Metabox-Nexus-PlayerCap/releases/download/", tag: "v4.0.0-rc.11",
			want: "https://gitee.com/VTB-LINK/Metabox-Nexus-PlayerCap/releases/tag/v4.0.0-rc.11"},
		{name: "github", prefix: "https://github.com/VTB-LINK/Metabox-Nexus-PlayerCap/releases/download/", tag: "v4.0.0",
			want: "https://github.com/VTB-LINK/Metabox-Nexus-PlayerCap/releases/tag/v4.0.0"},
		{name: "proxied github", prefix: "https://github.boki.moe/https://github.com/VTB-LINK/Metabox-Nexus-PlayerCap/releases/download/", tag: "v4.0.0",
			want: "https://github.boki.moe/https://github.com/VTB-LINK/Metabox-Nexus-PlayerCap/releases/tag/v4.0.0"},
		// 反例：不是 …/releases/download/ 结尾的前缀照旧给出前缀之下的目录。
		{name: "no trailing slash", prefix: "https://gitee.com/o/r/releases/download", tag: "v4.0.0",
			want: "https://gitee.com/o/r/releases/downloadv4.0.0/"},
		{name: "other cdn", prefix: "https://cdn.example.com/playercap/", tag: "v4.0.0",
			want: "https://cdn.example.com/playercap/v4.0.0/"},
		{name: "download segment not at end", prefix: "https://cdn.example.com/releases/download/mirror/", tag: "v4.0.0",
			want: "https://cdn.example.com/releases/download/mirror/v4.0.0/"},
		{name: "case differs", prefix: "https://gitee.com/o/r/Releases/Download/", tag: "v4.0.0",
			want: "https://gitee.com/o/r/Releases/Download/v4.0.0/"},
		{name: "empty prefix", prefix: "", tag: "v4.0.0", want: "v4.0.0/"},
		// 只换末尾一处：路径里更早出现的同名片段不动。
		{name: "segment repeated", prefix: "https://m.example.com/releases/download/https://gitee.com/o/r/releases/download/", tag: "v4.0.0",
			want: "https://m.example.com/releases/download/https://gitee.com/o/r/releases/tag/v4.0.0"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := releasePageURL(tt.prefix, tt.tag); got != tt.want {
				t.Fatalf("releasePageURL(%q, %q) = %q, want %q", tt.prefix, tt.tag, got, tt.want)
			}
		})
	}
}
