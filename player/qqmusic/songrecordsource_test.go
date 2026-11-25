package qqmusic

// 本文件只测读记录的来源何时建立（设计 §6.4 第 4 步「谁来读」「版本」）：只在版本号精确匹配、且该版本核实过记录格式与真值时
// （songRecordVersions）由会话的进程句柄复制一个只读句柄；借用偏移表、未打开的版本与复制失败都不读，每会话打一条 Detail。
// 以本测试进程自己的伪句柄充当 QQ 的进程句柄，复制出的句柄读的是本进程的内存。

import (
	"errors"
	"reflect"
	"slices"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// recordSourceProbe 是经复制的句柄读回来的内容；放在包级，地址不随栈增长而变。
var recordSourceProbe = []byte("song-record-source-probe")

// 22.82：得到来源，struct1 为 QQMusic.dll 基址加该版本的 Struct1；复制出的句柄读得到本进程的内存、查得到区域，查用户地址空间
// 之外报 ERROR_INVALID_PARAMETER；close 关掉它，此后再读即句柄无效。
// 22.16（窄字符串）：同样得到来源，struct1 与偏移表为该版本的，不打 Detail。
// 22.52（没有做真值补测）、21.81（记录不随入口变）、借用偏移表（fallbackTo 非空）、hProcess 为 0（复制失败）：nil，各打一条 Detail。
// 变异自证（实测）：不看 fallbackTo，借用偏移表与读不到版本号两例得到来源而红；close 不关句柄，关后仍读得到而红；22.16 的
// SongRecord 去掉，22.16 一例红；21.81 加上 SongRecord，21.81 一例红。
func TestNewRecordSource(t *testing.T) {
	const base = 0x6000_0000
	self := uintptr(windows.CurrentProcess())
	t.Run("22.82", func(t *testing.T) {
		logs := captureAssocLog(t)
		rec := newRecordSource(&QQMusicMem{hProcess: self, qqmusicDllBase: base, version: "22.82", offsets: knownVersions["22.82"]})
		if rec == nil {
			t.Fatalf("22.82 没有得到来源\n%s", logs())
		}
		if want := uintptr(base) + knownVersions["22.82"].Struct1; rec.struct1 != want || !reflect.DeepEqual(rec.offs, knownVersions["22.82"]) {
			t.Fatalf("struct1 %#x，want %#x；偏移表须为 22.82 的", rec.struct1, want)
		}
		secret := recordSourceProbe
		addr := uintptr(unsafe.Pointer(&secret[0]))
		got := make([]byte, len(secret))
		if n, err := rec.mem.read(addr, got); err != nil || n != len(secret) || string(got) != string(secret) {
			t.Fatalf("经复制的句柄读本进程：%d 字节 %q、%v，want %q", n, got, err, secret)
		}
		if r, err := rec.mem.query(addr); err != nil || addr < r.base || addr-r.base >= r.size || r.state != windows.MEM_COMMIT {
			t.Fatalf("查询 %#x：%+v（%v），want 含该地址的已提交区域", addr, r, err)
		}
		// 地址空间到头的约定：超出进程可访问的最高地址时 VirtualQueryEx 报 ERROR_INVALID_PARAMETER，scanRecord 只认它为到头。
		if _, err := rec.mem.query(1 << 47); !errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			t.Fatalf("查询用户地址空间之外：%v，want ERROR_INVALID_PARAMETER", err)
		}
		rec.close()
		if _, err := rec.mem.read(addr, got); !errors.Is(err, windows.ERROR_INVALID_HANDLE) {
			t.Fatalf("close 之后再读：%v，want 句柄无效", err)
		}
		if n := countLog(logs(), "+", ""); n != 0 {
			t.Fatalf("22.82 打了 %d 条 Detail，want 0\n%s", n, logs())
		}
	})
	t.Run("22.16", func(t *testing.T) {
		logs := captureAssocLog(t)
		rec := newRecordSource(&QQMusicMem{hProcess: self, qqmusicDllBase: base, version: "22.16", offsets: knownVersions["22.16"]})
		if rec == nil {
			t.Fatalf("22.16 没有得到来源\n%s", logs())
		}
		defer rec.close()
		if want := uintptr(base) + knownVersions["22.16"].Struct1; rec.struct1 != want || !reflect.DeepEqual(rec.offs, knownVersions["22.16"]) {
			t.Fatalf("struct1 %#x，want %#x；偏移表须为 22.16 的", rec.struct1, want)
		}
		if n := countLog(logs(), "+", ""); n != 0 {
			t.Fatalf("22.16 打了 %d 条 Detail，want 0\n%s", n, logs())
		}
	})
	for _, c := range []struct {
		name string
		mem  *QQMusicMem
	}{
		{"22.52", &QQMusicMem{hProcess: self, qqmusicDllBase: base, version: "22.52", offsets: knownVersions["22.52"]}},
		{"21.81", &QQMusicMem{hProcess: self, qqmusicDllBase: base, version: "21.81", offsets: knownVersions["21.81"]}},
		{"借用偏移表", &QQMusicMem{hProcess: self, qqmusicDllBase: base, version: "22.90", fallbackTo: "22.82", offsets: knownVersions["22.82"]}},
		{"读不到版本号", &QQMusicMem{hProcess: self, qqmusicDllBase: base, version: "unknown", fallbackTo: "22.82", offsets: knownVersions["22.82"]}},
		{"复制失败", &QQMusicMem{hProcess: 0, qqmusicDllBase: base, version: "22.82", offsets: knownVersions["22.82"]}},
	} {
		t.Run(c.name, func(t *testing.T) {
			logs := captureAssocLog(t)
			if rec := newRecordSource(c.mem); rec != nil {
				rec.close()
				t.Fatal("得到了来源，want nil")
			}
			if n := countLog(logs(), "+", ""); n != 1 {
				t.Fatalf("Detail %d 条，want 1\n%s", n, logs())
			}
		})
	}
}

// songRecordVersions 是读当前歌曲记录的版本：22.82 与 2026-11-23 核对过真值的九个版本（设计 §6.4 第 4 步「版本」；22.26
// 随 #232 收录，依据见 knownVersions["22.26"] 的注释），已排序。
var songRecordVersions = []string{"22.16", "22.22", "22.26", "22.31", "22.41", "22.60", "22.71", "22.76", "22.81", "22.82"}

// SongRecord 为真的恰为 songRecordVersions。反例逐个为假：20.05、22.52（没有做真值补测）、21.81（记录不随入口变，设计 §8）。
// 变异自证（实测）：22.16 的 SongRecord 去掉而红；21.81 加上 SongRecord 而红；22.26 的 SongRecord 去掉而红。
func TestKnownVersionsSongRecord(t *testing.T) {
	var got []string
	for v, o := range knownVersions {
		if o.SongRecord {
			got = append(got, v)
		}
	}
	slices.Sort(got)
	if !slices.Equal(got, songRecordVersions) {
		t.Fatalf("SongRecord 为真的版本 %q，want %q", got, songRecordVersions)
	}
	for _, v := range []string{"20.05", "21.81", "22.52"} {
		if o, ok := knownVersions[v]; !ok || o.SongRecord {
			t.Errorf("%s：在表里 %v、SongRecord %v，want 在表里且为假", v, ok, o.SongRecord)
		}
	}
}

// 读记录的版本里只有 22.16、22.22 是窄字符串（UseWideStrings 为假），走 readSongPrefix 的窄字符一支；这两个的歌名、歌手、
// 专辑是三个相邻的 24 字节 std::string（SingerOff = NameOff + 0x18，AlbumOff = NameOff + 0x30），钉住偏移表的布局。
// 变异自证（实测）：22.26 的 UseWideStrings 写成假而红（stateprobeversions_test.go 同时红）。
func TestKnownVersionsSongRecordModel(t *testing.T) {
	for _, v := range songRecordVersions {
		o := knownVersions[v]
		narrow := v == "22.16" || v == "22.22"
		if o.UseWideStrings == narrow {
			t.Errorf("%s：UseWideStrings %v，want %v", v, o.UseWideStrings, !narrow)
		}
		if narrow && (o.SingerOff != o.NameOff+0x18 || o.AlbumOff != o.NameOff+0x30) {
			t.Errorf("%s：NameOff %#x、SingerOff %#x、AlbumOff %#x，want 三个相邻的 24 字节 std::string", v, o.NameOff, o.SingerOff, o.AlbumOff)
		}
	}
}
