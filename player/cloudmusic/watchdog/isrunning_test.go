package watchdog

// 本文件只测 IsRunning：按名字查进程快照，有进程或快照出错都算在运行。

import (
	"errors"
	"testing"

	"Metabox-Nexus-PlayerCap/player/launch"
)

// TestIsRunning：快照里有 cloudmusic.exe（读不到映像路径的也算）即在运行；没有即不在运行；快照出错按在运行处理，
// 外层循环不因一次枚举失败改报未运行。
//
// 变异自证（均实测变红）：快照出错按不在运行处理（「快照出错」）；按别的映像名查快照（全部子例）；读不到映像路径的
// 进程不算（「只有一个读不到路径的」）。
func TestIsRunning(t *testing.T) {
	cases := []struct {
		name    string
		running []launch.Proc
		err     error
		want    bool
	}{
		{"没有进程", nil, nil, false},
		{"一个官网实例", []launch.Proc{{PID: noPID1, ExePath: `C:\Program Files\NetEase\CloudMusic\cloudmusic.exe`}}, nil, true},
		{"只有一个读不到路径的", []launch.Proc{{PID: noPID2}}, nil, true},
		{"多个实例", []launch.Proc{{PID: noPID1, ExePath: storeExe}, {PID: noPID2}}, nil, true},
		{"快照出错", nil, errors.New("snapshot failed"), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeNetEase{}
			asked := f.installRunning(t, c.running, c.err)
			if got := IsRunning(); got != c.want {
				t.Errorf("IsRunning() = %v，期望 %v", got, c.want)
			}
			if *asked != TargetProcessName {
				t.Errorf("快照按 %q 查，期望 %q", *asked, TargetProcessName)
			}
			if f.kills != 0 || len(f.starts) != 0 {
				t.Errorf("kills=%d starts=%v，判进程不得结束或拉起", f.kills, f.starts)
			}
		})
	}
}
