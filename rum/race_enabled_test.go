//go:build race

package rum

import "time"

// nonBlockingTrackBudget 为 -race 模式下 10,000 次并发 Track 的非阻塞上限；
// race detector 插桩与并发竞争开销显著高于常规模式，但仍远低于挂死服务端 5s 阻塞时长。
const nonBlockingTrackBudget = 1 * time.Second
