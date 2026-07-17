package main

import "testing"

// pool 模拟录音池：只有这些值「真实出现过」。
const pool = `
{"type":"lyric_update","player":"wesing","data":{"index":1,"text":"往事就像流星刹那划过心房","sub_text":"","timestamp":31.483,"play_time":31.282999,"progress":0.105743244}}
{"type":"all_lyrics","player":"qqmusic","data":{"title":"Sunshine - Ryan Farish","duration":225,"play_time":28.151,"progress":0.12511556,"count":0,"lyrics":[]}}
`

// TestCatchesFabricated 钉死本工具存在的理由：编造的示例值必须被抓住。
//
// 这不是假设的失效模式。checkYAML 之前不存在时，docsample 对 doc/openapi.yaml
// 报「✓ 所有示例的数值都能在录音里找到」，而那份文件里躺着 38 个编造值——
// 包括本工具文件头拿来当反面教材的 `play_time: 9.15` 本身。
func TestCatchesFabricated(t *testing.T) {
	cases := []struct {
		name string
		yaml string
	}{
		{"载荷字段直接赋值", "        play_time: 9.15\n"},
		{"progress 编造值", "        progress: 0.4167\n"},
		{"歌词文本编造", `        text: "君に嘘をついていた"` + "\n"},
		{"副歌词编造", `        sub_text: "我已无法认识自己"` + "\n"},
		{"schema 的 example", "    play_time:\n      type: number\n      example: 9.15\n"},
		{"逐字时间戳编造", "    duration:\n      type: number\n      example: 3.46\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if bad := checkYAML("t.yaml", c.yaml, pool); bad == 0 {
				t.Errorf("编造值未被抓住，门禁形同虚设:\n%s", c.yaml)
			}
		})
	}
}

// TestAcceptsReal 真实录音里的值不能误报——误报会逼人往 trivial 表里塞豁免，
// 那正是本工具注释里警告的后门。
func TestAcceptsReal(t *testing.T) {
	cases := []struct {
		name string
		yaml string
	}{
		{"真实 play_time", "        play_time: 31.282999\n"},
		{"真实 progress", "        progress: 0.105743244\n"},
		{"真实歌词", `        text: "往事就像流星刹那划过心房"` + "\n"},
		{"真实 duration", "        duration: 225\n"},
		{"退化值 0", "        progress: 0\n"},
		{"退化值 -1", "        index: -1\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if bad := checkYAML("t.yaml", c.yaml, pool); bad != 0 {
				t.Errorf("真实值被误报 %d 次:\n%s", bad, c.yaml)
			}
		})
	}
}

// TestConfigExemptByOwner 是 owner 回溯的存在理由。
//
// `example: 200` 既可能是 play_time 的（该查），也可能是 wesing-offset 的
// （config.yml 的量，播放器录音里永远不会有）。只看 key 名一律放行或一律检查
// 都是错的——前者漏掉真示例，后者把整个 config schema 报成红。
func TestConfigExemptByOwner(t *testing.T) {
	const cfg = `
    config:
      properties:
        wesing-offset:
          type: integer
          example: 200
        qqmusic-poll:
          type: integer
          example: 50
`
	if bad := checkYAML("t.yaml", cfg, pool); bad != 0 {
		t.Errorf("config 的量被误报 %d 次——它们来自 config.yml，不可能在播放器录音里", bad)
	}

	// 同样是 example: 200，挂在 play_time 下就必须查。
	const payload = `
    play_time:
      type: number
      example: 200
`
	if bad := checkYAML("t.yaml", payload, pool); bad == 0 {
		t.Error("play_time 的 example 未被检查——owner 回溯失效了")
	}
}

// TestInlineJSON openapi 的 description 里有大量内联 JSON，它们不在 ```json 块中，
// checkDoc 扫不到。歌词文本主要就藏在那里。
func TestInlineJSON(t *testing.T) {
	const src = `
  description: |
    收到消息：
    {"type":"lyric_update","player":"wesing","data":{"index":5,"text":"新歌第一行歌词","timestamp":15.0}}
`
	if bad := checkYAML("t.yaml", src, pool); bad == 0 {
		t.Error("内联 JSON 里的编造歌词未被抓住")
	}
}

// TestInlineSongMeta 钉的是 strLit 覆盖 name/singer/title/detail，不止 text。
//
// 用例真实发生过：内联 song_info 的 title 改成了真值，name/singer 忘了跟着改，
// 于是 `"name":"告白","singer":"花澤香菜"` 与 title 打架却逃检——strLit 当时只认
// "text"，字段名不对就看不见。如果有人把它缩回只认 text，这条会红。
func TestInlineSongMeta(t *testing.T) {
	fields := []string{"name", "singer", "detail", "title"}
	for _, f := range fields {
		src := `  description: |` + "\n    " +
			`{"type":"song_info_update","player":"wesing","data":{"` + f + `":"不存在的歌手XYZ"}}` + "\n"
		if bad := checkYAML("t.yaml", src, pool); bad == 0 {
			t.Errorf("内联 JSON 的 %q 字段编造值未被抓住", f)
		}
	}
}

// TestCatchesBrokenYAML 钉的是「契约文件能不能被解析」——这条比示例值更基础。
//
// 用例不是假想的：重写 openapi 示例时，往 summary 的裸标量里写了 `index: -1`，
// YAML 把冒号加空格当 mapping，整个文件解析失败。当时 docsample 报「✓ 全绿」——
// 它的检查全是正则，语法错误对它不存在。
func TestCatchesBrokenYAML(t *testing.T) {
	const broken = `
examples:
  x:
    summary: 所以 index: -1 不蕴含 text 为空
`
	if bad := checkFile("doc/openapi.yaml", broken, pool); bad == 0 {
		t.Error("YAML 语法错误未被抓住 —— 契约挂了，Apifox/Swagger 打不开")
	}
	// 合法 YAML 不能因语法被误报。
	const ok = `
examples:
  x:
    summary: "index 为 -1 时 text 不保证为空"
`
	if bad := checkFile("doc/openapi.yaml", ok, pool); bad != 0 {
		t.Errorf("合法 YAML 被误报 %d 次", bad)
	}
}

// TestDispatchWiring 钉的是**接线**，不是 checkYAML 本身。
//
// 上面所有测试都直接调 checkYAML，所以「main 忘了对 .yaml 调 checkYAML」这种断裂
// 它们一个都发现不了——而那恰恰是本次要修的原始缺陷：checkYAML 不存在时，docsample
// 对 openapi.yaml 报绿。只测纯函数会让同一个洞原样重开。
func TestDispatchWiring(t *testing.T) {
	const fabricated = "        play_time: 9.15\n"

	if bad := checkFile("doc/openapi.yaml", fabricated, pool); bad == 0 {
		t.Error(".yaml 未走 checkYAML —— 分派断了，YAML 示例整批逃检")
	}
	// .md 不该走 YAML 扫描：markdown 正文里的 `key: 数字` 不是示例。
	if bad := checkFile("doc/API_RESPONSE_EXAMPLES.md", fabricated, pool); bad != 0 {
		t.Errorf(".md 误走了 YAML 扫描，报了 %d 个假阳性", bad)
	}
}
