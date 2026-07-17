package player

// 本文件只测**推给前端的事件类型的 JSON 形状**：字段是否出现、空值序列化成对象还是数组、
// 字段顺序、lyrics_detailed 取词的来源。这些是与 overlay 的契约，改了会静默破坏前端。

import (
	"encoding/json"
	"strings"
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

func TestAllLyricsDataLyricsDetailedEmptyArray(t *testing.T) {
	data, err := json.Marshal(AllLyricsData{Lyrics: []LyricLine{{Index: 0, Text: "line"}}, Count: 1})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var got struct {
		LyricsDetailed []LyricDetailedLine `json:"lyrics_detailed"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if got.LyricsDetailed == nil || len(got.LyricsDetailed) != 0 {
		t.Fatalf("lyrics_detailed = %#v, want empty array", got.LyricsDetailed)
	}
}

func TestAllLyricsDataJSONFieldOrder(t *testing.T) {
	data, err := json.Marshal(AllLyricsData{
		Title:    "song",
		Duration: 1,
		PlayTime: 0,
		Progress: 0,
		Count:    1,
		Lyrics:   []LyricLine{{Index: 0, Text: "line"}},
	})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	jsonText := string(data)
	countIndex := strings.Index(jsonText, `"count"`)
	lyricsIndex := strings.Index(jsonText, `"lyrics"`)
	if countIndex < 0 || lyricsIndex < 0 {
		t.Fatalf("json = %s, want count and lyrics fields", jsonText)
	}
	if countIndex > lyricsIndex {
		t.Fatalf("json = %s, want count before lyrics", jsonText)
	}
}

func TestAllLyricsDataLyricsDetailedUsesWordText(t *testing.T) {
	data, err := json.Marshal(AllLyricsData{
		Lyrics: []LyricLine{{
			Index:     3,
			Timestamp: 10,
			PlayTime:  9.5,
			Text:      "line text must not be used",
			TextDetailed: LyricTextDetailed{
				Timestamp: 9.8,
				PlayTime:  9.3,
				Duration:  1.2,
				Words: []LyricTextDetailedWord{
					{Timestamp: 9.8, PlayTime: 9.3, Duration: 0.5, Text: "word "},
					{Timestamp: 10.3, PlayTime: 9.8, Duration: 0.7, Text: "text"},
				},
			},
		}},
		Count: 1,
	})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var got struct {
		LyricsDetailed []LyricDetailedLine `json:"lyrics_detailed"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if len(got.LyricsDetailed) != 1 {
		t.Fatalf("len(lyrics_detailed) = %d, want 1", len(got.LyricsDetailed))
	}
	if got.LyricsDetailed[0].LyricIndex != 3 || got.LyricsDetailed[0].Text != "word text" {
		t.Fatalf("lyrics_detailed[0] = %#v", got.LyricsDetailed[0])
	}
}
