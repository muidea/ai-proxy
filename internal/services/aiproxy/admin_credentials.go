package aiproxy

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"ai-proxy/internal/pkg/aiproxyconfig"

	"go.yaml.in/yaml/v4"
)

// runAdminSetCredentials 创建或重置唯一的 Admin 登录凭据。密码只经 TTY
// 读取；成功后以原文件权限原子替换 YAML，并自动开启 admin_auth_enabled。
func runAdminSetCredentials(args []string) int {
	fs := flag.NewFlagSet("ai-proxy admin set-credentials", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	username := fs.String("username", "", "Admin username")
	configPath := fs.String("config", os.Getenv("AI_PROXY_CONFIG"), "config file path")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || strings.TrimSpace(*username) == "" {
		fmt.Fprintln(os.Stderr, "usage: ai-proxy admin set-credentials --username <username> [--config <config.yaml>]")
		return 2
	}
	path := config.ResolvePath(*configPath)
	if strings.TrimSpace(path) == "" {
		fmt.Fprintln(os.Stderr, "no active config file; pass --config <config.yaml>")
		return 1
	}
	passwordHash, err := promptAdminPasswordHash()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := setAdminCredentials(path, strings.TrimSpace(*username), passwordHash); err != nil {
		fmt.Fprintln(os.Stderr, "failed to save admin credentials:", err)
		return 1
	}
	fmt.Fprintf(os.Stdout, "Admin credentials saved in %s. Restart ai-proxy to apply them.\n", path)
	return 0
}

// setAdminCredentials 只修改 server.admin_auth_enabled、admin_username 与
// admin_password_hash。先对临时文件执行完整配置校验，再原子替换正式文件。
func setAdminCredentials(path, username, passwordHash string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	root, err := yamlDocumentRoot(&document)
	if err != nil {
		return err
	}
	server := yamlMappingValue(root, "server")
	if server == nil {
		server = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		yamlSetMappingValue(root, "server", server)
	}
	if server.Kind != yaml.MappingNode {
		return errors.New("server must be a mapping")
	}
	yamlSetMappingValue(server, "admin_auth_enabled", yamlScalar("true", "!!bool"))
	yamlSetMappingValue(server, "admin_username", yamlScalar(username, "!!str"))
	yamlSetMappingValue(server, "admin_password_hash", yamlScalar(passwordHash, "!!str"))

	var encoded bytes.Buffer
	encoder := yaml.NewEncoder(&encoded)
	encoder.SetIndent(2)
	if err := encoder.Encode(&document); err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return fmt.Errorf("close config encoder: %w", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat config: %w", err)
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".ai-proxy-config-*.yaml")
	if err != nil {
		return fmt.Errorf("create temporary config: %w", err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(info.Mode().Perm()); err != nil {
		_ = temp.Close()
		return fmt.Errorf("set temporary config mode: %w", err)
	}
	if _, err := temp.Write(encoded.Bytes()); err != nil {
		_ = temp.Close()
		return fmt.Errorf("write temporary config: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close temporary config: %w", err)
	}
	if _, err := config.Load(tempPath); err != nil {
		return fmt.Errorf("configuration rejected: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}
	return nil
}

func yamlDocumentRoot(document *yaml.Node) (*yaml.Node, error) {
	if document.Kind != yaml.DocumentNode || len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("config must contain one YAML mapping document")
	}
	return document.Content[0], nil
}

func yamlMappingValue(mapping *yaml.Node, key string) *yaml.Node {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

func yamlSetMappingValue(mapping *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content[i+1] = value
			return
		}
	}
	mapping.Content = append(mapping.Content, yamlMappingKey(key), value)
}

func yamlMappingKey(value string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
}

func yamlScalar(value, tag string) *yaml.Node {
	node := &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: value}
	if tag == "!!str" {
		node.Style = yaml.DoubleQuotedStyle
	}
	return node
}
