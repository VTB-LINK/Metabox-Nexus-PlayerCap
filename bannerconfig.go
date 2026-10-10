package main

import (
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"strings"

	"Metabox-Nexus-PlayerCap/config"
	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/server"
)

// 启动横幅的配置呈现。配置行逐键取自 buildConfigEcho 的三件套（与 /service-status 的 config /
// config_effective / config_overwritten 是同一份），横幅不另算任何值。每个键写成一项：
//
//	[*]名字=写下的值[ → 生效值]
//
// * 标出显式设置的键（config_overwritten）；写下的值与生效值不同（被钳制、被播放器层轮询下限抬高、
// 被 Windows 11 降级、取值非法而回退）时两者都列。
//
// 三件套之外横幅只多读一样：config.yml 里因类型不符而被拒的原值（Config.RejectedValues）。这些值不进
// /service-status（那里各字段的类型不能变），对三件套而言该键视同未设置；横幅在该键自己那一项上把原值
// 按字面写在「写下的值」的位置、不带单位（audio-frame-rate="30" → 25fps），见 bannerLiteral。
//
// 各行只做统一缩进、不做列对齐：「→」与中文标签的显示宽度随终端字体而变，列对齐只在一种界面语言下成立。

// bannerGlobalLines 是全局键在横幅上的分行：行格式（英文原文，经 i18n.T 取当前语言）与该行所列的键。
// buildConfigEcho 新增全局键时在此归入某一行；没有归入的键由 bannerConfigLines 各占一行列在其后，
// 不会从横幅上消失，但 bannerconfig_test.go 的 TestBannerPlacesEveryKey 会红。
var bannerGlobalLines = []struct {
	format string
	keys   []string
}{
	{"   Listening: %s\n", []string{"addr"}},
	{"   Global offset & poll: %s\n", []string{"offset", "poll"}},
	{"   Prior player: %s\n", []string{"prior-player", "prior-player-expire"}},
	{"   Per-player idle auto-hide: %s\n", []string{"per-player-idle-hide"}},
	{"   Audio normalization: %s\n", []string{"audio-normalize", "audio-normalize-target", "audio-normalize-max-gain"}},
	{"   Audio waveform frame rate: %s\n", []string{"audio-frame-rate"}},
}

// bannerUnits 是横幅上各项取值的单位，按横幅上的名字（每播放器键为去掉播放器前缀后的名字）查；
// 不在表中的不带单位。
var bannerUnits = map[string]string{
	"offset":                   "ms",
	"poll":                     "ms",
	"prior-player-expire":      "s",
	"per-player-idle-hide":     "s",
	"idle-hide":                "s",
	"audio-normalize-target":   "dBFS",
	"audio-normalize-max-gain": "dB",
	"audio-frame-rate":         "fps",
}

// bannerConfigLines 把 buildConfigEcho 的三件套排成启动横幅的配置行（每行以换行结尾）：全局键按
// bannerGlobalLines 分行，随后每个播放器一行，最后一行是图例。键名以「播放器名-」开头的键归该播放器
// 那一行、去掉前缀列出（cloudmusicv3-effect-strategy 因此列在 cloudmusicv3 行），各播放器都有的键在前。
// 播放器名互为前缀时这一归属会判错，现有六家没有这种情形。
//
// rejected 是因类型不符而被拒的原值（Config.RejectedValues），按完整键名查：只在写错的那个键自己那一项上
// 列出，继承全局键的每播放器项不带全局键的原值。
func bannerConfigLines(written, effective *server.OrderedMap, overwritten []string, rejected map[string]any, playerNames []string) []string {
	explicit := make(map[string]bool, len(overwritten))
	for _, k := range overwritten {
		explicit[k] = true
	}
	item := func(key, name string) string {
		w, _ := written.Get(key)
		e, _ := effective.Get(key)
		unit := bannerUnits[name]
		ws, es := fmt.Sprint(w)+unit, fmt.Sprint(e)+unit
		// 该键写下的值因类型不符没有被采纳：列它的原值，按字面写、不带单位。
		if raw, ok := rejected[key]; ok {
			ws = bannerLiteral(raw)
		}
		s := name + "=" + ws
		if ws != es {
			s += " → " + es
		}
		if explicit[key] {
			s = "*" + s
		}
		return s
	}

	keys := effective.Keys()
	pending := make(map[string]bool, len(keys)) // 尚未列出的键
	for _, k := range keys {
		pending[k] = true
	}
	take := func(k string) bool {
		ok := pending[k]
		delete(pending, k)
		return ok
	}

	var lines []string
	for _, l := range bannerGlobalLines {
		var items []string
		for _, k := range l.keys {
			if take(k) {
				items = append(items, item(k, k))
			}
		}
		if len(items) > 0 {
			lines = append(lines, fmt.Sprintf(i18n.T(l.format), strings.Join(items, " ")))
		}
	}

	shared := func(name string) bool {
		for _, pn := range playerNames {
			if _, ok := effective.Get(pn + "-" + name); !ok {
				return false
			}
		}
		return true
	}
	var playerLines []string
	for _, pn := range playerNames {
		var common, own []string
		for _, k := range keys {
			name, ok := strings.CutPrefix(k, pn+"-")
			if !ok || !take(k) {
				continue
			}
			if shared(name) {
				common = append(common, item(k, name))
			} else {
				own = append(own, item(k, name))
			}
		}
		if items := append(common, own...); len(items) > 0 {
			playerLines = append(playerLines, fmt.Sprintf(i18n.T("   Player: %s (%s)\n"), pn, strings.Join(items, " ")))
		}
	}

	for _, k := range keys {
		if pending[k] { // 没有归入 bannerGlobalLines 的全局键：各占一行，不丢
			lines = append(lines, "   "+item(k, k)+"\n")
		}
	}
	lines = append(lines, playerLines...)
	return append(lines, i18n.T("   * = set explicitly (config.yml or command line); a → b = written a, in effect b\n"))
}

// bannerLiteral 返回因类型不符而未被采纳的原值在横幅上的字面写法：按 JSON 写，字符串带引号，列表与
// 映射照 JSON，空值为 null。取整数值的小数（200.0）补上小数点，否则与被采纳的整数无从区分。不能按 JSON
// 写的（键不是字符串的映射、NaN、Inf）退回 Go 的默认写法。
func bannerLiteral(v any) string {
	if f, ok := v.(float64); ok && f == math.Trunc(f) {
		return strconv.FormatFloat(f, 'f', 1, 64)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

// bannerConfigFileNotes 是 config.yml 的加载结果在配置文件行上的附注（英文原文，经 i18n.T 取当前语言）；正常加载不加注。
// 三种失败只说 config.yml 未生效，不说本次运行用的是什么配置：Load 在这之后照常应用命令行参数。
var bannerConfigFileNotes = map[config.FileState]string{
	config.FileGenerated:      " (newly generated)",
	config.FileReadFailed:     " (read failed; config.yml not applied)",
	config.FileParseFailed:    " (parse failed; config.yml not applied)",
	config.FileGenerateFailed: " (not found and could not be created; config.yml not applied)",
}

// bannerNoteIndent 是配置文件行之下另起的附注行的缩进。
const bannerNoteIndent = "      "

// bannerConfigFileLine 返回启动横幅的配置文件行（一行或多行，每行以换行结尾）：config.Load 记下的 config.yml
// 绝对路径与加载结果；版本键与自动迁移的改写结果、按模板生成而目录里另有备份的提示各自另起一行、缩进
// 列出。后两类带着备份的文件名，接在路径后面会超出终端宽度而折行。只读 cfg，不另算：路径另算会与 Load
// 实际读的那一份分叉。
func bannerConfigFileLine(cfg *config.Config) string {
	s := fmt.Sprintf(i18n.T("   Config file: %s%s\n"), cfg.ConfigFile, i18n.T(bannerConfigFileNotes[cfg.ConfigFileState]))
	if note := bannerRewriteNote(cfg); note != "" {
		s += bannerNoteIndent + note + "\n"
	}
	// 按模板生成而目录里另有备份：上一次改写可能在原文件已改名、新文件还没放上时被打断。
	if cfg.ConfigFileState == config.FileGenerated && cfg.OrphanBackup != "" {
		s += bannerNoteIndent + fmt.Sprintf(i18n.T("the backup %s in this folder may hold your previous settings"), cfg.OrphanBackup) + "\n"
	}
	return s
}

// bannerRewriteNote 返回配置文件行之下版本键与自动迁移的附注（不带行首的缩进与末尾的换行）：文件版本
// （没有版本键的写作 v3）→ 盖章值，以及结果。不需要改写时为空串，不另起行。应改而未写成、又留有备份时
// 是两行，备份的文件名在第二行：两段接在一行里同样会折行。
func bannerRewriteNote(cfg *config.Config) string {
	from, to := cfg.FileVersionLabel, cfg.FileStamp
	kept := "" // 应改而未写成时可能留下的备份：改写中途放弃而没能放回的那一份
	if cfg.FileBackup != "" {
		kept = "\n" + bannerNoteIndent + fmt.Sprintf(i18n.T("the previous file is kept as %s"), filepath.Base(cfg.FileBackup))
	}
	switch cfg.FileRewrite {
	case config.RewriteStamped:
		return from + " → " + to
	case config.RewriteMigrated:
		return fmt.Sprintf(i18n.T("%s → %s, migrated; original kept as %s"), from, to, filepath.Base(cfg.FileBackup))
	case config.RewriteStampPending:
		return fmt.Sprintf(i18n.T("%s → %s could not be written"), from, to) + kept
	case config.RewriteMigratePending:
		return fmt.Sprintf(i18n.T("%s → %s: migration could not be written; converted in memory for this run"), from, to) + kept
	case config.RewriteNewerFile:
		return fmt.Sprintf(i18n.T("written by a newer version %s; read as is"), from)
	}
	return ""
}
