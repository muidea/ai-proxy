package aiproxy

import (
	"bufio"
	"errors"
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
	if len(args) == 0 || args[0] != "admin" {
		return 0, false
	}
	if len(args) == 1 || args[1] == "-h" || args[1] == "--help" {
		printAdminCommandUsage()
		return 0, true
	}
	switch args[1] {
	case "password-hash":
		if len(args) == 3 && (args[2] == "-h" || args[2] == "--help") {
			fmt.Fprintln(os.Stderr, "usage: ai-proxy admin password-hash")
			return 0, true
		}
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: ai-proxy admin password-hash")
			return 2, true
		}
		return runAdminPasswordHash(), true
	case "set-credentials":
		return runAdminSetCredentials(args[2:]), true
	default:
		fmt.Fprintf(os.Stderr, "unknown admin subcommand %q\n", args[1])
		printAdminCommandUsage()
		return 2, true
	}
}

func printAdminCommandUsage() {
	fmt.Fprintln(os.Stderr, "Admin commands:")
	fmt.Fprintln(os.Stderr, "  ai-proxy admin password-hash")
	fmt.Fprintln(os.Stderr, "      Interactively generate an Argon2id password hash.")
	fmt.Fprintln(os.Stderr, "  ai-proxy admin set-credentials --username <username> [--config <config.yaml>]")
	fmt.Fprintln(os.Stderr, "      Create or reset Admin login credentials and enable Admin authentication.")
}

// runAdminPasswordHash 从交互式 TTY 两次读取密码(关闭回显),
// 确认一致后向 stdout 输出一行 Argon2id PHC 字符串。
// 密码不得作为命令行参数、环境变量、日志或错误消息的一部分。
func runAdminPasswordHash() int {
	phc, err := promptAdminPasswordHash()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println(phc)
	return 0
}

// promptAdminPasswordHash 从交互式 TTY 两次读取密码并生成 PHC 哈希。
// 错误信息不包含密码或哈希。
func promptAdminPasswordHash() (string, error) {
	fd := int(os.Stdin.Fd())
	if !isTerminal(fd) {
		return "", errors.New("admin password operation requires an interactive TTY")
	}
	fmt.Fprint(os.Stderr, "New admin password: ")
	first, err := readPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", errors.New("failed to read password")
	}
	fmt.Fprint(os.Stderr, "Confirm admin password: ")
	second, err := readPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		zeroBytes(first)
		return "", errors.New("failed to read password confirmation")
	}
	if string(first) != string(second) {
		zeroBytes(first)
		zeroBytes(second)
		return "", errors.New("passwords do not match")
	}
	if len(first) == 0 {
		zeroBytes(first)
		zeroBytes(second)
		return "", errors.New("password must not be empty")
	}
	password := string(first)
	zeroBytes(first)
	zeroBytes(second)
	phc, err := config.HashAdminPassword(password)
	// 尽快丢弃明文。
	password = strings.Repeat("\x00", len(password))
	_ = password
	if err != nil {
		return "", errors.New("failed to hash password")
	}
	return phc, nil
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
