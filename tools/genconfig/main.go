// Command genconfig 把内置默认配置(config.DefaultConfigContent)写成一份 clean 的 config.yml。
// 供 CI 打包时生成随包配置，使发版不受开发者本地 config.yml(park 等个人参数)影响。
//
// 用法: go run ./tools/genconfig [-version 程序版本] [输出路径]   （输出路径缺省 ./config.yml）
//
// -version 定随包 config.yml 的版本键 config-version：传发布版本号（如 4.0.0）得到 "v4.0.0"；不传、
// 或传的不是发布版本号（CI 开发构建的非 semver 版本号）时，得到开发构建的盖章值。规则见
// config.StampVersion。
//
// 两条打包流水线（release.yml、build-windows.yml）都在 Windows runner 上直接 go run。本命令的依赖暂须保持
// GOOS=linux 可构建，作为退回 Linux 发版流水线的前提（AGENTS.md §0.3）；在 Linux 上运行时须清空作业级的
// GOOS：  GOOS= GOARCH= go run ./tools/genconfig -version <版本> <path>
package main

import (
	"flag"
	"fmt"
	"os"

	"Metabox-Nexus-PlayerCap/config"
)

func main() {
	out, err := run(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "写入 %s 失败: %v\n", out, err)
		os.Exit(1)
	}
	fmt.Printf("已生成默认配置: %s\n", out)
}

// run 按命令行参数 args 生成 config.yml，返回输出路径。参数写错时由 flag 包报错并退出。
func run(args []string) (string, error) {
	fs := flag.NewFlagSet("genconfig", flag.ExitOnError)
	version := fs.String("version", "", "程序版本，定 config-version 的取值；不传或不是发布版本号时按开发构建")
	fs.Parse(args)
	out := "config.yml"
	if fs.NArg() > 0 {
		out = fs.Arg(0)
	}
	return out, os.WriteFile(out, []byte(config.DefaultConfigContent(*version)), 0644)
}
