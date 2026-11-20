package qqmusic

// 本文件只测随包使用说明 doc/release/README.html（#215）列出的 QQ 音乐版本与 knownVersions 完全相同。
// 说明随发版出货，代码收录或删去版本而说明没跟上，主播读到的支持范围就是错的。
//
// 版本清单与界面语言无关，页面里只写一处：<ul id="qq-versions">，每个版本一个不带属性的 <li>。
// 这个 id 须恰好出现一次；列表里除这些 <li> 与空白外不得有别的内容，每一项须形如 22.82，认不出的项、
// 重复项都报出来，不跳过。
//
// 变异自证（实测，在工作区之外的副本里逐条施加，均红，改回即绿）：
//   - mem.go 的 knownVersions 加一项 "22.90"
//   - 删去 <li>22.82</li>
//   - 多写 <li>22.90</li>
//   - 整个列表复制一份（id 出现 2 次）
//   - 22.60 写成 22.6（认不出的项）
//   - 22.81 写成 22.82（重复项）
//   - 22.71 写成 <li class="x">22.71</li>（列表里有认不出的内容）

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

const releaseDocPath = "../../doc/release/README.html"

var (
	releaseDocList    = regexp.MustCompile(`(?s)<ul id="qq-versions">(.*?)</ul>`)
	releaseDocItem    = regexp.MustCompile(`<li>([^<]*)</li>`)
	releaseDocVersion = regexp.MustCompile(`^\d+\.\d{2}$`)
)

func TestReleaseDocListsKnownVersions(t *testing.T) {
	var want []string
	for v := range knownVersions {
		want = append(want, v)
	}
	slices.Sort(want)

	data, err := os.ReadFile(releaseDocPath)
	if err != nil {
		t.Fatalf("读不到 %s: %v", releaseDocPath, err)
	}
	doc := string(data)
	if n := strings.Count(doc, `id="qq-versions"`); n != 1 {
		t.Fatalf(`%s: id="qq-versions" 出现 %d 次，应恰好 1 次`, releaseDocPath, n)
	}
	m := releaseDocList.FindStringSubmatch(doc)
	if m == nil {
		t.Fatalf(`%s: 找不到 <ul id="qq-versions">…</ul>`, releaseDocPath)
	}
	if rest := strings.TrimSpace(releaseDocItem.ReplaceAllString(m[1], "")); rest != "" {
		t.Errorf("%s: 版本清单里有认不出的内容 %q", releaseDocPath, rest)
	}

	var got []string
	for _, item := range releaseDocItem.FindAllStringSubmatch(m[1], -1) {
		v := strings.TrimSpace(item[1])
		switch {
		case !releaseDocVersion.MatchString(v):
			t.Errorf("%s: 版本清单里有认不出的项 %q", releaseDocPath, v)
		case slices.Contains(got, v):
			t.Errorf("%s: 版本清单里 %s 重复", releaseDocPath, v)
		default:
			got = append(got, v)
		}
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("%s 列出的版本为 %q，knownVersions 为 %q", releaseDocPath, got, want)
	}
}
