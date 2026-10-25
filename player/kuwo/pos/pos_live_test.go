package pos

// 本文件是真机联调用例，默认跳过（仓库惯例：PLAYERCAP_LIVE=1 才跑）。
//
// 它验证的是「活得下去」：定位到结构、位置随播放推进、插值与读数一致。
// 需要酷我**正在播放**且以中完整性运行（管理员启动读不到内存）。
//
// 用法：
//   $env:PLAYERCAP_LIVE='1'; $env:KUWO_RID='MUSIC_204419549'
//   go test ./player/kuwo/pos -run TestLiveRead -v

import (
	"os"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v3/process"
)

func findKuwo(t *testing.T) int32 {
	t.Helper()
	procs, err := process.Processes()
	if err != nil {
		t.Fatalf("列举进程失败: %v", err)
	}
	for _, p := range procs {
		name, err := p.Name()
		if err != nil {
			continue
		}
		if name == "kwmusic.exe" {
			return p.Pid
		}
	}
	t.Skip("酷我没在跑")
	return 0
}

func TestLiveRead(t *testing.T) {
	if os.Getenv("PLAYERCAP_LIVE") != "1" {
		t.Skip("跳过真机联调（PLAYERCAP_LIVE=1 才跑）")
	}
	rid := os.Getenv("KUWO_RID")
	if rid == "" {
		t.Skip("需要 KUWO_RID（例如 MUSIC_204419549）")
	}
	pid := findKuwo(t)

	r, err := OpenSnapshot(int(pid), rid, 0)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer r.Close()
	song := r.Song()
	t.Logf("定位成功: %s", song)

	var tr Tracker
	p0, err := r.Position()
	if err != nil {
		t.Fatalf("Position: %v", err)
	}
	now := time.Now()
	tr.Observe(p0, now)
	t.Logf("t=0 快照=%dms 估算=%.3fs", p0, tr.Estimate(now, song.DurationMs))

	// 采 10 秒，看快照是否按 4 秒节奏推进（单曲循环会回到 0，所以看最大步进而不是净位移）
	deadline := time.Now().Add(10 * time.Second)
	var last int32 = p0
	changes := 0
	seeks := 0
	maxStep := int32(0)
	for time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
		p, err := r.Position()
		if err != nil {
			t.Fatalf("Position: %v", err)
		}
		n := time.Now()
		changed, seek := tr.Observe(p, n)
		if !changed {
			continue
		}
		changes++
		if seek {
			seeks++
		}
		if step := p - last; step > maxStep {
			maxStep = step
		}
		t.Logf("快照 %d -> %dms（seek=%v），估算=%.3fs", last, p, seek, tr.Estimate(n, song.DurationMs))
		last = p
	}
	est := tr.Estimate(time.Now(), song.DurationMs)
	if changes == 0 {
		t.Fatalf("10 秒内快照一次都没变（位置没在走？）: 停在 %dms", last)
	}
	if maxStep < 3000 {
		t.Errorf("最大步进只有 %dms，不像在播放（正常一次刷新是 +4000）", maxStep)
	}
	t.Logf("10 秒内变化 %d 次（其中跳变 %d 次），末值=%dms，估算=%.3fs，jumps=%d",
		changes, seeks, last, est, tr.Jumps())
}
