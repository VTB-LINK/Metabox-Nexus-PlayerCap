package assoc

// 本文件只测 readCandidates 从加密的仿酷狗库里取出的候选：设计 §2.3 四种样本与 collect 的期望一致，
// 没有条目的 hash 得到空候选。读库的时序与失败判定见 dbreadconsistency_test.go。

import (
	"context"
	"strings"
	"testing"
)

func TestReadCandidatesEncryptedFixture(t *testing.T) {
	for _, s := range designSamples {
		t.Run(s.name, func(t *testing.T) {
			m := encMemFS(t, fixtureFor(t, s.associated))
			got, err := readCandidates(context.Background(), testDBPath, s.hash, m.fs())
			if err != nil {
				t.Fatalf("readCandidates: %v", err)
			}
			wantCandidates(t, got.cands, s.want)
			if m.opens != 2 {
				t.Errorf("opened %d times, want 2 (two passes)", m.opens)
			}
			m.wantClosed(t)
		})
	}
}

// 反例：大写的 hash 照样取到；库里没有条目的 hash 得到空候选、不是错误。
func TestReadCandidatesEncryptedFixtureOtherHashes(t *testing.T) {
	hash, want := tonghuaWant(t)
	for _, c := range []struct {
		name string
		hash string
		want []candidate
	}{
		{"uppercase", strings.ToUpper(hash), want},
		{"no entries", hex32("ee", 1), nil},
		{"empty", "", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := encMemFS(t, fixturePlain(t, "kugou_like.db"))
			got, err := readCandidates(context.Background(), testDBPath, c.hash, m.fs())
			if err != nil {
				t.Fatalf("readCandidates: %v", err)
			}
			wantCandidates(t, got.cands, c.want)
		})
	}
}
