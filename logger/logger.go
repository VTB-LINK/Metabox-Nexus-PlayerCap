package logger

import (
	"fmt"
	"log"

	"Metabox-Nexus-PlayerCap/i18n"
)

// Logger 统一日志输出
type Logger struct {
	module string
}

// New 创建指定模块的 Logger
func New(module string) *Logger {
	return &Logger{module: module}
}

// line 拼出一行日志：「[模块] [图标] 」之后接正文。
//
// 正文单独格式化（format 先过 i18n.T，再代入 args），前缀不进格式串、模块名不当实参。
// 别改回 log.Printf("[%s] [*] "+i18n.T(format), 模块名, args...)：那样模块名占了第一个实参，
// 正文里带显式序号的占位符全部错位——%[1]s 读到模块名，%[2]s 读到调用方的第一个实参，最后一个
// 实参没人读（英文原文里 server/router.go 的六条「%[1]s player [%[2]s] …」曾因此输出错的内容）。
// 不带实参时也照常格式化，%% 才会折成 %。✅ logger/bodyformat_test.go
func (l *Logger) line(icon, format string, args []interface{}) string {
	return "[" + l.module + "] [" + icon + "] " + fmt.Sprintf(i18n.T(format), args...)
}

// Info 输出信息（[*]）
func (l *Logger) Info(format string, args ...interface{}) {
	log.Print(l.line("*", format, args))
}

// Success 输出成功（[✓]）
func (l *Logger) Success(format string, args ...interface{}) {
	log.Print(l.line("✓", format, args))
}

// Warn 输出警告（[!]）
func (l *Logger) Warn(format string, args ...interface{}) {
	log.Print(l.line("!", format, args))
}

// Error 输出错误（[✗]）
func (l *Logger) Error(format string, args ...interface{}) {
	log.Print(l.line("✗", format, args))
}

// Detail 输出详细信息（[+]）
func (l *Logger) Detail(format string, args ...interface{}) {
	log.Print(l.line("+", format, args))
}
