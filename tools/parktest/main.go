// Command parktest 手动测试 park 包：park / unpark / restore / status。仅供本地开发测试。
package main

import (
	"fmt"
	"os"

	"Metabox-Nexus-PlayerCap/player/cloudmusic/park"
)

func main() {
	cmd := "status"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "park":
		fmt.Println("park ->", park.Park())
	case "unpark":
		fmt.Println("unpark ->", park.Unpark())
	case "restore":
		park.RestoreOrphaned()
		fmt.Println("restore done")
	case "list":
		for _, s := range park.DebugList() {
			fmt.Println(s)
		}
	default:
		fmt.Println("isParked ->", park.IsParked())
	}
}
