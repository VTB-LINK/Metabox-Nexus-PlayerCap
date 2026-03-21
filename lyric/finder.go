package lyric

import (
	"fmt"
	"syscall"
	"Metabox-Nexus-WesingCap/proc"
)

// FindLyricHost 定位 LyricHost 实例地址
// 流程: KSongsLyric.dll 导出表 → CreateLyricHost → 构造函数 → vtable → 堆搜索
func FindLyricHost(handle syscall.Handle, modules []proc.Module) (hostAddr uint32, subStructAddr uint32, err error) {
	// 1. 找到 KSongsLyric.dll
	var lyricMod *proc.Module
	for i := range modules {
		if modules[i].Name == "KSongsLyric.dll" {
			lyricMod = &modules[i]
			break
		}
	}
	if lyricMod == nil {
		return 0, 0, fmt.Errorf("未找到 KSongsLyric.dll，请确保 WeSing 正在显示歌词")
	}
	fmt.Printf("[+] KSongsLyric.dll 基址: 0x%08X (大小: 0x%X)\n", lyricMod.Base, lyricMod.Size)

	// 2. 解析 PE 导出表，找到 CreateLyricHost
	createAddr, err := findExportFunction(handle, lyricMod.Base, "CreateLyricHost")
	if err != nil {
		return 0, 0, fmt.Errorf("查找 CreateLyricHost 失败: %v", err)
	}
	fmt.Printf("[+] CreateLyricHost: 0x%08X\n", createAddr)

	// 3. 在 CreateLyricHost 函数体中搜索第一条 CALL 指令 → 构造函数
	constructorAddr, err := findFirstCall(handle, createAddr, 128)
	if err != nil {
		return 0, 0, fmt.Errorf("查找构造函数失败: %v", err)
	}
	fmt.Printf("[+] 构造函数: 0x%08X\n", constructorAddr)

	// 4. 在构造函数中搜索 mov [edi], imm32 → vtable 地址
	vtableAddr, err := findVtableAssignment(handle, constructorAddr, 200)
	if err != nil {
		return 0, 0, fmt.Errorf("查找 vtable 失败: %v", err)
	}
	fmt.Printf("[+] vtable: 0x%08X\n", vtableAddr)

	// 5. 在堆上搜索 vtable 值 → LyricHost 实例
	regions := proc.EnumWritableRegions(handle)
	pattern, mask := proc.Uint32ToAOB(vtableAddr)
	results := proc.AOBScan(handle, pattern, mask, regions)

	if len(results) == 0 {
		return 0, 0, fmt.Errorf("未找到 LyricHost 实例（vtable 0x%08X 无匹配）", vtableAddr)
	}

	hostAddr = results[0]
	subStructAddr = hostAddr + 0x0C // 歌词子结构偏移
	fmt.Printf("[+] LyricHost 实例: 0x%08X\n", hostAddr)
	fmt.Printf("[+] 歌词子结构: 0x%08X\n", subStructAddr)
	return hostAddr, subStructAddr, nil
}

// findExportFunction 解析 PE 导出表，按名称查找导出函数地址
func findExportFunction(handle syscall.Handle, moduleBase uint32, funcName string) (uint32, error) {
	// 读取 PE 签名偏移
	peOff, err := proc.ReadUint32(handle, moduleBase+0x3C)
	if err != nil {
		return 0, fmt.Errorf("读取 PE 偏移失败: %v", err)
	}

	// 读取导出表 RVA（PE Optional Header 的 DataDirectory[0]）
	exportRVA, err := proc.ReadUint32(handle, moduleBase+peOff+0x78)
	if err != nil || exportRVA == 0 {
		return 0, fmt.Errorf("无导出表")
	}

	exportDir := moduleBase + exportRVA

	// 读取导出表字段
	numNames, _ := proc.ReadUint32(handle, exportDir+0x18)
	namesRVA, _ := proc.ReadUint32(handle, exportDir+0x20)
	ordinalsRVA, _ := proc.ReadUint32(handle, exportDir+0x24)
	funcsRVA, _ := proc.ReadUint32(handle, exportDir+0x1C)

	addrOfNames := moduleBase + namesRVA
	addrOfOrdinals := moduleBase + ordinalsRVA
	addrOfFuncs := moduleBase + funcsRVA

	// 遍历名称表
	for i := uint32(0); i < numNames; i++ {
		nameRVA, _ := proc.ReadUint32(handle, addrOfNames+i*4)
		name, _ := proc.ReadString(handle, moduleBase+nameRVA, 64)
		if name == funcName {
			ordIdx, _ := proc.ReadUint16(handle, addrOfOrdinals+i*2)
			funcRVA, _ := proc.ReadUint32(handle, addrOfFuncs+uint32(ordIdx)*4)
			return moduleBase + funcRVA, nil
		}
	}
	return 0, fmt.Errorf("导出函数 %s 不存在", funcName)
}

// findFirstCall 在指定地址开始的 maxBytes 字节内搜索第一条 CALL rel32 (E8 xx xx xx xx)
// 并返回调用目标地址
func findFirstCall(handle syscall.Handle, startAddr uint32, maxBytes int) (uint32, error) {
	buf, err := proc.ReadBytes(handle, startAddr, uint32(maxBytes))
	if err != nil {
		return 0, err
	}

	for i := 0; i < len(buf)-5; i++ {
		if buf[i] == 0xE8 { // CALL rel32
			// 读取 4 字节相对偏移（有符号）
			rel := int32(buf[i+1]) | int32(buf[i+2])<<8 | int32(buf[i+3])<<16 | int32(buf[i+4])<<24
			target := uint32(int32(startAddr) + int32(i) + 5 + rel)

			// 合理性验证：目标应该在模块范围内
			if target > 0x10000 && target < 0x7FFF0000 {
				return target, nil
			}
		}
	}
	return 0, fmt.Errorf("在 0x%X 起始的 %d 字节内未找到 CALL 指令", startAddr, maxBytes)
}

// findVtableAssignment 在构造函数中搜索 mov [edi], imm32 (C7 07 xx xx xx xx)
// 返回 imm32 值（vtable 地址）
func findVtableAssignment(handle syscall.Handle, startAddr uint32, maxBytes int) (uint32, error) {
	buf, err := proc.ReadBytes(handle, startAddr, uint32(maxBytes))
	if err != nil {
		return 0, err
	}

	for i := 0; i < len(buf)-6; i++ {
		if buf[i] == 0xC7 && buf[i+1] == 0x07 { // mov [edi], imm32
			vtable := uint32(buf[i+2]) | uint32(buf[i+3])<<8 | uint32(buf[i+4])<<16 | uint32(buf[i+5])<<24

			// vtable 应该指向模块的 .rdata 段（高地址范围）
			if vtable > 0x10000 && vtable < 0x7FFF0000 {
				return vtable, nil
			}
		}
	}
	return 0, fmt.Errorf("在构造函数中未找到 vtable 赋值")
}
