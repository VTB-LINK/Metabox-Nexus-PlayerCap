//go:build windows

package manager

// 本文件只测一件事：createKillOnCloseJob 建出的 Job 带 JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE（issue #104）。

import (
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// 读回走 Proc.Call（带 //go:uintptrescapes）：x/sys 的 QueryInformationJobObject 同样不带该指令，
// 就地转换的缓冲区地址会依赖栈位置。
var procQueryInformationJobObject = windows.NewLazySystemDLL("kernel32.dll").NewProc("QueryInformationJobObject")

// TestCreateKillOnCloseJobSetsLimit 读回 Job 的扩展限制信息，断言 LimitFlags 含 KILL_ON_JOB_CLOSE。
// 该标志没设上时，关闭 Job 句柄不会回收 helper，主服务异常退出后 helper 成孤儿，且建 Job 不报错。
func TestCreateKillOnCloseJobSetsLimit(t *testing.T) {
	job, err := createKillOnCloseJob()
	if err != nil {
		t.Fatalf("createKillOnCloseJob: %v", err)
	}
	defer windows.CloseHandle(job)

	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	r, _, e := procQueryInformationJobObject.Call(uintptr(job), windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info), 0)
	if r == 0 {
		t.Fatalf("QueryInformationJobObject: %v", e)
	}
	if got := info.BasicLimitInformation.LimitFlags; got&windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE == 0 {
		t.Fatalf("LimitFlags = %#x，缺 JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE", got)
	}
}
