//go:build ignore

// sessionprobe 只读枚举所有活跃渲染端点上的音频会话（IAudioSessionManager2）。用于 issue #115 的真机取证：
// 播放器在共享 / 独占 / ASIO 下有没有活跃会话。
//
// 只读：不打开音频流、不改音量、不注册回调。
//
// 任何一步 COM 或进程快照调用失败，该拍标为「不完整」，不得据此判定某个会话缺席（失败不是阴性证据）。
//
// 用法：go run ./tools/sessionprobe/main.go [秒数] [间隔毫秒] [进程名子串] [v]
//   - 默认摘要模式：只在「活跃会话集合」变化时打印一行（外加 10 秒心跳），适合长时间观察。
//   - 进程名子串（不分大小写，匹配会话进程名或其进程树根）：把匹配的会话单独列出，其余 Active 会话列为「其他」。
//   - 第 4 个参数 v：每拍打印全部会话。
package main

import (
	"fmt"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"Metabox-Nexus-PlayerCap/audiocap/procscan"
)

//go:uintptrescapes
func comCall(obj unsafe.Pointer, idx int, args ...uintptr) uintptr {
	vtbl := *(**[64]uintptr)(obj)
	all := make([]uintptr, 0, len(args)+1)
	all = append(all, uintptr(obj))
	all = append(all, args...)
	ret, _, _ := syscall.SyscallN(vtbl[idx], all...)
	return ret
}

//go:uintptrescapes
func coCreate(clsid, iid, out uintptr) uintptr {
	r, _, _ := syscall.SyscallN(procCoCreateInstance.Addr(), clsid, 0, clsctxAll, iid, out)
	return r
}

const clsctxAll = 0x17

var procCoCreateInstance = windows.NewLazySystemDLL("ole32.dll").NewProc("CoCreateInstance")

func release(p unsafe.Pointer) {
	if p != nil {
		comCall(p, 2)
	}
}

func failed(hr uintptr) bool { return uint32(hr)&0x80000000 != 0 }

func mustGUID(s string) *windows.GUID {
	g, err := windows.GUIDFromString(s)
	if err != nil {
		panic(err)
	}
	return &g
}

var (
	clsidMMDeviceEnumerator = mustGUID("{BCDE0395-E52F-467C-8E3D-C4579291692E}")
	iidMMDeviceEnumerator   = mustGUID("{A95664D2-9614-4F35-A746-DE8DB63617E6}")
	iidSessionManager2      = mustGUID("{77AA99A0-1BD6-484F-8BC7-2C654C9A9B6F}")
	iidSessionControl2      = mustGUID("{BFB7FF88-7239-4FC9-8FA2-07C950BE9C6D}")
	iidMeterInformation     = mustGUID("{C02216F6-8C67-4B5B-9D00-D008E73E0064}")
)

type proc struct {
	ppid uint32
	exe  string
}

func snapshot() (map[uint32]proc, error) {
	list, err := procscan.Snapshot()
	if err != nil {
		return nil, err
	}
	m := make(map[uint32]proc, len(list))
	for _, p := range list {
		m[p.PID] = proc{ppid: p.PPID, exe: p.Name}
	}
	return m, nil
}

// root 沿父链上溯到最顶端仍存在的祖先，返回其 exe 名（对 Chromium 系即主进程）。
func root(m map[uint32]proc, pid uint32) string {
	cur := pid
	for i := 0; i < 16; i++ {
		p, ok := m[cur]
		if !ok {
			break
		}
		if _, ok := m[p.ppid]; !ok || p.ppid == 0 || p.ppid == cur {
			return p.exe
		}
		cur = p.ppid
	}
	return m[cur].exe
}

type sess struct {
	dev   int
	pid   uint32
	exe   string
	root  string
	state int32
	peak  float32
	sys   bool
}

func main() {
	secs, every, filter, verbose := 6, 1000, "", false
	if len(os.Args) > 1 {
		secs, _ = strconv.Atoi(os.Args[1])
	}
	if len(os.Args) > 2 {
		every, _ = strconv.Atoi(os.Args[2])
	}
	if len(os.Args) > 3 {
		filter = strings.ToLower(os.Args[3])
	}
	if len(os.Args) > 4 && os.Args[4] == "v" {
		verbose = true
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := windows.CoInitializeEx(0, windows.COINIT_MULTITHREADED); err != nil {
		fmt.Println("CoInitializeEx:", err)
		return
	}
	defer windows.CoUninitialize()

	var enum unsafe.Pointer
	if hr := coCreate(uintptr(unsafe.Pointer(clsidMMDeviceEnumerator)), uintptr(unsafe.Pointer(iidMMDeviceEnumerator)), uintptr(unsafe.Pointer(&enum))); failed(hr) {
		fmt.Printf("CoCreateInstance hr=%#x\n", uint32(hr))
		return
	}
	defer release(enum)

	start := time.Now()
	var last string
	var lastPrint time.Time
	for time.Since(start) < time.Duration(secs)*time.Second {
		t0 := time.Now()
		ts := time.Since(start).Seconds()
		procs, perr := snapshot()
		var list []sess
		var ndev uint32
		var problems []string
		if perr != nil {
			problems = append(problems, "进程快照失败: "+perr.Error())
		} else {
			list, ndev, problems = enumerate(enum, procs)
		}
		if verbose {
			fmt.Printf("t=%.1f devices=%d sessions=%d elapsed=%v 不完整=%v\n", ts, ndev, len(list), time.Since(t0), problems)
			for _, s := range list {
				fmt.Printf("    dev#%-2d pid=%-6d exe=%-24s root=%-24s state=%d peak=%.4f sys=%v\n", s.dev, s.pid, s.exe, s.root, s.state, s.peak, s.sys)
			}
		} else {
			sum := summarize(list, filter, problems)
			if sum != last || time.Since(lastPrint) > 10*time.Second {
				fmt.Printf("t=%5.1f devices=%d %s\n", ts, ndev, sum)
				last, lastPrint = sum, time.Now()
			}
		}
		time.Sleep(time.Duration(every) * time.Millisecond)
	}
}

// summarize 把一拍的结果压成一行：匹配 filter 的会话按「有无 Active、在哪个端点」列出，其余 Active 会话列为 other。
// 本拍不完整时，只能报告「已看到的 Active」，不得说「无 Active」。
func summarize(list []sess, filter string, problems []string) string {
	var mine, other []string
	present := 0
	seen := map[string]bool{}
	for _, s := range list {
		match := filter != "" && (strings.Contains(strings.ToLower(s.exe), filter) || strings.Contains(strings.ToLower(s.root), filter))
		if match {
			present++
		}
		if s.state != 1 || s.sys {
			continue
		}
		item := fmt.Sprintf("%s(pid=%d,dev#%d)", s.exe, s.pid, s.dev)
		if match {
			mine = append(mine, item)
		} else if !seen[s.exe] {
			seen[s.exe] = true
			other = append(other, s.exe)
		}
	}
	sort.Strings(mine)
	sort.Strings(other)
	out := ""
	if filter != "" {
		switch {
		case len(mine) > 0:
			out = fmt.Sprintf("[%s] ACTIVE: %s", filter, strings.Join(mine, " "))
		case len(problems) > 0:
			out = fmt.Sprintf("[%s] 本拍不完整，不能判定有无 Active", filter)
		default:
			out = fmt.Sprintf("[%s] 无 Active 会话（该进程树共有 %d 个会话，均非 Active）", filter, present)
		}
		out += " | "
	}
	if len(other) == 0 {
		out += "其他 Active: 无"
	} else {
		out += "其他 Active: " + strings.Join(other, ",")
	}
	if len(problems) > 0 {
		out += fmt.Sprintf(" | 不完整(%d): %s", len(problems), problems[0])
	}
	return out
}

// enumerate 枚举全部活跃渲染端点上的会话。problems 非空表示本拍不完整：其中任一步失败，返回的 list 不得用来证明某个会话缺席。
func enumerate(enum unsafe.Pointer, procs map[uint32]proc) (list []sess, ndev uint32, problems []string) {
	var coll unsafe.Pointer
	// IMMDeviceEnumerator::EnumAudioEndpoints(eRender=0, DEVICE_STATE_ACTIVE=1, &coll)
	if hr := comCall(enum, 3, 0, 1, uintptr(unsafe.Pointer(&coll))); failed(hr) {
		return nil, 0, []string{fmt.Sprintf("EnumAudioEndpoints hr=%#x", uint32(hr))}
	}
	defer release(coll)
	if hr := comCall(coll, 3, uintptr(unsafe.Pointer(&ndev))); failed(hr) { // IMMDeviceCollection::GetCount
		return nil, 0, []string{fmt.Sprintf("IMMDeviceCollection.GetCount hr=%#x", uint32(hr))}
	}
	bad := func(format string, a ...any) { problems = append(problems, fmt.Sprintf(format, a...)) }
	for di := uint32(0); di < ndev; di++ {
		var dev unsafe.Pointer
		if hr := comCall(coll, 4, uintptr(di), uintptr(unsafe.Pointer(&dev))); failed(hr) { // Item
			bad("dev#%d Item hr=%#x", di, uint32(hr))
			continue
		}
		var mgr unsafe.Pointer
		// IMMDevice::Activate(iid, CLSCTX_ALL, NULL, &mgr)
		if hr := comCall(dev, 3, uintptr(unsafe.Pointer(iidSessionManager2)), uintptr(clsctxAll), 0, uintptr(unsafe.Pointer(&mgr))); failed(hr) {
			bad("dev#%d Activate(SessionManager2) hr=%#x", di, uint32(hr))
			release(dev)
			continue
		}
		var senum unsafe.Pointer
		if hr := comCall(mgr, 5, uintptr(unsafe.Pointer(&senum))); failed(hr) { // GetSessionEnumerator
			bad("dev#%d GetSessionEnumerator hr=%#x", di, uint32(hr))
			release(mgr)
			release(dev)
			continue
		}
		var n int32
		if hr := comCall(senum, 3, uintptr(unsafe.Pointer(&n))); failed(hr) { // GetCount
			bad("dev#%d GetCount hr=%#x", di, uint32(hr))
			release(senum)
			release(mgr)
			release(dev)
			continue
		}
		for i := int32(0); i < n; i++ {
			var ctl unsafe.Pointer
			if hr := comCall(senum, 4, uintptr(i), uintptr(unsafe.Pointer(&ctl))); failed(hr) { // GetSession
				bad("dev#%d GetSession(%d) hr=%#x", di, i, uint32(hr))
				continue
			}
			var state int32
			if hr := comCall(ctl, 3, uintptr(unsafe.Pointer(&state))); failed(hr) { // IAudioSessionControl::GetState
				bad("dev#%d session#%d GetState hr=%#x", di, i, uint32(hr))
				release(ctl)
				continue
			}
			var ctl2 unsafe.Pointer
			var pid uint32
			sys := false
			if hr := comCall(ctl, 0, uintptr(unsafe.Pointer(iidSessionControl2)), uintptr(unsafe.Pointer(&ctl2))); failed(hr) {
				if state == 1 {
					bad("dev#%d session#%d(Active) QueryInterface(IAudioSessionControl2) hr=%#x", di, i, uint32(hr))
				}
			} else {
				if hr := comCall(ctl2, 14, uintptr(unsafe.Pointer(&pid))); failed(hr) && state == 1 { // GetProcessId
					bad("dev#%d session#%d(Active) GetProcessId hr=%#x", di, i, uint32(hr))
				}
				sys = comCall(ctl2, 15) == 0 // IsSystemSoundsSession: S_OK(0)=是
				release(ctl2)
			}
			var meter unsafe.Pointer
			var peak float32 = -1
			if hr := comCall(ctl, 0, uintptr(unsafe.Pointer(iidMeterInformation)), uintptr(unsafe.Pointer(&meter))); !failed(hr) {
				comCall(meter, 3, uintptr(unsafe.Pointer(&peak))) // GetPeakValue（峰值只是参考，失败保持 -1）
				release(meter)
			}
			list = append(list, sess{dev: int(di), pid: pid, exe: procs[pid].exe, root: root(procs, pid), state: state, peak: peak, sys: sys})
			release(ctl)
		}
		release(senum)
		release(mgr)
		release(dev)
	}
	return list, ndev, problems
}
