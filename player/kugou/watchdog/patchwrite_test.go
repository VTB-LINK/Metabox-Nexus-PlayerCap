package watchdog

// 本文件只测 patch 写盘链路（patchWithBackup / copyFile / rollback）：备份 → 写 → 自校验
// → 失败回滚，以及「版本不认识就一个字节都不写」。不测状态判定，那在 patchstatus_test.go。

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// real20xLibcef 返回真机上 20.1.22.27795 的 libcef.dll 路径，找不到返回 ""。
// 端到端测试全部在它的临时副本上操作，绝不改动真实安装。
func real20xLibcef() string {
	for _, c := range []string{
		`C:\Program Files\KuGou\KGMusic\20.1.22.27795\libcef.dll`,
		`C:\Program Files (x86)\KuGou\KGMusic\20.1.22.27795\libcef.dll`,
	} {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

// workCopyAtOrig 复制真机 libcef.dll 到临时目录，并把 9 处偏移归一到 orig 字节。
//
// **为什么要归一**：真机的 DLL 可能已被真实修复流程打过补丁，测试不能假设它是原版
// ——依赖外部可变状态的测试会在用户跑过一次 patch 之后凭空变红。归一后本测试与机器
// 当前状态无关。
//
// **归一之后本测试只验证机制，不验证 orig 表的保真度**：写完 orig 再读 orig 当然相等。
// orig 表与真机字节是否相符，由 TestCheckPatchStatusRealDLL 锚定（它读真机原始字节，
// 要求每处都落在 {orig,data} 内），二者分工不同，别混淆。
func workCopyAtOrig(t *testing.T, src string) string {
	t.Helper()
	work := filepath.Join(t.TempDir(), "libcef.dll")
	if err := copyFile(src, work); err != nil {
		t.Fatalf("准备工作副本失败: %v", err)
	}
	f, err := os.OpenFile(work, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for i, p := range libcefPatches {
		if _, err := f.WriteAt(p.orig, p.offset); err != nil {
			t.Fatalf("把 Patch%d 归一到 orig 失败: %v", i+1, err)
		}
	}
	return work
}

func writeByteAt(t *testing.T, path string, off int64, b byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteAt([]byte{b}, off); err != nil {
		t.Fatal(err)
	}
}

func readBytesAt(t *testing.T, path string, off int64, n int) []byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b := make([]byte, n)
	if _, err := f.ReadAt(b, off); err != nil {
		t.Fatal(err)
	}
	return b
}

// TestCopyFile 验证复制内容逐字节一致。
func TestCopyFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	want := []byte("ORIGINAL-CONTENT-\x00\x01\xFF")
	if err := os.WriteFile(src, want, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(src, dst); err != nil {
		t.Fatalf("copyFile: %v", err)
	}
	got, _ := os.ReadFile(dst)
	if !bytes.Equal(got, want) {
		t.Errorf("复制内容不一致: got %q want %q", got, want)
	}
}

// TestRollback 验证回滚：从备份恢复原内容，成功后删除备份。
func TestRollback(t *testing.T) {
	dir := t.TempDir()
	orig := filepath.Join(dir, "f.dll")
	bak := orig + ".playercap.bak"
	if err := os.WriteFile(orig, []byte("ORIGINAL"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(orig, bak); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(orig, []byte("CORRUPTED-XXXXX"), 0o644); err != nil {
		t.Fatal(err)
	}
	rollback(bak, orig)
	got, _ := os.ReadFile(orig)
	if string(got) != "ORIGINAL" {
		t.Errorf("回滚后内容应为 ORIGINAL，实得 %q", got)
	}
	if _, err := os.Stat(bak); !os.IsNotExist(err) {
		t.Errorf("回滚成功后备份应被删除，但仍存在: %v", err)
	}
}

// TestPatchWithBackupRealDLL 端到端：在真实 libcef.dll 的临时副本上跑完整 patch 流程。
// 验证的是机制在真实 ~139MB 文件、真实偏移上的行为：备份 → 写 → 自校验 → 清理 → 幂等。
func TestPatchWithBackupRealDLL(t *testing.T) {
	src := real20xLibcef()
	if src == "" {
		t.Skip("未找到 20.1.22.27795 真实 libcef.dll，跳过端到端 patch 测试")
	}
	work := workCopyAtOrig(t, src)

	// 前置 sanity（归一后必然成立；不成立说明 classifyLibcef 与写入读的不是同一批偏移，
	// 或某处 orig==data 使指纹退化）
	if ap, caf, err := CheckPatchStatus(work); err != nil || ap || !caf {
		t.Fatalf("归一到 orig 后应为 (false,true,nil)，实得 (%v,%v,%v)", ap, caf, err)
	}

	if err := patchWithBackup(work); err != nil {
		t.Fatalf("patchWithBackup 失败: %v", err)
	}

	if ap, caf, err := CheckPatchStatus(work); err != nil || !ap || !caf {
		t.Fatalf("patch 后应为 (true,true,nil)，实得 (%v,%v,%v)", ap, caf, err)
	}
	if _, err := os.Stat(work + ".playercap.bak"); !os.IsNotExist(err) {
		t.Errorf("patch 成功后 .bak 应被删除，但仍存在")
	}

	// 幂等：对已 patched 的再打，复检早退，不报错、不再动文件
	if err := patchWithBackup(work); err != nil {
		t.Errorf("对已 patched 的 DLL 再打应无错（幂等），实得: %v", err)
	}
}

// TestPatchWithBackupRejectsUnknownRealDLL 端到端：把副本的一个偏移改成陌生字节
// （模拟未知版本），落笔前复检必须拒绝，且**一个字节都不写**。
//
// 关键断言用「拒绝前后对比」而非硬编码字节值——后者会随真机 DLL 是否已 patch 而失效。
func TestPatchWithBackupRejectsUnknownRealDLL(t *testing.T) {
	src := real20xLibcef()
	if src == "" {
		t.Skip("未找到 20.1.22.27795 真实 libcef.dll，跳过")
	}
	work := workCopyAtOrig(t, src)

	const p1Off = 0x58C63EB // Patch1 所在（0x58C 簇）
	const p5Off = 0x4BC180E // Patch5 所在（0x4BC 簇，距 P1 约 12.8MB 的另一个代码簇）

	before := readBytesAt(t, work, p5Off, 6)
	writeByteAt(t, work, p1Off, 0xFF) // 破坏 P1 → 既非 orig 也非 data → 未知版本

	err := patchWithBackup(work)
	if err == nil {
		t.Fatal("对未知版本 DLL 应拒绝 patch，实得 nil")
	}
	if !strings.Contains(err.Error(), "版本指纹不符") {
		t.Errorf("拒绝原因应为版本指纹不符，实得: %v", err)
	}

	// 拒绝时不得写任何补丁——P5 在另一个代码簇，正是旧单点哨兵会盲写的地方
	if after := readBytesAt(t, work, p5Off, 6); !bytes.Equal(before, after) {
		t.Errorf("拒绝时不得写任何补丁，但 P5 被改动了：before=%X after=%X", before, after)
	}
	if _, err := os.Stat(work + ".playercap.bak"); !os.IsNotExist(err) {
		t.Errorf("复检在备份之前就该拒绝，不应产生 .bak")
	}
}
