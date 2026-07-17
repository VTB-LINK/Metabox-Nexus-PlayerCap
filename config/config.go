package config

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"Metabox-Nexus-PlayerCap/logger"

	"gopkg.in/yaml.v3"
)

var log = logger.New("Config")

// PlayerConfig 单播放器配置覆盖（nil 表示沿用全局值）
type PlayerConfig struct {
	Offset *int
	Poll   *int
}

// 播放器自动注册（播放器包在 init() 中调用 RegisterPlayer）
var (
	registeredPlayers []string
	regMu             sync.Mutex
)

// RegisterPlayer 注册播放器名称，自动生成对应的 CLI flag 和 YAML 字段支持。
// 应在播放器包的 init() 中调用。
func RegisterPlayer(name string) {
	regMu.Lock()
	defer regMu.Unlock()
	registeredPlayers = append(registeredPlayers, name)
}

// RegisteredPlayers 返回已注册的播放器名列表
func RegisteredPlayers() []string {
	regMu.Lock()
	defer regMu.Unlock()
	out := make([]string, len(registeredPlayers))
	copy(out, registeredPlayers)
	return out
}

// Config 应用配置
type Config struct {
	Addr              string                   `yaml:"addr"`                // WebSocket 监听地址
	Offset            int                      `yaml:"offset"`              // 全局时间偏移（毫秒）
	Poll              int                      `yaml:"poll"`                // 全局轮询间隔（毫秒）
	PriorPlayer       []string                 `yaml:"prior-player"`        // 优先播放器列表
	PriorPlayerExpire int                      `yaml:"prior-player-expire"` // 优先播放器暂停超时（秒）
	EffectStrategy    string                   `yaml:"-"`                   // 网易云特效最小化策略：park | fadeout（键 cloudmusicv3-effect-strategy）
	Players           map[string]*PlayerConfig `yaml:"-"`                   // 各播放器专属配置
	Sources           []string                 `yaml:"-"`                   // 配置来源列表（内部字段）
	ExplicitKeys      map[string]bool          `yaml:"-"`                   // 被显式设置（非默认）的字段集合
}

// DefaultConfig 返回内置默认配置
func DefaultConfig() Config {
	return Config{
		Addr:              "0.0.0.0:8765",
		Offset:            200,
		Poll:              30,
		PriorPlayer:       []string{"wesing"},
		PriorPlayerExpire: 15,
		EffectStrategy:    "fadeout",
		Players:           make(map[string]*PlayerConfig),
		ExplicitKeys:      make(map[string]bool),
	}
}

// GetPlayerOffset 获取播放器偏移（未设置则用全局）
func (c *Config) GetPlayerOffset(playerName string) int {
	if pc, ok := c.Players[playerName]; ok && pc.Offset != nil {
		return *pc.Offset
	}
	return c.Offset
}

// GetPlayerPoll 获取播放器轮询间隔（未设置则用全局）
func (c *Config) GetPlayerPoll(playerName string) int {
	if pc, ok := c.Players[playerName]; ok && pc.Poll != nil {
		return *pc.Poll
	}
	return c.Poll
}

// IsPriorPlayer 检查播放器是否为优先播放器
func (c *Config) IsPriorPlayer(playerName string) bool {
	for _, p := range c.PriorPlayer {
		if p == playerName {
			return true
		}
	}
	return false
}

const configName = "config.yml"

// 可注入，便于测 configPath 的选址逻辑（真实的 exe 路径与 CWD 都是进程级全局状态，
// 测试里没法安全地改）。生产路径永远是这两个 stdlib 函数。
var (
	osExecutable = os.Executable
	osGetwd      = os.Getwd
)

// configPath 定位 config.yml：**exe 所在目录优先**，而不是当前工作目录。
//
// 为什么不能用裸 "config.yml"（= 按 CWD 解析）：README「自动加载同目录下的 config.yml」，
// 且 CI 打包就是把 config.yml 和 exe 放进同一目录（release.yml / build-windows.yml 的
// `go run ./tools/genconfig "${PACKAGE_DIR}/config.yml"`）—— 「同目录」是设计意图，
// 按 CWD 解析是代码单方面偏离。双击运行时两者相同（主流路径无差异），但计划任务、
// 注册表 Run 键自启、起始位置为空的快捷方式，CWD 都是 C:\Windows\system32：
// 配置在那儿读、在那儿生成，还可能在提权时把 System32 写脏。
//
// 更糟的是 CWD 会跨 re-exec 传染：ensureCanonicalName(main.go:50) 与 restartSelf() 都用
// exec.Command 且不设 cmd.Dir，而 ensureCanonicalName 跑在 config.Load()(main.go:55)
// **之前** —— 子进程是带着坏 CWD 去 Load 的。
//
// CWD 回退只为「开发者 go build 到临时目录再从别处跑」这一种场景保留。**它对 zip 用户
// 永不触发**（exe 目录里 CI 那份 config.yml 永远在），所以别把它当兼容保证 —— 真正的
// 兼容保护是下面两条日志。
func configPath() string {
	exe, err := osExecutable()
	if err != nil {
		// 定位不了自己就维持旧行为（按 CWD），不 panic：配置读不到最多是用默认值，
		// 不该让整个服务起不来。
		log.Warn("无法定位程序路径（%v），config.yml 按当前工作目录解析", err)
		return configName
	}
	exeDir := filepath.Dir(exe)
	inExeDir := filepath.Join(exeDir, configName)

	if _, err := os.Stat(inExeDir); err == nil {
		// 唯一的真实回归人群在这里：计划任务/自启用户的 CWD 是 System32，他一直编辑的是
		// 那份；改成 exe 目录优先后，exe 目录里 CI 那份默认配置会赢，他的自定义静默归零。
		// 这条 Warn 精确命中他，把静默失效变成一行可见告警。
		if cwd, err := osGetwd(); err == nil && cwd != exeDir {
			if _, err := os.Stat(filepath.Join(cwd, configName)); err == nil {
				log.Warn("发现两份 config.yml，使用 %s，已忽略 %s",
					inExeDir, filepath.Join(cwd, configName))
			}
		}
		return inExeDir
	}

	// exe 目录没有 → CWD 有就用 CWD（开发者场景）
	if cwd, err := osGetwd(); err == nil && cwd != exeDir {
		inCwd := filepath.Join(cwd, configName)
		if _, err := os.Stat(inCwd); err == nil {
			log.Warn("exe 目录无 config.yml，改用当前工作目录的 %s", inCwd)
			return inCwd
		}
	}

	// 两处都没有 → 生成到 exe 目录（与 README 的「同目录」一致）
	return inExeDir
}

// Load 加载配置，优先级：命令行参数 > config.yml > 内置默认
func Load() Config {
	cfg := DefaultConfig()
	cfg.Sources = []string{"内置默认"}

	// 为已注册的播放器初始化配置
	for _, name := range registeredPlayers {
		cfg.Players[name] = &PlayerConfig{}
	}

	// 读、写、重读必须是同一个 path，否则会「写了读不到」（28b095c 修过这个坑）
	path := configPath()

	// 尝试从 config.yml 加载
	if data, err := os.ReadFile(path); err == nil {
		var m map[string]interface{}
		if err := yaml.Unmarshal(data, &m); err == nil {
			mergeYAML(&cfg, m)
			cfg.Sources = []string{"config.yml"}
			// 打绝对路径：没有它，任何「读错了那一份」都无法诊断 —— 用户和排障者
			// 都只会看到「已加载 config.yml」而不知道是哪一份。
			log.Info("已加载 %s", path)
		} else {
			log.Warn("解析 %s 失败: %v", path, err)
		}
	} else if os.IsNotExist(err) {
		generateDefaultConfig(path)
		// 生成后立即加载，使本次运行也应用生成的配置
		if data, err := os.ReadFile(path); err == nil {
			var m map[string]interface{}
			if err := yaml.Unmarshal(data, &m); err == nil {
				mergeYAML(&cfg, m)
				cfg.Sources = []string{"config.yml"}
			}
		}
	} else {
		log.Warn("读取 %s 失败: %v", path, err)
	}

	// 命令行参数覆盖
	var cliAddr string
	var cliOffset, cliPoll int
	flag.StringVar(&cliAddr, "addr", "", "WebSocket 监听地址")
	flag.IntVar(&cliOffset, "offset", 0, "歌词时间偏移（毫秒）")
	flag.IntVar(&cliPoll, "poll", 0, "轮询间隔（毫秒）")

	// 为已注册的播放器动态创建 CLI flag
	type playerCLI struct {
		offset *int
		poll   *int
	}
	cliPlayers := make(map[string]*playerCLI)
	for _, name := range registeredPlayers {
		cliPlayers[name] = &playerCLI{
			offset: flag.Int(name+"-offset", 0, fmt.Sprintf("%s 歌词时间偏移（毫秒）", name)),
			poll:   flag.Int(name+"-poll", 0, fmt.Sprintf("%s 轮询间隔（毫秒）", name)),
		}
	}

	// 自定义 Usage：全局 flag 在前，播放器 flag 按字母排序在后
	globalFlags := []string{"addr", "offset", "poll"}
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage of %s:\n", os.Args[0])
		// 先输出全局 flag
		for _, name := range globalFlags {
			f := flag.Lookup(name)
			if f != nil {
				printFlag(f)
			}
		}
		// 收集并排序播放器 flag
		var playerFlagNames []string
		flag.VisitAll(func(f *flag.Flag) {
			for _, g := range globalFlags {
				if f.Name == g {
					return
				}
			}
			playerFlagNames = append(playerFlagNames, f.Name)
		})
		sort.Strings(playerFlagNames)
		for _, name := range playerFlagNames {
			f := flag.Lookup(name)
			if f != nil {
				printFlag(f)
			}
		}
	}

	flag.Parse()

	hasCliArgs := false
	flag.Visit(func(f *flag.Flag) {
		hasCliArgs = true
		cfg.ExplicitKeys[f.Name] = true
		switch f.Name {
		case "addr":
			cfg.Addr = cliAddr
		case "offset":
			cfg.Offset = cliOffset
		case "poll":
			cfg.Poll = cliPoll
		default:
			for _, name := range registeredPlayers {
				if f.Name == name+"-offset" {
					v := *cliPlayers[name].offset
					cfg.Players[name].Offset = &v
				} else if f.Name == name+"-poll" {
					v := *cliPlayers[name].poll
					cfg.Players[name].Poll = &v
				}
			}
		}
	})
	if hasCliArgs {
		cfg.Sources = append(cfg.Sources, "命令行参数")
	}

	clampPolls(&cfg)

	return cfg
}

// 轮询间隔的安全边界。
//
// 这是**安全下限**，不是调参下限：各播放器自身另有更高的下限（qqmusic <30ms→50ms、
// cloudmusic <50ms→100ms，且后者兼防 time.NewTicker(0) panic），那些是它们各自的性能
// 考量，都在 pollMin 之上、不会与本边界冲突。这里只挡「会把机器打爆」的值。
const (
	pollMin = 10
	pollMax = 2000
)

// clampPoll 把轮询间隔夹进 [pollMin, pollMax]，返回夹后的值与是否发生过夹紧。
func clampPoll(v int) (int, bool) {
	if v < pollMin {
		return pollMin, true
	}
	if v > pollMax {
		return pollMax, true
	}
	return v, false
}

// clampPolls 对全局 Poll 与**每个 per-player 覆盖**应用同一边界，越界打 Warn。
//
// per-player 那半边曾经是直通的：钳制只作用于 cfg.Poll，而 GetPlayerPoll 命中覆盖时
// 原样返回 *pc.Poll，main.go 直接把它喂进各播放器的 New()。真正会咬人的是 poll <= 0
// —— time.Sleep(<=0) 立即返回，主循环变成无界忙等；wesing 还会每 33 圈自旋一次
// CreateToolhelp32Snapshot（全进程表快照）+ EnumWindows。而 kugou **完全没有自保**
// （pollMs 直通到 time.Sleep），本函数是它唯一的保护。
//
// 遍历用排序后的键而非 range map：Warn 的输出顺序得是确定的，否则同一份配置每次启动
// 的日志顺序都不同，排障时无法比对。
func clampPolls(cfg *Config) {
	if v, clamped := clampPoll(cfg.Poll); clamped {
		log.Warn("poll=%d 超出范围 [%d,%d]，已钳制为 %d", cfg.Poll, pollMin, pollMax, v)
		cfg.Poll = v
	}

	names := make([]string, 0, len(cfg.Players))
	for name := range cfg.Players {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		pc := cfg.Players[name]
		// nil 覆盖 = 沿用全局，别把它实体化成值（*int 的 nil 与 0 必须可区分，见 AGENTS.md §3.3）
		if pc == nil || pc.Poll == nil {
			continue
		}
		if v, clamped := clampPoll(*pc.Poll); clamped {
			log.Warn("%s-poll=%d 超出范围 [%d,%d]，已钳制为 %d", name, *pc.Poll, pollMin, pollMax, v)
			*pc.Poll = v
		}
	}
}

// mergeYAML 只覆盖 YAML 中实际写了的字段
func mergeYAML(dst *Config, m map[string]interface{}) {
	mark := func(key string) { dst.ExplicitKeys[key] = true }
	if v, ok := m["addr"]; ok {
		if s, ok := v.(string); ok {
			dst.Addr = s
			mark("addr")
		}
	}
	if v, ok := m["offset"]; ok {
		if i, ok := v.(int); ok {
			dst.Offset = i
			mark("offset")
		}
	}
	if v, ok := m["poll"]; ok {
		if i, ok := v.(int); ok {
			dst.Poll = i
			mark("poll")
		}
	}
	if v, ok := m["prior-player"]; ok {
		if arr, ok := v.([]interface{}); ok {
			dst.PriorPlayer = nil
			for _, item := range arr {
				if s, ok := item.(string); ok {
					dst.PriorPlayer = append(dst.PriorPlayer, s)
				}
			}
			mark("prior-player")
		}
	}
	if v, ok := m["prior-player-expire"]; ok {
		if i, ok := v.(int); ok {
			dst.PriorPlayerExpire = i
			mark("prior-player-expire")
		}
	}
	if v, ok := m["cloudmusicv3-effect-strategy"]; ok {
		if s, ok := v.(string); ok && (s == "park" || s == "fadeout") {
			dst.EffectStrategy = s
			mark("cloudmusicv3-effect-strategy")
		}
	}

	// 动态合并已注册播放器的专属配置
	for _, name := range registeredPlayers {
		pc := dst.Players[name]
		if pc == nil {
			pc = &PlayerConfig{}
			dst.Players[name] = pc
		}
		if v, ok := m[name+"-offset"]; ok {
			if i, ok := v.(int); ok {
				pc.Offset = &i
				mark(name + "-offset")
			}
		}
		if v, ok := m[name+"-poll"]; ok {
			if i, ok := v.(int); ok {
				pc.Poll = &i
				mark(name + "-poll")
			}
		}
	}
}

func printFlag(f *flag.Flag) {
	// 复现 flag 包默认格式
	s := fmt.Sprintf("  -%s", f.Name)
	name, usage := flag.UnquoteUsage(f)
	if len(name) > 0 {
		s += " " + name
	}
	if len(s) <= 4 {
		s += "\t"
	} else {
		s += "\n    \t"
	}
	s += usage
	fmt.Fprint(os.Stderr, s, "\n")
}

const defaultConfigContent = `# Metabox-Nexus-PlayerCap 配置文件
# 优先级：命令行参数 > config.yml > 内置默认值

# HTTP/WebSocket/SSE 监听地址
addr: "0.0.0.0:8765"

# 歌词时间偏移（毫秒），正值=歌词提前，负值=延后
offset: 200

# 轮询间隔（毫秒），范围 10~2000
poll: 30

# 优先播放器；按需取消注释以分配
prior-player:
- wesing
# - cloudmusicv3
# - qqmusic
# - kugou

# 优先播放器暂停超过n秒，自动切换到最后一个普通播放器
prior-player-expire: 15

# 全民K歌 配置
# wesing-offset: 0
# wesing-poll: 30

# 网易云音乐 v3 配置
cloudmusicv3-offset: 500
# cloudmusicv3-poll: 100   # 低于 50 会被网易云自身的下限抬到 100，写 30 也是跑 100
cloudmusicv3-effect-strategy: fadeout # 特效最小化策略：park 自动屏外渲染保活 / fadeout 自动淡出

# QQ 音乐 配置
qqmusic-offset: 400
# qqmusic-poll: 50

# 酷狗音乐 配置
kugou-offset: 430
# kugou-poll: 30
`

// DefaultConfigContent 返回内置默认配置模板（唯一权威来源）。
// 供 tools/genconfig 在 CI 打包时生成一份 clean 的 config.yml，使发版不受本地 config.yml 影响。
func DefaultConfigContent() string { return defaultConfigContent }

// generateDefaultConfig 把模板写到 path。path 必须与 Load 的读取点一致，否则「写了读不到」。
func generateDefaultConfig(path string) {
	if err := os.WriteFile(path, []byte(defaultConfigContent), 0644); err != nil {
		// 原本是无 else 的 `if err == nil`，写失败零输出 —— 用户只会发现「我的配置怎么
		// 没生效」而看不到任何线索。写失败不是致命错误（本次运行用内置默认继续），但必须可见。
		log.Warn("生成 %s 失败: %v（本次运行使用内置默认配置）", path, err)
		return
	}
	log.Info("已自动生成 %s", path)
}
