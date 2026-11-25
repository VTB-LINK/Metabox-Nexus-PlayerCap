package assoc

// 本文件只测 LocalCandidates：从关联表选出本地文件认定的候选（设计 §6.5 第 1 步），含查文件次数的上限，
// 以及它所用的名字比较 nameEquals、nameContains。

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"unicode"
)

func TestNameEqualsContains(t *testing.T) {
	cases := []struct {
		path, name       string
		equals, contains bool
	}{
		{`E:\a\海底.flac`, "海底", true, true},
		{`E:\音乐\一支榴莲 - 海底.flac`, "海底", false, true},
		{`E:\音乐\海底 (Live).flac`, "海底", false, true},
		{`E:\m\Love Song.MP3`, "love", false, true},
		{`E:\m\LOVE.mp3`, "love", true, true},
		// EqualFold 相等；转小写后为「ος」与「οσ」，不含，靠 nameEquals 一支入选。
		{`E:\m\Ος.flac`, "ΟΣ", true, true},
		// 扩展名不参与：去扩展名得「海底.flac」。
		{`E:\m\海底.flac.bak`, "海底", false, true},
		{`E:\m\海底`, "海底", true, true},
		// 反例：只有目录名含歌名、文件名短于歌名、中间隔着空格、歌名为空。
		{`E:\海底\track01.flac`, "海底", false, false},
		{`E:\m\海.flac`, "海底", false, false},
		{`E:\m\海 底.flac`, "海底", false, false},
		{`E:\a\海底.flac`, "", false, false},
		{`E:\m\.flac`, "", false, false},
	}
	for _, c := range cases {
		if got := nameEquals(c.path, c.name); got != c.equals {
			t.Errorf("nameEquals(%q, %q) = %v，want %v", c.path, c.name, got, c.equals)
		}
		if got := nameContains(c.path, c.name); got != c.contains {
			t.Errorf("nameContains(%q, %q) = %v，want %v", c.path, c.name, got, c.contains)
		}
	}

	// 性质：nameEquals 为真则 nameContains 为真。文件名由歌名逐字取 Unicode 折叠轨道上的随机一员、再随机加前后缀，
	// 字母表含希腊文 Σσς、长 s（ſ）、微符号（µ）等转小写与 EqualFold 不一致的字母，固定种子。
	r := rand.New(rand.NewPCG(22, 22))
	alphabet := []rune("aAsSſkKΣσςΟοµΜμ海底 -")
	pick := func(n int) string {
		var b strings.Builder
		for range n {
			b.WriteRune(alphabet[r.IntN(len(alphabet))])
		}
		return b.String()
	}
	fold := func(s string) string {
		var b strings.Builder
		for _, c := range s {
			orbit := []rune{c}
			for f := unicode.SimpleFold(c); f != c; f = unicode.SimpleFold(f) {
				orbit = append(orbit, f)
			}
			b.WriteRune(orbit[r.IntN(len(orbit))])
		}
		return b.String()
	}
	diverged := 0 // EqualFold 相等而转小写后不含的样本数：为 0 则本性质没有测到 nameEquals 一支。
	var bad []string
	for range 5000 {
		name := pick(1 + r.IntN(4))
		file := fold(name) // 文件名去扩展名
		if r.IntN(2) == 0 {
			file = pick(r.IntN(3)) + file + pick(r.IntN(3))
		}
		path := `E:\d\` + file + ".flac"
		if strings.EqualFold(file, name) && !strings.Contains(strings.ToLower(file), strings.ToLower(name)) {
			diverged++
		}
		if nameEquals(path, name) && !nameContains(path, name) {
			bad = append(bad, fmt.Sprintf("(%q, %q)", path, name))
		}
	}
	if len(bad) > 0 {
		t.Errorf("nameEquals 为真而 nameContains 为假：%d 例，如 %s", len(bad), bad[0])
	}
	if diverged == 0 {
		t.Fatal("随机样本里没有 EqualFold 相等而转小写后不含的，性质断言没有测到 nameEquals 一支")
	}
}

// localEntries 为每个路径造一条本地文件条目。
func localEntries(paths ...string) []Entry {
	es := make([]Entry, len(paths))
	for i, p := range paths {
		es[i] = mkEntry(p, p+".qrc", false, 0)
	}
	return es
}

// numbered 返回 n 个互不相同、文件名都不是 "Seabed" 的路径。
func numbered(n int) []string {
	ps := make([]string, n)
	for i := range ps {
		ps[i] = fmt.Sprintf(`E:\m\f%03d.flac`, i)
	}
	return ps
}

func TestLocalCandidates(t *testing.T) {
	allFiles := func(string) bool { return true }
	notFile := func(ps ...string) func(string) bool {
		return func(p string) bool { return !slices.Contains(ps, p) }
	}
	seabed := []string{`E:\a\Seabed.flac`, `E:\b\SEABED.mp3`, `\\srv\s\seabed.FLAC`}
	// 文件名（去扩展名）包含 "Seabed" 而不相等的。
	contains := []string{`E:\m\Seabed (Live).flac`, `E:\m\Seabed.flac.bak`, `E:\m\xSeabed.flac`, `E:\m\Singer - seabed.flac`}
	// 文件名不包含 "Seabed" 的反例：只有目录名含、少一个字母、中间隔着空格。
	nearMiss := []string{`E:\Seabed\f.flac`, `E:\m\Seabe.flac`, `E:\m\Sea bed.flac`}
	mixed := slices.Concat(nearMiss, contains, numbered(257-len(seabed)-len(contains)-len(nearMiss)))
	mixed = slices.Insert(mixed, 100, seabed...)
	kept := slices.Concat(contains, seabed) // mixed 里包含 "Seabed" 的，按出现顺序
	sameName := make([]string, 257)
	for i := range sameName {
		sameName[i] = fmt.Sprintf(`E:\d%03d\Seabed.flac`, i)
	}

	cases := []struct {
		name    string
		es      []Entry
		song    string
		exclude string
		isFile  func(string) bool
		want    []string
		ok      bool
	}{
		{"路径不区分大小写去重、保留首次的写法与顺序",
			localEntries(`E:\Music\A.flac`, `E:\Music\B.flac`, `e:\music\a.FLAC`, `E:\MUSIC\B.FLAC`), "", "", allFiles,
			[]string{`E:\Music\A.flac`, `E:\Music\B.flac`}, true},
		{"排除上一首（大小写不同仍排除）",
			localEntries(`E:\Music\A.flac`, `E:\Music\B.flac`), "", `e:\MUSIC\a.FLAC`, allFiles,
			[]string{`E:\Music\B.flac`}, true},
		{"排除的不是候选：照常",
			localEntries(`E:\Music\A.flac`), "", `E:\Music\C.flac`, allFiles,
			[]string{`E:\Music\A.flac`}, true},
		{"不是普通文件的去掉",
			localEntries(`E:\Music\A.flac`, `E:\Music\missing.flac`, `E:\Music\dir`), "", "", notFile(`E:\Music\missing.flac`, `E:\Music\dir`),
			[]string{`E:\Music\A.flac`}, true},
		{"在线与其余写法的条目不算",
			[]Entry{
				mkEntry(`http://stream4.qqmusic.qq.com/000AbCd.wma`, `E:\l\x.qrc`, true, 0),
				mkEntry(`http://stream4.qqmusic.qq.com/123.wma`, `E:\l\y.qrc`, true, 0),
				mkEntry(`a.flac`, `E:\l\z.qrc`, true, 0),
				mkEntry(`E:\Music\A.flac`, `E:\l\a.qrc`, false, 0),
			}, "", "", allFiles,
			[]string{`E:\Music\A.flac`}, true},
		{"没有本地条目：空、不放弃", localEntries(), "Seabed", "", allFiles, nil, true},
		{"全被去掉：空、不放弃", localEntries(`E:\Music\A.flac`), "", "", notFile(`E:\Music\A.flac`), nil, true},
		{"恰好 256 个：不按名字筛", localEntries(numbered(256)...), "Seabed", "", allFiles, numbered(256), true},
		{"不超过 256 个：文件名不含歌名的也全留", localEntries(slices.Concat(nearMiss, contains[:1], seabed[:1])...), "Seabed", "", allFiles,
			slices.Concat(nearMiss, contains[:1], seabed[:1]), true},
		{"257 个且无名字包含歌名：放弃", localEntries(numbered(257)...), "Seabed", "", allFiles, nil, false},
		{"257 个、歌名为空：放弃", localEntries(append(numbered(256), `E:\m\.flac`)...), "", "", allFiles, nil, false},
		{"257 个中 7 个包含歌名：只留这 7 个、保序", localEntries(mixed...), "Seabed", "", allFiles, kept, true},
		{"名字相等的 257 个：放弃", localEntries(sameName...), "Seabed", "", allFiles, nil, false},
		{"257 个去重后 256 个：不筛", localEntries(append(numbered(256), `e:\M\F000.FLAC`)...), "Seabed", "", allFiles, numbered(256), true},
		{"257 个排除一个后 256 个：不筛", localEntries(numbered(257)...), "Seabed", `E:\m\f256.flac`, allFiles, numbered(256), true},
		// 上限按查文件之前的个数计：其中有非文件也先按名字筛。
		{"257 个含一个非文件、无名字包含歌名：放弃", localEntries(numbered(257)...), "Seabed", "", notFile(`E:\m\f000.flac`), nil, false},
		{"257 个中 7 个包含歌名、其一不是文件：留 6 个", localEntries(mixed...), "Seabed", "", notFile(seabed[1]),
			slices.DeleteFunc(slices.Clone(kept), func(p string) bool { return p == seabed[1] }), true},
		{"257 个中 7 个包含歌名、都不是文件：空、不放弃", localEntries(mixed...), "Seabed", "", notFile(kept...), nil, true},
	}
	for _, c := range cases {
		calls := 0
		isFile := func(p string) bool { calls++; return c.isFile(p) }
		got, ok := LocalCandidates(c.es, c.song, c.exclude, isFile)
		if ok != c.ok || !slices.Equal(got, c.want) {
			t.Errorf("%s：得到 (%d 个 %q, %v)，应为 (%d 个, %v)", c.name, len(got), got, ok, len(c.want), c.ok)
		}
		if calls > maxLocalCandidates {
			t.Errorf("%s：isFile 调用 %d 次，超过 %d", c.name, calls, maxLocalCandidates)
		}
	}
}

// 本地条目极多时先按名字筛，isFile 只对筛剩的调用；筛不下来则放弃且一次也不调用。
func TestLocalCandidatesStatBound(t *testing.T) {
	seabed := []string{`E:\a\Seabed.flac`, `E:\b\SEABED.mp3`, `\\srv\s\seabed.FLAC`}
	paths := make([]string, 0, maxEntries)
	for i := range maxEntries - len(seabed) {
		paths = append(paths, fmt.Sprintf(`E:\m\f%06d.flac`, i))
	}
	paths = slices.Insert(paths, 50_000, seabed...)

	var called []string
	isFile := func(p string) bool { called = append(called, p); return true }
	got, ok := LocalCandidates(localEntries(paths...), "Seabed", "", isFile)
	if !ok || !slices.Equal(got, seabed) || !slices.Equal(called, seabed) {
		t.Errorf("3 个名字相符：得到 (%q, %v)，isFile 调用 %d 次 %q；应只对这 3 个各调用一次并返回它们", got, ok, len(called), called)
	}

	called = nil
	got, ok = LocalCandidates(localEntries(paths...), "Fensheng", "", isFile)
	if ok || got != nil || len(called) != 0 {
		t.Errorf("无名字相符：得到 (%d 个, %v)，isFile 调用 %d 次；应放弃且不调用", len(got), ok, len(called))
	}
}
