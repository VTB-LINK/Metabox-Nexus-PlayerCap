package config

import (
	"flag"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config 应用配置
type Config struct {
	Addr    string   `yaml:"addr"`        // WebSocket 监听地址
	Offset  int      `yaml:"offset"`      // 时间偏移（毫秒）
	Poll    int      `yaml:"poll"`        // 轮询间隔（毫秒）
	Sources []string `yaml:"-"`           // 配置来源列表（内部字段）
}

// DefaultConfig 返回内置默认配置
func DefaultConfig() Config {
	return Config{
		Addr:   "0.0.0.0:8765",
		Offset: 200,
		Poll:   30,
	}
}

// Load 加载配置，优先级：命令行参数 > config.yml > 内置默认
func Load() Config {
	cfg := DefaultConfig()
	cfg.Sources = []string{"内置默认"}
	
	// 尝试从 config.yml 加载
	if data, err := os.ReadFile("config.yml"); err == nil {
		var fileCfg Config
		if err := yaml.Unmarshal(data, &fileCfg); err == nil {
			mergeYAML(&cfg, &fileCfg, data)
			cfg.Sources = []string{"config.yml"}
			fmt.Println("[*] 已加载 config.yml")
		} else {
			fmt.Printf("[!] 解析 config.yml 失败: %v\n", err)
		}
	} else if os.IsNotExist(err) {
		// 自动生成默认配置文件
		generateDefaultConfig()
	}

	// 命令行参数覆盖（仅覆盖用户显式指定的参数）
	var cliAddr string
	var cliOffset, cliPoll int
	flag.StringVar(&cliAddr, "addr", "", "WebSocket 监听地址")
	flag.IntVar(&cliOffset, "offset", 0, "歌词时间偏移（毫秒）")
	flag.IntVar(&cliPoll, "poll", 0, "轮询间隔（毫秒）")
	flag.Parse()

	// 只有用户显式传了参数才覆盖
	hasCliArgs := false
	flag.Visit(func(f *flag.Flag) {
		hasCliArgs = true
		switch f.Name {
		case "addr":
			cfg.Addr = cliAddr
		case "offset":
			cfg.Offset = cliOffset
		case "poll":
			cfg.Poll = cliPoll
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
func mergeYAML(dst *Config, src *Config, raw []byte) {
	// 用 map 检测哪些字段在 YAML 中被设置了
	var m map[string]interface{}
	yaml.Unmarshal(raw, &m)

	if _, ok := m["addr"]; ok {
		dst.Addr = src.Addr
	}
	if _, ok := m["offset"]; ok {
		dst.Offset = src.Offset
	}
	if _, ok := m["poll"]; ok {
		dst.Poll = src.Poll
	}
}

const defaultConfigContent = `# Metabox-Nexus-WesingCap 配置文件
# 优先级：命令行参数 > config.yml > 内置默认值

# WebSocket 监听地址
addr: "0.0.0.0:8765"

# 歌词时间偏移（毫秒），正值=歌词提前，负值=延后
offset: 0

# 轮询间隔（毫秒），范围 10~2000
poll: 100
`

func generateDefaultConfig() {
	if err := os.WriteFile("config.yml", []byte(defaultConfigContent), 0644); err == nil {
		fmt.Println("[*] 已自动生成 config.yml")
	}
}
