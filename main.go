package main

import (
	"Metabox-Nexus-PlayerCap/config"
	"Metabox-Nexus-PlayerCap/logger"
	"Metabox-Nexus-PlayerCap/player/cloudmusic"
	"Metabox-Nexus-PlayerCap/player/qqmusic"
	"Metabox-Nexus-PlayerCap/player/wesing"
	"Metabox-Nexus-PlayerCap/server"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// 版本信息（编译时通过 -ldflags 注入）
var Version = "0.0.0"

var mainLog = logger.New("Main")

func main() {
	// 设置日志格式
	log.SetFlags(log.Ldate | log.Ltime)

	// 确保以标准文件名运行
	ensureCanonicalName()

	// 清理旧版 WesingCap 残留文件（v1.x → v2.0 迁移）
	cleanupLegacyExe()

	cfg := config.Load()

	// 播放器注册表（新增播放器只需在此追加）
	playerNames := []string{wesing.PlayerName, cloudmusic.PlayerName, qqmusic.PlayerName}

	fmt.Println("===========================================================")
	fmt.Println("   Metabox-Nexus-PlayerCap 多播放器歌词实时推送服务          ")
	fmt.Println("===========================================================")
	fmt.Printf("   版本: v%s\n", Version)
	fmt.Printf("   监听: %s\n", cfg.Addr)
	for _, pn := range playerNames {
		fmt.Printf("   播放器: %s (offset=%dms poll=%dms)\n", pn, cfg.GetPlayerOffset(pn), cfg.GetPlayerPoll(pn))
	}
	fmt.Printf("   优先播放器: %v (超时: %ds)\n", cfg.PriorPlayer, cfg.PriorPlayerExpire)
	fmt.Println("===========================================================")

	// 强制版本检查与自动更新
	checkAndUpdate()
	srv := server.NewServer(playerNames)

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
		endpointsOM.Set(pn, pe)
	}

	// 构建有序配置表：按 config.yml 的字段顺序，播放器字段输出所有 resolved 值
	configOM := server.NewOrderedMap()
	configOM.Set("addr", cfg.Addr)
	configOM.Set("offset", cfg.Offset)
	configOM.Set("poll", cfg.Poll)
	configOM.Set("prior-player", cfg.PriorPlayer)
	configOM.Set("prior-player-expire", cfg.PriorPlayerExpire)
	for _, name := range playerNames {
		configOM.Set(name+"-offset", cfg.GetPlayerOffset(name))
		configOM.Set(name+"-poll", cfg.GetPlayerPoll(name))
	}

	// config_overwritten：按 config 的键顺序筛选出显式设置的条目
	configOverwritten := []string{}
	for _, k := range []string{"addr", "offset", "poll", "prior-player", "prior-player-expire"} {
		if cfg.ExplicitKeys[k] {
			configOverwritten = append(configOverwritten, k)
		}
	}
	for _, name := range playerNames {
		for _, k := range []string{name + "-offset", name + "-poll"} {
			if cfg.ExplicitKeys[k] {
				configOverwritten = append(configOverwritten, k)
			}
		}
	}

	srv.SetServiceInfo(&server.ServiceInfo{
		Version:           Version,
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

	// 创建路由器
	router := server.NewRouter(&cfg, srv, playerNames)
	router.Register(wp)
	router.Register(cp)
	router.Register(qp)

	// 启动播放器 goroutines
	go wp.Start()
	go cp.Start()
	go qp.Start()

	mainLog.Info("所有播放器已启动，事件路由中...")

	// 主循环：路由器分发事件（阻塞）
	router.Run()
}

// ============================================================================
// 版本检查与自动更新（保留原逻辑）
// ============================================================================

const versionCheckURL = "https://gateway.vtb.link/vtb-tools/metabox/nexus/playercap/v2/client-version"

type releaseInfo struct {
	TagName   string `json:"tag_name"`
	GlobalCDN string `json:"global_cdn_download_url_prefix"`
	ChinaCDN  string `json:"china_cdn_download_url_prefix"`
	Assets    []struct {
		Name   string `json:"name"`
		Size   int64  `json:"size"`
		Digest string `json:"digest"`
	} `json:"assets"`
}

func checkAndUpdate() {
	if !isSemver(Version) {
		mainLog.Info("非发布版本 (%s)，跳过更新检查", Version)
		return
	}

	cleanupOldExe()
	mainLog.Info("正在检查版本更新...")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(versionCheckURL)
	if err != nil {
		mainLog.Warn("版本检查失败: %v，继续运行", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		mainLog.Warn("版本检查返回 %d，继续运行", resp.StatusCode)
		return
	}

	var release releaseInfo
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		mainLog.Warn("解析版本信息失败: %v，继续运行", err)
		return
	}

	latestVersion := strings.TrimPrefix(release.TagName, "v")
	currentVersion := strings.TrimPrefix(Version, "v")

	if latestVersion == currentVersion {
		mainLog.Success("当前已是最新版本 v%s", Version)
		return
	}

	if !isNewerVersion(latestVersion, currentVersion) {
		mainLog.Success("当前版本 v%s 已是最新", Version)
		return
	}

	if len(release.Assets) == 0 {
		mainLog.Warn("未找到可用的更新文件，继续运行")
		return
	}

	fmt.Println()
	fmt.Println("╔═════════════════════════════════════════════════════════╗")
	fmt.Printf("║  🆕 发现新版本: v%s → %s\n", Version, release.TagName)
	fmt.Printf("║  📦 共 %d 个文件需要更新\n", len(release.Assets))
	fmt.Println("║  正在自动更新...")
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

	if err := performUpdateAll(cdnPrefix, release.TagName, sortedAssets); err != nil {
		manualURL := release.ChinaCDN + release.TagName + "/"
		mainLog.Error("自动更新失败: %v", err)
		mainLog.Warn("当前版本已过期，请手动下载最新版本:")
		mainLog.Warn("%s", manualURL)
		fmt.Println("\n按回车键退出...")
		fmt.Scanln()
		os.Exit(1)
	}

	mainLog.Success("全部更新完成！程序将自动重启...")
	time.Sleep(1 * time.Second)
	restartSelf()
}

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

		mainLog.Info("[%d/%d] %s", i+1, len(assets), asset.Name)
		mainLog.Info("正在下载: %s", downloadURL)

		tmpPath := targetPath + ".new"
		if err := downloadFile(client, downloadURL, tmpPath, asset.Size, asset.Digest); err != nil {
			return fmt.Errorf("下载 %s 失败: %v", asset.Name, err)
		}

		if isExe {
			oldPath := exePath + ".old"
			os.Remove(oldPath)
			if err := os.Rename(exePath, oldPath); err != nil {
				os.Remove(tmpPath)
				return fmt.Errorf("替换 %s 失败 (重命名): %v", asset.Name, err)
			}
			if err := renameWithRetry(tmpPath, exePath); err != nil {
				os.Rename(oldPath, exePath)
				return fmt.Errorf("替换 %s 失败: %v", asset.Name, err)
			}
			mainLog.Success("已替换: %s", asset.Name)
		} else {
			os.Remove(targetPath)
			if err := renameWithRetry(tmpPath, targetPath); err != nil {
				os.Remove(tmpPath)
				return fmt.Errorf("放置 %s 失败: %v", asset.Name, err)
			}
			mainLog.Success("已更新: %s", asset.Name)
		}
	}
	return nil
}

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

	hasher := sha256.New()
	pr := &progressWriter{total: totalSize}
	written, err := io.Copy(out, io.TeeReader(resp.Body, io.MultiWriter(hasher, pr)))
	out.Sync()
	out.Close()
	if err != nil {
		os.Remove(destPath)
		return fmt.Errorf("下载中断: %v", err)
	}

	if expectedDigest != "" {
		actualHash := "sha256:" + hex.EncodeToString(hasher.Sum(nil))
		if actualHash != expectedDigest {
			os.Remove(destPath)
			return fmt.Errorf("SHA256 验证失败: 期望 %s，实际 %s", expectedDigest, actualHash)
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
			fmt.Printf("\r[*] 下载进度: %d%% (%.1f/%.1f MB)", pct,
				float64(pw.written)/1024/1024, float64(pw.total)/1024/1024)
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
