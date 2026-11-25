package assoc

// 本文件只测大曲库下本地文件认定的候选（设计 §6.10「本地认定」、§7「大曲库」）：关联表有 5k 条本地条目时先留文件名包含
// 歌名的，超过 256 条再退回相等；isFile 只对留下的调用、至多 256 次；筛不下来即放弃，一次也不调用。对分的会话数上界
// （n = 256、k = 1 时不超过 17 个）由 localidentify_test.go 的 TestIdentifyLocalSessionBound 覆盖，这里不重复。

import (
	"fmt"
	"slices"
	"testing"
)

// libraryPath 是曲库里第 i 首的路径，文件名为「歌手 - 歌名」（本机 QQ 自动匹配的本地文件即这种写法，§2.2）。
func libraryPath(i int) string {
	return fmt.Sprintf(`E:\音乐\歌手%04d\歌手%04d - 歌曲%04d.flac`, i, i, i)
}

// library5k 返回 5000 条本地条目：named 依次散放在其中（第 k 个放在第 k·5000/len(named) 条），其余为 libraryPath。
func library5k(named ...string) []Entry {
	const n = 5_000
	paths := make([]string, 0, n)
	for i := range n - len(named) {
		paths = append(paths, libraryPath(i))
	}
	for k, p := range named {
		paths = slices.Insert(paths, k*n/len(named), p)
	}
	return localEntries(paths...)
}

// 变异自证（实测）：先查文件再按名字筛，各例 isFile 调用 5000 次以上而红。
func TestLocalCandidates5k(t *testing.T) {
	seabed := []string{`E:\音乐\一支榴莲\海底.flac`, `F:\备份\海底.MP3`, `\\nas\music\海底.flac`}
	tagged := []string{`E:\音乐\一支榴莲\一支榴莲 - 海底.flac`, `E:\音乐\海底 (Live).flac`, `E:\音乐\一支榴莲\海底.flac`}
	sameName := func(k int) []string {
		ps := make([]string, k)
		for i := range ps {
			ps[i] = fmt.Sprintf(`E:\版本%03d\海底.flac`, i)
		}
		return ps
	}
	// loveTagged 是 k 条文件名为「歌手 - 爱」的路径：包含「爱」而不相等。
	loveTagged := func(k int) []string {
		ps := make([]string, k)
		for i := range ps {
			ps[i] = fmt.Sprintf(`E:\音乐\歌手%03d\歌手%03d - 爱.flac`, i, i)
		}
		return ps
	}
	// loveExact 是 k 条不同目录下文件名恰为「爱」的路径。
	loveExact := func(k int) []string {
		ps := make([]string, k)
		for i := range ps {
			ps[i] = fmt.Sprintf(`E:\版本%03d\爱.flac`, i)
		}
		return ps
	}
	love298 := loveTagged(298)
	exact2 := []string{`E:\甲\爱.flac`, `F:\乙\爱.MP3`}
	allFiles := func(string) bool { return true }
	cases := []struct {
		name   string
		es     []Entry
		song   string
		isFile func(string) bool
		want   []string
		ok     bool
		calls  int
	}{
		// 第一层：文件名包含歌名。
		{"「歌手 - 歌名」、带后缀的、相等的 3 条：都留下", library5k(tagged...), "海底", allFiles, tagged, true, 3},
		// 第 3 版只认相等，这一例放弃（文件名为「歌手 - 歌名」的带标签歌认不出）；第 4 版按包含留下。
		{"文件名为「歌手 - 歌名」、与歌名不相等：留下", library5k(tagged[:2]...), "海底", allFiles, tagged[:2], true, 2},
		{"3 条文件名与歌名相同：只查这 3 条", library5k(seabed...), "海底", allFiles, seabed, true, 3},
		{"3 条相同、其一不是文件：查 3 次、留 2 条", library5k(seabed...), "海底", func(p string) bool { return p != seabed[1] },
			[]string{seabed[0], seabed[2]}, true, 3},
		{"ASCII 歌名不分大小写", library5k(`E:\音乐\LOVE song.mp3`, `E:\音乐\x\love.flac`), "Love", allFiles,
			[]string{`E:\音乐\LOVE song.mp3`, `E:\音乐\x\love.flac`}, true, 2},
		// Task 22 设计审的反例：「ΟΣ」与「Ος」EqualFold 相等、转小写后不含，第一层须留下它（第 3 版能认出的不能筛掉）。
		{"歌名「ΟΣ」、只有「Ος.flac」与它 EqualFold 相等：留下", library5k(`E:\音乐\Ος.flac`), "ΟΣ", allFiles,
			[]string{`E:\音乐\Ος.flac`}, true, 1},
		{"歌名为空：放弃", library5k(seabed...), "", allFiles, nil, false, 0},
		{"第一层一个不剩：放弃", library5k(), "海底", allFiles, nil, false, 0},

		// 第二层：第一层超过 256 条，退回相等。
		{"包含「爱」300 条、恰为「爱」2 条：留下这 2 条、保序",
			library5k(slices.Concat(love298[:100], exact2[:1], love298[100:200], exact2[1:], love298[200:])...), "爱", allFiles,
			exact2, true, 2},
		{"包含「爱」300 条、恰为「爱」0 条：放弃", library5k(loveTagged(300)...), "爱", allFiles, nil, false, 0},
		{"包含「爱」300 条、恰为「爱」257 条（不同目录）：放弃", library5k(slices.Concat(loveTagged(43), loveExact(257))...), "爱", allFiles,
			nil, false, 0},
		{"300 条与歌名相同：放弃", library5k(sameName(300)...), "海底", allFiles, nil, false, 0},
		{"257 条与歌名相同：放弃", library5k(sameName(257)...), "海底", allFiles, nil, false, 0},
		// 已知限制（设计 §8）：正在放的是带标签的「张三 - 爱」（QQ 歌名「爱」、歌手「张三」），包含「爱」的 300 条里没有恰为
		// 「爱」的，退回相等后一个不剩、放弃。不用路径含歌手再缩小一层（主会话 Ruling 37 补充）。
		{"已知限制：「张三 - 爱」在包含「爱」的 300 条里、没有恰为「爱」的：放弃",
			library5k(append(loveTagged(299), `E:\张三\张三 - 爱.flac`)...), "爱", allFiles, nil, false, 0},

		// 边界：恰 256 条包含不进第二层；257 条包含即进第二层。
		{"恰 256 条包含、没有相等的：全留、不退回相等", library5k(loveTagged(256)...), "爱", allFiles, loveTagged(256), true, 256},
		{"恰 256 条与歌名相同：查 256 次、全留", library5k(sameName(256)...), "海底", allFiles, sameName(256), true, 256},
		{"257 条包含、没有相等的：放弃、不查文件", library5k(loveTagged(257)...), "爱", allFiles, nil, false, 0},
	}
	for _, c := range cases {
		if len(c.es) != 5_000 {
			t.Fatalf("%s：夹具 %d 条，want 5000", c.name, len(c.es))
		}
		calls := 0
		isFile := func(p string) bool { calls++; return c.isFile(p) }
		got, ok := LocalCandidates(c.es, c.song, "", isFile)
		if ok != c.ok || !slices.Equal(got, c.want) || calls != c.calls {
			t.Errorf("%s：得到 (%d 个, %v)、isFile 调用 %d 次，want (%d 个, %v)、%d 次", c.name, len(got), ok, calls, len(c.want), c.ok, c.calls)
		}
		if calls > maxLocalCandidates {
			t.Errorf("%s：isFile 调用 %d 次，超过 %d", c.name, calls, maxLocalCandidates)
		}
	}
}
