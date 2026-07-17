package watchdog

// 本文件只测 10.x 版本黑名单：orig 字节指纹之外的一道确定性负向防线。
// 它曾被误删（详见函数注释），别再删。

import "testing"

// TestIsUnsupportedMajor 守住 10.x 黑名单。
//
// 这道确定性负向防线由 80e9182「不对10.x版本做patch」引入，是实际遭遇过 10.x 后加的。
// 它一度在改用 orig 字节指纹时被误删（commit message 错称它「不设防」，实则它 return），
// 后经 git 溯源确认是有意设计而恢复。别再删——orig 指纹通常能识别 10.x，但它是概率性的
// （依赖 9 处字节不落进 20.x {orig,data}），而本黑名单是确定性的，且读的是 exe 版本这个
// 独立于 libcef 字节的证据源。
func TestIsUnsupportedMajor(t *testing.T) {
	unsupported := []string{"10.1.94.25498", "10.0.0.0", "10."}
	for _, v := range unsupported {
		if !isUnsupportedMajor(v) {
			t.Errorf("%q 属 10.x 系列，应判为不受支持", v)
		}
	}
	supported := []string{"20.1.22.27795", "21.0.0.0", "2.0.0.0", "", "100.0.0.0"}
	for _, v := range supported {
		if isUnsupportedMajor(v) {
			t.Errorf("%q 不应判为不受支持（黑名单只针对 10.x 大版本）", v)
		}
	}
}
