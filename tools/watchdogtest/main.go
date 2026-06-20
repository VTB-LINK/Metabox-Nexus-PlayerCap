// Command watchdogtest 调用 watchdog.EnsureDebugMode，测试「检测到网易云未带保活参数 → 杀掉重启注入」这条冷启动路径。仅供本地开发测试。
package main

import (
	"fmt"

	"Metabox-Nexus-PlayerCap/player/cloudmusic/watchdog"
)

func main() {
	fmt.Println("调用 EnsureDebugMode ...")
	restarted, err := watchdog.EnsureDebugMode()
	fmt.Printf("restarted=%v err=%v\n", restarted, err)
}
