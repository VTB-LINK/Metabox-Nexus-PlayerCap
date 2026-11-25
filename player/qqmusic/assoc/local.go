package assoc

import (
	"path/filepath"
	"slices"
	"strings"

	"Metabox-Nexus-PlayerCap/i18n"
)

const maxLocalCandidates = 256

// ErrAmbiguous 表示不止一个候选被 QQ 打开，按歌名消歧后不剩恰好一个（设计 §6.5 第 3 步），由调用方打一条 Detail。
var ErrAmbiguous = i18n.Sentinel("several candidate files are open in QQ Music; cannot tell which one is playing")

// LocalCandidates 选出本地文件认定的候选（设计 §6.5 第 1 步）：本地文件条目的音频路径，按路径不区分大小写去重
// （保留首次出现的写法与顺序），排除 exclude（上一首采纳过的本地文件，不区分大小写）。此时超过 256 个则按歌名 name 筛：
// 先只保留文件名包含歌名的（nameContains），仍超过 256 个再退回只保留其中文件名与歌名相等的（nameEquals）；name 为空、
// 一个不剩或仍超过 256 个则第二个返回值为假，放弃本次认定。筛子只决定交给占用查询的路径，认定仍以被 QQ 打开为准；
// 候选至多 256 个，IdentifyLocal 的会话至多 1 + 2k·⌈log₂n⌉ 个且不超过 2n − 1（n 为候选数，k 为其中被 QQ 打开的个数）。
// 最后去掉 isFile 为假的（不存在、是目录）；isFile 只对筛剩的调用，至多 256 次。
func LocalCandidates(es []Entry, name, exclude string, isFile func(string) bool) ([]string, bool) {
	seen := map[string]bool{strings.ToLower(exclude): true}
	var cands []string
	for _, e := range es {
		k := strings.ToLower(e.SongURL)
		if e.Kind != KindLocal || seen[k] {
			continue
		}
		seen[k] = true
		cands = append(cands, e.SongURL)
	}
	if len(cands) > maxLocalCandidates {
		cands = slices.DeleteFunc(cands, func(p string) bool { return !nameContains(p, name) })
		if len(cands) > maxLocalCandidates {
			cands = slices.DeleteFunc(cands, func(p string) bool { return !nameEquals(p, name) })
		}
		if len(cands) == 0 || len(cands) > maxLocalCandidates {
			return nil, false
		}
	}
	return slices.DeleteFunc(cands, func(p string) bool { return !isFile(p) }), true
}

// Holders 查询打开 paths 中任一文件的进程号，每次调用为一个 Restart Manager 会话；与 restartmgr.Holders 签名相同。
type Holders func(paths ...string) ([]uint32, error)

// IdentifyLocal 从候选中认定 QQ（进程号 qqPID）正在播放的文件（设计 §6.5 第 2、3 步）。
//
// 先一次查询全部候选；占用者含 QQ 时对半分开、各查一次，只对含 QQ 的一半继续对分，直到单个文件，得出被 QQ 打开的
// 全部候选。查询依次进行，候选 n 个、其中 k 个被 QQ 打开时至多 1 + 2k·⌈log₂n⌉ 次，且不超过 2n − 1（全部被打开时
// 对分树的节点数）。恰好一个被打开即认定；不止一个时只保留文件名与歌名 name 相等的（nameEquals），恰好剩一个才认定，
// 否则返回 ErrAmbiguous。消歧不用 nameContains（设计 §6.5 第 3 步）：两份都被打开、正在放的文件名不含歌名（如带标签的
// track01）而另一份包含时，按包含会认成另一份；准确优先，宁可认不出也不认错。
// held 是对分得出的被 QQ 打开的候选数（消歧之前），供调用方记入 Detail。认定时 path 非空；消歧不成为 ("", held, ErrAmbiguous)；
// 一个也没有被打开、候选为空（此时不查询）为 ("", 0, nil)。holders 的错误原样返回、不认定（held 为 0），由调用方分类。
func IdentifyLocal(cands []string, name string, qqPID uint32, holders Holders) (path string, held int, err error) {
	if len(cands) == 0 {
		return "", 0, nil
	}
	open, err := heldBy(cands, qqPID, holders)
	if err != nil {
		return "", 0, err
	}
	held = len(open)
	if held > 1 {
		open = slices.DeleteFunc(open, func(p string) bool { return !nameEquals(p, name) })
		if len(open) != 1 {
			return "", held, ErrAmbiguous
		}
	}
	if held == 0 {
		return "", 0, nil
	}
	return open[0], held, nil
}

// heldBy 对分查询，返回 paths（非空）中被进程 pid 打开的那些，按 paths 的顺序。返回的切片另行分配，不与 paths 共用底层数组。
func heldBy(paths []string, pid uint32, holders Holders) ([]string, error) {
	pids, err := holders(paths...)
	if err != nil || !slices.Contains(pids, pid) {
		return nil, err
	}
	if len(paths) == 1 {
		return []string{paths[0]}, nil
	}
	a, err := heldBy(paths[:len(paths)/2], pid, holders)
	if err != nil {
		return nil, err
	}
	b, err := heldBy(paths[len(paths)/2:], pid, holders)
	if err != nil {
		return nil, err
	}
	return append(a, b...), nil
}

// stem 返回 path 的文件名去掉扩展名，不含目录。
func stem(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// nameEquals 报告 path 的文件名（去扩展名）是否与歌名 name 不区分大小写相等（strings.EqualFold）；name 为空时为假。
func nameEquals(path, name string) bool {
	return name != "" && strings.EqualFold(stem(path), name)
}

// nameContains 报告 path 的文件名（去扩展名，不看目录）是否包含歌名 name：nameEquals 为真，或两者转小写后文件名含歌名；
// name 为空时为假。前一半不能省：EqualFold 按 Unicode 大小写折叠比较，转小写在少数字母上与它不一致（如希腊文词尾 ς：
// 「Ος」与「ΟΣ」EqualFold 相等，转小写为「ος」与「οσ」，不含），省了会筛掉 nameEquals 认得的文件（设计 §6.5 第 1 步）。
func nameContains(path, name string) bool {
	return name != "" && (nameEquals(path, name) || strings.Contains(strings.ToLower(stem(path)), strings.ToLower(name)))
}
