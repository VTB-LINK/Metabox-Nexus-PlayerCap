// Package wsdial 是各播放器 CDP 客户端共用的 WebSocket 握手（#231）：握手随 ctx 中止，停止不被挂住的握手挡住。
//
// gorilla/websocket 的 DialContext 只按 ctx 的期限设握手的读写期限，ctx 被取消打断不了进行中的握手：对端接下连接而
// 不回应时，取消之后一直等到期限（用例实测）。Dial 在 ctx 结束时关掉底层连接。kugou、cloudmusicv3、spotify、sodamusic、
// kuwo 的 cdp.Connect 经它握手。
package wsdial

import (
	"context"
	"net"

	"github.com/gorilla/websocket"
)

// Dial 以 websocket.DefaultDialer 的设置（代理取环境变量，握手上限 45 s）对 url 握手，ctx 结束即中止。握手完成之后
// ctx 再结束不影响返回的连接。
func Dial(ctx context.Context, url string) (*websocket.Conn, error) {
	var stop func() bool
	d := *websocket.DefaultDialer
	d.NetDialContext = func(dctx context.Context, network, addr string) (net.Conn, error) {
		c, err := (&net.Dialer{}).DialContext(dctx, network, addr)
		if err == nil {
			stop = context.AfterFunc(ctx, func() { c.Close() })
		}
		return c, err
	}
	conn, _, err := d.DialContext(ctx, url, nil)
	if stop != nil && !stop() && err == nil { // 握手刚完成时 ctx 恰好结束，底层连接已被关掉
		conn.Close()
		return nil, ctx.Err()
	}
	return conn, err
}
