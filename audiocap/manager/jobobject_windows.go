//go:build windows

package manager

import (
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

// createKillOnCloseJob 创建一个 KILL_ON_JOB_CLOSE 的 Job Object。语义：只要这个 job 句柄的
// 最后一个引用被关闭，内核就连带回收 job 内的所有进程——**父进程死亡时内核自动关闭其全部句柄**，
// 于是被赋入此 job 的 helper 必被内核杀掉，不成孤儿（防孤儿的内核级主防线，§6）。
//
// 返回的句柄由调用方持有并负责关闭（关闭即触发 KILL_ON_JOB_CLOSE 强杀 helper）。
func createKillOnCloseJob() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	_, err = windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	)
	// info 的地址以 uintptr 传入上面的调用，KeepAlive 确保它在调用返回前不被 GC 回收/移动
	//（照本仓 helper_windows.go QPC 处的 uintptr(unsafe.Pointer(&x)) + runtime.KeepAlive 范式）。
	runtime.KeepAlive(&info)
	if err != nil {
		windows.CloseHandle(job)
		return 0, err
	}
	return job, nil
}

// assignToJob 把进程句柄 proc 赋入 job。proc 须带 PROCESS_SET_QUOTA | PROCESS_TERMINATE 权限。
func assignToJob(job windows.Handle, proc windows.Handle) error {
	return windows.AssignProcessToJobObject(job, proc)
}
