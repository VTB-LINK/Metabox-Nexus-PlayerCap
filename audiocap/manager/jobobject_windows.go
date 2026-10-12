//go:build windows

package manager

import (
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
	// 转换须就地写在实参里，见 setJobInformation。
	err = setJobInformation(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
	if err != nil {
		windows.CloseHandle(job)
		return 0, err
	}
	return job, nil
}

// setJobInformation 调 windows.SetInformationJobObject。
//
// uintptrescapes：x/sys 的 SetInformationJobObject 是不带该指令的 Go 函数，直接交给它的
// uintptr(unsafe.Pointer(&x)) 不会让 x 上堆；转换与系统调用之间若 goroutine 栈增长，栈被整体搬走，
// 已转成整数的地址不随之修正，内核读到旧栈上的内容。本指令使调用点就地转换的 x 逃逸到堆并保活至本调用
// 返回；先存进变量再传则不受保护。由 jobobject_lint_test.go 守。
//
//go:uintptrescapes
func setJobInformation(job windows.Handle, class uint32, info uintptr, size uint32) error {
	_, err := windows.SetInformationJobObject(job, class, info, size)
	return err
}

// assignToJob 把进程句柄 proc 赋入 job。proc 须带 PROCESS_SET_QUOTA | PROCESS_TERMINATE 权限。
func assignToJob(job windows.Handle, proc windows.Handle) error {
	return windows.AssignProcessToJobObject(job, proc)
}
