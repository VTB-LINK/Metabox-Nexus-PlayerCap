package main

// 本文件钉住随包四个 html 页面的内容（issue #63）：它们自 4.0 起只做一件事，带着全部查询参数
// 跳到线上 v3 的同名页面；唯一的例外是 lyric_page.html，查询串除 lang 外无键（编辑器模式，判定与 3.x
// 的 lyric_page.html 一致）时跳线上新编辑器。
//
// # 它守的是什么
//
// 这四个文件随 exe 发到用户磁盘，并经自动更新覆盖旧文件，写错了没有办法追回：旧版本的页面永远指向
// 它写死的地址。所以内容逐字钉死：除各页自己的 title 外，文件必须与 canonicalRedirectPage 给出的
// 页面一字不差（行尾 CRLF 与 LF 视为相同）。这一条兜住了注释掉的跳转、藏在条件分支或 template 里的
// 跳转、被贴回来的页面逻辑等一切形状；另有几条结构检查（同名映射、https、查询串、无 hash、体积），
// 只是为了在不一致时给出可读的原因。
// 域名换了要同时改这里与四个页面，并让旧域名长期保持可用。
//
// # 它不守什么
//
// 线上 v3 是否可访问、跳转在 OBS 与直播姬里是否生效，都要真机或联网才能测，不在这里。

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

const redirectOrigin = "https://playercap.cfg.vtb.tools/v3/"

// redirectEditor 是线上新编辑器的地址，只有 lyric_page.html 在编辑器模式下跳到它。
const redirectEditor = "https://playercap.cfg.vtb.tools/"

var redirectPages = []string{"lyric_display.html", "lyric_page.html", "effect_display.html", "effect_page.html"}

var redirectTitleRE = regexp.MustCompile(`<title>([^<]*)</title>`)

// redirectComment 与 redirectStatement 是页面脚本里的约定注释与唯一的跳转语句。
func redirectComment(name string) string {
	if name == "lyric_page.html" {
		return "// 4.0 起本页只是跳转页：查询串除 lang 外无键时进线上新编辑器，否则进线上 v3（冻结旧版，参数与 3.x 相同）；查询串原样带过去，不带 hash。"
	}
	return "// 4.0 起本页只是跳转页：页面内容迁到线上 v3（冻结旧版，参数与 3.x 相同），查询串原样带过去，不带 hash。"
}

func redirectStatement(name string) string {
	if name == "lyric_page.html" {
		return "location.replace('" + redirectEditor + "' + ([...new URLSearchParams(location.search).keys()].every(k => k === 'lang') ? '' : 'v3/lyric_page.html') + location.search);"
	}
	return "location.replace('" + redirectOrigin + name + "' + location.search);"
}

// canonicalRedirectPage 是跳转页的约定内容，title 随页面而异。
func canonicalRedirectPage(name, title string) string {
	return "<!DOCTYPE html>\n" +
		"<html lang=\"zh-CN\">\n" +
		"<head>\n" +
		"    <meta charset=\"UTF-8\">\n" +
		"    <title>" + title + "</title>\n" +
		"    <style>\n" +
		"        html, body { margin: 0; background: transparent; }\n" +
		"    </style>\n" +
		"</head>\n" +
		"<body>\n" +
		"<script>\n" +
		redirectComment(name) + "\n" +
		redirectStatement(name) + "\n" +
		"</script>\n" +
		"</body>\n" +
		"</html>\n"
}

// redirectViolations 返回页面 src 不满足跳转页约定的地方。
func redirectViolations(name, src string) []string {
	var v []string
	norm := strings.ReplaceAll(src, "\r\n", "\n")
	want := redirectStatement(name)
	if strings.Count(norm, "location.replace(") != 1 {
		v = append(v, "应当恰有一个 location.replace(")
	}
	if !strings.Contains(norm, want) {
		v = append(v, "没有按约定跳转：应有 "+want)
	}
	if strings.Contains(norm, "http://") {
		v = append(v, "不得出现 http:// 地址")
	}
	if strings.Count(norm, "https://") != 1 {
		v = append(v, "应当恰有一个 https:// 地址（跳转目标）")
	}
	for _, bad := range []string{"location.hash", "location.href", "<iframe", "WebSocket", "fetch(", "<!--", "/*", "<template", "<noscript"} {
		if strings.Contains(norm, bad) {
			v = append(v, "不得出现 "+bad+"：跳转页不带别的逻辑，也不得把跳转藏进注释或惰性元素")
		}
	}
	if len(norm) > 2048 {
		v = append(v, "超过 2 KB：跳转页不该再带页面内容")
	}
	// 兜底：与约定内容逐字一致，兜住注释掉的跳转、条件分支里的跳转等一切形状。
	title := ""
	if m := redirectTitleRE.FindStringSubmatch(norm); m != nil {
		title = m[1]
	}
	if title == "" {
		v = append(v, "缺少 title")
	}
	if norm != canonicalRedirectPage(name, title) {
		v = append(v, "与约定的跳转页内容不一致（除 title 外必须逐字相同，见 canonicalRedirectPage）")
	}
	return v
}

// TestHTMLPagesAreRedirects 四个页面都是写死目标的跳转页。
//
// 变异自证（均实测）：lyric_page.html 的判定去掉（恒跳编辑器或恒跳 v3）、every 改 some、effect_display.html
// 的目标指向 lyric_display.html、去掉 + location.search、https 改 http、换域名、把跳转语句注释掉，均红。
func TestHTMLPagesAreRedirects(t *testing.T) {
	for _, name := range redirectPages {
		t.Run(name, func(t *testing.T) {
			b, err := os.ReadFile(name)
			if err != nil {
				t.Fatalf("读 %s 失败: %v", name, err)
			}
			for _, v := range redirectViolations(name, string(b)) {
				t.Errorf("%s：%s", name, v)
			}
		})
	}
}

// TestRedirectLintCatchesKnownHoles 钉死门禁自身认得出目标形状。
func TestRedirectLintCatchesKnownHoles(t *testing.T) {
	const name = "lyric_display.html"
	good := canonicalRedirectPage(name, "T")
	redirect := redirectStatement(name)

	for _, n := range redirectPages {
		if v := redirectViolations(n, canonicalRedirectPage(n, "T")); len(v) != 0 {
			t.Fatalf("约定内容被误报（%s）: %v", n, v)
		}
	}
	if v := redirectViolations(name, strings.ReplaceAll(good, "\n", "\r\n")); len(v) != 0 {
		t.Fatalf("CRLF 行尾被误报: %v", v)
	}

	sub := func(old, new string) string {
		if !strings.Contains(good, old) {
			t.Fatalf("自测写错了：约定页面里没有 %q", old)
		}
		return strings.Replace(good, old, new, 1)
	}
	shouldCatch := map[string]string{
		"目标指向别的页面":        sub(redirectOrigin+name, redirectOrigin+"effect_display.html"),
		"改成 http":         sub("https://", "http://"),
		"没带查询串":           sub(" + location.search", ""),
		"带上 hash":         sub("location.search)", "location.search + location.hash)"),
		"换了域名":            sub("playercap.cfg.vtb.tools", "cfg.playercap.vtb.tools"),
		"别的页面也跳新编辑器":      sub(redirectOrigin+name+"' + location.search", redirectEditor+"' + location.search"),
		"两个跳转":            sub(redirect, redirect+"\n"+redirect),
		"带了 iframe":       sub("<body>", "<body><iframe src='x'></iframe>"),
		"带了页面逻辑":          sub(redirect, redirect+"\nnew WebSocket('ws://127.0.0.1:8765/ws');"),
		"没有跳转":            sub(redirect, "console.log(1);"),
		"超过 2 KB":         sub("</body>", strings.Repeat("<p>x</p>", 400)+"</body>"),
		"跳转被行注释注释掉":       sub(redirect, "// "+redirect),
		"跳转被块注释注释掉":       sub(redirect, "/* "+redirect+" */"),
		"跳转被 HTML 注释掉":    sub("<script>", "<!--<script>") + "",
		"藏在恒假的条件里":        sub(redirect, "if (false) { "+redirect+" }"),
		"藏在 template 里":   sub("<script>\n", "<template><script>\n"),
		"title 不一样但其余被改动": sub("margin: 0;", "margin: 1px;"),
		"缺少 title":        strings.Replace(good, "<title>T</title>", "", 1),
		"脚本带了属性":          sub("<script>", "<script type=\"text/plain\">"),
	}
	for caseName, src := range shouldCatch {
		t.Run(caseName, func(t *testing.T) {
			if v := redirectViolations(name, src); len(v) == 0 {
				t.Errorf("门禁漏报了这种形状：\n%s", src)
			}
		})
	}
}

// TestLyricPageJudgementIsPinned 钉死 lyric_page.html 独有的判定：查询串除 lang 外无键 → 新编辑器，否则 → v3。
// 它是四个页面里唯一带条件的，判定一旦写歪（恒跳一边、some 代 every、lang 不计入失效），有参数的 OBS 链接
// 会被送进编辑器，或无参数的打开者拿到旧版编辑器，且文件已发到用户磁盘、追不回。
//
// 变异自证（均实测）：每一项 shouldCatch 都是对约定内容的一处改动，门禁须红。
func TestLyricPageJudgementIsPinned(t *testing.T) {
	const name = "lyric_page.html"
	good := canonicalRedirectPage(name, "T")
	if v := redirectViolations(name, good); len(v) != 0 {
		t.Fatalf("约定内容被误报: %v", v)
	}
	stmt := redirectStatement(name)
	sub := func(old, new string) string {
		if !strings.Contains(good, old) {
			t.Fatalf("自测写错了：约定页面里没有 %q", old)
		}
		return strings.Replace(good, old, new, 1)
	}
	shouldCatch := map[string]string{
		"恒跳新编辑器":       sub(stmt, "location.replace('"+redirectEditor+"' + location.search);"),
		"恒跳 v3":        sub(stmt, "location.replace('"+redirectOrigin+name+"' + location.search);"),
		"every 改 some": sub(".every(", ".some("),
		"lang 拼错":      sub("k === 'lang'", "k === 'Lang'"),
		"不再排除 lang":    sub("k === 'lang'", "false"),
		"改成只认空查询串":     sub("[...new URLSearchParams(location.search).keys()].every(k => k === 'lang')", "location.search === ''"),
		"分支对调":         sub("? '' : 'v3/lyric_page.html'", "? 'v3/lyric_page.html' : ''"),
		"v3 分支指向别的页面":  sub("'v3/lyric_page.html'", "'v3/lyric_display.html'"),
		"编辑器分支带 hash":  sub("+ location.search);", "+ location.search + location.hash);"),
		"没带查询串":        sub(" + location.search);", ");"),
		"判定写进恒假条件":     sub(stmt, "if (false) { "+stmt+" }"),
	}
	for caseName, src := range shouldCatch {
		t.Run(caseName, func(t *testing.T) {
			if v := redirectViolations(name, src); len(v) == 0 {
				t.Errorf("门禁漏报了这种形状：\n%s", src)
			}
		})
	}
}
