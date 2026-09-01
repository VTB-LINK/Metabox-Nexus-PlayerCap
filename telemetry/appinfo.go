package telemetry

// PlayerCap 自身的信息：版本、装在哪、配置成什么样。
//
// # 为什么 config 值得整份带上
//
// 这个服务运行在一组脆弱假设上（内存扫描 / CDP / 二进制补丁），而 config 决定了其中好几个：
// poll 间隔决定采集频率、effect-strategy 决定 park 还是 fadeout、prior-player 决定路由行为。
// 「主播改了什么」往往就是「为什么只有他崩」的答案。
//
// ExplicitKeys 因此比 config 本身更值钱：它区分「这个值是默认的」和「主播特意改成了这个」。

import (
	"os"
	"sort"

	"github.com/getsentry/sentry-go"

	"Metabox-Nexus-PlayerCap/config"
	"Metabox-Nexus-PlayerCap/i18n"
)

// appInfo 是 PlayerCap 自身的一份快照。
type appInfo struct {
	Version     string
	ExePath     string
	Config      map[string]any
	Overwritten []string // 被主播显式改过的键（已排序）
	Sources     []string // 配置来源（内置默认 / config.yml / 命令行…）
	Strategy    string   // config 里写的 effect-strategy，**不是实际生效的**，见 applyAppInfo
}

// collectAppInfo 采一次自身信息。
//
// exe 路径**原样上报，不脱敏**：它含 Windows 用户名，但那正是有用的部分 —— 装在哪个盘、
// 是不是 OneDrive 的同步目录、路径里有没有中文，都是真实排查过的方向。
func collectAppInfo(version string, cfg config.Config, playerNames []string) appInfo {
	a := appInfo{
		Version:  version,
		Sources:  cfg.Sources,
		Strategy: cfg.EffectStrategy,
	}

	if p, err := os.Executable(); err == nil {
		a.ExePath = p
	}

	players := make(map[string]any, len(playerNames))
	for _, name := range playerNames {
		// 用 resolved 值而不是 cfg.Players[name] —— 后者只有被显式配过的播放器才有条目，
		// 而我们要的是每个播放器**实际在用**的值（未配的会回落到全局 offset/poll）。
		players[name] = map[string]any{
			"offset": cfg.GetPlayerOffset(name),
			"poll":   cfg.GetPlayerPoll(name),
		}
	}

	a.Config = map[string]any{
		"addr":                cfg.Addr,
		"offset":              cfg.Offset,
		"poll":                cfg.Poll,
		"prior_player":        cfg.PriorPlayer,
		"prior_player_expire": cfg.PriorPlayerExpire,
		"effect_strategy":     cfg.EffectStrategy,
		"players":             players,
	}

	for k, explicit := range cfg.ExplicitKeys {
		if explicit {
			a.Overwritten = append(a.Overwritten, k)
		}
	}
	// **必须排序**：Go 的 map 迭代序是随机的，不排的话同一台机器每次启动都会得到不同顺序，
	// 在 Sentry 里看着像「配置变了」。本仓库在 config 与 router 里已经各踩过一次这个坑。
	sort.Strings(a.Overwritten)

	return a
}

// applyAppInfo 把自身信息写进 scope。
func applyAppInfo(scope *sentry.Scope, a appInfo) {
	scope.SetContext("playercap", map[string]any{
		"version":     a.Version,
		"exe_path":    a.ExePath,
		"config":      a.Config,
		"overwritten": a.Overwritten,
		"sources":     a.Sources,
	})

	// effect-strategy 是个已知的故障维度（park 的屏外保活在 Win11 上失效，会黑屏），
	// 值得一个能分面的 tag。
	//
	// **注意这是 config 里写的值，不是实际生效的值**：main.go 在 Win11 上会把 park 强制降级成
	// fadeout，而那步发生在 telemetry.Init 之后（Init 必须早于 checkAndUpdate，见 §1.2）。
	// 想知道实际生效的，用这个 tag 配合 os.build 判断 —— 或者等哪天把降级结果也报上来。
	setTagIfNotEmpty(scope, "config.effect_strategy", a.Strategy)

	// 「主播动过配置没有」是个好的第一刀：绝大多数机器是默认配置，出问题的往往不是。
	scope.SetTag("config.customized", boolTag(len(a.Overwritten) > 0))
}

// boolTag 把 bool 转成 tag 用的字符串。
func boolTag(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// logAppInfo 打一行自身信息。exe 路径对主播自己也有用（他可能不知道自己开的是哪个副本）。
func logAppInfo(a appInfo) {
	log.Info("程序: v%s @ %s", a.Version, nonEmpty(a.ExePath, i18n.T("路径未知")))
	if len(a.Overwritten) > 0 {
		log.Info("配置: 已改动 %d 项 %v（来源 %v）", len(a.Overwritten), a.Overwritten, a.Sources)
	}
}
