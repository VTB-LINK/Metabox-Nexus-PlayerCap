// Package assoc 取酷狗为当前歌曲显示的本地歌词：手动关联的歌词文件或酷狗的歌词缓存（设计 §6.5、§6.7）。
package assoc

// 本文件是解析规则（设计 §6.5）：由库内的表收集候选（collect），再依次读文件取第一份解析出行的（pick）。
// 表与文件读取都由调用方注入，本文件不碰文件系统。

import (
	"cmp"
	"errors"
	"path/filepath"
	"slices"
	"strings"

	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/logger"
	"Metabox-Nexus-PlayerCap/player/kugou/kgdb"
	klyric "Metabox-Nexus-PlayerCap/player/kugou/lyric"
)

var log = logger.New("KuGou] [Assoc") // 渲染为 [KuGou] [Assoc]

// maxCandidates 是关联与缓存各一级的候选上限（设计 §6.10）。
const maxCandidates = 64

// maxPathBytes 是库里留存的一个 LyricPath 或 LyricKey 的字节数上限（设计 §6.10）。Windows 路径至多
// 32767 个 UTF-16 单元，每个单元转成 UTF-8 至多 3 字节（代理对两个单元合 4 字节），超过它的不是有效路径；
// LyricKey 正常为 32 位十六进制，同用此限。超长的行跳过，不打日志（畸形库可让每行都超长）。
// 单条记录至多 4 MiB，不设此限时留存的候选与关联键可达 64 × 4 MiB。
const maxPathBytes = 32767 * 3

const (
	maxAbsLyricOffsetMs = 600_000
	unassocMenuKey      = "419721cb5e7bf802fd14b272f5d5322e" // 右键菜单取消关联：酷狗重播仍保持取消状态
	unassocDialogKey    = "a9695a2f209c200cd9d393fa58d5f915" // 关联弹窗取消关联：酷狗仅当次播放清词，重播回落缓存
)

// clampLyricOffsetMs 将酷狗歌词偏移钳位在 [-600_000, +600_000] 毫秒范围内。
func clampLyricOffsetMs(v int) int {
	if v > maxAbsLyricOffsetMs {
		return maxAbsLyricOffsetMs
	}
	if v < -maxAbsLyricOffsetMs {
		return -maxAbsLyricOffsetMs
	}
	return v
}

// Kind 是一次解析的结局。
type Kind uint8

const (
	KindNone         Kind = iota // 无本地可读歌词文件，调用方走在线取词（可能携带内存 LyricOffsetMs 与 HasMemoryLyric）
	KindFound                    // 取到本地或缓存歌词行（携带 LyricOffsetMs）
	KindUnassociated             // 用户显式取消关联：清空歌词且禁止回退缓存/在线/同名复用
	KindTransient                // 暂时失败
	KindNotConfirmed             // 尚未确认
	KindDisabled                 // 本会话停用本地读取
)

// Source 是歌词的来源。
type Source uint8

const (
	SourceAssociation Source = iota + 1 // 手动关联
	SourceCache                         // 酷狗的歌词缓存（含自动匹配）
)

// String 返回来源名的英文原文，其余值为空串。注入日志时由调用方 i18n.T（AGENTS.md §6.4 ⑤）。
func (s Source) String() string {
	switch s {
	case SourceAssociation:
		return "KuGou association"
	case SourceCache:
		return "KuGou cache"
	}
	return ""
}

// Result 是对一个播放 hash 的解析结果。Retry 为真表示结果之前（结果为 KindNone 时即全部候选中）
// 有文件暂缺，稍后重试可能得到更优先的歌词（设计 §6.5 第 4 步）。
//
// AssocSkipped 为真表示「关联未取用」：第 1 步得出了关联键，而结果不是关联（缓存或 KindNone），即键在 LyricInfo
// 里没有行、文件暂缺或文件不可用（设计 §6.5 第 4 步）。第 1 步没有关联键（含超过 maxCandidates 个而不用关联一级）
// 时为假。PlayedAt 是正在播放的队列行 P 的 LastPlayTime，没有 P 为 0，作为「哪一次播放」的标识：同一行每次开始
// 播放都会更新它，换歌拍误选的 P 与落盘后的 P 也不同（设计 §6.5 第 4 步）。会话据这两项在同一次播放中保持来自
// 关联的当前歌词（设计 §6.7「播放中」）。
//
// LocalFile 为真表示「本地文件」：有 P、且 P 的 MusicFilePath 为绝对路径（盘符或 UNC，按 Windows 规则），即酷狗正在
// 播放的是磁盘上的文件。只看 P：同一 hash 可能另有磁盘文件的行，正在播放的是在线条目还是磁盘文件只由 P 决定。在线
// 歌曲的行是「歌手 - 歌名.mp3」形式的文件名，不带。会话据它决定同名复用与播放中清词（设计 §6.7）。
//
// 没有走到 pick 的结果（暂时失败、未确认、停用）以上三项都为零值。
type Result struct {
	Hash           string
	Kind           Kind
	Lines          []klyric.Line
	LyricOffsetMs  int  // 酷狗侧歌词偏移（毫秒，正数提前、负数延后，钳位在 [-600_000, +600_000]）
	HasMemoryLyric bool // 内存模式下酷狗 CLyric 已加载歌词行但磁盘路径为空或不可读，供会话触发在线恢复
	MemoryNoLyric  bool // 内存模式下酷狗 CLyric 已置空或明确判定无歌词（区别于外部磁盘文件暂缺），供会话撤销双拍超窗误采纳的旧词并恢复在线取词
	Source         Source
	FromMemory     bool // 由内存只读读取模式产出（区分 local_assoc_mem 与 local_assoc_db）
	Retry          bool
	AssocSkipped   bool
	PlayedAt       int64
	LocalFile      bool
}

// tables 是 collect 所需的读库接口，*kgdb.DB 满足它。
type tables interface {
	Tables() []string
	Columns(table string) ([]string, error)
	Scan(table string, cols []string, fn func(row []kgdb.Value) bool) error
}

var _ tables = (*kgdb.DB)(nil)

// candidate 是一份待读的歌词文件。Path 为 LyricInfo.LyricPath 原文：绝对路径，或相对歌词目录的路径。
type candidate struct {
	Path         string
	Source       Source
	UpdateTime   int64
	RealOffsetMs int
}

// collected 是 collect 的结局：按次序的候选；keyed 为第 1 步得出了关联键（键在 LyricInfo 里没有行的也算）；
// playedAt 为 P 的 LastPlayTime，没有 P 为 0；localFile 为 P 的 MusicFilePath 是绝对路径；
// menuUnassoc 为 P 命中右键菜单取消关联哨兵；pKey 为 P 的规范化 LyricKey。
type collected struct {
	cands       []candidate
	keyed       bool
	playedAt    int64
	localFile   bool
	menuUnassoc bool
	pKey        string
}

// errSchema 表示取不到 LyricInfo 表或其必需的列，即酷狗的表结构不是认得的这一版（设计 §6.5 末）。
func errSchema(cause error) error {
	return i18n.Errorf("%w: KuGou lyric table layout not recognized: %v", kgdb.ErrUnsupportedDB, cause)
}

// isPlayQueue 判断 t 是否为播放队列表：表名以 PlayQueueAssistItem 开头，不分大小写；
// PlayQueueBackupItems_N 等其余表都不算（设计 §6.5 第 1 步）。
func isPlayQueue(t string) bool {
	return strings.HasPrefix(strings.ToLower(t), "playqueueassistitem")
}

// collect 按设计 §6.5 第 1、2 步收集 hash 的歌词候选：关联一级在前、缓存一级在后，各按 UpdateTime 降序。
// 关联一级取哪些 LyricKey 见 associationKeys。
// hash 去两端空白后不分大小写比较，为空或全 0 时没有候选、不扫表：全 0 是换歌过渡态的播放 hash，
// 而手动关联的 LyricInfo 行 SongHash 也是全 0，照常比较会把它们全当成缓存候选。
//
// 先扫歌曲条目表得出关联键（associationKeys，已去重）；再扫一遍 LyricInfo，Key 在其中的行为关联，其余
// SongHash 相符的行为缓存。关联键超过 maxCandidates 个时不用关联一级：此时还没有 UpdateTime，不在不完整的
// 集合里挑最新。缓存一级只留 UpdateTime 最大的 maxCandidates 条。两处各打一条 Detail。LyricPath 超过
// maxPathBytes 的行在取用之前跳过，两级都不计。结局另带第 1 步是否得出了关联键、P 的 LastPlayTime 与 P 是否为
// 本地文件（collected）。P 的路径为空、缺列或超长（filePath 记为空）时不算本地文件。
//
// 找表与扫表出错的处理见 eachSongTable；LyricInfo 不存在、缺列或种类不支持返回 errSchema，其余错误原样返回。
func collect(db tables, hash string) (collected, error) {
	h := strings.TrimSpace(hash)
	if strings.Trim(h, "0") == "" {
		return collected{}, nil
	}
	keys, p, err := associationKeys(db, h)
	if err != nil {
		return collected{}, err
	}
	res := collected{keyed: len(keys) > 0, playedAt: p.played, localFile: filepath.IsAbs(p.path), pKey: p.key}
	if p.played > 0 && p.key == unassocMenuKey {
		res.menuUnassoc = true
		return res, nil
	}
	cols, err := db.Columns("LyricInfo")
	if errors.Is(err, kgdb.ErrNoTable) || errors.Is(err, kgdb.ErrUnsupportedTable) {
		return collected{}, errSchema(err)
	}
	if err != nil {
		return collected{}, err
	}
	hasRealOffset := hasColumn(cols, "RealOffset")
	scanCols := []string{"Key", "LyricPath", "SongHash", "UpdateTime"}
	if hasRealOffset {
		scanCols = append(scanCols, "RealOffset")
	}
	var manual, cache []candidate
	cacheRows := 0
	err = db.Scan("LyricInfo", scanCols, func(row []kgdb.Value) bool {
		if len(row[1].Bytes) > maxPathBytes {
			return true
		}
		realOffsetMs := 0
		if hasRealOffset {
			realOffsetMs = clampLyricOffsetMs(int(max(min(row[4].Int64(), maxAbsLyricOffsetMs), -maxAbsLyricOffsetMs)))
		}
		c := candidate{Path: row[1].String(), UpdateTime: row[3].Int64(), RealOffsetMs: realOffsetMs}
		if k := lyricKey(row[0]); keys[k] {
			delete(keys, k) // Key 是主键；库异常时同一 Key 有多行也只取第一行，关联一级不超过 maxCandidates 条
			c.Source = SourceAssociation
			manual = append(manual, c)
		} else if sameHash(row[2], h) || (p.played > 0 && p.musicHash != "" && sameHash(row[2], p.musicHash)) {
			c.Source = SourceCache
			cache = keepLatest(cache, c)
			cacheRows++
		}
		return true
	})
	if errors.Is(err, kgdb.ErrNoTable) || errors.Is(err, kgdb.ErrNoColumn) || errors.Is(err, kgdb.ErrUnsupportedTable) {
		return collected{}, errSchema(err)
	}
	if err != nil {
		return collected{}, err
	}
	if cacheRows > maxCandidates {
		log.Detail("Hash %s has %d KuGou cache lyric entries; keeping the latest %d", h, cacheRows, maxCandidates)
	}
	newestFirst := func(a, b candidate) int { return cmp.Compare(b.UpdateTime, a.UpdateTime) }
	slices.SortStableFunc(manual, newestFirst)
	slices.SortStableFunc(cache, newestFirst)
	res.cands = append(manual, cache...)
	return res, nil
}

// playing 是正在播放的队列行 P（设计 §6.5 第 1 步）。played 为其 LastPlayTime，为 0 即没有 P；key 为其
// LyricKey（经 lyricKey 规范化），空即不带关联；path 为其 MusicFilePath（经 filePath 规范化）；
// musicHash 为其规范化非零 MusicFileHash（长度 <= 64），空即无有效默认音质 Hash。
type playing struct {
	played    int64
	key, path string
	musicHash string
}

const maxHashBytes = 64

// extraHashCols 是歌曲条目表中除 MusicFileHash 之外用于多音质/MV 流式播放的可选 Hash 列。
var extraHashCols = [...]string{"LastPlayedMusicHash", "MVFileHash", "LastPlayedMVHash"}

// appendExtraHashCols 按需把 cols 中存在的可选多音质/MV Hash 列追加进 scanCols，返回可选列起始下标。
func appendExtraHashCols(scanCols, cols []string) ([]string, int) {
	start := len(scanCols)
	for _, name := range extraHashCols {
		if hasColumn(cols, name) {
			scanCols = append(scanCols, name)
		}
	}
	return scanCols, start
}

// rowMatchesHash 判断 row[0]（MusicFileHash）或 row[extraStart:]（可选多音质/MV 列）任一列与已规范化的 h 相符。
func rowMatchesHash(row []kgdb.Value, extraStart int, h string) bool {
	if sameHash(row[0], h) {
		return true
	}
	for _, v := range row[extraStart:] {
		if sameHash(v, h) {
			return true
		}
	}
	return false
}

// normMusicHash 把 P 的 MusicFileHash 规范为去两端空白的非零串（长度 <= 64）；空串、全 0 或超长返回空串。
func normMusicHash(v kgdb.Value) string {
	if len(v.Bytes) > maxPathBytes {
		return ""
	}
	mh := strings.TrimSpace(v.String())
	if len(mh) > maxHashBytes || strings.Trim(mh, "0") == "" {
		return ""
	}
	return mh
}

// playingRow 扫各播放队列表定出 P：MusicFileHash 或可选多音质/MV 列与 h 相符、LastPlayTime 最大且大于 0 的行，
// 并列时取先扫到的。空串或全 0 的 h 直接返回零值，避免与音频行全 0 的 LastPlayedMVHash 误匹配。
// 没有 LastPlayTime 列的队列表各行按 0，不扫；非整数（含 NULL）按 0。表里没有 MusicFilePath 列时 P 的路径为空。
// LyricKey 超过 maxPathBytes 的行跳过（设计 §6.10）。只留 P 的一个键、一条路径与默认 Hash，工作集不随行数增长。
func playingRow(db tables, h string) (playing, error) {
	h = strings.TrimSpace(h)
	if strings.Trim(h, "0") == "" {
		return playing{}, nil
	}
	var p playing
	err := eachSongTable(db, true, func(t string, cols []string) (bool, error) {
		if !hasColumn(cols, "LastPlayTime") {
			return true, nil
		}
		withPath := hasColumn(cols, "MusicFilePath")
		scanCols := []string{"MusicFileHash", "LyricKey", "LastPlayTime"}
		if withPath {
			scanCols = append(scanCols, "MusicFilePath")
		}
		scanCols, extraStart := appendExtraHashCols(scanCols, cols)
		return true, db.Scan(t, scanCols, func(row []kgdb.Value) bool {
			if len(row[1].Bytes) > maxPathBytes || !rowMatchesHash(row, extraStart, h) {
				return true
			}
			if played := row[2].Int64(); played > p.played {
				p = playing{played: played, key: lyricKey(row[1]), musicHash: normMusicHash(row[0])}
				if withPath {
					p.path = filePath(row[3])
				}
			}
			return true
		})
	})
	return p, err
}

// associationKeys 按设计 §6.5 第 1 步返回 hash 的关联键集合（经 lyricKey 规范化、去空）。先由 playingRow
// 定出正在播放的队列行 P，再分三种情形：
//   - 有 P、且 P 带关联：只有 P 的键，不再扫其余各表。键在 LyricInfo 里没有行（关联被拒）、或其文件暂缺、
//     不可用时，也不取其他行的关联：酷狗显示的只是这一行的关联，「最近播放」等行只是开始播放时抄过去的副本。
//   - 有 P、P 不带关联：扫各歌曲条目表，只收与 P 不是同一文件的行的键（用户决定「1甲」）。同一文件指
//     MusicFilePath 去两端空白后不分大小写相同；任一方为空、或该表没有 MusicFilePath 列，按同一文件算、不收：
//     酷狗此时显示缓存，不用同一文件其余各行上的关联。
//   - 没有 P：收全部 MusicFileHash 或可选多音质/MV Hash 列与 h 相符的行的键。
//
// 另返回 P（没有 P 为零值）；出错时为零值。
//
// LyricKey 超过 maxPathBytes 的行跳过、不计数。超过 maxCandidates 个即停扫，打一条 Detail 并返回 nil 的集合。
// 找表与扫表出错的处理见 eachSongTable。
func associationKeys(db tables, h string) (keys map[string]bool, p playing, err error) {
	h = strings.TrimSpace(h)
	if strings.Trim(h, "0") == "" {
		return nil, playing{}, nil
	}
	p, err = playingRow(db, h)
	if err != nil {
		return nil, playing{}, err
	}
	if p.played > 0 && p.key == unassocMenuKey {
		return nil, p, nil
	}
	if p.played > 0 && p.key != "" {
		return map[string]bool{p.key: true}, p, nil
	}
	keys = map[string]bool{}
	if p.played > 0 && p.path == "" {
		return keys, p, nil // 各行都与 P 按同一文件算
	}
	err = eachSongTable(db, false, func(t string, cols []string) (bool, error) {
		scanCols := []string{"MusicFileHash", "LyricKey"}
		if p.played > 0 {
			if !hasColumn(cols, "MusicFilePath") {
				return true, nil // 整表按与 P 同一文件算
			}
			scanCols = append(scanCols, "MusicFilePath")
		}
		scanCols, extraStart := appendExtraHashCols(scanCols, cols)
		err := db.Scan(t, scanCols, func(row []kgdb.Value) bool {
			if len(row[1].Bytes) > maxPathBytes {
				return true
			}
			k := lyricKey(row[1])
			if k == "" || k == unassocMenuKey || k == unassocDialogKey || !rowMatchesHash(row, extraStart, h) {
				return true
			}
			if p.played > 0 {
				if f := filePath(row[2]); f == "" || strings.EqualFold(f, p.path) {
					return true
				}
			}
			keys[k] = true
			return len(keys) <= maxCandidates
		})
		return len(keys) <= maxCandidates, err
	})
	if len(keys) > maxCandidates {
		log.Detail("Hash %s has more than %d lyric associations in KuGou; using KuGou cache only", h, maxCandidates)
		return nil, p, nil
	}
	if err != nil {
		return nil, playing{}, err
	}
	return keys, p, nil
}

// eachSongTable 依次以 visit 扫各歌曲条目表（同时有 MusicFileHash 与 LyricKey 列的表；queueOnly 为真时只看
// 播放队列表），传入表名与其列；visit 返回 false 即不再看后面的表。
//
// LyricInfo 以外的表取不到列（如酷狗日后新加的表建表语句解析不了）即跳过并打一条 Detail，不让整次解析失败。
// LyricInfo 的种类不支持留给 collect 判为 errSchema，其余错误原样返回；visit 返回种类不支持即跳过该表，
// 其余错误原样返回。
func eachSongTable(db tables, queueOnly bool, visit func(t string, cols []string) (bool, error)) error {
	for _, t := range db.Tables() {
		if queueOnly && !isPlayQueue(t) {
			continue
		}
		cols, err := db.Columns(t)
		if err != nil && !strings.EqualFold(t, "LyricInfo") {
			log.Detail("Skipping table %s in KuGou database: %v", t, err)
			continue
		}
		if errors.Is(err, kgdb.ErrUnsupportedTable) {
			continue
		}
		if err != nil {
			return err
		}
		if !hasColumn(cols, "MusicFileHash") || !hasColumn(cols, "LyricKey") {
			continue
		}
		more, err := visit(t, cols)
		if !more {
			return nil
		}
		if errors.Is(err, kgdb.ErrUnsupportedTable) {
			continue
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// filePath 把 MusicFilePath 规范为去两端空白的原文；超过 maxPathBytes 的按空串，即与 P 按同一文件算，
// P 也就不必留存超长的路径。
func filePath(v kgdb.Value) string {
	if len(v.Bytes) > maxPathBytes {
		return ""
	}
	return strings.TrimSpace(v.String())
}

// keepLatest 把 c 收进 cache；已满 maxCandidates 条时，c 比其中 UpdateTime 最小的一条新才替换它。
// 设计 §6.10 写的是有界最小堆；至多 64 条，逐条找最小即可，内存同样有界。
func keepLatest(cache []candidate, c candidate) []candidate {
	if len(cache) < maxCandidates {
		return append(cache, c)
	}
	oldest := 0
	for i := range cache {
		if cache[i].UpdateTime < cache[oldest].UpdateTime {
			oldest = i
		}
	}
	if c.UpdateTime > cache[oldest].UpdateTime {
		cache[oldest] = c
	}
	return cache
}

func hasColumn(cols []string, name string) bool {
	return slices.ContainsFunc(cols, func(c string) bool { return strings.EqualFold(c, name) })
}

// sameHash 判断库里的 hash 与已去空白的 h 是否相同：去两端空白、不分大小写。
func sameHash(v kgdb.Value, h string) bool {
	return strings.EqualFold(strings.TrimSpace(v.String()), h)
}

// lyricKey 把 LyricKey 或 LyricInfo.Key 规范为去两端空白的小写；空串与全 0 为空串，即没有关联。
func lyricKey(v kgdb.Value) string {
	k := strings.ToLower(strings.TrimSpace(v.String()))
	if strings.Trim(k, "0") == "" {
		return ""
	}
	return k
}

// pick 按设计 §6.5 第 3、4 步依次试 col.cands，取第一份解析出行的；Hash 留空，由调用方填入。
//
// LyricPath 为绝对路径原样读；为相对路径则拼在 lyricDir 下，lyricDir 为空、或路径不在 lyricDir 之内
// （带根、带盘符、经 .. 逃出、为空或为设备名，见 filepath.IsLocal）即不可用、不读。read 返回
// klyric.ErrLyricTooLarge 或 errNotDiskFile 即不可用，其余错误即暂缺；读出的内容超限或解析出零行同为不可用。
// 结果之前有暂缺的候选时 Retry 为真。col.keyed 为真、而结果不是关联时 AssocSkipped 为真；PlayedAt、LocalFile 取
// col.playedAt、col.localFile，结果为 KindNone 时同样带上。
func pick(col collected, lyricDir string, read func(path string) ([]byte, error)) Result {
	if col.menuUnassoc {
		return Result{Kind: KindUnassociated, PlayedAt: col.playedAt, LocalFile: col.localFile}
	}
	r := Result{AssocSkipped: col.keyed, PlayedAt: col.playedAt, LocalFile: col.localFile}
	for _, c := range col.cands {
		path := c.Path
		if !filepath.IsAbs(path) {
			if lyricDir == "" || !filepath.IsLocal(path) {
				continue
			}
			path = filepath.Join(lyricDir, path)
		}
		b, err := read(path)
		if err != nil {
			r.Retry = r.Retry || !errors.Is(err, klyric.ErrLyricTooLarge) && !errors.Is(err, errNotDiskFile)
			continue
		}
		if lines, fileOff, hasOffset, err := klyric.ParseLocalWithOffset(b); err == nil && len(lines) > 0 {
			r.Kind, r.Lines, r.Source = KindFound, lines, c.Source
			if hasOffset {
				r.LyricOffsetMs = clampLyricOffsetMs(fileOff)
			} else {
				r.LyricOffsetMs = clampLyricOffsetMs(c.RealOffsetMs)
			}
			r.AssocSkipped = col.keyed && c.Source != SourceAssociation
			return r
		}
	}
	return r
}
