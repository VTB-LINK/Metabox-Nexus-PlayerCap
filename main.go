package main

import (
	"Metabox-Nexus-PlayerCap/clientid"
	"Metabox-Nexus-PlayerCap/config"
	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/logger"
	"Metabox-Nexus-PlayerCap/player/cloudmusic"
	"Metabox-Nexus-PlayerCap/player/cloudmusic/effect"
	"Metabox-Nexus-PlayerCap/player/cloudmusic/park"
	"Metabox-Nexus-PlayerCap/player/kugou"
	"Metabox-Nexus-PlayerCap/player/kugou/watchdog"
	"Metabox-Nexus-PlayerCap/player/qqmusic"
	"Metabox-Nexus-PlayerCap/player/sodamusic"
	"Metabox-Nexus-PlayerCap/player/wesing"
	"Metabox-Nexus-PlayerCap/server"
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

	// 设置日志格式
	log.SetFlags(log.Ldate | log.Ltime)

	// 界面语言：只看 Windows 显示语言，中文系统用中文、其余一律英文（规则见 i18n 包）。
	// 必须早于任何用户可见输出（Banner / config 告警 / mainLog）。探测是 Windows-only，
	// 结果注入纯 Go 的 i18n 包，使 logger/ 与 config/ 保持 GOOS=linux 可构建（AGENTS.md §0.3）。
	i18n.SetLanguage(i18n.DetectSystemLanguage())

	// 确保以标准文件名运行
	ensureCanonicalName()

	// 清理旧版 WesingCap 残留文件（v1.x → v2.0 迁移）
	cleanupLegacyExe()

	cfg := config.Load()

	// 播放器注册表（新增播放器只需在此追加）
	playerNames := []string{wesing.PlayerName, cloudmusic.PlayerName, qqmusic.PlayerName, kugou.PlayerName, sodamusic.PlayerName}

	fmt.Println()
	fmt.Println(`___    __________________         ______ ___________    ____________`)
	fmt.Println(`__ |  / /___  __/___  __ )        ___  / ____  _/__ |  / /___  ____/`)
	fmt.Println(`__ | / / __  /   __  __  |__________  /   __  /  __ | / / __  __/   `)
	fmt.Println(`__ |/ /  _  /    _  /_/ / _/_____/_  /_____/ /   __ |/ /  _  /___   `)
	fmt.Println(`_____/   /_/     /_____/          /_____//___/   _____/   /_____/   `)
	fmt.Println()
	fmt.Println("===========================================================")
	fmt.Println(i18n.T("VTB-TOOLS Metabox Nexus-PlayerCap 多播放器歌词实时推送服务 "))
	fmt.Println("===========================================================")
	fmt.Printf(i18n.T("   版本: v%s\n"), displayVersion(Version))
	fmt.Printf(i18n.T("   监听: %s\n"), cfg.Addr)
	for _, pn := range playerNames {
		fmt.Printf(i18n.T("   播放器: %s (offset=%dms poll=%dms)\n"), pn, cfg.GetPlayerOffset(pn), cfg.GetPlayerPoll(pn))
	}
	fmt.Printf(i18n.T("   优先播放器: %v (超时: %ds)\n"), cfg.PriorPlayer, cfg.PriorPlayerExpire)
	idleHideParts := []string{}
	if cfg.PerPlayerIdleHide > 0 {
		idleHideParts = append(idleHideParts, fmt.Sprintf(i18n.T("全局 %ds"), cfg.PerPlayerIdleHide))
	} else {
		idleHideParts = append(idleHideParts, i18n.T("全局关"))
	}
	for _, pn := range playerNames {
		// 只列与全局不同的显式覆盖（IdleHide != nil）
		if pc := cfg.Players[pn]; pc != nil && pc.IdleHide != nil && *pc.IdleHide != cfg.PerPlayerIdleHide {
			idleHideParts = append(idleHideParts, fmt.Sprintf("%s=%ds", pn, *pc.IdleHide))
		}
	}
	fmt.Printf(i18n.T("   per-player 无活跃自动隐藏: %s\n"), strings.Join(idleHideParts, i18n.ListSep()))
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

	// 强制版本检查与自动更新
	checkAndUpdate()

	// 每日在线心跳。**必须在 checkAndUpdate 之后起** —— 那一步可能替换 exe 并
	// restartSelf() → os.Exit(0)，在它之前起等于给一个马上要死的进程挂后台 goroutine。
	// 它只发请求、绝不做任何更新动作，见 sendHeartbeat。
	startDailyHeartbeat()

	srv := server.NewServer(playerNames)
	// 网易云特效最小化策略（config 静态读取）。Win11 下 park 屏外保活失效（DWM 停止合成不可见窗口），
	// 故无论 config 配什么，Win11 一律强制 fadeout。
	effectStrategy := cfg.EffectStrategy
	if effectStrategy == "park" && park.IsWindows11() {
		mainLog.Warn("检测到 Windows 11：park 屏外保活在 Win11 上失效，特效策略强制改为 fadeout")
		effectStrategy = "fadeout"
	}
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
	endpointsOM.Set("all_lyrics", scheme+"://"+cfg.Addr+"/all_lyrics")
	endpointsOM.Set("lyric_update", scheme+"://"+cfg.Addr+"/lyric_update")
	endpointsOM.Set("status_update", scheme+"://"+cfg.Addr+"/status_update")
	endpointsOM.Set("song_info", scheme+"://"+cfg.Addr+"/song_info")
	endpointsOM.Set("lyric_update-SSE", scheme+"://"+cfg.Addr+"/lyric_update-SSE")
	endpointsOM.Set("song_info-SSE", scheme+"://"+cfg.Addr+"/song_info-SSE")
	for _, pn := range playerNames {
		pe := server.NewOrderedMap()
		pe.Set("ws", wsScheme+"://"+cfg.Addr+"/"+pn+"/ws")
		pe.Set("all_lyrics", scheme+"://"+cfg.Addr+"/"+pn+"/all_lyrics")
		pe.Set("lyric_update", scheme+"://"+cfg.Addr+"/"+pn+"/lyric_update")
		pe.Set("status_update", scheme+"://"+cfg.Addr+"/"+pn+"/status_update")
		pe.Set("song_info", scheme+"://"+cfg.Addr+"/"+pn+"/song_info")
		pe.Set("lyric_update-SSE", scheme+"://"+cfg.Addr+"/"+pn+"/lyric_update-SSE")
		pe.Set("song_info-SSE", scheme+"://"+cfg.Addr+"/"+pn+"/song_info-SSE")
		// 网易云特效镜像通道（仅 cloudmusicv3，归于其命名空间）
		if pn == cloudmusic.PlayerName {
			pe.Set("effect-ws", wsScheme+"://"+cfg.Addr+"/"+pn+"/effect-ws")
			pe.Set("effect-ingest", wsScheme+"://"+cfg.Addr+"/"+pn+"/effect-ingest")
		}
		endpointsOM.Set(pn, pe)
	}

	// 构建有序配置表：按 config.yml 的字段顺序，播放器字段输出所有 resolved 值
	configOM := server.NewOrderedMap()
	configOM.Set("addr", cfg.Addr)
	configOM.Set("offset", cfg.Offset)
	configOM.Set("poll", cfg.Poll)
	configOM.Set("prior-player", cfg.PriorPlayer)
	configOM.Set("prior-player-expire", cfg.PriorPlayerExpire)
	configOM.Set("per-player-idle-hide", cfg.PerPlayerIdleHide)
	configOM.Set("cloudmusicv3-effect-strategy", cfg.EffectStrategy)
	for _, name := range playerNames {
		configOM.Set(name+"-offset", cfg.GetPlayerOffset(name))
		configOM.Set(name+"-poll", cfg.GetPlayerPoll(name))
		configOM.Set(name+"-idle-hide", cfg.GetPlayerIdleHide(name))
	}

	// config_overwritten：按 config 的键顺序筛选出显式设置的条目
	configOverwritten := []string{}
	for _, k := range []string{"addr", "offset", "poll", "prior-player", "prior-player-expire", "per-player-idle-hide", "cloudmusicv3-effect-strategy"} {
		if cfg.ExplicitKeys[k] {
			configOverwritten = append(configOverwritten, k)
		}
	}
	for _, name := range playerNames {
		for _, k := range []string{name + "-offset", name + "-poll", name + "-idle-hide"} {
			if cfg.ExplicitKeys[k] {
				configOverwritten = append(configOverwritten, k)
			}
		}
	}

	srv.SetServiceInfo(&server.ServiceInfo{
		Version:           displayVersion(Version),
		Addr:              cfg.Addr,
		Sources:           cfg.Sources,
		Endpoints:         endpointsOM,
		PlayerSupport:     playerNames,
		Config:            configOM,
		ConfigOverwritten: configOverwritten,
	})

	// 启动统一服务（等待端口就绪后再启动播放器）
	readyCh := make(chan struct{})
	go func() {
		defer telemetry.Guard()
		if err := srv.Start(cfg.Addr, readyCh); err != nil {
			mainLog.Error("服务启动失败: %v", err)
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

	// 创建路由器
	router := server.NewRouter(&cfg, srv, playerNames)
	router.Register(wp)
	router.Register(cp)
	router.Register(qp)
	router.Register(kp)
	router.Register(sp)

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

	// 启动时若发现上次崩溃遗留的「网易云屏外泊车」状态，立即还原窗口
	park.RestoreOrphaned()

	// 网易云特效镜像捕获器（按需截帧，有前端订阅 /cloudmusicv3/effect-ws 时才工作）
	// 传 cp.IsConnected 门控生命周期：取词 player 没连上网易云时静默待命，不独立探 9222、不刷屏
	effectCapturer := effect.New(srv, cp.IsConnected)
	go func() { defer telemetry.Guard(); effectCapturer.Run() }()

	// 优雅退出：收到中断信号时，还原网易云被双击隐藏的菜单后再退出
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		defer telemetry.Guard()
		<-sigCh
		mainLog.Info("收到退出信号，正在还原网易云窗口与菜单...")
		park.Unpark() // 若处于屏外泊车，先飞回原位
		effectCapturer.RestoreChrome()
		os.Exit(0)
	}()

	mainLog.Info("所有播放器已启动，事件路由中...")

	// 主循环：路由器分发事件（阻塞）
	router.Run()
}

// ============================================================================
// 版本检查与自动更新
// ============================================================================

const versionCheckURL = "https://gateway.vtb.link/vtb-tools/metabox/nexus/playercap/v2/client-version"

// ---------------------------------------------------------------------------
// 版本检查请求的客户端标注
// ---------------------------------------------------------------------------
//
// 这个请求过去不带任何身份，网关侧只能按源 IP 计数 —— 主播基本都在 NAT / 动态 IP 后面，
// 同一台机器换 IP 算成多台、同一出口的多台算成一台，DAU/MAU 两头都不准。
// 现在带上匿名设备标识与客户端环境，让网关能去重、能按版本切分。见 clientid 包注释。
//
// **标识走请求头，绝不走 query。** 这个端点后面挂着网关的响应缓存（有一条专门的
// purge workflow，AGENTS.md §12），把客户端标识拼进 query 会让缓存键按客户端打散，
// 缓存当场失效 —— 每台机器每天都回源。头不参与默认缓存键，没有这个副作用。

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
	req, err := http.NewRequest(http.MethodGet, versionCheckURL, nil)
	if err != nil {
		return nil, err
	}
	clientid.Apply(req, clientEnvFor(ping))
	return req, nil
}

type releaseInfo struct {
	TagName   string `json:"tag_name"`
	Name      string `json:"name"`
	GlobalCDN string `json:"global_cdn_download_url_prefix"`
	ChinaCDN  string `json:"china_cdn_download_url_prefix"`
	Assets    []struct {
		Name   string `json:"name"`
		Size   int64  `json:"size"`
		Digest string `json:"digest"`
	} `json:"assets"`
}

func checkAndUpdate() {
	if !isReleaseVersion(Version) {
		mainLog.Info("当前版本 (%s) 不是可自动更新的发布版本，跳过更新检查", displayVersion(Version))
		return
	}

	cleanupOldExe()
	mainLog.Info("正在检查版本更新...")

	req, err := newVersionCheckRequest(clientid.PingStart)
	if err != nil {
		mainLog.Warn("版本检查请求组装失败: %v，继续运行", err)
		return
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		// 连不上网关：标红提示几秒后清除，避免永久卡红。这条路径不重启、要继续跑服务，故
		// clear 后 detach 释放 COM 与被锁的 OS 线程，不让 consoleInit 的 LockOSThread 泄漏进
		// server 阶段。sleep 阻塞启动几秒只发生在连不上这一异常路径，OBS 此刻还没在拉流。
		taskbarInit()
		taskbarError()
		time.Sleep(4 * time.Second)
		taskbarClear()
		taskbarDetach()
		mainLog.Warn("版本检查失败: %v，继续运行", err)
		return
	}
	defer resp.Body.Close()

	// 拿到响应就算「今天已经报过一次在线」，与状态码无关 —— 网关那边这条请求已经落进
	// 访问日志、已经被计数了。只有连都没连上（上面那个 return）才算今天还没报过，
	// 那种情况下留给心跳去补。
	markPinged(time.Now())

	if resp.StatusCode != http.StatusOK {
		mainLog.Warn("版本检查返回 %d，继续运行", resp.StatusCode)
		return
	}

	var release releaseInfo
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		mainLog.Warn("解析版本信息失败: %v，继续运行", err)
		return
	}

	shouldUpdate, reason, err := decideUpdate(Version, release.TagName, release.Name)
	if err != nil {
		mainLog.Warn("版本信息无效: %v，继续运行", err)
		return
	}

	if !shouldUpdate {
		switch reason {
		case updateReasonSameVersion:
			mainLog.Success("当前已是最新版本 v%s", displayVersion(Version))
		case updateReasonOlderTargetBlocked:
			mainLog.Info("检测到较低版本 v%s，release 标题未以 -force 结尾，跳过同步", displayVersion(release.TagName))
		default:
			mainLog.Success("当前版本 v%s 无需更新", displayVersion(Version))
		}
		return
	}

	if reason == updateReasonOlderTargetForced {
		mainLog.Warn("检测到强制版本同步: v%s -> v%s", displayVersion(Version), displayVersion(release.TagName))
	}

	if len(release.Assets) == 0 {
		mainLog.Warn("未找到可用的更新文件，继续运行")
		return
	}

	fmt.Println()
	fmt.Println("╔═════════════════════════════════════════════════════════╗")
	fmt.Printf(i18n.T("║  🆕 发现新版本: v%s → v%s\n"), displayVersion(Version), displayVersion(release.TagName))
	fmt.Printf(i18n.T("║  📦 共 %d 个文件需要更新\n"), len(release.Assets))
	fmt.Println(i18n.T("║  正在自动更新..."))
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

	taskbarInit() // 确定要下载后再探测终端；老 conhost 会锁定当前 OS 线程并建立 COM

	if err := performUpdateAll(cdnPrefix, release.TagName, sortedAssets); err != nil {
		taskbarError()
		manualURL := release.ChinaCDN + release.TagName + "/"
		mainLog.Error("自动更新失败: %v", err)
		mainLog.Warn("当前版本已过期，请手动下载最新版本:")
		mainLog.Warn("%s", manualURL)
		fmt.Println(i18n.T("\n按回车键退出..."))
		fmt.Scanln()
		os.Exit(1)
	}

	taskbarWarning()
	taskbarFlash()
	mainLog.Success("全部更新完成！程序将自动重启...")
	// 金色醒目显示几秒后清除再重启：既不像 1s 那样一闪而过，也不会永久卡在金色。
	time.Sleep(4 * time.Second)
	taskbarClear()
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
		mainLog.Detail("在线心跳组装失败: %v", err)
		return
	}

	client := &http.Client{Timeout: heartbeatTimeout}
	resp, err := client.Do(req)
	if err != nil {
		mainLog.Detail("在线心跳发送失败: %v", err)
		return
	}
	defer resp.Body.Close()

	// 读干净才能让连接回到池里复用。响应内容本身**故意不解析**。
	io.Copy(io.Discard, resp.Body)

	markPinged(time.Now())
	mainLog.Detail("在线心跳已发送")
}

func performUpdateAll(cdnPrefix, tagName string, assets []struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	Digest string `json:"digest"`
}) error {
	exePath, err := os.Executable()
	if err != nil {
		return i18n.Errorf("获取程序路径失败: %v", err)
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
		mainLog.Info("正在下载: %s", downloadURL)

		tmpPath := targetPath + ".new"
		if err := downloadFile(client, downloadURL, tmpPath, asset.Size, asset.Digest); err != nil {
			return i18n.Errorf("下载 %s 失败: %v", asset.Name, err)
		}

		if isExe {
			oldPath := exePath + ".old"
			os.Remove(oldPath)
			if err := os.Rename(exePath, oldPath); err != nil {
				os.Remove(tmpPath)
				return i18n.Errorf("替换 %s 失败 (重命名): %v", asset.Name, err)
			}
			if err := renameWithRetry(tmpPath, exePath); err != nil {
				os.Rename(oldPath, exePath)
				return i18n.Errorf("替换 %s 失败: %v", asset.Name, err)
			}
			mainLog.Success("已替换: %s", asset.Name)
		} else {
			os.Remove(targetPath)
			if err := renameWithRetry(tmpPath, targetPath); err != nil {
				os.Remove(tmpPath)
				return i18n.Errorf("放置 %s 失败: %v", asset.Name, err)
			}
			mainLog.Success("已更新: %s", asset.Name)
		}
	}
	return nil
}

func downloadFile(client *http.Client, url, destPath string, expectedSize int64, expectedDigest string) error {
	resp, err := client.Get(url)
	if err != nil {
		return i18n.Errorf("连接失败: %v", err)
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
		return i18n.Errorf("创建文件失败: %v", err)
	}

	hasher := sha256.New()
	pr := &progressWriter{total: totalSize}
	if totalSize > 0 {
		taskbarProgress(0) // 每个文件从 0 重新起跑（文件数不固定，逐个跑满进度条）
	}
	written, err := io.Copy(out, io.TeeReader(resp.Body, io.MultiWriter(hasher, pr)))
	out.Sync()
	out.Close()
	if err != nil {
		os.Remove(destPath)
		return i18n.Errorf("下载中断: %v", err)
	}

	if expectedDigest != "" {
		actualHash := "sha256:" + hex.EncodeToString(hasher.Sum(nil))
		if actualHash != expectedDigest {
			os.Remove(destPath)
			return i18n.Errorf("SHA256 验证失败: 期望 %s，实际 %s", expectedDigest, actualHash)
		}
		mainLog.Success("SHA256 验证成功")
	}

	mainLog.Success("下载完成 (%.1f MB)", float64(written)/1024/1024)
	return nil
}

func renameWithRetry(src, dst string) error {
	var err error
	for i := 0; i < 5; i++ {
		err = os.Rename(src, dst)
		if err == nil {
			return nil
		}
		mainLog.Info("文件被占用，等待释放 (%d/5)...", i+1)
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
			fmt.Printf(i18n.T("\r[*] 下载进度: %d%% (%.1f/%.1f MB)"), pct,
				float64(pw.written)/1024/1024, float64(pw.total)/1024/1024)
			taskbarProgress(pct)
			pw.lastPct = pct
		}
	}
	return n, nil
}

func cleanupOldExe() {
	exePath, err := os.Executable()
	if err != nil {
		return
	}
	os.Remove(exePath + ".old")
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
	legacyName := "Metabox-Nexus-WesingCap.exe"

	// 收集需要清理的文件
	var targets []string
	for _, name := range []string{legacyName, legacyName + ".old"} {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			targets = append(targets, p)
		}
	}
	if len(targets) == 0 {
		return
	}

	// 后台重试删除：父进程（旧 exe）退出可能有短暂延迟，文件仍被锁定
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
				mainLog.Info("已清理旧版 WesingCap 残留文件")
				return
			}
			targets = remaining
			time.Sleep(1 * time.Second)
		}
	}()
}

func restartSelf() {
	exePath, err := os.Executable()
	if err != nil {
		mainLog.Error("无法自动重启: %v，请手动重新启动程序", err)
		os.Exit(0)
	}
	cmd := exec.Command(exePath, os.Args[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	cmd.Start()
	os.Exit(0)
}

func pickFastestCDNPrefix(globalPrefix, chinaPrefix, tagName, testFile string) string {
	globalURL := globalPrefix + tagName + "/" + testFile

	mainLog.Info("测试 GitHub CDN 下载速度...")

	client := &http.Client{Timeout: 5 * time.Second}
	start := time.Now()
	resp, err := client.Get(globalURL)
	if err != nil {
		mainLog.Info("GitHub CDN 连接失败，使用国内镜像")
		return chinaPrefix
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		mainLog.Info("GitHub CDN 返回 %d，使用国内镜像", resp.StatusCode)
		return chinaPrefix
	}

	buf := make([]byte, 32*1024)
	n, err := io.ReadAtLeast(resp.Body, buf, 1024)
	elapsed := time.Since(start).Seconds()

	if err != nil || elapsed == 0 {
		mainLog.Info("GitHub CDN 下载测试失败，使用国内镜像")
		return chinaPrefix
	}

	speedKBs := float64(n) / elapsed / 1024
	if speedKBs < 10 {
		mainLog.Info("GitHub CDN 速度 %.1f KB/s < 10 KB/s，使用国内镜像", speedKBs)
		return chinaPrefix
	}

	mainLog.Info("GitHub CDN 可用 (%.0f KB/s)", speedKBs)
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
		return false, "", i18n.Errorf("当前版本不是有效 semver 发布版本: %s", currentVersion)
	}

	target := normalizeSemver(targetVersion)
	if !isReleaseVersion(targetVersion) {
		return false, "", i18n.Errorf("目标版本不是有效 semver 发布版本: %s", targetVersion)
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

	mainLog.Info("已复制为标准文件名: %s", canonicalExeName)

	cmd := exec.Command(canonicalPath, os.Args[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	cmd.Start()
	os.Exit(0)
}
