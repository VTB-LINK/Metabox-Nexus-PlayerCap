package wesing

import (
	"strings"
	"syscall"
	"time"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/wesing/lyric"
	"Metabox-Nexus-PlayerCap/player/wesing/proc"
	"Metabox-Nexus-PlayerCap/telemetry"
)

// coverFetchOps 是封面 goroutine 用到的外部操作，测试里换成假的。
type coverFetchOps struct {
	openProc  func(pid uint32) (syscall.Handle, error)
	closeProc func(syscall.Handle)
	findURL   func(h syscall.Handle, songMID string) string
	download  func(url string) string // 返回封面 base64，失败为空串
	sleep     func(time.Duration)
}

var realCoverFetchOps = coverFetchOps{
	openProc:  proc.OpenProc,
	closeProc: proc.CloseProc,
	findURL:   lyric.FindCoverURL,
	download:  func(url string) string { return player.FetchCoverBase64("WeSing", url, 5*time.Second) },
	sleep:     time.Sleep,
}

// startCoverFetch 起一个 goroutine 异步取封面 base64（带重试），完成后补发 song_info_update，回写经
// EmitForGen 受代次守卫。返回的通道在 goroutine 结束时关闭，生产代码不用，测试据此等它收尾。
//
// 内存重试要读全民K歌进程的内存，用的句柄由这个 goroutine 自己打开、自己关闭，不借会话的句柄：
// 会话结束时 Start 会关掉会话的句柄，而这里的内存扫描（枚举可写区域加特征扫描）可能还在途中，
// 句柄值关闭后可能已被系统复用给别的对象（issue #117）。封面地址一开始就有时，全程不碰进程，不开句柄。
func (p *WesingPlayer) startCoverFetch(ops coverFetchOps, gen uint64, pid uint32, mid, name, singer, title, url string) <-chan struct{} {
	scan := url == "" && mid != ""
	var h syscall.Handle
	if scan {
		var err error
		if h, err = ops.openProc(pid); err != nil {
			log.Warn("Cannot open the WeSing process for cover retries: %v", err)
			scan = false
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer telemetry.Guard()
		if scan {
			defer ops.closeProc(h)
		}
		coverURL := url
		// 如果初始未获取到封面 URL，重试从内存中搜索（K歌客户端可能延迟加载封面数据）
		if scan {
			for i := 0; i < 5; i++ {
				ops.sleep(1 * time.Second)
				if found := ops.findURL(h, mid); found != "" {
					coverURL = strings.Replace(found, "/mid_album_500/", "/mid_album_800/", 1)
					log.Info("Cover URL fetched on retry %d", i+1)
					break
				}
			}
		}
		if coverURL == "" {
			log.Warn("Cover URL fetch failed, skipping base64 encoding")
			return
		}
		if b64 := ops.download(coverURL); b64 != "" {
			p.EmitForGen(gen, player.EventSongInfoUpdate, &player.SongInfo{
				Name: name, Singer: singer, Title: title,
				Cover: coverURL, CoverBase64: b64,
			})
		} else {
			log.Warn("Cover download failed: %s", coverURL)
		}
	}()
	return done
}
