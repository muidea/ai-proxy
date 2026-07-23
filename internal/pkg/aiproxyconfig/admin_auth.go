package config

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// adminBasePathSegmentRE 匹配 RFC 3986 unreserved 路径段: ALPHA / DIGIT / "-" / "." / "_" / "~"
var adminBasePathSegmentRE = regexp.MustCompile(`^[A-Za-z0-9._~-]+$`)

// normalizeAdminAuth 填充默认 basePath/TTL,并做轻量规范化。
// 敏感值不写入日志。
func normalizeAdminAuth(auth *AdminAuthConfig) error {
	if auth == nil {
		return nil
	}
	if strings.TrimSpace(auth.BasePath) == "" {
		auth.BasePath = DefaultAdminBasePath
	}
	auth.BasePath = strings.TrimSpace(auth.BasePath)
	auth.Username = strings.TrimSpace(auth.Username)
	auth.PasswordHash = strings.TrimSpace(auth.PasswordHash)
	if auth.SessionTTLSeconds == 0 {
		auth.SessionTTLSeconds = DefaultAdminSessionTTLSeconds
	}
	return nil
}

// validateAdminAuth 在启动期校验 Admin 登录安全配置。
// 关闭时:账号/哈希若非空仅 warning,不启用认证。
// 开启时:账号、哈希与 TTL 必须合法,否则 fail-fast。
func validateAdminAuth(auth AdminAuthConfig) error {
	if err := validateAdminBasePath(auth.BasePath); err != nil {
		return err
	}
	if auth.SessionTTLSeconds < MinAdminSessionTTLSeconds || auth.SessionTTLSeconds > MaxAdminSessionTTLSeconds {
		return fmt.Errorf("admin_session_ttl_seconds must be between %d and %d", MinAdminSessionTTLSeconds, MaxAdminSessionTTLSeconds)
	}
	if !auth.Enabled {
		if auth.Username != "" || auth.PasswordHash != "" {
			slog.Warn("admin auth credentials configured while admin_auth_enabled is false; credentials are ignored")
		}
		return nil
	}
	if err := validateAdminUsername(auth.Username); err != nil {
		return err
	}
	if auth.PasswordHash == "" {
		return fmt.Errorf("admin_password_hash is required when admin_auth_enabled is true")
	}
	if _, err := ParseAdminPasswordHash(auth.PasswordHash); err != nil {
		// 错误不得回显哈希原文。
		return fmt.Errorf("admin_password_hash is invalid")
	}
	return nil
}

// validateAdminBasePath 校验 Admin basePath。
// 必须以单个 / 开始、不以 / 结束、长度 <=128、无 // . .. % ? #,各段仅 unreserved。
func validateAdminBasePath(path string) error {
	if path == "" {
		return fmt.Errorf("admin_base_path is required")
	}
	if !strings.HasPrefix(path, "/") {
		return fmt.Errorf("admin_base_path must start with /")
	}
	if path != "/" && strings.HasSuffix(path, "/") {
		return fmt.Errorf("admin_base_path must not end with /")
	}
	if len(path) > MaxAdminBasePathLength {
		return fmt.Errorf("admin_base_path length must be <= %d", MaxAdminBasePathLength)
	}
	if strings.Contains(path, "//") {
		return fmt.Errorf("admin_base_path must not contain //")
	}
	if strings.ContainsAny(path, "%?#") {
		return fmt.Errorf("admin_base_path must not contain %%, ? or #")
	}
	// 去掉前导 / 后按段检查。
	trimmed := strings.TrimPrefix(path, "/")
	if trimmed == "" {
		return fmt.Errorf("admin_base_path must not be empty path")
	}
	for _, seg := range strings.Split(trimmed, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("admin_base_path path segment is invalid")
		}
		if !adminBasePathSegmentRE.MatchString(seg) {
			return fmt.Errorf("admin_base_path path segment contains invalid characters")
		}
	}
	return nil
}

// validateAdminUsername 校验单管理员账号。
func validateAdminUsername(username string) error {
	if username == "" {
		return fmt.Errorf("admin_username is required when admin_auth_enabled is true")
	}
	if n := utf8.RuneCountInString(username); n < 1 || n > MaxAdminUsernameLength {
		return fmt.Errorf("admin_username length must be 1~%d", MaxAdminUsernameLength)
	}
	if strings.TrimSpace(username) != username {
		return fmt.Errorf("admin_username must not have leading or trailing whitespace")
	}
	if strings.Contains(username, ":") {
		return fmt.Errorf("admin_username must not contain ':'")
	}
	for _, r := range username {
		if unicode.IsControl(r) {
			return fmt.Errorf("admin_username must not contain control characters")
		}
	}
	return nil
}

// AdminPasswordHash 是解析后的 Argon2id PHC 参数。
type AdminPasswordHash struct {
	Memory      uint32
	Time        uint32
	Parallelism uint8
	Salt        []byte
	Hash        []byte
}

// ParseAdminPasswordHash 解析并校验唯一允许的 Argon2id PHC 格式。
// 只接受 m=65536,t=3,p=1、salt>=16、hash>=32。错误消息不得包含哈希原文。
func ParseAdminPasswordHash(phc string) (AdminPasswordHash, error) {
	// 期望: $argon2id$v=19$m=65536,t=3,p=1$<salt>$<hash>
	// 也兼容设计文档示例中 argon2id$v=19$... 无前导 $ 的写法。
	raw := strings.TrimSpace(phc)
	if raw == "" {
		return AdminPasswordHash{}, fmt.Errorf("empty")
	}
	if !strings.HasPrefix(raw, "$") {
		raw = "$" + raw
	}
	parts := strings.Split(raw, "$")
	// "", "argon2id", "v=19", "m=...,t=...,p=...", salt, hash
	if len(parts) != 6 {
		return AdminPasswordHash{}, fmt.Errorf("invalid format")
	}
	if parts[1] != "argon2id" {
		return AdminPasswordHash{}, fmt.Errorf("unsupported algorithm")
	}
	if parts[2] != "v=19" {
		return AdminPasswordHash{}, fmt.Errorf("unsupported version")
	}
	params := parts[3]
	var memory, timeCost uint32
	var parallelism uint8
	for _, field := range strings.Split(params, ",") {
		kv := strings.SplitN(field, "=", 2)
		if len(kv) != 2 {
			return AdminPasswordHash{}, fmt.Errorf("invalid params")
		}
		switch kv[0] {
		case "m":
			n, err := strconv.ParseUint(kv[1], 10, 32)
			if err != nil {
				return AdminPasswordHash{}, fmt.Errorf("invalid memory")
			}
			memory = uint32(n)
		case "t":
			n, err := strconv.ParseUint(kv[1], 10, 32)
			if err != nil {
				return AdminPasswordHash{}, fmt.Errorf("invalid time")
			}
			timeCost = uint32(n)
		case "p":
			n, err := strconv.ParseUint(kv[1], 10, 8)
			if err != nil {
				return AdminPasswordHash{}, fmt.Errorf("invalid parallelism")
			}
			parallelism = uint8(n)
		default:
			return AdminPasswordHash{}, fmt.Errorf("unknown param")
		}
	}
	if memory != AdminArgon2MemoryKiB || timeCost != AdminArgon2Time || parallelism != AdminArgon2Parallelism {
		return AdminPasswordHash{}, fmt.Errorf("unsupported parameters")
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		// 兼容标准 base64 填充。
		salt, err = base64.StdEncoding.DecodeString(parts[4])
		if err != nil {
			return AdminPasswordHash{}, fmt.Errorf("invalid salt")
		}
	}
	hash, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		hash, err = base64.StdEncoding.DecodeString(parts[5])
		if err != nil {
			return AdminPasswordHash{}, fmt.Errorf("invalid hash")
		}
	}
	if len(salt) < AdminArgon2MinSaltBytes {
		return AdminPasswordHash{}, fmt.Errorf("salt too short")
	}
	if len(hash) < AdminArgon2MinKeyBytes {
		return AdminPasswordHash{}, fmt.Errorf("hash too short")
	}
	return AdminPasswordHash{
		Memory:      memory,
		Time:        timeCost,
		Parallelism: parallelism,
		Salt:        salt,
		Hash:        hash,
	}, nil
}

// HashAdminPassword 使用固定 Argon2id 参数生成 PHC 字符串。
// 随机 salt 来自 crypto/rand。
func HashAdminPassword(password string) (string, error) {
	if password == "" {
		return "", fmt.Errorf("password is required")
	}
	salt := make([]byte, AdminArgon2MinSaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}
	hash := argon2.IDKey([]byte(password), salt, AdminArgon2Time, AdminArgon2MemoryKiB, AdminArgon2Parallelism, AdminArgon2MinKeyBytes)
	return formatAdminPasswordPHC(salt, hash), nil
}

func formatAdminPasswordPHC(salt, hash []byte) string {
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s",
		AdminArgon2MemoryKiB,
		AdminArgon2Time,
		AdminArgon2Parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(hash),
	)
}

// VerifyAdminPassword 使用常量时间比较校验密码与 PHC 哈希。
// 任何解析或匹配失败都返回 false,不区分原因。
func VerifyAdminPassword(password, phc string) bool {
	parsed, err := ParseAdminPasswordHash(phc)
	if err != nil {
		return false
	}
	derived := argon2.IDKey([]byte(password), parsed.Salt, parsed.Time, parsed.Memory, parsed.Parallelism, uint32(len(parsed.Hash)))
	if len(derived) != len(parsed.Hash) {
		return false
	}
	return subtle.ConstantTimeCompare(derived, parsed.Hash) == 1
}

// ConstantTimeUsernameEqual 使用常量时间比较管理员账号。
func ConstantTimeUsernameEqual(a, b string) bool {
	if len(a) != len(b) {
		// 长度不同时仍做一次 dummy 比较,降低计时侧信道。
		_ = subtle.ConstantTimeCompare([]byte(a), []byte(a))
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// AdminAuthFingerprint 返回用于检测认证相关配置是否变化的稳定摘要键。
// 不包含敏感值本身的明文形式以外的用途;仅用于比较是否需要清空会话。
func AdminAuthFingerprint(auth AdminAuthConfig) string {
	return strings.Join([]string{
		strconv.FormatBool(auth.Enabled),
		auth.BasePath,
		auth.Username,
		auth.PasswordHash,
		strconv.FormatBool(auth.SessionCookieSecure),
		strconv.Itoa(auth.SessionTTLSeconds),
	}, "\x00")
}
