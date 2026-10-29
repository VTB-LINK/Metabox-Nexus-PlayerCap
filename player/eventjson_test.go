package player

// 本文件只测**推给前端的事件类型的 JSON 形状**：字段是否出现、空值序列化成对象还是数组、
// 字段顺序与键集合、逐字文本取自 words。这些是与 overlay 的契约，改了会静默破坏前端。

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

func TestLyricLineTextDetailedEmptyObject(t *testing.T) {
	line := LyricLine{Index: 1, Timestamp: 2.5, PlayTime: 2.3, Text: "hello", SubText: ""}

	data, err := json.Marshal(line)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	detailed, ok := got["text_detailed"].(map[string]any)
	if !ok {
		t.Fatalf("text_detailed = %#v, want object", got["text_detailed"])
	}
	if len(detailed) != 0 {
		t.Fatalf("text_detailed = %#v, want empty object", detailed)
	}
}

func TestLyricUpdateTextDetailedWithWords(t *testing.T) {
	update := LyricUpdate{
		Index:     0,
		Text:      "还没",
		SubText:   "",
		Timestamp: 16.21,
		PlayTime:  15.71,
		Progress:  0.1,
		TextDetailed: LyricTextDetailed{
			Timestamp: 16.21,
			PlayTime:  15.71,
			Duration:  1.08,
			Words: []LyricTextDetailedWord{
				{Timestamp: 16.21, PlayTime: 15.71, Duration: 0.67, Text: "还"},
				{Timestamp: 16.88, PlayTime: 16.38, Duration: 0.41, Text: "没"},
			},
		},
	}

	data, err := json.Marshal(update)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var got struct {
		TextDetailed struct {
			Words []struct {
				Text string `json:"text"`
			} `json:"words"`
		} `json:"text_detailed"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if len(got.TextDetailed.Words) != 2 || got.TextDetailed.Words[0].Text != "还" {
		t.Fatalf("words = %#v, want detailed words", got.TextDetailed.Words)
	}
}

// topLevelKeys 按出现顺序取出一个 JSON 对象的顶层键。
func topLevelKeys(t *testing.T, data []byte) []string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("不是 JSON 对象：%s", data)
	}
	var keys []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			t.Fatalf("读键失败：%v（%s）", err, data)
		}
		keys = append(keys, tok.(string))
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			t.Fatalf("读值失败：%v（%s）", err, data)
		}
	}
	return keys
}

// all_lyrics 的顶层键集合与键序恰为 title（为空时省略）、duration、position、progress、count、lyrics，
// 不多不少。顶层没有派生的逐字集合（issue #132 删除）：各例都是删除之前会多带一个顶层数组的输入——
// Lyrics 为 nil 或空切片时它是 []，有逐字的行时它有元素。
//
// 变异自证（隔离副本，均实测）：换回删除前的 player.go（MarshalJSON 加回顶层数组）、对调 Count 与 Lyrics
// 的声明顺序 → 六例全红；Title 去掉 omitempty → 「标题为空时省略 title」「零值」红；Lyrics 加 omitempty →
// 「无歌词（空切片）」「Lyrics 为 nil」「零值」红。
func TestAllLyricsDataTopLevelKeys(t *testing.T) {
	full := []string{"title", "duration", "position", "progress", "count", "lyrics"}
	withWords := []LyricLine{{Index: 0, Text: "line", TextDetailed: LyricTextDetailed{
		Timestamp: 1, PlayTime: 0.8, Duration: 1,
		Words: []LyricTextDetailedWord{{Timestamp: 1, PlayTime: 0.8, Duration: 1, Text: "line"}},
	}}}
	cases := []struct {
		name string
		data AllLyricsData
		want []string
	}{
		{"有逐字的行", AllLyricsData{Title: "song", Duration: 1, Count: 1, Lyrics: withWords}, full},
		{"只有逐行的行", AllLyricsData{Title: "song", Duration: 1, Count: 1, Lyrics: []LyricLine{{Index: 0, Text: "line"}}}, full},
		{"无歌词（空切片）", AllLyricsData{Title: "song", Duration: 1, Lyrics: []LyricLine{}}, full},
		{"Lyrics 为 nil", AllLyricsData{Title: "song", Duration: 1}, full},
		{"标题为空时省略 title", AllLyricsData{Duration: 1, Count: 1, Lyrics: withWords}, full[1:]},
		{"零值", AllLyricsData{}, full[1:]},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			data, err := json.Marshal(&c.data)
			if err != nil {
				t.Fatalf("Marshal() error = %v", err)
			}
			if got := topLevelKeys(t, data); !reflect.DeepEqual(got, c.want) {
				t.Errorf("顶层键 = %v，want %v（%s）", got, c.want, data)
			}
		})
	}
}

// all_lyrics 的每一行都带 text_detailed 与 roma_text_detailed：有逐字时带 words，没有时为 {}，两者互不影响；
// 行的键集合与键序同 LyricLine。逐字只在行内（issue #132 起顶层不再另给一份）：逐字文本由 text_detailed 的
// words[].text 直接拼接，词首词尾的空白原样保留、不取行的 text；按行的 index 定位。
//
// 变异自证（隔离副本，均实测）：LyricLine 的 RomaTextDetailed 换一个 json 标签、去掉 LyricTextDetailed 零值
// 序列化为 {} 的分支、LyricTextDetailedWord.Text 换一个 json 标签、序列化逐字时去掉各 words[].text 的首尾空白，
// 本例均红。
func TestAllLyricsDataRowsCarryDetailed(t *testing.T) {
	timeline := func(texts ...string) LyricTextDetailed {
		d := LyricTextDetailed{Timestamp: 9.8, PlayTime: 9.3, Duration: 0.5 * float32(len(texts))}
		for i, s := range texts {
			d.Words = append(d.Words, LyricTextDetailedWord{
				Timestamp: 9.8 + 0.5*float32(i), PlayTime: 9.3 + 0.5*float32(i), Duration: 0.5, Text: s,
			})
		}
		return d
	}
	lines := []LyricLine{
		{Index: 3, Text: "line text must not be used", RomaText: "wa do", TextDetailed: timeline("word ", "text"), RomaTextDetailed: timeline("wa ", "do")},
		{Index: 4, Text: "只有逐行"},
		{Index: 5, Text: "只有逐字", TextDetailed: timeline("只", "有")},
		{Index: 6, Text: "只有逐字音译", RomaText: "ro ma", RomaTextDetailed: timeline("ro ", "ma")},
		{Index: 7, Text: "Hey you", TextDetailed: timeline(" ", "Hey ", "you ")},
	}
	// text / roma 是 text_detailed / roma_text_detailed 的 words[].text 直接拼接的结果，"" 表示该字段应为 {}。
	wants := []struct {
		index      int
		text, roma string
	}{{3, "word text", "wa do"}, {4, "", ""}, {5, "只有", ""}, {6, "", "ro ma"}, {7, " Hey you ", ""}}
	rowKeys := []string{"index", "timestamp", "play_time", "text", "sub_text", "roma_text", "text_detailed", "roma_text_detailed"}

	data, err := json.Marshal(&AllLyricsData{Title: "song", Duration: 10, Count: len(lines), Lyrics: lines})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var got struct {
		Lyrics []json.RawMessage `json:"lyrics"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if len(got.Lyrics) != len(wants) {
		t.Fatalf("lyrics 有 %d 行，want %d：%s", len(got.Lyrics), len(wants), data)
	}
	for i, raw := range got.Lyrics {
		w := wants[i]
		if keys := topLevelKeys(t, raw); !reflect.DeepEqual(keys, rowKeys) {
			t.Errorf("第 %d 行的键 = %v，want %v", i, keys, rowKeys)
		}
		var row struct {
			Index            int             `json:"index"`
			TextDetailed     json.RawMessage `json:"text_detailed"`
			RomaTextDetailed json.RawMessage `json:"roma_text_detailed"`
		}
		if err := json.Unmarshal(raw, &row); err != nil {
			t.Fatalf("第 %d 行 Unmarshal() error = %v", i, err)
		}
		if row.Index != w.index {
			t.Errorf("第 %d 行的 index = %d，want %d", i, row.Index, w.index)
		}
		for _, f := range []struct {
			name string
			raw  json.RawMessage
			want string
		}{{"text_detailed", row.TextDetailed, w.text}, {"roma_text_detailed", row.RomaTextDetailed, w.roma}} {
			if f.want == "" {
				if string(f.raw) != "{}" {
					t.Errorf("index %d 的 %s = %s，want {}", w.index, f.name, f.raw)
				}
				continue
			}
			var d struct {
				Words []struct {
					Text string `json:"text"`
				} `json:"words"`
			}
			if err := json.Unmarshal(f.raw, &d); err != nil {
				t.Fatalf("index %d 的 %s Unmarshal() error = %v", w.index, f.name, err)
			}
			var joined string
			for _, wd := range d.Words {
				joined += wd.Text
			}
			if joined != f.want {
				t.Errorf("index %d 的 %s.words[].text 拼接 = %q，want %q（%s）", w.index, f.name, joined, f.want, f.raw)
			}
		}
	}
}

// roma_text_detailed 与 text_detailed 同形：零值序列化成 {}（不是 null，也不省略），有词时带 words。
// 每条歌词行与 lyric_update 都带这个键；目前只有 wesing 会填。
//
// 变异自证：把 LyricLine 或 LyricUpdate 里 RomaTextDetailed 的 json tag 改成别的名字，对应那一段
// 取不到 roma_text_detailed 而红。
func TestRomaTextDetailedJSONShape(t *testing.T) {
	decode := func(v any) map[string]any {
		t.Helper()
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("Marshal() error = %v", err)
		}
		var got map[string]any
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("Unmarshal() error = %v", err)
		}
		return got
	}
	for name, v := range map[string]any{
		"LyricLine":   LyricLine{Index: 1, Text: "hello"},
		"LyricUpdate": LyricUpdate{Index: 1, Text: "hello"},
	} {
		obj, ok := decode(v)["roma_text_detailed"].(map[string]any)
		if !ok || len(obj) != 0 {
			t.Fatalf("%s 零值：roma_text_detailed = %#v，want 空对象 {}", name, decode(v)["roma_text_detailed"])
		}
	}

	roma := LyricTextDetailed{Timestamp: 13.711, PlayTime: 13.511, Duration: 0.69, Words: []LyricTextDetailedWord{
		{Timestamp: 13.711, PlayTime: 13.511, Duration: 0.2, Text: "hi "},
		{Timestamp: 13.911, PlayTime: 13.711, Duration: 0.49, Text: "ka "},
	}}
	for name, v := range map[string]any{
		"LyricLine":   LyricLine{Text: "ひか", RomaText: "hi ka", RomaTextDetailed: roma},
		"LyricUpdate": LyricUpdate{Text: "ひか", RomaText: "hi ka", RomaTextDetailed: roma},
	} {
		obj, _ := decode(v)["roma_text_detailed"].(map[string]any)
		words, _ := obj["words"].([]any)
		if len(words) != 2 || words[1].(map[string]any)["text"] != "ka " {
			t.Fatalf("%s：roma_text_detailed.words = %#v，want 两个音节", name, obj["words"])
		}
	}
}
