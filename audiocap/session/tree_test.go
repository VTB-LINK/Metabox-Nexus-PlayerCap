package session

import "testing"

// 本文件只测进程树成员判定 inTree：会话 PID 是否为目标 PID 或其后代。

// chain 造一条长 n 的父链：pid base+i 的父为 base+i-1，base 的父为 1。
func chain(base uint32, n int) map[uint32]uint32 {
	parent := map[uint32]uint32{base: 1}
	for i := 1; i < n; i++ {
		parent[base+uint32(i)] = base + uint32(i-1)
	}
	return parent
}

// 变异自证（实测）：去掉「cur == 0 即否」→「PID 0 与目标 0」红；断链时改为返回 true →「断链」等红；
// 步数上限改为 17 →「17 级」红，改为 15 →「16 级」红。成环一例在去掉步数上限后死循环（测试超时）。
func TestInTree(t *testing.T) {
	const root = 100
	parent := map[uint32]uint32{
		root: 1,   // 目标的父是无关进程
		200:  100, // 子进程
		300:  200, // 孙进程（如 Chromium 的音频子进程）
		400:  1,   // 无关进程（如 helper 自己）
		500:  999, // 父不在快照里（断链）
		610:  620, // 620 原是目标的子进程、已退出，不在快照里
		700:  800, // 700 ↔ 800 成环，不含目标
		800:  700,
		900:  900, // 自环
		10:   0,   // 父为 0
	}
	deep := chain(1000, 18) // 1000 ← 1001 ← … ← 1017
	for k, v := range deep {
		parent[k] = v
	}
	for _, c := range []struct {
		name      string
		root, pid uint32
		want      bool
	}{
		{"目标自身", root, root, true},
		{"子进程", root, 200, true},
		{"孙进程", root, 300, true},
		{"无关进程", root, 400, false},
		{"目标的父不算", root, 1, false},
		{"PID 0", root, 0, false},
		{"PID 0 与目标 0", 0, 0, false},
		{"父为 0 的进程与目标 0", 0, 10, false},
		{"断链", root, 500, false},
		{"中间一级已退出", root, 610, false},
		{"不在快照里", root, 12345, false},
		{"成环", root, 700, false},
		{"自环", root, 900, false},
		{"16 级", 1000, 1016, true},
		{"17 级", 1000, 1017, false},
	} {
		if got := inTree(parent, c.root, c.pid); got != c.want {
			t.Errorf("%s: inTree(root=%d, pid=%d) = %v, 期望 %v", c.name, c.root, c.pid, got, c.want)
		}
	}
}
