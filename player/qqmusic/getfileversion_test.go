package qqmusic

// 本文件只测 getFileVersion：从 PE 版本资源读 QQ 音乐主版本号，它决定用哪张偏移表。

import (
	"os"
	"path/filepath"
	"testing"
)

// TestGetFileVersion 用系统自带的 PE 文件验证版本探测。
// 这条路径是承重的：getFileVersion 的结果决定 offsetsForVersion 选哪套偏移表，
// 选错会让整个 QQ 音乐后端去解引用错误的地址。而它内部要做 uintptr→切片偏移的指针
// 运算（VerQueryValue 返回的指针落在我们自己分配的 buf 里），一旦算错就是静默读到垃圾。
func TestGetFileVersion(t *testing.T) {
	// kernel32.dll 一定存在且一定带版本资源。
	sysDir := os.Getenv("SystemRoot")
	if sysDir == "" {
		sysDir = `C:\Windows`
	}
	path := filepath.Join(sysDir, "System32", "kernel32.dll")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("找不到 %s，跳过：%v", path, err)
	}

	major, minor := getFileVersion(path)
	if major == 0 && minor == 0 {
		t.Fatalf("getFileVersion(%s) 返回 0.0——版本资源解析失败（指针偏移算错时正是这个症状）", path)
	}
	// Win10/11 的 kernel32 主版本号是 10。这里只做宽松的合理性检查，
	// 目的是确认「读到的是真实版本字段」而不是「读到了垃圾」。
	if major < 5 || major > 99 {
		t.Errorf("major=%d 不像真实的 Windows 版本号，疑似读到垃圾内存", major)
	}
	t.Logf("kernel32.dll ProductVersion = %d.%d", major, minor)
}

// TestGetFileVersionBadPath 确认坏输入不 panic、不返回垃圾。
func TestGetFileVersionBadPath(t *testing.T) {
	cases := []string{
		filepath.Join(t.TempDir(), "does-not-exist.exe"),
		"", // 空路径
	}
	for _, p := range cases {
		major, minor := getFileVersion(p)
		if major != 0 || minor != 0 {
			t.Errorf("getFileVersion(%q) = %d.%d，坏输入应返回 0,0", p, major, minor)
		}
	}
}

// TestGetFileVersionNonPE 确认对「存在但不是 PE」的文件安全返回。
// 这是 QQ 音乐被换成占位文件/损坏安装时会走到的路径。
func TestGetFileVersionNonPE(t *testing.T) {
	p := filepath.Join(t.TempDir(), "notpe.exe")
	if err := os.WriteFile(p, []byte("this is not a PE file"), 0o644); err != nil {
		t.Fatal(err)
	}
	major, minor := getFileVersion(p)
	if major != 0 || minor != 0 {
		t.Errorf("getFileVersion(非PE) = %d.%d，应返回 0,0", major, minor)
	}
}
