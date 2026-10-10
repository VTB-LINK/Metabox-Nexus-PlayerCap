package main

// 本文件只测一件事：config 包定 config.yml 版本键盖章值时对「发布版本」的判定，与本包自动更新所用的
// isReleaseVersion 是同一个判据。
//
// config 包须保持 GOOS=linux 可构建，不能引用本包，所以它自带一份同样的判定（config.StampVersion 内）。
// 两份分叉的后果：某个版本号在这里算开发构建、不联网，在那里却被当作发布版本盖进 config.yml（或反过来），
// 开发构建盖下的 0.0.0 之类的值低于任何迁移边界，下次启动会把已迁移的 offset 再取反一遍。

import (
	"testing"

	"Metabox-Nexus-PlayerCap/config"
)

// 发布版本的盖章值是它的规范形式（normalizeSemver）；其余一律是同一个开发构建的盖章值。
//
// 变异自证（隔离的 worktree，实测）：config 包的判定去掉 0.0.0 的特判 →「0.0.0」「v0.0.0」红；本包的
// isReleaseVersion 去掉同一特判 → 同样两例红。
func TestStampVersionAgreesWithReleaseCheck(t *testing.T) {
	dev := config.StampVersion("")
	if dev == "" || dev == "v0.0.0" {
		t.Fatalf("开发构建的盖章值不应是 0.0.0：%q", dev)
	}
	for _, v := range []string{
		"4.0.0", "v4.0.0", "4.0.0-beta.3", "4.0.0-rc.1", "3.3.4", "10.20.30", " 4.0.0 ",
		"0.0.0", "v0.0.0", "", " ", "dev", "4.0.0.1", "v", "abc-20261009-153045-abc1234",
		"Metabox-Nexus-PlayerCap-20261009-153045-abc1234",
	} {
		got := config.StampVersion(v)
		want := dev
		if isReleaseVersion(v) {
			want = normalizeSemver(v)
		}
		if got != want {
			t.Errorf("config.StampVersion(%q) = %q，期望 %q（isReleaseVersion = %v）", v, got, want, isReleaseVersion(v))
		}
	}
}
