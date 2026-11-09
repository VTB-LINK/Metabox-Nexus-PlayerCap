package lyric

// 本文件只测 readImage：按 64 KB 分块读模块映像，块读失败退到逐页读，读不出的页留零。
// 假内存沿用 roma_test.go 的 fakeMem（没写过的地址读不出）。

import "testing"

// 变异自证：去掉逐页读的回退（块读失败就整块留零），第二块里读得出的页成了 0 而红；逐页读时把
// 读到的内容拷到块的起点（copy(img[off:], b)），第二块的页错位而红；分块大小改成整个映像一次读，
// maxRead 超过 64 KB 而红。
func TestReadImageFallsBackToPages(t *testing.T) {
	const base = 0x20000000
	const size = 2*imageChunk + 100 // 两个整块加一段不足一页的尾巴
	holePage := uint32(imageChunk + 2*imagePage)
	tail := uint32(2 * imageChunk)
	want := make([]byte, size)
	f := newFakeMem()
	for i := uint32(0); i < size; i++ {
		v := byte(i%251 + 1) // 不取 0，读不出留下的 0 才分得出来
		if (i >= holePage && i < holePage+imagePage) || i == tail {
			continue // 这一页、以及尾巴的第一个字节读不出
		}
		f.b[base+i] = v
		if i < tail {
			want[i] = v // 尾巴整段读不出（它不足一页，逐页读也是整段一次）
		}
	}
	got := readImage(f, base, size)
	if len(got) != size {
		t.Fatalf("映像长度 %d，want %d", len(got), size)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("偏移 %#x：得到 %#x，want %#x", i, got[i], want[i])
		}
	}
	if f.maxRead > imageChunk {
		t.Errorf("单次读了 %d 字节，超过分块大小 %d", f.maxRead, imageChunk)
	}
}
