package kgdb

import (
	"encoding/binary"
	"testing"
)

// 本文件只测单次 Scan 的取页次数（含溢出页）以库的总页数为上限：正常扫描不超过它，
// 成环与重复引用的结构在此数之内报 ErrCorrupt。
//
// 读取器不记已访问页：内部页成环受深度上限所限、溢出链成环受「读满载荷时下一页须为 0」所限，
// 都在取页次数用完之前报错；多个指针指向同一棵子树不违反这两条，只能由取页次数兜住。

// scanCounted 打开 b、清零取页记录后以全部列扫描 table，返回 Scan 期间的取页次数与错误。
func scanCounted(t *testing.T, b []byte, table string) (int, error) {
	t.Helper()
	rp := &recordingPager{Pager: plainPager(b)}
	db := mustOpen(t, rp)
	rp.pages = nil
	_, err := scanAll(t, db, table)
	return len(rp.pages), err
}

func TestScanVisitBudget(t *testing.T) {
	count := int(plainPager(loadFixture(t, "basic.db")).PageCount())

	t.Run("intact", func(t *testing.T) {
		for _, table := range []string{"LocalFile", "LyricInfo"} {
			n, err := scanCounted(t, loadFixture(t, "basic.db"), table)
			if err != nil || n == 0 || n > count {
				t.Errorf("%s: %d page reads, err %v; want 1..%d, no error", table, n, err, count)
			}
		}
	})

	t.Run("shared subtree", func(t *testing.T) {
		// LocalFile 第一个二级内部页的每个子指针都改指第二个二级内部页：每个页都合法、深度不变，
		// 但那棵子树被读一百多遍。
		b := loadFixture(t, "basic.db")
		root := localFileRoot(t, b)
		level2 := children(b, root)
		if len(level2) < 2 || !isInterior(b, level2[0]) || !isInterior(b, level2[1]) {
			t.Fatalf("fixture: LocalFile is not a three-level b-tree")
		}
		for i := range children(b, level2[0]) {
			setChild(b, level2[0], i, level2[1])
		}
		n, err := scanCounted(t, b, "LocalFile")
		wantErr(t, err, ErrCorrupt)
		if n != count {
			t.Fatalf("%d page reads, want exactly %d (budget exhausted)", n, count)
		}
	})

	cycles := map[string]func(b []byte){
		"interior cycle": func(b []byte) { setChild(b, rootOf(t, b, "LocalFile"), 0, rootOf(t, b, "LocalFile")) },
		"overflow cycle": func(b []byte) {
			head := overflowHead(t, b, "LyricInfo")
			binary.BigEndian.PutUint32(page(b, head), head)
		},
	}
	for name, corrupt := range cycles {
		t.Run(name, func(t *testing.T) {
			b := loadFixture(t, "basic.db")
			corrupt(b)
			table := "LocalFile"
			if name == "overflow cycle" {
				table = "LyricInfo"
			}
			n, err := scanCounted(t, b, table)
			wantErr(t, err, ErrCorrupt)
			if n > count {
				t.Fatalf("%d page reads, want at most %d", n, count)
			}
		})
	}
}
