package aiproxy

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"ai-proxy/internal/pkg/aiproxyconfig"

	"golang.org/x/sys/unix"
)

// tryAdminSubcommand 处理受限的 admin 子命令。
// 当前仅支持: ai-proxy admin password-hash
// 成功处理后返回 (code, true);未匹配时返回 (0, false) 以继续主服务。
func tryAdminSubcommand(args []string) (int, bool) {
	if len(args) < 2 {
		return 0, false
	}
	if args[0] != "admin" {
		return 0, false
	}
	switch args[1] {
	case "password-hash":
		return runAdminPasswordHash(), true
	default:
		fmt.Fprintf(os.Stderr, "unknown admin subcommand %q\n", args[1])
		fmt.Fprintf(os.Stderr, "usage: ai-proxy admin password-hash\n")
		return 2, true
	}
}

// runAdminPasswordHash 从交互式 TTY 两次读取密码(关闭回显),
// 确认一致后向 stdout 输出一行 Argon2id PHC 字符串。
// 密码不得作为命令行参数、环境变量、日志或错误消息的一部分。
func runAdminPasswordHash() int {
	fd := int(os.Stdin.Fd())
	if !isTerminal(fd) {
		fmt.Fprintln(os.Stderr, "admin password-hash requires an interactive TTY")
		return 1
	}
	fmt.Fprint(os.Stderr, "New admin password: ")
	first, err := readPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "failed to read password")
		return 1
	}
	fmt.Fprint(os.Stderr, "Confirm admin password: ")
	second, err := readPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		zeroBytes(first)
		fmt.Fprintln(os.Stderr, "failed to read password confirmation")
		return 1
	}
	if string(first) != string(second) {
		zeroBytes(first)
		zeroBytes(second)
		fmt.Fprintln(os.Stderr, "passwords do not match")
		return 1
	}
	if len(first) == 0 {
		zeroBytes(first)
		zeroBytes(second)
		fmt.Fprintln(os.Stderr, "password must not be empty")
		return 1
	}
	password := string(first)
	zeroBytes(first)
	zeroBytes(second)
	phc, err := config.HashAdminPassword(password)
	// 尽快丢弃明文。
	password = strings.Repeat("\x00", len(password))
	_ = password
	if err != nil {
		fmt.Fprintln(os.Stderr, "failed to hash password")
		return 1
	}
	fmt.Println(phc)
	return 0
}

func isTerminal(fd int) bool {
	_, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	return err == nil
}

func readPassword(fd int) ([]byte, error) {
	old, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return nil, err
	}
	raw := *old
	raw.Lflag &^= unix.ECHO
	raw.Lflag |= unix.ICANON | unix.ISIG
	raw.Iflag |= unix.ICRNL
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &raw); err != nil {
		return nil, err
	}
	defer func() { _ = unix.IoctlSetTermios(fd, unix.TCSETS, old) }()

	reader := bufio.NewReader(os.NewFile(uintptr(fd), "/dev/stdin"))
	line, err := reader.ReadString('\n')
	if err != nil && len(line) == 0 {
		return nil, err
	}
	line = strings.TrimRight(line, "\r\n")
	return []byte(line), nil
}

func zeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
