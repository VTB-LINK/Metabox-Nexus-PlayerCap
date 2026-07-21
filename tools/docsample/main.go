// Command docsample 校验对外文档里的 JSON 示例：**每个数值都必须能在真实录音里找到**。
//
// 为 issue #43 而写。它抓的是一类人抓不住的错：**编造的示例值**。
//
// # 它治什么
//
// doc/API_RESPONSE_EXAMPLES.md 与 doc/openapi.yaml 的示例长期是手写的，而手写示例会长出
// 真实响应里不可能出现的东西：
//
//	"play_time": 45.2, "progress": 0        // 播了 45 秒，进度 0——不可能
//	"timestamp": 9.0,  "play_time": 9.15    // play_time = timestamp - offset，恒小于它
//	"何也もかもが手から零れ落ちる"                // 日语里没有「何也もかも」，打字错
//
// 这些错误 gofmt 不管、JSON 解析通过、人眼扫一遍也看不出。重写文档时**我自己又编了一个**
// （猜了 4 个 words 的时间戳，其中一个 duration 差了近一倍），唯一抓到它的是回头核录音。
//
// # 判据
//
// 示例里的每个数值，都必须在录音池（tools/wsrecord 的产物）里逐字节出现过。
// 这不能证明示例整体真实（数值可能来自不同的歌），但**能挡住凭空编的值**——而那正是
// 已知的、反复发生的失效模式。
//
// 0 / 1 / -1 这类退化值不检查：它们在任何录音里都找得到，检查了也没有区分度。
//
// 用法：
//
//	go run ./tools/docsample -rec <录音目录> doc/API_RESPONSE_EXAMPLES.md
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

var (
	jsonBlock = regexp.MustCompile("(?s)```json\\n(.*?)\\n```")
	numLit    = regexp.MustCompile(`:\s*(-?\d+\.?\d*)`)
	// 播放器产出的文本字段。原先只认 "text"，于是内联 JSON 里
	// `"name":"告白","singer":"花澤香菜"` 这类编造值逃检——它们和 title 字段打架
	// （title 改了、name/singer 没跟着改），却因为字段名不是 text 而漏网。
	strLit = regexp.MustCompile(`"(?:text|name|singer|title|detail)"\s*:\s*"([^"]{4,})"`)

	// yamlLine 拆 `<缩进><key>: <值>`。openapi 的示例是 YAML 结构而非 ```json 块，
	// 只扫 jsonBlock 会让它们**整批逃检**——本文件头举的反面教材 `play_time: 9.15`
	// 当初就是这么在 openapi.yaml 里活下来的：门禁对它报绿。
	yamlLine = regexp.MustCompile(`(?m)^(\s*)([\w-]+):[ \t]*(.*?)[ \t]*$`)
	numOnly  = regexp.MustCompile(`^-?\d+\.?\d*$`)
	quoted   = regexp.MustCompile(`^"(.{4,})"$`)
)

// payloadFields 是**播放器产出的量**——只有它们必须能在录音里找到。
//
// openapi 里另有两类数值不属此列，检查它们只会制造噪音：结构性的（maxLength、
// 端口）、以及 config 的（`wesing-offset`、`qqmusic-poll`——来自 config.yml，
// 播放器录音里根本不会出现）。靠字段名区分，不靠缩进层级。
var payloadFields = map[string]bool{
	"index": true, "timestamp": true, "play_time": true,
	// position 是整曲实时播放位置（pause/resume、all_lyrics 顶层、lyric_update 均有）。
	// 不列入的话它的示例值会整批逃检——这正是 play_time 改名 position 后必须补的一环。
	"position": true,
	"progress": true, "duration": true, "count": true,
	"text": true, "sub_text": true,
	// detail 也是播放器产出的：要么是歌曲标题，要么是「网易云音乐已退出」这类
	// 固定文案——两者录音里都有。title / name / singer / cover 同理。
	//
	// cover_base64 **不列入**：录音归档时它被裁成了长度标记（原始 426MB 有 200MB
	// 是 base64），而文档示例本就写占位符，两边都不是原值，验它必然误报。
	"detail": true, "title": true, "name": true, "singer": true, "cover": true,
}

// trivial 是不值得检查的值。两类：
//
//  1. 退化值（0/1/-1/2）——任何录音里都有，检查了没有区分度。
//  2. **不来自播放器的量**——端口、客户端地址这些由环境决定，录音里当然没有。
//     wsrecord 录的是事件载荷，不含 service-status 的 endpoints 表。
//
// ⚠️ 这张表是**豁免出口**，加条目必须写明「它为什么不可能出现在录音里」。
// 把「录音里没找到」当成「加进豁免表」来解决，就是在给自己开后门——那正是本工具要防的。
var trivial = map[string]bool{
	"0": true, "1": true, "-1": true, "2": true,
	"8765":  true, // 服务端口，来自 config.yml 而非播放器
	"54321": true, // service-status 示例里的客户端端口，环境决定
	"54322": true,
}

func main() {
	rec := flag.String("rec", "", "录音目录（tools/wsrecord 的产物，递归扫 *.jsonl）")
	flag.Parse()
	docs := flag.Args()

	if *rec == "" || len(docs) == 0 {
		fmt.Println("用法: docsample -rec <录音目录> <文档>...")
		os.Exit(2)
	}

	pool, err := loadPool(*rec)
	if err != nil {
		fmt.Println("读录音失败:", err)
		os.Exit(1)
	}
	fmt.Printf("录音池: %.1f MB\n\n", float64(len(pool))/1e6)

	bad := 0
	for _, doc := range docs {
		b, err := os.ReadFile(doc)
		if err != nil {
			fmt.Println("读文档失败:", err)
			os.Exit(1)
		}
		bad += checkFile(doc, string(b), pool)
	}

	fmt.Println()
	if bad > 0 {
		fmt.Printf("✗ %d 个示例含录音里不存在的值 —— 它们是编的，或者录音过期了\n", bad)
		os.Exit(1)
	}
	fmt.Println("✓ 所有示例的数值都能在录音里找到")
}

// loadPool 把所有录音拼成一个大字符串。
//
// 简单粗暴但够用：这些文件加起来几百 MB，一次性读进内存在开发机上没问题，
// 而按行解析再比对反而会漏掉「值在嵌套结构里」的情况。
func loadPool(dir string) (string, error) {
	var sb strings.Builder
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		// .jsonl 是 wsrecord 的流录音，.json 是 capture 脚本的 HTTP 快照——两种都是真实响应。
		// 只扫 .jsonl 会让 HTTP 端点的示例（service-status 那类）无处比对，从而误报。
		if err != nil || d.IsDir() || (!strings.HasSuffix(path, ".jsonl") && !strings.HasSuffix(path, ".json")) {
			return err
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil // 读不了的跳过，不是本工具的职责
		}
		sb.Write(b)
		sb.WriteByte('\n')
		return nil
	})
	return sb.String(), err
}

// checkFile 按文件类型分派检查，返回有问题的示例数。
//
// 抽成函数是为了让**分派本身**能被测到。原先这段逻辑长在 main 里，而 main 会
// os.Exit、测不了——于是「YAML 文档漏掉 checkYAML」这种接线断裂只能靠人眼发现。
// 这个工具存在的全部意义就是不靠人眼，它自己不能例外。
func checkFile(name, src, pool string) int {
	bad := checkDoc(name, src, pool)
	// YAML 的示例在 ```json 块之外，得单独扫；.md 里没有 YAML 示例。
	if strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml") {
		bad += checkYAMLSyntax(name, src)
		bad += checkYAML(name, src, pool)
	}
	return bad
}

// checkYAMLSyntax 确认文件是合法 YAML。
//
// openapi.yaml 是对外契约：语法一挂，Apifox / Swagger 直接打不开。而本工具其余的
// 检查全是正则，对语法错误视而不见——重写示例时往裸标量 summary 里写了个 `index: -1`
// （YAML 把冒号加空格当成 mapping），整个文件解析失败，docsample 照样报「✓ 全绿」。
func checkYAMLSyntax(name, src string) int {
	var probe any
	if err := yaml.Unmarshal([]byte(src), &probe); err != nil {
		fmt.Printf("[%s] YAML 解析失败，Apifox/Swagger 会打不开: %v\n", name, err)
		return 1
	}
	return 0
}

// checkYAML 扫 YAML 结构里的示例值，返回有问题的行数。
//
// 两种形状，openapi.yaml 里都有：
//
//	play_time: 9.15        // examples 的 value 下，直接赋值
//	example: 9.15          // schema 定义下，得回溯才知道它是谁的 example
//
// 后者必须回溯：`example: 200` 既可能是 play_time 的（该查），也可能是
// `wesing-offset` 的（config 的量，录音里没有，查了就是误报）。
func checkYAML(name, src, pool string) int {
	type line struct {
		indent, key, val string
		no               int
	}
	var lines []line
	for i, raw := range strings.Split(src, "\n") {
		m := yamlLine.FindStringSubmatch(raw)
		if m == nil {
			continue
		}
		lines = append(lines, line{indent: m[1], key: m[2], val: m[3], no: i + 1})
	}

	// owner 回溯：往上找第一个缩进更浅的 key，那就是这个 example 所属的字段。
	owner := func(i int) string {
		for j := i - 1; j >= 0; j-- {
			if len(lines[j].indent) < len(lines[i].indent) {
				return lines[j].key
			}
		}
		return ""
	}

	bad := 0
	for i, ln := range lines {
		field := ln.key
		if field == "example" {
			field = owner(i)
		}
		if !payloadFields[field] {
			continue
		}
		switch {
		case numOnly.MatchString(ln.val):
			if !trivial[ln.val] && !strings.Contains(pool, ln.val) {
				fmt.Printf("[%s:%d] %s: %s —— 录音里没有这个值\n", name, ln.no, ln.key, ln.val)
				bad++
			}
		// 用 field 而非 ln.key：`example: "…"` 回溯出的 owner 才是 text/detail 等。
		case !numOnly.MatchString(ln.val):
			if m := quoted.FindStringSubmatch(ln.val); m != nil && !strings.Contains(pool, m[1]) {
				fmt.Printf("[%s:%d] %s: %q —— 录音里没有这句歌词\n", name, ln.no, field, m[1])
				bad++
			}
		}
	}

	// description 里的内联 JSON（`data: {"type":"lyric_update",...}`）也带歌词文本，
	// 它们不在 ```json 块里，checkDoc 扫不到。
	for _, s := range strLit.FindAllStringSubmatch(src, -1) {
		if v := s[1]; !strings.Contains(pool, v) {
			fmt.Printf("[%s] 内联 JSON 的 text: %q —— 录音里没有这句歌词\n", name, v)
			bad++
		}
	}
	return bad
}

// checkDoc 返回有问题的示例块数。
func checkDoc(name, src, pool string) int {
	bad := 0
	for i, m := range jsonBlock.FindAllStringSubmatch(src, -1) {
		block := m[1]

		// 先确保它是合法 JSON——不合法的示例本身就是缺陷（曾有 "words": [...] 这种）
		var probe any
		if err := json.Unmarshal([]byte(block), &probe); err != nil {
			fmt.Printf("[%s 示例 %d] JSON 不合法: %v\n", name, i+1, err)
			bad++
			continue
		}

		var missing []string
		for _, n := range numLit.FindAllStringSubmatch(block, -1) {
			if v := n[1]; !trivial[v] && !strings.Contains(pool, v) {
				missing = append(missing, v)
			}
		}
		// 歌词文本同样要能找到——"何也もかもが…" 那种打字错就是这么混进去的
		for _, s := range strLit.FindAllStringSubmatch(block, -1) {
			if v := s[1]; !strings.Contains(pool, v) {
				missing = append(missing, fmt.Sprintf("%q", v))
			}
		}

		if len(missing) > 0 {
			fmt.Printf("[%s 示例 %d] 录音里找不到这些值: %s\n", name, i+1, strings.Join(missing, ", "))
			bad++
		}
	}
	return bad
}
