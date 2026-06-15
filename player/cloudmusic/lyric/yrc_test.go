package lyric

import "testing"

func TestParseYRCWithNetEaseSample(t *testing.T) {
	yrc := `{"t":0,"c":[{"tx":"作词: "},{"tx":"伍佰"}]}
[14640,6070](14640,420,0)无(15060,440,0)心(15500,440,0)过(15940,410,0)问(16350,470,0)你(16820,240,0)的(17060,550,0)心(17610,450,0)里(18060,850,0)我(18910,390,0)的(19300,1410,0)吻`

	details := ParseYRC(yrc, 0.5)
	detail, ok := details[14640]
	if !ok {
		t.Fatalf("details[14640] missing")
	}
	if detail.Timestamp != 14.64 || detail.PlayTime != 14.14 || detail.Duration != 6.07 {
		t.Fatalf("detail = %#v, want timestamp/play_time/duration 14.64/14.14/6.07", detail)
	}
	if len(detail.Words) != 11 {
		t.Fatalf("len(words) = %d, want 11", len(detail.Words))
	}
	if detail.Words[0].Text != "无" || detail.Words[0].Timestamp != 14.64 || detail.Words[0].PlayTime != 14.14 || detail.Words[0].Duration != 0.42 {
		t.Fatalf("first word = %#v", detail.Words[0])
	}
	last := detail.Words[len(detail.Words)-1]
	if last.Text != "吻" || last.Timestamp != 19.3 || last.PlayTime != 18.8 || last.Duration != 1.41 {
		t.Fatalf("last word = %#v", last)
	}
}

func TestMergeYRC(t *testing.T) {
	lyrics := []LyricLine{{Index: 0, Time: 14.64, Text: "无心过问你的心里我的吻"}}
	yrc := `[14640,6070](14640,420,0)无(15060,440,0)心(15500,440,0)过(15940,410,0)问(16350,470,0)你(16820,240,0)的(17060,550,0)心(17610,450,0)里(18060,850,0)我(18910,390,0)的(19300,1410,0)吻`

	MergeYRC(lyrics, yrc, 0.5)
	if len(lyrics[0].TextDetailed.Words) != 11 {
		t.Fatalf("words = %#v, want 11 words", lyrics[0].TextDetailed.Words)
	}
	if lyrics[0].TextDetailed.PlayTime != 14.14 {
		t.Fatalf("play_time = %v, want 14.14", lyrics[0].TextDetailed.PlayTime)
	}
}

func TestMergeYRCToleratesTimestampDrift(t *testing.T) {
	lyrics := []LyricLine{{Index: 1, Time: 21.45, Text: "厌倦 我的亏欠 代替你所爱的人"}}
	yrc := `[21490,6770](21490,410,0)厌(21900,870,0)倦 (22770,240,0)我(23010,200,0)的(23210,440,0)亏(23650,880,0)欠 (24530,220,0)代(24750,250,0)替(25000,680,0)你(25680,390,0)所(26070,380,0)爱(26450,680,0)的(27130,1130,0)人`

	MergeYRC(lyrics, yrc, 0.5)
	if len(lyrics[0].TextDetailed.Words) == 0 {
		t.Fatalf("text_detailed.words is empty, want tolerant match")
	}
	if lyrics[0].TextDetailed.Timestamp != 21.49 || lyrics[0].TextDetailed.PlayTime != 20.99 {
		t.Fatalf("detail = %#v, want raw yrc timestamp 21.49 and offset play_time 20.99", lyrics[0].TextDetailed)
	}
}

func TestMergeYRCMatchesMultipleDriftedLines(t *testing.T) {
	lyrics := []LyricLine{
		{Index: 1, Time: 21.45, Text: "厌倦 我的亏欠 代替你所爱的人"},
		{Index: 2, Time: 28.80, Text: "这个时候 我心落花一样飘落下来"},
	}
	yrc := `[21490,6770](21490,410,0)厌(21900,870,0)倦 (22770,240,0)我(23010,200,0)的(23210,440,0)亏(23650,880,0)欠 (24530,220,0)代(24750,250,0)替(25000,680,0)你(25680,390,0)所(26070,380,0)爱(26450,680,0)的(27130,1130,0)人
[28840,6410](28840,370,0)这(29210,460,0)个(29670,260,0)时(29930,920,0)候 (30850,330,0)我(31180,420,0)心(31600,660,0)落(32260,420,0)花(32680,330,0)一(33010,360,0)样(33370,470,0)飘(33840,180,0)落(34020,850,0)下(34870,380,0)来`

	MergeYRC(lyrics, yrc, 0.5)
	for _, line := range lyrics {
		if len(line.TextDetailed.Words) == 0 {
			t.Fatalf("line %d has empty text_detailed", line.Index)
		}
	}
}

func TestMergeYRCToleratesLargeVerifiedDrift(t *testing.T) {
	lyrics := []LyricLine{{Index: 47, Time: 158.03, Text: "Baby you light up my world like nobody else"}}
	yrc := `[157100,3780](157100,420,0)Baby (157520,180,0)you (157700,510,0)light (158210,240,0)up (158450,240,0)my (158690,420,0)world (159110,300,0)like (159410,1080,0)nobody (160490,390,0)else`

	MergeYRC(lyrics, yrc, 0.5)
	if len(lyrics[0].TextDetailed.Words) == 0 {
		t.Fatalf("text_detailed.words is empty, want match for verified 930ms drift")
	}
	if lyrics[0].TextDetailed.Timestamp != 157.1 || lyrics[0].TextDetailed.PlayTime != 156.6 {
		t.Fatalf("detail = %#v, want raw timestamp 157.1 and offset play_time 156.6", lyrics[0].TextDetailed)
	}
}

func TestParseYRCFallbackDurationFromWords(t *testing.T) {
	details := ParseYRC(`[1000,0](1000,200,0)你(1200,300,0)好`, 0)
	detail, ok := details[1000]
	if !ok {
		t.Fatalf("details[1000] missing")
	}
	if detail.Duration != 0.5 {
		t.Fatalf("duration = %v, want 0.5", detail.Duration)
	}
}
