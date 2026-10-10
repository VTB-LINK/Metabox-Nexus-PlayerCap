package main

// 本文件只测启动横幅的配置呈现：配置行（bannerConfigLines）逐键取自 buildConfigEcho 的三件套——每个键
// 都列出、显式设置的键带 *、写下的值与生效值不同时写成「写下的值 → 生效值」——与配置文件行
// （bannerConfigFileLine）。
//
// 三件套如何组装由 configecho_test.go 守。Config 有两种来历：样式、标记、轮询下限、钳制各组直接构造成
// config.Load 返回时的样子；被拒的取值与命令行覆盖两组用 loadConfig 以 config.yml 文本与命令行参数跑真实的
// config.Load，config.yml 一层、flag.Visit 与钳制都走到。

import (
	"flag"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"Metabox-Nexus-PlayerCap/config"
	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/server"
)

func intPtr(v int) *int    { return &v }
func boolPtr(v bool) *bool { return &v }

const bannerLegend = "   * = 显式设置（config.yml 或命令行）；a → b = 写下 a，实际生效 b\n"

// bannerLines 按 cfg 组装三件套并渲染配置行，同 main() 的接法：被拒的原值取 cfg.RejectedValues。
// win11 是注入的系统判定，不取本机版本。
func bannerLines(cfg *config.Config, win11 bool) []string {
	w, e, o := buildConfigEcho(cfg, echoPlayers, effectiveEffectStrategy(cfg.EffectStrategy, win11))
	return bannerConfigLines(w, e, o, cfg.RejectedValues, echoPlayers)
}

// loadConfig 以给定的 config.yml 文本与命令行参数跑一遍真实的 config.Load，返回它得到的 Config。
//
// 文本之前另加一行版本键，取值是本进程的盖章值：文件被当作已是当前版本，Load 不迁移、不改写它，yml 里
// 写的取值原样生效。没有版本键的文件（按 v3 迁移）走 loadConfigIn，见 configmigrate_test.go。
func loadConfig(t *testing.T, yml string, args ...string) config.Config {
	t.Helper()
	dir := t.TempDir()
	stamped := `config-version: "` + config.StampVersion(Version) + "\"\n" + yml
	if err := os.WriteFile(filepath.Join(dir, "config.yml"), []byte(stamped), 0644); err != nil {
		t.Fatal(err)
	}
	return loadConfigIn(t, dir, Version, args...)
}

// loadConfigIn 以 dir 为当前目录、version 为程序版本、args 为命令行参数跑一遍真实的 config.Load。
// dir 里的 config.yml 由调用方备好（没有即按模板生成）。
//
// Load 用的是全局的 flag.CommandLine 与 os.Args：这里换上一个新的 FlagSet 与参数表、用完还原，所以能
// 反复调。Load 先找测试二进制所在的目录，那里没有 config.yml 时才用当前目录的这一份，所以二进制旁边有
// config.yml 时直接失败，不去读一份不相干的配置。
func loadConfigIn(t *testing.T, dir, version string, args ...string) config.Config {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(exe), "config.yml")); err == nil {
		t.Fatalf("测试二进制所在的 %s 里有 config.yml，Load 会优先读它而不是本用例的配置；把二进制放到没有 config.yml 的目录再跑", filepath.Dir(exe))
	}
	t.Chdir(dir)

	oldFlags, oldArgs, oldUsage, oldOut := flag.CommandLine, os.Args, flag.Usage, log.Writer()
	flag.CommandLine = flag.NewFlagSet("playercap", flag.ContinueOnError)
	os.Args = append([]string{"playercap"}, args...)
	log.SetOutput(io.Discard) // Load 的加载日志与告警
	defer func() {
		flag.CommandLine, os.Args, flag.Usage = oldFlags, oldArgs, oldUsage
		log.SetOutput(oldOut)
	}()
	return config.Load(version)
}

// wantBannerLines 断言 want 的每一行都原样（整行）出现在横幅里。
func wantBannerLines(t *testing.T, lines []string, want ...string) {
	t.Helper()
	for _, w := range want {
		found := false
		for _, l := range lines {
			found = found || l == w+"\n"
		}
		if !found {
			t.Errorf("横幅缺少这一行：\n%s\n实际：\n%s", w, strings.Join(lines, ""))
		}
	}
}

// markExplicit 把键标为显式设置：config.Load 的 mergeYAML 与 flag.Visit 对被采纳的键所做的标记。
func markExplicit(cfg *config.Config, keys ...string) {
	for _, k := range keys {
		cfg.ExplicitKeys[k] = true
	}
}

// noteWritten 记下键写下而未按原样采纳的值：config 的钳制与取值校验所做的记录。
func noteWritten(cfg *config.Config, key string, v any) {
	if cfg.WrittenValues == nil {
		cfg.WrittenValues = map[string]any{}
	}
	cfg.WrittenValues[key] = v
}

// setGlobalInt 把一个可钳制的全局整数键置成 Load 之后的状态：运行值 adopted、标为显式；写下的值 written
// 与之不同（被钳制）时记入 WrittenValues。
func setGlobalInt(t *testing.T, cfg *config.Config, key string, written, adopted int) {
	t.Helper()
	switch key {
	case "poll":
		cfg.Poll = adopted
	case "audio-frame-rate":
		cfg.AudioFrameRate = adopted
	case "audio-normalize-target":
		cfg.AudioNormalizeTarget = adopted
	case "audio-normalize-max-gain":
		cfg.AudioNormalizeMaxGain = adopted
	default:
		t.Fatalf("setGlobalInt 不认识 %s", key)
	}
	if written != adopted {
		noteWritten(cfg, key, written)
	}
	markExplicit(cfg, key)
}

// playerLine 返回某播放器在默认配置下的横幅行，其中 poll 一项换成 pollItem。
func playerLine(name, pollItem string) string {
	s := "   播放器: " + name + " (offset=-200ms " + pollItem + " idle-hide=0s audio-normalize=true"
	if name == "cloudmusicv3" {
		s += " effect-strategy=fadeout"
	}
	return s + ")"
}

// stockEchoConfig 返回模板生成的 config.yml 原样加载后的 Config：全局键都写在文件里（都算显式设置），
// cloudmusicv3、qqmusic、kugou、sodamusic、applemusic 各写了 offset。
func stockEchoConfig() config.Config {
	cfg := defaultEchoConfig()
	markExplicit(&cfg, "addr", "offset", "poll", "prior-player", "prior-player-expire", "per-player-idle-hide",
		"audio-normalize", "audio-normalize-target", "audio-normalize-max-gain", "audio-frame-rate", "cloudmusicv3-effect-strategy")
	for name, offset := range map[string]int{"cloudmusicv3": -500, "qqmusic": -400, "kugou": -430, "sodamusic": -340, "applemusic": -400} {
		cfg.Players[name].Offset = intPtr(offset)
		markExplicit(&cfg, name+"-offset")
	}
	return cfg
}

// overriddenEchoConfig 返回一份覆盖较多的配置加载后的 Config，含配置层钳制（poll 5、audio-normalize-target 10、
// audio-normalize-max-gain 99、audio-frame-rate 100）、播放器层轮询下限与写成 park 的特效策略。
func overriddenEchoConfig(t *testing.T) config.Config {
	cfg := defaultEchoConfig()
	cfg.Addr, cfg.Offset, cfg.PriorPlayer, cfg.PriorPlayerExpire = "127.0.0.1:9000", 250, []string{"wesing", "kugou"}, 20
	cfg.PerPlayerIdleHide, cfg.AudioNormalize, cfg.EffectStrategy = 15, false, "park"
	setGlobalInt(t, &cfg, "poll", 5, 10)
	setGlobalInt(t, &cfg, "audio-normalize-target", 10, 0)
	setGlobalInt(t, &cfg, "audio-normalize-max-gain", 99, 45)
	setGlobalInt(t, &cfg, "audio-frame-rate", 100, 60)
	cfg.Players["wesing"].Offset = intPtr(0)
	cfg.Players["cloudmusicv3"].Offset, cfg.Players["cloudmusicv3"].Poll = intPtr(500), intPtr(40)
	cfg.Players["qqmusic"].IdleHide = intPtr(0)
	cfg.Players["kugou"].AudioNormalize = boolPtr(true)
	cfg.Players["sodamusic"].Poll = intPtr(30)
	cfg.Players["applemusic"].Offset = intPtr(400)
	markExplicit(&cfg, "addr", "offset", "prior-player", "prior-player-expire", "per-player-idle-hide", "audio-normalize",
		"cloudmusicv3-effect-strategy", "wesing-offset", "cloudmusicv3-offset", "cloudmusicv3-poll", "qqmusic-idle-hide",
		"kugou-audio-normalize", "sodamusic-poll", "applemusic-offset")
	return cfg
}

// TestBannerGolden 钉死配置行的整体样式：分行与行序、标签、键名、单位、标记、箭头与图例，中英文各一份。
//
// 变异自证（隔离副本，实测）：去掉单位、播放器行按回显顺序排列（effect-strategy 排到最前）、箭头两侧对调，
// 四例均红；中文译表缺一条行标签 → 中文三例红；bannerGlobalLines 里一条行格式照旧写成中文 → 英文一例红。
func TestBannerGolden(t *testing.T) {
	cases := []struct {
		name  string
		cfg   config.Config
		win11 bool
		lang  i18n.Lang
		want  string
	}{
		{"内置默认", defaultEchoConfig(), false, i18n.Chinese, `   监听: addr=0.0.0.0:8765
   全局偏移与轮询: offset=-200ms poll=30ms
   优先播放器: prior-player=[wesing] prior-player-expire=15s
   per-player 无活跃自动隐藏: per-player-idle-hide=0s
   音量归一化: audio-normalize=true audio-normalize-target=-14dBFS audio-normalize-max-gain=30dB
   音频波形帧率: audio-frame-rate=25fps
   播放器: wesing (offset=-200ms poll=30ms idle-hide=0s audio-normalize=true)
   播放器: cloudmusicv3 (offset=-200ms poll=30ms → 100ms idle-hide=0s audio-normalize=true effect-strategy=fadeout)
   播放器: qqmusic (offset=-200ms poll=30ms idle-hide=0s audio-normalize=true)
   播放器: kugou (offset=-200ms poll=30ms idle-hide=0s audio-normalize=true)
   播放器: sodamusic (offset=-200ms poll=30ms → 200ms idle-hide=0s audio-normalize=true)
   播放器: applemusic (offset=-200ms poll=30ms → 50ms idle-hide=0s audio-normalize=true)
   * = 显式设置（config.yml 或命令行）；a → b = 写下 a，实际生效 b
`},
		{"模板生成的 config.yml", stockEchoConfig(), false, i18n.Chinese, `   监听: *addr=0.0.0.0:8765
   全局偏移与轮询: *offset=-200ms *poll=30ms
   优先播放器: *prior-player=[wesing] *prior-player-expire=15s
   per-player 无活跃自动隐藏: *per-player-idle-hide=0s
   音量归一化: *audio-normalize=true *audio-normalize-target=-14dBFS *audio-normalize-max-gain=30dB
   音频波形帧率: *audio-frame-rate=25fps
   播放器: wesing (offset=-200ms poll=30ms idle-hide=0s audio-normalize=true)
   播放器: cloudmusicv3 (*offset=-500ms poll=30ms → 100ms idle-hide=0s audio-normalize=true *effect-strategy=fadeout)
   播放器: qqmusic (*offset=-400ms poll=30ms idle-hide=0s audio-normalize=true)
   播放器: kugou (*offset=-430ms poll=30ms idle-hide=0s audio-normalize=true)
   播放器: sodamusic (*offset=-340ms poll=30ms → 200ms idle-hide=0s audio-normalize=true)
   播放器: applemusic (*offset=-400ms poll=30ms → 50ms idle-hide=0s audio-normalize=true)
   * = 显式设置（config.yml 或命令行）；a → b = 写下 a，实际生效 b
`},
		{"覆盖较多（Windows 11）", overriddenEchoConfig(t), true, i18n.Chinese, `   监听: *addr=127.0.0.1:9000
   全局偏移与轮询: *offset=250ms *poll=5ms → 10ms
   优先播放器: *prior-player=[wesing kugou] *prior-player-expire=20s
   per-player 无活跃自动隐藏: *per-player-idle-hide=15s
   音量归一化: *audio-normalize=false *audio-normalize-target=10dBFS → 0dBFS *audio-normalize-max-gain=99dB → 45dB
   音频波形帧率: *audio-frame-rate=100fps → 60fps
   播放器: wesing (*offset=0ms poll=5ms → 10ms idle-hide=15s audio-normalize=false)
   播放器: cloudmusicv3 (*offset=500ms *poll=40ms → 100ms idle-hide=15s audio-normalize=false *effect-strategy=park → fadeout)
   播放器: qqmusic (offset=250ms poll=5ms → 50ms *idle-hide=0s audio-normalize=false)
   播放器: kugou (offset=250ms poll=5ms → 10ms idle-hide=15s *audio-normalize=true)
   播放器: sodamusic (offset=250ms *poll=30ms → 200ms idle-hide=15s audio-normalize=false)
   播放器: applemusic (*offset=400ms poll=5ms → 50ms idle-hide=15s audio-normalize=false)
   * = 显式设置（config.yml 或命令行）；a → b = 写下 a，实际生效 b
`},
		{"覆盖较多（Windows 11），英文界面", overriddenEchoConfig(t), true, i18n.English, `   Listening: *addr=127.0.0.1:9000
   Global offset & poll: *offset=250ms *poll=5ms → 10ms
   Prior player: *prior-player=[wesing kugou] *prior-player-expire=20s
   Per-player idle auto-hide: *per-player-idle-hide=15s
   Audio normalization: *audio-normalize=false *audio-normalize-target=10dBFS → 0dBFS *audio-normalize-max-gain=99dB → 45dB
   Audio waveform frame rate: *audio-frame-rate=100fps → 60fps
   Player: wesing (*offset=0ms poll=5ms → 10ms idle-hide=15s audio-normalize=false)
   Player: cloudmusicv3 (*offset=500ms *poll=40ms → 100ms idle-hide=15s audio-normalize=false *effect-strategy=park → fadeout)
   Player: qqmusic (offset=250ms poll=5ms → 50ms *idle-hide=0s audio-normalize=false)
   Player: kugou (offset=250ms poll=5ms → 10ms idle-hide=15s *audio-normalize=true)
   Player: sodamusic (offset=250ms *poll=30ms → 200ms idle-hide=15s audio-normalize=false)
   Player: applemusic (*offset=400ms poll=5ms → 50ms idle-hide=15s audio-normalize=false)
   * = set explicitly (config.yml or command line); a → b = written a, in effect b
`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			i18n.SetLanguage(c.lang)
			t.Cleanup(func() { i18n.SetLanguage(i18n.Chinese) })
			if got := strings.Join(bannerLines(&c.cfg, c.win11), ""); got != c.want {
				t.Errorf("横幅配置行\n得：\n%s\n期望：\n%s", got, c.want)
			}
		})
	}
}

// TestBannerListsEveryEchoKey 横幅列出的键集合与 buildConfigEcho 的键集合一致。键集合取自 buildConfigEcho
// 的实际输出，测试里没有键清单：逐个键把写下的值与生效值换成两个哨兵，横幅上必须各恰好出现一次；
// 默认配置下横幅的项数等于键数（没有多列、重复列）；三件套为空时除图例外不列任何东西。
//
// 变异自证（隔离副本，实测）：从 bannerGlobalLines 删掉 audio-frame-rate 并去掉兜底行 → 该键的哨兵不出现，红；
// 播放器行只列 offset 与 poll、或不列只属于该播放器的键（cloudmusicv3-effect-strategy）→ 红；
// 列出后不销账（每个键在兜底行再列一次）→ 项数不符，红；去掉图例行 → 红。
func TestBannerListsEveryEchoKey(t *testing.T) {
	cfg := defaultEchoConfig()
	_, effective, _ := buildConfigEcho(&cfg, echoPlayers, cfg.EffectStrategy)
	keys := effective.Keys()
	if len(keys) == 0 {
		t.Fatal("buildConfigEcho 没有给出任何键，本用例失效")
	}
	for _, k := range keys {
		w, e, _ := buildConfigEcho(&cfg, echoPlayers, cfg.EffectStrategy)
		w.Set(k, "WRITTEN")
		e.Set(k, "EFFECTIVE")
		text := strings.Join(bannerConfigLines(w, e, nil, nil, echoPlayers), "")
		if strings.Count(text, "=WRITTEN") != 1 || strings.Count(text, " → EFFECTIVE") != 1 {
			t.Errorf("%s 写下的值与生效值应各在横幅上出现一次，实际：\n%s", k, text)
		}
	}

	lines := bannerLines(&cfg, false)
	body := strings.Join(lines[:len(lines)-1], "") // 图例行自带等号，不计
	if got := strings.Count(body, "="); got != len(keys) {
		t.Errorf("横幅列了 %d 项，buildConfigEcho 有 %d 个键：\n%s", got, len(keys), body)
	}

	empty := bannerConfigLines(server.NewOrderedMap(), server.NewOrderedMap(), nil, map[string]any{"offset": "300"}, echoPlayers)
	if len(empty) != 1 || empty[0] != bannerLegend {
		t.Errorf("三件套为空时应只有图例一行（被拒原值的记录里有、回显里没有的键也不列），实际：\n%s", strings.Join(empty, ""))
	}
}

// TestBannerPlacesEveryKey buildConfigEcho 现有的键都归在既定的行里（全局各行与每播放器一行），没有落进
// 兜底行；不认识的全局键各占一行列在全局各行之后，不认识的每播放器键列在该播放器那一行末尾，都不丢。
// buildConfigEcho 新增全局键而没有归入 bannerGlobalLines 时，前一条即红。
//
// 变异自证（隔离副本，实测）：从 bannerGlobalLines 删掉 audio-frame-rate（保留兜底行）→ 行数多一，红；
// buildConfigEcho 删掉 audio-frame-rate → 行数少一，红；去掉兜底行 → zz-new 不出现，红；兜底行排到播放器行
// 之后 → 位置不符，红；播放器行不列只属于该播放器的键 → wesing 的 zz 不出现，红。
func TestBannerPlacesEveryKey(t *testing.T) {
	cfg := defaultEchoConfig()
	if got, want := len(bannerLines(&cfg, false)), len(bannerGlobalLines)+len(echoPlayers)+1; got != want {
		t.Errorf("横幅配置行共 %d 行，期望 %d 行（全局 %d 行、播放器 %d 行、图例 1 行）：有键没有归入 bannerGlobalLines",
			got, want, len(bannerGlobalLines), len(echoPlayers))
	}

	w, e, o := buildConfigEcho(&cfg, echoPlayers, cfg.EffectStrategy)
	w.Set("zz-new", 1)
	e.Set("zz-new", 2)
	w.Set("wesing-zz", "a")
	e.Set("wesing-zz", "a")
	lines := bannerConfigLines(w, e, append(o, "zz-new"), nil, echoPlayers)
	wantBannerLines(t, lines,
		"   *zz-new=1 → 2",
		"   播放器: wesing (offset=-200ms poll=30ms idle-hide=0s audio-normalize=true zz=a)")
	if got := lines[len(bannerGlobalLines)]; got != "   *zz-new=1 → 2\n" {
		t.Errorf("不认识的全局键应紧跟在全局各行之后，该位置实为 %q", got)
	}
}

// TestBannerDefaultHasNoMarks 内置默认配置：没有任何显式标记；「→」只出现在三个被播放器层下限抬高的
// poll 上（各播放器继承全局写下的 30），图例在最后一行。
//
// 变异自证（隔离副本，实测）：标记判断取反 → 每项都带 *，红；不显示写下的值（只列生效值）→ 没有箭头，红；
// 写下的值与生效值相同时也写箭头 → 箭头数不符，红；去掉图例行 → 红。
func TestBannerDefaultHasNoMarks(t *testing.T) {
	cfg := defaultEchoConfig()
	lines := bannerLines(&cfg, false)
	if got := lines[len(lines)-1]; got != bannerLegend {
		t.Errorf("最后一行应是图例，实为 %q", got)
	}
	body := strings.Join(lines[:len(lines)-1], "")
	if strings.Contains(body, "*") {
		t.Errorf("默认配置不该有显式标记：\n%s", body)
	}
	if got := strings.Count(body, "→"); got != 3 {
		t.Errorf("默认配置应恰有 3 处「→」（cloudmusicv3、sodamusic、applemusic 的 poll），实得 %d 处：\n%s", got, body)
	}
	wantBannerLines(t, lines,
		playerLine("wesing", "poll=30ms"),
		playerLine("cloudmusicv3", "poll=30ms → 100ms"),
		playerLine("qqmusic", "poll=30ms"),
		playerLine("kugou", "poll=30ms"),
		playerLine("sodamusic", "poll=30ms → 200ms"),
		playerLine("applemusic", "poll=30ms → 50ms"))
}

// TestBannerMarksExactlyTheExplicitKey 每个键分别显式设置：横幅上恰好该键一项带 *。键集合取自
// buildConfigEcho 的实际输出，全局键与每播放器键都在内；带标记的那一项以哨兵值认出。
//
// 变异自证（隔离副本，实测）：标记判断取反 → 红；按横幅上的短名而不是完整键名查标记（offset 显式时六个
// 播放器的 offset 也带 *，每播放器键显式时反而不带）→ 红。
func TestBannerMarksExactlyTheExplicitKey(t *testing.T) {
	base := defaultEchoConfig()
	_, effective, _ := buildConfigEcho(&base, echoPlayers, base.EffectStrategy)
	marked := regexp.MustCompile(`[ (]\*[a-z0-9-]+=MARKED`)
	for _, k := range effective.Keys() {
		cfg := defaultEchoConfig()
		markExplicit(&cfg, k)
		w, e, o := buildConfigEcho(&cfg, echoPlayers, cfg.EffectStrategy)
		if len(o) != 1 || o[0] != k {
			t.Fatalf("%s 标为显式后 config_overwritten=%v，本用例失效", k, o)
		}
		w.Set(k, "MARKED")
		e.Set(k, "MARKED")
		lines := bannerConfigLines(w, e, o, nil, echoPlayers)
		body := strings.Join(lines[:len(lines)-1], "")
		if strings.Count(body, "*") != 1 || !marked.MatchString(body) {
			t.Errorf("%s 显式设置时应恰好该项带 *，实际：\n%s", k, body)
		}
	}
}

// TestBannerPlayerPollFloor 每播放器 poll 低于播放器层下限时列出写下的值与抬高后的生效值；达到下限的、
// 没有播放器层下限的 wesing 与 kugou 只列一个值。全局那一行不受每播放器覆盖影响。
//
// 变异自证（隔离副本，实测）：buildConfigEcho 去掉播放器层下限（poll 不经 playerPollIntervals）→ 低于下限的
// 各例红；横幅只列生效值 → 低于下限的各例红；横幅只列写下的值 → 同上各例红。
func TestBannerPlayerPollFloor(t *testing.T) {
	cases := []struct {
		player string
		poll   int
		want   string
	}{
		{"wesing", 30, "*poll=30ms"},
		{"wesing", 10, "*poll=10ms"},
		{"kugou", 30, "*poll=30ms"},
		{"kugou", 10, "*poll=10ms"},
		{"cloudmusicv3", 30, "*poll=30ms → 100ms"},
		{"cloudmusicv3", 40, "*poll=40ms → 100ms"},
		{"cloudmusicv3", 49, "*poll=49ms → 100ms"},
		{"cloudmusicv3", 50, "*poll=50ms"},
		{"qqmusic", 20, "*poll=20ms → 50ms"},
		{"qqmusic", 29, "*poll=29ms → 50ms"},
		{"qqmusic", 30, "*poll=30ms"},
		{"sodamusic", 30, "*poll=30ms → 200ms"},
		{"sodamusic", 199, "*poll=199ms → 200ms"},
		{"sodamusic", 200, "*poll=200ms"},
		{"sodamusic", 500, "*poll=500ms"},
		{"applemusic", 30, "*poll=30ms → 50ms"},
		{"applemusic", 49, "*poll=49ms → 50ms"},
		{"applemusic", 50, "*poll=50ms"},
	}
	for _, c := range cases {
		cfg := defaultEchoConfig()
		cfg.Players[c.player].Poll = intPtr(c.poll)
		markExplicit(&cfg, c.player+"-poll")
		wantBannerLines(t, bannerLines(&cfg, false),
			playerLine(c.player, c.want),
			"   全局偏移与轮询: offset=-200ms poll=30ms")
	}
}

// TestBannerClampedValues 被配置层钳制的键列出写下的值与钳制后的值；恰在边界上的只列一个值。全局 poll
// 被钳制时，继承它的各播放器列出同一个写下的值与各自的生效值（钳制后再过播放器层下限）；每播放器 poll
// 自身被钳制同理。
//
// 变异自证（隔离副本，实测）：横幅只列生效值 → 越界各例红；横幅只列写下的值 → 越界各例红；
// 箭头两侧对调 → 越界各例红。
func TestBannerClampedValues(t *testing.T) {
	const (
		globalLine = "   全局偏移与轮询: offset=-200ms "
		normLine   = "   音量归一化: audio-normalize=true "
		rateLine   = "   音频波形帧率: "
	)
	cases := []struct {
		key              string
		written, adopted int
		want             []string
	}{
		{"poll", 5, 10, []string{globalLine + "*poll=5ms → 10ms",
			playerLine("wesing", "poll=5ms → 10ms"), playerLine("cloudmusicv3", "poll=5ms → 100ms"),
			playerLine("qqmusic", "poll=5ms → 50ms"), playerLine("kugou", "poll=5ms → 10ms"),
			playerLine("sodamusic", "poll=5ms → 200ms"), playerLine("applemusic", "poll=5ms → 50ms")}},
		{"poll", 5000, 2000, []string{globalLine + "*poll=5000ms → 2000ms",
			playerLine("wesing", "poll=5000ms → 2000ms"), playerLine("cloudmusicv3", "poll=5000ms → 2000ms"),
			playerLine("qqmusic", "poll=5000ms → 2000ms"), playerLine("kugou", "poll=5000ms → 2000ms"),
			playerLine("sodamusic", "poll=5000ms → 2000ms"), playerLine("applemusic", "poll=5000ms → 2000ms")}},
		{"poll", 10, 10, []string{globalLine + "*poll=10ms",
			playerLine("wesing", "poll=10ms"), playerLine("sodamusic", "poll=10ms → 200ms")}},
		{"poll", 2000, 2000, []string{globalLine + "*poll=2000ms",
			playerLine("wesing", "poll=2000ms"), playerLine("sodamusic", "poll=2000ms")}},
		{"audio-frame-rate", 100, 60, []string{rateLine + "*audio-frame-rate=100fps → 60fps"}},
		{"audio-frame-rate", 0, 15, []string{rateLine + "*audio-frame-rate=0fps → 15fps"}},
		{"audio-frame-rate", 60, 60, []string{rateLine + "*audio-frame-rate=60fps"}},
		{"audio-frame-rate", 15, 15, []string{rateLine + "*audio-frame-rate=15fps"}},
		{"audio-normalize-target", 10, 0, []string{normLine + "*audio-normalize-target=10dBFS → 0dBFS audio-normalize-max-gain=30dB"}},
		{"audio-normalize-target", -100, -60, []string{normLine + "*audio-normalize-target=-100dBFS → -60dBFS audio-normalize-max-gain=30dB"}},
		{"audio-normalize-target", 0, 0, []string{normLine + "*audio-normalize-target=0dBFS audio-normalize-max-gain=30dB"}},
		{"audio-normalize-target", -60, -60, []string{normLine + "*audio-normalize-target=-60dBFS audio-normalize-max-gain=30dB"}},
		{"audio-normalize-max-gain", -5, 0, []string{normLine + "audio-normalize-target=-14dBFS *audio-normalize-max-gain=-5dB → 0dB"}},
		{"audio-normalize-max-gain", 99, 45, []string{normLine + "audio-normalize-target=-14dBFS *audio-normalize-max-gain=99dB → 45dB"}},
		{"audio-normalize-max-gain", 0, 0, []string{normLine + "audio-normalize-target=-14dBFS *audio-normalize-max-gain=0dB"}},
		{"audio-normalize-max-gain", 45, 45, []string{normLine + "audio-normalize-target=-14dBFS *audio-normalize-max-gain=45dB"}},
	}
	for _, c := range cases {
		cfg := defaultEchoConfig()
		setGlobalInt(t, &cfg, c.key, c.written, c.adopted)
		wantBannerLines(t, bannerLines(&cfg, false), c.want...)
	}

	// 每播放器 poll 自身越界：kugou-poll 1（钳为 10，无播放器层下限）、sodamusic-poll 5（钳为 10，再抬到 200）、
	// wesing-poll 99999（钳为 2000）。
	cfg := defaultEchoConfig()
	for name, v := range map[string][2]int{"kugou": {1, 10}, "sodamusic": {5, 10}, "wesing": {99999, 2000}} {
		cfg.Players[name].Poll = intPtr(v[1])
		noteWritten(&cfg, name+"-poll", v[0])
		markExplicit(&cfg, name+"-poll")
	}
	wantBannerLines(t, bannerLines(&cfg, false),
		playerLine("kugou", "*poll=1ms → 10ms"),
		playerLine("sodamusic", "*poll=5ms → 200ms"),
		playerLine("wesing", "*poll=99999ms → 2000ms"),
		playerLine("qqmusic", "poll=30ms"))
}

// TestBannerEffectStrategyWin11 写成 park 且判定为 Windows 11 时列出 park → fadeout；非 Windows 11 只列 park；
// 写成 fadeout 或没写时与系统无关。
//
// 变异自证（隔离副本，实测）：effectiveEffectStrategy 不做降级（恒返回配置值）→ park 且 Windows 11 一例红；
// 不看系统、park 一律降级 → park 且非 Windows 11 一例红。main() 把降级后的策略交给 buildConfigEcho、
// 再把同一份三件套交给横幅与 SetServiceInfo 的接线不在本用例之内。
func TestBannerEffectStrategyWin11(t *testing.T) {
	const prefix = "   播放器: cloudmusicv3 (offset=-200ms poll=30ms → 100ms idle-hide=0s audio-normalize=true "
	cases := []struct {
		name     string
		strategy string // 空串 = 没写这个键
		win11    bool
		want     string
	}{
		{"park，Windows 11", "park", true, "*effect-strategy=park → fadeout)"},
		{"park，非 Windows 11", "park", false, "*effect-strategy=park)"},
		{"fadeout，Windows 11", "fadeout", true, "*effect-strategy=fadeout)"},
		{"fadeout，非 Windows 11", "fadeout", false, "*effect-strategy=fadeout)"},
		{"没写，Windows 11", "", true, "effect-strategy=fadeout)"},
		{"没写，非 Windows 11", "", false, "effect-strategy=fadeout)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := defaultEchoConfig()
			if c.strategy != "" {
				cfg.EffectStrategy = c.strategy
				markExplicit(&cfg, "cloudmusicv3-effect-strategy")
			}
			wantBannerLines(t, bannerLines(&cfg, c.win11), prefix+c.want)
		})
	}
}

// loadCase 是一条经真实的 config.Load 驱动的用例：config.yml 文本与命令行参数，横幅里必须原样出现的整行，
// 与横幅里不得出现的片段。
type loadCase struct {
	name  string
	yml   string
	args  []string
	win11 bool
	want  []string
	not   []string
}

// 各行的行首，与 cloudmusicv3 行在默认配置下 effect-strategy 之前的部分。
const (
	lineListen  = "   监听: "
	lineGlobal  = "   全局偏移与轮询: "
	linePrior   = "   优先播放器: "
	lineIdle    = "   per-player 无活跃自动隐藏: "
	lineNorm    = "   音量归一化: "
	lineRate    = "   音频波形帧率: "
	cloudPrefix = "   播放器: cloudmusicv3 (offset=-200ms poll=30ms → 100ms idle-hide=0s audio-normalize=true "
)

// runLoadCases 逐条跑真实的 Load、组装三件套、渲染横幅并断言；另断言三件套各键的类型没有随写法变化
// （wantEchoTypes：被拒的原值只上横幅，不进 /service-status）。
func runLoadCases(t *testing.T, cases []loadCase) {
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := loadConfig(t, c.yml, c.args...)
			w, e, o := buildConfigEcho(&cfg, echoPlayers, effectiveEffectStrategy(cfg.EffectStrategy, c.win11))
			wantEchoTypes(t, c.name, w, e)
			lines := bannerConfigLines(w, e, o, cfg.RejectedValues, echoPlayers)
			wantBannerLines(t, lines, c.want...)
			text := strings.Join(lines[:len(lines)-1], "")
			for _, n := range c.not {
				if strings.Contains(text, n) {
					t.Errorf("横幅不该出现 %s：\n%s", n, text)
				}
			}
		})
	}
}

// TestBannerRejectedValues 写下的值因类型不符或取值非法被拒时，横幅列出原值与生效的默认值，不带 *。
// 类型不符的原值取自 Config.RejectedValues（不进三件套），按字面写（字符串带引号、小数带小数点、列表与映射
// 按 JSON）、不带单位，以区别于被采纳的取值；取值非法但类型相符的（cloudmusicv3-effect-strategy: foo）仍在
// 三件套里，照普通取值写。原值只在写错的那个键自己那一项上列出：全局键写错时播放器行照列生效值，播放器
// 专属键写错时列在该播放器行上。各例以 config.yml 文本经真实的 config.Load 驱动，并逐例断言三件套各键的
// 类型没有变（runLoadCases）。
//
// 变异自证（隔离的 worktree，实测）：
//   - mergeYAML 的 offset、每播放器 offset 分支拒收时不记原值；offset、poll、prior-player（非列表）、
//     audio-frame-rate、每播放器 poll、每播放器 audio-normalize 分支拒收时改记进 WrittenValues；
//     prior-player 丢项时不记原列表或记进 WrittenValues；cloudmusicv3-effect-strategy 的非字符串取值改记进
//     WrittenValues、非法字符串改记进 RejectedValues → 对应各例红。
//   - 横幅不读被拒原值 → 类型不符的各例红；按去掉前缀后的短名查被拒原值（全局键的原值落到各播放器行上、
//     播放器专属键的原值查不到）→「全局键被拒，只在全局那一项上列原值…」与每播放器各例红；原值仍带单位、
//     字面写法不带引号 → 各例红；原值的小数不补小数点 →「取整数值的小数」红；不能按 JSON 写时不退回默认
//     写法 →「不能按 JSON 写的原值…」红。
//   - buildConfigEcho 的 offset、每播放器 offset 的写下值改读被拒原值 → 对应各例红（三件套的类型变了）。
//   - Load 不调 clampPolls、clampAudioNormalize、clampAudioFrameRate 任一 →「反例：越界的整数…」红；
//     Load 不调 loadFile → 结果取决于 config.yml 的各例红；不经 flag.Visit、无条件用命令行变量给 offset
//     赋值 → 没传 -offset 而断言里带 offset 的各例红。
func TestBannerRejectedValues(t *testing.T) {
	runLoadCases(t, []loadCase{
		{name: "字符串写的数字", yml: `audio-frame-rate: "30"`,
			want: []string{lineRate + `audio-frame-rate="30" → 25fps`}},
		{name: "全局键被拒，只在全局那一项上列原值，播放器行照列生效值", yml: `offset: "300"`,
			want: []string{
				lineGlobal + `offset="300" → -200ms poll=30ms`,
				playerLine("wesing", "poll=30ms"),
				playerLine("sodamusic", "poll=30ms → 200ms")},
			not: []string{`(offset="300"`}},
		{name: "小数", yml: `poll: 30.5`,
			want: []string{
				lineGlobal + `offset=-200ms poll=30.5 → 30ms`,
				playerLine("wesing", "poll=30ms"),
				playerLine("sodamusic", "poll=30ms → 200ms")}},
		{name: "取整数值的小数", yml: `offset: 200.0`,
			want: []string{lineGlobal + `offset=200.0 → -200ms poll=30ms`}},
		{name: "布尔写成字符串", yml: `audio-normalize: "false"`,
			want: []string{
				lineNorm + `audio-normalize="false" → true audio-normalize-target=-14dBFS audio-normalize-max-gain=30dB`,
				playerLine("wesing", "poll=30ms")}},
		{name: "布尔写成整数", yml: `audio-normalize: 0`,
			want: []string{lineNorm + `audio-normalize=0 → true audio-normalize-target=-14dBFS audio-normalize-max-gain=30dB`}},
		{name: "整数写成布尔", yml: `prior-player-expire: true`,
			want: []string{linePrior + `prior-player=[wesing] prior-player-expire=true → 15s`}},
		{name: "列表写成标量", yml: `prior-player: kugou`,
			want: []string{linePrior + `prior-player="kugou" → [wesing] prior-player-expire=15s`}},
		{name: "列表里有不是字符串的项", yml: `prior-player: [kugou, 1]`,
			want: []string{linePrior + `*prior-player=["kugou",1] → [kugou] prior-player-expire=15s`}},
		{name: "标量写成列表", yml: `per-player-idle-hide: [15]`,
			want: []string{
				lineIdle + `per-player-idle-hide=[15] → 0s`,
				playerLine("wesing", "poll=30ms")}},
		{name: "标量写成映射", yml: `addr: {host: 127.0.0.1, port: 8765}`,
			want: []string{lineListen + `addr={"host":"127.0.0.1","port":8765} → 0.0.0.0:8765`}},
		{name: "字符串键写成整数", yml: `addr: 8765`,
			want: []string{lineListen + `addr=8765 → 0.0.0.0:8765`}},
		{name: "空值", yml: "audio-normalize-target:\n",
			want: []string{lineNorm + `audio-normalize=true audio-normalize-target=null → -14dBFS audio-normalize-max-gain=30dB`}},
		{name: "不能按 JSON 写的原值退回默认写法", yml: "offset: {1: 2}\npoll: .nan\n",
			want: []string{lineGlobal + `offset=map[1:2] → -200ms poll=NaN → 30ms`}},
		{name: "每播放器键类型不符",
			yml: "wesing-offset: \"0\"\nqqmusic-poll: fast\nsodamusic-poll: \"100\"\nkugou-idle-hide: 1.5\napplemusic-audio-normalize: 0\n",
			want: []string{
				`   播放器: wesing (offset="0" → -200ms poll=30ms idle-hide=0s audio-normalize=true)`,
				`   播放器: qqmusic (offset=-200ms poll="fast" → 30ms idle-hide=0s audio-normalize=true)`,
				`   播放器: kugou (offset=-200ms poll=30ms idle-hide=1.5 → 0s audio-normalize=true)`,
				`   播放器: sodamusic (offset=-200ms poll="100" → 200ms idle-hide=0s audio-normalize=true)`,
				`   播放器: applemusic (offset=-200ms poll=30ms → 50ms idle-hide=0s audio-normalize=0 → true)`,
				playerLine("cloudmusicv3", "poll=30ms → 100ms"),
				lineGlobal + `offset=-200ms poll=30ms`}},
		{name: "全局键与播放器专属键都写错：各在自己那一项上列自己的原值，写对了的播放器不受影响",
			yml: "offset: \"300\"\nkugou-offset: 430\nwesing-offset: abc\n",
			want: []string{
				lineGlobal + `offset="300" → -200ms poll=30ms`,
				`   播放器: kugou (*offset=430ms poll=30ms idle-hide=0s audio-normalize=true)`,
				`   播放器: wesing (offset="abc" → -200ms poll=30ms idle-hide=0s audio-normalize=true)`,
				playerLine("qqmusic", "poll=30ms")}},
		{name: "特效策略取值非法", yml: `cloudmusicv3-effect-strategy: foo`,
			want: []string{cloudPrefix + `effect-strategy=foo → fadeout)`}},
		{name: "特效策略取值非法，Windows 11", yml: `cloudmusicv3-effect-strategy: foo`, win11: true,
			want: []string{cloudPrefix + `effect-strategy=foo → fadeout)`}},
		{name: "特效策略写成空串", yml: `cloudmusicv3-effect-strategy: ""`,
			want: []string{cloudPrefix + `effect-strategy= → fadeout)`}},
		{name: "特效策略写成整数", yml: `cloudmusicv3-effect-strategy: 1`,
			want: []string{cloudPrefix + `effect-strategy=1 → fadeout)`}},
		{name: "特效策略写成列表", yml: `cloudmusicv3-effect-strategy: [park]`,
			want: []string{cloudPrefix + `effect-strategy=["park"] → fadeout)`}},

		{name: "反例：按原样采纳的值带单位与 *，不带引号",
			yml: "addr: \"8765\"\noffset: 300\naudio-normalize: false\naudio-frame-rate: 30\n",
			want: []string{
				lineListen + `*addr=8765`,
				lineGlobal + `*offset=300ms poll=30ms`,
				lineNorm + `*audio-normalize=false audio-normalize-target=-14dBFS audio-normalize-max-gain=30dB`,
				lineRate + `*audio-frame-rate=30fps`},
			not: []string{`"`}},
		{name: "反例：越界的整数是被钳制而不是被拒，两侧都带单位",
			yml: "poll: 5\naudio-normalize-target: 10\naudio-normalize-max-gain: 99\naudio-frame-rate: 100\nkugou-poll: 99999\n",
			want: []string{
				lineGlobal + `offset=-200ms *poll=5ms → 10ms`,
				lineNorm + `audio-normalize=true *audio-normalize-target=10dBFS → 0dBFS *audio-normalize-max-gain=99dB → 45dB`,
				lineRate + `*audio-frame-rate=100fps → 60fps`,
				playerLine("kugou", "*poll=99999ms → 2000ms"),
				playerLine("wesing", "poll=5ms → 10ms")}},
		{name: "反例：没写的键不带箭头", yml: "",
			want: []string{lineRate + `audio-frame-rate=25fps`, lineGlobal + `offset=-200ms poll=30ms`}},
	})
}

// TestBannerCommandLineOverride 命令行覆盖 config.yml：以命令行的值为准，带显式标记。config.yml 里同一个键
// 写下而被拒的原值随之作废，不再列出（它已被更高一层的来源取代，加载时的告警仍在日志里）；命令行覆盖的是
// 别的键时，被拒的原值照列在写错的那个键自己那一项上。命令行的值自己越界或取值非法时，写下的值是命令行的
// 那个。各例以 config.yml 文本与命令行参数经真实的 config.Load 驱动。
//
// 变异自证（隔离的 worktree，实测）：
//   - flag.Visit 不清 config.yml 一层留下的被拒原值（RejectedValues）→「config.yml 类型不符 / 列表项被丢弃、
//     命令行…」六例红；不清 WrittenValues →「特效策略：config.yml 非法、命令行合法」红。
//   - flag.Visit 的 15 个分支（11 个全局键、每播放器 4 个维度）逐个保留 case、删掉赋值 → 各有用例红；
//     flag.Visit 不标记显式 → 红。
//   - 命令行的特效策略非法时不记原值、不撤销显式标记、连 config.yml 的显式标记也撤销 → 对应各例红。
//   - Load 不调 clampPolls →「命令行的值越界」「每播放器 poll 越界」「config.yml 类型不符、命令行的值越界」红。
func TestBannerCommandLineOverride(t *testing.T) {
	runLoadCases(t, []loadCase{
		{name: "命令行覆盖 config.yml 的同一个键，各播放器继承命令行的值", yml: "offset: 200\n", args: []string{"-offset", "350"},
			want: []string{
				lineGlobal + `*offset=350ms poll=30ms`,
				`   播放器: wesing (offset=350ms poll=30ms idle-hide=0s audio-normalize=true)`}},
		// offset 是延迟量，提前写作负值（issue #54）。flag 包对非布尔参数取下一个实参为值，前导的 - 不影响。
		{name: "命令行的负值，值另作一个实参", args: []string{"-offset", "-200", "-kugou-offset", "-430"},
			want: []string{
				lineGlobal + `*offset=-200ms poll=30ms`,
				`   播放器: kugou (*offset=-430ms poll=30ms idle-hide=0s audio-normalize=true)`,
				`   播放器: wesing (offset=-200ms poll=30ms idle-hide=0s audio-normalize=true)`}},
		{name: "命令行的负值，等号写法", args: []string{"-offset=-350", "-kugou-offset=-430"},
			want: []string{
				lineGlobal + `*offset=-350ms poll=30ms`,
				`   播放器: kugou (*offset=-430ms poll=30ms idle-hide=0s audio-normalize=true)`,
				`   播放器: wesing (offset=-350ms poll=30ms idle-hide=0s audio-normalize=true)`}},
		{name: "命令行的正值即延后，不换算", args: []string{"-offset", "200"},
			want: []string{lineGlobal + `*offset=200ms poll=30ms`}},
		{name: "命令行的值越界，写下的值是命令行的", yml: "poll: 100\n", args: []string{"-poll", "5"},
			want: []string{lineGlobal + `offset=-200ms *poll=5ms → 10ms`, playerLine("wesing", "poll=5ms → 10ms")}},
		{name: "每播放器键，0 与 false 照列", args: []string{"-wesing-offset", "0", "-qqmusic-audio-normalize=false"},
			want: []string{
				`   播放器: wesing (*offset=0ms poll=30ms idle-hide=0s audio-normalize=true)`,
				`   播放器: qqmusic (offset=-200ms poll=30ms idle-hide=0s *audio-normalize=false)`}},
		{name: "每播放器 poll 越界", args: []string{"-kugou-poll", "1"},
			want: []string{playerLine("kugou", "*poll=1ms → 10ms")}},
		{name: "清空优先播放器", args: []string{"-prior-player", ""},
			want: []string{linePrior + `*prior-player=[] prior-player-expire=15s`}},
		{name: "其余各键只在命令行给出",
			args: []string{"-addr", "127.0.0.1:9000", "-prior-player", "kugou", "-prior-player-expire", "20", "-per-player-idle-hide", "15",
				"-audio-normalize-target=-20", "-audio-normalize-max-gain", "12", "-kugou-idle-hide", "7", "-sodamusic-poll", "500"},
			want: []string{
				lineListen + `*addr=127.0.0.1:9000`,
				linePrior + `*prior-player=[kugou] *prior-player-expire=20s`,
				lineIdle + `*per-player-idle-hide=15s`,
				lineNorm + `audio-normalize=true *audio-normalize-target=-20dBFS *audio-normalize-max-gain=12dB`,
				`   播放器: kugou (offset=-200ms poll=30ms *idle-hide=7s audio-normalize=true)`,
				`   播放器: sodamusic (offset=-200ms *poll=500ms idle-hide=15s audio-normalize=true)`,
				`   播放器: wesing (offset=-200ms poll=30ms idle-hide=15s audio-normalize=true)`}},
		{name: "特效策略：config.yml 非法、命令行合法", yml: "cloudmusicv3-effect-strategy: foo\n",
			args: []string{"-cloudmusicv3-effect-strategy", "park"},
			want: []string{cloudPrefix + `*effect-strategy=park)`}, not: []string{"foo"}},
		{name: "特效策略：config.yml 合法、命令行非法", yml: "cloudmusicv3-effect-strategy: park\n",
			args: []string{"-cloudmusicv3-effect-strategy", "foo"},
			want: []string{cloudPrefix + `*effect-strategy=foo → park)`}},
		{name: "特效策略：config.yml 合法、命令行非法，Windows 11", yml: "cloudmusicv3-effect-strategy: park\n",
			args: []string{"-cloudmusicv3-effect-strategy", "foo"}, win11: true,
			want: []string{cloudPrefix + `*effect-strategy=foo → fadeout)`}},
		{name: "特效策略：只有命令行、取值非法", args: []string{"-cloudmusicv3-effect-strategy", "foo"},
			want: []string{cloudPrefix + `effect-strategy=foo → fadeout)`}},

		{name: "config.yml 类型不符、命令行给出合法值", yml: "audio-frame-rate: \"30\"\n", args: []string{"-audio-frame-rate", "40"},
			want: []string{lineRate + `*audio-frame-rate=40fps`}, not: []string{`"30"`}},
		{name: "config.yml 类型不符、命令行的值越界", yml: "poll: abc\n", args: []string{"-poll", "5"},
			want: []string{lineGlobal + `offset=-200ms *poll=5ms → 10ms`}, not: []string{"abc"}},
		{name: "config.yml 类型不符、命令行覆盖布尔键", yml: "audio-normalize: \"false\"\n", args: []string{"-audio-normalize=false"},
			want: []string{
				lineNorm + `*audio-normalize=false audio-normalize-target=-14dBFS audio-normalize-max-gain=30dB`,
				`   播放器: wesing (offset=-200ms poll=30ms idle-hide=0s audio-normalize=false)`},
			not: []string{`"false"`}},
		{name: "config.yml 类型不符、命令行覆盖全局键后各播放器继承命令行的值", yml: "offset: \"300\"\n", args: []string{"-offset", "350"},
			want: []string{
				lineGlobal + `*offset=350ms poll=30ms`,
				`   播放器: wesing (offset=350ms poll=30ms idle-hide=0s audio-normalize=true)`},
			not: []string{`"300"`}},
		{name: "config.yml 类型不符、命令行覆盖每播放器键", yml: "wesing-offset: \"0\"\n", args: []string{"-wesing-offset", "50"},
			want: []string{`   播放器: wesing (*offset=50ms poll=30ms idle-hide=0s audio-normalize=true)`}, not: []string{`"0"`}},
		{name: "config.yml 列表项被丢弃、命令行覆盖", yml: "prior-player: [kugou, 1]\n", args: []string{"-prior-player", "wesing,kugou"},
			want: []string{linePrior + `*prior-player=[wesing kugou] prior-player-expire=15s`}, not: []string{`["kugou",1]`}},
		{name: "反例：命令行覆盖的是别的键，被拒的原值照列", yml: "poll: abc\n", args: []string{"-wesing-poll", "50"},
			want: []string{
				lineGlobal + `offset=-200ms poll="abc" → 30ms`,
				playerLine("wesing", "*poll=50ms"),
				playerLine("qqmusic", "poll=30ms")}},
		{name: "反例：命令行覆盖的是全局键，播放器专属键被拒的原值照列", yml: "wesing-offset: abc\n", args: []string{"-offset", "350"},
			want: []string{
				lineGlobal + `*offset=350ms poll=30ms`,
				`   播放器: wesing (offset="abc" → 350ms poll=30ms idle-hide=0s audio-normalize=true)`,
				`   播放器: kugou (offset=350ms poll=30ms idle-hide=0s audio-normalize=true)`}},
	})
}

// TestBannerPlayerInheritAndOverride 每播放器键继承全局值时照列生效值、不带标记；显式覆盖时列覆盖值并带标记，
// 覆盖成 0 与 false 同样照列。
//
// 变异自证（隔离副本，实测）：播放器行只列 offset 与 poll（继承来的 idle-hide、audio-normalize 不列）→ 红；
// 按横幅上的短名查标记 → 继承全局显式值的播放器也带 *，红。
func TestBannerPlayerInheritAndOverride(t *testing.T) {
	cfg := defaultEchoConfig()
	cfg.Offset, cfg.Poll, cfg.PerPlayerIdleHide, cfg.AudioNormalize = 300, 100, 15, false
	cfg.Players["wesing"].Offset = intPtr(0)
	cfg.Players["qqmusic"].IdleHide = intPtr(0)
	cfg.Players["kugou"].AudioNormalize = boolPtr(true)
	cfg.Players["kugou"].Poll = intPtr(500)
	markExplicit(&cfg, "offset", "poll", "per-player-idle-hide", "audio-normalize",
		"wesing-offset", "qqmusic-idle-hide", "kugou-audio-normalize", "kugou-poll")
	wantBannerLines(t, bannerLines(&cfg, false),
		"   全局偏移与轮询: *offset=300ms *poll=100ms",
		"   per-player 无活跃自动隐藏: *per-player-idle-hide=15s",
		"   音量归一化: *audio-normalize=false audio-normalize-target=-14dBFS audio-normalize-max-gain=30dB",
		"   播放器: wesing (*offset=0ms poll=100ms idle-hide=15s audio-normalize=false)",
		"   播放器: cloudmusicv3 (offset=300ms poll=100ms idle-hide=15s audio-normalize=false effect-strategy=fadeout)",
		"   播放器: qqmusic (offset=300ms poll=100ms *idle-hide=0s audio-normalize=false)",
		"   播放器: kugou (offset=300ms *poll=500ms idle-hide=15s *audio-normalize=true)",
		"   播放器: sodamusic (offset=300ms poll=100ms → 200ms idle-hide=15s audio-normalize=false)",
		"   播放器: applemusic (offset=300ms poll=100ms idle-hide=15s audio-normalize=false)")
}

// TestBannerConfigFileLine 配置文件行给出 config.Load 记下的路径，并如实注明加载结果：正常加载不加注，
// 本次新生成、读取失败、解析失败、不存在且生成失败各有说明。三种失败的附注只说 config.yml 未生效，
// 不说本次运行用的是什么配置：命令行参数照常生效（见 TestBannerConfigFileFailureWithCommandLine）。
// 路径与加载结果如何记下由 config/loadfile_test.go 守。
//
// 变异自证（隔离副本，实测）：不列路径 → 各例红；附注表里对调读取失败与解析失败 → 两例红；
// 删掉本次新生成的附注 → 一例红；中文译表缺一条附注 → 该例中文红；附注表里一条照旧写成中文 → 该例英文红。
func TestBannerConfigFileLine(t *testing.T) {
	const path = `C:\PlayerCap\config.yml`
	cases := []struct {
		state  config.FileState
		zh, en string
	}{
		{config.FileLoaded, "", ""},
		{config.FileGenerated, "（本次新生成）", " (newly generated)"},
		{config.FileReadFailed, "（读取失败，config.yml 未生效）", " (read failed; config.yml not applied)"},
		{config.FileParseFailed, "（解析失败，config.yml 未生效）", " (parse failed; config.yml not applied)"},
		{config.FileGenerateFailed, "（不存在且生成失败，config.yml 未生效）", " (not found and could not be created; config.yml not applied)"},
	}
	t.Cleanup(func() { i18n.SetLanguage(i18n.Chinese) })
	for _, c := range cases {
		cfg := defaultEchoConfig()
		cfg.ConfigFile, cfg.ConfigFileState = path, c.state
		i18n.SetLanguage(i18n.Chinese)
		if got, want := bannerConfigFileLine(&cfg), "   配置文件: "+path+c.zh+"\n"; got != want {
			t.Errorf("加载结果 %d\n得 %q\n期望 %q", c.state, got, want)
		}
		i18n.SetLanguage(i18n.English)
		if got, want := bannerConfigFileLine(&cfg), "   Config file: "+path+c.en+"\n"; got != want {
			t.Errorf("加载结果 %d（英文）\n得 %q\n期望 %q", c.state, got, want)
		}
	}
}

// TestBannerConfigFileFailureWithCommandLine config.yml 解析失败而另有命令行参数：配置文件行注明 config.yml
// 未生效，配置行照列命令行的值并带 *（含被钳制的），其余键是内置默认值、不带 *。附注因此不能写成本次运行
// 用的是内置默认配置。经真实的 config.Load 驱动。反例：config.yml 能解析时不加注，config.yml 与命令行的值
// 都在。
//
// 变异自证（隔离的 worktree，实测）：解析失败的附注改回「本次运行使用内置默认配置」→ 红；loadFile 解析
// 失败时不置 FileParseFailed → 红；解析失败后不再应用命令行参数（Load 在 loadFile 失败时提前返回）→ 红。
func TestBannerConfigFileFailureWithCommandLine(t *testing.T) {
	args := []string{"-offset", "350", "-wesing-poll", "5"}

	cfg := loadConfig(t, "poll: [1, 2\n", args...)
	if cfg.ConfigFileState != config.FileParseFailed || filepath.Base(cfg.ConfigFile) != "config.yml" {
		t.Fatalf("加载结果=%d、路径=%q，期望 FileParseFailed 与 config.yml 的路径", cfg.ConfigFileState, cfg.ConfigFile)
	}
	if got, want := bannerConfigFileLine(&cfg), "   配置文件: "+cfg.ConfigFile+"（解析失败，config.yml 未生效）\n"; got != want {
		t.Errorf("配置文件行\n得 %q\n期望 %q", got, want)
	}
	lines := bannerLines(&cfg, false)
	wantBannerLines(t, lines,
		lineGlobal+`*offset=350ms poll=30ms`,
		`   播放器: wesing (offset=350ms *poll=5ms → 10ms idle-hide=0s audio-normalize=true)`,
		`   播放器: kugou (offset=350ms poll=30ms idle-hide=0s audio-normalize=true)`,
		lineRate+`audio-frame-rate=25fps`)
	if got := strings.Count(strings.Join(lines[:len(lines)-1], ""), "*"); got != 2 {
		t.Errorf("只有命令行给出的两个键该带 *，实得 %d 处：\n%s", got, strings.Join(lines, ""))
	}

	cfg = loadConfig(t, "poll: 100\n", args...)
	if got, want := bannerConfigFileLine(&cfg), "   配置文件: "+cfg.ConfigFile+"\n"; got != want {
		t.Errorf("config.yml 能解析时不该加注\n得 %q\n期望 %q", got, want)
	}
	wantBannerLines(t, bannerLines(&cfg, false),
		lineGlobal+`*offset=350ms *poll=100ms`,
		`   播放器: wesing (offset=350ms *poll=5ms → 10ms idle-hide=0s audio-normalize=true)`,
		`   播放器: kugou (offset=350ms poll=100ms idle-hide=0s audio-normalize=true)`)
}
