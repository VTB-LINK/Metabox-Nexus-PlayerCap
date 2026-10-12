package player

// 本文件只放 BenchmarkNormalizeCover：normalizeCover 在各类源图上的耗时与分配量（设计 §6、§9 第 17、18、20 条），
// 便于在主播机或换 Go 版本后复测：go test ./player/ -run '^$' -bench NormalizeCover -benchmem

import (
	"image"
	"testing"
)

// coverBenchSources 是基准用的源图。压力夹具（设计 §9 第 18 条）都恰好卡在 jpegScanBudget 放行的上限：
//   - refine：渐进式灰度 1600²，1 个零 DC 扫描加 maxJPEGScans-1 个 Ah=1 的 AC 逐次逼近扫描，EOB 游程覆盖全部块；
//     不消耗熵位的零系数遍历。refine444 是同样的扫描加在 YCbCr 4:4:4 的 Y 分量上（DC 扫描交错三个分量）。
//   - repeat：基线 YCbCr 4:4:4 1600²，一个三分量交错扫描（顺序式的上限）；每个扫描都做反量化、IDCT 与写像素。
//   - entropy：1600² 基线 JPEG（image/jpeg 只编 4:2:0），噪声大、质量 100，约 4.8MB，接近 5MB 的字节上限。
func coverBenchSources(t testing.TB) []struct {
	name string
	src  []byte
} {
	return []struct {
		name string
		src  []byte
	}{
		{"jpeg1200", photoJPEG(t, 1200, 1200)},
		{"jpeg640", photoJPEG(t, 640, 640)},
		{"jpeg120", photoJPEG(t, 120, 120)},
		{"pass800", photoJPEG(t, 800, 800)},
		{"refine", progressiveGray(1600, 1600, maxJPEGScans-1, 0x10)},
		{"refine444", progressiveRefine444(1600, 1600, maxJPEGScans-1)},
		{"repeat", repeatedBaseline(1600, 1600, 1)},
		{"entropy", encodeJPEG(t, photoImage(1600, 1600, 250), 100)},
		{"png1600", photoPNG(t, 1600, 1600)},
		{"png16bit1600", encodePNG(t, nrgba64Photo(1600, 1600))},
		{"adam7_1600", adam7PNG(t, (*image.NRGBA)(photoImage(1600, 1600, 40)))},
		{"prog444_1600", progressiveYCC444(1600, 1600)},
		{"prog444_1200", progressiveYCC444(1200, 1200)},
	}
}

func BenchmarkNormalizeCover(b *testing.B) {
	for _, c := range coverBenchSources(b) {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				normalizeCover(c.src, "image/jpeg")
			}
			b.ReportMetric(float64(len(c.src)), "srcB")
		})
	}
}
