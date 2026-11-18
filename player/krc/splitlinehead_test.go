package krc

// 本文件只测 splitLineHead 与它取代的行首正则 ^\[(\d+),(\d+)\](.*)$ 逐例等价（输入不含 "\n"，
// 同出货代码里按换行切开后的行）。

import (
	"math/rand/v2"
	"regexp"
	"strings"
	"testing"
)

var lineHeadRegex = regexp.MustCompile(`^\[(\d+),(\d+)\](.*)$`)

func checkLineHead(t *testing.T, raw string) {
	t.Helper()
	start, dur, body, ok := splitLineHead(raw)
	m := lineHeadRegex.FindStringSubmatch(raw)
	if (m != nil) != ok || ok && (m[1] != start || m[2] != dur || m[3] != body) {
		t.Fatalf("%q：splitLineHead = (%q, %q, %q, %v)，正则 = %q", raw, start, dur, body, ok, m)
	}
}

func TestSplitLineHeadMatchesRegex(t *testing.T) {
	for _, raw := range []string{
		"", "[", "]", "[]", "[,]", "[0,1]", "[0,1]a", "[0,1]]x", "[00,0010]<0,1,0>a",
		"[1,2,3]x", "[a,1]x", "[1,a]x", "[1,]x", "[,1]x", "[1 ,2]x", "[1, 2]x", "[１,2]x",
		"[1,2", "x[1,2]y", "[[1,2]x", "[1,2][3,4]x", "[1,2]\r中\x85文", "[1,2]\xff\xfe", "[\xff,2]x",
		"[ti:t]", "[language:abc]", "[-1,2]x", "[+1,2]x", "[1.5,2]x", "[99999999999999999999,1]x",
	} {
		checkLineHead(t, raw)
	}

	alphabet := []string{"[", "]", ",", "0", "9", "a", " ", "<", "\r", "\xff", "中", "１"}
	r := rand.New(rand.NewPCG(205, 1))
	var b strings.Builder
	for range 200000 {
		b.Reset()
		for range r.IntN(10) {
			b.WriteString(alphabet[r.IntN(len(alphabet))])
		}
		checkLineHead(t, b.String())
	}
}
