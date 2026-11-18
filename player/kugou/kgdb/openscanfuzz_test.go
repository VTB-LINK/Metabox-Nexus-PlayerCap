package kgdb

import "testing"

// 本文件只放模糊测试：任意字节当明文库，Open 后对每张表以全部列 Scan，只要求不 panic、能返回。
// 运行：go test ./player/kugou/kgdb/ -run XXX -fuzz FuzzOpenScan -fuzztime 60s

func FuzzOpenScan(f *testing.F) {
	b := loadFixture(f, "basic.db")
	f.Add(b[:64<<10])
	f.Add(b)
	f.Fuzz(func(t *testing.T, data []byte) {
		db, err := Open(plainPager(data))
		if err != nil {
			return
		}
		for _, name := range db.Tables() {
			cols, err := db.Columns(name)
			if err != nil {
				continue
			}
			_ = db.Scan(name, cols, func([]Value) bool { return true })
		}
	})
}
