package config

import (
	"flag"
	"fmt"
	"os"
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

// Load 加载配置，优先级：命令行参数 > config.yml > 内置默认
func Load() Config {
	cfg := DefaultConfig()
	cfg.Sources = []string{"内置默认"}

	// 为已注册的播放器初始化配置
	for _, name := range registeredPlayers {
		cfg.Players[name] = &PlayerConfig{}
	}

	// 尝试从 config.yml 加载
	if data, err := os.ReadFile("config.yml"); err == nil {
		var m map[string]interface{}
		if err := yaml.Unmarshal(data, &m); err == nil {
			mergeYAML(&cfg, m)
			cfg.Sources = []string{"config.yml"}
			log.Info("已加载 config.yml")
		} else {
			log.Warn("解析 config.yml 失败: %v", err)
		}
	} else if os.IsNotExist(err) {
		generateDefaultConfig()
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

	// 限制轮询间隔
	if cfg.Poll < 10 {
		cfg.Poll = 10
	} else if cfg.Poll > 2000 {
		cfg.Poll = 2000
	}

	return cfg
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

# WebSocket 监听地址
addr: "0.0.0.0:8765"

# 歌词时间偏移（毫秒），正值=歌词提前，负值=延后
offset: 200

# 轮询间隔（毫秒），范围 10~2000
poll: 30

# 优先播放器
prior-player:
- wesing

# 优先播放器暂停超过n秒，自动切换到最后一个普通播放器
prior-player-expire: 15

# 全民K歌配置
# wesing-offset: 0
# wesing-poll: 30

#网易云音乐 v3 配置
cloudmusicv3-offset: 500
# cloudmusicv3-poll: 30
`

func generateDefaultConfig() {
	if err := os.WriteFile("config.yml", []byte(defaultConfigContent), 0644); err == nil {
		log.Info("已自动生成 config.yml")
	}
}
