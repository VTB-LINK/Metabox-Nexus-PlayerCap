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

func TestMergeYRCToleratesGoodbyeLRCYRCDrift(t *testing.T) {
	lyrics := []LyricLine{
		{Index: 6, Time: 48.70, Text: "I don't want to lead you on"},
		{Index: 28, Time: 182.21, Text: "You would never ask me why"},
		{Index: 34, Time: 214.26, Text: "it's no other way than to say goodbye"},
	}
	yrc := `[47420,4140](47420,690,0)I (48110,270,0)don't (48380,360,0)want (48740,90,0)to (48830,510,0)lead (49340,330,0)you (49670,1890,0)on
[180830,4860](180830,1320,0)You (182150,510,0)would (182660,870,0)never (183530,570,0)ask (184100,240,0)me (184340,1350,0)why
[211790,10590](211790,240,0)it's (212030,570,0)no (212600,360,0)other (212960,1440,0)way (214400,270,0)than (214670,150,0)to (214820,1110,0)say (215930,6450,0)goodbye`

	MergeYRC(lyrics, yrc, 0.5)
	for _, line := range lyrics {
		if len(line.TextDetailed.Words) == 0 {
			t.Fatalf("line %d has empty text_detailed", line.Index)
		}
	}
	if lyrics[2].TextDetailed.Timestamp != 211.79 {
		t.Fatalf("last detail timestamp = %v, want 211.79", lyrics[2].TextDetailed.Timestamp)
	}
}

func TestMergeYRCMatchesSmallTextGap(t *testing.T) {
	lyrics := []LyricLine{{Index: 0, Time: 16.52, Text: "I can see the pain living in your eyes"}}
	yrc := `[15830,3750](15830,240,0)I (16070,210,0)can (16280,270,0)see (16730,510,0)pain (17240,540,0)living (17780,420,0)in (18200,330,0)your (18530,1050,0)eyes`

	MergeYRC(lyrics, yrc, 0.5)
	if len(lyrics[0].TextDetailed.Words) == 0 {
		t.Fatalf("text_detailed.words is empty, want boundary text match")
	}
}

func TestMergeYRCDoesNotConfuseRepeatedNaNaLines(t *testing.T) {
	lyrics := []LyricLine{
		{Index: 0, Time: 127.86, Text: "Na na na na na na na na na na"},
		{Index: 1, Time: 131.11, Text: "Na na na na na na na"},
		{Index: 2, Time: 134.96, Text: "Na na na na na na na na na na"},
		{Index: 3, Time: 138.74, Text: "Na na na na na na na"},
	}
	yrc := `[127520,3810](127520,180,0)Na (127700,270,0)na (127970,510,0)na (128480,240,0)na (128720,210,0)na (128930,600,0)na (129530,60,0)na (129590,390,0)na (129980,810,0)na (130790,540,0)na
[131330,3840](131330,240,0)Na (131570,240,0)na (131810,510,0)na (132320,240,0)na (132560,750,0)na (133310,1260,0)na (134570,600,0)na
[135170,3840](135170,240,0)Na (135410,240,0)na (135650,510,0)na (136160,270,0)na (136430,180,0)na (136610,600,0)na (137210,60,0)na (137270,390,0)na (137660,810,0)na (138470,540,0)na
[139010,2310](139010,240,0)Na (139250,240,0)na (139490,510,0)na (140000,240,0)na (140240,750,0)na (140990,120,0)na (141110,210,0)na`

	MergeYRC(lyrics, yrc, 0.5)
	wantTimestamps := []float32{127.52, 131.33, 135.17, 139.01}
	for i, want := range wantTimestamps {
		if lyrics[i].TextDetailed.Timestamp != want {
			t.Fatalf("line %d detail timestamp = %v, want %v", i, lyrics[i].TextDetailed.Timestamp, want)
		}
	}
}

func TestMergeYRCMatchesFirefliesEarlyTypo(t *testing.T) {
	lyrics := []LyricLine{{Index: 36, Time: 152.61, Text: "I'd like To make myself believe"}}
	yrc := `[152410,4110](152410,510,0)I'd (152920,180,0)lke (153100,180,0)To (153280,360,0)make (153640,870,0)myself (154510,2010,0)believe`

	MergeYRC(lyrics, yrc, 0.5)
	if len(lyrics[0].TextDetailed.Words) == 0 {
		t.Fatalf("text_detailed.words is empty, want single-character typo match")
	}
	if lyrics[0].TextDetailed.Timestamp != 152.41 {
		t.Fatalf("detail timestamp = %v, want 152.41", lyrics[0].TextDetailed.Timestamp)
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
