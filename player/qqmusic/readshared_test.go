package qqmusic

// 本文件只测以读、写、删除三种共享读取 QQ 音乐的文件（openShared、readShared、longPath，设计 §6.2）：持有句柄时
// QQ 照常覆盖写、以改名替换、改名、删除；长路径与中文路径能读；大小上限；只接受磁盘文件。
// 用 t.TempDir() 里的真文件，不碰 QQ 的文件。

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

// qqOps 在另一个句柄持有 path 时，依次做 QQ 与用户会对这些文件做的事：按 QQ 的写法（设计 §2.1：GENERIC_WRITE、
// 共享模式只有 FILE_SHARE_READ、CREATE_ALWAYS）整份重写，按 QQ 的读法（FILE_SHARE_READ、OPEN_ALWAYS）打开，
// 再改名、删除。返回第一个失败的操作与错误。
func qqOps(path string) (failed string, err error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "path", err
	}
	w, err := windows.CreateFile(name, windows.GENERIC_WRITE, windows.FILE_SHARE_READ, nil, windows.CREATE_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return "QQ rewrite", err
	}
	_, err = windows.Write(w, []byte("QQ rewrote"))
	windows.CloseHandle(w)
	if err != nil {
		return "QQ write", err
	}
	r, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return "QQ read", err
	}
	windows.CloseHandle(r)
	moved := path + ".bak"
	if err := os.Rename(path, moved); err != nil {
		return "rename", err
	}
	if err := os.Remove(moved); err != nil {
		return "remove", err
	}
	return "", nil
}

// 变异自证（实测）：openShared 的共享模式去掉 FILE_SHARE_WRITE（QQ 重写报共享冲突）、去掉 FILE_SHARE_DELETE
// （改名报共享冲突），各自即红。
func TestOpenSharedAllowsQQOps(t *testing.T) {
	path := writeLyricFile(t, t.TempDir(), "LyricAssociation.dat", []byte("original content"))
	f, err := openShared(path)
	if err != nil {
		t.Fatalf("openShared: %v", err)
	}
	defer f.Close()
	if op, err := qqOps(path); err != nil {
		t.Fatalf("%s while openShared holds the file: %v", op, err)
	}
	// 句柄照常可读，读到的是 QQ 重写之后的内容。
	got := make([]byte, 10)
	if _, err := f.ReadAt(got, 0); err != nil || string(got) != "QQ rewrote" {
		t.Errorf("ReadAt after QQ's rewrite = %q, %v; want %q", got, err, "QQ rewrote")
	}
}

// 对照：共享模式不含删除的句柄（os.Open 的写法）在场时，改名失败。说明上一个用例能分辨共享模式。
func TestOpenSharedControlWithoutShareDelete(t *testing.T) {
	path := writeLyricFile(t, t.TempDir(), "LyricAssociation.dat", []byte("original content"))
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	op, err := qqOps(path)
	if op != "rename" || !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		t.Errorf("with os.Open holding the file: failed at %q with %v; want rename to fail with a sharing violation", op, err)
	}
}

func utf16Len(s string) int { return len(utf16.Encode([]rune(s))) }

// 目录名含中文、全路径超过 270 个 UTF-16 字符：readShared 读回原字节；写成正斜杠同样能读。
//
// 开发机开着系统长路径支持时去掉 \\?\ 前缀本用例也能过；前缀是否加上由 TestLongPathPrefix 钉住。
func TestReadSharedLongAndChinesePath(t *testing.T) {
	dir := t.TempDir()
	for utf16Len(dir) <= 270 {
		dir = filepath.Join(dir, "QQ音乐歌词目录_关联测试")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	want := []byte("[00:01.00]歌词内容")
	path := writeLyricFile(t, dir, "歌手 - 歌名.lrc", want)
	if n := utf16Len(path); n <= 270 {
		t.Fatalf("fixture: path is %d UTF-16 units, want more than 270", n)
	}
	for _, p := range []string{path, filepath.ToSlash(path)} {
		got, err := readShared(p, maxLyricFileBytes)
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("readShared(%d UTF-16 units) = %q, %v; want %q", utf16Len(p), got, err, want)
		}
	}
}

// 变异自证（实测）：加前缀前不 Clean，带正斜杠与 .. 的一例红；从不加前缀，三例红。
func TestLongPathPrefix(t *testing.T) {
	seg := strings.Repeat("歌", 9) + `\` // 10 个 UTF-16 字符
	long := `C:\` + strings.Repeat(seg, 30) + "a.qrc"
	for _, c := range []struct {
		name, in, want string
	}{
		{"short absolute path", `C:\QQMusicCache\a.qrc`, `C:\QQMusicCache\a.qrc`},
		{"259 UTF-16 units", `C:\` + strings.Repeat("a", 256), `C:\` + strings.Repeat("a", 256)},
		{"260 UTF-16 units", `C:\` + strings.Repeat("a", 257), `\\?\C:\` + strings.Repeat("a", 257)},
		{"long drive path", long, `\\?\` + long},
		{"long path with slashes and dot segments", strings.ReplaceAll(long, `\`, "/") + "/../a.qrc", `\\?\` + long},
		{"long UNC path", `\\server\share\` + strings.Repeat(seg, 30) + "a.qrc", `\\?\UNC\server\share\` + strings.Repeat(seg, 30) + "a.qrc"},
		{"already prefixed", `\\?\` + long, `\\?\` + long},
		{"long relative path", strings.Repeat(seg, 30) + "a.qrc", strings.Repeat(seg, 30) + "a.qrc"},
		{"supplementary characters count twice", `C:\` + strings.Repeat("😀", 129), `\\?\C:\` + strings.Repeat("😀", 129)},
	} {
		if got := longPath(c.in); got != c.want {
			t.Errorf("%s: longPath(%d UTF-16 units) = %q\nwant %q", c.name, utf16Len(c.in), got, c.want)
		}
	}
}

// 恰好 max 字节可读，多一字节即 errLocalTooLarge；空文件读出空内容。
// 变异自证（实测）：上限判断 `>` 改为 `>=`，恰好 max 一例红。
func TestReadSharedLimit(t *testing.T) {
	dir := t.TempDir()
	for _, c := range []struct {
		name    string
		size    int
		tooBig  bool
		maxRead int64
	}{
		{"empty file", 0, false, 16},
		{"exactly max", 16, false, 16},
		{"max plus one", 17, true, 16},
		{"far beyond max", 1 << 20, true, 16},
	} {
		path := writeLyricFile(t, dir, c.name, bytes.Repeat([]byte{'x'}, c.size))
		got, err := readShared(path, c.maxRead)
		if c.tooBig {
			if !errors.Is(err, errLocalTooLarge) || got != nil {
				t.Errorf("%s: readShared = %d bytes, %v; want errLocalTooLarge", c.name, len(got), err)
			}
		} else if err != nil || len(got) != c.size {
			t.Errorf("%s: readShared = %d bytes, %v; want %d bytes", c.name, len(got), err, c.size)
		}
	}
}

// 反例：不存在的文件与目录（os.IsNotExist 与 errors.Is 都认得出）、目录、字符设备、带 NUL 的路径，一律报错。
// 只有字符设备报 errLocalNotDisk。
// 变异自证（实测）：去掉文件类型检查，NUL 设备两例红。
func TestReadSharedRejects(t *testing.T) {
	dir := t.TempDir()
	for _, c := range []struct {
		name, path        string
		notExist, notDisk bool
	}{
		{"missing file", filepath.Join(dir, "missing.qrc"), true, false},
		{"missing directory", filepath.Join(dir, "missing", "a.qrc"), true, false},
		{"directory", dir, false, false},
		{"NUL device", "NUL", false, true},
		{"NUL device namespace", `\\.\NUL`, false, true},
		{"path with NUL", filepath.Join(dir, "a\x00.qrc"), false, false},
	} {
		got, err := readShared(c.path, maxLyricFileBytes)
		if err == nil || got != nil {
			t.Errorf("%s: readShared = %q, %v; want an error", c.name, got, err)
			continue
		}
		if c.notExist && (!os.IsNotExist(err) || !errors.Is(err, fs.ErrNotExist)) {
			t.Errorf("%s: err = %v; want os.IsNotExist and errors.Is(fs.ErrNotExist)", c.name, err)
		}
		if !c.notExist && errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s: err = %v; must not read as not-exist", c.name, err)
		}
		if errors.Is(err, errLocalNotDisk) != c.notDisk {
			t.Errorf("%s: errors.Is(%v, errLocalNotDisk) = %v, want %v", c.name, err, !c.notDisk, c.notDisk)
		}
	}
}
