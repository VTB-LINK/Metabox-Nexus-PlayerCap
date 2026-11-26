package assoc

// 本文件只测 pick 依次试候选（设计 §6.5 第 3、4 步）：读不到为暂缺、试下一份并带待重试；超限或零行为
// 不可用、试下一份、不带待重试；第一份解析出行的即结果，其后的不再读。

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"testing"

	klyric "Metabox-Nexus-PlayerCap/player/kugou/lyric"
)

// fakeRead 按路径返回预设的内容或错误，并记下所读路径；没预设的路径视为不存在。
type fakeRead struct {
	files map[string]any // string 为内容，error 为读取错误
	read  []string
}

func (f *fakeRead) fn(p string) ([]byte, error) {
	f.read = append(f.read, p)
	switch v := f.files[p].(type) {
	case string:
		return []byte(v), nil
	case error:
		return nil, v
	}
	return nil, os.ErrNotExist
}

func TestPickMissingSetsRetry(t *testing.T) {
	missing := []error{
		os.ErrNotExist,
		&fs.PathError{Op: "open", Path: `D:\a.krc`, Err: errors.New("The process cannot access the file")},
		fmt.Errorf("read: %w", os.ErrPermission),
	}
	for _, err := range missing {
		f := &fakeRead{files: map[string]any{`D:\a.krc`: err, `D:\cache.lrc`: testLRC}}
		r := pick(collected{cands: []candidate{{Path: `D:\a.krc`, Source: SourceAssociation, UpdateTime: 2}, {Path: `D:\cache.lrc`, Source: SourceCache, UpdateTime: 1}}}, "", f.fn)
		if r.Kind != KindFound || r.Source != SourceCache || !r.Retry || len(r.Lines) != 2 || r.Hash != "" {
			t.Errorf("关联读出 %v：结果 %+v，want KindFound、SourceCache、Retry、Hash 为空", err, r)
		}
	}
}

func TestPickAllMissingNoneWithRetry(t *testing.T) {
	f := &fakeRead{files: map[string]any{`D:\b.krc`: errors.New("I/O error")}}
	r := pick(collected{cands: []candidate{{Path: `D:\a.krc`, Source: SourceAssociation, UpdateTime: 2}, {Path: `D:\b.krc`, Source: SourceCache, UpdateTime: 1}}}, "", f.fn)
	if r.Kind != KindNone || !r.Retry || r.Lines != nil || r.Source != 0 || len(f.read) != 2 {
		t.Fatalf("结果 %+v，读了 %q；want KindNone、Retry、两份都试过", r, f.read)
	}
}

// 读取报超限（含被包装的）、不是磁盘文件（含被包装的，如设备路径）、内容超过文件上限、零行（空文件、只有标签、
// 乱码）：都不可用，结果为没有且不待重试。没有候选同样为没有、不待重试。
//
// 变异自证（实测）：pick 不认 errNotDiskFile，本用例红（Retry 为真）。
func TestPickUnusableNoRetry(t *testing.T) {
	f := &fakeRead{files: map[string]any{
		`D:\1`: klyric.ErrLyricTooLarge,
		`D:\2`: fmt.Errorf("read D:\\2: %w", klyric.ErrLyricTooLarge),
		`D:\3`: strings.Repeat("[00:01.00]x\n", klyric.MaxLocalFileBytes/12+1),
		`D:\4`: "",
		`D:\5`: "[ti:标题]\n[ar:歌手]\n",
		`D:\6`: "\x00\x01\x02krc",
		`D:\7`: errNotDiskFile,
		`D:\8`: fmt.Errorf("open \\\\.\\COM1: %w", errNotDiskFile),
	}}
	var cands []candidate
	for i := 1; i <= 8; i++ {
		cands = append(cands, candidate{Path: fmt.Sprintf(`D:\%d`, i), Source: SourceCache, UpdateTime: int64(i)})
	}
	r := pick(collected{cands: cands}, "", f.fn)
	if r.Kind != KindNone || r.Retry || r.Lines != nil || len(f.read) != 8 {
		t.Fatalf("结果 %+v，读了 %q；want KindNone、不待重试、八份都试过", r, f.read)
	}
	if r := pick(collected{}, `C:\KuGou\Lyric`, f.fn); r.Kind != KindNone || r.Retry {
		t.Fatalf("没有候选：结果 %+v", r)
	}
}

// 第一份零行、第二份有行：取第二份，来源随第二份。其后的候选（含会暂缺的）不再读，不因之带待重试。
func TestPickFirstParsable(t *testing.T) {
	f := &fakeRead{files: map[string]any{
		`D:\empty.krc`: "[ti:x]\n",
		`D:\good.lrc`:  "[00:03.00]第二份\n",
		`D:\later.lrc`: testLRC,
	}}
	cands := []candidate{
		{Path: `D:\empty.krc`, Source: SourceAssociation, UpdateTime: 3},
		{Path: `D:\good.lrc`, Source: SourceAssociation, UpdateTime: 2},
		{Path: `D:\missing.lrc`, Source: SourceCache, UpdateTime: 9},
		{Path: `D:\later.lrc`, Source: SourceCache, UpdateTime: 1},
	}
	r := pick(collected{cands: cands}, "", f.fn)
	if r.Kind != KindFound || r.Source != SourceAssociation || r.Retry || len(r.Lines) != 1 || r.Lines[0].Text != "第二份" {
		t.Fatalf("结果 %+v，want 第二份、SourceAssociation、不待重试", r)
	}
	if !slices.Equal(f.read, []string{`D:\empty.krc`, `D:\good.lrc`}) {
		t.Fatalf("读了 %q，want 只读前两份", f.read)
	}
}
