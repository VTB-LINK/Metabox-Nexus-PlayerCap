package wesing

import (
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
	findInfo  func(h syscall.Handle, expectedName string) (lyric.SongInfo, error)
	findByMID func(h syscall.Handle, songMID string) (lyric.SongInfo, bool) // PCM 模式补读；兜底模式补歌手（#146）
	findAlbum func(h syscall.Handle, songMID string) string                 // 返回专辑 mid（#146）
	download  func(url string) string                                       // 返回封面 base64，失败为空串
	sleep     func(time.Duration)
}

var realCoverFetchOps = coverFetchOps{
	openProc:  proc.OpenProc,
	closeProc: proc.CloseProc,
	findInfo:  lyric.FindSongInfoPreferMID, // 兜底模式的补读，同宣告（设计 §3.1）
	findByMID: lyric.FindSongInfoByMID,
	findAlbum: lyric.FindAlbumMID,
	download:  func(url string) string { return player.FetchCoverBase64("WeSing", url, 5*time.Second) },
	sleep:     time.Sleep,
}

// startCoverFetch 起一个 goroutine 异步取封面 base64（带重试），完成后补发 song_info_update，回写经
// EmitForGen 受代次守卫。返回的通道在 goroutine 结束时关闭，生产代码不用，测试据此等它收尾。
//
// 宣告时没有 mid（getSongMeta 匹配不到窗口歌名，或匹配上的记录没有 mid）时，同一个重试循环里先补读歌曲信息
// （#145，设计 §4.4「元数据补读」）：此前没有 mid 就不重试，整首没有歌手与封面。只认歌名等于宣告时窗口歌名
// windowTitle 的记录：#145 时 FindSongInfo 匹配不到窗口歌名会退回地址最高的记录，可能是上一首，补读期间代次
// 不变，EmitForGen 拦不住（设计审 F5）；#146 起 FindSongInfoPreferMID 不再退回，这里照旧核对歌名。匹配上的记录
// 有 mid 而没有歌手时按 mid 补歌手，查到的歌名相同才取（#146，设计 §3.2）。补读到的歌名或歌手与宣告时的不同，
// 先回写一次歌名与歌手，歌手可能因此被清空：宣告时没有 mid，宣告时的歌手来源无法核对，以补读匹配上的记录为准
// （设计 §3.5，兜底歌手设计审 R2）。有了 mid 之后在剩余次数里按它查专辑 mid、构造封面地址（#146，设计 §3.4）。
// 两次回写各自独立，标题仍是宣告时的。窗口歌名为空时不补读。
//
// pcm 为真是 PCM 模式（#146，设计 §3.5），以上补读都不用：mid 是路径 mid，歌曲信息未确认（unconfirmed）、或
// 宣告时缺封面的，在同一个循环里按它补读，不用窗口歌名。歌曲信息查到即确认，回写一次带
// 歌名、歌手的（封面地址已有的一并带上，服务端整体替换歌曲信息）；封面按它查专辑 mid、构造地址，循环结束之后
// 下载、回写。两次回写的先后同上；标题仍是宣告时的，status_update 与 all_lyrics 不改。路径 mid 为空时没有可
// 补读的。
//
// 内存重试要读全民K歌进程的内存，用的句柄由这个 goroutine 自己打开、自己关闭，不借会话的句柄：
// 会话结束时 Start 会关掉会话的句柄，而这里的内存扫描（枚举可写区域加特征扫描）可能还在途中，
// 句柄值关闭后可能已被系统复用给别的对象（issue #117）。封面地址一开始就有时，全程不碰进程，不开句柄。
func (p *WesingPlayer) startCoverFetch(ops coverFetchOps, gen uint64, pid uint32, windowTitle, mid, name, singer, title, url string, pcm, unconfirmed bool) <-chan struct{} {
	scan := url == "" && (mid != "" || windowTitle != "")
	if pcm {
		scan = mid != "" && (url == "" || unconfirmed)
	}
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
		infoFound := false // PCM 模式：已按路径 mid 查到歌曲信息（name、singer 已更新），但回写未必已入队
		// 如果初始未获取到封面 URL，重试从内存中搜索（K歌客户端可能延迟加载封面数据）
		if scan {
			for i := 0; i < 5; i++ {
				ops.sleep(1 * time.Second)
				if pcm {
					if unconfirmed {
						if !infoFound {
							if info, ok := ops.findByMID(h, mid); ok {
								infoFound, name, singer = true, info.Name, info.Singer
								log.Info("Song info fetched on retry %d", i+1)
							}
						}
						// 回写入队了才算确认：事件通道满被丢弃的，下一拍按已查到的重发，不再扫内存（#146 代码审）。
						// 封面那次回写同样带着 name、singer，所以没机会重发时，查到的歌曲信息仍随封面发出。
						if infoFound && p.EmitForGen(gen, player.EventSongInfoUpdate, &player.SongInfo{Name: name, Singer: singer, Title: title, Cover: coverURL}) {
							unconfirmed = false
						}
					}
					if coverURL == "" {
						if a := ops.findAlbum(h, mid); a != "" {
							coverURL = lyric.AlbumCoverURL(a)
							log.Info("Cover URL fetched on retry %d", i+1)
						}
					}
					if !unconfirmed && coverURL != "" {
						break
					}
					continue
				}
				if mid == "" {
					info, err := ops.findInfo(h, windowTitle)
					if err != nil || info.Name != windowTitle {
						continue
					}
					if info.Mid != "" && info.Singer == "" {
						if s, ok := ops.findByMID(h, info.Mid); ok && s.Name == info.Name {
							info.Singer = s.Singer
						}
					}
					if info.Name != name || info.Singer != singer {
						name, singer = info.Name, info.Singer
						log.Info("Song info fetched on retry %d", i+1)
						p.EmitForGen(gen, player.EventSongInfoUpdate, &player.SongInfo{Name: name, Singer: singer, Title: title})
					}
					if mid = info.Mid; mid == "" {
						continue
					}
					log.Detail("Song mid fetched on retry %d: %s", i+1, mid)
				}
				if a := ops.findAlbum(h, mid); a != "" {
					coverURL = lyric.AlbumCoverURL(a)
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
