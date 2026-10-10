package mem

// 本文件只测全扫读缓冲的留用上限（Reader.dropOversizedBuf）：容量不超过 maxKeptBuf 的留给下一次全扫，
// 超过的丢掉；以及 eachRegion 返回时确实调用了它。

import "testing"

// eachRegion 返回时处理读缓冲。这里用没有附加任何进程的 Reader（句柄为 0）走到调用点：VirtualQueryEx 对
// 无效句柄立即失败，遍历从第一个出口返回，不读任何进程的内存。遍历真进程并正常走完的出口共用同一个
// defer，但那条路径要有真进程，没有离线测试。
//
// 变异自证（隔离副本，实测）：去掉 eachRegion 里的 `defer r.dropOversizedBuf()`，超限的缓冲仍留着而红。
func TestEachRegionDropsOversizedBufOnReturn(t *testing.T) {
	r := &Reader{buf: make([]byte, maxKeptBuf+1)}
	called := false
	r.eachRegion(func(uintptr, []byte) { called = true })
	if called {
		t.Fatal("句柄为 0 的 Reader 不应读到任何内存区")
	}
	if r.buf != nil {
		t.Fatalf("超限的读缓冲应在 eachRegion 返回时丢掉，却还留着 cap=%d", cap(r.buf))
	}
	r.buf = make([]byte, 4096)
	r.eachRegion(func(uintptr, []byte) {})
	if cap(r.buf) != 4096 {
		t.Fatalf("限内的读缓冲应留用，got cap=%d", cap(r.buf))
	}
}

// 上限两侧：等于上限留用（且是同一块内存，没有重新分配）、超过 1 字节丢弃；按容量而不是长度判；
// 没有缓冲时不出错。
//
// 变异自证（隔离副本，实测）：把 `>` 改成 `>=`，「等于上限」被丢掉而红；去掉置 nil，「超过 1 字节」
// 与「长度小、容量超限」仍留着而红；把 cap 改成 len，「长度小、容量超限」仍留着而红；改成一律丢掉，
// 三个应留用的用例变红。
func TestDropOversizedBuf(t *testing.T) {
	cases := []struct {
		name string
		buf  []byte
		keep bool
	}{
		{"没有缓冲", nil, false},
		{"小缓冲", make([]byte, 4096), true},
		{"等于上限", make([]byte, maxKeptBuf), true},
		{"超过 1 字节", make([]byte, maxKeptBuf+1), false},
		{"长度小、容量超限", make([]byte, 16, maxKeptBuf+1), false},
		{"长度为 0、容量在限内", make([]byte, 0, 4096), true},
	}
	for _, c := range cases {
		r := &Reader{buf: c.buf}
		r.dropOversizedBuf()
		if !c.keep {
			if r.buf != nil {
				t.Errorf("%s: 应丢掉，却还留着 cap=%d", c.name, cap(r.buf))
			}
			continue
		}
		if cap(r.buf) != cap(c.buf) || len(r.buf) != len(c.buf) {
			t.Errorf("%s: 应原样留用，got len=%d cap=%d，want len=%d cap=%d", c.name, len(r.buf), cap(r.buf), len(c.buf), cap(c.buf))
			continue
		}
		if cap(c.buf) > 0 && &r.buf[:1][0] != &c.buf[:1][0] {
			t.Errorf("%s: 留用的不是原来那块内存", c.name)
		}
	}
}
