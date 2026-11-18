package manager

import "testing"

// TestProcessLoopbackSupported 钉住进程级 loopback 的系统版本门槛：Windows 10 build 19041（2004）及以上为真。
// 反例覆盖门槛前一号、issue #206 的现场版本，以及只看 build 不看 major、只看 major 不看 build 两种写错的判据。
func TestProcessLoopbackSupported(t *testing.T) {
	for _, c := range []struct {
		name         string
		major, build uint32
		want         bool
	}{
		{"Win10 1909（issue #206 现场）", 10, 18363, false},
		{"门槛前一号", 10, 19040, false},
		{"Win10 1507", 10, 10240, false},
		{"门槛 2004", 10, 19041, true},
		{"Win10 22H2", 10, 19045, true},
		{"Win11 23H2", 10, 22631, true},
		{"major 大于 10", 11, 0, true},
		{"Win8.1", 6, 9600, false},
		{"major 小于 10 而 build 过门槛", 6, 26100, false},
		{"零值", 0, 0, false},
	} {
		if got := processLoopbackSupported(c.major, c.build); got != c.want {
			t.Errorf("%s: processLoopbackSupported(%d, %d) = %v, 期望 %v", c.name, c.major, c.build, got, c.want)
		}
	}
}
