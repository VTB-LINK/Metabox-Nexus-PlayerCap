package config

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/logger"

	"gopkg.in/yaml.v3"
)

var log = logger.New("Config")

// PlayerConfig 单播放器配置覆盖（nil 表示沿用全局值）
type PlayerConfig struct {
	Offset         *int
	Poll           *int
	IdleHide       *int  // per-player 通道无活跃自动隐藏阈值（秒，0=显式关）；nil=跟随全局 PerPlayerIdleHide
	AudioNormalize *bool // per-player 音量归一化开关；nil=跟随全局 AudioNormalize（对照 IdleHide 的 *int 语义）
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
	Addr              string   `yaml:"addr"`                 // WebSocket 监听地址
	Offset            int      `yaml:"offset"`               // 全局歌词延迟（毫秒）：正值=延后，负值=提前
	Poll              int      `yaml:"poll"`                 // 全局轮询间隔（毫秒）
	PriorPlayer       []string `yaml:"prior-player"`         // 优先播放器列表
	PriorPlayerExpire int      `yaml:"prior-player-expire"`  // 优先播放器暂停超时（秒）
	PerPlayerIdleHide int      `yaml:"per-player-idle-hide"` // 指定播放器通道无活跃自动隐藏（秒，0=关）
	// 音量归一化（EMA-AGC）。yaml tag 仅为文档，实际解码经 mergeYAML 手工搬运（见 §3.3）。
	AudioNormalize        bool                     `yaml:"audio-normalize"`          // 全局音量归一化开关
	AudioNormalizeTarget  int                      `yaml:"audio-normalize-target"`   // 目标响度（dBFS），范围 [-60,0]
	AudioNormalizeMaxGain int                      `yaml:"audio-normalize-max-gain"` // 最大增益（dB），范围 [0,45]
	AudioFrameRate        int                      `yaml:"audio-frame-rate"`         // 音频波形帧的出帧帧率（帧每秒），范围 [15,60]
	EffectStrategy        string                   `yaml:"-"`                        // 网易云特效最小化策略：park | fadeout（键 cloudmusicv3-effect-strategy）
	Players               map[string]*PlayerConfig `yaml:"-"`                        // 各播放器专属配置
	Sources               []string                 `yaml:"-"`                        // 配置来源列表（内部字段）
	ExplicitKeys          map[string]bool          `yaml:"-"`                        // 被显式设置（非默认）的字段集合
	// WrittenValues 记录被钳制或校验回退改写过的键在配置中写下的原值（键名同 config.yml），
	// 未被改写的键不在其中。上面各字段保存的始终是采纳后的运行值；原值只供 /service-status
	// 的 config 回显（写下的值），经 Written / WrittenPlayerPoll 读取。
	//
	// 这里每个值的类型都与该键的正常类型相同：钳制前的整数、取值非法的 cloudmusicv3-effect-strategy
	// 字符串。它们会进 /service-status 的 config，而那里各字段的类型是对外约定，不得随写法变化。
	// 类型不符而被拒的原值因此不记在这里，见 RejectedValues。
	WrittenValues map[string]any `yaml:"-"`
	// RejectedValues 记录 config.yml 里因类型不符而没有被采纳的键写下的原值（键名同 config.yml），类型
	// 即写下的类型：写成字符串的数字是 string，另可能是小数、布尔、nil、列表、映射。prior-player 列表里
	// 混有非字符串项时记的是整个原列表（其余项照常被采纳）。只供启动横幅列出「写下的是什么」，不进
	// /service-status：对三件套而言这些键视同未设置。
	RejectedValues map[string]any `yaml:"-"`
	// ConfigFile 是本次加载所用 config.yml 的绝对路径（configPath 的选址结果），ConfigFileState 是加载结果。
	// 二者由 loadFile 记下，只供启动横幅说明本次用的是哪一份；未经 Load 的 Config 上 ConfigFile 为空。
	ConfigFile      string    `yaml:"-"`
	ConfigFileState FileState `yaml:"-"`
	// FileVersion 是 config.yml 在本次启动改写之前的版本（版本键 config-version 的规范形式，如 v4.0.0；
	// 没有版本键的是 v3.0.0），由 loadFile 在改写文件之前记下。读取或解析失败、版本键不是合法版本号时为空串。
	//
	// 强制重建的判定（网关的 config_force_update）只能用它，不得重读文件：loadFile 返回时磁盘上的版本键
	// 已是本进程的盖章值，重读只会得出「已是新版本」。
	FileVersion string `yaml:"-"`
	// 以下各项同样由 loadFile 记下，供启动横幅的配置文件行与强制重建使用。
	// FileVersionLabel 是文件版本在备份名与提示里的写法：没有版本键的写作 v3，版本键非法的写作 unknown。
	// FileStamp 是本进程的盖章值。FileRewrite 是改写结果。FileBackup 是本次改写留在磁盘上的备份的路径
	// （迁移留下的原文件；改写中途放弃而没能放回的那一份）。OrphanBackup 是本次按模板生成 config.yml 时
	// 同目录里最新一份非空备份的文件名：原有设置可能在其中。
	FileVersionLabel string       `yaml:"-"`
	FileStamp        string       `yaml:"-"`
	FileRewrite      RewriteState `yaml:"-"`
	FileBackup       string       `yaml:"-"`
	OrphanBackup     string       `yaml:"-"`
}

// FileState 是 Load 对 config.yml 的加载结果。除 FileLoaded 与 FileGenerated 外，config.yml 一层都没有生效，
// 本次运行只有内置默认配置与命令行参数。
type FileState int

const (
	FileLoaded         FileState = iota // 已读取并解析
	FileGenerated                       // 原先不存在，本次按模板生成后读取并解析
	FileReadFailed                      // 存在但读取失败
	FileParseFailed                     // 读到了但 YAML 解析失败
	FileGenerateFailed                  // 不存在，且未能生成出可加载的文件
)

// noteWritten 记录 key 写下但未按原样采纳的值。
func (c *Config) noteWritten(key string, v any) {
	if c.WrittenValues == nil {
		c.WrittenValues = make(map[string]any)
	}
	c.WrittenValues[key] = v
}

// noteRejected 记录 key 在 config.yml 里写下而因类型不符未被采纳的原值，只供启动横幅读取。
func (c *Config) noteRejected(key string, v any) {
	if c.RejectedValues == nil {
		c.RejectedValues = make(map[string]any)
	}
	c.RejectedValues[key] = v
}

// Written 返回 key 在配置中写下的值；该键未被钳制或校验回退改写过时即为 adopted（运行值）。
func (c *Config) Written(key string, adopted any) any {
	if v, ok := c.WrittenValues[key]; ok {
		return v
	}
	return adopted
}

// WrittenPlayerPoll 返回 <name>-poll 写下的值：继承解析同 GetPlayerPoll（播放器覆盖 → 全局），
// 但取钳制前的原值。
func (c *Config) WrittenPlayerPoll(playerName string) int {
	key, v := "poll", c.Poll
	if pc, ok := c.Players[playerName]; ok && pc.Poll != nil {
		key, v = playerName+"-poll", *pc.Poll
	}
	if w, ok := c.WrittenValues[key].(int); ok {
		return w
	}
	return v
}

// DefaultConfig 返回内置默认配置
func DefaultConfig() Config {
	return Config{
		Addr:                  "0.0.0.0:8765",
		Offset:                -200,
		Poll:                  30,
		PriorPlayer:           []string{"wesing"},
		PriorPlayerExpire:     15,
		PerPlayerIdleHide:     0,
		AudioNormalize:        true,
		AudioNormalizeTarget:  -14,
		AudioNormalizeMaxGain: 30,
		AudioFrameRate:        25,
		EffectStrategy:        "fadeout",
		Players:               make(map[string]*PlayerConfig),
		ExplicitKeys:          make(map[string]bool),
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

// GetPlayerIdleHide 获取播放器 per-player 通道无活跃自动隐藏阈值（秒，0=关）。
// 未设置（nil）则回退全局 PerPlayerIdleHide，语义同 GetPlayerOffset / GetPlayerPoll。
func (c *Config) GetPlayerIdleHide(playerName string) int {
	if pc, ok := c.Players[playerName]; ok && pc.IdleHide != nil {
		return *pc.IdleHide
	}
	return c.PerPlayerIdleHide
}

// GetPlayerAudioNormalize 获取播放器音量归一化开关。
// per-player *bool 覆盖优先（false 与 nil 可区分），nil 则回退全局 AudioNormalize，语义同 GetPlayerIdleHide。
func (c *Config) GetPlayerAudioNormalize(playerName string) bool {
	if pc, ok := c.Players[playerName]; ok && pc.AudioNormalize != nil {
		return *pc.AudioNormalize
	}
	return c.AudioNormalize
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
// 更糟的是 CWD 会跨 re-exec 传染：main.go 的 ensureCanonicalName() 与 restartSelf() 都用
// exec.Command 且不设 cmd.Dir，而 main() 里 ensureCanonicalName 跑在 config.Load()
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
		log.Warn("Cannot locate executable path (%v); resolving config.yml against the current working directory", err)
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
				log.Warn("Found two config.yml files; using %s and ignoring %s",
					inExeDir, filepath.Join(cwd, configName))
			}
		}
		return inExeDir
	}

	// exe 目录没有 → CWD 有就用 CWD（开发者场景）
	if cwd, err := osGetwd(); err == nil && cwd != exeDir {
		inCwd := filepath.Join(cwd, configName)
		if _, err := os.Stat(inCwd); err == nil {
			log.Warn("No config.yml in the executable directory; using %s from the current working directory instead", inCwd)
			return inCwd
		}
	}

	// 两处都没有 → 生成到 exe 目录（与 README 的「同目录」一致）
	return inExeDir
}

// Load 加载配置，优先级：命令行参数 > config.yml > 内置默认。programVersion 是程序版本（main.Version），
// 只用来定 config.yml 版本键的盖章值（StampVersion）。
func Load(programVersion string) Config {
	cfg := DefaultConfig()
	cfg.Sources = []string{i18n.T("built-in default")}

	// 为已注册的播放器初始化配置
	for _, name := range registeredPlayers {
		cfg.Players[name] = &PlayerConfig{}
	}

	loadFile(&cfg, programVersion)

	// 命令行参数覆盖
	var cliAddr, cliPriorPlayer, cliEffectStrategy string
	var cliOffset, cliPoll, cliIdleHide, cliPriorExpire int
	var cliAudioNormalize bool
	var cliAudioNormalizeTarget, cliAudioNormalizeMaxGain, cliAudioFrameRate int
	flag.StringVar(&cliAddr, "addr", "", i18n.T("WebSocket listen address"))
	flag.IntVar(&cliOffset, "offset", 0, i18n.T("lyric delay (ms); positive = later, negative = earlier"))
	flag.IntVar(&cliPoll, "poll", 0, i18n.T("poll interval (ms)"))
	flag.IntVar(&cliPriorExpire, "prior-player-expire", 0, i18n.T("prior player pause timeout (s); 0 disables all timeouts including the normal group, use with caution"))
	flag.IntVar(&cliIdleHide, "per-player-idle-hide", 0, i18n.T("per-player channel idle auto-hide (s, 0 = off)"))
	flag.BoolVar(&cliAudioNormalize, "audio-normalize", false, i18n.T("volume normalization switch"))
	flag.IntVar(&cliAudioNormalizeTarget, "audio-normalize-target", 0, i18n.T("volume normalization target loudness (dBFS), range [-60,0]"))
	flag.IntVar(&cliAudioNormalizeMaxGain, "audio-normalize-max-gain", 0, i18n.T("volume normalization maximum gain (dB), range [0,45]"))
	flag.IntVar(&cliAudioFrameRate, "audio-frame-rate", 0, i18n.T("audio waveform frame rate (frames per second), range [15,60]"))
	flag.StringVar(&cliPriorPlayer, "prior-player", "", i18n.T("prior player list, comma-separated (e.g. wesing,kugou); an empty string means no prior player"))
	flag.StringVar(&cliEffectStrategy, "cloudmusicv3-effect-strategy", "", i18n.T("NetEase effect minimize strategy: park (off-screen keep-alive) | fadeout (fade out)"))

	// 为已注册的播放器动态创建 CLI flag
	type playerCLI struct {
		offset         *int
		poll           *int
		idleHide       *int
		audioNormalize *bool
	}
	cliPlayers := make(map[string]*playerCLI)
	for _, name := range registeredPlayers {
		cliPlayers[name] = &playerCLI{
			offset:         flag.Int(name+"-offset", 0, fmt.Sprintf(i18n.T("%s lyric delay (ms); positive = later, negative = earlier"), name)),
			poll:           flag.Int(name+"-poll", 0, fmt.Sprintf(i18n.T("%s poll interval (ms)"), name)),
			idleHide:       flag.Int(name+"-idle-hide", 0, fmt.Sprintf(i18n.T("%s idle auto-hide (s, 0 = off, omit = follow global)"), name)),
			audioNormalize: flag.Bool(name+"-audio-normalize", false, fmt.Sprintf(i18n.T("%s volume normalization switch (omit = follow global)"), name)),
		}
	}

	// 自定义 Usage：全局 flag 在前，播放器 flag 按字母排序在后
	globalFlags := []string{"addr", "offset", "poll", "prior-player", "prior-player-expire", "per-player-idle-hide", "audio-normalize", "audio-normalize-target", "audio-normalize-max-gain", "audio-frame-rate", "cloudmusicv3-effect-strategy"}
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

	// 开关的空格赋值写法（-audio-normalize false）先并成等号写法再解析。解析出错与 -h 的退出同
	// flag.Parse()：CommandLine 是 ExitOnError。
	flag.CommandLine.Parse(joinBoolArgs(os.Args[1:], flag.Lookup))
	// 解析停在第一个不是参数的词上，它后面的参数都没有生效：照写错参数名处理，不静默忽略。
	if err := extraArgError(flag.Args()); err != nil {
		fmt.Fprintln(flag.CommandLine.Output(), err)
		flag.Usage()
		os.Exit(2)
	}

	// CLI 的 cloudmusicv3-effect-strategy 取值非法时要撤销下面的显式标记，但 config.yml 已合法
	// 设置过的标记须保留（它记录的是 yml 那一层的改动）。
	effectStrategyFromYAML := cfg.ExplicitKeys["cloudmusicv3-effect-strategy"]

	hasCliArgs := false
	flag.Visit(func(f *flag.Flag) {
		hasCliArgs = true
		cfg.ExplicitKeys[f.Name] = true
		// 命令行的值取代 config.yml 一层写下的值：那一层为这个键留下的记录（取值非法的原值、类型不符而
		// 被拒的原值）随之作废。命令行的值自己越界或非法时，由后面的钳制与下面的分支重新记下。
		delete(cfg.WrittenValues, f.Name)
		delete(cfg.RejectedValues, f.Name)
		switch f.Name {
		case "addr":
			cfg.Addr = cliAddr
		case "offset":
			cfg.Offset = cliOffset
		case "poll":
			cfg.Poll = cliPoll
		case "prior-player-expire":
			cfg.PriorPlayerExpire = cliPriorExpire
		case "per-player-idle-hide":
			cfg.PerPlayerIdleHide = cliIdleHide
		case "audio-normalize":
			cfg.AudioNormalize = cliAudioNormalize
		case "audio-normalize-target":
			cfg.AudioNormalizeTarget = cliAudioNormalizeTarget
		case "audio-normalize-max-gain":
			cfg.AudioNormalizeMaxGain = cliAudioNormalizeMaxGain
		case "audio-frame-rate":
			cfg.AudioFrameRate = cliAudioFrameRate
		case "prior-player":
			cfg.PriorPlayer = splitCommaList(cliPriorPlayer)
		case "cloudmusicv3-effect-strategy":
			if validEffectStrategy(cliEffectStrategy) {
				cfg.EffectStrategy = cliEffectStrategy
			} else {
				cfg.noteWritten(f.Name, cliEffectStrategy)
				log.Warn("-cloudmusicv3-effect-strategy expects park or fadeout, got %q; ignored", cliEffectStrategy)
				if !effectStrategyFromYAML {
					delete(cfg.ExplicitKeys, f.Name)
				}
			}
		default:
			for _, name := range registeredPlayers {
				if f.Name == name+"-offset" {
					v := *cliPlayers[name].offset
					cfg.Players[name].Offset = &v
				} else if f.Name == name+"-poll" {
					v := *cliPlayers[name].poll
					cfg.Players[name].Poll = &v
				} else if f.Name == name+"-idle-hide" {
					v := *cliPlayers[name].idleHide
					cfg.Players[name].IdleHide = &v
				} else if f.Name == name+"-audio-normalize" {
					v := *cliPlayers[name].audioNormalize
					cfg.Players[name].AudioNormalize = &v
				}
			}
		}
	})
	if hasCliArgs {
		cfg.Sources = append(cfg.Sources, i18n.T("command-line arguments"))
	}

	clampPolls(&cfg)
	clampAudioNormalize(&cfg)
	clampAudioFrameRate(&cfg)
	warnUnknownPriorPlayers(&cfg)

	return cfg
}

// loadFile 读取 config.yml 并合并进 cfg（不存在则先按模板生成再读），同时记下所用的路径与加载结果
// （cfg.ConfigFile / cfg.ConfigFileState）。Load 的 config.yml 一层全在这里，独立成函数是为了能测：
// Load 本身注册全局 flag，调不了第二次。programVersion 是程序版本，定版本键的盖章值（StampVersion）。
func loadFile(cfg *Config, programVersion string) { loadFileWithOps(osFileOps, cfg, programVersion) }

// loadFileWithOps 是 loadFile 的实现，文件操作经 ops 执行（测试注入）。
//
// 合并之前先处理文件的版本（upgradeFile）：记下改写之前的文件版本，跨过语义变更时自动迁移，把版本键
// 改写为盖章值并写回文件。交给 mergeYAML 的是迁移之后的解析结果，与文件有没有写成无关。
func loadFileWithOps(ops *fileOps, cfg *Config, programVersion string) {
	// 读、写、重读必须是同一个 path，否则会「写了读不到」（28b095c 修过这个坑）
	path := configPath()
	cfg.ConfigFile = path
	if abs, err := filepath.Abs(path); err == nil {
		cfg.ConfigFile = abs // 定位不了 exe 时 configPath 返回相对路径，记下的仍须是绝对路径
	}

	data, err := os.ReadFile(path)
	generated := false
	if os.IsNotExist(err) {
		// 没能生成（generateDefaultConfig 已告警）或写出的文件读不回来，都算未能生成。放置时发现文件已由
		// 别的实例建出，则加载那一份。
		cfg.ConfigFileState = FileGenerateFailed
		generated = generateDefaultConfig(ops, cfg, path, StampVersion(programVersion))
		if data, err = os.ReadFile(path); err != nil {
			return
		}
	} else if err != nil {
		cfg.ConfigFileState = FileReadFailed
		log.Warn("Failed to read %s: %v", path, err)
		return
	}

	var m map[string]interface{}
	if err := yaml.Unmarshal(data, &m); err != nil {
		if !generated {
			cfg.ConfigFileState = FileParseFailed
			log.Warn("Failed to parse %s: %v", path, err)
		}
		return
	}
	mergeYAML(cfg, upgradeFile(ops, cfg, path, data, m, programVersion))
	cfg.Sources = []string{"config.yml"}
	if generated {
		cfg.ConfigFileState = FileGenerated // 生成后立即加载，使本次运行也应用生成的配置
		return
	}
	cfg.ConfigFileState = FileLoaded
	// 打绝对路径：没有它，任何「读错了那一份」都无法诊断 —— 用户和排障者
	// 都只会看到「已加载 config.yml」而不知道是哪一份。
	log.Info("Loaded %s", path)
}

// splitCommaList 把逗号分隔的 CLI 值切成去空白、去空项的字符串切片。传空串得到空切片
// （语义：显式清空，如 -prior-player "" 表示无优先播放器），与 mergeYAML 读 yaml 列表殊途同归。
func splitCommaList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// warnUnknownPriorPlayers 对 prior-player 里不认识的名字发告警。
//
// **它只告警，绝不过滤，也绝不让启动失败。** 未知名字本身是无害的：Router 会为它建一个
// 永远收不到事件的状态条目，状态恒 idle，既不进 priorGroupBlocking 也不抢主输出
// （server/router.go 的 NewRouter 从 cfg.PriorPlayer 建组，判据全是字符串比较）。
// 有害的是**静默**：用户把 sodamusic 拼成 sodamusci，配置看着生效了、日志一个字没有，
// 而 K 歌一开唱照样抢不到优先——排查时几乎不会怀疑到拼写。
//
// 判据用 registeredPlayers（各播放器包 init() 注册，Load 被调用时已全部就位）而不是
// main.go 的 playerNames：后者在 Load 之后才构造，这里拿不到。二者在正常构建下一致；
// 若某个包注册了却漏进 playerNames，那是 §3.2 单独盯的另一个坑，不由本函数负责。
func warnUnknownPriorPlayers(cfg *Config) {
	if len(cfg.PriorPlayer) == 0 {
		return // 不配优先播放器是合法的，不是错误
	}
	known := RegisteredPlayers()
	for _, name := range cfg.PriorPlayer {
		found := false
		for _, k := range known {
			if k == name {
				found = true
				break
			}
		}
		if !found {
			log.Warn("%q in prior-player is not a known player; this entry has no effect. Available values: %v", name, known)
		}
	}
}

// 轮询间隔的安全边界。
//
// 这是**安全下限**，不是调参下限：各播放器自身另有更高的下限（qqmusic <30ms→50ms、
// cloudmusic <50ms→100ms，且后者兼防 time.NewTicker(0) panic；sodamusic <200ms→200ms），
// 定义在各包的 EffectivePollInterval，那些是它们各自的性能考量，都在 pollMin 之上、不会与
// 本边界冲突。这里只挡「会把机器打爆」的值。
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
		log.Warn("poll=%d is out of range [%d,%d]; clamped to %d", cfg.Poll, pollMin, pollMax, v)
		cfg.noteWritten("poll", cfg.Poll)
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
			log.Warn("%s-poll=%d is out of range [%d,%d]; clamped to %d", name, *pc.Poll, pollMin, pollMax, v)
			cfg.noteWritten(name+"-poll", *pc.Poll)
			*pc.Poll = v
		}
	}
}

// 音量归一化的参数边界。target 是 dBFS（0 为满刻度，负值为低于满刻度），max-gain 是允许
// 放大的上限（dB，非负）。越界只钳不拒，照 clampPolls 惯例打 Warn。per-player 覆盖是 bool，
// 无数值范围，不参与钳制。
const (
	audioNormTargetMin  = -60
	audioNormTargetMax  = 0
	audioNormMaxGainMin = 0
	audioNormMaxGainMax = 45
)

// clampInt 把 v 夹进 [lo, hi]，返回夹后的值与是否发生过夹紧。
func clampInt(v, lo, hi int) (int, bool) {
	if v < lo {
		return lo, true
	}
	if v > hi {
		return hi, true
	}
	return v, false
}

// clampAudioNormalize 对全局 target/max-gain 应用边界，越界打 Warn。
func clampAudioNormalize(cfg *Config) {
	if v, clamped := clampInt(cfg.AudioNormalizeTarget, audioNormTargetMin, audioNormTargetMax); clamped {
		log.Warn("audio-normalize-target=%d is out of range [%d,%d]; clamped to %d",
			cfg.AudioNormalizeTarget, audioNormTargetMin, audioNormTargetMax, v)
		cfg.noteWritten("audio-normalize-target", cfg.AudioNormalizeTarget)
		cfg.AudioNormalizeTarget = v
	}
	if v, clamped := clampInt(cfg.AudioNormalizeMaxGain, audioNormMaxGainMin, audioNormMaxGainMax); clamped {
		log.Warn("audio-normalize-max-gain=%d is out of range [%d,%d]; clamped to %d",
			cfg.AudioNormalizeMaxGain, audioNormMaxGainMin, audioNormMaxGainMax, v)
		cfg.noteWritten("audio-normalize-max-gain", cfg.AudioNormalizeMaxGain)
		cfg.AudioNormalizeMaxGain = v
	}
}

// 音频波形帧率（帧每秒）的边界。下限使帧间隔（66.7ms）低于读包的超时与墙钟上限（100ms）：没有数据时
// 保活节拍仍约 100ms，管理器的 hangThreshold 与 noCaptureAfter（audiocap/manager/gating.go）以此为
// 前提；连续的数据流也不会被墙钟切出残帧（audiocap/helper 的 clockDue）。上限是直播画面常见的最高帧率。
const (
	audioFrameRateMin = 15
	audioFrameRateMax = 60
)

// clampAudioFrameRate 对全局 audio-frame-rate 应用边界，越界打 Warn。
func clampAudioFrameRate(cfg *Config) {
	if v, clamped := clampInt(cfg.AudioFrameRate, audioFrameRateMin, audioFrameRateMax); clamped {
		log.Warn("audio-frame-rate=%d is out of range [%d,%d]; clamped to %d",
			cfg.AudioFrameRate, audioFrameRateMin, audioFrameRateMax, v)
		cfg.noteWritten("audio-frame-rate", cfg.AudioFrameRate)
		cfg.AudioFrameRate = v
	}
}

// validEffectStrategy 是 cloudmusicv3-effect-strategy 的取值判据，mergeYAML 与 CLI 覆盖共用。
//
// 非法值必须在 config 层拦下：server.SetDefaultEffectStrategy 对非法值静默忽略（运行时仍是
// fadeout），而 /service-status 的 config_effective 与 config_overwritten 读的是 cfg 的运行值 ——
// 放过去就是回显显示已生效、实际并未生效。被拦下的写下值记入 WrittenValues，只进 config 回显与横幅的「写下的值」。
func validEffectStrategy(s string) bool { return s == "park" || s == "fadeout" }

// mergeYAML 只覆盖 YAML 中实际写了的字段。
//
// 每个键的值类型不符时都走同一套：不采纳、不标记显式，noteRejected 记下原值（只供启动横幅列出
// 「写下的是什么」），并告警点名键与实际类型。原值不得记进 WrittenValues：那会让它进 /service-status 的
// config，而该处各字段的类型是对外约定。新增分支照此写；config/writtenvalues_test.go 的
// TestMergeYAMLRejectedValues 按本函数读取的键逐个核对。
func mergeYAML(dst *Config, m map[string]interface{}) {
	mark := func(key string) { dst.ExplicitKeys[key] = true }
	if v, ok := m["addr"]; ok {
		if s, ok := v.(string); ok {
			dst.Addr = s
			mark("addr")
		} else {
			dst.noteRejected("addr", v)
			log.Warn("addr expects a string, got %T (%v); ignored", v, v)
		}
	}
	if v, ok := m["offset"]; ok {
		if i, ok := v.(int); ok {
			dst.Offset = i
			mark("offset")
		} else {
			dst.noteRejected("offset", v)
			log.Warn("offset expects an integer, got %T (%v); ignored", v, v)
		}
	}
	if v, ok := m["poll"]; ok {
		if i, ok := v.(int); ok {
			dst.Poll = i
			mark("poll")
		} else {
			dst.noteRejected("poll", v)
			log.Warn("poll expects an integer, got %T (%v); ignored", v, v)
		}
	}
	if v, ok := m["prior-player"]; ok {
		if arr, ok := v.([]interface{}); ok {
			dst.PriorPlayer = nil
			for _, item := range arr {
				if s, ok := item.(string); ok {
					dst.PriorPlayer = append(dst.PriorPlayer, s)
				} else {
					// 其余项照常采纳，键仍算显式设置；写下的原列表记下，与采纳后的列表对照。
					dst.noteRejected("prior-player", arr)
					log.Warn("prior-player item %v is not a string (%T); item ignored", item, item)
				}
			}
			mark("prior-player")
		} else {
			dst.noteRejected("prior-player", v)
			log.Warn("prior-player expects a list, got %T (%v); ignored", v, v)
		}
	}
	if v, ok := m["prior-player-expire"]; ok {
		if i, ok := v.(int); ok {
			dst.PriorPlayerExpire = i
			mark("prior-player-expire")
		} else {
			dst.noteRejected("prior-player-expire", v)
			log.Warn("prior-player-expire expects an integer, got %T (%v); ignored", v, v)
		}
	}
	if v, ok := m["per-player-idle-hide"]; ok {
		if i, ok := v.(int); ok {
			dst.PerPlayerIdleHide = i
			mark("per-player-idle-hide")
		} else {
			dst.noteRejected("per-player-idle-hide", v)
			log.Warn("per-player-idle-hide expects an integer, got %T (%v); ignored", v, v)
		}
	}
	if v, ok := m["audio-normalize"]; ok {
		if b, ok := v.(bool); ok {
			dst.AudioNormalize = b
			mark("audio-normalize")
		} else {
			dst.noteRejected("audio-normalize", v)
			log.Warn("audio-normalize expects a boolean, got %T (%v); ignored", v, v)
		}
	}
	if v, ok := m["audio-normalize-target"]; ok {
		if i, ok := v.(int); ok {
			dst.AudioNormalizeTarget = i
			mark("audio-normalize-target")
		} else {
			dst.noteRejected("audio-normalize-target", v)
			log.Warn("audio-normalize-target expects an integer, got %T (%v); ignored", v, v)
		}
	}
	if v, ok := m["audio-normalize-max-gain"]; ok {
		if i, ok := v.(int); ok {
			dst.AudioNormalizeMaxGain = i
			mark("audio-normalize-max-gain")
		} else {
			dst.noteRejected("audio-normalize-max-gain", v)
			log.Warn("audio-normalize-max-gain expects an integer, got %T (%v); ignored", v, v)
		}
	}
	if v, ok := m["audio-frame-rate"]; ok {
		if i, ok := v.(int); ok {
			dst.AudioFrameRate = i
			mark("audio-frame-rate")
		} else {
			dst.noteRejected("audio-frame-rate", v)
			log.Warn("audio-frame-rate expects an integer, got %T (%v); ignored", v, v)
		}
	}
	if v, ok := m["cloudmusicv3-effect-strategy"]; ok {
		if s, ok := v.(string); ok && validEffectStrategy(s) {
			dst.EffectStrategy = s
			mark("cloudmusicv3-effect-strategy")
		} else {
			if ok {
				dst.noteWritten("cloudmusicv3-effect-strategy", s) // 非法字符串：不采纳，但回显写下的值（类型没变）
			} else {
				dst.noteRejected("cloudmusicv3-effect-strategy", v) // 不是字符串：类型不符，只上横幅
			}
			log.Warn("cloudmusicv3-effect-strategy expects park or fadeout, got %T (%v); ignored", v, v)
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
			} else {
				dst.noteRejected(name+"-offset", v)
				log.Warn("%s-offset expects an integer, got %T (%v); ignored", name, v, v)
			}
		}
		if v, ok := m[name+"-poll"]; ok {
			if i, ok := v.(int); ok {
				pc.Poll = &i
				mark(name + "-poll")
			} else {
				dst.noteRejected(name+"-poll", v)
				log.Warn("%s-poll expects an integer, got %T (%v); ignored", name, v, v)
			}
		}
		if v, ok := m[name+"-idle-hide"]; ok {
			if i, ok := v.(int); ok {
				pc.IdleHide = &i
				mark(name + "-idle-hide")
			} else {
				dst.noteRejected(name+"-idle-hide", v)
				log.Warn("%s-idle-hide expects an integer, got %T (%v); ignored", name, v, v)
			}
		}
		if v, ok := m[name+"-audio-normalize"]; ok {
			if b, ok := v.(bool); ok {
				pc.AudioNormalize = &b
				mark(name + "-audio-normalize")
			} else {
				dst.noteRejected(name+"-audio-normalize", v)
				log.Warn("%s-audio-normalize expects a boolean, got %T (%v); ignored", name, v, v)
			}
		}
	}
}

func printFlag(f *flag.Flag) {
	fmt.Fprint(os.Stderr, flagUsage(f), "\n")
}

// flagUsage 返回 -h 里 f 的那一项，复现 flag 包默认格式（不列默认值）。开关另带取值提示：flag 包对开关
// 不列取值占位，看不出它可以带值（-x、-x false、-x=false 都认，见 joinBoolArgs）。
func flagUsage(f *flag.Flag) string {
	s := fmt.Sprintf("  -%s", f.Name)
	name, usage := flag.UnquoteUsage(f)
	if isBoolFlag(f) {
		name = "[true|false]"
	}
	if len(name) > 0 {
		s += " " + name
	}
	if len(s) <= 4 {
		s += "\t"
	} else {
		s += "\n    \t"
	}
	return s + usage
}

const defaultConfigContent = `# Metabox-Nexus-PlayerCap 配置文件
# 优先级：命令行参数 > config.yml > 内置默认值

# ` + versionKeyComment + `
` + versionKey + `: "` + versionPlaceholder + `"

# HTTP/WebSocket/SSE 监听地址
addr: "0.0.0.0:8765"

# 歌词延迟（毫秒），正值=延后，负值=提前
offset: -200

# 轮询间隔（毫秒），范围 10~2000
poll: 30

# 优先播放器；按需取消注释以分配
prior-player:
- wesing
# - cloudmusicv3
# - qqmusic
# - kugou
# - sodamusic
# - applemusic

# 优先播放器暂停超过n秒，自动切换到最后一个普通播放器
prior-player-expire: 15

# 「指定播放器」通道（/<player>/ws 等）无活跃自动隐藏：静默超过 n 秒后向该通道推一次清屏，
# 隐藏停留的末行歌词。0 = 关闭（默认）。仅作用于 per-player 端点，不影响跟随活跃播放器的根路径
# （根路径的自动隐藏另由 prior-player-expire 控制）。每个播放器可用 <name>-idle-hide 单独覆盖：
# 不写=跟随此全局值，0=该播放器关闭，n=该播放器用 n 秒（示例见下方 cloudmusicv3 段）。
per-player-idle-hide: 0

# 音频波形音量归一化：只作用于音频波形帧（/audio-ws 与 /<player>/audio-ws）的 RMS、左右声道峰值与频谱，
# 使各播放器的波形幅度趋于一致，不改变实际播放音量。每个播放器可用 <name>-audio-normalize 单独覆盖
# （true/false），不写=跟随此全局开关。
audio-normalize: true
# 目标响度（dBFS），范围 [-60,0]，越界自动钳到边界
audio-normalize-target: -14
# 最大增益（dB），范围 [0,45]，越界自动钳到边界
audio-normalize-max-gain: 30

# 音频波形帧率（帧每秒）：有音频数据时 /audio-ws 与 /<player>/audio-ws 每秒推送的帧数，
# 范围 [15,60]，越界自动钳到边界
audio-frame-rate: 25

# 全民K歌 配置
# wesing-offset: 0
# wesing-poll: 30
# wesing-idle-hide: 15   # 无活跃自动隐藏（秒）：不写=跟随全局 per-player-idle-hide，0=关
# wesing-audio-normalize: false   # 波形音量归一化开关：不写=跟随全局 audio-normalize

# 网易云音乐 v3 配置
cloudmusicv3-offset: -500
# cloudmusicv3-poll: 100   # 低于 50 会被网易云自身的下限抬到 100，写 30 也是跑 100
# cloudmusicv3-idle-hide: 15   # 无活跃自动隐藏（秒）：不写=跟随全局 per-player-idle-hide，0=关
# cloudmusicv3-audio-normalize: false   # 波形音量归一化开关：不写=跟随全局 audio-normalize
cloudmusicv3-effect-strategy: fadeout # 特效最小化策略：park 自动屏外渲染保活 / fadeout 自动淡出

# QQ 音乐 配置
qqmusic-offset: -400
# qqmusic-poll: 50
# qqmusic-idle-hide: 15   # 无活跃自动隐藏（秒）：不写=跟随全局 per-player-idle-hide，0=关
# qqmusic-audio-normalize: false   # 波形音量归一化开关：不写=跟随全局 audio-normalize

# 酷狗音乐 配置
kugou-offset: -430
# kugou-poll: 30
# kugou-idle-hide: 15   # 无活跃自动隐藏（秒）：不写=跟随全局 per-player-idle-hide，0=关
# kugou-audio-normalize: false   # 波形音量归一化开关：不写=跟随全局 audio-normalize

# 汽水音乐 配置
sodamusic-offset: -340
# sodamusic-poll: 200     # 低于 200 会被静默抬到 200（每次 Extract 是主进程→渲染器桥 + transport 往返，较重）
# sodamusic-idle-hide: 15   # 无活跃自动隐藏（秒）：不写=跟随全局 per-player-idle-hide，0=关
# sodamusic-audio-normalize: false   # 波形音量归一化开关：不写=跟随全局 audio-normalize

# Apple Music 配置（纯内存扫描接入，与自身同用户的 AppleMusic.exe 普通权限即可读，无需管理员）
applemusic-offset: -400
# applemusic-poll: 50     # 低于 50 会被静默抬到 50（每轮只读 24 字节；位置的跳变与歌词行都在这一轮里发现、推出，调慢歌词跟着晚）
# applemusic-idle-hide: 15   # 无活跃自动隐藏（秒）：不写=跟随全局 per-player-idle-hide，0=关
# applemusic-audio-normalize: false   # 波形音量归一化开关：不写=跟随全局 audio-normalize
`

// versionPlaceholder 是模板里版本键取值的占位符，由 templateContent 换成盖章值。
const versionPlaceholder = "__CONFIG_VERSION__"

// templateContent 返回版本键为 stamp 的模板。
func templateContent(stamp string) string {
	return strings.Replace(defaultConfigContent, versionPlaceholder, stamp, 1)
}

// DefaultConfigContent 返回内置默认配置模板（唯一权威来源），版本键为 programVersion 的盖章值
// （StampVersion：发布版本得到 v4.0.0 这样的值，其余得到开发构建的盖章值）。
// 供 tools/genconfig 在 CI 打包时生成一份 clean 的 config.yml，使发版不受本地 config.yml 影响。
func DefaultConfigContent(programVersion string) string {
	return templateContent(StampVersion(programVersion))
}
