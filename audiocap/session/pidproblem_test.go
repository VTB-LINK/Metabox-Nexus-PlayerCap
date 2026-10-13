package session

import "testing"

// 本文件只测 GetProcessId 的 HRESULT 能否用来归属 Active 会话（Codex 审查 R2）：跨进程会话返回成功码
// AUDCLNT_S_NO_SINGLE_PROCESS，PID 只是最初创建者，当前出声的流可能属于树外的进程；把它当可信 PID 会给目标
// 发布错误的 (1,1)。

// 变异自证（实测）：pidProblem 不识别 hrNoSingleProcess → 「跨进程会话」红；去掉对系统声音会话的豁免 →
// 「系统声音会话」红；把任何非零成功码都当问题 → 「S_FALSE」红。
func TestPidProblem(t *testing.T) {
	for _, c := range []struct {
		name    string
		hr      uint32
		system  bool
		wantErr bool
	}{
		{"S_OK", 0, false, false},
		{"S_FALSE 按成功处理", 1, false, false},
		{"E_POINTER", 0x80004003, false, true},
		{"AUDCLNT_E_DEVICE_INVALIDATED", 0x88890004, false, true},
		{"跨进程会话", hrNoSingleProcess, false, true},
		{"系统声音会话", hrNoSingleProcess, true, false},
		{"系统声音会话但调用失败", 0x88890004, true, true},
	} {
		if err := pidProblem(c.hr, c.system); (err != nil) != c.wantErr {
			t.Errorf("%s: pidProblem(%#x, system=%v) = %v, 期望出错=%v", c.name, c.hr, c.system, err, c.wantErr)
		}
	}
}

// 经 scan 与 decide：目标树外的进程与目标用同一 GUID 建的跨进程会话，PID 是目标（最初创建者）、状态 Active：
// 不得给目标发布 (1,1)，也不得发布负结果（本拍不完整，槽位保持从未发布）。
//
// 变异自证（实测）：scan 忽略 pidErr（把它当 PID 可信）→ 目标被发布 (1,1)，红。COM 层 sessionAt 里真正调用 pidProblem
// 的那一行离线够不到，由 pidwiring_windows_test.go 的源码扫描钉住。
func TestCrossProcessSessionIsNotAttributed(t *testing.T) {
	cross := sessionInfo{active: true, pid: target, pidErr: pidProblem(hrNoSingleProcess, false)}
	if f := probeOnce(t, []endpoint{{sessions: []sessionInfo{cross}}}); f != 0 {
		t.Fatalf("跨进程会话: flags = %#x, 期望 0（不能归属，本拍不完整）", f)
	}
	// 另有确切的正例时仍可发布。
	if f := probeOnce(t, []endpoint{{sessions: []sessionInfo{cross, active(audioKid)}}}); f != both {
		t.Fatalf("跨进程会话之外另有目标子进程的 Active 会话: flags = %#x, 期望 %#x", f, both)
	}
}
