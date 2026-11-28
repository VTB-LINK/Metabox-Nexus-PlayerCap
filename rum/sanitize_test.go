package rum

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestSanitizeStripsBlockedKeys 校验所有违禁键（含大小写与包含子串形式，含嵌套 map/slice）均被剥离，
// 合法键原样保留。
//
// 变异自证：从 blockedKeySubstrings 删掉任一键或把嵌套 map 改成跳过检查，本测试变红。
func TestSanitizeStripsBlockedKeys(t *testing.T) {
	blocked := []string{
		"song_name", "track_name", "singer", "artist", "album",
		"lyric_text", "raw_lyric",
		"exe_path", "file_path", "dir_path", "music_path", "libcef_path",
		"cover_url", "raw_url", "referer",
		"username", "machine_guid", "unknown_prior_players",
	}

	in := Props{
		"player":                     "qqmusic",
		"unknown_prior_player_count": 2,
		"lyric_ms":                   42,
		"Song_Name":                  "should-strip-case",
		"prev_artist_id":             "should-strip-substr",
		"nested_map": map[string]any{
			"EXE_PATH": "C:\\app.exe",
			"ok_key":   "kept",
		},
		"nested_props": Props{
			"my_raw_lyric_buf": "secret",
			"safe":             true,
		},
		"nested_slice": []any{
			map[string]any{
				"machine_guid": "abc-123",
				"code":         200,
			},
		},
	}
	for _, k := range blocked {
		in[k] = "sensitive-" + k
		in["prefix_"+strings.ToUpper(k)+"_suffix"] = "sensitive-sub-" + k
	}

	got := sanitizeProps(in)

	for _, k := range blocked {
		if _, ok := got[k]; ok {
			t.Errorf("违禁键 %q 未被剥离", k)
		}
		subKey := "prefix_" + strings.ToUpper(k) + "_suffix"
		if _, ok := got[subKey]; ok {
			t.Errorf("含违禁子串的键 %q 未被剥离", subKey)
		}
	}
	if _, ok := got["Song_Name"]; ok {
		t.Errorf("大小写变体 Song_Name 未被剥离")
	}
	if _, ok := got["prev_artist_id"]; ok {
		t.Errorf("子串变体 prev_artist_id 未被剥离")
	}

	if got["player"] != "qqmusic" || got["unknown_prior_player_count"] != 2 || got["lyric_ms"] != 42 {
		t.Errorf("合法顶层键未正确保留：%v", got)
	}

	nm, ok := got["nested_map"].(map[string]any)
	if !ok {
		t.Fatalf("nested_map 类型异常：%T", got["nested_map"])
	}
	if _, exists := nm["EXE_PATH"]; exists {
		t.Errorf("nested_map 中 EXE_PATH 未被剥离")
	}
	if nm["ok_key"] != "kept" {
		t.Errorf("nested_map 中合法键丢失：%v", nm)
	}

	np, ok := got["nested_props"].(map[string]any)
	if !ok {
		t.Fatalf("nested_props 类型异常：%T", got["nested_props"])
	}
	if _, exists := np["my_raw_lyric_buf"]; exists {
		t.Errorf("nested_props 中 my_raw_lyric_buf 未被剥离")
	}
	if np["safe"] != true {
		t.Errorf("nested_props 中合法键丢失：%v", np)
	}

	ns, ok := got["nested_slice"].([]any)
	if !ok || len(ns) != 1 {
		t.Fatalf("nested_slice 异常：%v", got["nested_slice"])
	}
	elem, ok := ns[0].(map[string]any)
	if !ok {
		t.Fatalf("nested_slice[0] 类型异常：%T", ns[0])
	}
	if _, exists := elem["machine_guid"]; exists {
		t.Errorf("nested_slice[0] 中 machine_guid 未被剥离")
	}
	if elem["code"] != 200 {
		t.Errorf("nested_slice[0] 合法键丢失：%v", elem)
	}
}

// TestSanitizeRedactsPathsRecursively 校验盘符路径、UNC 路径与 file:// URI 在顶层及各类嵌套结构中
// 均被 100% 替换为 <path>，且原始路径信息零泄露。
//
// 变异自证：关闭任一路径正则或不对 []string / []any / map[string]any 递归脱敏，本测试变红。
func TestSanitizeRedactsPathsRecursively(t *testing.T) {
	samples := []string{
		`C:\Users\Alice\Music\a.mp3`,
		`D:\Music\private.mp3`,
		`\\nas\share\a.flac`,
		`file:///C:/Users/Alice/lyric_page.html`,
		`c:/foo/bar/baz.lrc`,
		`C:\Users\Alice Smith\My Music\secret track.mp3`,
	}

	in := Props{
		"p0":        samples[0],
		"p1":        samples[1],
		"p2":        samples[2],
		"p3":        samples[3],
		"p4":        samples[4],
		"p5":        samples[5],
		"embedded":  `open D:\Music\private.mp3: access denied`,
		"emb_unc":   `read \\nas\share\a.flac: io error`,
		"emb_uri":   `src=file:///C:/Users/Alice/lyric_page.html`,
		"str_slice": append([]string(nil), samples...),
		"any_slice": []any{
			samples[0],
			samples[2],
			[]string{samples[1], samples[3]},
			map[string]any{"inner": samples[3], "msg": `at C:\Users\Alice\Music\a.mp3`},
		},
		"nested": map[string]any{
			"a": samples[0],
			"b": samples[1],
			"c": samples[2],
			"d": samples[3],
		},
	}

	got := sanitizeProps(in)

	for _, key := range []string{"p0", "p1", "p2", "p3", "p4", "p5"} {
		if got[key] != "<path>" {
			t.Errorf("%s 期望 <path>，实际 %q", key, got[key])
		}
	}
	if got["embedded"] != "open <path>: access denied" {
		t.Errorf("embedded 脱敏不符：got=%q", got["embedded"])
	}
	if got["emb_unc"] != "read <path>: io error" {
		t.Errorf("emb_unc 脱敏不符：got=%q", got["emb_unc"])
	}
	if got["emb_uri"] != "src=<path>" {
		t.Errorf("emb_uri 脱敏不符：got=%q", got["emb_uri"])
	}

	ss, ok := got["str_slice"].([]string)
	if !ok || len(ss) != len(samples) {
		t.Fatalf("str_slice 异常：%#v", got["str_slice"])
	}
	for i, v := range ss {
		if v != "<path>" {
			t.Errorf("str_slice[%d] 期望 <path>，实际 %q", i, v)
		}
	}

	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("json.Marshal 失败：%v", err)
	}
	serialized := string(raw)
	for _, leak := range []string{"Alice", "private.mp3", "a.flac", "lyric_page.html", "foo/bar", "secret track", `\\nas`} {
		if strings.Contains(serialized, leak) {
			t.Errorf("序列化结果仍泄露敏感片段 %q：%s", leak, serialized)
		}
	}
}

// TestSanitizeTruncatesLongUTF8Strings 校验超 128 字节的 ASCII 与多字节 UTF-8 字符串截断长度与合法性。
//
// 变异自证：按 byte 硬切片 s[:128]（破坏中文 UTF-8）或不做截断，本测试变红。
func TestSanitizeTruncatesLongUTF8Strings(t *testing.T) {
	ascii := strings.Repeat("a", 200)
	gotASCII := sanitizeString(ascii)
	if len(gotASCII) != 128 {
		t.Errorf("ASCII 截断长度期望 128，实际 %d", len(gotASCII))
	}

	// 每个汉字 3 字节，50 个汉字 = 150 字节，截断后应为 42 个汉字 = 126 字节。
	cn := strings.Repeat("歌", 50)
	gotCN := sanitizeString(cn)
	if len(gotCN) > 128 || len(gotCN) != 126 {
		t.Errorf("中文截断长度期望 126，实际 %d", len(gotCN))
	}
	if !utf8.ValidString(gotCN) {
		t.Errorf("中文截断后出现非法 UTF-8：%q", gotCN)
	}

	// 边界：127 字节 ASCII + 1 个 3 字节汉字 = 130 字节，应截断为 127 字节，不破坏汉字。
	mixed := strings.Repeat("x", 127) + "中"
	gotMixed := sanitizeString(mixed)
	if len(gotMixed) != 127 || !utf8.ValidString(gotMixed) {
		t.Errorf("混合边界截断异常：len=%d valid=%v", len(gotMixed), utf8.ValidString(gotMixed))
	}

	// 超长路径应先脱敏再截断，绝不把截断后的残余路径片段放行。
	longPath := `D:\Users\Alice\` + strings.Repeat("dir_", 40) + `secret.mp3`
	if got := sanitizeString(longPath); got != "<path>" {
		t.Errorf("超长路径期望 <path>，实际 %q", got)
	}
}

// TestSanitizeHandlesNaNInfAndDoesNotMutateInput 校验 NaN/Inf 被过滤防止整批 JSON 序列化失败，
// 且 sanitizeProps 深拷贝不原地修改调用方传入的 map 与 slice。
//
// 变异自证：不过滤 NaN/Inf（json.Marshal 报错）或原地覆写输入 map/slice，本测试变红。
func TestSanitizeHandlesNaNInfAndDoesNotMutateInput(t *testing.T) {
	origSlice := []string{`D:\Music\private.mp3`, "ok"}
	origAny := []any{math.NaN(), `C:\Users\Alice\a.mp3`, float32(math.Inf(1)), "keep"}
	origNested := map[string]any{
		"artist": "secret-singer",
		"nan":    math.NaN(),
		"inf":    math.Inf(-1),
		"f32nan": float32(math.NaN()),
		"good":   1.5,
	}
	in := Props{
		"nan":    math.NaN(),
		"pinf":   math.Inf(1),
		"ninf":   math.Inf(-1),
		"f32inf": float32(math.Inf(-1)),
		"ok_num": 3.14,
		"slice":  origSlice,
		"any":    origAny,
		"nested": origNested,
		"singer": "orig-singer",
	}

	got := sanitizeProps(in)

	if _, err := json.Marshal(got); err != nil {
		t.Fatalf("含 NaN/Inf 的 Props 清洗后仍无法 JSON 序列化：%v", err)
	}
	for _, k := range []string{"nan", "pinf", "ninf", "f32inf"} {
		if _, exists := got[k]; exists {
			t.Errorf("异常浮点键 %q 未被过滤", k)
		}
	}
	if got["ok_num"] != 3.14 {
		t.Errorf("正常浮点被误删：%v", got["ok_num"])
	}

	gotAny, ok := got["any"].([]any)
	if !ok {
		t.Fatalf("any 类型异常：%T", got["any"])
	}
	wantAny := []any{"<path>", "keep"}
	if !reflect.DeepEqual(gotAny, wantAny) {
		t.Errorf("any 清洗不符：got=%#v want=%#v", gotAny, wantAny)
	}

	// 修改清洗结果，确认原始输入完好无损。
	got["ok_num"] = 999.0
	got["slice"].([]string)[0] = "mutated"
	got["nested"].(map[string]any)["good"] = -1.0

	if in["singer"] != "orig-singer" || in["ok_num"] != 3.14 {
		t.Errorf("输入顶层 Props 被原地修改：%v", in)
	}
	if origSlice[0] != `D:\Music\private.mp3` {
		t.Errorf("输入 []string 被原地修改：%v", origSlice)
	}
	if origAny[1] != `C:\Users\Alice\a.mp3` {
		t.Errorf("输入 []any 被原地修改：%v", origAny)
	}
	if origNested["artist"] != "secret-singer" || origNested["good"] != 1.5 {
		t.Errorf("输入嵌套 map 被原地修改：%v", origNested)
	}
}

// TestSanitizeRedactsEmbeddedSpacedPaths 校验带英文/中文前缀、成对引号与含空格的盘符/UNC/file:// 路径
// 完整脱敏，绝不残留用户名、目录名或歌名尾段。
//
// 变异自证：路径正则遇空白即停，本测试对 Alice/Smith/secret 等敏感尾段残留立即变红。
func TestSanitizeRedactsEmbeddedSpacedPaths(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		noLeaks []string
	}{
		{
			name:    "英文前缀加含空格盘符路径与冒号错误后缀",
			in:      `open C:\Users\Alice Smith\Music\secret song.mp3: access denied`,
			want:    `open <path>: access denied`,
			noLeaks: []string{"Alice", "Smith", "secret", "song.mp3"},
		},
		{
			name:    "双引号包裹含空格盘符路径",
			in:      `open "C:\Users\Alice Smith\Music\secret song.mp3" failed`,
			want:    `open "<path>" failed`,
			noLeaks: []string{"Alice", "Smith", "secret", "song.mp3"},
		},
		{
			name:    "单引号包裹含空格盘符路径",
			in:      `open 'D:\My Music\Alice Smith\secret track.flac' failed`,
			want:    `open '<path>' failed`,
			noLeaks: []string{"Alice", "Smith", "secret", "track.flac"},
		},
		{
			name:    "中文前缀加含空格盘符路径",
			in:      `打开文件失败: C:\Users\Alice Smith\Music\私密 歌曲.mp3`,
			want:    `打开文件失败: <path>`,
			noLeaks: []string{"Alice", "Smith", "私密", "歌曲.mp3"},
		},
		{
			name:    "英文前缀加含空格UNC路径",
			in:      `read \\my nas\Alice Share\secret file.flac failed`,
			want:    `read <path>`,
			noLeaks: []string{"my nas", "Alice", "Share", "secret", "file.flac"},
		},
		{
			name:    "含空格file_URI与冒号后缀",
			in:      `src=file:///C:/Users/Alice Smith/My Lyrics/lyric page.html: not found`,
			want:    `src=<path>: not found`,
			noLeaks: []string{"Alice", "Smith", "My Lyrics", "lyric page"},
		},
		{
			name:    "正常HTTPS地址不被误杀",
			in:      `post https://eu.i.posthog.com/batch/ ok`,
			want:    `post https://eu.i.posthog.com/batch/ ok`,
			noLeaks: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeString(tc.in)
			if got != tc.want {
				t.Errorf("sanitizeString(%q)\n  got = %q\n  want= %q", tc.in, got, tc.want)
			}
			for _, leak := range tc.noLeaks {
				if strings.Contains(got, leak) {
					t.Errorf("脱敏后仍残留敏感片段 %q：%q", leak, got)
				}
			}
		})
	}
}

// TestSanitizeUnknownContainersDeepCopyAndRedact 校验 []map[string]any、[]Props、自定义结构体与指针等
// 未知容器同样递归剥离违禁键、脱敏路径且彻底深拷贝隔离调用方底层容器。
//
// 变异自证：default 分支原样返回未知容器，本测试立即检出敏感键/路径残留及共享底层 map 变异。
func TestSanitizeUnknownContainersDeepCopyAndRedact(t *testing.T) {
	type nestedStruct struct {
		Username string `json:"username"`
		Detail   string `json:"detail"`
		Count    int    `json:"count"`
	}

	origItem := map[string]any{
		"username": "Alice",
		"detail":   `C:\Users\Alice\private.txt`,
		"ok":       "kept",
	}
	origPropsItem := Props{
		"song_name": "secret-song",
		"msg":       `open D:\Music\Alice Smith\a.mp3: denied`,
	}
	origCounts := map[string]uint64{
		"ws_sub":   3,
		"username": 99,
	}
	st := &nestedStruct{
		Username: "Alice",
		Detail:   `\\nas\Alice\secret.mp3`,
		Count:    7,
	}

	in := Props{
		"items":      []map[string]any{origItem},
		"prop_items": []Props{origPropsItem},
		"struct_ptr": st,
		"counts":     origCounts,
		"bad_chan":   make(chan int),
	}

	got := sanitizeProps(in)

	if _, exists := got["bad_chan"]; exists {
		t.Errorf("不可序列化的 chan 应被丢弃")
	}

	// 修改调用方原始容器，确保与清洗结果完全隔离。
	origItem["ok"] = "mutated-by-caller"
	origItem["injected"] = "late-write"
	origCounts["ws_sub"] = 9999
	st.Count = 1234

	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("json.Marshal 失败：%v", err)
	}
	serialized := string(raw)
	for _, leak := range []string{"Alice", "private.txt", "secret-song", "secret.mp3", "username", "song_name", "mutated-by-caller", "late-write", "9999", "1234"} {
		if strings.Contains(serialized, leak) {
			t.Errorf("未知容器清洗或隔离失效，残留 %q：%s", leak, serialized)
		}
	}
	if !strings.Contains(serialized, `"ok":"kept"`) || !strings.Contains(serialized, `"count":7`) || !strings.Contains(serialized, `"ws_sub":3`) {
		t.Errorf("合法字段未正确保留：%s", serialized)
	}
}

// TestSanitizePreservesLargeIntegersAndDropsInvalidJSONNumber 校验：
// 1. default 分支经 JSON 往返清洗结构体/未知容器时，超 2^53 的大整数（如 9007199254740993）保持精确不舍入；
// 2. 显式传入的非法 json.Number（如 "not-a-num"、"NaN"、"true"、"1e9999"）被安全丢弃不破坏整批 JSON 序列化。
//
// 变异自证：default 分支不启用 UseNumber() 或缺少 json.Number 校验分支，本测试变红。
func TestSanitizePreservesLargeIntegersAndDropsInvalidJSONNumber(t *testing.T) {
	type bigStats struct {
		Count    uint64 `json:"count"`
		Signed   int64  `json:"signed"`
		Username string `json:"username"`
		Path     string `json:"detail"`
	}

	in := Props{
		"stats": bigStats{
			Count:    9007199254740993,
			Signed:   -9007199254740993,
			Username: "Alice",
			Path:     `C:\Users\Alice\a.mp3`,
		},
		"valid_num":   json.Number("9007199254740993"),
		"valid_float": json.Number("12.34"),
		"bad_text":    json.Number("not-a-num"),
		"bad_nan":     json.Number("NaN"),
		"bad_inf":     json.Number("Infinity"),
		"bad_bool":    json.Number("true"),
		"bad_huge":    json.Number("1e9999"),
		"bad_lead0":   json.Number("01"),
	}

	got := sanitizeProps(in)

	for _, k := range []string{"bad_text", "bad_nan", "bad_inf", "bad_bool", "bad_huge", "bad_lead0"} {
		if _, exists := got[k]; exists {
			t.Errorf("非法 json.Number 键 %q 未被丢弃：%v", k, got[k])
		}
	}

	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("清洗后 json.Marshal 失败：%v", err)
	}
	serialized := string(raw)

	if !strings.Contains(serialized, `"count":9007199254740993`) {
		t.Errorf("大无符号整数精度丢失：%s", serialized)
	}
	if !strings.Contains(serialized, `"signed":-9007199254740993`) {
		t.Errorf("大有符号整数精度丢失：%s", serialized)
	}
	if !strings.Contains(serialized, `"valid_num":9007199254740993`) {
		t.Errorf("合法 json.Number 应保留为 JSON 数字而非字符串或被舍入：%s", serialized)
	}
	if strings.Contains(serialized, "Alice") || strings.Contains(serialized, "username") {
		t.Errorf("结构体敏感字段未脱敏：%s", serialized)
	}
}

// TestSanitizeGuardsCyclicAndOverDeepContainers 校验自引用与相互循环的嵌套容器不会导致栈溢出崩溃，
// 同时正常多级嵌套属性仍完整保留并正确脱敏。
//
// 变异自证：去掉深度上限或节点预算，自引用分支立即触发不可恢复的栈溢出。
func TestSanitizeGuardsCyclicAndOverDeepContainers(t *testing.T) {
	// 1. 自引用 []Props
	cyclicPropsSlice := make([]Props, 1)
	cyclicPropsSlice[0] = Props{
		"ok":    "kept-props",
		"again": cyclicPropsSlice,
	}

	// 2. 自引用 []map[string]any
	cyclicMapSlice := make([]map[string]any, 1)
	cyclicMapSlice[0] = map[string]any{
		"ok":    "kept-map-slice",
		"again": cyclicMapSlice,
	}

	// 3. 自引用 Props
	cyclicProps := Props{"ok": "kept-self"}
	cyclicProps["self"] = cyclicProps

	// 4. 相互嵌套循环（map <-> []any，含多分支展开）
	nodeA := map[string]any{"name": "A"}
	nodeB := []any{nodeA, nodeA}
	nodeA["next"] = nodeB

	// 5. 正常 4 级嵌套应完整保留并脱敏
	normalNested := Props{
		"lvl1": map[string]any{
			"lvl2": []any{
				Props{
					"lvl3": map[string]any{
						"ok":       "deep-ok",
						"username": "Alice",
						"path":     `D:\Music\Alice\a.mp3`,
					},
				},
			},
		},
	}

	got := sanitizeProps(Props{
		"c_props_slice": cyclicPropsSlice,
		"c_map_slice":   cyclicMapSlice,
		"c_self":        cyclicProps,
		"c_mutual":      nodeA,
		"normal":        normalNested,
	})

	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("含循环容器清洗后 json.Marshal 失败：%v", err)
	}
	serialized := string(raw)
	if !strings.Contains(serialized, `"ok":"deep-ok"`) {
		t.Errorf("正常多级嵌套值丢失：%s", serialized)
	}
	if strings.Contains(serialized, "Alice") || strings.Contains(serialized, "username") {
		t.Errorf("正常多级嵌套内敏感信息未脱敏：%s", serialized)
	}

	// 验证经由 Track 入口传入自引用容器也立即安全返回，不崩溃。
	withInjectedCreds(t, "phc_key", "https://rum.example.invalid")
	Init(Options{
		IsRelease:  true,
		AppVersion: "4.0.0",
		DistinctID: "abcdef0123456789abcdef0123456789",
	})
	Track(CatPlayer, "player_compat_signal", Props{"items": cyclicPropsSlice})
}
