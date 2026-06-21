// Command genconfig 把内置默认配置(config.DefaultConfigContent)写成一份 clean 的 config.yml。
// 供 CI 打包时生成随包配置，使发版不受开发者本地 config.yml(park 等个人参数)影响。
//
// 用法: go run ./tools/genconfig [输出路径]   （缺省 ./config.yml）
// CI 在 linux 上构建运行，需用原生 GOOS：  GOOS= GOARCH= go run ./tools/genconfig <path>
package main

import (
	"fmt"
	"os"

	"Metabox-Nexus-PlayerCap/config"
)

func main() {
	out := "config.yml"
	if len(os.Args) > 1 {
		out = os.Args[1]
	}
	if err := os.WriteFile(out, []byte(config.DefaultConfigContent()), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "写入 %s 失败: %v\n", out, err)
		os.Exit(1)
	}
	fmt.Printf("已生成默认配置: %s\n", out)
}
