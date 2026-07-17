// Package usageimport 是旧 usage.csv 的一次性导入进程服务。
// 它不在主服务启动时自动执行，默认 api_key_id=default。
package usageimport

import (
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"ai-proxy/internal/pkg/aiproxyconfig"
	"ai-proxy/internal/pkg/aiproxyusage"
)

var apiKeyIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// Main 保持既有 CLI 行为；cmd/ai-proxy-usage-import 只负责调用该进程服务。
func Main() {
	source := flag.String("source", "usage.csv", "source usage.csv path")
	database := flag.String("database", "usage.duckdb", "target DuckDB path")
	apiKeyID := flag.String("api-key-id", "default", "api_key_id for imported rows (not a secret)")
	flag.Parse()

	id := strings.ToLower(strings.TrimSpace(*apiKeyID))
	if id == "" {
		fail("api-key-id is required")
	}
	if id != "default" && !apiKeyIDPattern.MatchString(id) {
		fail("invalid api-key-id (must match [a-z0-9][a-z0-9._-]{0,63})")
	}

	f, err := os.Open(*source)
	if err != nil {
		fail("open source: %v", err)
	}
	defer f.Close()

	store, err := usage.OpenDuckDB(config.UsageStoreConfig{
		Path:              *database,
		MemoryLimit:       "256MB",
		Threads:           2,
		QueryCacheSeconds: 0,
	})
	if err != nil {
		fail("open database: %v", err)
	}
	defer store.Close()

	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	header, err := r.Read()
	if err != nil {
		fail("read header: %v", err)
	}
	col := map[string]int{}
	for i, h := range header {
		col[strings.TrimSpace(h)] = i
	}
	for _, need := range []string{"time", "provider", "model", "input_tokens", "output_tokens"} {
		if _, ok := col[need]; !ok {
			fail("missing required column %q", need)
		}
	}

	srcID := sourceIdentity(*source)
	var readN, writeN, dupN, failN int
	ctx := context.Background()
	lineNo := 1 // header
	for {
		lineNo++
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			failN++
			fmt.Fprintf(os.Stderr, "line %d: parse error: %v\n", lineNo, err)
			continue
		}
		readN++
		eventID := deterministicEventID(srcID, lineNo, rec)
		startedAt, err := time.Parse(time.RFC3339, get(rec, col, "time"))
		if err != nil {
			// try common variants
			startedAt, err = time.Parse(time.RFC3339Nano, get(rec, col, "time"))
			if err != nil {
				failN++
				fmt.Fprintf(os.Stderr, "line %d: bad time\n", lineNo)
				continue
			}
		}
		inTok, err := parseNonNegativeInt64("input_tokens", get(rec, col, "input_tokens"))
		if err != nil {
			failN++
			fmt.Fprintf(os.Stderr, "line %d: %v\n", lineNo, err)
			continue
		}
		outTok, err := parseNonNegativeInt64("output_tokens", get(rec, col, "output_tokens"))
		if err != nil {
			failN++
			fmt.Fprintf(os.Stderr, "line %d: %v\n", lineNo, err)
			continue
		}
		cachedIn, err := parseNonNegativeInt64("cached_input_tokens", get(rec, col, "cached_input_tokens"))
		if err != nil {
			failN++
			fmt.Fprintf(os.Stderr, "line %d: %v\n", lineNo, err)
			continue
		}
		cacheCreation, err := parseNonNegativeInt64("cache_creation_input_tokens", get(rec, col, "cache_creation_input_tokens"))
		if err != nil {
			failN++
			fmt.Fprintf(os.Stderr, "line %d: %v\n", lineNo, err)
			continue
		}
		durationMS, err := parseNonNegativeInt64("duration_ms", get(rec, col, "duration_ms"))
		if err != nil {
			failN++
			fmt.Fprintf(os.Stderr, "line %d: %v\n", lineNo, err)
			continue
		}
		status64, err := parseHTTPStatus(get(rec, col, "http_status"))
		if err != nil {
			failN++
			fmt.Fprintf(os.Stderr, "line %d: %v\n", lineNo, err)
			continue
		}
		stream, err := parseOptionalBool("stream", get(rec, col, "stream"))
		if err != nil {
			failN++
			fmt.Fprintf(os.Stderr, "line %d: %v\n", lineNo, err)
			continue
		}
		estimated, err := parseOptionalBool("estimated", get(rec, col, "estimated"))
		if err != nil {
			failN++
			fmt.Fprintf(os.Stderr, "line %d: %v\n", lineNo, err)
			continue
		}
		if err := store.Start(ctx, usage.StartRecord{
			EventID:        eventID,
			StartedAt:      startedAt.UTC(),
			APIKeyID:       id,
			Provider:       get(rec, col, "provider"),
			Model:          get(rec, col, "model"),
			Operation:      get(rec, col, "operation"),
			ClientEndpoint: get(rec, col, "client_endpoint"),
			ClientProtocol: "",
			Route:          get(rec, col, "operation"),
		}); err != nil {
			if err == usage.ErrDuplicateEvent {
				dupN++
				continue
			}
			failN++
			fmt.Fprintf(os.Stderr, "line %d: start: %v\n", lineNo, err)
			continue
		}
		status := int(status64)
		outcome := get(rec, col, "outcome")
		if outcome == "" {
			if status >= 400 {
				outcome = "error"
			} else {
				outcome = "success"
			}
		}
		if err := store.Complete(ctx, usage.CompleteRecord{
			EventID:                  eventID,
			CompletedAt:              startedAt.UTC(),
			Provider:                 get(rec, col, "provider"),
			Model:                    get(rec, col, "model"),
			UpstreamProtocol:         get(rec, col, "upstream_protocol"),
			UpstreamEndpoint:         get(rec, col, "upstream_endpoint"),
			ConversionMode:           get(rec, col, "conversion_mode"),
			InputTokens:              inTok,
			OutputTokens:             outTok,
			CachedInputTokens:        cachedIn,
			CacheCreationInputTokens: cacheCreation,
			HTTPStatus:               status,
			Outcome:                  outcome,
			Duration:                 time.Duration(durationMS) * time.Millisecond,
			Stream:                   stream,
			Estimated:                estimated,
		}); err != nil {
			failN++
			fmt.Fprintf(os.Stderr, "line %d: complete: %v\n", lineNo, err)
			continue
		}
		writeN++
	}

	fmt.Printf("import done source=%s database=%s api_key_id=%s read=%d write=%d duplicate=%d failed=%d\n",
		*source, *database, id, readN, writeN, dupN, failN)
	if failN > 0 {
		os.Exit(2)
	}
}

func get(rec []string, col map[string]int, name string) string {
	i, ok := col[name]
	if !ok || i >= len(rec) {
		return ""
	}
	return strings.TrimSpace(rec[i])
}

func parseNonNegativeInt64(name, s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid %s", name)
	}
	return n, nil
}

func parseHTTPStatus(s string) (int64, error) {
	if s == "" {
		return http.StatusOK, nil
	}
	n, err := parseNonNegativeInt64("http_status", s)
	if err != nil || n < 100 || n > 599 {
		return 0, fmt.Errorf("invalid http_status")
	}
	return n, nil
}

func parseOptionalBool(name, s string) (bool, error) {
	if s == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(s)
	if err != nil {
		return false, fmt.Errorf("invalid %s", name)
	}
	return b, nil
}

func sourceIdentity(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	sum := sha256.Sum256([]byte(abs))
	return hex.EncodeToString(sum[:8])
}

func deterministicEventID(srcID string, lineNo int, rec []string) string {
	h := sha256.New()
	_, _ = io.WriteString(h, srcID)
	_, _ = io.WriteString(h, "|")
	_, _ = io.WriteString(h, strconv.Itoa(lineNo))
	_, _ = io.WriteString(h, "|")
	_, _ = io.WriteString(h, strings.Join(rec, "\x1f"))
	return "csvimp-" + hex.EncodeToString(h.Sum(nil)[:16])
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
