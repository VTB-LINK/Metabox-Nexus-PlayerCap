//go:build ignore

// loopcheck 对指定 PID（含子进程树）开一路进程 loopback 捕获，统计样本是否非零。用于 issue #115 的真机取证：
// 目标进程在独占模式下输出时，进程 loopback 能不能捕到声音。
//
// 只开捕获侧（进程 loopback 虚拟设备），不触碰任何物理设备；时长 1..15 秒（RME 蓝屏先例：短跑）。
//
// 用法：go run ./tools/loopcheck/main.go <PID> [秒数，默认 5]
package main

import (
	"fmt"
	"math"
	"os"
	"runtime"
	"strconv"
	"time"

	"golang.org/x/sys/windows"

	"Metabox-Nexus-PlayerCap/audiocap/wasapi"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("用法: loopcheck <PID> [秒数]")
		os.Exit(2)
	}
	pid64, err := strconv.ParseUint(os.Args[1], 10, 32)
	if err != nil {
		fmt.Println("PID 非法:", err)
		os.Exit(2)
	}
	secs := 5
	if len(os.Args) > 2 {
		secs, _ = strconv.Atoi(os.Args[2])
	}
	if secs < 1 || secs > 15 {
		fmt.Println("秒数须在 1..15")
		os.Exit(2)
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := windows.CoInitializeEx(0, windows.COINIT_MULTITHREADED); err != nil {
		fmt.Println("CoInitializeEx:", err)
		return
	}
	defer windows.CoUninitialize()

	c, err := wasapi.Activate(uint32(pid64), true)
	if err != nil {
		fmt.Println("Activate 失败:", err)
		return
	}
	defer c.Close()

	buf := make([]float32, wasapi.SampleRate*wasapi.Channels/10)
	var packets, silentFlag, nonzero int
	var maxAbs float64
	deadline := time.Now().Add(time.Duration(secs) * time.Second)
	for time.Now().Before(deadline) {
		n, silent, err := c.Read(buf)
		if err != nil {
			fmt.Println("Read 出错:", err)
			break
		}
		if n == 0 {
			continue
		}
		packets++
		if silent {
			silentFlag++
		}
		nz := false
		for _, v := range buf[:n*wasapi.Channels] {
			if a := math.Abs(float64(v)); a > 0 {
				nz = true
				if a > maxAbs {
					maxAbs = a
				}
			}
		}
		if nz {
			nonzero++
		}
	}
	fmt.Printf("pid=%d %ds：packets=%d 带 SILENT 标志=%d 含非零样本=%d 最大绝对值=%.6f\n", pid64, secs, packets, silentFlag, nonzero, maxAbs)
}
