package lyric

import (
	"encoding/json"
	"math"
	"testing"
)

// 回归：strconv.ParseFloat 接受 "NaN"/"Inf"/"1e999"，非有限时间一路进入 Line.Time / Detailed，
// 最终让 all_lyrics 载荷 json.Marshal 失败（AGENTS §3.7：会让 WS 写协程退出而连接不关）。
func TestParseTSRejectsNonFinite(t *testing.T) {
	for _, s := range []string{"NaN", "nan", "Inf", "+Inf", "-Inf", "Infinity", "1e999", "1e39", "0:NaN", "1:Inf", "-5", "-0.5"} {
		if v, ok := parseTS(s); ok {
			t.Errorf("parseTS(%q) 应拒绝，却得到 (%v,true)", s, v)
		}
	}
	for _, s := range []string{"0", "29.188", "1:05.491", "1:02:03.500", "4:29.747"} {
		if v, ok := parseTS(s); !ok || math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) || v < 0 {
			t.Errorf("parseTS(%q) 应接受: (%v,%v)", s, v, ok)
		}
	}
}

func TestParseTTMLNonFiniteTimesNeverReachPayload(t *testing.T) {
	doc := `<tt xmlns="http://www.w3.org/ns/ttml" xmlns:itunes="http://music.apple.com/lyric-ttml-internal" itunes:timing="Word"><body dur="NaN"><div>` +
		`<p begin="NaN" end="5" itunes:key="L1">坏行</p>` +
		`<p begin="1.0" end="Inf" itunes:key="L2"><span begin="Inf" end="2">字</span><span begin="1.5" end="NaN">好</span></p>` +
		`</div></body></tt>`
	lines := ParseTTML(doc)
	if _, err := json.Marshal(lines); err != nil {
		t.Fatalf("解析结果必须总能序列化: %v", err)
	}
	if d, ok := DocDuration(doc); ok {
		t.Errorf("DocDuration 不应接受 NaN: %v", d)
	}
}
