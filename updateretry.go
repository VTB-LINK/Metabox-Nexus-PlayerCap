package main

import (
	"Metabox-Nexus-PlayerCap/i18n"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/windows"
)

// askUpdateRetry 在自动更新失败后打印提示并等待选择，返回是否重试（#216）。
//
// 标准输入是控制台时，等待期间关闭行输入与回显（关闭行输入时回显也须关闭），单键即生效、不必回车；
// 返回前恢复原控制台模式，恢复失败不报错。
//
// 同时关闭 ENABLE_PROCESSED_INPUT，使 Ctrl+C 以字节 0x03 读入、按退出处理。走到这里时
// signal.Notify 尚未注册，Ctrl+C 若照常作为信号，会由系统默认处理直接结束进程，defer 不执行，
// 控制台停在关闭行输入与回显的模式；从终端启动时，外层 shell 可能随之没有回显、不能按行输入。
// Ctrl+Break 不受此模式位控制，照旧作为信号结束进程，原模式同样不恢复。
//
// 改完模式后清空控制台输入缓冲：下载期间先按下的键（例如回车）不算作选择。清空失败不报错。
//
// 不是控制台（管道、重定向）时按字节读，行为同 readRetryChoice；是控制台但改不了模式时同样按字节读，
// 输入要等回车后才读得到。
func askUpdateRetry() bool {
	fmt.Println(i18n.T("\nPress R to retry, or Enter to exit. You can also reopen this program later to retry."))
	h := windows.Handle(os.Stdin.Fd())
	var mode uint32
	if windows.GetConsoleMode(h, &mode) == nil &&
		windows.SetConsoleMode(h, mode&^(windows.ENABLE_LINE_INPUT|windows.ENABLE_ECHO_INPUT|windows.ENABLE_PROCESSED_INPUT)) == nil {
		defer windows.SetConsoleMode(h, mode)
		windows.FlushConsoleInputBuffer(h)
	}
	return readRetryChoice(os.Stdin)
}

// readRetryChoice 从 r 逐字节读，返回是否重试：先读到 'r' 或 'R' 返回 true；先读到 '\r'、'\n'
// 或 0x03（Ctrl+C）返回 false；读到 EOF 或出错返回 false；其他字节忽略、继续读。
//
// 每次只读一个字节：判定之后的输入留在原处，不多读。与错误同一次返回的字节先判定。
// 因此管道输入 "r\n" 只消费 'r'，换行留在原处：重试后再次失败时，下一次询问会立即读到它而退出。
// 不为此清空：主播不经管道输入；控制台关闭行输入后按 R 只产生一个字符，没有余下的换行。
func readRetryChoice(r io.Reader) bool {
	var b [1]byte
	for {
		n, err := r.Read(b[:])
		if n == 1 {
			switch b[0] {
			case 'r', 'R':
				return true
			case '\r', '\n', 0x03:
				return false
			}
		}
		if err != nil {
			return false
		}
	}
}
