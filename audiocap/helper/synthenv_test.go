package helper

// 本文件只放归一化测试共用的合成信号与换算辅助，不含测试用例。

import "math"

// lin 把 dBFS 换成线性幅度。
func lin(dbfs float64) float64 { return math.Pow(10, dbfs/20) }

// toDB 把线性幅度比换成 dB。
func toDB(ratio float64) float64 { return 20 * math.Log10(ratio) }

// beatEnvelope 合成带节拍起伏的逐帧 RMS 序列（线性值）：每 0.5 秒一拍，拍点电平为 peakDBFS，其后
// 0.3 秒内线性落到 −contrast dB 并保持到下一拍，另加确定性的 ±1.5 dB 抖动（线性同余序列，同一组参数
// 每次相同）。它不是真实音乐，只用来给出有起伏的输入。
func beatEnvelope(fps int, seconds, peakDBFS, contrast float64) []float64 {
	n := int(seconds * float64(fps))
	out := make([]float64, n)
	seed := uint32(12345)
	for i := range out {
		phase := math.Mod(float64(i)/float64(fps), 0.5)
		seed = seed*1664525 + 1013904223
		jitter := (float64(seed>>8)/float64(1<<24) - 0.5) * 3
		out[i] = lin(peakDBFS - contrast*math.Min(1, phase/0.3) + jitter)
	}
	return out
}
