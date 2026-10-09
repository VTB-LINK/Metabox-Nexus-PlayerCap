//go:build windows

// Command audioprobe 是一个可行性 spike：验证「纯 Go（不引入 cgo）能否用 WASAPI 进程级 loopback
// 捕获指定进程的音频输出」。对目标 PID（可连子进程树）捕获 N 秒，周期打印帧数/是否静音/RMS，可选落一段
// 48k/2ch/float32 的 WAV 供人耳复核。COM 内核见 audiocap/wasapi；判断报告随交付返回，不入库。
//
// 只读本机音频引擎、仅本地调试；离线、不需网络。
//
// 用法：
//
//	audioprobe -pid 1234 [-seconds 5] [-wav out.wav] [-exclude-tree]
//	audioprobe -exe chrome.exe [-seconds 5] [-wav out.wav]
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"math"
	"os"
	"runtime"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"Metabox-Nexus-PlayerCap/audiocap/wasapi"
)

// WAVE_FORMAT_EXTENSIBLE 标签与 IEEE_FLOAT 子格式 GUID：仅供本工具写 WAV 头描述采集格式，
// 与 wasapi 包的采集内核无关（不引其未导出符号，避免为一个调试工具扩大包 API）。
const wavFormatExtensible = 0xFFFE

var wavSubtypeIEEEFloat = windows.GUID{Data1: 0x00000003, Data2: 0x0000, Data3: 0x0010, Data4: [8]byte{0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71}}

func main() {
	pid := flag.Uint("pid", 0, "目标进程 PID（必需，或用 -exe 按名解析）")
	exe := flag.String("exe", "", "按进程名解析 PID（如 chrome.exe），-pid 未给时生效")
	seconds := flag.Int("seconds", 5, "捕获时长（秒）")
	wavPath := flag.String("wav", "", "可选：把捕获落成 WAV 文件供人耳复核")
	excludeTree := flag.Bool("exclude-tree", false, "捕获除目标进程树之外的其余进程（默认捕获目标进程树）")
	flag.Parse()

	targetPID := uint32(*pid)
	if targetPID == 0 && *exe != "" {
		resolved, err := findPIDByName(*exe)
		if err != nil {
			fmt.Fprintln(os.Stderr, "解析进程名失败:", err)
			os.Exit(1)
		}
		targetPID = resolved
		fmt.Printf("按名 %q 解析到 PID %d\n", *exe, targetPID)
	}
	if targetPID == 0 {
		fmt.Fprintln(os.Stderr, "必须指定 -pid 或 -exe")
		flag.Usage()
		os.Exit(2)
	}
	if *seconds <= 0 {
		*seconds = 5
	}

	includeTree := !*excludeTree
	fmt.Printf("目标 PID=%d  含子进程树=%v  时长=%ds\n", targetPID, includeTree, *seconds)

	if err := capture(targetPID, includeTree, *seconds, *wavPath); err != nil {
		fmt.Fprintln(os.Stderr, "捕获失败:", err)
		os.Exit(1)
	}
}

// capture 跑完整的激活 → 采集流程。COM 有 apartment 线程亲和，须锁定 OS 线程；激活/初始化/启动的 COM
// 内核已抽进 audiocap/wasapi，本函数只管线程与 COM 生命周期 + 采集展示（RMS/WAV）。
func capture(pid uint32, includeTree bool, seconds int, wavPath string) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// COINIT_MULTITHREADED；S_FALSE(0x1) 表示本线程已初始化过，视作成功。
	if err := windows.CoInitializeEx(0, windows.COINIT_MULTITHREADED); err != nil {
		if errno, ok := err.(syscall.Errno); !ok || uintptr(errno) != 0x1 {
			return fmt.Errorf("CoInitializeEx 失败: %w", err)
		}
	}
	defer windows.CoUninitialize()

	c, err := wasapi.Activate(pid, includeTree)
	if err != nil {
		return err
	}
	defer c.Close()
	fmt.Printf("激活成功：开始采集 %dHz/%dch/float32\n", wasapi.SampleRate, wasapi.Channels)

	// WAV 仅在 -wav 指定时缓冲（有界内存，用户显式选择）。
	var wavBuf []float32
	total, silent := drainLoop(c, seconds, func(interleaved []float32) {
		if wavPath != "" {
			wavBuf = append(wavBuf, interleaved...)
		}
	})

	dur := float64(total) / float64(wasapi.SampleRate)
	fmt.Printf("\n结束：总帧数 %d  静音帧 %d  实际时长 %.2fs  格式 %dHz/%dch/float32\n",
		total, silent, dur, wasapi.SampleRate, wasapi.Channels)

	if wavPath != "" {
		if err := writeFloatWAV(wavPath, wavBuf, wasapi.SampleRate, wasapi.Channels); err != nil {
			return fmt.Errorf("写 WAV 失败: %w", err)
		}
		fmt.Printf("已写 WAV: %s（%d 帧）\n", wavPath, len(wavBuf)/wasapi.Channels)
	}
	return nil
}

// drainLoop 采集 seconds 秒：循环 wasapi.Read（每次一个 packet，内部等事件、超时也返回），每 ~200ms
// 打印一段统计。返回总帧数与静音帧数。
func drainLoop(c *wasapi.Capturer, seconds int, sink func([]float32)) (total, silentTotal uint64) {
	start := time.Now()
	deadline := start.Add(time.Duration(seconds) * time.Second)
	nextPrint := start.Add(200 * time.Millisecond)

	buf := make([]float32, wasapi.SampleRate*wasapi.Channels/10) // 100ms 容量，足够单个引擎周期
	var segFrames uint64
	var segSumSq float64
	var segSilent bool

	for time.Now().Before(deadline) {
		frames, silent, err := c.Read(buf)
		if err != nil {
			fmt.Fprintln(os.Stderr, "读取失败:", err)
			break
		}
		if frames > 0 {
			n := frames * wasapi.Channels
			if silent {
				segSilent = true
				silentTotal += uint64(frames)
			} else {
				for _, s := range buf[:n] {
					segSumSq += float64(s) * float64(s)
				}
			}
			// 拷一份给 sink（buf 下次 Read 会复写）
			sink(append([]float32(nil), buf[:n]...))
			segFrames += uint64(frames)
			total += uint64(frames)
		}

		if now := time.Now(); now.After(nextPrint) {
			rms := 0.0
			if cnt := segFrames * uint64(wasapi.Channels); cnt > 0 {
				rms = math.Sqrt(segSumSq / float64(cnt))
			}
			fmt.Printf("[%5.1fs] 帧=%-6d SILENT=%-5v RMS=%.5f\n",
				now.Sub(start).Seconds(), segFrames, segSilent, rms)
			segFrames, segSumSq, segSilent = 0, 0, false
			nextPrint = now.Add(200 * time.Millisecond)
		}
	}
	return total, silentTotal
}

// findPIDByName 用 Toolhelp 快照按进程名（不分大小写）找第一个匹配的 PID。
func findPIDByName(name string) (uint32, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return 0, fmt.Errorf("CreateToolhelp32Snapshot 失败: %w", err)
	}
	defer windows.CloseHandle(snap)

	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	if err := windows.Process32First(snap, &entry); err != nil {
		return 0, fmt.Errorf("Process32First 失败: %w", err)
	}
	want := toLowerASCII(name)
	for {
		exeName := windows.UTF16ToString(entry.ExeFile[:])
		if toLowerASCII(exeName) == want {
			return entry.ProcessID, nil
		}
		if err := windows.Process32Next(snap, &entry); err != nil {
			break
		}
	}
	return 0, fmt.Errorf("未找到进程: %s", name)
}

func toLowerASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// writeFloatWAV 落一个 WAVE_FORMAT_EXTENSIBLE / IEEE_FLOAT 的 WAV（含 fact 块，符合非 PCM 规范）。
func writeFloatWAV(path string, samples []float32, sampleRate, channels int) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	bitsPerSample := 32
	blockAlign := channels * bitsPerSample / 8
	byteRate := sampleRate * blockAlign
	dataBytes := len(samples) * 4
	numFrames := 0
	if channels > 0 {
		numFrames = len(samples) / channels
	}

	// fmt(cbSize=22)=40 字节子块内容；fact=4；RIFF 剩余 = 4("WAVE") +8+40 +8+4 +8+dataBytes
	riffSize := 4 + (8 + 40) + (8 + 4) + (8 + dataBytes)
	le := binary.LittleEndian
	var hdr []byte
	put16 := func(v uint16) { b := make([]byte, 2); le.PutUint16(b, v); hdr = append(hdr, b...) }
	put32 := func(v uint32) { b := make([]byte, 4); le.PutUint32(b, v); hdr = append(hdr, b...) }
	putStr := func(s string) { hdr = append(hdr, s...) }

	putStr("RIFF")
	put32(uint32(riffSize))
	putStr("WAVE")

	putStr("fmt ")
	put32(40) // 子块大小
	put16(wavFormatExtensible)
	put16(uint16(channels))
	put32(uint32(sampleRate))
	put32(uint32(byteRate))
	put16(uint16(blockAlign))
	put16(uint16(bitsPerSample))
	put16(22)                    // cbSize
	put16(uint16(bitsPerSample)) // wValidBitsPerSample
	put32(0x3)                   // dwChannelMask = 立体声
	// SubFormat GUID = KSDATAFORMAT_SUBTYPE_IEEE_FLOAT，按 GUID 二进制布局写
	put32(wavSubtypeIEEEFloat.Data1)
	put16(wavSubtypeIEEEFloat.Data2)
	put16(wavSubtypeIEEEFloat.Data3)
	hdr = append(hdr, wavSubtypeIEEEFloat.Data4[:]...)

	putStr("fact")
	put32(4)
	put32(uint32(numFrames))

	putStr("data")
	put32(uint32(dataBytes))

	if _, err := f.Write(hdr); err != nil {
		return err
	}
	// 样本：float32 小端
	buf := make([]byte, dataBytes)
	for i, s := range samples {
		le.PutUint32(buf[i*4:], math.Float32bits(s))
	}
	_, err = f.Write(buf)
	return err
}
