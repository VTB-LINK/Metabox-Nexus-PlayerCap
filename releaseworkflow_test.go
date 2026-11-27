package main

// 本文件守三件事：发版流水线 release.yml 在演练时不发布（R1–R4，#164，设计见
// doc/design/ci-windows-release-design.md §3.2）、作业与全部 PowerShell HTTP 请求显式设超时、
// 避免网络慢链或卡死占满 runner（R5，#256），以及 Sentry commit 关联（Associate commits to
// Sentry release）容忍前序发行版 SHA 缺失并在失败时先写入作业摘要再以原退出码退出（R6，#166）。
//
// 发布与否只看发版作业 env 里的 PUBLISH：tag 推送为真，分支推送与手动派发为假。演练若误建了
// draft，dotcom 那份一旦被人发布就是一次真实发版；Gitee 没有草稿，误建即公开；Sentry 里也会多出
// 非 semver 的 release。
// workflow 不参与编译，改坏这些写法时四道门禁照样全绿，故在这里按 YAML 结构与文本核对：
//
//	R1 判据只在发版作业的 env 里定义一次，写法同设计 §3.2.1；workflow 级 env、其他作业、
//	   步骤级 env 与脚本（经 GITHUB_ENV）都不得再定义 PUBLISH。
//	R2 environment 的表达式与判据一致：发布取 build-production，演练取 build-dev。
//	R3 发布步骤（softprops 建 draft；脚本里调 gh release、generate-notes、Gitee API、sentry-cli 的
//	   releases 与 deploys）的 if 恰为 env.PUBLISH == 'true'，且恰为已知的八步；sentry-cli 只经
//	   $env:SENTRY_CLI（Install sentry-cli 校验通过才写入的绝对路径）调用，不按名字取 PATH 里的（代码审第 1 轮）。
//	R4 三个发布凭据只出现在步骤级 env，每处都写成只在判据为真时取值的形式。
//	R5 发版作业的 timeout-minutes 在 1..60 之间；所有作业的步骤脚本中每处 Invoke-WebRequest /
//	   Invoke-RestMethod 调用都显式指定正整数 -TimeoutSec。
//	R6 Associate commits to Sentry release 步骤保持 continue-on-error: true；经 $env:SENTRY_CLI
//	   调用 releases set-commits 时同时携带 --auto 与 --ignore-missing；其后存在 $LASTEXITCODE
//	   非零检查分支，在首次顶层 exit 之前向 $env:GITHUB_STEP_SUMMARY 追加告警并以原退出码 $LASTEXITCODE exit。
//
// 只按字面核对。用别的方式发布（如直接调 GitHub API）、用 GITHUB_TOKEN 发布，这里查不到（设计 §3.2.6）。
// R3 按子串匹配脚本，注释也算：不发布的步骤里不要写这几个命令与 Gitee API 的地址。
//
// 变异自证：TestReleaseWorkflowCatchesMutations 对 release.yml 原文逐条施加变异，每条须被对应规则
// 报出。另在工作区直接改 release.yml 实测过（TestReleaseWorkflowPublishGuards 均红，改回即绿）：
// 删掉 Create Release Draft (本仓库) 的 if（R3）、判据去掉 event_name（R1）、Create Sentry release
// 的凭据去掉门控（R4）、environment 写死为 build-production（R2）、删掉 Create Release (Gitee mirror)
// 的 if（R3，#208）、删掉 Prune old releases (Gitee mirror) 的 if（R3，#209）。

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const releaseWorkflowPath = ".github/workflows/release.yml"

const (
	releaseJobID             = "release"
	publishPredicate         = "${{ github.event_name == 'push' && github.ref_type == 'tag' }}"
	releaseEnvExpr           = "${{ github.event_name == 'push' && github.ref_type == 'tag' && 'build-production' || 'build-dev' }}"
	publishCondition         = "env.PUBLISH == 'true'"
	publishActionUses        = "softprops/action-gh-release"
	sentrySetCommitsStepName = "Associate commits to Sentry release"
)

// publishSecrets 是只在发布时才可引用的凭据。
var publishSecrets = []string{"GH_DOTCOM_TOKEN", "GITEE_TOKEN", "SENTRY_AUTH_TOKEN"}

// publishCommands 出现在某步脚本里，该步即为发布步骤。按名字调用 sentry-cli 的两种写法也算，R3 另报它们。
var publishCommands = []string{"gh release", "generate-notes", "gitee.com/api", "SENTRY_CLI releases", "SENTRY_CLI deploys",
	"sentry-cli releases", "sentry-cli deploys"}

// bareSentryCLI 是按名字调用 sentry-cli：取到的可能是 PATH 里未经校验的另一份。
var bareSentryCLI = regexp.MustCompile(`(?m)^\s*sentry-cli\s`)

// psHTTPCommand 匹配 PowerShell HTTP 请求命令（不区分大小写）。
var psHTTPCommand = regexp.MustCompile(`(?i)\bInvoke-(?:RestMethod|WebRequest)\b`)

// psTimeoutSec 匹配合法正整数 -TimeoutSec 参数（支持空格或冒号分隔，拒绝 0、负数与非正整数）。
var psTimeoutSec = regexp.MustCompile(`(?i)(?:^|\s)-TimeoutSec(?:\s+|:)[1-9]\d*(?:\s|$|[;|\)\}])`)

// R6：Sentry set-commits 命令行与失败分支核对用正则。
var (
	psSentrySetCommits  = regexp.MustCompile(`(?i)(?:^\s*|[;&|{(=]\s*)(&\s+\$\{?env:SENTRY_CLI\}?\s+releases\s+set-commits(?:\s|$))`)
	psFlagAuto          = regexp.MustCompile(`(?:^|\s)--auto(?:\s|$)`)
	psFlagIgnoreMissing = regexp.MustCompile(`(?:^|\s)--ignore-missing(?:\s|$)`)
	psLastExitNonZeroIf = regexp.MustCompile(`(?i)\bif\s*\(\s*(?:\$LASTEXITCODE(?:\s+-ne\s+0)?|0\s+-ne\s+\$LASTEXITCODE)\s*\)\s*\{`)
	psAddContentCmd     = regexp.MustCompile(`(?i)(?:^\s*|[;&|{(]\s*)(Add-Content)\b`)
	psOutFileCmd        = regexp.MustCompile(`(?i)(?:^\s*|[;&|{(]\s*)(Out-File)\b`)
	psStepSummaryToken  = regexp.MustCompile(`(?i)^(?:\$\{?env:GITHUB_STEP_SUMMARY\}?|"\$\{?env:GITHUB_STEP_SUMMARY\}?")$`)
	psRedirAppend       = regexp.MustCompile(`>>`)
	psStepSummaryTarget = regexp.MustCompile(`(?i)^\s*(?:\$\{?env:GITHUB_STEP_SUMMARY\}?|"\$\{?env:GITHUB_STEP_SUMMARY\}?")(?:[\s;|\)\}]|$)`)
	psVarAssign         = regexp.MustCompile(`(?i)^\$([A-Za-z_]\w*)\s*=\s*(.+)$`)
	psAnyVarAssignName  = regexp.MustCompile(`(?i)\$([A-Za-z_]\w*)\s*=`)
	psExitCmd           = regexp.MustCompile(`(?i)^exit(?:\s+(.*))?$`)
	psExitWord          = regexp.MustCompile(`(?i)\bexit\b`)
	psLastExitVar       = regexp.MustCompile(`(?i)^\$LASTEXITCODE$`)
	psSimpleVar         = regexp.MustCompile(`(?i)^\$([A-Za-z_]\w*)$`)
)

// psFileWriteSwitches 是 Add-Content / Out-File 常见开关参数（小写），不消费后续词元。
var psFileWriteSwitches = map[string]bool{
	"append":       true,
	"asbytestream": true,
	"confirm":      true,
	"debug":        true,
	"force":        true,
	"noclobber":    true,
	"nonewline":    true,
	"passthru":     true,
	"raw":          true,
	"verbose":      true,
	"whatif":       true,
}

// knownPublishSteps 是 release.yml 的全部发布步骤。新增或改名发布步骤要同步这里，扫描认不出某一步时也在这里暴露。
var knownPublishSteps = []string{
	sentrySetCommitsStepName,
	"Create Release (Gitee mirror)",
	"Create Release Draft (dotcom mirror)",
	"Create Release Draft (本仓库)",
	"Create Sentry release",
	"Finalize Sentry release",
	"Generate release notes (dotcom mirror)",
	"Prune old releases (Gitee mirror)",
}

type workflowFile struct {
	Env  map[string]any         `yaml:"env"`
	Jobs map[string]workflowJob `yaml:"jobs"`
}

type workflowJob struct {
	TimeoutMinutes int            `yaml:"timeout-minutes"`
	Env            map[string]any `yaml:"env"`
	Environment    any            `yaml:"environment"`
	Steps          []workflowStep `yaml:"steps"`
}

type workflowStep struct {
	Name            string         `yaml:"name"`
	If              string         `yaml:"if"`
	ContinueOnError bool           `yaml:"continue-on-error"`
	Uses            string         `yaml:"uses"`
	Run             string         `yaml:"run"`
	Env             map[string]any `yaml:"env"`
}

func isPublishStep(s workflowStep) bool {
	if strings.HasPrefix(s.Uses, publishActionUses) {
		return true
	}
	for _, c := range publishCommands {
		if strings.Contains(s.Run, c) {
			return true
		}
	}
	return false
}

// psLogicalStatements 将 PowerShell 脚本规范化为逻辑语句：剥除引号外的 <# ... #> 块注释与
// 整行/行尾 # 注释，合并反引号续行与跨行的 @{ ... } 哈希表字面量（普通控制流块 { 不合并）。
func psLogicalStatements(script string) []string {
	normalized := stripPSBlockComments(strings.ReplaceAll(script, "\r\n", "\n"))
	var stmts []string
	var cur strings.Builder
	hashDepth := 0

	flush := func() {
		if cur.Len() > 0 {
			stmts = append(stmts, cur.String())
			cur.Reset()
		}
	}

	for _, raw := range strings.Split(normalized, "\n") {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		line := stripPSInlineComment(trimmed)
		if line == "" {
			continue
		}

		continued := endsWithPSContinuation(line)
		if continued {
			line = strings.TrimRight(line[:len(line)-1], " \t")
		}

		if cur.Len() > 0 {
			cur.WriteByte(' ')
		}
		cur.WriteString(line)

		hashDepth = updatePSHashDepth(line, hashDepth)
		if !continued && hashDepth == 0 {
			flush()
		}
	}
	flush()
	return stmts
}

// stripPSBlockComments 剥除引号与单行 # 注释之外的 PowerShell <# ... #> 块注释（替换为空格并保留换行）。
func stripPSBlockComments(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	var quote byte
	for i := 0; i < len(s); i++ {
		if next, inQuote := consumePSQuote(s, i, &quote); inQuote {
			b.WriteString(s[i : next+1])
			i = next
			continue
		}
		switch s[i] {
		case '\'', '"':
			quote = s[i]
			b.WriteByte(s[i])
		case '#':
			// 单行注释直到行尾，其中的 <# 不开启块注释。
			if i == 0 || strings.ContainsRune("\n \t;|(){}", rune(s[i-1])) {
				end := strings.IndexByte(s[i:], '\n')
				if end < 0 {
					b.WriteString(s[i:])
					return b.String()
				}
				b.WriteString(s[i : i+end])
				i += end - 1
				continue
			}
			b.WriteByte('#')
		case '<':
			if i+1 < len(s) && s[i+1] == '#' && (i == 0 || strings.ContainsRune("\n \t;|(){}", rune(s[i-1]))) {
				b.WriteString("  ")
				i += 2
				for i < len(s) {
					if s[i] == '#' && i+1 < len(s) && s[i+1] == '>' {
						b.WriteString("  ")
						i++
						break
					}
					if s[i] == '\n' {
						b.WriteByte('\n')
					} else {
						b.WriteByte(' ')
					}
					i++
				}
				continue
			}
			b.WriteByte('<')
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// consumePSQuote 推进 PowerShell 单双引号内的字符与转义；若 s[i] 在引号内或关闭当前引号，返回新下标与 true。
func consumePSQuote(s string, i int, quote *byte) (int, bool) {
	switch *quote {
	case '\'':
		if s[i] == '\'' {
			if i+1 < len(s) && s[i+1] == '\'' {
				return i + 1, true
			}
			*quote = 0
		}
		return i, true
	case '"':
		if s[i] == '`' && i+1 < len(s) {
			return i + 1, true
		}
		if s[i] == '"' {
			*quote = 0
		}
		return i, true
	}
	return i, false
}

func stripPSInlineComment(line string) string {
	var quote byte
	for i := 0; i < len(line); i++ {
		if next, inQuote := consumePSQuote(line, i, &quote); inQuote {
			i = next
			continue
		}
		switch line[i] {
		case '\'', '"':
			quote = line[i]
		case '#':
			if i == 0 || strings.ContainsRune(" \t;|({}", rune(line[i-1])) {
				return strings.TrimRight(line[:i], " \t")
			}
		}
	}
	return strings.TrimRight(line, " \t")
}

func endsWithPSContinuation(line string) bool {
	n := 0
	for i := len(line) - 1; i >= 0 && line[i] == '`'; i-- {
		n++
	}
	return n%2 == 1
}

func updatePSHashDepth(line string, depth int) int {
	var quote byte
	for i := 0; i < len(line); i++ {
		if next, inQuote := consumePSQuote(line, i, &quote); inQuote {
			i = next
			continue
		}
		switch line[i] {
		case '\'', '"':
			quote = line[i]
		case '@':
			if depth == 0 && i+1 < len(line) && line[i+1] == '{' {
				depth = 1
				i++
			}
		case '{':
			if depth > 0 {
				depth++
			}
		case '}':
			if depth > 0 {
				depth--
			}
		}
	}
	return depth
}

// psCallArgs 返回从 stmt[start:] 开始的单次命令调用的顶层参数文本：
// 跳过引号字面量与嵌套 (...) / { ... } 内部文本，遇到当前层级的 ;、|、&& 或外层闭合括号即止。
func psCallArgs(stmt string, start int) string {
	var b strings.Builder
	var quote byte
	paren, brace := 0, 0

	for i := start; i < len(stmt); i++ {
		if next, inQuote := consumePSQuote(stmt, i, &quote); inQuote {
			i = next
			if quote == 0 {
				b.WriteByte(' ')
			}
			continue
		}
		c := stmt[i]
		switch c {
		case '\'', '"':
			quote = c
			b.WriteByte(' ')
		case '(':
			paren++
			b.WriteByte(' ')
		case ')':
			paren--
			if paren < 0 {
				return b.String()
			}
			b.WriteByte(' ')
		case '{':
			brace++
			b.WriteByte(' ')
		case '}':
			brace--
			if brace < 0 {
				return b.String()
			}
			b.WriteByte(' ')
		case ';', '|':
			if paren == 0 && brace == 0 {
				return b.String()
			}
		case '&':
			if paren == 0 && brace == 0 && i+1 < len(stmt) && stmt[i+1] == '&' {
				return b.String()
			}
			if paren == 0 && brace == 0 {
				b.WriteByte(c)
			}
		default:
			if paren == 0 && brace == 0 {
				b.WriteByte(c)
			}
		}
	}
	return b.String()
}

// psMaskQuoted 将 PowerShell 单双引号字面量（含引号本身）替换为空格，保持字节长度与偏移不变。
func psMaskQuoted(s string) string {
	b := []byte(s)
	var quote byte
	for i := 0; i < len(b); i++ {
		if next, inQuote := consumePSQuote(s, i, &quote); inQuote {
			for j := i; j <= next; j++ {
				if b[j] != '\n' {
					b[j] = ' '
				}
			}
			i = next
			continue
		}
		if b[i] == '\'' || b[i] == '"' {
			quote = b[i]
			b[i] = ' '
		}
	}
	return string(b)
}

// psRawCommandSegment 返回从 rawLine[start:] 开始、按 maskedLine 定位的单次命令原始切片，
// 遇到当前层级的 ;、|、&& 或外层闭合括号即止。
func psRawCommandSegment(rawLine, maskedLine string, start int) string {
	paren, brace := 0, 0
	for i := start; i < len(maskedLine); i++ {
		switch maskedLine[i] {
		case '(':
			paren++
		case ')':
			paren--
			if paren < 0 {
				return rawLine[start:i]
			}
		case '{':
			brace++
		case '}':
			brace--
			if brace < 0 {
				return rawLine[start:i]
			}
		case ';', '|':
			if paren == 0 && brace == 0 {
				return rawLine[start:i]
			}
		case '&':
			if paren == 0 && brace == 0 && i+1 < len(maskedLine) && maskedLine[i+1] == '&' {
				return rawLine[start:i]
			}
		}
	}
	return rawLine[start:]
}

func psFindMatchingBrace(masked string, openIdx int) int {
	depth := 0
	for i := openIdx; i < len(masked); i++ {
		switch masked[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// psSplitCommandTokens 按顶层空白切分单条 PowerShell 命令的词元，保持引号字符串与成对 ()/{} 完整。
func psSplitCommandTokens(cmd string) []string {
	var tokens []string
	var cur strings.Builder
	var quote byte
	paren, brace := 0, 0
	flush := func() {
		if cur.Len() > 0 {
			tokens = append(tokens, cur.String())
			cur.Reset()
		}
	}
	for i := 0; i < len(cmd); i++ {
		if next, inQuote := consumePSQuote(cmd, i, &quote); inQuote {
			cur.WriteString(cmd[i : next+1])
			i = next
			continue
		}
		c := cmd[i]
		switch c {
		case '\'', '"':
			quote = c
			cur.WriteByte(c)
		case '(':
			paren++
			cur.WriteByte(c)
		case ')':
			if paren > 0 {
				paren--
			}
			cur.WriteByte(c)
		case '{':
			brace++
			cur.WriteByte(c)
		case '}':
			if brace > 0 {
				brace--
			}
			cur.WriteByte(c)
		case ' ', '\t':
			if paren == 0 && brace == 0 {
				flush()
			} else {
				cur.WriteByte(c)
			}
		default:
			cur.WriteByte(c)
		}
	}
	flush()
	return tokens
}

func isPSParamToken(tok string) bool {
	if len(tok) < 2 || tok[0] != '-' {
		return false
	}
	c := tok[1]
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// psCommandWritesStepSummary 严格核对单条 Add-Content / Out-File 命令将 $env:GITHUB_STEP_SUMMARY
// 绑定为目标路径（显式 -Path/-LiteralPath/Out-File 的 -FilePath，或无显式路径时的 Position 0 位置参数），
// 并要求 Out-File 显式携带 -Append。
func psCommandWritesStepSummary(rawCmd string, isOutFile bool) bool {
	tokens := psSplitCommandTokens(rawCmd)
	if len(tokens) < 2 {
		return false
	}
	hasAppend := !isOutFile
	hasExplicitPath := false
	explicitPathIsSummary := false
	var positional []string
	endOfParams := false

	for i := 1; i < len(tokens); i++ {
		tok := tokens[i]
		if !endOfParams && tok == "--" {
			endOfParams = true
			continue
		}
		if !endOfParams && isPSParamToken(tok) {
			name, inlineVal, hasColon := strings.Cut(tok[1:], ":")
			lowerName := strings.ToLower(name)
			if lowerName == "append" {
				if !hasColon {
					hasAppend = true
				} else {
					hasAppend = strings.EqualFold(inlineVal, "$true") || inlineVal == "1"
				}
				continue
			}
			isPathParam := lowerName == "path" || lowerName == "literalpath" || (isOutFile && lowerName == "filepath")
			if isPathParam {
				if hasExplicitPath {
					return false
				}
				hasExplicitPath = true
				val := inlineVal
				if !hasColon {
					if i+1 >= len(tokens) {
						return false
					}
					i++
					val = tokens[i]
				}
				explicitPathIsSummary = psStepSummaryToken.MatchString(val)
				continue
			}
			if hasColon || psFileWriteSwitches[lowerName] {
				continue
			}
			if i+1 < len(tokens) {
				i++
			}
			continue
		}
		positional = append(positional, tok)
	}
	if !hasAppend {
		return false
	}
	if hasExplicitPath {
		return explicitPathIsSummary
	}
	return len(positional) > 0 && psStepSummaryToken.MatchString(positional[0])
}

// psBranchWritesStepSummary 判定分支体中是否包含向 $env:GITHUB_STEP_SUMMARY 追加写入的命令。
func psBranchWritesStepSummary(branchRaw, branchMasked string) bool {
	rawLines := strings.Split(branchRaw, "\n")
	maskedLines := strings.Split(branchMasked, "\n")
	for i := range rawLines {
		rawLine, maskedLine := rawLines[i], maskedLines[i]
		for _, loc := range psRedirAppend.FindAllStringIndex(maskedLine, -1) {
			if loc[0] > 0 && (maskedLine[loc[0]-1] == '>' || (maskedLine[loc[0]-1] >= '0' && maskedLine[loc[0]-1] <= '9')) {
				continue
			}
			if psStepSummaryTarget.MatchString(rawLine[loc[1]:]) {
				return true
			}
		}
		for _, loc := range psAddContentCmd.FindAllStringSubmatchIndex(maskedLine, -1) {
			rawCmd := psRawCommandSegment(rawLine, maskedLine, loc[2])
			if psCommandWritesStepSummary(rawCmd, false) {
				return true
			}
		}
		for _, loc := range psOutFileCmd.FindAllStringSubmatchIndex(maskedLine, -1) {
			rawCmd := psRawCommandSegment(rawLine, maskedLine, loc[2])
			if psCommandWritesStepSummary(rawCmd, true) {
				return true
			}
		}
	}
	return false
}

type psTopLevelCmd struct {
	text  string
	start int
}

// psTopLevelCommands 提取 branchMasked 顶层（跳过嵌套 { ... } 块）的各条命令及其起始偏移。
func psTopLevelCommands(branchMasked string) []psTopLevelCmd {
	var cmds []psTopLevelCmd
	var cur strings.Builder
	cmdStart := -1
	brace, paren := 0, 0
	flush := func() {
		if s := strings.TrimSpace(cur.String()); s != "" {
			cmds = append(cmds, psTopLevelCmd{text: s, start: cmdStart})
		}
		cur.Reset()
		cmdStart = -1
	}
	appendByte := func(i int, c byte) {
		if cmdStart < 0 && c != ' ' && c != '\t' && c != '\r' && c != '\n' {
			cmdStart = i
		}
		cur.WriteByte(c)
	}
	for i := 0; i < len(branchMasked); i++ {
		c := branchMasked[i]
		if brace > 0 {
			switch c {
			case '{':
				brace++
			case '}':
				brace--
			}
			continue
		}
		switch c {
		case '{':
			flush()
			brace = 1
		case '}':
			flush()
		case '(':
			paren++
			appendByte(i, c)
		case ')':
			if paren > 0 {
				paren--
			}
			appendByte(i, c)
		case '\n', ';':
			if paren == 0 {
				flush()
			} else {
				appendByte(i, c)
			}
		default:
			appendByte(i, c)
		}
	}
	flush()
	return cmds
}

// psBranchFirstExit 查找分支顶层首次出现的无条件 exit，返回其在 branchMasked 中的起始偏移，
// 并判定该首次 exit 是否直接保留了 $LASTEXITCODE（或经顶层赋值链传递的 $LASTEXITCODE），
// 拒绝固定整数退出码（含 exit 0 / exit 1）、裸 exit、未绑定 $LASTEXITCODE 的变量以及含 exit 的 &&/|| 条件链。
// 未找到合法顶层 exit 时返回 (-1, false)。
func psBranchFirstExit(branchMasked string) (int, bool) {
	lastExitVars := make(map[string]bool)
	for _, cmd := range psTopLevelCommands(branchMasked) {
		if strings.Contains(cmd.text, "&&") || strings.Contains(cmd.text, "||") {
			if psExitWord.MatchString(cmd.text) {
				return -1, false
			}
			for _, m := range psAnyVarAssignName.FindAllStringSubmatch(cmd.text, -1) {
				delete(lastExitVars, strings.ToLower(m[1]))
			}
			continue
		}
		if m := psVarAssign.FindStringSubmatch(cmd.text); m != nil {
			name := strings.ToLower(m[1])
			rhs := strings.TrimSpace(m[2])
			if psLastExitVar.MatchString(rhs) {
				lastExitVars[name] = true
			} else if vm := psSimpleVar.FindStringSubmatch(rhs); vm != nil && lastExitVars[strings.ToLower(vm[1])] {
				lastExitVars[name] = true
			} else {
				delete(lastExitVars, name)
			}
			continue
		}
		if m := psExitCmd.FindStringSubmatch(cmd.text); m != nil {
			arg := strings.TrimSpace(m[1])
			if psLastExitVar.MatchString(arg) {
				return cmd.start, true
			}
			if vm := psSimpleVar.FindStringSubmatch(arg); vm != nil && lastExitVars[strings.ToLower(vm[1])] {
				return cmd.start, true
			}
			return cmd.start, false
		}
	}
	return -1, false
}

// checkSentrySetCommitsRun 核对 Associate commits to Sentry release 步骤脚本是否满足 R6 要求。
func checkSentrySetCommitsRun(stepName, run string, report func(rule, format string, args ...any)) {
	stmts := psLogicalStatements(run)
	setCommitsIdx := -1
	var setCommitsArgs string
	var sameLineTail string

	for i, stmt := range stmts {
		masked := psMaskQuoted(stmt)
		loc := psSentrySetCommits.FindStringSubmatchIndex(masked)
		if loc == nil {
			continue
		}
		ampIdx := loc[2]
		setCommitsIdx = i
		setCommitsArgs = psCallArgs(stmt, ampIdx)
		rawCmd := psRawCommandSegment(stmt, masked, ampIdx)
		if end := ampIdx + len(rawCmd); end < len(stmt) && stmt[end] == ';' {
			sameLineTail = strings.TrimSpace(stmt[end+1:])
		}
		break
	}

	if setCommitsIdx < 0 {
		report("R6", "步骤 %q 缺少经 $env:SENTRY_CLI 调用的 releases set-commits 语句", stepName)
		return
	}
	if !psFlagAuto.MatchString(setCommitsArgs) || !psFlagIgnoreMissing.MatchString(setCommitsArgs) {
		report("R6", "步骤 %q 的 releases set-commits 调用须同时携带 --auto 与 --ignore-missing", stepName)
	}

	var afterRaw []string
	if sameLineTail != "" {
		afterRaw = append(afterRaw, sameLineTail)
	}
	afterRaw = append(afterRaw, stmts[setCommitsIdx+1:]...)
	afterMasked := make([]string, len(afterRaw))
	for i, s := range afterRaw {
		afterMasked[i] = psMaskQuoted(s)
	}
	afterRawJoined := strings.Join(afterRaw, "\n")
	afterMaskedJoined := strings.Join(afterMasked, "\n")

	matches := psLastExitNonZeroIf.FindAllStringIndex(afterMaskedJoined, -1)
	if len(matches) == 0 {
		report("R6", "步骤 %q 在 releases set-commits 之后缺少 $LASTEXITCODE 非零检查分支", stepName)
		return
	}

	anySummary, anyExit, anyBoth := false, false, false
	for _, m := range matches {
		openBrace := m[1] - 1
		closeBrace := psFindMatchingBrace(afterMaskedJoined, openBrace)
		if closeBrace < 0 {
			continue
		}
		bRaw := afterRawJoined[openBrace+1 : closeBrace]
		bMasked := afterMaskedJoined[openBrace+1 : closeBrace]
		exitStart, wExit := psBranchFirstExit(bMasked)
		summaryRaw, summaryMasked := bRaw, bMasked
		if exitStart >= 0 {
			summaryRaw, summaryMasked = bRaw[:exitStart], bMasked[:exitStart]
		}
		wSummary := psBranchWritesStepSummary(summaryRaw, summaryMasked)
		if wSummary {
			anySummary = true
		}
		if wExit {
			anyExit = true
		}
		if wSummary && wExit {
			anyBoth = true
		}
	}
	if !anyBoth {
		if !anySummary {
			report("R6", "步骤 %q 的 $LASTEXITCODE 非零分支未在首次 exit 前向 $env:GITHUB_STEP_SUMMARY 追加告警", stepName)
		}
		if !anyExit {
			report("R6", "步骤 %q 的 $LASTEXITCODE 非零分支未以原退出码 $LASTEXITCODE exit", stepName)
		}
		if anySummary && anyExit {
			report("R6", "步骤 %q 未在同一个 $LASTEXITCODE 非零分支内先追加 $env:GITHUB_STEP_SUMMARY 再以原退出码 exit", stepName)
		}
	}
}

// checkReleaseWorkflow 返回 data（release.yml 的内容）违反 R1–R6 的各处，每条以规则名开头。
func checkReleaseWorkflow(data []byte) []string {
	var found []string
	report := func(rule, format string, args ...any) {
		found = append(found, rule+": "+fmt.Sprintf(format, args...))
	}

	var wf workflowFile
	if err := yaml.Unmarshal(data, &wf); err != nil {
		report("parse", "%v", err)
		return found
	}
	job, ok := wf.Jobs[releaseJobID]
	if !ok {
		report("R1", "没有作业 %s", releaseJobID)
		return found
	}

	// R1
	if got, _ := job.Env["PUBLISH"].(string); got != publishPredicate {
		report("R1", "jobs.%s.env.PUBLISH 为 %q，应为 %q", releaseJobID, got, publishPredicate)
	}
	if _, ok := wf.Env["PUBLISH"]; ok {
		report("R1", "workflow 级 env 定义了 PUBLISH")
	}
	for id, j := range wf.Jobs {
		if _, ok := j.Env["PUBLISH"]; ok && id != releaseJobID {
			report("R1", "作业 %s 的 env 定义了 PUBLISH", id)
		}
		for _, s := range j.Steps {
			if _, ok := s.Env["PUBLISH"]; ok {
				report("R1", "步骤 %q 的 env 定义了 PUBLISH", s.Name)
			}
			if strings.Contains(s.Run, "PUBLISH=") {
				report("R1", "步骤 %q 的脚本写了 PUBLISH=", s.Name)
			}
		}
	}

	// R2
	environment, _ := job.Environment.(map[string]any)
	if got, _ := environment["name"].(string); got != releaseEnvExpr {
		report("R2", "jobs.%s.environment.name 为 %q，应为 %q", releaseJobID, got, releaseEnvExpr)
	}

	// R3
	var publishing []string
	for _, j := range wf.Jobs {
		for _, s := range j.Steps {
			if !isPublishStep(s) {
				continue
			}
			publishing = append(publishing, s.Name)
			if s.If != publishCondition {
				report("R3", "发布步骤 %q 的 if 为 %q，应为 %q", s.Name, s.If, publishCondition)
			}
			if bareSentryCLI.MatchString(s.Run) {
				report("R3", "发布步骤 %q 按名字调用 sentry-cli，应经 $env:SENTRY_CLI", s.Name)
			}
		}
	}
	slices.Sort(publishing)
	if !slices.Equal(publishing, knownPublishSteps) {
		report("R3", "发布步骤为 %q，应为 %q", publishing, knownPublishSteps)
	}

	// R4
	text := string(data)
	for _, name := range publishSecrets {
		want := "${{ env.PUBLISH == 'true' && secrets." + name + " || '' }}"
		gated := 0
		for _, j := range wf.Jobs {
			for _, s := range j.Steps {
				for key, value := range s.Env {
					v, _ := value.(string)
					if !strings.Contains(v, "secrets."+name) {
						continue
					}
					if v != want {
						report("R4", "步骤 %q 的 env.%s 为 %q，应为 %q", s.Name, key, v, want)
						continue
					}
					gated++
				}
			}
		}
		refs := len(regexp.MustCompile(`secrets\.`+name+`\b`).FindAllStringIndex(text, -1))
		if refs != gated {
			report("R4", "secrets.%s 在文件里出现 %d 次，其中步骤级 env 里的门控写法只有 %d 次", name, refs, gated)
		}
	}

	// R5
	if job.TimeoutMinutes <= 0 || job.TimeoutMinutes > 60 {
		report("R5", "jobs.%s.timeout-minutes 为 %d，应在 1..60 之间", releaseJobID, job.TimeoutMinutes)
	}
	for _, j := range wf.Jobs {
		for _, s := range j.Steps {
			for _, stmt := range psLogicalStatements(s.Run) {
				for _, loc := range psHTTPCommand.FindAllStringIndex(stmt, -1) {
					cmd := stmt[loc[0]:loc[1]]
					if !psTimeoutSec.MatchString(psCallArgs(stmt, loc[0])) {
						report("R5", "步骤 %q 调用 %s 未显式指定正整数 -TimeoutSec", s.Name, cmd)
					}
				}
			}
		}
	}

	// R6
	foundSetCommits := false
	for _, s := range job.Steps {
		if s.Name != sentrySetCommitsStepName {
			continue
		}
		foundSetCommits = true
		if !s.ContinueOnError {
			report("R6", "步骤 %q 须保持 continue-on-error: true", s.Name)
		}
		checkSentrySetCommitsRun(s.Name, s.Run, report)
	}
	if !foundSetCommits {
		report("R6", "作业 %s 缺少步骤 %q", releaseJobID, sentrySetCommitsStepName)
	}
	return found
}

func readReleaseWorkflow(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(releaseWorkflowPath)
	if err != nil {
		t.Fatalf("读不到 %s: %v", releaseWorkflowPath, err)
	}
	return string(data)
}

// TestReleaseWorkflowPublishGuards 核对现行 release.yml 满足 R1–R6。
func TestReleaseWorkflowPublishGuards(t *testing.T) {
	for _, v := range checkReleaseWorkflow([]byte(readReleaseWorkflow(t))) {
		t.Error(v)
	}
}

// TestReleaseWorkflowCatchesMutations 对 release.yml 原文施加变异，每条须被对应规则报出。
// 变异的原文在文件里须恰好出现一次：release.yml 改动后对不上时本用例先报出来，而不是悄悄失效。
func TestReleaseWorkflowCatchesMutations(t *testing.T) {
	text := readReleaseWorkflow(t)
	const (
		guardedGH     = "${{ env.PUBLISH == 'true' && secrets.GH_DOTCOM_TOKEN || '' }}"
		guardedGitee  = "${{ env.PUBLISH == 'true' && secrets.GITEE_TOKEN || '' }}"
		guardedSentry = "${{ env.PUBLISH == 'true' && secrets.SENTRY_AUTH_TOKEN || '' }}"
	)
	for _, m := range []struct {
		name, rule, old, new string
	}{
		{"判据去掉 event_name（手动派发选 tag 时会发布）", "R1",
			"      PUBLISH: " + publishPredicate + "\n",
			"      PUBLISH: ${{ github.ref_type == 'tag' }}\n"},
		{"workflow 级 env 定义 PUBLISH", "R1",
			"\njobs:\n",
			"\nenv:\n  PUBLISH: 'true'\n\njobs:\n"},
		{"步骤级 env 覆盖 PUBLISH", "R1",
			"          SENTRY_DSN: ${{ secrets.SENTRY_DSN }}\n",
			"          SENTRY_DSN: ${{ secrets.SENTRY_DSN }}\n          PUBLISH: 'true'\n"},
		{"脚本经 GITHUB_ENV 改写 PUBLISH", "R1",
			"          Add-Content -Path $env:GITHUB_ENV -Value \"VERSION=${VERSION}\" -Encoding utf8\n",
			"          Add-Content -Path $env:GITHUB_ENV -Value \"VERSION=${VERSION}\" -Encoding utf8\n" +
				"          Add-Content -Path $env:GITHUB_ENV -Value \"PUBLISH=true\" -Encoding utf8\n"},
		{"environment 写死为 build-production", "R2",
			"      name: " + releaseEnvExpr + "\n",
			"      name: build-production\n"},
		{"删掉 Create Release Draft (本仓库) 的 if", "R3",
			"        if: " + publishCondition + "\n        uses: " + publishActionUses,
			"        uses: " + publishActionUses},
		{"Create Release Draft (dotcom mirror) 的 if 改成 always()", "R3",
			"      - name: Create Release Draft (dotcom mirror)\n        if: " + publishCondition + "\n",
			"      - name: Create Release Draft (dotcom mirror)\n        if: always()\n"},
		{"删掉 Finalize Sentry release 的 if", "R3",
			"      - name: Finalize Sentry release\n        if: " + publishCondition + "\n",
			"      - name: Finalize Sentry release\n"},
		{"Associate commits 改回按名字调用 sentry-cli", "R3",
			"          & $env:SENTRY_CLI releases set-commits $env:VERSION --auto --ignore-missing\n",
			"          sentry-cli releases set-commits $env:VERSION --auto --ignore-missing\n"},
		{"不发布的 Compose 一步里调 gh release", "R3",
			"            [IO.File]::WriteAllBytes($path, $notes.ToArray())\n",
			"            [IO.File]::WriteAllBytes($path, $notes.ToArray())\n            gh release view\n"},
		{"删掉 Create Release (Gitee mirror) 的 if", "R3",
			"      - name: Create Release (Gitee mirror)\n        if: " + publishCondition + "\n",
			"      - name: Create Release (Gitee mirror)\n"},
		{"不发布的 Compose 一步里调 Gitee API", "R3",
			"            $path = Join-Path $PWD $target.Path\n",
			"            $path = Join-Path $PWD $target.Path\n" +
				"            Invoke-RestMethod -Uri 'https://gitee.com/api/v5/repos/VTB-LINK/Metabox-Nexus-PlayerCap/releases'\n"},
		{"删掉 Prune old releases (Gitee mirror) 的 if", "R3",
			"      - name: Prune old releases (Gitee mirror)\n        if: " + publishCondition + "\n",
			"      - name: Prune old releases (Gitee mirror)\n"},
		{"GH_TOKEN 挪回作业级 env", "R4",
			"      SENTRY_ORG: kzmtech\n",
			"      GH_TOKEN: ${{ secrets.GH_DOTCOM_TOKEN }}\n      SENTRY_ORG: kzmtech\n"},
		{"Create Sentry release 的凭据去掉门控", "R4",
			"      - name: Create Sentry release\n        if: " + publishCondition + "\n        continue-on-error: true\n        env:\n          SENTRY_AUTH_TOKEN: " + guardedSentry + "\n",
			"      - name: Create Sentry release\n        if: " + publishCondition + "\n        continue-on-error: true\n        env:\n          SENTRY_AUTH_TOKEN: ${{ secrets.SENTRY_AUTH_TOKEN }}\n"},
		{"Create Release (Gitee mirror) 的令牌去掉门控", "R4",
			"      - name: Create Release (Gitee mirror)\n        if: " + publishCondition + "\n        env:\n          GITEE_TOKEN: " + guardedGitee + "\n",
			"      - name: Create Release (Gitee mirror)\n        if: " + publishCondition + "\n        env:\n          GITEE_TOKEN: ${{ secrets.GITEE_TOKEN }}\n"},
		{"Prune old releases (Gitee mirror) 的令牌去掉门控", "R4",
			"      - name: Prune old releases (Gitee mirror)\n        if: " + publishCondition + "\n        continue-on-error: true\n        env:\n          GITEE_TOKEN: " + guardedGitee + "\n",
			"      - name: Prune old releases (Gitee mirror)\n        if: " + publishCondition + "\n        continue-on-error: true\n        env:\n          GITEE_TOKEN: ${{ secrets.GITEE_TOKEN }}\n"},
		{"门控里的判据写反", "R4",
			"      - name: Generate release notes (dotcom mirror)\n        if: " + publishCondition + "\n        env:\n          GH_HOST: github.com\n          GH_TOKEN: " + guardedGH + "\n",
			"      - name: Generate release notes (dotcom mirror)\n        if: " + publishCondition + "\n        env:\n          GH_HOST: github.com\n          GH_TOKEN: ${{ env.PUBLISH != 'false' && secrets.GH_DOTCOM_TOKEN || '' }}\n"},
		{"softprops 的 with 里引用发布凭据", "R4",
			"          generate_release_notes: true\n",
			"          generate_release_notes: true\n          token: ${{ secrets.GH_DOTCOM_TOKEN }}\n"},
		{"删掉 release 作业的 timeout-minutes", "R5",
			"    timeout-minutes: 45\n",
			""},
		{"release 作业的 timeout-minutes 超过上限", "R5",
			"    timeout-minutes: 45\n",
			"    timeout-minutes: 61\n"},
		{"Set up GitHub CLI 的 Invoke-WebRequest 去掉 -TimeoutSec", "R5",
			" -OutFile $zip -TimeoutSec 120\n",
			" -OutFile $zip\n"},
		{"Create Release (Gitee mirror) 上传附件的 Invoke-RestMethod 去掉 -TimeoutSec", "R5",
			" -Headers $auth -TimeoutSec 300 -Form ",
			" -Headers $auth -Form "},
		{"Install sentry-cli 的 Invoke-WebRequest 将 -TimeoutSec 改为 0", "R5",
			" -OutFile $exe -TimeoutSec 120\n",
			" -OutFile $exe -TimeoutSec 0\n"},
		{"Associate commits 去掉 --ignore-missing", "R6",
			"          & $env:SENTRY_CLI releases set-commits $env:VERSION --auto --ignore-missing\n",
			"          & $env:SENTRY_CLI releases set-commits $env:VERSION --auto\n"},
		{"Associate commits 去掉 GITHUB_STEP_SUMMARY 写入", "R6",
			"              ) | Add-Content -Path $env:GITHUB_STEP_SUMMARY -Encoding utf8\n",
			"              ) | Out-Null\n"},
		{"Associate commits 去掉失败分支的非零退出", "R6",
			"            exit $code\n",
			""},
		{"Associate commits 失败分支改为 exit 0", "R6",
			"            exit $code\n",
			"            exit 0\n"},
		{"Associate commits 失败分支改为固定 exit 1", "R6",
			"            exit $code\n",
			"            exit 1\n"},
		{"Associate commits 将 exit 挪到 GITHUB_STEP_SUMMARY 写入之前", "R6",
			"            if ($env:GITHUB_STEP_SUMMARY) {\n" +
				"              @(\n" +
				"                \"### Warning: Sentry commit association failed\"\n" +
				"                \"\"\n" +
				"                \"- Release ``$env:VERSION``: commit association exited with code $code.\"\n" +
				"                \"- Release publishing was not blocked, but Suspect Commits will be unavailable in Sentry.\"\n" +
				"              ) | Add-Content -Path $env:GITHUB_STEP_SUMMARY -Encoding utf8\n" +
				"            }\n" +
				"            exit $code\n",
			"            exit $code\n" +
				"            if ($env:GITHUB_STEP_SUMMARY) {\n" +
				"              @(\n" +
				"                \"### Warning: Sentry commit association failed\"\n" +
				"                \"\"\n" +
				"                \"- Release ``$env:VERSION``: commit association exited with code $code.\"\n" +
				"                \"- Release publishing was not blocked, but Suspect Commits will be unavailable in Sentry.\"\n" +
				"              ) | Add-Content -Path $env:GITHUB_STEP_SUMMARY -Encoding utf8\n" +
				"            }\n"},
		{"Associate commits 去掉 continue-on-error: true", "R6",
			"      - name: Associate commits to Sentry release\n        if: " + publishCondition + "\n        continue-on-error: true\n",
			"      - name: Associate commits to Sentry release\n        if: " + publishCondition + "\n"},
	} {
		t.Run(m.name, func(t *testing.T) {
			if n := strings.Count(text, m.old); n != 1 {
				t.Fatalf("变异的原文在 %s 里出现 %d 次，应恰好 1 次：%q", releaseWorkflowPath, n, m.old)
			}
			found := checkReleaseWorkflow([]byte(strings.Replace(text, m.old, m.new, 1)))
			if !slices.ContainsFunc(found, func(v string) bool { return strings.HasPrefix(v, m.rule+": ") }) {
				t.Errorf("变异未被 %s 报出；报出的是 %q", m.rule, found)
			}
		})
	}
}

// TestReleaseWorkflowHTTPTimeoutEdgeCases 核对 R5 对 PowerShell 常见写法的判定边界。
func TestReleaseWorkflowHTTPTimeoutEdgeCases(t *testing.T) {
	hasR5 := func(run string) bool {
		// 仅检查脚本的 R5 规则（其余规则在此合成用例中无关）。
		for _, stmt := range psLogicalStatements(run) {
			for _, loc := range psHTTPCommand.FindAllStringIndex(stmt, -1) {
				if !psTimeoutSec.MatchString(psCallArgs(stmt, loc[0])) {
					return true
				}
			}
		}
		return false
	}

	for _, tc := range []struct {
		name    string
		run     string
		wantErr bool
	}{
		{"整行注释含 Invoke-RestMethod 忽略", "# Invoke-RestMethod -Uri https://example.com\n", false},
		{"行尾注释伪造 -TimeoutSec 须拦截", "Invoke-RestMethod -Uri $u # -TimeoutSec 60\n", true},
		{"反引号续行带 -TimeoutSec 通过", "Invoke-RestMethod `\n  -Uri $u `\n  -TimeoutSec 60\n", false},
		{"多行哈希表后跟 -TimeoutSec 通过", "$r = Invoke-RestMethod -Uri $u -Body @{\n  a = 1\n  b = 2\n} -TimeoutSec 60\n", false},
		{"多行哈希表内伪造 TimeoutSec 须拦截", "$r = Invoke-RestMethod -Uri $u -Body @{\n  TimeoutSec = 60\n}\n", true},
		{"管道后接 Out-Null 通过", "Invoke-RestMethod -Method Delete -Uri $u -TimeoutSec 60 | Out-Null\n", false},
		{"大小写不敏感与冒号写法通过", "invoke-webrequest -Uri $u -timeoutsec:120\n", false},
		{"无前导空格赋值缺 -TimeoutSec 须拦截", "$a =Invoke-RestMethod -Uri $u\n", true},
		{"TimeoutSec 为 0 须拦截", "Invoke-WebRequest -Uri $u -TimeoutSec 0\n", true},
		{"TimeoutSec 为负数须拦截", "Invoke-WebRequest -Uri $u -TimeoutSec -5\n", true},
		{"普通控制流块不跨行合并", "try {\n  Invoke-RestMethod -Uri $a -TimeoutSec 60\n  Invoke-RestMethod -Uri $b\n}\n", true},
		{"嵌套子表达式外层缺 -TimeoutSec 须拦截", "Invoke-RestMethod -Uri $a -Body $(Invoke-RestMethod -Uri $b -TimeoutSec 60)\n", true},
		{"嵌套子表达式内层缺 -TimeoutSec 须拦截", "Invoke-RestMethod -Uri $a -Body $(Invoke-RestMethod -Uri $b) -TimeoutSec 60\n", true},
		{"嵌套子表达式内外均有 -TimeoutSec 通过", "Invoke-RestMethod -Uri $a -Body $(Invoke-RestMethod -Uri $b -TimeoutSec 60) -TimeoutSec 60\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasR5(tc.run); got != tc.wantErr {
				t.Errorf("hasR5(%q) = %v，应为 %v", tc.run, got, tc.wantErr)
			}
		})
	}
}

// TestReleaseWorkflowSentrySetCommitsEdgeCases 核对 R6 对 PowerShell 常见与伪造写法的判定边界。
func TestReleaseWorkflowSentrySetCommitsEdgeCases(t *testing.T) {
	checkR6 := func(run string) []string {
		var found []string
		checkSentrySetCommitsRun(sentrySetCommitsStepName, run, func(rule, format string, args ...any) {
			found = append(found, rule+": "+fmt.Sprintf(format, args...))
		})
		return found
	}

	const validScript = `
if (-not $env:SENTRY_CLI) {
  exit 1
}
& $env:SENTRY_CLI releases set-commits $env:VERSION --auto --ignore-missing
if ($LASTEXITCODE -ne 0) {
  $code = $LASTEXITCODE
  Write-Output "::warning::failed (exit $code)"
  if ($env:GITHUB_STEP_SUMMARY) {
    @(
      "### Warning: Sentry commit association failed"
      ""
      "- Release ` + "`" + `` + "`" + `$env:VERSION` + "`" + `` + "`" + `: exit $code."
    ) | Add-Content -Path $env:GITHUB_STEP_SUMMARY -Encoding utf8
  }
  exit $code
}
`

	for _, tc := range []struct {
		name    string
		run     string
		wantErr bool
	}{
		{"标准写法通过", validScript, false},
		{"反引号续行与单行 if 写摘要并退出通过", "& $env:SENTRY_CLI releases set-commits $env:VERSION `\n  --auto `\n  --ignore-missing\nif ($LASTEXITCODE) { Add-Content -Path $env:GITHUB_STEP_SUMMARY -Value 'warn'; exit $LASTEXITCODE }\n", false},
		{"Out-File 带 -Append 与双引号路径通过", "& $env:SENTRY_CLI releases set-commits $env:VERSION --auto --ignore-missing\nif ($LASTEXITCODE -ne 0) { 'warn' | Out-File -FilePath \"$env:GITHUB_STEP_SUMMARY\" -Append; exit $LASTEXITCODE }\n", false},
		{"追加重定向 >> 写摘要通过", "& $env:SENTRY_CLI releases set-commits $env:VERSION --auto --ignore-missing\nif ($LASTEXITCODE -ne 0) { 'warn' >> $env:GITHUB_STEP_SUMMARY; exit $LASTEXITCODE }\n", false},
		{"整行注释伪造 --ignore-missing 须拦截", "& $env:SENTRY_CLI releases set-commits $env:VERSION --auto\n# --ignore-missing\nif ($LASTEXITCODE -ne 0) { Add-Content -Path $env:GITHUB_STEP_SUMMARY -Value 'warn'; exit $LASTEXITCODE }\n", true},
		{"行尾注释伪造 --ignore-missing 须拦截", "& $env:SENTRY_CLI releases set-commits $env:VERSION --auto # --ignore-missing\nif ($LASTEXITCODE -ne 0) { Add-Content -Path $env:GITHUB_STEP_SUMMARY -Value 'warn'; exit $LASTEXITCODE }\n", true},
		{"同行后一条命令参数伪造 --ignore-missing 须拦截", "& $env:SENTRY_CLI releases set-commits $env:VERSION --auto; Write-Output --ignore-missing\nif ($LASTEXITCODE -ne 0) { Add-Content -Path $env:GITHUB_STEP_SUMMARY -Value 'warn'; exit $LASTEXITCODE }\n", true},
		{"粘连选项 --ignore-missing-extra 须拦截", "& $env:SENTRY_CLI releases set-commits $env:VERSION --auto --ignore-missing-extra\nif ($LASTEXITCODE -ne 0) { Add-Content -Path $env:GITHUB_STEP_SUMMARY -Value 'warn'; exit $LASTEXITCODE }\n", true},
		{"缺少 --auto 只带 --ignore-missing 须拦截", "& $env:SENTRY_CLI releases set-commits $env:VERSION --ignore-missing\nif ($LASTEXITCODE -ne 0) { Add-Content -Path $env:GITHUB_STEP_SUMMARY -Value 'warn'; exit $LASTEXITCODE }\n", true},
		{"整行注释伪造 GITHUB_STEP_SUMMARY 写入须拦截", "& $env:SENTRY_CLI releases set-commits $env:VERSION --auto --ignore-missing\nif ($LASTEXITCODE -ne 0) {\n  # Add-Content -Path $env:GITHUB_STEP_SUMMARY -Value 'warn'\n  exit $LASTEXITCODE\n}\n", true},
		{"行尾注释伪造 GITHUB_STEP_SUMMARY 写入须拦截", "& $env:SENTRY_CLI releases set-commits $env:VERSION --auto --ignore-missing\nif ($LASTEXITCODE -ne 0) {\n  Write-Output 'warn' # | Add-Content -Path $env:GITHUB_STEP_SUMMARY\n  exit $LASTEXITCODE\n}\n", true},
		{"仅判 if ($env:GITHUB_STEP_SUMMARY) 未写入须拦截", "& $env:SENTRY_CLI releases set-commits $env:VERSION --auto --ignore-missing\nif ($LASTEXITCODE -ne 0) {\n  if ($env:GITHUB_STEP_SUMMARY) { Write-Output 'warn' }\n  exit $LASTEXITCODE\n}\n", true},
		{"字符串字面量伪造 Add-Content 须拦截", "& $env:SENTRY_CLI releases set-commits $env:VERSION --auto --ignore-missing\nif ($LASTEXITCODE -ne 0) {\n  Write-Output \"Add-Content -Path $env:GITHUB_STEP_SUMMARY\"\n  exit $LASTEXITCODE\n}\n", true},
		{"单引号路径不展开 GITHUB_STEP_SUMMARY 须拦截", "& $env:SENTRY_CLI releases set-commits $env:VERSION --auto --ignore-missing\nif ($LASTEXITCODE -ne 0) {\n  Add-Content -Path '$env:GITHUB_STEP_SUMMARY' -Value 'warn'\n  exit $LASTEXITCODE\n}\n", true},
		{"Out-File 缺少 -Append 须拦截", "& $env:SENTRY_CLI releases set-commits $env:VERSION --auto --ignore-missing\nif ($LASTEXITCODE -ne 0) {\n  'warn' | Out-File -FilePath $env:GITHUB_STEP_SUMMARY\n  exit $LASTEXITCODE\n}\n", true},
		{"失败分支写成 exit 0 须拦截", "& $env:SENTRY_CLI releases set-commits $env:VERSION --auto --ignore-missing\nif ($LASTEXITCODE -ne 0) {\n  Add-Content -Path $env:GITHUB_STEP_SUMMARY -Value 'warn'\n  exit 0\n}\n", true},
		{"失败分支省略 exit 且文案含 (exit $code) 须拦截", "& $env:SENTRY_CLI releases set-commits $env:VERSION --auto --ignore-missing\nif ($LASTEXITCODE -ne 0) {\n  $code = $LASTEXITCODE\n  Write-Output \"::warning::failed (exit $code)\"\n  Add-Content -Path $env:GITHUB_STEP_SUMMARY -Value 'warn'\n}\n", true},
		{"失败分支裸 exit 无退出码须拦截", "& $env:SENTRY_CLI releases set-commits $env:VERSION --auto --ignore-missing\nif ($LASTEXITCODE -ne 0) {\n  Add-Content -Path $env:GITHUB_STEP_SUMMARY -Value 'warn'\n  exit\n}\n", true},
		{"失败分支将退出变量赋 0 后 exit 须拦截", "& $env:SENTRY_CLI releases set-commits $env:VERSION --auto --ignore-missing\nif ($LASTEXITCODE -ne 0) {\n  $code = 0\n  Add-Content -Path $env:GITHUB_STEP_SUMMARY -Value 'warn'\n  exit $code\n}\n", true},
		{"exit 误放在内层 if ($env:GITHUB_STEP_SUMMARY) 块内须拦截", "& $env:SENTRY_CLI releases set-commits $env:VERSION --auto --ignore-missing\nif ($LASTEXITCODE -ne 0) {\n  $code = $LASTEXITCODE\n  if ($env:GITHUB_STEP_SUMMARY) {\n    Add-Content -Path $env:GITHUB_STEP_SUMMARY -Value 'warn'\n    exit $code\n  }\n}\n", true},
		{"反向条件 if ($LASTEXITCODE -eq 0) 须拦截", "& $env:SENTRY_CLI releases set-commits $env:VERSION --auto --ignore-missing\nif ($LASTEXITCODE -eq 0) {\n  Add-Content -Path $env:GITHUB_STEP_SUMMARY -Value 'warn'\n  exit $LASTEXITCODE\n}\n", true},
		{"LASTEXITCODE 检查位于 set-commits 之前须拦截", "if ($LASTEXITCODE -ne 0) {\n  Add-Content -Path $env:GITHUB_STEP_SUMMARY -Value 'warn'\n  exit $LASTEXITCODE\n}\n& $env:SENTRY_CLI releases set-commits $env:VERSION --auto --ignore-missing\n", true},
		{"摘要写入与 exit 拆到两个不同 if 分支须拦截", "& $env:SENTRY_CLI releases set-commits $env:VERSION --auto --ignore-missing\nif ($LASTEXITCODE -ne 0) { Add-Content -Path $env:GITHUB_STEP_SUMMARY -Value 'warn' }\nif ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }\n", true},
		{"行内块注释伪造 --ignore-missing 须拦截", "& $env:SENTRY_CLI releases set-commits $env:VERSION --auto <# --ignore-missing #>\nif ($LASTEXITCODE -ne 0) { Add-Content -Path $env:GITHUB_STEP_SUMMARY -Value 'warn'; exit $LASTEXITCODE }\n", true},
		{"跨行块注释伪造 set-commits 命令与选项须拦截", "<#\n& $env:SENTRY_CLI releases set-commits $env:VERSION --auto --ignore-missing\n#>\nif ($LASTEXITCODE -ne 0) { Add-Content -Path $env:GITHUB_STEP_SUMMARY -Value 'warn'; exit $LASTEXITCODE }\n", true},
		{"跨行块注释伪造 GITHUB_STEP_SUMMARY 写入与 exit 须拦截", "& $env:SENTRY_CLI releases set-commits $env:VERSION --auto --ignore-missing\nif ($LASTEXITCODE -ne 0) {\n  <#\n  Add-Content -Path $env:GITHUB_STEP_SUMMARY -Value 'warn'\n  exit $LASTEXITCODE\n  #>\n}\n", true},
		{"引号内字面 <# ... #> 不误作块注释通过", "& $env:SENTRY_CLI releases set-commits $env:VERSION --auto --ignore-missing\nif ($LASTEXITCODE -ne 0) {\n  Write-Output \"note <# not comment #>\"\n  Add-Content -Path $env:GITHUB_STEP_SUMMARY -Value 'literal <# marker'\n  exit $LASTEXITCODE\n}\n", false},
		{"单行 # 注释含 <# 不吞后续脚本通过", "# comment mentioning <# block syntax\n& $env:SENTRY_CLI releases set-commits $env:VERSION --auto --ignore-missing\nif ($LASTEXITCODE -ne 0) { Add-Content -Path $env:GITHUB_STEP_SUMMARY -Value 'warn'; exit $LASTEXITCODE }\n", false},
		{"Add-Content -Path 指向别处且仅 -Value 含摘要变量须拦截", "& $env:SENTRY_CLI releases set-commits $env:VERSION --auto --ignore-missing\nif ($LASTEXITCODE -ne 0) {\n  Add-Content -Path other.md -Value $env:GITHUB_STEP_SUMMARY\n  exit $LASTEXITCODE\n}\n", true},
		{"Add-Content 第 1 位置参数为别处且第 2 参数为摘要变量须拦截", "& $env:SENTRY_CLI releases set-commits $env:VERSION --auto --ignore-missing\nif ($LASTEXITCODE -ne 0) {\n  Add-Content other.md $env:GITHUB_STEP_SUMMARY\n  exit $LASTEXITCODE\n}\n", true},
		{"Out-File -FilePath 指向别处且仅 -InputObject 含摘要变量须拦截", "& $env:SENTRY_CLI releases set-commits $env:VERSION --auto --ignore-missing\nif ($LASTEXITCODE -ne 0) {\n  'warn' | Out-File -FilePath other.md -Append -InputObject $env:GITHUB_STEP_SUMMARY\n  exit $LASTEXITCODE\n}\n", true},
		{"Out-File -Append:$false 须拦截", "& $env:SENTRY_CLI releases set-commits $env:VERSION --auto --ignore-missing\nif ($LASTEXITCODE -ne 0) {\n  'warn' | Out-File -Append:$false $env:GITHUB_STEP_SUMMARY\n  exit $LASTEXITCODE\n}\n", true},
		{"Add-Content 位置路径绑定摘要变量通过", "& $env:SENTRY_CLI releases set-commits $env:VERSION --auto --ignore-missing\nif ($LASTEXITCODE -ne 0) {\n  Add-Content $env:GITHUB_STEP_SUMMARY 'warn'\n  exit $LASTEXITCODE\n}\n", false},
		{"Add-Content 跳过命名参数与开关后位置路径绑定摘要变量通过", "& $env:SENTRY_CLI releases set-commits $env:VERSION --auto --ignore-missing\nif ($LASTEXITCODE -ne 0) {\n  Add-Content -Encoding utf8 -Force -NoNewline \"$env:GITHUB_STEP_SUMMARY\" 'warn'\n  exit $LASTEXITCODE\n}\n", false},
		{"Out-File 跳过 -Append 后位置路径绑定摘要变量通过", "& $env:SENTRY_CLI releases set-commits $env:VERSION --auto --ignore-missing\nif ($LASTEXITCODE -ne 0) {\n  'warn' | Out-File -Append $env:GITHUB_STEP_SUMMARY\n  exit $LASTEXITCODE\n}\n", false},
		{"Add-Content 冒号写法 -LiteralPath 绑定摘要变量通过", "& $env:SENTRY_CLI releases set-commits $env:VERSION --auto --ignore-missing\nif ($LASTEXITCODE -ne 0) {\n  'warn' | Add-Content -LiteralPath:$env:GITHUB_STEP_SUMMARY -Encoding:utf8\n  exit $LASTEXITCODE\n}\n", false},
		{"exit $code 位于摘要写入之前须拦截", "& $env:SENTRY_CLI releases set-commits $env:VERSION --auto --ignore-missing\nif ($LASTEXITCODE -ne 0) {\n  $code = $LASTEXITCODE\n  exit $code\n  Add-Content -Path $env:GITHUB_STEP_SUMMARY -Value 'warn'\n}\n", true},
		{"固定非零退出码 exit 1 须拦截", "& $env:SENTRY_CLI releases set-commits $env:VERSION --auto --ignore-missing\nif ($LASTEXITCODE -ne 0) {\n  Add-Content -Path $env:GITHUB_STEP_SUMMARY -Value 'warn'\n  exit 1\n}\n", true},
		{"固定非零变量赋值 $code = 1 后 exit 须拦截", "& $env:SENTRY_CLI releases set-commits $env:VERSION --auto --ignore-missing\nif ($LASTEXITCODE -ne 0) {\n  $code = 1\n  Add-Content -Path $env:GITHUB_STEP_SUMMARY -Value 'warn'\n  exit $code\n}\n", true},
		{"首次顶层 exit 1 后跟摘要写入与 exit $code 须拦截", "& $env:SENTRY_CLI releases set-commits $env:VERSION --auto --ignore-missing\nif ($LASTEXITCODE -ne 0) {\n  $code = $LASTEXITCODE\n  exit 1\n  Add-Content -Path $env:GITHUB_STEP_SUMMARY -Value 'warn'\n  exit $code\n}\n", true},
		{"&& 条件执行 exit $code 后跟 exit 0 须拦截", "& $env:SENTRY_CLI releases set-commits $env:VERSION --auto --ignore-missing\nif ($LASTEXITCODE -ne 0) {\n  $code = $LASTEXITCODE\n  Add-Content -Path $env:GITHUB_STEP_SUMMARY -Value 'warn'\n  Get-Item -LiteralPath missing.file -ErrorAction SilentlyContinue && exit $code\n  exit 0\n}\n", true},
		{"&& 条件执行 exit 0 后跟 exit $code 须拦截", "& $env:SENTRY_CLI releases set-commits $env:VERSION --auto --ignore-missing\nif ($LASTEXITCODE -ne 0) {\n  $code = $LASTEXITCODE\n  Add-Content -Path $env:GITHUB_STEP_SUMMARY -Value 'warn'\n  Test-Path . && exit 0\n  exit $code\n}\n", true},
		{"|| 条件执行 exit $code 须拦截", "& $env:SENTRY_CLI releases set-commits $env:VERSION --auto --ignore-missing\nif ($LASTEXITCODE -ne 0) {\n  $code = $LASTEXITCODE\n  Add-Content -Path $env:GITHUB_STEP_SUMMARY -Value 'warn'\n  Test-Path . || exit $code\n}\n", true},
		{"&& 条件链覆写退出变量 $code = 0 后 exit $code 须拦截", "& $env:SENTRY_CLI releases set-commits $env:VERSION --auto --ignore-missing\nif ($LASTEXITCODE -ne 0) {\n  $code = $LASTEXITCODE\n  Add-Content -Path $env:GITHUB_STEP_SUMMARY -Value 'warn'\n  Test-Path . && $code = 0\n  exit $code\n}\n", true},
		{"无关 && 条件链后无条件 exit $code 通过", "& $env:SENTRY_CLI releases set-commits $env:VERSION --auto --ignore-missing\nif ($LASTEXITCODE -ne 0) {\n  $code = $LASTEXITCODE\n  Add-Content -Path $env:GITHUB_STEP_SUMMARY -Value 'warn'\n  Test-Path . && Write-Output 'warn'\n  exit $code\n}\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			found := checkR6(tc.run)
			if gotErr := len(found) > 0; gotErr != tc.wantErr {
				t.Errorf("checkR6(%q) = %q (err=%v)，应为 err=%v", tc.run, found, gotErr, tc.wantErr)
			}
		})
	}
}
