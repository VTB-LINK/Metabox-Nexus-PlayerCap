package qqmusic

// 本文件只测 ssoFromBuf 的堆/内联分派：堆模式确诊后，length 不合法必须返回空，
// 绝不能落到内联分支去把堆指针当文本读。
// 不测 ReadAllMetadata 的整体、不测版本偏移表、不测 sanitizeString。

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// ssoBuf 造一个 24 字节的 MSVC std::string 快照。
// ptr 只在堆模式（capacity > 15）下有意义；inline 直接铺在 buf[0:16]。
func ssoBuf(inline []byte, ptr, length, capacity uint32) []byte {
	buf := make([]byte, 24)
	if ptr != 0 {
		binary.LittleEndian.PutUint32(buf[0x00:0x04], ptr)
	} else {
		copy(buf[:16], inline)
	}
	binary.LittleEndian.PutUint32(buf[0x10:0x14], length)
	binary.LittleEndian.PutUint32(buf[0x14:0x18], capacity)
	return buf
}

// noHeap 断言堆读**没有**发生 —— 用于证明「堆模式 + length 非法」时我们根本没去解引用。
func noHeap(t *testing.T) func(ptr, n uint32) []byte {
	t.Helper()
	return func(ptr, n uint32) []byte {
		t.Errorf("不该读堆：ptr=%#x n=%d", ptr, n)
		return nil
	}
}

// TestSSOHeapWithBadLengthReturnsEmpty 钉死本条核心：
// **堆模式确诊后，length 不合法必须返回空，绝不能 fallthrough 到内联分支。**
//
// 缺陷背景：原写法 `if capacity > 15 && length > 0 && length < 1000` 把「堆/内联判定」
// 和「length 合法性」挤进同一个 &&，于是「已确诊是堆字符串、但 length 不合法」会掉进
// else 的内联分支 —— 而那里 buf[0:4] 装的是**堆指针不是文本** → 指针的原始字节被当歌名
// 返回 → sanitizeString 只按 rune 过滤（0xB1 解成 RuneError 且 >= 32 被保留）→
// overlay 闪一个乱码标题。
//
// 触发主力是 clear()：capacity 保留（仍 > 15）而 length 归 0 —— 实测这条 96.9% 出乱码。
//
// 变异自证：把两层结构改回单层 `capacity > 15 && length > 0 && length < 1000` 即红。
func TestSSOHeapWithBadLengthReturnsEmpty(t *testing.T) {
	// 一个看起来像小端堆指针的值：低位字节都是可打印 ASCII，正是它们变成乱码
	const ptr = 0x0A1B2C3D

	cases := []struct {
		name   string
		length uint32
	}{
		{"clear() 后：capacity 保留、length 归 0（实测 96.9% 出乱码的那条）", 0},
		{"偏移错位：length 是随机 DWORD", 0xDEADBEEF},
		{"length 恰好等于上界", 1000},
		{"length 超上界", 5000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			buf := ssoBuf(nil, ptr, c.length, 31) // capacity=31 → 堆模式确诊
			got := ssoFromBuf(buf, noHeap(t))
			if got != "" {
				t.Errorf("堆模式 + length 非法应返回空，实得 %q —— 这是堆指针的原始字节，会当歌名推上 OBS", got)
			}
		})
	}
}

// TestSSOHeapHappyPath 反方向：别把堆读坏了。
// 没有这条，「堆模式一律返回空」这种把功能废掉的假修复能通过上面那条。
func TestSSOHeapHappyPath(t *testing.T) {
	const ptr = 0x00400000
	want := "一首很长的歌名超过十五字节"
	buf := ssoBuf(nil, ptr, uint32(len(want)), 64)

	got := ssoFromBuf(buf, func(p, n uint32) []byte {
		if p != ptr {
			t.Fatalf("堆指针应为 %#x，实得 %#x", ptr, p)
		}
		return []byte(want)[:n]
	})
	if got != want {
		t.Errorf("堆模式 + length 合法应读出歌名，实得 %q", got)
	}
}

// TestSSOHeapTrimsTrailingNul 堆读要裁掉尾部 NUL（原行为，别丢）。
func TestSSOHeapTrimsTrailingNul(t *testing.T) {
	buf := ssoBuf(nil, 0x00400000, 8, 32)
	got := ssoFromBuf(buf, func(p, n uint32) []byte {
		return append([]byte("abc"), bytes.Repeat([]byte{0}, 5)...)
	})
	if got != "abc" {
		t.Errorf("尾部 NUL 应被裁掉，实得 %q", got)
	}
}

// TestSSOInlineUnchanged 内联分支必须逐字不变 —— 这是「别盲目收紧」的那半边。
//
// else 分支对短名是正确且已验证的路径：mem.go 的 21.81 条目注释记着验证用例
// 「无地自容」= 12 UTF-8 字节 → cap==15 → 走内联。收紧它会误杀所有短歌名。
func TestSSOInlineUnchanged(t *testing.T) {
	cases := []struct {
		name string
		text string
	}{
		{"验证用例（笔记记载的那个）", "无地自容"},
		{"短英文", "A"},
		{"乐队名", "黑豹乐队"},
		{"恰好 15 字节满格", "123456789012345"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			inline := make([]byte, 16)
			copy(inline, c.text) // 其余为 0 → NUL 终止
			buf := ssoBuf(inline, 0, uint32(len(c.text)), 15)

			if got := ssoFromBuf(buf, noHeap(t)); got != c.text {
				t.Errorf("内联短名应原样读出，实得 %q want %q", got, c.text)
			}
		})
	}
}

// TestSSOInlineEmptyName 内联且首字节即 NUL（空歌名）→ 返回空。
func TestSSOInlineEmptyName(t *testing.T) {
	buf := ssoBuf(make([]byte, 16), 0, 0, 15) // 全 0 内联缓冲
	if got := ssoFromBuf(buf, noHeap(t)); got != "" {
		t.Errorf("空内联字符串应返回空，实得 %q", got)
	}
}

// TestSSOHeapPointerNeverReachesInline 直接钉死本条的因果：
// 堆指针的**原始字节**在任何情况下都不得被当成文本返回。
//
// 用一个低位字节全是可打印 ASCII 的假堆指针（正是它们会变成 overlay 上的乱码），
// 配上各种非法 length —— 只要返回值非空，就说明堆指针漏进了内联读路径。
func TestSSOHeapPointerNeverReachesInline(t *testing.T) {
	// 0x0A1B2C3D 小端 = 3D 2C 1B 0A —— '=' ',' 都是可打印字符
	for _, length := range []uint32{0, 1000, 0xFFFFFFFF, 0xDEADBEEF} {
		for _, capacity := range []uint32{16, 31, 64, 0xFFFFFFFF} {
			buf := ssoBuf(nil, 0x0A1B2C3D, length, capacity)
			got := ssoFromBuf(buf, func(p, n uint32) []byte { return nil })
			if got != "" {
				t.Errorf("length=%#x capacity=%d：堆指针字节漏成了文本 %q", length, capacity, got)
			}
		}
	}
}

// TestSSOShortBufReturnsEmpty 快照不足 24 字节直接返回空（读内存失败的兜底）。
func TestSSOShortBufReturnsEmpty(t *testing.T) {
	if got := ssoFromBuf(make([]byte, 12), noHeap(t)); got != "" {
		t.Errorf("buf 不足 24 字节应返回空，实得 %q", got)
	}
}
