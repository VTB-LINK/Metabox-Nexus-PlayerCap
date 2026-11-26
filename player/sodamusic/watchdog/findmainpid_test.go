package watchdog

// 本文件测 FindMainPID 的三种结论（#231 代码审）：没有 SodaMusic.exe 为 ErrNotRunning；有而主进程的命令行读不到为
// ErrMainUnreadable（进程在运行，上层报 standby / no_admin，不当作未运行）；认得出主进程即返回它的 pid。枚举与读命令行
// 经包级变量换成桩，不碰真进程。

import (
	"errors"
	"testing"

	"Metabox-Nexus-PlayerCap/player/launch"
)

// TestFindMainPID
//
// 变异自证（均实测变红）：读不到命令行的候选照旧跳过、最后一律报 ErrNotRunning（「主进程的命令行读不到」「全都读不到」两例）；
// 读不到即报 ErrMainUnreadable、不再看后面的候选（「子进程读不到、主进程读得到」一例）；枚举出错报 ErrNotRunning（「枚举出错」一例）。
func TestFindMainPID(t *testing.T) {
	errDenied := errors.New("合成的拒绝访问")
	main, child := "\"C:\\合成\\SodaMusic.exe\"", "\"C:\\合成\\SodaMusic.exe\" --type=renderer"
	cases := []struct {
		name    string
		procs   []launch.Proc
		listErr error
		cmd     map[uint32]string // 缺的 pid 读命令行出错
		wantPID int
		wantErr error // nil 即不应出错；errAny 即任意不是 ErrNotRunning、ErrMainUnreadable 的错误
	}{
		{name: "没有进程", wantErr: ErrNotRunning},
		{name: "主进程读得到", procs: []launch.Proc{{PID: 10}, {PID: 11}}, cmd: map[uint32]string{10: child, 11: main}, wantPID: 11},
		{name: "子进程读不到、主进程读得到", procs: []launch.Proc{{PID: 10}, {PID: 11}}, cmd: map[uint32]string{11: main}, wantPID: 11},
		{name: "主进程的命令行读不到", procs: []launch.Proc{{PID: 10}, {PID: 11}}, cmd: map[uint32]string{10: child}, wantErr: ErrMainUnreadable},
		{name: "全都读不到", procs: []launch.Proc{{PID: 10}, {PID: 11}}, wantErr: ErrMainUnreadable},
		{name: "都读得到、都是子进程（主进程已退出）", procs: []launch.Proc{{PID: 10}}, cmd: map[uint32]string{10: child}, wantErr: ErrNotRunning},
		{name: "枚举出错", listErr: errors.New("合成的快照失败"), wantErr: errAny},
	}
	defer func(r func(string) ([]launch.Proc, error), c func(uint32) (string, error)) {
		runningFn, cmdlineFn = r, c
	}(runningFn, cmdlineFn)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			runningFn = func(name string) ([]launch.Proc, error) {
				if name != TargetProcessName {
					t.Errorf("按 %q 枚举，期望 %q", name, TargetProcessName)
				}
				return c.procs, c.listErr
			}
			cmdlineFn = func(pid uint32) (string, error) {
				if s, ok := c.cmd[pid]; ok {
					return s, nil
				}
				return "", errDenied
			}
			pid, err := FindMainPID()
			switch {
			case c.wantErr == errAny:
				if err == nil || errors.Is(err, ErrNotRunning) || errors.Is(err, ErrMainUnreadable) {
					t.Errorf("错误 %v，期望枚举出错原样返回（不是未运行，也不是读不到）", err)
				}
			case c.wantErr != nil:
				if !errors.Is(err, c.wantErr) {
					t.Errorf("错误 %v，期望 %v", err, c.wantErr)
				}
			case err != nil || pid != c.wantPID:
				t.Errorf("得到 pid %d、错误 %v，期望 pid %d", pid, err, c.wantPID)
			}
		})
	}
}

// errAny 是 TestFindMainPID 里「任意别的错误」的占位。
var errAny = errors.New("任意别的错误")
