// Command devserver 启动「server + 网易云特效捕获器」最小组合，用于隔离验证
// 链路（不启动其他播放器，避免 watchdog/提权等副作用）。仅供本地开发测试。
//
// 测试控制口（:8766）：
//
//	GET /active?p=cloudmusicv3   将活跃播放器设为网易云（特效淡入）
//	GET /active?p=wesing         切到别的播放器（特效淡出）
//	GET /active?p=               清空活跃播放器（淡出）
package main

import (
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"Metabox-Nexus-PlayerCap/player/cloudmusic/effect"
	"Metabox-Nexus-PlayerCap/player/cloudmusic/park"
	"Metabox-Nexus-PlayerCap/server"
)

func main() {
	srv := server.NewServer([]string{"cloudmusicv3"})
	ready := make(chan struct{})
	go func() {
		if err := srv.Start("0.0.0.0:8765", ready); err != nil {
			panic(err)
		}
	}()
	<-ready

	// 默认网易云为活跃输出，特效默认显示
	srv.SetActivePlayer("cloudmusicv3")

	park.RestoreOrphaned()
	capturer := effect.New(srv, nil) // nil = 总是可用（devserver 无取词管线，不门控）
	go capturer.Run()

	// 测试控制口：手动切换活跃播放器
	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/active", func(w http.ResponseWriter, r *http.Request) {
			p := r.URL.Query().Get("p")
			srv.SetActivePlayer(p)
			w.Write([]byte("active=" + p + "\n"))
		})
		http.ListenAndServe("127.0.0.1:8766", mux)
	}()

	// 优雅退出：还原网易云被双击隐藏的菜单
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	<-sigCh
	park.Unpark()
	capturer.RestoreChrome()
}
