package config

// 本文件只测自动迁移的文本层（rewriteText）：值层改过的键写回各自的行，被注释掉的键同样换算，旧注释里
// 说明方向的短语替换，连同版本键一次改完；认不了的写法报错、不产出文本。其余字节不变。
// 版本键行自身的改写见 versionkeytext_test.go。

import (
	"os"
	"strings"
	"testing"
	"unicode/utf16"
)

// 迁移时插入的版本键（文件里原先没有版本键）：两行注释、键行、一个空行。
const migratedHeader = "# 配置文件版本，由程序维护，请勿手动修改\n" +
	"# 自 4.0 起 offset 表示延迟（正值=延后，负值=提前）；本文件的 offset 已在迁移时取反\n" +
	"config-version: \"v4.0.0\"\n\n"

var textPlayers = []string{"kugou", "qqmusic", "cloudmusicv3"}

// migrateText 把 src 迁移到盖章值 stamp（没有版本键按 v3），返回改写后的全文。
func migrateText(t *testing.T, src, stamp string) (string, error) {
	t.Helper()
	m := parseYAML(t, src)
	plan := planUpgrade(m, stamp)
	var steps []migrationStep
	if plan.migrate {
		steps = stepsAfter(plan.fileVersion)
	}
	next, changed := migrateValues(m, steps, textPlayers)
	next[versionKey] = stamp
	_, hadKey := m[versionKey]
	out, err := rewriteText([]byte(src), hadKey, next, changed, steps, textPlayers, stamp)
	return string(out), err
}

// 设计 §4.2 的各种写法。每例的文件都没有版本键（v3），迁移到 v4.0.0；want 是版本键之后的部分。
//
// 变异自证（隔离的 worktree，实测）：
//   - 文本层自行按键名改行、不看值层改了什么（凡整数键行的键是 offset 键就取反）→「零」「负零」不红
//     （取反后仍是 0），「嵌套映射里只有它」红（改了不该改的行，校验不过而报错）。
//   - 被注释掉的键不换算 → 四例注释掉的键红；被注释掉的键不限于整行匹配（# 与键之间允许别的文字）→
//     「注释里提到键、行末是数字」「两个 # 的注释」红；未注册播放器的注释掉的键也换算 →「注释掉的
//     未注册播放器」红。
//   - 被注释掉的键不要求规范的十进制写法（去掉与 strconv.Itoa 结果的比对）→ 五例前导零红（01000 被
//     写成 -1000，-010 被写成 10）；比对时不去掉前导的 + →「注释掉的键、带缩进」「注释掉的键、带正号的
//     十进制」红。
//   - 不替换注释短语 → 两例短语红；替换顺序颠倒（短的在前）→「带负值说明的短语」红；行尾注释里的
//     短语也替换 →「行尾注释里的短语不动」红。
//   - 写回时不保留行尾注释、缩进、行尾 → 对应各例红；int 最小值按普通取反写回 →「最小值」红（值层
//     与文本层不一致，校验不过）。
func TestRewriteTextMigratesOffsetLines(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"正数", "offset: 200\n", "offset: -200\n"},
		{"带正号", "offset: +200\n", "offset: -200\n"},
		{"负数", "offset: -300\n", "offset: 300\n"},
		{"零", "offset: 0\n", "offset: 0\n"},
		{"负零", "offset: -0\n", "offset: -0\n"},
		{"行尾注释", "offset: 200 # 说明\n", "offset: -200 # 说明\n"},
		{"行尾注释里的短语不动", "offset: 200   # 正值=歌词提前\n", "offset: -200   # 正值=歌词提前\n"},
		{"冒号后多个空白", "offset:   200\n", "offset:   -200\n"},
		{"双引号字符串不动", "offset: \"200\"\n", "offset: \"200\"\n"},
		{"单引号字符串不动", "offset: '200'\n", "offset: '200'\n"},
		{"小数不动", "offset: 200.0\n", "offset: 200.0\n"},
		{"空值不动", "offset:\npoll: 30\n", "offset:\npoll: 30\n"},
		{"最小值", "offset: -9223372036854775808\n", "offset: 9223372036854775807\n"},
		{"超过 int64 上限的不动、不妨碍别的键", "offset: 9223372036854775808\nkugou-offset: 430\n", "offset: 9223372036854775808\nkugou-offset: -430\n"},
		{"播放器的键", "offset: 200\nkugou-offset: 430\nqqmusic-offset: -50\ncloudmusicv3-offset: 0\n", "offset: -200\nkugou-offset: -430\nqqmusic-offset: 50\ncloudmusicv3-offset: 0\n"},
		{"未注册播放器的键不动", "foo-offset: 200\nwesing-offset: 5\n", "foo-offset: 200\nwesing-offset: 5\n"},
		{"别的整数键不动", "poll: 30\naudio-normalize-target: -14\n", "poll: 30\naudio-normalize-target: -14\n"},
		{"嵌套映射里只有它", "extra:\n  offset: 200\n", "extra:\n  offset: 200\n"},
		{"注释掉的键", "poll: 30\n# qqmusic-offset: 350\n", "poll: 30\n# qqmusic-offset: -350\n"},
		{"注释掉的键、# 后无空白", "poll: 30\n#offset: 5\n", "poll: 30\n#offset: -5\n"},
		{"注释掉的键、带行尾注释", "poll: 30\n#  kugou-offset: -430 # x\n", "poll: 30\n#  kugou-offset: 430 # x\n"},
		{"注释掉的键、带缩进", "poll: 30\n  # offset: +7\n", "poll: 30\n  # offset: -7\n"},
		{"注释里提到键", "poll: 30\n# 例如 offset: 200 即可\n", "poll: 30\n# 例如 offset: 200 即可\n"},
		{"注释里提到键、行末是数字", "poll: 30\n# 默认 offset: 200\n", "poll: 30\n# 默认 offset: 200\n"},
		{"注释掉的未注册播放器", "poll: 30\n# foo-offset: 5\n# wesing-offset: 5\n", "poll: 30\n# foo-offset: 5\n# wesing-offset: 5\n"},
		{"注释掉的键、数值超出 int", "poll: 30\n# offset: 99999999999999999999\n", "poll: 30\n# offset: 99999999999999999999\n"},
		{"注释掉的键、十六进制", "poll: 30\n# offset: 0x10\n", "poll: 30\n# offset: 0x10\n"},
		{"注释掉的键、冒号后无空白", "poll: 30\n#offset:5\n", "poll: 30\n#offset:5\n"},
		{"注释掉的零", "poll: 30\n# kugou-offset: 0\n", "poll: 30\n# kugou-offset: 0\n"},
		{"两个 # 的注释", "poll: 30\n## offset: 5\n", "poll: 30\n## offset: 5\n"},
		{"注释掉的别的键", "poll: 30\n# kugou-poll: 30\n", "poll: 30\n# kugou-poll: 30\n"},
		// 被注释掉的键只认规范的十进制写法：带前导零的数字 yaml 按八进制解析（01000 即 512），照十进制
		// 取反会改掉它日后取消注释时的值，所以不动。
		{"注释掉的键、前导零", "poll: 30\n# kugou-offset: 01000\n", "poll: 30\n# kugou-offset: 01000\n"},
		{"注释掉的键、负号加前导零", "poll: 30\n# offset: -010\n", "poll: 30\n# offset: -010\n"},
		{"注释掉的键、正号加前导零", "poll: 30\n# offset: +010\n", "poll: 30\n# offset: +010\n"},
		{"注释掉的键、# 后无空白、前导零", "poll: 30\n#offset: 007\n", "poll: 30\n#offset: 007\n"},
		{"注释掉的键、前导零、带行尾注释与缩进", "poll: 30\n  #  kugou-offset: 0430 # x\n", "poll: 30\n  #  kugou-offset: 0430 # x\n"},
		{"注释掉的键、两个零", "poll: 30\n# offset: 00\n", "poll: 30\n# offset: 00\n"},
		{"注释掉的键、带正号的十进制", "poll: 30\n# offset: +200\n", "poll: 30\n# offset: -200\n"},
		{"注释掉的全局键为零", "poll: 30\n# offset: 0\n", "poll: 30\n# offset: 0\n"},
		{"注释掉的负零", "poll: 30\n# offset: -0\n", "poll: 30\n# offset: -0\n"},
		{"注释掉的正零", "poll: 30\n# offset: +0\n", "poll: 30\n# offset: +0\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := migrateText(t, c.src, "v4.0.0")
			if err != nil {
				t.Fatalf("迁移 %q 失败: %v", c.src, err)
			}
			if want := migratedHeader + c.want; got != want {
				t.Errorf("迁移结果\n得   %q\n期望 %q", got, want)
			}
		})
	}
}

// 版本键与注释的位置、旧注释短语、行尾与 BOM。want 是完整的迁移结果。
func TestRewriteTextMigratesWholeFile(t *testing.T) {
	crlf := strings.ReplaceAll(migratedHeader, "\n", "\r\n")
	cases := []struct{ name, src, want string }{
		{"带负值说明的短语", "# 头\n\n# 歌词时间偏移（毫秒），正值=歌词提前，负值=延后\noffset: 200\n",
			"# 头\n\n" + migratedHeader + "# 歌词时间偏移（毫秒），正值=延后，负值=提前\noffset: -200\n"},
		{"只有正值说明的短语", "poll: 30\n\n# 正值=歌词提前\noffset: 200\n",
			migratedHeader + "poll: 30\n\n# 正值=延后\noffset: -200\n"},
		{"别的注释不动", "# 歌词提前量，越大越早\noffset: 200\n",
			migratedHeader + "# 歌词提前量，越大越早\noffset: -200\n"},
		{"CRLF", "addr: x\r\noffset: 200\r\n", crlf + "addr: x\r\noffset: -200\r\n"},
		{"行尾混用", "addr: x\noffset: 200\r\npoll: 30\n", migratedHeader + "addr: x\noffset: -200\r\npoll: 30\n"},
		{"末行无换行", "addr: x\noffset: 200", migratedHeader + "addr: x\noffset: -200"},
		{"UTF-8 BOM", "\xEF\xBB\xBFoffset: 200\n", "\xEF\xBB\xBF" + migratedHeader + "offset: -200\n"},
		{"整体缩进", "  offset: 200 # 说明\n  poll: 30\n",
			"  # 配置文件版本，由程序维护，请勿手动修改\n  # 自 4.0 起 offset 表示延迟（正值=延后，负值=提前）；本文件的 offset 已在迁移时取反\n" +
				"  config-version: \"v4.0.0\"\n\n  offset: -200 # 说明\n  poll: 30\n"},
		{"空文件", "", strings.TrimSuffix(migratedHeader, "\n")},
		{"只有注释", "# 正值=歌词提前\n", "# 正值=延后\n" + strings.TrimSuffix(migratedHeader, "\n")},
		// 已有版本键：只换那一行的值，不再插入注释。
		{"版本键是 3.x 的版本号", "config-version: \"3.3.4\"\noffset: 200\n", "config-version: \"v4.0.0\"\noffset: -200\n"},
		{"版本键非法：不迁移，只改版本键", "config-version: 4.0\noffset: 200\n# kugou-offset: 5\n# 正值=歌词提前\n",
			"config-version: \"v4.0.0\"\noffset: 200\n# kugou-offset: 5\n# 正值=歌词提前\n"},
		{"已是 4.0 预发布：不迁移，只改版本键", "config-version: \"v4.0.0-beta.3\"\noffset: -200\n# kugou-offset: 5\n",
			"config-version: \"v4.0.0\"\noffset: -200\n# kugou-offset: 5\n"},
		// 没有迁移步骤时，整数键行的检查不适用：形状不合的 offset 不妨碍更新版本键。
		{"没有迁移步骤时形状不合的 offset 不妨碍", "config-version: \"v4.0.0-0\"\noffset: 0x10\nkugou-offset: 1_000\n",
			"config-version: \"v4.0.0\"\noffset: 0x10\nkugou-offset: 1_000\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := migrateText(t, c.src, "v4.0.0")
			if err != nil {
				t.Fatalf("迁移 %q 失败: %v", c.src, err)
			}
			if got != c.want {
				t.Errorf("迁移结果\n得   %q\n期望 %q", got, c.want)
			}
		})
	}
}

// 认不了的写法：报错，不产出文本（调用方不写文件，本次在内存里换算）。
//
// 变异自证（隔离的 worktree，实测）：
//   - 去掉改写校验（sameParse）→「块标量里形如注释掉的键的行」「首行是 ---」红。
//   - 整数键行的检查扩大到全部 offset 键、而不限于值层改过的（文件里有 offset 键却找不到它的整数键行
//     即报错）→ TestRewriteTextMigratesOffsetLines 的「双引号字符串不动」「单引号字符串不动」「小数不动」
//     「空值不动」等例、TestRewriteTextMigratesWholeFile 的「没有迁移步骤时形状不合的 offset 不妨碍」与
//     upgradefile_test.go 的 TestLoadFileStampsVersionKeyOnly 红。
//   - 找到多行时取第一行 →「嵌套映射里另有同名的行」「块标量里另有同名的行」红；不核对行里的数字是不是
//     原值 →「八进制」红；去掉文档数量的检查 →「多文档、各文档的键不同」「多文档、第二个文档里有注释掉
//     的键」红（第一例「多文档」不红：两个文档里各有一行 offset，另被「恰好对应一行」拦下）。
//   - 去掉 UTF-8 检查 → 不红：UTF-16 的文件里找不到认得的键行，插入版本键后又通不过校验，都另有一道拦着。
//     该检查只在 versionkeytext_test.go 的 TestCheckRewritable 里有用例变红。
func TestRewriteTextRejectsUnrecognizedForms(t *testing.T) {
	utf16le := func(s string, bom bool) string {
		runes := []rune(s)
		if bom {
			runes = append([]rune{0xFEFF}, runes...)
		}
		var b []byte
		for _, u := range utf16.Encode(runes) {
			b = append(b, byte(u), byte(u>>8))
		}
		return string(b)
	}
	for _, c := range []struct{ name, src string }{
		{"十六进制", "offset: 0x10\n"},
		{"下划线分隔", "offset: 1_000\n"},
		{"八进制", "offset: 010\n"},
		{"流式映射", "{offset: 200, poll: 30}\n"},
		{"键带引号", "\"offset\": 200\n"},
		{"键与值分行", "offset:\n  200\n"},
		{"锚点与别名", "offset: &o 200\nkugou-offset: *o\n"},
		{"别名指向的值", "base: &o 200\noffset: *o\n"},
		{"合并键", "defaults: &d\n  x: 1\n<<: *d\noffset: !!int 200\n"},
		{"显式类型标记", "offset: !!int 200\n"},
		{"嵌套映射里另有同名的行", "offset: 200\nextra:\n  offset: 5\n"},
		{"块标量里另有同名的行", "offset: 200\nnote: |\n  offset: 5\n"},
		{"块标量里形如注释掉的键的行", "offset: 200\nnote: |\n  # kugou-offset: 5\n"},
		{"首行是 ---", "---\noffset: 200\n"},
		{"多文档", "offset: 200\n---\nconfig-version: \"v4.0.0\"\noffset: -500\n"},
		{"多文档、各文档的键不同", "offset: 200\n---\npoll: 30\n"},
		{"多文档、第二个文档里有注释掉的键", "poll: 30\n---\n# offset: 5\n"},
		{"UTF-16LE 带 BOM", utf16le("offset: 200\r\n", true)},
		{"UTF-16LE 带 BOM、只有注释掉的键", utf16le("# offset: 200\r\npoll: 30\r\n", true)},
		{"一个键认得、另一个认不了", "offset: 200\nkugou-offset: 0x1ae\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := migrateText(t, c.src, "v4.0.0")
			if err == nil {
				t.Errorf("应报错，实际改写为 %q", got)
			}
			if got != "" {
				t.Errorf("报错时不应产出文本，实际 %q", got)
			}
		})
	}
}

// 3.3.4 随包的 config.yml（testdata/config-v3.3.4.yml，取自 tag v3.3.4 的 config.yml，逐字节相同）迁移到
// v4.0.0：五个 offset 取反，说明方向的那句注释改写，版本键连同两行注释插在 addr 的注释之前，其余逐字节不变。
// 迁移结果里各 offset 的取值与 4.0 模板相同；模板后来新增的键（audio-frame-rate、Apple Music 一段）不会
// 出现在迁移结果里，那部分由强制重建解决。
func TestRewriteTextMigratesShippedV3Config(t *testing.T) {
	raw, err := os.ReadFile("testdata/config-v3.3.4.yml")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	want := src
	for _, r := range [][2]string{
		{"# HTTP/WebSocket/SSE 监听地址\n", migratedHeader + "# HTTP/WebSocket/SSE 监听地址\n"},
		{"# 歌词时间偏移（毫秒），正值=歌词提前，负值=延后\noffset: 200\n", "# 歌词时间偏移（毫秒），正值=延后，负值=提前\noffset: -200\n"},
		{"\ncloudmusicv3-offset: 500\n", "\ncloudmusicv3-offset: -500\n"},
		{"\nqqmusic-offset: 400\n", "\nqqmusic-offset: -400\n"},
		{"\nkugou-offset: 430\n", "\nkugou-offset: -430\n"},
		{"\nsodamusic-offset: 340\n", "\nsodamusic-offset: -340\n"},
	} {
		if strings.Count(want, r[0]) != 1 {
			t.Fatalf("夹具里应恰有一处 %q", r[0])
		}
		want = strings.Replace(want, r[0], r[1], 1)
	}

	m := parseYAML(t, src)
	steps := stepsAfter(legacyFileVersion)
	next, changed := migrateValues(m, steps, realPlayers)
	next[versionKey] = "v4.0.0"
	got, err := rewriteText(raw, false, next, changed, steps, realPlayers, "v4.0.0")
	if err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	if string(got) != want {
		t.Errorf("迁移结果与预期不同\n得：\n%s\n期望：\n%s", got, want)
	}

	// 各 offset 与 4.0 模板取值相同（wesing 在两边都是注释掉的 0）。
	tmpl := parseYAML(t, templateContent("v4.0.0"))
	for _, key := range offsetKeys(realPlayers) {
		if _, inOld := m[key]; inOld && next[key] != tmpl[key] {
			t.Errorf("%s 迁移后 = %v，4.0 模板为 %v", key, next[key], tmpl[key])
		}
	}
}
