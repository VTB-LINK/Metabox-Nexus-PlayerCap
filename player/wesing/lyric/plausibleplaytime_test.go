package lyric

// 本文件只测 IsPlausiblePlayTime：播放时间的合理性判定，要害是 NaN 必须被拒。
// 用接受式而非德摩根拒绝式——后者会放行 NaN（详见函数注释）。

import (
	"math"
	"testing"
)

// TestIsPlausiblePlayTime 覆盖判定的全部边界，重点是 NaN。
func TestIsPlausiblePlayTime(t *testing.T) {
	nan := float32(math.NaN())
	posInf := float32(math.Inf(1))
	negInf := float32(math.Inf(-1))

	cases := []struct {
		name string
		v    float32
		want bool
	}{
		{"零", 0, true},
		{"正常播放时间", 123.45, true},
		{"上界内", 99999.9, true},
		{"恰好上界", 100000, false},
		{"超上界", 100001, false},
		{"负数", -0.001, false},
		{"大负数", -5000, false},
		{"正无穷", posInf, false},
		{"负无穷", negInf, false},
		{"NaN", nan, false}, // ← 这条是本测试存在的理由
	}
	for _, c := range cases {
		if got := IsPlausiblePlayTime(c.v); got != c.want {
			t.Errorf("%s: IsPlausiblePlayTime(%v) = %v, 期望 %v", c.name, c.v, got, c.want)
		}
	}
}

// TestDeMorganBreaksOnNaN 钉死这个 bug 的本质：接受式与德摩根拒绝式对实数完全等价，
// 但在 NaN 上结论恰好相反。
//
// 这条测试的作用是防止有人「化简」IsPlausiblePlayTime——把
//
//	!(v >= 0 && v < 100000)
//
// 手工德摩根成看起来更简洁的
//
//	v < 0 || v >= 100000
//
// 而这正是 validateTimeAddr 原本的写法与 NaN 泄漏的来源。
func TestDeMorganBreaksOnNaN(t *testing.T) {
	// 拒绝式：validateTimeAddr 原本的写法
	rejectForm := func(v float32) bool { return !(v < 0 || v >= 100000) }
	// 接受式：正确写法
	acceptForm := IsPlausiblePlayTime

	// 对所有非 NaN 的值，两者必须一致
	normals := []float32{
		0, 1, 123.45, 99999.9, 100000, 100001, -0.001, -5000,
		float32(math.Inf(1)), float32(math.Inf(-1)),
		math.SmallestNonzeroFloat32, math.MaxFloat32,
	}
	for _, v := range normals {
		if rejectForm(v) != acceptForm(v) {
			t.Errorf("非 NaN 值 %v 上两种写法应当一致：拒绝式=%v 接受式=%v",
				v, rejectForm(v), acceptForm(v))
		}
	}

	// 唯独 NaN 上两者相反——这就是 bug
	nan := float32(math.NaN())
	if !rejectForm(nan) {
		t.Fatalf("前提失效：德摩根拒绝式本应放行 NaN")
	}
	if acceptForm(nan) {
		t.Fatalf("接受式必须拒绝 NaN")
	}
	t.Logf("已确认：德摩根拒绝式放行 NaN(=%v)，接受式拒绝之——两种写法仅在 NaN 上分歧", rejectForm(nan))
}

// TestNaNPoisonsDownstream 记录 NaN 通过校验后的下游后果，说明为什么这条值得当 HIGH 修。
// encoding/json 编码不了 NaN，会让 WS 的 WriteJSON 报错；而写 goroutine 遇错即 return
// 却不关闭连接（server.go:490），订阅者会永久僵死却仍在册。
func TestNaNPoisonsDownstream(t *testing.T) {
	nan := float32(math.NaN())
	if !math.IsNaN(float64(nan)) {
		t.Fatal("前提失效")
	}
	// NaN 参与任何算术仍是 NaN —— 一旦进入 PlayTime，进度、插值全部污染
	if !math.IsNaN(float64(nan / 180)) {
		t.Error("NaN/180 应仍为 NaN")
	}
	if !math.IsNaN(float64(nan + 1)) {
		t.Error("NaN+1 应仍为 NaN")
	}
}
