//go:build ignore

// audiostack 在进程内拼出音频波形的服务端栈：真实的 server（含 audioHub 与 /audio-ws 的 WebSocket 处理器）、真实的
// audiocap/manager（门控、进程发现、helper 拉起与解复用）、真实的捕获 helper 子进程。没有歌词播放器与路由，不接
// 任何播放器的取词通道，也不碰其它实例。用于 issue #115 合入后的真后端核对：helper 写的帧头 flags 经 manager 的
// demux 与重编码、hub 扇出，原样到达 WebSocket 客户端。
//
// 为什么不直接起第二个完整服务：manager 启动时的孤儿清扫按「同名 exe + 命令行含 --audio-capture-helper」杀进程，
// 与路径无关；标准文件名的第二个实例会杀掉用户实例的 helper。故 -helper 必须是非标准文件名的完整应用构建，
// 本程序拒绝标准文件名。
//
// 用法：audiostack -helper <完整应用 exe，非标准文件名> [-addr 127.0.0.1:8791] [-secs 600]
// 连 ws://<addr>/<player>/audio-ws 即按需捕获该播放器（进程在跑时），断开后约 1.5 秒内停止。
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"time"

	"Metabox-Nexus-PlayerCap/audiocap/manager"
	"Metabox-Nexus-PlayerCap/server"
	"Metabox-Nexus-PlayerCap/tools/audiostack/guard"
)

func main() {
	helper := flag.String("helper", "", "完整应用的 exe（非标准文件名）；其 --audio-capture-helper 子命令即捕获 helper")
	addr := flag.String("addr", "127.0.0.1:8791", "监听地址（只绑回环，避开用户实例的端口）")
	secs := flag.Int("secs", 600, "最长运行秒数")
	flag.Parse()
	// 两道校验都在启动 manager 与监听之前，且校验的是规范化之后真正交出去的值（见 guard 包）：manager 一启动就按
	// 文件名清扫，原始输入的文件名与规范化后的可以不同（如末尾 \.）。
	abs, err := guard.CheckHelper(*helper)
	if err != nil {
		fmt.Println("拒绝：", err)
		os.Exit(2)
	}
	listen, err := guard.CheckAddr(*addr)
	if err != nil {
		fmt.Println("拒绝：", err)
		os.Exit(2)
	}

	names := []string{"wesing", "cloudmusicv3", "qqmusic", "kugou", "sodamusic", "applemusic", "spotify"}
	srv := server.NewServer(names)
	norm := func(string) manager.NormParams { return manager.NormParams{Enabled: false, Target: -14, MaxGain: 30} }
	mgr := manager.New(srv.AudioHub(), srv.GetPlayerStatus, norm, 25, abs)
	srv.SetAudioWake(mgr.Wake)
	go mgr.Run()

	ready := make(chan struct{})
	errc := make(chan error, 1)
	go func() { errc <- srv.Start(listen, ready) }()
	select {
	case <-ready:
		fmt.Printf("就绪：ws://%s/<player>/audio-ws，helper=%s，最长 %d 秒\n", listen, abs, *secs)
	case err := <-errc:
		fmt.Println("监听失败:", err)
		os.Exit(1)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	select {
	case <-time.After(time.Duration(*secs) * time.Second):
	case <-sig:
	case err := <-errc:
		fmt.Println("服务退出:", err)
	}
	mgr.Stop()
	time.Sleep(1500 * time.Millisecond) // 给 manager 拆 helper 的时间
	fmt.Println("已停止")
}
