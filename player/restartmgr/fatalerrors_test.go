package restartmgr

// 本文件只测 IsFatal：哪些 Restart Manager 错误使调用方本会话不再查询。

import (
	"errors"
	"syscall"
	"testing"

	"Metabox-Nexus-PlayerCap/i18n"
	"golang.org/x/sys/windows"
)

func TestIsFatal(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"ERROR_SEM_TIMEOUT", syscall.Errno(121), true},
		{"ERROR_WRITE_FAULT", syscall.Errno(29), true},
		{"ERROR_MAX_SESSIONS_REACHED", syscall.Errno(353), true},
		{"%w 包装的 ERROR_SEM_TIMEOUT", i18n.Errorf("RmStartSession failed: %w", windows.ERROR_SEM_TIMEOUT), true},
		{"两层包装的 ERROR_MAX_SESSIONS_REACHED", i18n.Errorf("query: %w", i18n.Errorf("RmStartSession failed: %w", windows.ERROR_MAX_SESSIONS_REACHED)), true},
		{"nil", nil, false},
		{"ERROR_ACCESS_DENIED", windows.ERROR_ACCESS_DENIED, false},
		{"%w 包装的 ERROR_ACCESS_DENIED", i18n.Errorf("RmGetList failed: %w", windows.ERROR_ACCESS_DENIED), false},
		{"ERROR_MORE_DATA", windows.ERROR_MORE_DATA, false},
		{"同号文字但未以 %w 包装", errors.New(syscall.Errno(121).Error()), false},
		{"%v 格式化的 ERROR_SEM_TIMEOUT", i18n.Errorf("RmStartSession failed: %v", windows.ERROR_SEM_TIMEOUT), false},
	}
	for _, c := range cases {
		if got := IsFatal(c.err); got != c.want {
			t.Errorf("%s：IsFatal = %v，want %v", c.name, got, c.want)
		}
	}
}
