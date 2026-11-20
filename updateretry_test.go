package main

// 本文件只测 readRetryChoice：自动更新失败后按 R 重试、按回车或 Ctrl+C 退出的按键判定（#216）。

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// consumed 是读到判定为止消费的字节数：判定之后的输入须留在原处，不能多读。
//
// 变异自证（隔离副本，实测）：
//   - 只认小写 'r' → "R"、"xyzR" 返回 false，红；
//   - 不认 'r' → "r"、" r"、"rr"、"重r" 返回 false，红；
//   - 不认 '\r' → "\r\n" 消费 2 字节、"\rr" 返回 true，红；
//   - 不认 '\n' → "\nr" 返回 true，红；
//   - '\r' 当作重试 → "\r"、"\r\n"、"\rr" 返回 true，红；
//   - '\n' 当作重试 → "\n"、"xyz\n"、"\nr"、"重\n"、"q\n" 返回 true，红；
//   - 读到 EOF 返回 true → "" 红；
//   - 一次读入 64 字节再逐字节判定 → "\r\n"、"\nr"、"\rr"、"rr"、"\x03r" 消费 2 字节，"x\x03r" 消费 3 字节，红；
//   - 不认识的字节即返回 false（只看首字节）→ "xyzR"、" r"、"重r" 返回 false，"xyz\n"、"q\n"、"重\n"、"x\x03r" 只消费 1 字节，红；
//   - 非 ASCII 字节即返回 false → "重r" 返回 false、"重\n" 只消费 1 字节，红；
//   - 不认 0x03 → "\x03r"、"x\x03r" 返回 true，红；
//   - 0x03 当作重试 → "\x03"、"\x03r"、"x\x03r" 返回 true，红。
func TestReadRetryChoice(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		want     bool
		consumed int
	}{
		{name: "lower r", in: "r", want: true, consumed: 1},
		{name: "upper R", in: "R", want: true, consumed: 1},
		// 控制台关闭行输入后，回车只产生 '\r'。
		{name: "CR", in: "\r", want: false, consumed: 1},
		{name: "LF", in: "\n", want: false, consumed: 1},
		{name: "CRLF stops at CR", in: "\r\n", want: false, consumed: 1},
		{name: "EOF", in: "", want: false, consumed: 0},
		{name: "other keys then LF", in: "xyz\n", want: false, consumed: 4},
		{name: "other keys then R", in: "xyzR", want: true, consumed: 4},
		{name: "space then r", in: " r", want: true, consumed: 2},
		// 先回车后 R：以先读到的为准。
		{name: "LF before r", in: "\nr", want: false, consumed: 1},
		{name: "CR before r", in: "\rr", want: false, consumed: 1},
		{name: "rr reads one byte", in: "rr", want: true, consumed: 1},
		// 多字节 UTF-8 的各字节都不小于 0x80，不会被当成 R 或回车，逐字节跳过。
		{name: "multibyte then LF", in: "重\n", want: false, consumed: 4},
		{name: "multibyte then r", in: "重r", want: true, consumed: 4},
		{name: "q then LF", in: "q\n", want: false, consumed: 2},
		// 控制台关闭 ENABLE_PROCESSED_INPUT 后，Ctrl+C 以 0x03 读入，按退出处理。
		{name: "Ctrl+C", in: "\x03", want: false, consumed: 1},
		{name: "other key then Ctrl+C", in: "x\x03r", want: false, consumed: 2},
		{name: "Ctrl+C before r", in: "\x03r", want: false, consumed: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := strings.NewReader(tt.in)
			if got := readRetryChoice(r); got != tt.want {
				t.Fatalf("readRetryChoice(%q) = %v, want %v", tt.in, got, tt.want)
			}
			if got := len(tt.in) - r.Len(); got != tt.consumed {
				t.Fatalf("readRetryChoice(%q) consumed %d bytes, want %d", tt.in, got, tt.consumed)
			}
		})
	}
}

// stepReader 第 i 次 Read 原样返回 steps[i]（data 至多 1 字节），用完后返回 io.EOF。
type stepReader struct {
	steps []readStep
}

type readStep struct {
	data string
	err  error
}

func (s *stepReader) Read(p []byte) (int, error) {
	if len(s.steps) == 0 {
		return 0, io.EOF
	}
	st := s.steps[0]
	s.steps = s.steps[1:]
	return copy(p, st.data), st.err
}

// 读出错或 EOF 即停，此后的输入不再读；与错误同一次返回的字节仍须判定（io.Reader 的约定）。
//
// 变异自证（隔离副本，实测）：
//   - 只在 EOF 时停、其他错误继续读 → "error then r" 返回 true，红；
//   - 只在非 EOF 错误时停、EOF 后继续读 → "EOF then r"、"other byte with EOF" 返回 true，红；
//   - 出错返回 true → "error"、"error then r"、"EOF then r"、"other byte with EOF" 返回 true，红；
//   - 先看错误、后看同一次返回的字节 → "r with EOF"、"r with error" 返回 false，红。
func TestReadRetryChoiceReadErrors(t *testing.T) {
	boom := errors.New("boom")
	tests := []struct {
		name  string
		steps []readStep
		want  bool
	}{
		{name: "error", steps: []readStep{{err: boom}}, want: false},
		{name: "error then r", steps: []readStep{{err: boom}, {data: "r"}}, want: false},
		{name: "EOF then r", steps: []readStep{{err: io.EOF}, {data: "r"}}, want: false},
		{name: "r with EOF", steps: []readStep{{data: "r", err: io.EOF}}, want: true},
		{name: "r with error", steps: []readStep{{data: "R", err: boom}}, want: true},
		{name: "other byte with EOF", steps: []readStep{{data: "x", err: io.EOF}, {data: "r"}}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := readRetryChoice(&stepReader{steps: tt.steps}); got != tt.want {
				t.Fatalf("readRetryChoice = %v, want %v", got, tt.want)
			}
		})
	}
}
