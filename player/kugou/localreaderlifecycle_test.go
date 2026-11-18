package kugou

// 本文件只测会话侧读取者的启停（#205，设计 §6.7「生命周期」）：生产的启动器在 assoc.Start 返回 nil（上一个会话的
// 读取者还没退出）时返回无类型的 nil，会话据此不读本地歌词；会话的每条退出路径都取消读取者的 ctx。
// assoc.Start 经包级 seam assocStart 替换，不起真读取者、不碰本机酷狗。

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"Metabox-Nexus-PlayerCap/player/kugou/assoc"
	"Metabox-Nexus-PlayerCap/player/kugou/cdp"
)

// assoc.Start 返回 nil 时 startLocalLyrics 返回的接口 == nil：把 (*assoc.Reader)(nil) 装进接口，接口不等于 nil，
// 会话会去调 nil 读取者的方法。反例：assoc.Start 返回读取者时接口非 nil。
//
// 变异自证（实测）：startLocalLyrics 直接 `return assocStart(ctx)`，「返回 nil」一例红。
func TestStarterTypedNil(t *testing.T) {
	orig := assocStart
	defer func() { assocStart = orig }()

	assocStart = func(context.Context) *assoc.Reader { return nil }
	if got := startLocalLyrics(context.Background()); got != nil {
		t.Fatalf("assoc.Start 返回 nil 时应得到无类型的 nil，实得 %#v", got)
	}
	assocStart = func(context.Context) *assoc.Reader { return &assoc.Reader{} }
	if got := startLocalLyrics(context.Background()); got == nil {
		t.Fatal("assoc.Start 返回读取者时不应得到 nil")
	}
}

// closedSource 的连接已断开。
type closedSource struct{}

func (closedSource) GetPlayInfo() (*cdp.PlayInfo, error) { return nil, errors.New("closed") }
func (closedSource) IsClosed() bool                      { return true }

// 会话的三条退出路径（Stop、连接已断开、读数出错）都取消读取者的 ctx；会话进行中不取消（runLocal 另有断言）。
//
// 变异自证（工作树的隔离副本里逐条注入、跑完即还原，实测）：三条退出路径各去掉 localCancel()，`go vet` 都报
// lostcancel，对应子例红；读数出错那条另使 fakelocal_test.go 的 runLocal 断言全红。
func TestSessionCancelsReaderOnExit(t *testing.T) {
	cases := []struct {
		name string
		run  func(p *KuGouPlayer, start localLyricsStarter)
	}{
		{"Stop", func(p *KuGouPlayer, start localLyricsStarter) {
			p.Stop()
			p.runSession(&scriptedSource{}, start)
		}},
		{"连接已断开", func(p *KuGouPlayer, start localLyricsStarter) { p.runSession(closedSource{}, start) }},
		{"读数出错", func(p *KuGouPlayer, start localLyricsStarter) { p.runSession(&scriptedSource{}, start) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newFakeLocal(nil)
				c.run(New(0, 0), f.starter())
				if f.ctx == nil || f.ctx.Err() == nil {
					t.Fatalf("会话返回时读取者的 ctx 应已取消（ctx=%v）", f.ctx)
				}
			})
		})
	}
}
