package watchdog

// 网易云 v2/v3 的识别。
//
// **只有 exe 文件版本号能区分这两个版本 —— 别再找别的判据了，实测都塌了。**
// 2026-07-16 拿真实的 v2（2.10.13.202675）与 v3（3.1.35.205293）逐项对比：
//
//	探针                      v2      v3
//	libcef.dll                HIT     HIT   ← 两版都是 CEF，「有没有 CEF」区分不了
//	chrome_elf.dll            HIT     HIT
//	v8_context_snapshot.bin   HIT     HIT
//	icudtl.dat / resources.pak / snapshot_blob.bin / libEGL.dll   HIT  HIT
//	resources\app.asar        miss    miss  ← 两版都不是 Electron
//	node.dll                  miss    miss
//	FileDescription           NetEase Cloud Music（**完全一致**）
//	CompanyName               NetEase（**完全一致**）
//	FileVersion               **2**.10.13.202675   **3**.1.35.205293   ← 唯一的差别
//
// 目录结构也几乎一模一样（locales / package / redist_packages / resource / swiftshader /
// avcodec-58.dll …）。曾以为「v2 没有 CEF 所以没有 CDP，这是因果不是相关」—— **实测证伪**：
// v2 也是 CEF，它只是不开 remote debugging（且页面结构与 v3 的 Redux 那套不同）。
//
// 顺带：连第三方魔改版（文件名带 Modified、目录里有 .bat 和推广 url）都保留了正确的版本
// 资源，说明这条判据比预想的耐操。

import (
	"fmt"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// v3MinMajor 支持 CDP 取词的最低主版本号。v2.x 的 CEF 不开 remote debugging，
// 连不上 9222，整条取词/特效链路都用不了。
const v3MinMajor = 3

// UnsupportedVersionError 表示检测到不支持 CDP 的网易云版本（v2.x 及更早）。
// 用具体类型而不是哨兵字符串：调用方要能把它与真正的 watchdog 故障区分开 ——
// 前者是「这台机器上本功能就是用不了」的终态，后者是可重试的瞬时错误。
type UnsupportedVersionError struct {
	Version string // 读到的完整版本号，如 "2.10.13.202675"
	ExePath string
}

func (e *UnsupportedVersionError) Error() string {
	return fmt.Sprintf("网易云音乐 v%s 不支持 CDP（需 v%d 及以上）", e.Version, v3MinMajor)
}

var (
	procGetFileVersionInfoSizeW = windows.NewLazySystemDLL("version.dll").NewProc("GetFileVersionInfoSizeW")
	procGetFileVersionInfoW     = windows.NewLazySystemDLL("version.dll").NewProc("GetFileVersionInfoW")
	procVerQueryValueW          = windows.NewLazySystemDLL("version.dll").NewProc("VerQueryValueW")
)

// vsFixedFileInfo 对应 VS_FIXEDFILEINFO（只取用得上的字段）。
type vsFixedFileInfo struct {
	Signature        uint32
	StrucVersion     uint32
	FileVersionMS    uint32
	FileVersionLS    uint32
	ProductVersionMS uint32
	ProductVersionLS uint32
}

// ReadExeVersion 读 exe 的文件版本号，形如 "3.1.35.205293"。读不到返回空串。
//
// 与 kugou/watchdog、qqmusic 的同名实现是复制关系而非共用：跨播放器 import 会引入包耦合，
// 本仓库已有判例否决（见 doc 的 §7b 处置）。复制时**必须连同下面两处防护一起带走**，
// 它们都是本仓库踩出来的：
//   - `uintptr → Pointer` 的 UAF：VerQueryValue 返回的 pInfo 指向 buf 内部，但 uintptr
//     不持有引用，直接转 Pointer 会让 GC 有机会在解引用前回收 buf。改为算偏移再回 buf
//     取地址（顺带做了越界校验）。
//   - Signature 校验：VS_FIXEDFILEINFO 首字段恒为 0xFEEF04BD。不校验的话，偏移一错就会把
//     任意 4 组字节格式化成一个**看着合法的垃圾版本号**，上层无从分辨 —— 而空串至少是个
//     明确信号。
func ReadExeVersion(exePath string) string {
	pathW, err := windows.UTF16PtrFromString(exePath)
	if err != nil {
		return ""
	}
	size, _, _ := procGetFileVersionInfoSizeW.Call(uintptr(unsafe.Pointer(pathW)), 0)
	if size == 0 {
		return ""
	}
	buf := make([]byte, size)
	ret, _, _ := procGetFileVersionInfoW.Call(
		uintptr(unsafe.Pointer(pathW)), 0,
		uintptr(len(buf)), uintptr(unsafe.Pointer(&buf[0])),
	)
	if ret == 0 {
		return ""
	}
	subBlock, _ := windows.UTF16PtrFromString(`\`)
	var pInfo uintptr
	var infoLen uint32
	ret, _, _ = procVerQueryValueW.Call(
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(subBlock)),
		uintptr(unsafe.Pointer(&pInfo)),
		uintptr(unsafe.Pointer(&infoLen)),
	)
	if ret == 0 || pInfo == 0 {
		return ""
	}
	base := uintptr(unsafe.Pointer(&buf[0]))
	if pInfo < base {
		return ""
	}
	off := pInfo - base
	if off+unsafe.Sizeof(vsFixedFileInfo{}) > uintptr(len(buf)) {
		return ""
	}
	fi := (*vsFixedFileInfo)(unsafe.Pointer(&buf[off]))
	if fi.Signature != 0xFEEF04BD {
		return ""
	}
	return fmt.Sprintf("%d.%d.%d.%d",
		fi.FileVersionMS>>16, fi.FileVersionMS&0xFFFF,
		fi.FileVersionLS>>16, fi.FileVersionLS&0xFFFF,
	)
}

// IsUnsupportedVersion 判断这个版本号是不是不支持 CDP 的老版本（v2.x 及更早）。
//
// **拿不准一律返回 false（按 v3 走）**，这个方向是硬要求：
//   - v2 被当成 v3 → 维持现状（连不上 9222，会重试）—— 就是今天的样子，不更坏。
//   - v3 被当成 v2 → 我们主动禁用 → **主播的歌词与特效镜像静默消失**，而且是我们干的。
//
// 所以版本号读不到（权限不足 / 文件被占 / 无版本资源 / 魔改版剥了资源段）时，
// 绝不能顺手判成「不支持」。
func IsUnsupportedVersion(version string) bool {
	if version == "" {
		return false // 读不到 → 按 v3 走
	}
	majorStr, _, ok := strings.Cut(version, ".")
	if !ok {
		majorStr = version // 没有点号，整串当主版本试试
	}
	major, err := strconv.Atoi(majorStr)
	if err != nil {
		return false // 解析不了 → 按 v3 走
	}
	if major < 1 {
		// 负数/0 都是垃圾（版本资源损坏、被魔改剥过、或读到了非 exe）。
		// `strconv.Atoi("-1")` 是**成功**的，不带这道闸就会让 `-1 < 3` 成立 → 误判成 v2
		// → 误禁用一个可能好好的 v3。拿不准一律按 v3 走。
		return false
	}
	return major < v3MinMajor
}
