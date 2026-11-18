//go:build race

package lyric

// 本文件与 racebuild_norace_test.go 只定义 raceBuild：测试是否以 -race 构建。race 构建下 sync.Pool
// 随机丢弃放回的对象，regexp 的匹配机靠它复用，分配总量远大于出货构建（实测最短产出行一例约 950 MiB，
// 出货构建约 58 MiB），故分配上限只在非 race 构建下断言。
const raceBuild = true
