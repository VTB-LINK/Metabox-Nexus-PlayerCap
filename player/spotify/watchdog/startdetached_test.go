package watchdog

// 本文件只测 startFn 的生产实现 startDetached：经 launch.Detached 拉起 Spotify（issue #127），退回直接启动
// （detached=false、err=nil）时打一条 Warn，出错时原样返回错误、不打这条 Warn。launch.Detached 经包级变量 detachFn
// 替换，不启动任何进程。

import (
	"bytes"
	"errors"
	stdlog "log"
	"slices"
	"strings"
	"testing"
)

// TestStartDetachedWarnsOnFallback
//
// 防的缺陷与变异（均实测变红）：去掉 Warn（退回直接启动时没有任何痕迹，主播关 OBS 时 Spotify 被一并结束却查不到原因）；
// 不看 err 一律在 detached=false 时 Warn（启动失败时多打一条误导的「已直接启动」）；detached=true 也 Warn；
// 不把参数原样交给 Detached；吞掉 Detached 的错误。
func TestStartDetachedWarnsOnFallback(t *testing.T) {
	boom := errors.New("boom")
	cases := []struct {
		name     string
		detached bool
		err      error
		wantWarn bool
	}{
		{"经 cmd.exe 中转成功", true, nil, false},
		{"退回直接启动", false, nil, true},
		{"启动失败", false, boom, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var gotExe, gotDir string
			var gotArgs []string
			old := detachFn
			detachFn = func(exe string, args []string, dir string) (bool, error) {
				gotExe, gotArgs, gotDir = exe, args, dir
				return c.detached, c.err
			}
			t.Cleanup(func() { detachFn = old })

			var buf bytes.Buffer
			w := stdlog.Writer()
			stdlog.SetOutput(&buf)
			t.Cleanup(func() { stdlog.SetOutput(w) })

			err := startDetached(spotifyExe, LaunchFlags)
			if !errors.Is(err, c.err) || (c.err == nil) != (err == nil) {
				t.Errorf("startDetached = %v，期望 %v", err, c.err)
			}
			if gotExe != spotifyExe || !slices.Equal(gotArgs, LaunchFlags) || gotDir != "" {
				t.Errorf("交给 Detached 的是 (%q, %q, %q)，期望 (%q, LaunchFlags, \"\")", gotExe, gotArgs, gotDir, spotifyExe)
			}
			out := buf.String()
			warned := strings.Contains(out, "[!]") && strings.Contains(out, TargetProcessName)
			if warned != c.wantWarn {
				t.Errorf("打了 Warn = %v，期望 %v；日志：%q", warned, c.wantWarn, out)
			}
		})
	}
}
