//go:build race

package assoc

// 本文件与 racebuild_norace_test.go 只定义 raceBuild：测试是否以 -race 构建。race 构建下的分配总量与出货构建不同
// （同酷狗 player/kugou/lyric/racebuild_race_test.go），故分配上限只在非 race 构建下断言。
const raceBuild = true
