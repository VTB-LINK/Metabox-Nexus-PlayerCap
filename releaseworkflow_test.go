package main

// 本文件只守一件事：发版流水线 release.yml 在演练时不发布（#164，设计见
// doc/design/ci-windows-release-design.md §3.2）。
//
// 发布与否只看发版作业 env 里的 PUBLISH：tag 推送为真，分支推送与手动派发为假。演练若误建了
// draft，dotcom 那份一旦被人发布就是一次真实发版；Sentry 里也会多出非 semver 的 release。
// workflow 不参与编译，改坏这些写法时四道门禁照样全绿，故在这里按 YAML 结构与文本核对：
//
//	R1 判据只在发版作业的 env 里定义一次，写法同设计 §3.2.1；workflow 级 env、其他作业、
//	   步骤级 env 与脚本（经 GITHUB_ENV）都不得再定义 PUBLISH。
//	R2 environment 的表达式与判据一致：发布取 build-production，演练取 build-dev。
//	R3 发布步骤（softprops 建 draft；脚本里调 gh release、generate-notes、sentry-cli 的 releases 与
//	   deploys）的 if 恰为 env.PUBLISH == 'true'，且恰为已知的六步；sentry-cli 只经 $env:SENTRY_CLI
//	   （Install sentry-cli 校验通过才写入的绝对路径）调用，不按名字取 PATH 里的（代码审第 1 轮）。
//	R4 两个发布凭据只出现在步骤级 env，每处都写成只在判据为真时取值的形式。
//
// 只按字面核对。用别的方式发布（如直接调 API）、用 GITHUB_TOKEN 发布，这里查不到（设计 §3.2.6）。
// R3 按子串匹配脚本，注释也算：不发布的步骤里不要写这几个命令。
//
// 变异自证：TestReleaseWorkflowCatchesMutations 对 release.yml 原文逐条施加变异，每条须被对应规则
// 报出。另在工作区直接改 release.yml 实测过（TestReleaseWorkflowPublishGuards 均红，改回即绿）：
// 删掉 Create Release Draft (本仓库) 的 if（R3）、判据去掉 event_name（R1）、Create Sentry release
// 的凭据去掉门控（R4）、environment 写死为 build-production（R2）。

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
	releaseJobID      = "release"
	publishPredicate  = "${{ github.event_name == 'push' && github.ref_type == 'tag' }}"
	releaseEnvExpr    = "${{ github.event_name == 'push' && github.ref_type == 'tag' && 'build-production' || 'build-dev' }}"
	publishCondition  = "env.PUBLISH == 'true'"
	publishActionUses = "softprops/action-gh-release"
)

// publishSecrets 是只在发布时才可引用的凭据。
var publishSecrets = []string{"GH_DOTCOM_TOKEN", "SENTRY_AUTH_TOKEN"}

// publishCommands 出现在某步脚本里，该步即为发布步骤。按名字调用 sentry-cli 的两种写法也算，R3 另报它们。
var publishCommands = []string{"gh release", "generate-notes", "SENTRY_CLI releases", "SENTRY_CLI deploys",
	"sentry-cli releases", "sentry-cli deploys"}

// bareSentryCLI 是按名字调用 sentry-cli：取到的可能是 PATH 里未经校验的另一份。
var bareSentryCLI = regexp.MustCompile(`(?m)^\s*sentry-cli\s`)

// knownPublishSteps 是 release.yml 的全部发布步骤。新增或改名发布步骤要同步这里，扫描认不出某一步时也在这里暴露。
var knownPublishSteps = []string{
	"Associate commits to Sentry release",
	"Create Release Draft (dotcom mirror)",
	"Create Release Draft (本仓库)",
	"Create Sentry release",
	"Finalize Sentry release",
	"Generate release notes (dotcom mirror)",
}

type workflowFile struct {
	Env  map[string]any         `yaml:"env"`
	Jobs map[string]workflowJob `yaml:"jobs"`
}

type workflowJob struct {
	Env         map[string]any `yaml:"env"`
	Environment any            `yaml:"environment"`
	Steps       []workflowStep `yaml:"steps"`
}

type workflowStep struct {
	Name string         `yaml:"name"`
	If   string         `yaml:"if"`
	Uses string         `yaml:"uses"`
	Run  string         `yaml:"run"`
	Env  map[string]any `yaml:"env"`
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

// checkReleaseWorkflow 返回 data（release.yml 的内容）违反 R1–R4 的各处，每条以规则名开头。
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

// TestReleaseWorkflowPublishGuards 核对现行 release.yml 满足 R1–R4。
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
			"          & $env:SENTRY_CLI releases set-commits $env:VERSION --auto\n",
			"          sentry-cli releases set-commits $env:VERSION --auto\n"},
		{"不发布的 Compose 一步里调 gh release", "R3",
			"          [IO.File]::WriteAllBytes($path, $notes.ToArray())\n",
			"          [IO.File]::WriteAllBytes($path, $notes.ToArray())\n          gh release view\n"},
		{"GH_TOKEN 挪回作业级 env", "R4",
			"      SENTRY_ORG: kzmtech\n",
			"      GH_TOKEN: ${{ secrets.GH_DOTCOM_TOKEN }}\n      SENTRY_ORG: kzmtech\n"},
		{"Create Sentry release 的凭据去掉门控", "R4",
			"      - name: Create Sentry release\n        if: " + publishCondition + "\n        continue-on-error: true\n        env:\n          SENTRY_AUTH_TOKEN: " + guardedSentry + "\n",
			"      - name: Create Sentry release\n        if: " + publishCondition + "\n        continue-on-error: true\n        env:\n          SENTRY_AUTH_TOKEN: ${{ secrets.SENTRY_AUTH_TOKEN }}\n"},
		{"门控里的判据写反", "R4",
			"      - name: Generate release notes (dotcom mirror)\n        if: " + publishCondition + "\n        env:\n          GH_HOST: github.com\n          GH_TOKEN: " + guardedGH + "\n",
			"      - name: Generate release notes (dotcom mirror)\n        if: " + publishCondition + "\n        env:\n          GH_HOST: github.com\n          GH_TOKEN: ${{ env.PUBLISH != 'false' && secrets.GH_DOTCOM_TOKEN || '' }}\n"},
		{"softprops 的 with 里引用发布凭据", "R4",
			"          generate_release_notes: true\n",
			"          generate_release_notes: true\n          token: ${{ secrets.GH_DOTCOM_TOKEN }}\n"},
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
