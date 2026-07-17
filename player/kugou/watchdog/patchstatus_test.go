package watchdog

// 本文件只测 patch 状态判定（classifyLibcef / CheckPatchStatus）：9 处偏移的 orig/data
// 字节指纹如何映射到「已打 / 可打 / 版本不认识 / 读失败」四态。不测写盘，那在 patchwrite_test.go。

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// sparseReaderAt 是一个稀疏的 io.ReaderAt：只在登记过的偏移返回字节，其余偏移或读越界
// 返回 io.EOF。用它取代真实的 139MB DLL —— 我们只关心 9 个补丁偏移处的字节。
type sparseReaderAt struct {
	at map[int64][]byte
}

func (s sparseReaderAt) ReadAt(p []byte, off int64) (int, error) {
	b, ok := s.at[off]
	if !ok || len(b) < len(p) {
		return 0, io.EOF // 模拟「读不到该偏移」（短文件 / 偏移未登记）
	}
	copy(p, b[:len(p)])
	return len(p), nil
}

// buildReader 按 pick 从补丁表生成一个稀疏读取器：每个偏移放 pick(p) 选出的字节。
func buildReader(pick func(p patchEntry) []byte) sparseReaderAt {
	m := make(map[int64][]byte, len(libcefPatches))
	for _, p := range libcefPatches {
		m[p.offset] = pick(p)
	}
	return sparseReaderAt{at: m}
}

// TestPatchTableConsistency 守住 patchEntry 的两条不变量，二者都是 classifyLibcef 正确性的前提：
//  1. len(orig) == len(data)：CheckPatchStatus 按 len(data) 读取再与 orig 比，长度不等则
//     orig 分支永假，指纹退化。
//  2. orig != data：若某处 orig==data，则该处对「已打 / 未打」的区分力为零，指纹被稀释。
//     旧实现的致命点正是 Patch1 的 data 等于它自己的哨兵位点，本测试确保新表不再犯。
func TestPatchTableConsistency(t *testing.T) {
	if len(libcefPatches) != 9 {
		t.Fatalf("补丁表应有 9 项，实得 %d", len(libcefPatches))
	}
	for i, p := range libcefPatches {
		if len(p.orig) != len(p.data) {
			t.Errorf("Patch%d (offset=0x%X): len(orig)=%d != len(data)=%d", i+1, p.offset, len(p.orig), len(p.data))
		}
		if bytes.Equal(p.orig, p.data) {
			t.Errorf("Patch%d (offset=0x%X): orig 与 data 相同（%X），该处指纹区分力为零", i+1, p.offset, p.orig)
		}
	}
}

// TestClassifyLibcef 用合成字节流覆盖 classifyLibcef 的全部分支。
func TestClassifyLibcef(t *testing.T) {
	origPick := func(p patchEntry) []byte { return p.orig }
	dataPick := func(p patchEntry) []byte { return p.data }

	t.Run("全 orig（原版未打）→ 可打、未打", func(t *testing.T) {
		allPatched, canAutoFix, err := classifyLibcef(buildReader(origPick))
		if err != nil || allPatched != false || canAutoFix != true {
			t.Fatalf("期望 (false,true,nil)，实得 (%v,%v,%v)", allPatched, canAutoFix, err)
		}
	})

	t.Run("全 data（已打）→ 已打、可识别", func(t *testing.T) {
		allPatched, canAutoFix, err := classifyLibcef(buildReader(dataPick))
		if err != nil || allPatched != true || canAutoFix != true {
			t.Fatalf("期望 (true,true,nil)，实得 (%v,%v,%v)", allPatched, canAutoFix, err)
		}
	})

	t.Run("混合态（部分已打）→ 未全打但可打", func(t *testing.T) {
		// 奇数项已打、偶数项未打：每处仍 ∈{orig,data}，应判可打、幂等重打
		mixed := buildReader(func(p patchEntry) []byte { return p.orig })
		mixed.at[libcefPatches[0].offset] = libcefPatches[0].data
		mixed.at[libcefPatches[3].offset] = libcefPatches[3].data
		allPatched, canAutoFix, err := classifyLibcef(mixed)
		if err != nil || allPatched != false || canAutoFix != true {
			t.Fatalf("期望 (false,true,nil)，实得 (%v,%v,%v)", allPatched, canAutoFix, err)
		}
	})

	t.Run("任一处字节陌生 → 拒绝（版本不认识）", func(t *testing.T) {
		// 逐个偏移注入垃圾，每次都必须判 canAutoFix=false —— 覆盖「区域 A 漂移」这类
		// 只动某几处、其余仍匹配的场景，正是旧单点哨兵抓不住的。
		for victim := range libcefPatches {
			r := buildReader(origPick)
			junk := make([]byte, len(libcefPatches[victim].data))
			for k := range junk {
				junk[k] = 0xEE // 既非 orig 也非 data
			}
			r.at[libcefPatches[victim].offset] = junk
			allPatched, canAutoFix, err := classifyLibcef(r)
			if err != nil || allPatched != false || canAutoFix != false {
				t.Errorf("污染 Patch%d 后期望 (false,false,nil)，实得 (%v,%v,%v)", victim+1, allPatched, canAutoFix, err)
			}
		}
	})

	t.Run("读不到偏移（短文件）→ 返回 err，绝不当作可打", func(t *testing.T) {
		// 空读取器：任何偏移都 EOF。旧实现此处会返回 (false,true,nil) 照打，是缺陷。
		allPatched, canAutoFix, err := classifyLibcef(sparseReaderAt{at: map[int64][]byte{}})
		if err == nil {
			t.Fatalf("读失败必须返回 error，实得 (%v,%v,nil)", allPatched, canAutoFix)
		}
		if allPatched || canAutoFix {
			t.Errorf("读失败时不得返回可打/已打，实得 allPatched=%v canAutoFix=%v", allPatched, canAutoFix)
		}
	})
}

// TestCheckPatchStatusRealDLL 用真机上的真实 libcef.dll 锚定 orig/data 字节表
// （CI 的 Linux runner 没有此文件也编译不了本包，故 Skip；本机是唯一门禁）。
//
// **这是全仓唯一验证 orig 表保真度的测试**：合成测试从 orig 表生成数据，改错表数据也
// 跟着变，自洽而无从发现；只有读真机原始字节才能发现表写错了。
//
// 断言只要求 canAutoFix==true，即 9 处偏移**每一处都落在 {orig,data} 之内**——这与
// 真机 DLL 当前是否已被 patch 无关（两种状态都该判可识别），故不因用户跑过真实修复
// 流程而失效。allPatched 只记录不断言，它是机器状态而非代码正确性。
func TestCheckPatchStatusRealDLL(t *testing.T) {
	candidates := []string{
		`C:\Program Files\KuGou\KGMusic\20.1.22.27795\libcef.dll`,
		`C:\Program Files (x86)\KuGou\KGMusic\20.1.22.27795\libcef.dll`,
	}
	var path string
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			path = c
			break
		}
	}
	if path == "" {
		t.Skip("未找到 20.1.22.27795 的真实 libcef.dll，跳过真机锚定")
	}

	allPatched, canAutoFix, err := CheckPatchStatus(path)
	if err != nil {
		t.Fatalf("CheckPatchStatus(%s) 报错: %v", filepath.Base(path), err)
	}
	if !canAutoFix {
		t.Errorf("真机 20.1.22.27795 的 libcef.dll 应被识别（canAutoFix=true），实得 false" +
			" —— 9 处偏移里有字节既非 orig 也非 data，orig/data 表与真机不符")
	}
	t.Logf("真机判定: allPatched=%v canAutoFix=%v（allPatched 反映本机 DLL 是否已打，"+
		"只记录不断言；canAutoFix 才是对表的约束）", allPatched, canAutoFix)
}

// TestCheckPatchStatusOpenError 确认打不开文件时返回 error 而非静默放行。
func TestCheckPatchStatusOpenError(t *testing.T) {
	_, _, err := CheckPatchStatus(filepath.Join(t.TempDir(), "nonexistent.dll"))
	if err == nil {
		t.Fatal("打开不存在的文件应返回 error")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("期望 os.ErrNotExist，实得 %v", err)
	}
}
