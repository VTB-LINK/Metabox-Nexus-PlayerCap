package logger

import "log"

// Logger 统一日志输出
type Logger struct {
	module string
}

// New 创建指定模块的 Logger
func New(module string) *Logger {
	return &Logger{module: module}
}

// Info 输出信息（[*]）
func (l *Logger) Info(format string, args ...interface{}) {
	log.Printf("[%s] [*] "+format, append([]interface{}{l.module}, args...)...)
}

// Success 输出成功（[✓]）
func (l *Logger) Success(format string, args ...interface{}) {
	log.Printf("[%s] [✓] "+format, append([]interface{}{l.module}, args...)...)
}

// Warn 输出警告（[!]）
func (l *Logger) Warn(format string, args ...interface{}) {
	log.Printf("[%s] [!] "+format, append([]interface{}{l.module}, args...)...)
}

// Error 输出错误（[✗]）
func (l *Logger) Error(format string, args ...interface{}) {
	log.Printf("[%s] [✗] "+format, append([]interface{}{l.module}, args...)...)
}

// Detail 输出详细信息（[+]）
func (l *Logger) Detail(format string, args ...interface{}) {
	log.Printf("[%s] [+] "+format, append([]interface{}{l.module}, args...)...)
}
