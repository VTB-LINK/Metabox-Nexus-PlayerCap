package assoc

// 本文件只测以读、写、删除三种共享打开酷狗的文件（openShared，设计 §3「Windows 打开方式」）：持有句柄时
// 酷狗照常写、改名、删除；长路径与中文路径能打开；只接受磁盘文件。用真文件（t.TempDir()），不碰酷狗的文件。

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"

	"golang.org/x/sys/windows"
)

// kugouOps 在持有 path 的另一个句柄时，依次做酷狗会对自己的文件做的事：以读写打开并写入、改名、删除。
// 返回第一个失败的操作与错误；都成功时返回改名后的路径（已删除）。
func kugouOps(path string) (failed string, err error) {
	w, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return "open for write", err
	}
	_, err = w.WriteAt([]byte("KUGOU"), 0)
	w.Close()
	if err != nil {
		return "write", err
	}
	moved := path + ".bak"
	if err := os.Rename(path, moved); err != nil {
		return "rename", err
	}
	if err := os.Remove(moved); err != nil {
		return "remove", err
	}
	return "", nil
}

// 变异自证（实测）：openShared 的共享模式去掉 FILE_SHARE_DELETE 即红（改名报共享冲突）。
func TestOpenSharedAllowsKuGouOps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "KGMusicV3.db")
	if err := os.WriteFile(path, []byte("original content"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := openShared(path)
	if err != nil {
		t.Fatalf("openShared: %v", err)
	}
	defer f.Close()

	if op, err := kugouOps(path); err != nil {
		t.Fatalf("%s while openShared holds the file: %v", op, err)
	}
	// 句柄照常可读，读到的是酷狗写入之后的内容。
	got := make([]byte, 5)
	if _, err := f.ReadAt(got, 0); err != nil || string(got) != "KUGOU" {
		t.Errorf("ReadAt after KuGou's write = %q, %v; want %q", got, err, "KUGOU")
	}
}

// 对照：同样的操作，换成共享模式不含删除的句柄（os.Open 与 SQLite 的写法）时改名失败。
// 说明上一个用例确实能分辨共享模式，不是这些操作在任何句柄下都能成功。
func TestOpenSharedControlWithoutShareDelete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "KGMusicV3.db")
	if err := os.WriteFile(path, []byte("original content"), 0o644); err != nil {
		t.Fatal(err)
	}
	name, _ := windows.UTF16PtrFromString(path)
	h, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)

	op, err := kugouOps(path)
	if op != "rename" || !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		t.Errorf("without FILE_SHARE_DELETE: failed at %q with %v; want rename to fail with a sharing violation", op, err)
	}
}

func utf16Len(s string) int { return len(utf16.Encode([]rune(s))) }

// 目录名含中文、全路径超过 260 个 UTF-16 字符：readLyricFile 读回原字节；路径写成正斜杠同样能读。
//
// 本机（开发机）开着系统长路径支持（LongPathsEnabled=1），去掉 \\?\ 前缀本用例也能过（实测）；
// 前缀是否加上由 TestLongPathPrefix 钉住，本用例守的是加了前缀的路径确实打得开：加前缀前不 Clean，
// 正斜杠一例即红（实测）。
func TestOpenSharedLongAndChinesePath(t *testing.T) {
	dir := t.TempDir()
	for utf16Len(dir) <= 300 {
		dir = filepath.Join(dir, "酷狗歌词目录_本地关联测试")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "门尼 - 无名指 (The Rust).krc")
	want := []byte("krc1\x00\x01\x02 歌词内容")
	if err := os.WriteFile(path, want, 0o644); err != nil {
		t.Fatal(err)
	}
	if n := utf16Len(path); n <= 260 {
		t.Fatalf("fixture: path is %d UTF-16 units, want more than 260", n)
	}
	for _, p := range []string{path, filepath.ToSlash(path)} {
		got, err := readLyricFile(p)
		if err != nil {
			t.Fatalf("readLyricFile(%d UTF-16 units): %v", utf16Len(p), err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("readLyricFile = %q, want %q", got, want)
		}
	}
}

// 变异自证（实测）：不 Clean、按字节计长、按字符（rune）计长、从不加前缀，各自使对应的用例红。
func TestLongPathPrefix(t *testing.T) {
	seg := strings.Repeat("目", 9) + `\` // 10 个 UTF-16 字符、28 字节
	long := `C:\` + strings.Repeat(seg, 30) + "a.krc"
	for _, c := range []struct {
		name, in, want string
	}{
		{"short absolute path", `C:\KuGou\Lyric\a.krc`, `C:\KuGou\Lyric\a.krc`},
		{"259 UTF-16 units", `C:\` + strings.Repeat("a", 256), `C:\` + strings.Repeat("a", 256)},
		{"260 UTF-16 units", `C:\` + strings.Repeat("a", 257), `\\?\C:\` + strings.Repeat("a", 257)},
		{"long drive path", long, `\\?\` + long},
		{"long path with slashes and dot segments", strings.ReplaceAll(long, `\`, "/") + "/../a.krc", `\\?\` + long},
		{"long UNC path", `\\server\share\` + strings.Repeat(seg, 30) + "a.krc", `\\?\UNC\server\share\` + strings.Repeat(seg, 30) + "a.krc"},
		{"already prefixed", `\\?\` + long, `\\?\` + long},
		{"device namespace", `\\.\` + long[3:], `\\.\` + long[3:]},
		{"long relative path", strings.Repeat(seg, 30) + "a.krc", strings.Repeat(seg, 30) + "a.krc"},
		// 按 UTF-16 计长：250 个汉字 750 字节，连同 C:\ 只有 253 个 UTF-16 字符。
		{"Chinese counted in UTF-16 units", `C:\` + strings.Repeat("歌", 250), `C:\` + strings.Repeat("歌", 250)},
		// 辅助平面字符占 2 个 UTF-16 字符：129 个共 258，加上 C:\ 为 261。
		{"supplementary characters count twice", `C:\` + strings.Repeat("😀", 129), `\\?\C:\` + strings.Repeat("😀", 129)},
	} {
		if got := longPath(c.in); got != c.want {
			t.Errorf("%s: longPath(%d UTF-16 units) = %q\nwant %q", c.name, utf16Len(c.in), got, c.want)
		}
	}
}

// 反例：不存在的文件（errors.Is 能认出不存在）、目录、字符设备、带 NUL 的路径，一律报错、不返回句柄。
// 只有字符设备报 errNotDiskFile（pick 据此判不可用、不重试），其余不报。
// 变异自证（实测）：去掉文件类型检查，NUL 设备两例红；该情形不包装 errNotDiskFile，同两例红。
func TestOpenSharedRejects(t *testing.T) {
	dir := t.TempDir()
	for _, c := range []struct {
		name, path        string
		notExist, notDisk bool
	}{
		{"missing file", filepath.Join(dir, "missing.krc"), true, false},
		{"missing directory", filepath.Join(dir, "missing", "a.krc"), true, false},
		{"directory", dir, false, false},
		{"NUL device", "NUL", false, true},
		{"NUL device namespace", `\\.\NUL`, false, true},
		{"path with NUL", filepath.Join(dir, "a\x00.krc"), false, false},
	} {
		f, err := openShared(c.path)
		if err == nil {
			f.Close()
			t.Errorf("%s: openShared succeeded, want an error", c.name)
			continue
		}
		if f != nil {
			t.Errorf("%s: openShared returned a file with error %v", c.name, err)
		}
		if c.notExist && !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s: err = %v, want fs.ErrNotExist", c.name, err)
		}
		if errors.Is(err, errNotDiskFile) != c.notDisk {
			t.Errorf("%s: errors.Is(%v, errNotDiskFile) = %v, want %v", c.name, err, !c.notDisk, c.notDisk)
		}
	}
}
