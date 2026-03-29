package main

import (
	"Metabox-Nexus-WesingCap/config"
	"Metabox-Nexus-WesingCap/lyric"
	"Metabox-Nexus-WesingCap/proc"
	"Metabox-Nexus-WesingCap/ws"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// 全局配置
var (
	timeOffsetMs int
	pollMs       int
	// 版本信息（编译时通过 -ldflags 注入）
	Version = "0.0.0"
)

func main() {
	// 确保以标准文件名运行
	ensureCanonicalName()

	cfg := config.Load()
	timeOffsetMs = cfg.Offset
	pollMs = cfg.Poll

	offsetSec := float32(timeOffsetMs) / 1000.0
	pollInterval := time.Duration(pollMs) * time.Millisecond

	fmt.Println("===========================================================")
	fmt.Println("   VTB-TOOLS Metabox-Nexus 全民K歌 歌词实时推送服务 (Go v2)   ")
	fmt.Println("===========================================================")
	fmt.Printf("   版本: v%s\n", Version)
	fmt.Println("===========================================================")

	// 强制版本检查与自动更新（同步，必须在服务启动前完成）
	checkAndUpdate()

	// 启动 WebSocket 服务
	server := ws.NewServer()

	// 构建接口地址列表
	scheme := "http"
	endpoints := map[string]string{
		"ws":               "ws://" + cfg.Addr + "/ws",
		"health-check":     scheme + "://" + cfg.Addr + "/health-check",
		"service-status":   scheme + "://" + cfg.Addr + "/service-status",
		"all_lyrics":       scheme + "://" + cfg.Addr + "/all_lyrics",
		"lyric_update":     scheme + "://" + cfg.Addr + "/lyric_update",
		"status_update":    scheme + "://" + cfg.Addr + "/status_update",
		"song_info":        scheme + "://" + cfg.Addr + "/song_info",
		"lyric_update-SSE": scheme + "://" + cfg.Addr + "/lyric_update-SSE",
		"song_info-SSE":    scheme + "://" + cfg.Addr + "/song_info-SSE",
	}

	// 设置服务信息
	server.SetServiceInfo(&ws.ServiceInfo{
		Version:   Version,
		Addr:      cfg.Addr,
		Offset:    cfg.Offset,
		Poll:      cfg.Poll,
		Sources:   cfg.Sources,
		Endpoints: endpoints,
	})

	go func() {
		if err := server.Start(cfg.Addr); err != nil {
			fmt.Printf("[!] WebSocket 服务启动失败: %v\n", err)
			os.Exit(1)
		}
	}()

	// 主循环：等待进程 → 会话 → 断开后重新等待
	for {
		server.SetStatus("waiting_process", "K歌客户端未启动")
		server.ClearSongData()
		handle, pid := waitForProcess()
		runSession(handle, pid, server, offsetSec, pollInterval)
		proc.CloseProc(handle)
		server.SetStatus("standby", "K歌客户端已退出")
		server.ClearSongData()
		fmt.Println("\n[*] 会话结束，等待新的 WeSing 进程...")
		time.Sleep(2 * time.Second)
	}
}

// waitForProcess 等待 WeSing.exe 进程出现并打开
func waitForProcess() (syscall.Handle, uint32) {
	fmt.Println("[*] 等待 WeSing.exe 启动...")
	printed := false
	for {
		pid, err := proc.FindProcess("WeSing.exe")
		if err == nil {
			handle, err := proc.OpenProc(pid)
			if err == nil {
				fmt.Printf("[+] 找到 WeSing.exe (PID: %d)\n", pid)
				return handle, pid
			}
			fmt.Printf("[!] 打开进程失败: %v\n", err)
		}
		if !printed {
			fmt.Println("[*] WeSing.exe 未运行，等待中...")
			printed = true
		}
		time.Sleep(2 * time.Second)
	}
}

// runSession 运行一个完整的歌词推送会话
func runSession(handle syscall.Handle, pid uint32, server *ws.Server, offsetSec float32, pollInterval time.Duration) {
	// 枚举模块
	modules, err := proc.EnumModules(pid)
	if err != nil {
		fmt.Printf("[!] 枚举模块失败: %v\n", err)
		return
	}
	fmt.Printf("[+] 找到 %d 个模块\n", len(modules))

	lastTitle := ""
	var cachedTimeAddr uint32    // 缓存时间地址（整个会话复用）
	var lastPhase proc.PlayPhase // 追踪上一个播放状态，避免重复发送消息
	lastLoadingTitle := ""       // 追踪加载中的歌曲名，避免重复发送

	// 主会话循环
	for {
		if !isProcessAlive(pid) {
			return
		}

		state := proc.GetPlayState(pid)

		switch state.Phase {
		case proc.PhaseStandby:
			// 只在状态改变时发送一次
			if lastPhase != proc.PhaseStandby {
				server.SetStatus("waiting_song", "K歌窗口未打开")
				server.ClearSongData()
				server.BroadcastLyricNull() // 发送 lyric_update 的 null 消息
				lastPhase = proc.PhaseStandby
			}
			lastTitle = ""
			time.Sleep(1 * time.Second)
			continue

		case proc.PhaseLoading:
			// 只在歌曲名改变时发送状态更新
			if state.SongTitle != lastLoadingTitle {
				fmt.Printf("[*] 歌曲加载中: %s\n", state.SongTitle)
				lastLoadingTitle = state.SongTitle
				server.SetStatus("loading", fmt.Sprintf("加载中: %s", state.SongTitle))
				lastPhase = proc.PhaseLoading
			}
			time.Sleep(500 * time.Millisecond)
			continue

		case proc.PhasePlaying:
			if state.SongTitle != lastTitle {
				fmt.Printf("\n[♪] 歌曲开始播放: %s\n", state.SongTitle)
				lastTitle = state.SongTitle
			}
		}

		// === 播放中：初始化歌词并开始轮询 ===
		lyrics, timeAddr, ok := initSong(handle, pid, modules, cachedTimeAddr)
		if !ok {
			time.Sleep(1 * time.Second)
			continue
		}
		cachedTimeAddr = timeAddr // 缓存地址，后续切歌不再重新搜索

		// 设置 WebSocket 歌词缓存
		lyricItems := make([]ws.LyricItem, len(lyrics))
		for i, l := range lyrics {
			lyricItems[i] = ws.LyricItem{
				Index: l.Index,
				Time:  l.Time,
				Text:  l.Text,
			}
		}
		server.SetLyrics(lyricItems)

		// 获取歌曲总时长（从 UI 字符串 "mm:ss | mm:ss" 解析）
		var songDuration float32
		if d, err := lyric.FindSongDuration(handle); err == nil {
			songDuration = d
		} else {
			// fallback: 最后一行歌词时间 + 10秒
			if len(lyrics) > 0 {
				songDuration = lyrics[len(lyrics)-1].Time + 10
			}
			fmt.Printf("[*] 未找到总时长，使用歌词估算: %.0fs\n", songDuration)
		}
		server.SetDuration(songDuration)

		// 读取当前播放时间（供前端插值计时）
		initialPlayTime, _ := lyric.ReadPlayTime(handle, timeAddr)

		// 读取歌曲信息
		songTitle, songName, singer, coverURL, coverBase64 := getSongInfo(handle, pid, lastTitle)
		server.SetStatus("playing", songTitle)
		server.SetSongInfo(songName, singer, songTitle, coverURL, coverBase64)
		server.BroadcastAllLyrics(songTitle, lyricItems, initialPlayTime)
		lastPhase = proc.PhasePlaying

		// 歌词轮询循环
		exitReason := pollLyrics(handle, pid, lyrics, timeAddr, server, offsetSec, pollInterval, lastTitle, songDuration)
		server.BroadcastIdle()

		switch exitReason {
		case exitProcessDied:
			return
		case exitSongChanged:
			fmt.Println("\n[*] 检测到切歌，重新加载...")
			lastTitle = ""
			continue
		case exitWindowClosed:
			fmt.Println("[*] K歌窗口已关闭")
			// 只在真正关闭时发送一次，转到 PhaseStandby 会再发一次
			lastTitle = ""
			continue
		}
	}
}

// initSong 尝试初始化当前歌曲
// cachedTimeAddr: 上次搜索到的时间地址（非0则尝试复用）
func initSong(handle syscall.Handle, pid uint32, modules []proc.Module, cachedTimeAddr uint32) ([]lyric.LyricLine, uint32, bool) {
	_, subStructAddr, err := lyric.FindLyricHost(handle, modules)
	if err != nil {
		fmt.Printf("[!] %v\n", err)
		return nil, 0, false
	}

	fmt.Println("[*] 加载歌词数据...")
	lyrics, err := lyric.LoadLyrics(handle, subStructAddr)
	if err != nil || len(lyrics) == 0 {
		fmt.Printf("[!] 加载歌词失败: %v\n", err)
		return nil, 0, false
	}
	fmt.Printf("[+] 加载了 %d 行歌词\n", len(lyrics))
	lyric.PrintLyrics(lyrics)

	// 尝试复用缓存的时间地址
	if cachedTimeAddr != 0 {
		if t, err := lyric.ReadPlayTime(handle, cachedTimeAddr); err == nil && t >= 0 && t < 100000 {
			fmt.Printf("[+] 复用时间地址: 0x%08X (当前: %.2fs)\n", cachedTimeAddr, t)
			return lyrics, cachedTimeAddr, true
		}
		fmt.Println("[*] 缓存的时间地址失效，重新搜索...")
	}

	// 搜索时间地址（带重试）
	var timeAddr uint32
	for retry := 0; retry < 10; retry++ {
		timeAddr, err = lyric.FindPlayTimeAddr(handle)
		if err == nil {
			break
		}
		if retry == 0 {
			fmt.Println("[*] 等待音频引擎初始化...")
		}
		time.Sleep(500 * time.Millisecond)
		if !isProcessAlive(pid) {
			return nil, 0, false
		}
	}
	if err != nil {
		fmt.Printf("[!] %v\n", err)
		return nil, 0, false
	}

	return lyrics, timeAddr, true
}

// getSongInfo 获取歌曲信息（返回显示标题、歌曲名、歌手名、封面URL、封面base64）
func getSongInfo(handle syscall.Handle, pid uint32, windowTitle string) (string, string, string, string, string) {
	songInfo, err := lyric.FindSongInfo(handle, windowTitle)

	// 搜索封面 URL 并下载为 base64（使用 MID 精确匹配当前歌曲）
	coverURL := ""
	coverBase64 := ""
	songMID := ""
	if err == nil {
		songMID = songInfo.Mid
	}
	if songMID != "" {
		coverURL = lyric.FindCoverURL(handle, songMID)
		if coverURL != "" {
			fmt.Printf("[+] 封面 URL: %s\n", coverURL)
			coverBase64 = lyric.FetchCoverBase64(coverURL)
		}
	}

	if err == nil {
		if songInfo.Singer != "" {
			title := fmt.Sprintf("%s - %s", songInfo.Name, songInfo.Singer)
			fmt.Printf("[♪] 歌曲: %s  歌手: %s  MID: %s\n", songInfo.Name, songInfo.Singer, songMID)
			return title, songInfo.Name, songInfo.Singer, coverURL, coverBase64
		}
		return songInfo.Name, songInfo.Name, "", coverURL, coverBase64
	}
	title := proc.GetSongTitle(pid)
	name := title
	if title != "" {
		fmt.Printf("[♪] 当前歌曲: %s\n", title)
	}
	return title, name, "", coverURL, coverBase64
}

// exitReason 轮询退出原因
type exitReason int

const (
	exitProcessDied exitReason = iota
	exitSongChanged
	exitWindowClosed
)

// pollLyrics 轮询歌词推送，基于窗口状态检测切歌/关闭
func pollLyrics(handle syscall.Handle, pid uint32, lyrics []lyric.LyricLine, timeAddr uint32,
	server *ws.Server, offsetSec float32, pollInterval time.Duration, currentTitle string, songDuration float32) exitReason {

	fmt.Printf("\n[*] 开始歌词轮询 (%dms 间隔, 偏移 %+dms)...\n", pollMs, timeOffsetMs)
	lastLineIdx := -1
	failCount := 0

	windowCheckInterval := int(1000 / pollMs) // 每1秒检查一次窗口
	if windowCheckInterval < 1 {
		windowCheckInterval = 1
	}
	pollCount := 0

	// 暂停检测
	var lastPlayTime float32 = -1
	paused := false
	isMinimized := false
	isMoving := false
	var frozenSince time.Time // playTime 开始不变的时刻
	frozen := false
	const pauseDuration = 1 * time.Second // 持续冻结 1 秒才判定暂停（兜底）

	// 最小化时间插值：渲染时间冻结但音频继续，用系统时钟估算
	var minimizedAt time.Time
	var playTimeAtMinimize float32
	wasMinimized := false

	for {
		pollCount++

		// 定期检查窗口状态（切歌/关闭检测）
		if pollCount%windowCheckInterval == 0 {
			if !isProcessAlive(pid) {
				return exitProcessDied
			}
			state := proc.GetPlayState(pid)
			isMinimized = state.IsMinimized
			isMoving = state.IsMoving

			switch state.Phase {
			case proc.PhaseStandby:
				return exitWindowClosed
			case proc.PhaseLoading:
				// 标题变了 = 切到新歌在加载；标题没变 = 对话框/暂停，忽略
				if state.SongTitle != currentTitle && state.SongTitle != "" {
					return exitSongChanged
				}
			case proc.PhasePlaying:
				// 标题变化 = 切歌
				if state.SongTitle != currentTitle && state.SongTitle != "" {
					fmt.Printf("[*] 标题变化: %q → %q\n", currentTitle, state.SongTitle)
					return exitSongChanged
				}
			}
		}

		playTime, err := lyric.ReadPlayTime(handle, timeAddr)
		if err != nil {
			failCount++
			if failCount > int(3000/pollMs) {
				fmt.Println("[!] 播放时间读取持续失败")
				return exitSongChanged
			}
			time.Sleep(pollInterval)
			continue
		}
		failCount = 0

		// 最小化处理：用系统时钟插值替代冻结的渲染时间
		// 但如果已经暂停，最小化不应改变暂停状态（音频也是停止的）
		if isMinimized {
			if paused {
				// 已暂停状态下最小化：保持暂停，不做时钟插值
				wasMinimized = true
				frozen = false
			} else {
				if !wasMinimized {
					// 刚进入最小化（播放中）：记录基准点
					minimizedAt = time.Now()
					playTimeAtMinimize = playTime
					wasMinimized = true
					fmt.Printf("[*] 窗口最小化，切换到时钟插值 (基准: %.1fs)\n", playTime)
				}
				// 用系统时钟估算当前播放时间（不超过歌曲总时长）
				elapsed := float32(time.Since(minimizedAt).Seconds())
				playTime = playTimeAtMinimize + elapsed
				if songDuration > 0 && playTime > songDuration {
					playTime = songDuration
				}
				// 最小化期间不做暂停检测
				frozen = false
			}
		} else {
			if wasMinimized {
				// 刚从最小化恢复：切回内存读取
				wasMinimized = false
				fmt.Printf("[*] 窗口恢复，切回内存时间 (%.1fs)\n", playTime)
			}

			// 拖动/调整窗口期间跳过暂停检测（渲染冻结但音频继续）
			if isMoving {
				frozen = false
			} else if lastPlayTime >= 0 && playTime == lastPlayTime {
				// 暂停检测（需持续冻结 2 秒且非拖动状态才触发）
				if !frozen {
					frozenSince = time.Now()
					frozen = true
				}
				if frozen && time.Since(frozenSince) >= pauseDuration && !paused {
					paused = true
					fmt.Printf("[⏸] 检测到暂停 (%.1fs)\n", playTime)
					server.SetStatus("paused", currentTitle)
					server.BroadcastPause(playTime)
				}
			} else {
				frozen = false
				if paused {
					paused = false
					fmt.Printf("[▶] 恢复播放 (%.1fs)\n", playTime)
					server.SetStatus("playing", currentTitle)
					server.BroadcastResume(playTime)
				}
			}
		}
		lastPlayTime = playTime

		// 匹配歌词行
		adjustedTime := playTime + offsetSec
		currentIdx := lyric.FindCurrentLine(lyrics, adjustedTime)
		if currentIdx != lastLineIdx && currentIdx >= 0 {
			lastLineIdx = currentIdx
			l := lyrics[currentIdx]
			fmt.Printf("[♪] [%d] %s (%.1fs)\n", l.Index, l.Text, playTime)

			server.BroadcastLyricUpdate(&ws.LyricUpdate{
				LineIndex: l.Index,
				Text:      l.Text,
				Timestamp: l.Time,
				PlayTime:  playTime,
				Progress:  clampFloat32(playTime/songDuration, 0, 1),
			})
		}

		time.Sleep(pollInterval)
	}
}

func isProcessAlive(pid uint32) bool {
	_, err := proc.FindProcess("WeSing.exe")
	return err == nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func clampFloat32(v, min, max float32) float32 {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

// ============================================================================
// 版本检查与自动更新
// ============================================================================

const versionCheckURL = "https://gateway.vtb.link/vtb-tools/metabox/nexus/wesingcap/v1/client-version"

type releaseInfo struct {
	TagName   string `json:"tag_name"`
	GlobalCDN string `json:"global_cdn_download_url_prefix"`
	ChinaCDN  string `json:"china_cdn_download_url_prefix"`
	Assets    []struct {
		Name   string `json:"name"`
		Size   int64  `json:"size"`
		Digest string `json:"digest"` // SHA256 摘要，格式: "sha256:xxxxx"
	} `json:"assets"`
}

// checkAndUpdate 启动时强制版本检查，发现新版本自动更新
// 如果当前版本不是最新且更新失败，程序将退出
func checkAndUpdate() {
	// 开发/CI 构建版本跳过检查（非 x.y.z 格式）
	if !isSemver(Version) {
		fmt.Printf("[*] 非发布版本 (%s)，跳过更新检查\n", Version)
		return
	}

	// 清理上次更新的旧文件
	cleanupOldExe()

	fmt.Println("[*] 正在检查版本更新...")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(versionCheckURL)
	if err != nil {
		fmt.Printf("[!] 版本检查失败: %v，继续运行\n", err)
		return // 网络错误允许继续使用
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Printf("[!] 版本检查返回 %d，继续运行\n", resp.StatusCode)
		return
	}

	var release releaseInfo
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		fmt.Printf("[!] 解析版本信息失败: %v，继续运行\n", err)
		return
	}

	// 比对 tag_name
	latestVersion := strings.TrimPrefix(release.TagName, "v")
	currentVersion := strings.TrimPrefix(Version, "v")

	if latestVersion == currentVersion {
		fmt.Printf("[✓] 当前已是最新版本 v%s\n", Version)
		return
	}

	if len(release.Assets) == 0 {
		fmt.Println("[!] 未找到可用的更新文件，继续运行")
		return
	}

	fmt.Println()
	fmt.Println("╔═════════════════════════════════════════════════════════╗")
	fmt.Printf("║  🆕 发现新版本: v%s → %s\n", Version, release.TagName)
	fmt.Printf("║  📦 共 %d 个文件需要更新\n", len(release.Assets))
	fmt.Println("║  正在自动更新...")
	fmt.Println("╚═════════════════════════════════════════════════════════╝")
	fmt.Println()

	// exe 优先排序：确保可执行文件最先下载
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

	// 选择最快 CDN（只测一次，所有文件共用）
	cdnPrefix := pickFastestCDNPrefix(release.GlobalCDN, release.ChinaCDN, release.TagName, exeTestFile)

	// 下载所有资源（exe 在最前）
	if err := performUpdateAll(cdnPrefix, release.TagName, sortedAssets); err != nil {
		// 构建手动下载链接（用 China CDN 兜底）
		manualURL := release.ChinaCDN + release.TagName + "/"
		fmt.Printf("\n[!] 自动更新失败: %v\n", err)
		fmt.Println("[!] 当前版本已过期，请手动下载最新版本:")
		fmt.Printf("    %s\n", manualURL)
		fmt.Println("\n按回车键退出...")
		fmt.Scanln()
		os.Exit(1)
	}

	// 更新成功，自动重启
	fmt.Println("\n[✓] 全部更新完成！程序将自动重启...")
	time.Sleep(1 * time.Second)
	restartSelf()
}

// performUpdateAll 下载所有发布资源并替换/放置到程序目录
func performUpdateAll(cdnPrefix, tagName string, assets []struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	Digest string `json:"digest"`
}) error {
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("获取程序路径失败: %v", err)
	}
	exeDir := filepath.Dir(exePath)
	exeBase := filepath.Base(exePath)
	client := &http.Client{Timeout: 5 * time.Minute}

	for i, asset := range assets {
		downloadURL := cdnPrefix + tagName + "/" + asset.Name
		targetPath := filepath.Join(exeDir, asset.Name)
		isExe := strings.EqualFold(asset.Name, exeBase) ||
			strings.HasSuffix(strings.ToLower(asset.Name), ".exe")

		fmt.Printf("\n[%d/%d] %s\n", i+1, len(assets), asset.Name)
		fmt.Printf("[*] 正在下载: %s\n", downloadURL)

		// 下载到临时文件
		tmpPath := targetPath + ".new"
		if err := downloadFile(client, downloadURL, tmpPath, asset.Size, asset.Digest); err != nil {
			return fmt.Errorf("下载 %s 失败: %v", asset.Name, err)
		}

		if isExe {
			// exe 特殊处理：重命名当前运行的 exe 为 .old，再放置新文件
			oldPath := exePath + ".old"
			os.Remove(oldPath)
			if err := os.Rename(exePath, oldPath); err != nil {
				os.Remove(tmpPath)
				return fmt.Errorf("替换 %s 失败 (重命名): %v", asset.Name, err)
			}
			if err := renameWithRetry(tmpPath, exePath); err != nil {
				os.Rename(oldPath, exePath) // 回滚
				return fmt.Errorf("替换 %s 失败: %v", asset.Name, err)
			}
			fmt.Printf("[✓] 已替换: %s\n", asset.Name)
		} else {
			// 普通文件：直接覆盖或放置
			os.Remove(targetPath) // 删除旧文件（如果存在）
			if err := renameWithRetry(tmpPath, targetPath); err != nil {
				os.Remove(tmpPath)
				return fmt.Errorf("放置 %s 失败: %v", asset.Name, err)
			}
			fmt.Printf("[✓] 已更新: %s\n", asset.Name)
		}
	}

	return nil
}

// downloadFile 下载文件到指定路径（带进度显示）
func downloadFile(client *http.Client, url, destPath string, expectedSize int64, expectedDigest string) error {
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("连接失败: %v", err)
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
		return fmt.Errorf("创建文件失败: %v", err)
	}

	// 创建哈希计算器和进度显示写入器
	hasher := sha256.New()
	pr := &progressWriter{total: totalSize}
	written, err := io.Copy(out, io.TeeReader(resp.Body, io.MultiWriter(hasher, pr)))
	out.Sync()
	out.Close()
	if err != nil {
		os.Remove(destPath)
		return fmt.Errorf("下载中断: %v", err)
	}

	// 验证 SHA256
	if expectedDigest != "" {
		actualHash := "sha256:" + hex.EncodeToString(hasher.Sum(nil))
		if actualHash != expectedDigest {
			os.Remove(destPath)
			return fmt.Errorf("SHA256 验证失败: 期望 %s，实际 %s", expectedDigest, actualHash)
		}
		fmt.Printf("\n[✓] SHA256 验证成功\n")
	}

	fmt.Printf("[✓] 下载完成 (%.1f MB)\n", float64(written)/1024/1024)
	return nil
}

// renameWithRetry 重试重命名文件（应对杀毒软件/索引器短暂锁定）
func renameWithRetry(src, dst string) error {
	var err error
	for i := 0; i < 5; i++ {
		err = os.Rename(src, dst)
		if err == nil {
			return nil
		}
		fmt.Printf("[*] 文件被占用，等待释放 (%d/5)...\n", i+1)
		time.Sleep(1 * time.Second)
	}
	return err
}

// progressWriter 下载进度显示
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
			fmt.Printf("\r[*] 下载进度: %d%% (%.1f/%.1f MB)", pct,
				float64(pw.written)/1024/1024, float64(pw.total)/1024/1024)
			pw.lastPct = pct
		}
	}
	return n, nil
}

// cleanupOldExe 清理上次更新的旧文件
func cleanupOldExe() {
	exePath, err := os.Executable()
	if err != nil {
		return
	}
	os.Remove(exePath + ".old")
}

// restartSelf 自动重启程序
func restartSelf() {
	exePath, err := os.Executable()
	if err != nil {
		fmt.Printf("[!] 无法自动重启: %v，请手动重新启动程序\n", err)
		os.Exit(0)
	}
	cmd := exec.Command(exePath, os.Args[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	cmd.Start()
	os.Exit(0)
}

// pickFastestCDNPrefix 测试全球 CDN 速度，返回最优 CDN 前缀
// 不可用或 <10KB/s 则回退到国内镜像
func pickFastestCDNPrefix(globalPrefix, chinaPrefix, tagName, testFile string) string {
	globalURL := globalPrefix + tagName + "/" + testFile

	fmt.Println("[*] 测试 GitHub CDN 下载速度...")

	// 5 秒超时测试全球 CDN
	client := &http.Client{Timeout: 5 * time.Second}
	start := time.Now()
	resp, err := client.Get(globalURL)
	if err != nil {
		fmt.Println("[*] GitHub CDN 连接失败，使用国内镜像")
		return chinaPrefix
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Printf("[*] GitHub CDN 返回 %d，使用国内镜像\n", resp.StatusCode)
		return chinaPrefix
	}

	// 读取前 32KB 测速
	buf := make([]byte, 32*1024)
	n, err := io.ReadAtLeast(resp.Body, buf, 1024)
	elapsed := time.Since(start).Seconds()

	if err != nil || elapsed == 0 {
		fmt.Println("[*] GitHub CDN 下载测试失败，使用国内镜像")
		return chinaPrefix
	}

	speedKBs := float64(n) / elapsed / 1024
	if speedKBs < 10 {
		fmt.Printf("[*] GitHub CDN 速度 %.1f KB/s < 10 KB/s，使用国内镜像\n", speedKBs)
		return chinaPrefix
	}

	fmt.Printf("[*] GitHub CDN 可用 (%.0f KB/s)\n", speedKBs)
	return globalPrefix
}

// isNewerVersion 比较语义化版本号 (x.y.z)，latest > current 返回 true
func isNewerVersion(latest, current string) bool {
	lParts := strings.Split(latest, ".")
	cParts := strings.Split(current, ".")

	maxLen := len(lParts)
	if len(cParts) > maxLen {
		maxLen = len(cParts)
	}

	for i := 0; i < maxLen; i++ {
		var l, c int
		if i < len(lParts) {
			l, _ = strconv.Atoi(lParts[i])
		}
		if i < len(cParts) {
			c, _ = strconv.Atoi(cParts[i])
		}
		if l > c {
			return true
		}
		if l < c {
			return false
		}
	}
	return false
}

// isSemver 检查版本号是否符合语义化版本格式 (x.y.z)
// "0.9.16" → true, "dev" → false, "0.0.0" → false, "Metabox-..." → false
func isSemver(version string) bool {
	v := strings.TrimPrefix(version, "v")
	if v == "" || v == "0.0.0" {
		return false
	}
	hasDot := false
	for _, c := range v {
		if c == '.' {
			hasDot = true
		} else if c < '0' || c > '9' {
			return false
		}
	}
	return hasDot
}

const canonicalExeName = "Metabox-Nexus-WesingCap.exe"

// ensureCanonicalName 如果当前文件名不是标准名称，复制自身并以标准名称启动
func ensureCanonicalName() {
	exePath, err := os.Executable()
	if err != nil {
		return
	}

	currentName := filepath.Base(exePath)
	if strings.EqualFold(currentName, canonicalExeName) {
		return // 已经是标准名称
	}

	canonicalPath := filepath.Join(filepath.Dir(exePath), canonicalExeName)

	// 复制自身到标准名称
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

	fmt.Printf("[*] 已复制为标准文件名: %s\n", canonicalExeName)

	// 启动标准名称的程序
	cmd := exec.Command(canonicalPath, os.Args[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	cmd.Start()
	os.Exit(0)
}
