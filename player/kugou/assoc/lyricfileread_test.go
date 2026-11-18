package assoc

// 本文件只测读歌词文件的大小上限（readLyricFile）：不超过 klyric.MaxLocalFileBytes 的原样读回，超过的返回
// klyric.ErrLyricTooLarge 且只读到上限为止；打不开的不当作超限。用真文件（t.TempDir()）。

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	klyric "Metabox-Nexus-PlayerCap/player/kugou/lyric"
)

func writeSized(t *testing.T, n int64) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "a.krc")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(n); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadLyricFileTooLarge(t *testing.T) {
	_, err := readLyricFile(writeSized(t, klyric.MaxLocalFileBytes+1))
	if !errors.Is(err, klyric.ErrLyricTooLarge) {
		t.Fatalf("err = %v, want ErrLyricTooLarge", err)
	}
}

// 边界与反例：恰好在上限、空文件原样读回；打不开的文件报的不是超限（pick 据此记为暂缺）。
func TestReadLyricFileWithinLimit(t *testing.T) {
	for _, n := range []int64{0, 1, klyric.MaxLocalFileBytes} {
		path := writeSized(t, n)
		want := bytes.Repeat([]byte{0}, int(n))
		got, err := readLyricFile(path)
		if err != nil {
			t.Fatalf("size %d: %v", n, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("size %d: read %d bytes", n, len(got))
		}
	}
	_, err := readLyricFile(filepath.Join(t.TempDir(), "missing.krc"))
	if !errors.Is(err, fs.ErrNotExist) || errors.Is(err, klyric.ErrLyricTooLarge) {
		t.Errorf("missing file: err = %v, want fs.ErrNotExist and not ErrLyricTooLarge", err)
	}
}

// 远大于上限的文件只读到上限为止：64 MiB 的文件，读取期间累计分配不超过 32 MiB。读到上限为止实测约
// 10 MiB（race 构建约 20 MiB）；整个读入至少分配 64 MiB 再加一份同样大的拷贝（去掉 LimitReader 实测
// 157.7 MiB，红）。
func TestReadLyricFileStopsAtLimit(t *testing.T) {
	path := writeSized(t, 64<<20)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err := readLyricFile(path)
	runtime.ReadMemStats(&after)
	if !errors.Is(err, klyric.ErrLyricTooLarge) {
		t.Fatalf("err = %v, want ErrLyricTooLarge", err)
	}
	alloc := after.TotalAlloc - before.TotalAlloc
	t.Logf("allocated %.1f MiB", float64(alloc)/(1<<20))
	if alloc > 32<<20 {
		t.Errorf("allocated %d bytes reading a 64 MiB file, want at most 32 MiB", alloc)
	}
}
