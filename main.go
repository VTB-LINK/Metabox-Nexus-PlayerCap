package main

import (
	"Metabox-Nexus-PlayerCap/audiocap/helper"
	"Metabox-Nexus-PlayerCap/audiocap/manager"
	"Metabox-Nexus-PlayerCap/clientid"
	"Metabox-Nexus-PlayerCap/config"
	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/logger"
	"Metabox-Nexus-PlayerCap/player/applemusic"
	"Metabox-Nexus-PlayerCap/player/cloudmusic"
	"Metabox-Nexus-PlayerCap/player/cloudmusic/effect"
	"Metabox-Nexus-PlayerCap/player/cloudmusic/park"
	"Metabox-Nexus-PlayerCap/player/kugou"
	"Metabox-Nexus-PlayerCap/player/kugou/watchdog"
	"Metabox-Nexus-PlayerCap/player/qqmusic"
	"Metabox-Nexus-PlayerCap/player/sodamusic"
	"Metabox-Nexus-PlayerCap/player/wesing"
	"Metabox-Nexus-PlayerCap/server"
	"Metabox-Nexus-PlayerCap/taskbar"
	"Metabox-Nexus-PlayerCap/telemetry"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/mod/semver"
)

// 版本信息（编译时通过 -ldflags 注入）
var Version = "0.0.0"

// privacyNoticeWait 是隐私提示的停留时间。够读完那 20 行、又不至于让人觉得程序卡住。
// 只在遥测启用时才会真的等（未注入 DSN 时 PrintPrivacyNotice 立即返回）。
const privacyNoticeWait = 10 * time.Second

var mainLog = logger.New("Main")

func main() {
	// 主 goroutine 的 panic 上报。放在最开头以覆盖尽可能多的路径 —— 它执行在 main 返回时，
	// 那时 telemetry.Init 早已跑过。（Init 之前就 panic 的话不会上报，但语义照旧：进程死。）
	// os.Exit 不跑 defer，所以正常退出路径不受影响。
	defer telemetry.Guard()

	// 提权 helper 模式：由 watchdog.launchElevatedHelper 通过 ShellExecuteExW("runas")
	// 拉起，命令行形如 --kugou-patch-helper <libcefPath>。libcefPath 走命令行参数而非
	// %TEMP% 文件（防 UAC 窗口内被篡改重定向）。在任何初始化之前处理并退出。
	if len(os.Args) >= 3 && os.Args[1] == "--kugou-patch-helper" {
		watchdog.RunPatchHelper(os.Args[2])
		return // RunPatchHelper 内部调用 os.Exit，此 return 仅作为安全保障
	}

	// 音频波形捕获 helper 模式：由主服务通过 exec.Command 拉起，命令行形如 --audio-capture-helper。
	// 直接调用 helper.Run(stdin, stdout) 并退出，绕过一切初始化（隐私提示/更新检查等）。
	if len(os.Args) >= 2 && os.Args[1] == "--audio-capture-helper" {
		helper.Run(os.Stdin, os.Stdout)
		return
	}

	// 设置日志格式
	log.SetFlags(log.Ldate | log.Ltime)

	// 界面语言：只看 Windows 显示语言，中文系统用中文、其余一律英文（规则见 i18n 包）。
	// 必须早于任何用户可见输出（Banner / config 告警 / mainLog）。探测是 Windows-only，
	// 结果注入纯 Go 的 i18n 包，使 logger/ 与 config/ 保持 GOOS=linux 可构建（AGENTS.md §0.3）。
	i18n.SetLanguage(i18n.DetectSystemLanguage())

	// 确保以标准文件名运行。文件名不标准时本进程启动复制品并等它结束，不再往下执行。
	ensureCanonicalName()

	// 清理旧版 WesingCap 残留文件（v1.x → v2.0 迁移）
	cleanupLegacyExe()

	cfg := config.Load(Version)

	// 播放器注册表（新增播放器只需在此追加）
	playerNames := []string{wesing.PlayerName, cloudmusic.PlayerName, qqmusic.PlayerName, kugou.PlayerName, sodamusic.PlayerName, applemusic.PlayerName}

	// 网易云特效最小化策略（config 静态读取）。Win11 下 park 屏外保活失效（DWM 停止合成不可见窗口），
	// 故无论 config 配什么，Win11 一律强制 fadeout。
	//
	// 判定与 buildConfigEcho 都在横幅之前：横幅与 /service-status 用的是下面这同一份三件套，
	// 生效策略须先定下来，横幅才不必另算。
	effectStrategy := effectiveEffectStrategy(cfg.EffectStrategy, park.IsWindows11())
	if effectStrategy != cfg.EffectStrategy {
		mainLog.Warn("Detected Windows 11: park off-screen keep-alive is ineffective on Win11, forcing effect strategy to fadeout")
	}
	configOM, configEffectiveOM, configOverwritten := buildConfigEcho(&cfg, playerNames, effectStrategy)

	fmt.Println()
	fmt.Println(`___    __________________         ______ ___________    ____________`)
	fmt.Println(`__ |  / /___  __/___  __ )        ___  / ____  _/__ |  / /___  ____/`)
	fmt.Println(`__ | / / __  /   __  __  |__________  /   __  /  __ | / / __  __/   `)
	fmt.Println(`__ |/ /  _  /    _  /_/ / _/_____/_  /_____/ /   __ |/ /  _  /___   `)
	fmt.Println(`_____/   /_/     /_____/          /_____//___/   _____/   /_____/   `)
	fmt.Println()
	fmt.Println("===========================================================")
	fmt.Println(i18n.T("VTB-TOOLS Metabox Nexus-PlayerCap Multi-Player Lyrics Push Service "))
	fmt.Println("===========================================================")
	fmt.Printf(i18n.T("   Version: v%s\n"), displayVersion(Version))
	fmt.Print(bannerConfigFileLine(&cfg))
	// 配置行逐键取自上面的三件套；类型不符而被拒的原值不进三件套，由横幅单独列出，见 bannerConfigLines
	for _, line := range bannerConfigLines(configOM, configEffectiveOM, configOverwritten, cfg.RejectedValues, playerNames) {
		fmt.Print(line)
	}
	fmt.Println("===========================================================")

	// 遥测（Sentry）。放在 Banner 之后、checkAndUpdate 之前：自动更新本身就是个故障点
	// （下载/校验失败会 os.Exit(1) 结束进程），它也该在上报范围内。
	//
	// DSN 由 ldflags 注入，空 DSN 自动禁用 —— 本地 go build 与未配 secret 的构建天然不上报，
	// 没有开关配置项。IsRelease 复用自动更新的 semver 判据，dev 构建的非 semver 版本号
	// 会把它送进 development 环境，见 telemetry 包注释。
	telemetry.Init(telemetry.Options{
		Version:     Version,
		IsRelease:   isReleaseVersion(Version),
		Config:      cfg,
		PlayerNames: playerNames,
	})

	// 隐私提示：告诉主播我们上报什么、干什么用，给他 10 秒读完。
	//
	// 位置是刻意的 —— **在 checkAndUpdate 之前**：那一步会联网、可能替换 exe 并重启进程。
	// 提示必须出现在任何联网动作之前，否则「先斩后奏」。
	//
	// 两段文案各归各的开关：遥测段看 DSN 有没有注入，更新段看 isReleaseVersion —— 也就是
	// 这次启动到底会不会去查版本。两段都不适用时它立即返回（不打印也不等待），那正是本地
	// go build 的常态：一个字节都不外发，摆告示是撒谎，而且每次调试都白等 10 秒。
	// 见 telemetry/privacy.go。
	telemetry.PrintPrivacyNotice(privacyNoticeWait, isReleaseVersion(Version))

	// 强制版本检查与自动更新；网关要求时在这里重建 config.yml 并重启（见 rebuildConfigIfForced）
	checkAndUpdate(&cfg)

	// 每日在线心跳。**必须在 checkAndUpdate 之后起** —— 那一步可能替换 exe 并 restartSelf()：
	// 本进程此后只等新进程结束、不再提供服务，在它之前起心跳，这个旧版本进程会在整个运行期里
	// 照常上报在线。它只发请求、绝不做任何更新动作，见 sendHeartbeat。
	startDailyHeartbeat()

	srv := server.NewServer(playerNames)
	// 生效的特效策略在横幅之前已定下（effectiveEffectStrategy，含 Win11 降级）。
	srv.SetDefaultEffectStrategy(effectStrategy)
	// per-player 通道无活跃自动隐藏阈值解析器（issue #47）。Server 不持有 config，按既有单值注入
	// 的思路把 cfg.GetPlayerIdleHide 喂进去（nil=全关；此处恒非 nil，全局默认 0 即全关）。
	srv.SetIdleHideResolver(cfg.GetPlayerIdleHide)

	// 构建接口地址
	scheme := "http"
	wsScheme := "ws"

	// 构建有序端点表：全局管理接口 → 根路径数据接口 → 各播放器专属接口
	endpointsOM := server.NewOrderedMap()
	endpointsOM.Set("health-check", scheme+"://"+cfg.Addr+"/health-check")
	endpointsOM.Set("service-status", scheme+"://"+cfg.Addr+"/service-status")
	endpointsOM.Set("ws", wsScheme+"://"+cfg.Addr+"/ws")
	endpointsOM.Set("audio-ws", wsScheme+"://"+cfg.Addr+"/audio-ws") // 音频波形根端点（跟随活跃播放器），紧随 ws
	endpointsOM.Set("all_lyrics", scheme+"://"+cfg.Addr+"/all_lyrics")
	endpointsOM.Set("lyric_update", scheme+"://"+cfg.Addr+"/lyric_update")
	endpointsOM.Set("status_update", scheme+"://"+cfg.Addr+"/status_update")
	endpointsOM.Set("song_info", scheme+"://"+cfg.Addr+"/song_info")
	endpointsOM.Set("lyric_update-SSE", scheme+"://"+cfg.Addr+"/lyric_update-SSE")
	endpointsOM.Set("song_info-SSE", scheme+"://"+cfg.Addr+"/song_info-SSE")
	for _, pn := range playerNames {
		pe := server.NewOrderedMap()
		pe.Set("ws", wsScheme+"://"+cfg.Addr+"/"+pn+"/ws")
		// 音频波形通道（全播放器端点，与只在 cloudmusicv3 的 effect-ws 不同），归于各自命名空间，紧随 ws：
		pe.Set("audio-ws", wsScheme+"://"+cfg.Addr+"/"+pn+"/audio-ws")
		pe.Set("all_lyrics", scheme+"://"+cfg.Addr+"/"+pn+"/all_lyrics")
		pe.Set("lyric_update", scheme+"://"+cfg.Addr+"/"+pn+"/lyric_update")
		pe.Set("status_update", scheme+"://"+cfg.Addr+"/"+pn+"/status_update")
		pe.Set("song_info", scheme+"://"+cfg.Addr+"/"+pn+"/song_info")
		pe.Set("lyric_update-SSE", scheme+"://"+cfg.Addr+"/"+pn+"/lyric_update-SSE")
		pe.Set("song_info-SSE", scheme+"://"+cfg.Addr+"/"+pn+"/song_info-SSE")
		// 网易云特效镜像通道（仅 cloudmusicv3，归于其命名空间）：
		// 只广告读端 effect-ws；写端 effect-ingest 需内存随机 token，端点仍存在但不对外广告，
		// 注入脚本经 EffectIngestWSURL() 内部取址（含 token），不依赖此表。
		if pn == cloudmusic.PlayerName {
			pe.Set("effect-ws", wsScheme+"://"+cfg.Addr+"/"+pn+"/effect-ws")
		}
		endpointsOM.Set(pn, pe)
	}

	// 配置三件套即横幅所列的那一份（buildConfigEcho 在横幅之前算过，不重算）。
	srv.SetServiceInfo(&server.ServiceInfo{
		Version:           displayVersion(Version),
		Addr:              cfg.Addr,
		Sources:           cfg.Sources,
		Endpoints:         endpointsOM,
		PlayerSupport:     playerNames,
		Config:            configOM,
		ConfigEffective:   configEffectiveOM,
		ConfigOverwritten: configOverwritten,
	})

	// 启动统一服务（等待端口就绪后再启动播放器）
	readyCh := make(chan struct{})
	go func() {
		defer telemetry.Guard()
		if err := srv.Start(cfg.Addr, readyCh); err != nil {
			mainLog.Error("Server startup failed: %v", err)
			os.Exit(1)
		}
	}()
	<-readyCh

	// 创建并启动播放器
	wp := wesing.New(cfg.GetPlayerOffset("wesing"), cfg.GetPlayerPoll("wesing"))
	cp := cloudmusic.New(cfg.GetPlayerOffset("cloudmusicv3"), cfg.GetPlayerPoll("cloudmusicv3"))
	qp := qqmusic.New(cfg.GetPlayerOffset("qqmusic"), cfg.GetPlayerPoll("qqmusic"))
	kp := kugou.New(cfg.GetPlayerOffset("kugou"), cfg.GetPlayerPoll("kugou"))
	sp := sodamusic.New(cfg.GetPlayerOffset("sodamusic"), cfg.GetPlayerPoll("sodamusic"))
	ap := applemusic.New(cfg.GetPlayerOffset("applemusic"), cfg.GetPlayerPoll("applemusic"))

	// 创建路由器
	router := server.NewRouter(&cfg, srv, playerNames)
	router.Register(wp)
	router.Register(cp)
	router.Register(qp)
	router.Register(kp)
	router.Register(sp)
	router.Register(ap)

	// 启动播放器 goroutines
	//
	// 每个都包一层闭包只为挂 defer telemetry.Guard()：`go wp.Start()` 这种形式没有地方挂。
	// Guard 捕获 panic → 上报 → Flush → **原样 re-panic**，进程照常死 —— 采集 goroutine
	// 带着未知的损坏状态继续跑，会把错位歌词一直推上 OBS，比崩掉更坏。
	go func() { defer telemetry.Guard(); wp.Start() }()
	go func() { defer telemetry.Guard(); cp.Start() }()
	go func() { defer telemetry.Guard(); qp.Start() }()
	go func() { defer telemetry.Guard(); kp.Start() }()
	go func() { defer telemetry.Guard(); sp.Start() }()
	go func() { defer telemetry.Guard(); ap.Start() }()

	// 启动时若发现上次崩溃遗留的「网易云屏外泊车」状态，立即还原窗口
	park.RestoreOrphaned()

	// 网易云特效镜像捕获器（按需截帧，有前端订阅 /cloudmusicv3/effect-ws 时才工作）
	// 传 cp.IsConnected 门控生命周期：取词 player 没连上网易云时静默待命，不独立探 9222、不刷屏
	effectCapturer := effect.New(srv, cp.IsConnected)
	go func() { defer telemetry.Guard(); effectCapturer.Run() }()

	// 音频波形捕获管理器（进程级 WASAPI loopback → /audio-ws 扇出）。
	// 位置承重：必须在 checkAndUpdate 之后（正式启动才拉起），照特效捕获器一带——checkAndUpdate 可能
	// os.Exit/换 exe/restartSelf，在它之前起音频（乃至 spawn helper）是错的；且 manager 懒汉，无订阅
	// 不 spawn helper，双保险。manager 内部只查 GetPlayerStatus、扇出帧、Wake 唤醒，不回调 router。
	//
	// 关停：主服务退出走 os.Exit(0)，helper 由 Job Object（KILL_ON_JOB_CLOSE）+ stdin-EOF 自杀回收，
	// 无需在信号处理里显式 Stop（Phase 1 不加）。
	selfExe, _ := os.Executable()
	audioNormResolver := func(p string) manager.NormParams {
		return manager.NormParams{
			Enabled: cfg.GetPlayerAudioNormalize(p),
			Target:  cfg.AudioNormalizeTarget,
			MaxGain: cfg.AudioNormalizeMaxGain,
		}
	}
	audioMgr := manager.New(srv.AudioHub(), srv.GetPlayerStatus, audioNormResolver, cfg.AudioFrameRate, selfExe)
	srv.SetAudioWake(audioMgr.Wake) // 切歌钩子唤醒管理器立即重算门控
	// 本地 recover、绝不挂 telemetry.Guard：音频波形是可降级附加能力，其 goroutine panic 只记日志降级，
	// 绝不 re-panic 杀主进程（Guard 会 re-panic=直播事故）。这是对主服务侧 Guard 范式的有意豁免。
	go func() { defer localRecover("audio-manager"); audioMgr.Run() }()

	// 优雅退出：收到中断信号时，还原网易云被双击隐藏的菜单后再退出
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		defer telemetry.Guard()
		<-sigCh
		mainLog.Info("Received shutdown signal, restoring NetEase Cloud Music window and menu...")
		park.Unpark() // 若处于屏外泊车，先飞回原位
		effectCapturer.RestoreChrome()
		os.Exit(0)
	}()

	mainLog.Info("All players started, routing events...")
	// 本进程是强制重建 config.yml 之后重启出来的：在启动完成处再提示一行，使它留在屏幕的最后几行。
	if backup := os.Getenv(envConfigBackup); backup != "" {
		mainLog.Warn("config.yml was rebuilt from the template of this version during this start; the previous file is kept as %s", backup)
	}

	// 主循环：路由器分发事件（阻塞）
	router.Run()
}

// playerPollIntervals 是各播放器轮询循环所用的同一个下限函数；不在表中的播放器（wesing、kugou）
// 没有播放器层下限，实际间隔即配置层钳制后的值。播放器新增或去掉下限时同改此表。
var playerPollIntervals = map[string]func(pollMs int) time.Duration{
	cloudmusic.PlayerName: cloudmusic.EffectivePollInterval,
	qqmusic.PlayerName:    qqmusic.EffectivePollInterval,
	sodamusic.PlayerName:  sodamusic.EffectivePollInterval,
	applemusic.PlayerName: applemusic.EffectivePollInterval,
}

// effectiveEffectStrategy 返回网易云特效最小化策略的生效值：Windows 11 上 park 屏外保活失效，一律降为
// fadeout，其余原样。isWin11 由调用方传入（main 传 park.IsWindows11()），测试里注入。
func effectiveEffectStrategy(configured string, isWin11 bool) string {
	if configured == "park" && isWin11 {
		return "fadeout"
	}
	return configured
}

// buildConfigEcho 组装配置三件套，/service-status 与启动横幅（bannerConfigLines）共用同一份结果。
//
// config 是配置写下的值：按「播放器覆盖 → 全局 → 内置默认」解析继承，不含任何钳制或校验回退；
// effective 是运行时实际生效的值：计入配置层钳制、校验回退、播放器层轮询下限，以及传入的
// effectStrategy（effectiveEffectStrategy 对 cfg.EffectStrategy 做 Win11 降级后的结果）。
// 两表键集合与顺序相同：全局键按 config.yml 顺序，随后每个播放器（注册顺序）四个键。overwritten
// 按同一键序列出显式设置且被采纳的键（ExplicitKeys）。
//
// 每个键在两表里的类型恒定，不随 config.yml 的写法变化——/service-status 的对外约定
// （configecho_test.go 的 TestConfigEchoTypesNeverChange）。类型不符而被拒的值因此不在此列：对三件套
// 而言该键视同未设置，原值记在 cfg.RejectedValues，由横幅单独列出。别从 RejectedValues 往这里取值。
func buildConfigEcho(cfg *config.Config, playerNames []string, effectStrategy string) (written, effective *server.OrderedMap, overwritten []string) {
	written, effective, overwritten = server.NewOrderedMap(), server.NewOrderedMap(), []string{}
	set := func(key string, w, e any) {
		written.Set(key, w)
		effective.Set(key, e)
		if cfg.ExplicitKeys[key] {
			overwritten = append(overwritten, key)
		}
	}
	same := func(key string, v any) { set(key, v, v) }

	same("addr", cfg.Addr)
	same("offset", cfg.Offset)
	set("poll", cfg.Written("poll", cfg.Poll), cfg.Poll)
	prior := cfg.PriorPlayer
	if prior == nil {
		prior = []string{} // `prior-player: []` 与 `-prior-player ""` 得到 nil，回显须为 [] 而非 null
	}
	same("prior-player", prior)
	same("prior-player-expire", cfg.PriorPlayerExpire)
	same("per-player-idle-hide", cfg.PerPlayerIdleHide)
	same("audio-normalize", cfg.AudioNormalize)
	set("audio-normalize-target", cfg.Written("audio-normalize-target", cfg.AudioNormalizeTarget), cfg.AudioNormalizeTarget)
	set("audio-normalize-max-gain", cfg.Written("audio-normalize-max-gain", cfg.AudioNormalizeMaxGain), cfg.AudioNormalizeMaxGain)
	set("audio-frame-rate", cfg.Written("audio-frame-rate", cfg.AudioFrameRate), cfg.AudioFrameRate)
	set("cloudmusicv3-effect-strategy", cfg.Written("cloudmusicv3-effect-strategy", cfg.EffectStrategy), effectStrategy)
	for _, name := range playerNames {
		same(name+"-offset", cfg.GetPlayerOffset(name))
		poll := cfg.GetPlayerPoll(name)
		if f := playerPollIntervals[name]; f != nil {
			poll = int(f(poll).Milliseconds())
		}
		set(name+"-poll", cfg.WrittenPlayerPoll(name), poll)
		same(name+"-idle-hide", cfg.GetPlayerIdleHide(name))
		same(name+"-audio-normalize", cfg.GetPlayerAudioNormalize(name))
	}
	return written, effective, overwritten
}

// localRecover 是音频子系统 goroutine 的本地兜底：panic 只记日志降级，绝不 re-panic 杀主进程。
// 这是对主服务侧「go func(){ defer telemetry.Guard() }」范式的有意豁免——音频波形是可降级的附加
// 能力，其 goroutine 崩溃不该拖垮歌词/特效主线（telemetry.Guard 会 re-panic）。见计划启动时序条与 §1。
func localRecover(label string) {
	if r := recover(); r != nil {
		mainLog.Warn("[audio] %s panic recovered locally: %v", label, r)
	}
}

// ============================================================================
// 版本检查与自动更新
// ============================================================================

const versionCheckURL = "https://gateway.vtb.link/vtb-tools/metabox/nexus/playercap/v2/client-version"

// versionCheckEndpoint 是版本检查请求实际发往的地址，恒为 versionCheckURL；测试换成本地的 httptest 服务。
var versionCheckEndpoint = versionCheckURL

// ---------------------------------------------------------------------------
// 版本检查请求的客户端标注
// ---------------------------------------------------------------------------
//
// 这个请求过去不带任何身份，网关侧只能按源 IP 计数 —— 主播基本都在 NAT / 动态 IP 后面，
// 同一台机器换 IP 算成多台、同一出口的多台算成一台，DAU/MAU 两头都不准。
// 现在带上匿名设备标识与客户端环境，让网关能去重、能按版本切分。见 clientid 包注释。
//
// **标识走请求头，绝不走 query。** 这是面向将来的约束，不是对现状的描述：2026-08-10 实测
// 这条 route 上没有生效的 proxy-cache，配套的 purge workflow 也已停用（AGENTS.md §3.5、§12），
// 别把 purge workflow 的存在当成缓存存在的证据。一旦网关启用 proxy-cache，其默认缓存键是
// 「方法 + URI + query」、不含请求头：标识拼进 query 会让缓存键按客户端打散，缓存等于没开；
// 走请求头则不参与默认缓存键，没有这个副作用。

var (
	clientEnvOnce sync.Once
	clientEnvBase clientid.Env
)

// clientEnvFor 返回本次请求要带的客户端环境；ping 区分「启动」与「每日在线」。
//
// 系统信息采一次就够 —— 它在进程生命周期内不变，而心跳会按天反复取它。
func clientEnvFor(ping string) clientid.Env {
	clientEnvOnce.Do(func() {
		osVersion, edition, arch := telemetry.OSSummary()
		clientEnvBase = clientid.Env{
			Version:   Version,
			OSVersion: osVersion,
			OSEdition: edition,
			Arch:      arch,
		}
	})

	env := clientEnvBase
	env.Ping = ping
	return env
}

// newVersionCheckRequest 组装版本检查请求（启动与心跳共用同一个 URL 与同一套头）。
//
// 共用 URL 是有意的：网关侧不需要为心跳单独配一条 route 就能开始统计，
// 「今天在线」= 两种时机去重，「今天启动了多少次」= 只数 X-Client-Ping: start。
// 将来真要分开看，再在网关侧按这个头拆，客户端不用动。
func newVersionCheckRequest(ping string) (*http.Request, error) {
	req, err := http.NewRequest(http.MethodGet, versionCheckEndpoint, nil)
	if err != nil {
		return nil, err
	}
	clientid.Apply(req, clientEnvFor(ping))
	return req, nil
}

type releaseInfo struct {
	TagName string `json:"tag_name"`
	Name    string `json:"name"`
	// ConfigForceUpdate 是强制重建 config.yml 的门槛：带 v 前缀的版本号，低于它的配置须按模板重建。
	// 缺失、为空串或不是合法版本号即不强制。只在启动时的版本检查里读（rebuildConfigIfForced）。
	ConfigForceUpdate string `json:"config_force_update"`
	GlobalCDN         string `json:"global_cdn_download_url_prefix"`
	ChinaCDN          string `json:"china_cdn_download_url_prefix"`
	Assets            []struct {
		Name   string `json:"name"`
		Size   int64  `json:"size"`
		Digest string `json:"digest"`
	} `json:"assets"`
}

// checkAndUpdate 做启动时的版本检查：取得网关的响应，先按 config_force_update 判定是否重建 config.yml
// （rebuildConfigIfForced），再按版本决定是否更新（updateIfNeeded）。cfg 是 config.Load 的结果。
//
// **判定须在是否更新的分支之前**，凡拿到响应的进程都判一次。进程若先盖了 config.yml 的版本号、接着去
// 更新而不判定，下一个进程读到的文件版本就是它的盖章值，原先的版本信息丢失，门槛再也不成立。
// 判定用的是 cfg 里 Load 在改写之前记下的文件版本，不重读文件。
//
// 拿不到响应（非发布版本、连不上网关、响应非 200 或解析失败）时不重建，只有 Load 里的自动迁移。
func checkAndUpdate(cfg *config.Config) {
	if !isReleaseVersion(Version) {
		mainLog.Info("Current version (%s) is not an auto-updatable release version, skipping update check", displayVersion(Version))
		return
	}

	cleanupOldExe()
	mainLog.Info("Checking for version updates...")

	req, err := newVersionCheckRequest(clientid.PingStart)
	if err != nil {
		mainLog.Warn("Failed to assemble version-check request: %v, continuing", err)
		return
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		// 连不上网关：标红提示几秒后清除，避免永久卡红。这条路径不重启、要继续跑服务，故
		// clear 后 detach 释放 COM 与被锁的 OS 线程，不让 consoleInit 的 LockOSThread 泄漏进
		// server 阶段。sleep 阻塞启动几秒只发生在连不上这一异常路径，OBS 此刻还没在拉流。
		taskbar.Init()
		taskbar.Error()
		time.Sleep(4 * time.Second)
		taskbar.Clear()
		taskbar.Detach()
		mainLog.Warn("Version check failed: %v, continuing", err)
		return
	}
	defer resp.Body.Close()

	// 拿到响应就算「今天已经报过一次在线」，与状态码无关 —— 网关那边这条请求已经落进
	// 访问日志、已经被计数了。只有连都没连上（上面那个 return）才算今天还没报过，
	// 那种情况下留给心跳去补。
	markPinged(time.Now())

	if resp.StatusCode != http.StatusOK {
		mainLog.Warn("Version check returned %d, continuing", resp.StatusCode)
		return
	}

	var release releaseInfo
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		mainLog.Warn("Failed to parse version info: %v, continuing", err)
		return
	}

	rebuilt := rebuildConfigIfForced(cfg, release.ConfigForceUpdate)
	// 要更新的在这里更新并重启（restartSelf 不返回），由更新后的新进程读到重建好的 config.yml。
	updateIfNeeded(release)
	if rebuilt {
		// 不更新而刚重建过：本进程加载的仍是旧配置，横幅也已按旧配置打印，重启以加载新文件。
		// restartSelf 要等新进程结束才退出：先释放任务栏后端占用的 COM 与被锁的 OS 线程。
		taskbar.Detach()
		restartForRebuild()
	}
}

// updateIfNeeded 按网关响应决定是否更新：要更新则下载、替换并经 restartSelf 重启，不返回；
// 不更新、或没有可用的更新文件时返回。
func updateIfNeeded(release releaseInfo) {
	shouldUpdate, reason, err := decideUpdate(Version, release.TagName, release.Name)
	if err != nil {
		mainLog.Warn("Invalid version info: %v, continuing", err)
		return
	}

	if !shouldUpdate {
		switch reason {
		case updateReasonSameVersion:
			mainLog.Success("Already on the latest version v%s", displayVersion(Version))
		case updateReasonOlderTargetBlocked:
			mainLog.Info("Detected lower version v%s, release title does not end with -force, skipping sync", displayVersion(release.TagName))
		default:
			mainLog.Success("Current version v%s needs no update", displayVersion(Version))
		}
		return
	}

	if reason == updateReasonOlderTargetForced {
		mainLog.Warn("Detected forced version sync: v%s -> v%s", displayVersion(Version), displayVersion(release.TagName))
	}

	if len(release.Assets) == 0 {
		mainLog.Warn("No available update files found, continuing")
		return
	}

	fmt.Println()
	fmt.Println("╔═════════════════════════════════════════════════════════╗")
	fmt.Printf(i18n.T("║  🆕 New version available: v%s → v%s\n"), displayVersion(Version), displayVersion(release.TagName))
	fmt.Printf(i18n.T("║  📦 %d file(s) to update\n"), len(release.Assets))
	fmt.Println(i18n.T("║  Updating automatically..."))
	fmt.Println("╚═════════════════════════════════════════════════════════╝")
	fmt.Println()

	sortedAssets := make([]struct {
		Name   string `json:"name"`
		Size   int64  `json:"size"`
		Digest string `json:"digest"`
	}, 0, len(release.Assets))
	var exeTestFile string
	for _, a := range release.Assets {
		if strings.HasSuffix(strings.ToLower(a.Name), ".exe") {
			sortedAssets = append([]struct {
				Name   string `json:"name"`
				Size   int64  `json:"size"`
				Digest string `json:"digest"`
			}{a}, sortedAssets...)
			exeTestFile = a.Name
		} else {
			sortedAssets = append(sortedAssets, a)
		}
	}
	if exeTestFile == "" {
		exeTestFile = sortedAssets[0].Name
	}

	cdnPrefix := pickFastestCDNPrefix(release.GlobalCDN, release.ChinaCDN, release.TagName, exeTestFile)

	taskbar.Init() // 确定要下载后再探测终端；老 conhost 会锁定当前 OS 线程并建立 COM

	if err := performUpdateAll(cdnPrefix, release.TagName, sortedAssets); err != nil {
		taskbar.Error()
		manualURL := release.ChinaCDN + release.TagName + "/"
		mainLog.Error("Auto-update failed: %v", err)
		mainLog.Warn("Current version is outdated, please download the latest version manually:")
		mainLog.Warn("%s", manualURL)
		fmt.Println(i18n.T("\nPress Enter to exit..."))
		fmt.Scanln()
		os.Exit(1)
	}

	taskbar.Warning()
	taskbar.Flash()
	mainLog.Success("All updates complete! The program will restart automatically...")
	// 金色醒目显示几秒后清除再重启：既不像 1s 那样一闪而过，也不会永久卡在金色。
	time.Sleep(4 * time.Second)
	taskbar.Clear()
	// restartSelf 要等新进程结束才退出，本进程还会存活整个运行期：先释放 COM 与被锁的 OS 线程。
	taskbar.Detach()
	restartSelf()
}

// ============================================================================
// 每日在线心跳
// ============================================================================
//
// 版本检查只在启动时发一次。网关按它计 DAU 会漏掉长期挂机的机器 —— 主播连播三天，
// 只有第一天算活跃。心跳补上这个缺口。
//
// # 它绝不做更新动作
//
// 心跳只发请求、丢弃响应体，**不解析 release、不下载、不替换 exe、不 restartSelf**。
// 自动更新只允许发生在启动时，那时 OBS 还没在拉流；直播进行到一半把 exe 换掉并重启进程
// 就是直播事故（AGENTS.md §0.1）。
// ✅ 有门禁：heartbeatinert_test.go（AST 扫心跳函数体，禁止出现更新/退出相关调用）
//
// # 为什么按日历日判据，而不是 24h ticker
//
// ticker 走单调时钟。机器休眠/挂起期间它是否推进依系统而定，一台每天休眠十小时的播控机
// 会让 24 小时周期不断向后漂，漂满一天就会整天不发 —— 而这种漏报只体现为 DAU 曲线偏低，
// 没有任何报错，不会有人去查。跨日判据只看本地日期，休眠多久都不会跳过下一天。
//
// 唤醒间隔 1 小时是两头折中：它把「跨过零点后多久才报到」的延迟压在 1 小时内，
// 同时让各台机器按各自的启动时刻分散在这一小时里，而不是全体在 00:00 撞上去。

const (
	// heartbeatWakeInterval 是**唤醒**间隔，不是发送间隔。见上文。
	heartbeatWakeInterval = time.Hour

	// heartbeatTimeout 与启动时那次版本检查取同一个值：它打的是同一个端点。
	heartbeatTimeout = 10 * time.Second

	// pingDateLayout 是判「今天报过没有」用的本地日历日。
	pingDateLayout = "2006-01-02"
)

var (
	pingMu sync.Mutex
	// lastPingDate 是最近一次**成功发出**请求的本地日期。空串表示本次运行还一次都没发出去
	// （例如启动时断网），此时下一个唤醒点就会补发。
	lastPingDate string
)

func markPinged(now time.Time) {
	pingMu.Lock()
	defer pingMu.Unlock()
	lastPingDate = now.Format(pingDateLayout)
}

// shouldPing 报告「今天还没报过」。
func shouldPing(now time.Time) bool {
	pingMu.Lock()
	defer pingMu.Unlock()
	return lastPingDate != now.Format(pingDateLayout)
}

// startDailyHeartbeat 起后台心跳。非发布版本直接不起 —— 判据与 checkAndUpdate 一致，
// 否则开发机上每跑一次调试都会往网关灌一条假的「在线」。
func startDailyHeartbeat() {
	if !isReleaseVersion(Version) {
		return
	}

	go func() {
		defer telemetry.Guard()

		ticker := time.NewTicker(heartbeatWakeInterval)
		defer ticker.Stop()

		for range ticker.C {
			if shouldPing(time.Now()) {
				sendHeartbeat()
			}
		}
	}()
}

// sendHeartbeat 发一次在线心跳，**只发不做**。见本节开头。
//
// 失败只记 Detail 不记 Warn：这条路径对主播没有任何价值，网关不通也不影响任何功能，
// 用 [!] 刷屏只会淹没真正要看的东西。
func sendHeartbeat() {
	req, err := newVersionCheckRequest(clientid.PingDaily)
	if err != nil {
		mainLog.Detail("Failed to assemble online heartbeat: %v", err)
		return
	}

	client := &http.Client{Timeout: heartbeatTimeout}
	resp, err := client.Do(req)
	if err != nil {
		mainLog.Detail("Failed to send online heartbeat: %v", err)
		return
	}
	defer resp.Body.Close()

	// 读干净才能让连接回到池里复用。响应内容本身**故意不解析**。
	io.Copy(io.Discard, resp.Body)

	markPinged(time.Now())
	mainLog.Detail("Online heartbeat sent")
}

func performUpdateAll(cdnPrefix, tagName string, assets []struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	Digest string `json:"digest"`
}) error {
	exePath, err := os.Executable()
	if err != nil {
		return i18n.Errorf("failed to get program path: %v", err)
	}
	exeDir := filepath.Dir(exePath)
	exeBase := filepath.Base(exePath)
	client := &http.Client{Timeout: 5 * time.Minute}

	for i, asset := range assets {
		downloadURL := cdnPrefix + tagName + "/" + asset.Name
		targetPath := filepath.Join(exeDir, asset.Name)
		isExe := strings.EqualFold(asset.Name, exeBase) ||
			strings.HasSuffix(strings.ToLower(asset.Name), ".exe")

		mainLog.Info("[%d/%d] %s", i+1, len(assets), asset.Name)
		mainLog.Info("Downloading: %s", downloadURL)

		tmpPath := targetPath + ".new"
		if err := downloadFile(client, downloadURL, tmpPath, asset.Size, asset.Digest); err != nil {
			return i18n.Errorf("failed to download %s: %v", asset.Name, err)
		}

		if isExe {
			oldPath := updateBackupPath(exePath)
			if err := os.Rename(exePath, oldPath); err != nil {
				os.Remove(tmpPath)
				return i18n.Errorf("failed to replace %s (rename): %v", asset.Name, err)
			}
			if err := renameWithRetry(tmpPath, exePath); err != nil {
				os.Rename(oldPath, exePath)
				return i18n.Errorf("failed to replace %s: %v", asset.Name, err)
			}
			mainLog.Success("Replaced: %s", asset.Name)
		} else {
			os.Remove(targetPath)
			if err := renameWithRetry(tmpPath, targetPath); err != nil {
				os.Remove(tmpPath)
				return i18n.Errorf("failed to place %s: %v", asset.Name, err)
			}
			mainLog.Success("Updated: %s", asset.Name)
		}
	}
	return nil
}

func downloadFile(client *http.Client, url, destPath string, expectedSize int64, expectedDigest string) error {
	resp, err := client.Get(url)
	if err != nil {
		return i18n.Errorf("connection failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	totalSize := resp.ContentLength
	if totalSize <= 0 {
		totalSize = expectedSize
	}

	out, err := os.Create(destPath)
	if err != nil {
		return i18n.Errorf("failed to create file: %v", err)
	}

	hasher := sha256.New()
	pr := &progressWriter{total: totalSize}
	if totalSize > 0 {
		taskbar.Progress(0) // 每个文件从 0 重新起跑（文件数不固定，逐个跑满进度条）
	}
	written, err := io.Copy(out, io.TeeReader(resp.Body, io.MultiWriter(hasher, pr)))
	out.Sync()
	out.Close()
	if err != nil {
		os.Remove(destPath)
		return i18n.Errorf("download interrupted: %v", err)
	}

	if expectedDigest != "" {
		actualHash := "sha256:" + hex.EncodeToString(hasher.Sum(nil))
		if actualHash != expectedDigest {
			os.Remove(destPath)
			return i18n.Errorf("SHA256 verification failed: expected %s, got %s", expectedDigest, actualHash)
		}
		mainLog.Success("SHA256 verification succeeded")
	}

	mainLog.Success("Download complete (%.1f MB)", float64(written)/1024/1024)
	return nil
}

func renameWithRetry(src, dst string) error {
	var err error
	for i := 0; i < 5; i++ {
		err = os.Rename(src, dst)
		if err == nil {
			return nil
		}
		mainLog.Info("File is in use, waiting for release (%d/5)...", i+1)
		time.Sleep(1 * time.Second)
	}
	return err
}

type progressWriter struct {
	total   int64
	written int64
	lastPct int
}

func (pw *progressWriter) Write(p []byte) (int, error) {
	n := len(p)
	pw.written += int64(n)
	if pw.total > 0 {
		pct := int(pw.written * 100 / pw.total)
		if pct != pw.lastPct {
			fmt.Printf(i18n.T("\r[*] Download: %d%% (%.1f/%.1f MB)"), pct,
				float64(pw.written)/1024/1024, float64(pw.total)/1024/1024)
			taskbar.Progress(pct)
			pw.lastPct = pct
		}
	}
	return n, nil
}

// cleanupOldExe 删除以前的更新留下的旧 exe（范围见 removeUpdateBackups），失败不报。
//
// 更新后的那一次运行里删不掉：旧版本进程的映像就是备份文件，它在 restartSelf 里等本进程结束。
// 留到下一次启动时清理。
func cleanupOldExe() {
	exePath, err := os.Executable()
	if err != nil {
		return
	}
	removeUpdateBackups(exePath)
}

// updateBackupPath 返回 performUpdateAll 把正在运行的 exe 改名过去的备份路径。通常是 exePath 加
// .old，上一次更新留下的那个先删掉。删不掉且文件仍在时，改用 exePath 加 .old.<本进程号>。
//
// 删不掉的情形：上一次更新后的旧进程还在 restartSelf 里等本进程，.old 就是它的映像。本进程此时
// 再更新（旧进程下载期间又发布了新版本），改名的目标若仍是 .old 会因目标被占用而失败，更新失败、
// 服务不启动。
//
// 带进程号的名字上若有残留不必先删：持有这个进程号的只能是本进程，残留不会是别的进程的映像；
// os.Rename 在 Windows 上是 MoveFileEx 加 MOVEFILE_REPLACE_EXISTING（internal/syscall/windows 的
// Rename），直接盖掉。
func updateBackupPath(exePath string) string {
	oldPath := exePath + ".old"
	os.Remove(oldPath)
	if _, err := os.Lstat(oldPath); os.IsNotExist(err) {
		return oldPath
	}
	return fmt.Sprintf("%s.%d", oldPath, os.Getpid())
}

// removeUpdateBackups 删除 exePath 的更新备份，失败不报，删不掉的留着、继续删其余的。
//
// 范围是 updateBackupPath 产生的两种名字：exePath 加 .old；与 exe 同目录、名字为 exe 名加
// 「.old.」加非空的纯 ASCII 十进制数字、且不是目录的条目，exe 名不分大小写。名字相近的
// （.older、.old.txt、.old.12a 等）不动。
func removeUpdateBackups(exePath string) {
	os.Remove(exePath + ".old")

	dir, prefix := filepath.Dir(exePath), filepath.Base(exePath)+".old."
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || len(name) <= len(prefix) || !strings.EqualFold(name[:len(prefix)], prefix) {
			continue
		}
		if strings.Trim(name[len(prefix):], "0123456789") == "" {
			os.Remove(filepath.Join(dir, name))
		}
	}
}

// cleanupLegacyExe 清理旧版 Metabox-Nexus-WesingCap 的残留文件（v1.x → v2.0 一次性迁移）
func cleanupLegacyExe() {
	exePath, err := os.Executable()
	if err != nil {
		return
	}
	currentName := filepath.Base(exePath)
	if !strings.EqualFold(currentName, canonicalExeName) {
		return
	}
	dir := filepath.Dir(exePath)

	// 收集需要清理的文件
	var targets []string
	for _, name := range []string{legacyExeName, legacyExeName + ".old"} {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			targets = append(targets, p)
		}
	}
	if len(targets) == 0 {
		return
	}

	// 后台重试删除：父进程（旧 exe）退出可能有短暂延迟，文件仍被锁定。
	// 父进程不等待本进程、启动后即退出，是这里删得掉的前提，见 ensureCanonicalName 的例外。
	go func() {
		defer telemetry.Guard()
		for attempt := 0; attempt < 10; attempt++ {
			var remaining []string
			for _, p := range targets {
				if err := os.Remove(p); err != nil {
					remaining = append(remaining, p)
				}
			}
			if len(remaining) == 0 {
				mainLog.Info("Cleaned up legacy WesingCap residual files")
				return
			}
			targets = remaining
			time.Sleep(1 * time.Second)
		}
	}()
}

// restartSelf 在 performUpdateAll 替换 exe 之后启动新版本并等它结束，以它的退出码退出
// （relaunchSelf）；取不到自身路径或启动失败时报错，以 1 退出。不返回。
//
// 本进程的映像此时是 updateBackupPath 选定的备份文件，等待期间它一直被占用：新进程的
// cleanupOldExe 删不掉它，要到下一次启动才清理；新进程自己又要更新时，备份改用别的名字。
func restartSelf() {
	exePath, err := os.Executable()
	if err == nil {
		err = relaunchSelf(exePath)
	}
	mainLog.Error("Unable to restart automatically: %v, please restart the program manually", err)
	os.Exit(1)
}

// selfCommand 组装以 args 为参数、沿用本进程三个标准流运行 exePath 的命令。
func selfCommand(exePath string, args []string) *exec.Cmd {
	cmd := exec.Command(exePath, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd
}

// runAndWait 以 args 为参数启动 exePath 并等它结束，返回它的退出码。启动失败时返回 error，
// 此时没有子进程在跑。
//
// 等待期间接住中断（Ctrl+C、Ctrl+Break）而不处理。同一控制台里的进程都会收到 Ctrl+C；子进程
// 有自己的退出流程（main 末尾的信号处理），本进程若先按默认处理结束，终端回到提示符时子进程
// 还在收尾。
//
// 必须用 signal.Notify，signal.Ignore 在 Windows 上挡不住：换成 Ignore 或什么都不注册，本进程
// 在 CTRL_BREAK 下都以 0xC000013A 结束（relaunchwait_test.go 的 TestRelaunchSelfOutlivesCtrlBreak，
// 实测）。原因见 Go 1.26.1 的运行时源码：
//   - runtime.ctrlHandler 把 CTRL_C / CTRL_BREAK 映射为 SIGINT，把 CTRL_CLOSE / CTRL_LOGOFF /
//     CTRL_SHUTDOWN 映射为 SIGTERM；仅当 runtime.sigsend 返回 true 才向系统报告已处理，否则
//     交给系统默认处理。
//   - sigsend 只看 sig.wanted：signal.Notify 经 runtime.signal_enable 置位，signal.Ignore 经
//     runtime.signal_ignore 清除它；Windows 上 runtime.sigignore 是空函数，ctrlHandler 也不读
//     sig.ignored。
//
// 不接 syscall.SIGTERM（关窗、注销、关机）：控制台随之消失，本进程按默认处理立即结束即可，子进程
// 照常走自己的退出流程。接住的话 ctrlHandler 调 block、不再返回，本进程要到系统超时才被结束；
// 系统按什么顺序通知同一控制台里的各进程、是否等前一个结束再通知下一个，未核实，不让子进程的
// 退出依赖它。
//
// 返回前 signal.Stop（经 runtime.signal_disable 清除 sig.wanted）：启动失败时调用方以当前进程
// 继续运行，Ctrl+C 须恢复默认处理。
func runAndWait(exePath string, args []string) (int, error) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	defer signal.Stop(sigCh)

	cmd := selfCommand(exePath, args)
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	// 非零退出码不算错误，从 ProcessState 取。等待本身失败时 ProcessState 为 nil，ExitCode 返回 -1。
	cmd.Wait()
	return cmd.ProcessState.ExitCode(), nil
}

// relaunchSelf 以本进程的命令行参数运行 exePath，等它结束后以它的退出码退出本进程。
// 只在启动失败时返回。ensureCanonicalName 与 restartSelf 共用。
func relaunchSelf(exePath string) error {
	code, err := runAndWait(exePath, os.Args[1:])
	if err != nil {
		return err
	}
	os.Exit(code)
	return nil
}

func pickFastestCDNPrefix(globalPrefix, chinaPrefix, tagName, testFile string) string {
	globalURL := globalPrefix + tagName + "/" + testFile

	mainLog.Info("Testing GitHub CDN download speed...")

	client := &http.Client{Timeout: 5 * time.Second}
	start := time.Now()
	resp, err := client.Get(globalURL)
	if err != nil {
		mainLog.Info("GitHub CDN connection failed, using China mirror")
		return chinaPrefix
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		mainLog.Info("GitHub CDN returned %d, using China mirror", resp.StatusCode)
		return chinaPrefix
	}

	buf := make([]byte, 32*1024)
	n, err := io.ReadAtLeast(resp.Body, buf, 1024)
	elapsed := time.Since(start).Seconds()

	if err != nil || elapsed == 0 {
		mainLog.Info("GitHub CDN download test failed, using China mirror")
		return chinaPrefix
	}

	speedKBs := float64(n) / elapsed / 1024
	if speedKBs < 10 {
		mainLog.Info("GitHub CDN speed %.1f KB/s < 10 KB/s, using China mirror", speedKBs)
		return chinaPrefix
	}

	mainLog.Info("GitHub CDN available (%.0f KB/s)", speedKBs)
	return globalPrefix
}

const (
	updateReasonSameVersion        = "same-version"
	updateReasonNewerTarget        = "newer-target"
	updateReasonOlderTargetForced  = "older-target-forced"
	updateReasonOlderTargetBlocked = "older-target-blocked"
)

func normalizeSemver(version string) string {
	v := strings.TrimSpace(version)
	if v == "" {
		return ""
	}
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	return v
}

func displayVersion(version string) string {
	return strings.TrimPrefix(strings.TrimSpace(version), "v")
}

func isReleaseVersion(version string) bool {
	normalized := normalizeSemver(version)
	if normalized == "" || normalized == "v0.0.0" {
		return false
	}
	return semver.IsValid(normalized)
}

func isForceReleaseName(name string) bool {
	return strings.HasSuffix(strings.ToLower(strings.TrimSpace(name)), "-force")
}

func decideUpdate(currentVersion, targetVersion, releaseName string) (bool, string, error) {
	current := normalizeSemver(currentVersion)
	if !isReleaseVersion(currentVersion) {
		return false, "", i18n.Errorf("current version is not a valid semver release version: %s", currentVersion)
	}

	target := normalizeSemver(targetVersion)
	if !isReleaseVersion(targetVersion) {
		return false, "", i18n.Errorf("target version is not a valid semver release version: %s", targetVersion)
	}

	cmp := semver.Compare(target, current)
	switch {
	case cmp == 0:
		return false, updateReasonSameVersion, nil
	case cmp > 0:
		return true, updateReasonNewerTarget, nil
	case isForceReleaseName(releaseName):
		return true, updateReasonOlderTargetForced, nil
	default:
		return false, updateReasonOlderTargetBlocked, nil
	}
}

const canonicalExeName = "Metabox-Nexus-PlayerCap.exe"

// legacyExeName 是 v1.x 的可执行文件名，见 cleanupLegacyExe。
const legacyExeName = "Metabox-Nexus-WesingCap.exe"

// ensureCanonicalName 在文件名不是 canonicalExeName 时把自身复制成同目录下的该名字，启动复制品
// 并等它结束，以它的退出码退出（relaunchSelf，issue #71）。复制或启动失败则返回，以当前文件
// 继续运行。
//
// 启动后立即退出的旧写法在终端里的后果：本进程一退出，shell 回到提示符，复制品成为后台进程，
// Ctrl+C 到不了它，只能按进程号结束。等待期间本进程不做任何初始化；原文件在此期间一直被占用。
//
// 例外：以 legacyExeName 启动（v1.x 升级后的首次运行）时不等待，启动复制品后即以 0 退出。
// 复制品的 cleanupLegacyExe 要删掉这个文件，本进程不退出就删不掉；它留着，以后每次从它启动，
// 都会把它的旧内容复制到标准文件名上、盖掉更新过的版本。
func ensureCanonicalName() {
	exePath, err := os.Executable()
	if err != nil {
		return
	}

	currentName := filepath.Base(exePath)
	if strings.EqualFold(currentName, canonicalExeName) {
		return
	}

	canonicalPath := filepath.Join(filepath.Dir(exePath), canonicalExeName)

	src, err := os.Open(exePath)
	if err != nil {
		return
	}
	defer src.Close()

	dst, err := os.Create(canonicalPath)
	if err != nil {
		return
	}
	io.Copy(dst, src)
	dst.Close()
	src.Close() // 下面要等到复制品结束，不留着原文件的句柄

	mainLog.Info("Copied to canonical filename: %s", canonicalExeName)

	if strings.EqualFold(currentName, legacyExeName) {
		if err = selfCommand(canonicalPath, os.Args[1:]).Start(); err == nil {
			os.Exit(0)
		}
	} else {
		err = relaunchSelf(canonicalPath)
	}
	mainLog.Warn("Failed to start %s: %v; continuing under the current filename", canonicalExeName, err)
}
