package telemetry

// 本文件只测一件事：**隐私提示的「更新段」有没有交代版本检查请求真正发出去的每一个头**。
//
// 它是 privacynotice_test.go 那条门禁在另一条上报通路上的同位物。风险完全一样：
// 有人往 clientid.Apply 里加一个头，忘了改文案 —— 于是我们对主播的承诺变成了假的，
// 而四道门禁全绿、谁也不会发现。
//
// 依赖方向：telemetry 的测试引用 clientid，而 clientid **不引用 telemetry**（它只依赖
// logger）。这不是巧合 —— clientid 一旦反向依赖 telemetry，本文件就会成环、这条门禁
// 根本编译不出来。往 clientid 里加 telemetry import 之前先想想这句话。

import (
	"strings"
	"testing"

	"Metabox-Nexus-PlayerCap/clientid"
)

// gatewayNoticeCoverage 把「版本检查请求实际会带的头」映射到「更新段里对应的那句话」。
//
// **新增请求头时，这张表和文案都要改** —— 下面的测试会先一步红。
// 值是文案里必须出现的关键词，用它反查那句话还在不在。
var gatewayNoticeCoverage = map[string]string{
	clientid.HeaderID:      "匿名设备标识",
	clientid.HeaderVersion: "程序版本号",
	clientid.HeaderOS:      "Windows 版本",
	clientid.HeaderEdition: "edition",
	clientid.HeaderArch:    "CPU 架构",
	clientid.HeaderPing:    "「启动」还是「每日在线」",

	// UA 里拼的是产品名 + 版本号 + 系统版本 + 架构，全部已在上面三项里交代过，
	// 没有任何额外信息。登记它是为了让 HeaderNames() 的每一项都有出处，
	// 而不是让它从这张表里消失得无声无息。
	clientid.HeaderUA: "程序版本号",
}

// TestUpdateNoticeCoversEveryRequestHeader 钉死文案覆盖每一个实际发出去的请求头。
//
// 核的是 noticeUpdateSection 而不是整份 privacyNotice：「Windows 版本」「CPU 架构」
// 这些词在遥测段里也有，拿整份做子串匹配的话，只要遥测段还在，更新段被整个删掉都测不出来。
//
// 变异自证：往 clientid.HeaderNames() 里加一项而不改文案，即红。
func TestUpdateNoticeCoversEveryRequestHeader(t *testing.T) {
	for _, header := range clientid.HeaderNames() {
		keyword, known := gatewayNoticeCoverage[header]
		if !known {
			t.Errorf("版本检查请求会带 %q，但 gatewayNoticeCoverage 里没有它 —— "+
				"要么把它写进隐私提示的更新段并登记在这里，要么别发它。"+
				"我们对主播的承诺不能落后于代码。", header)
			continue
		}
		if !strings.Contains(noticeUpdateSection, keyword) {
			t.Errorf("请求头 %q 对应的文案关键词 %q 已经从隐私提示的更新段里消失了",
				header, keyword)
		}
	}
}

// TestUpdateNoticeCoverageHasNoStaleEntries 反向：登记了却已经不发的头要清掉。
//
// 留着它文案就会写着我们其实没发的东西 —— 同样是文案与代码分叉，只是方向相反，
// 而且这个方向不会有任何人抱怨，能一直烂着。
func TestUpdateNoticeCoverageHasNoStaleEntries(t *testing.T) {
	actual := make(map[string]bool, len(clientid.HeaderNames()))
	for _, header := range clientid.HeaderNames() {
		actual[header] = true
	}
	for header := range gatewayNoticeCoverage {
		if !actual[header] {
			t.Errorf("gatewayNoticeCoverage 里登记了 %q，但 clientid 已经不发这个头了 —— "+
				"清掉它，并检查更新段里那句话是不是也该删", header)
		}
	}
}

// TestUpdateNoticeStatesPurposeAndRecipient 钉死「干什么用」和「给谁」还在。
//
// 同 privacynotice_test.go 里的同名断言：「收集了什么」只是一半。这两句被删掉的话，
// 剩下的就只是一张吓人的清单。
func TestUpdateNoticeStatesPurposeAndRecipient(t *testing.T) {
	for _, must := range []string{
		"用途",               // 干什么用
		"gateway.vtb.link", // 给谁 —— 断言域名而非「更新服务器」这种中性说法，域名才具体
		"每天一次",             // 频率：主播有权知道这不是只在启动时发一次
	} {
		if !strings.Contains(noticeUpdateSection, must) {
			t.Errorf("隐私提示的更新段里缺少 %q", must)
		}
	}
}
