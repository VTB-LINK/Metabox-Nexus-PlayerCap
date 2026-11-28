//go:build !race

package rum

import "time"

// nonBlockingTrackBudget 为常规模式下 10,000 次并发 Track 的非阻塞上限。
const nonBlockingTrackBudget = 50 * time.Millisecond
