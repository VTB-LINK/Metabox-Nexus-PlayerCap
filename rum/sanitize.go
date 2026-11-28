package rum

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	maxPropStringBytes = 128
	maxSanitizeDepth   = 16   // 嵌套容器递归深度上限，防自引用/超深结构栈溢出
	maxSanitizeNodes   = 1024 // 单顶层属性最大遍历节点数，防多分支循环引用指数展开
	pathPlaceholder    = "<path>"
)

// blockedKeySubstrings 为属性键黑名单：键名规范化小写后等于或包含任一子串即直接剥离。
var blockedKeySubstrings = [...]string{
	"song_name",
	"track_name",
	"singer",
	"artist",
	"album",
	"lyric_text",
	"raw_lyric",
	"exe_path",
	"file_path",
	"dir_path",
	"music_path",
	"libcef_path",
	"cover_url",
	"raw_url",
	"referer",
	"username",
	"machine_guid",
	"unknown_prior_players",
}

// embeddedPathTail 保守吞并嵌入文本中的路径尾段（含目录与文件名中的空格），
// 仅在明确非路径终结边界（冒号+空白如 ": access denied"、引号、尖括号、竖线、换行或串尾）停止。
const embeddedPathTail = `(?:[^"'<>|\r\n:]|:(?:[^\s"'<>|\r\n]|$))*`

var (
	// rePurePath 识别整串为 file:// URI、UNC 路径或盘符路径（含路径内空格），整串替换为 <path>。
	rePurePath = regexp.MustCompile(`(?i)^(?:file://|\\\\[^\\/:*?"<>|\r\n]+\\|[a-z]:[\\/])[^\r\n]*$`)
	// reQuotedDoublePath / reQuotedSinglePath 优先脱敏被成对双/单引号包裹的路径段。
	reQuotedDoublePath = regexp.MustCompile(`(?i)"(?:file://|\\\\[^\\/:*?"<>|\r\n]+\\|[a-z]:[\\/])[^"\r\n]*"`)
	reQuotedSinglePath = regexp.MustCompile(`(?i)'(?:file://|\\\\[^\\/:*?"<>|\r\n]+\\|[a-z]:[\\/])[^'\r\n]*'`)
	// reFileURI 先于盘符正则匹配，避免 file:///C:/... 残留 file:/// 前缀。
	reFileURI   = regexp.MustCompile(`(?i)file://` + embeddedPathTail)
	reUNCPath   = regexp.MustCompile(`\\\\[^\\/:*?"<>|\r\n]+\\` + embeddedPathTail)
	reDrivePath = regexp.MustCompile(`(?i)\b[a-z]:[\\/]` + embeddedPathTail)
)

func isBlockedKey(key string) bool {
	lower := strings.ToLower(key)
	for _, sub := range blockedKeySubstrings {
		if strings.Contains(lower, sub) {
			return true
		}
	}
	return false
}

func mayContainPath(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == ':' || s[i] == '\\' {
			return true
		}
	}
	return false
}

// sanitizeString 对单个字符串执行路径脱敏与 UTF-8 安全字节截断（<= 128 字节）。
func sanitizeString(s string) string {
	if s == "" {
		return s
	}
	if mayContainPath(s) {
		trimmed := strings.TrimSpace(s)
		if rePurePath.MatchString(trimmed) {
			return pathPlaceholder
		}
		s = reQuotedDoublePath.ReplaceAllString(s, `"`+pathPlaceholder+`"`)
		s = reQuotedSinglePath.ReplaceAllString(s, `'`+pathPlaceholder+`'`)
		s = reFileURI.ReplaceAllString(s, pathPlaceholder)
		s = reUNCPath.ReplaceAllString(s, pathPlaceholder)
		s = reDrivePath.ReplaceAllString(s, pathPlaceholder)
	}
	return truncateUTF8(s, maxPropStringBytes)
}

// truncateUTF8 按完整 UTF-8 rune 边界将字符串截断至不超过 maxBytes 字节。
func truncateUTF8(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	n := 0
	for n < len(s) {
		_, size := utf8.DecodeRuneInString(s[n:])
		if n+size > maxBytes {
			break
		}
		n += size
	}
	return s[:n]
}

// isValidFiniteJSONNumber 校验 json.Number 是否为合法且有限的 RFC 8259 JSON 数字，
// 防止非法文本或 NaN/Inf/溢出数字破坏整批 JSON 序列化。
func isValidFiniteJSONNumber(n json.Number) bool {
	s := string(n)
	if s == "" || strings.TrimSpace(s) != s {
		return false
	}
	first := s[0]
	if first != '-' && (first < '0' || first > '9') {
		return false
	}
	if !json.Valid([]byte(s)) {
		return false
	}
	if _, err := strconv.ParseInt(s, 10, 64); err == nil {
		return true
	}
	if _, err := strconv.ParseUint(s, 10, 64); err == nil {
		return true
	}
	f, err := strconv.ParseFloat(s, 64)
	return err == nil && !math.IsNaN(f) && !math.IsInf(f, 0)
}

// sanitizeProps 深拷贝并递归清洗事件属性，绝不原地修改调用方传入的 map 或 slice。
func sanitizeProps(in Props) Props {
	if len(in) == 0 {
		return nil
	}
	out := make(Props, len(in))
	for k, v := range in {
		if isBlockedKey(k) {
			continue
		}
		budget := maxSanitizeNodes
		if sv, ok := sanitizeValue(v, 1, &budget); ok {
			out[k] = sv
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func sanitizeMapAny(in map[string]any, depth int, budget *int) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		if *budget <= 0 {
			break
		}
		if isBlockedKey(k) {
			continue
		}
		if sv, ok := sanitizeValue(v, depth+1, budget); ok {
			out[k] = sv
		}
	}
	return out
}

func sanitizeValue(v any, depth int, budget *int) (any, bool) {
	if depth > maxSanitizeDepth || *budget <= 0 {
		return nil, false
	}
	*budget--

	switch val := v.(type) {
	case nil:
		return nil, true
	case string:
		return sanitizeString(val), true
	case bool,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64:
		return val, true
	case float64:
		if math.IsNaN(val) || math.IsInf(val, 0) {
			return nil, false
		}
		return val, true
	case float32:
		f := float64(val)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, false
		}
		return val, true
	case json.Number:
		// 必须置于 fmt.Stringer 之前，避免合法大整数被转为字符串或精度丢失。
		if !isValidFiniteJSONNumber(val) {
			return nil, false
		}
		return val, true
	case Props:
		if val == nil {
			return nil, true
		}
		if depth >= maxSanitizeDepth {
			return nil, false
		}
		return sanitizeMapAny(map[string]any(val), depth, budget), true
	case map[string]any:
		if val == nil {
			return nil, true
		}
		if depth >= maxSanitizeDepth {
			return nil, false
		}
		return sanitizeMapAny(val, depth, budget), true
	case map[string]string:
		if val == nil {
			return nil, true
		}
		out := make(map[string]string, len(val))
		for k, s := range val {
			if isBlockedKey(k) {
				continue
			}
			out[k] = sanitizeString(s)
		}
		return out, true
	case map[string]uint64:
		if val == nil {
			return nil, true
		}
		out := make(map[string]uint64, len(val))
		for k, n := range val {
			if isBlockedKey(k) {
				continue
			}
			out[k] = n
		}
		return out, true
	case map[string]int:
		if val == nil {
			return nil, true
		}
		out := make(map[string]int, len(val))
		for k, n := range val {
			if isBlockedKey(k) {
				continue
			}
			out[k] = n
		}
		return out, true
	case map[string]int64:
		if val == nil {
			return nil, true
		}
		out := make(map[string]int64, len(val))
		for k, n := range val {
			if isBlockedKey(k) {
				continue
			}
			out[k] = n
		}
		return out, true
	case []string:
		if val == nil {
			return nil, true
		}
		out := make([]string, len(val))
		for i, s := range val {
			out[i] = sanitizeString(s)
		}
		return out, true
	case []int:
		if val == nil {
			return nil, true
		}
		return append([]int(nil), val...), true
	case []int64:
		if val == nil {
			return nil, true
		}
		return append([]int64(nil), val...), true
	case []uint64:
		if val == nil {
			return nil, true
		}
		return append([]uint64(nil), val...), true
	case []Props:
		if val == nil {
			return nil, true
		}
		if depth >= maxSanitizeDepth {
			return nil, false
		}
		out := make([]any, 0, len(val))
		for _, elem := range val {
			if *budget <= 0 {
				break
			}
			if elem == nil {
				out = append(out, nil)
				continue
			}
			*budget--
			out = append(out, sanitizeMapAny(map[string]any(elem), depth+1, budget))
		}
		return out, true
	case []map[string]any:
		if val == nil {
			return nil, true
		}
		if depth >= maxSanitizeDepth {
			return nil, false
		}
		out := make([]any, 0, len(val))
		for _, elem := range val {
			if *budget <= 0 {
				break
			}
			if elem == nil {
				out = append(out, nil)
				continue
			}
			*budget--
			out = append(out, sanitizeMapAny(elem, depth+1, budget))
		}
		return out, true
	case []any:
		if val == nil {
			return nil, true
		}
		if depth >= maxSanitizeDepth {
			return nil, false
		}
		out := make([]any, 0, len(val))
		for _, elem := range val {
			if *budget <= 0 {
				break
			}
			if se, ok := sanitizeValue(elem, depth+1, budget); ok {
				out = append(out, se)
			}
		}
		return out, true
	case error:
		return sanitizeString(val.Error()), true
	case fmt.Stringer:
		return sanitizeString(val.String()), true
	default:
		if depth >= maxSanitizeDepth {
			return nil, false
		}
		// 未知容器/结构体经 JSON 往返转为独立通用值树再递归清洗，彻底断开调用方底层共享并执行脱敏。
		// 启用 UseNumber 保留超过 2^53 的整数精度；Decode 仅产出 nil/bool/json.Number/string/[]any/map[string]any。
		b, err := json.Marshal(val)
		if err != nil {
			return nil, false
		}
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.UseNumber()
		var generic any
		if err := dec.Decode(&generic); err != nil {
			return nil, false
		}
		return sanitizeValue(generic, depth+1, budget)
	}
}

func copyPropsMap(in Props) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
