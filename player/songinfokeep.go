package player

import (
	"strings"
	"sync/atomic"
)

// songInfoCorrect 为真即全局开关 song-info-keep-first 关闭（#229）。取反存放：零值即默认的开启，
// 测试与未经 main 设置的进程都按开启。main 在启动播放器之前设一次，之后只读。
var songInfoCorrect atomic.Bool

// SetSongInfoKeepFirst 设置全局开关 song-info-keep-first。main 在启动播放器之前调用一次。
func SetSongInfoKeepFirst(on bool) { songInfoCorrect.Store(!on) }

// SupplementField 返回同一次宣告之内补发时 name 或 singer 的取值（#229）：announced 是本次宣告发出的
// （或之后已补上的）值，read 是之后读到的值。开关开启时，announced 非空（去掉首尾空白之后）即取 announced，
// 为空则 read 非空时取 read；开关关闭时取 read。title 不经此函数：同一次宣告内恒为宣告时的值。
func SupplementField(announced, read string) string {
	result, _ := SupplementDecision(announced, read)
	return result
}

// SupplementDecision 与 SupplementField 语义完全一致，同时返回脱敏决策枚举：
// "kept"（保持原宣告值）、"filled_empty"（补空）、"overwritten"（开关关闭时以新读值覆盖）、"unchanged"（无变化）。
func SupplementDecision(announced, read string) (result, decision string) {
	if songInfoCorrect.Load() {
		if read == announced {
			return read, "unchanged"
		}
		if strings.TrimSpace(announced) == "" && strings.TrimSpace(read) != "" {
			return read, "filled_empty"
		}
		return read, "overwritten"
	}
	if strings.TrimSpace(announced) != "" || strings.TrimSpace(read) == "" {
		if read == announced || strings.TrimSpace(read) == "" {
			return announced, "unchanged"
		}
		return announced, "kept"
	}
	return read, "filled_empty"
}
