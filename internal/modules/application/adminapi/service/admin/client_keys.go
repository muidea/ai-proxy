package admin

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"ai-proxy/internal/pkg/aiproxyconfig"

	"go.yaml.in/yaml/v4"
)

type clientKeyView struct {
	ID               string `json:"id"`
	Enabled          bool   `json:"enabled"`
	CredentialSource string `json:"credential_source"`
	KeyConfigured    bool   `json:"key_configured"`
}

type createClientKeyRequest struct {
	ID      string `json:"id"`
	Enabled *bool  `json:"enabled"`
}

type updateClientKeyRequest struct {
	Enabled *bool `json:"enabled"`
}

func (h *Handler) listClientAPIKeys(w http.ResponseWriter) {
	cfg := h.runtime.ConfigSnapshot()
	keys := clientKeyViews(cfg)
	writeJSON(w, http.StatusOK, map[string]any{"client_api_keys": keys, "writable": strings.TrimSpace(h.configPath) != "", "hot_reload": true})
}

func clientKeyViews(cfg config.Config) []clientKeyView {
	ids := make([]string, 0, len(cfg.ClientAPIKeys))
	for id := range cfg.ClientAPIKeys {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	result := make([]clientKeyView, 0, len(ids))
	for _, id := range ids {
		key := cfg.ClientAPIKeys[id]
		result = append(result, clientKeyView{ID: id, Enabled: key.Enabled, CredentialSource: credentialSource(key), KeyConfigured: key.APIKey != "" || key.APIKeyHash != ""})
	}
	return result
}

func credentialSource(key config.ClientAPIKey) string {
	if key.APIKeyHash != "" {
		return "managed"
	}
	return "external"
}

func (h *Handler) createClientAPIKey(w http.ResponseWriter, r *http.Request) {
	if !requireAdminWrite(w, r) {
		return
	}
	var input createClientKeyRequest
	if !decodeAdminJSON(w, r, &input) {
		return
	}
	id := strings.ToLower(strings.TrimSpace(input.ID))
	if id == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	enabled := true
	if input.Enabled != nil {
		enabled = *input.Enabled
	}
	secret, hash, err := generateClientKey()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "generate client API key")
		return
	}
	h.updateMu.Lock()
	defer h.updateMu.Unlock()
	if _, ok := h.runtime.ConfigSnapshot().ClientAPIKeys[id]; ok {
		writeError(w, http.StatusConflict, "client API key id already exists")
		return
	}
	cfg, err := mutateClientKeys(h.configPath, func(node *yaml.Node) error {
		if mappingValue(node, id) != nil {
			return errors.New("client API key id already exists")
		}
		entry := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		appendScalar(entry, "api_key_hash", hash, "!!str")
		appendScalar(entry, "enabled", fmt.Sprintf("%t", enabled), "!!bool")
		node.Content = append(node.Content, mappingKey(id), entry)
		return nil
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.runtime.UpdateConfig(cfg); err != nil {
		writeError(w, http.StatusInternalServerError, "activate config: "+err.Error())
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "enabled": enabled, "api_key": secret, "message": "Copy this API key now. It cannot be displayed again."})
}

func (h *Handler) clientAPIKeyAction(w http.ResponseWriter, r *http.Request) {
	if !requireAdminWrite(w, r) {
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/admin/api/client-api-keys/")
	rotate := strings.HasSuffix(rest, "/rotate")
	id := strings.TrimSuffix(rest, "/rotate")
	id = strings.ToLower(strings.TrimSpace(strings.Trim(id, "/")))
	if id == "" || id == config.ReservedClientAPIKeyID {
		writeError(w, http.StatusBadRequest, "invalid client API key id")
		return
	}
	h.updateMu.Lock()
	defer h.updateMu.Unlock()
	if _, ok := h.runtime.ConfigSnapshot().ClientAPIKeys[id]; !ok {
		writeError(w, http.StatusNotFound, "client API key not found")
		return
	}
	switch {
	case rotate && r.Method == http.MethodPost:
		secret, hash, err := generateClientKey()
		if err != nil {
			writeError(w, 500, "generate client API key")
			return
		}
		cfg, err := mutateClientKeys(h.configPath, func(node *yaml.Node) error {
			entry := mappingValue(node, id)
			if entry == nil {
				return errors.New("client API key not found")
			}
			removeMappingValue(entry, "api_key")
			setMappingValue(entry, "api_key_hash", scalar(hash, "!!str"))
			return nil
		})
		if err != nil {
			writeError(w, 400, err.Error())
			return
		}
		if err := h.runtime.UpdateConfig(cfg); err != nil {
			writeError(w, 500, "activate config: "+err.Error())
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, 200, map[string]any{"id": id, "api_key": secret, "message": "Copy this API key now. It cannot be displayed again."})
	case !rotate && r.Method == http.MethodPatch:
		var input updateClientKeyRequest
		if !decodeAdminJSON(w, r, &input) {
			return
		}
		if input.Enabled == nil {
			writeError(w, 400, "enabled is required")
			return
		}
		cfg, err := mutateClientKeys(h.configPath, func(node *yaml.Node) error {
			entry := mappingValue(node, id)
			if entry == nil {
				return errors.New("client API key not found")
			}
			setMappingValue(entry, "enabled", scalar(fmt.Sprintf("%t", *input.Enabled), "!!bool"))
			return nil
		})
		if err != nil {
			writeError(w, 400, err.Error())
			return
		}
		if err := h.runtime.UpdateConfig(cfg); err != nil {
			writeError(w, 500, "activate config: "+err.Error())
			return
		}
		writeJSON(w, 200, clientKeyView{ID: id, Enabled: *input.Enabled, CredentialSource: credentialSource(cfg.ClientAPIKeys[id]), KeyConfigured: cfg.ClientAPIKeys[id].APIKey != "" || cfg.ClientAPIKeys[id].APIKeyHash != ""})
	case !rotate && r.Method == http.MethodDelete:
		cfg, err := mutateClientKeys(h.configPath, func(node *yaml.Node) error { removeMappingValue(node, id); return nil })
		if err != nil {
			writeError(w, 400, err.Error())
			return
		}
		if err := h.runtime.UpdateConfig(cfg); err != nil {
			writeError(w, 500, "activate config: "+err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

func requireAdminWrite(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("X-AI-Proxy-Admin") != "1" {
		writeError(w, http.StatusForbidden, "missing admin request header")
		return false
	}
	return true
}
func decodeAdminJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		writeError(w, 400, "invalid request: "+err.Error())
		return false
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		writeError(w, 400, "invalid request: multiple JSON values")
		return false
	}
	return true
}

func generateClientKey() (string, string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", err
	}
	key := "aip_" + base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(key))
	return key, "sha256:" + hex.EncodeToString(sum[:]), nil
}

func mutateClientKeys(path string, mutate func(*yaml.Node) error) (config.Config, error) {
	if strings.TrimSpace(path) == "" {
		return config.Config{}, errors.New("no writable config file is active")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return config.Config{}, fmt.Errorf("read config: %w", err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return config.Config{}, fmt.Errorf("parse config: %w", err)
	}
	root, err := documentRoot(&doc)
	if err != nil {
		return config.Config{}, err
	}
	node := mappingValue(root, "client_api_keys")
	if node == nil {
		node = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		setMappingValue(root, "client_api_keys", node)
	}
	if node.Kind != yaml.MappingNode {
		return config.Config{}, errors.New("client_api_keys must be a mapping")
	}
	if err := mutate(node); err != nil {
		return config.Config{}, err
	}
	var encoded bytes.Buffer
	enc := yaml.NewEncoder(&encoded)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return config.Config{}, err
	}
	_ = enc.Close()
	info, err := os.Stat(path)
	if err != nil {
		return config.Config{}, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".ai-proxy-config-*.yaml")
	if err != nil {
		return config.Config{}, err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(info.Mode().Perm()); err != nil {
		_ = tmp.Close()
		return config.Config{}, err
	}
	if _, err := tmp.Write(encoded.Bytes()); err != nil {
		_ = tmp.Close()
		return config.Config{}, err
	}
	if err := tmp.Close(); err != nil {
		return config.Config{}, err
	}
	cfg, err := config.Load(tmpPath)
	if err != nil {
		return config.Config{}, fmt.Errorf("configuration rejected: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return config.Config{}, err
	}
	return cfg, nil
}

func removeMappingValue(mapping *yaml.Node, key string) {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content = append(mapping.Content[:i], mapping.Content[i+2:]...)
			return
		}
	}
}
