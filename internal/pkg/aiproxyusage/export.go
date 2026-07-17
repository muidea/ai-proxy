package usage

import (
	"context"
	"database/sql"
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
	"time"
)

// csvExportHeader 是导出固定列;不含正文与任何密钥。
var csvExportHeader = []string{
	"event_id",
	"round_id",
	"started_at",
	"completed_at",
	"usage_date",
	"api_key_id",
	"provider",
	"model",
	"operation",
	"route",
	"client_endpoint",
	"client_protocol",
	"upstream_protocol",
	"upstream_endpoint",
	"conversion_mode",
	"input_tokens",
	"output_tokens",
	"total_tokens",
	"cached_input_tokens",
	"cache_creation_input_tokens",
	"http_status",
	"outcome",
	"error_code",
	"duration_ms",
	"upstream_duration_ms",
	"stream",
	"estimated",
	"state",
}

// ExportCSV 按筛选条件流式写出 CSV。
func (s *DuckDBStore) ExportCSV(ctx context.Context, filter UsageFilter, w io.Writer) error {
	if s.closed.Load() {
		return ErrStoreClosed
	}
	if err := ValidateUsageFilter(&filter); err != nil {
		return err
	}
	from, to := filter.From.UTC(), filter.To.UTC()
	if filter.AllTime {
		if from.IsZero() {
			from = time.Unix(0, 0).UTC()
		}
		if to.IsZero() {
			to = time.Now().UTC().Add(24 * time.Hour)
		}
	}

	where, args := buildFilterWhere(filter, from, to)
	q := `
SELECT
    event_id, coalesce(round_id, 0), started_at, completed_at,
    cast(usage_date AS VARCHAR), api_key_id,
    coalesce(provider, ''), coalesce(model, ''),
    coalesce(operation, ''), coalesce(route, ''),
    coalesce(client_endpoint, ''), coalesce(client_protocol, ''),
    coalesce(upstream_protocol, ''), coalesce(upstream_endpoint, ''),
    coalesce(conversion_mode, ''),
    input_tokens, output_tokens, total_tokens,
    cached_input_tokens, cache_creation_input_tokens,
    http_status, coalesce(outcome, ''), coalesce(error_code, ''),
    duration_ms, upstream_duration_ms,
    stream, estimated, state
FROM usage_events
WHERE ` + where + `
ORDER BY started_at ASC, event_id ASC`

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return ErrStoreUnavailable
	}
	defer rows.Close()

	cw := csv.NewWriter(w)
	if err := cw.Write(csvExportHeader); err != nil {
		return fmt.Errorf("write csv header: %w", err)
	}

	for rows.Next() {
		var (
			eventID, apiKeyID, provider, model, operation, route string
			clientEndpoint, clientProtocol                       string
			upstreamProtocol, upstreamEndpoint, conversionMode   string
			outcome, errorCode, state, usageDate                 string
			roundID                                              int64
			inputTok, outputTok, totalTok                        int64
			cachedIn, cacheCreate                                int64
			stream, estimated                                    bool
			startedAt                                            time.Time
			completedAt                                          sql.NullTime
			httpStatus, durationMS, upstreamMS                   sql.NullInt64
		)
		if err := rows.Scan(
			&eventID, &roundID, &startedAt, &completedAt,
			&usageDate, &apiKeyID,
			&provider, &model,
			&operation, &route,
			&clientEndpoint, &clientProtocol,
			&upstreamProtocol, &upstreamEndpoint,
			&conversionMode,
			&inputTok, &outputTok, &totalTok,
			&cachedIn, &cacheCreate,
			&httpStatus, &outcome, &errorCode,
			&durationMS, &upstreamMS,
			&stream, &estimated, &state,
		); err != nil {
			return ErrStoreUnavailable
		}
		if len(usageDate) > 10 {
			usageDate = usageDate[:10]
		}
		completed := ""
		if completedAt.Valid {
			completed = completedAt.Time.UTC().Format(time.RFC3339Nano)
		}
		httpS := ""
		if httpStatus.Valid {
			httpS = strconv.FormatInt(httpStatus.Int64, 10)
		}
		durS := ""
		if durationMS.Valid {
			durS = strconv.FormatInt(durationMS.Int64, 10)
		}
		upDurS := ""
		if upstreamMS.Valid {
			upDurS = strconv.FormatInt(upstreamMS.Int64, 10)
		}
		row := []string{
			eventID,
			strconv.FormatInt(roundID, 10),
			startedAt.UTC().Format(time.RFC3339Nano),
			completed,
			usageDate,
			apiKeyID,
			provider,
			model,
			operation,
			route,
			clientEndpoint,
			clientProtocol,
			upstreamProtocol,
			upstreamEndpoint,
			conversionMode,
			strconv.FormatInt(inputTok, 10),
			strconv.FormatInt(outputTok, 10),
			strconv.FormatInt(totalTok, 10),
			strconv.FormatInt(cachedIn, 10),
			strconv.FormatInt(cacheCreate, 10),
			httpS,
			outcome,
			errorCode,
			durS,
			upDurS,
			strconv.FormatBool(stream),
			strconv.FormatBool(estimated),
			state,
		}
		if err := cw.Write(row); err != nil {
			return fmt.Errorf("write csv row: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return ErrStoreUnavailable
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return fmt.Errorf("flush csv: %w", err)
	}
	return nil
}
