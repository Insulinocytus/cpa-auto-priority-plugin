package priority

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/url"
	"sort"
	"strings"
	"time"
)

// periodSequence orders sorting times from the longest quota period down.
func periodSequence(periods map[int64]time.Time) []time.Time {
	durations := make([]int64, 0, len(periods))
	for duration := range periods {
		durations = append(durations, duration)
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] > durations[j] })
	sequence := make([]time.Time, 0, len(durations))
	for _, duration := range durations {
		sequence = append(sequence, periods[duration])
	}
	return sequence
}

// Download is a filename lookup, unlike the ID-based narrow write. The caller
// rejects ambiguous names; tokens are always resolved from the selected index.
func (s *Synchronizer) quotaMetadata(ctx context.Context, file authFile) (map[string]json.RawMessage, string) {
	if strings.ContainsAny(file.Name, "/\\") || !strings.HasSuffix(strings.ToLower(file.Name), ".json") {
		return nil, "auth_metadata_unavailable"
	}
	var metadata map[string]json.RawMessage
	var err error
	for range 2 {
		err = s.request(ctx, "GET", "credentials/download?name="+url.QueryEscape(file.Name), nil, &metadata)
		if err == nil && metadata != nil {
			return metadata, "ok"
		}
		if err == ErrManagementAuthentication || ctx.Err() != nil {
			break
		}
	}
	if err == ErrManagementAuthentication {
		return nil, err.Error()
	}
	return nil, "auth_metadata_unavailable"
}

func rawString(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return strings.TrimSpace(value)
}

// Official UI classifies 28–31 day windows as one monthly period.
const monthLayer = 31 * 86400

func quotaLayer(seconds int64) int64 {
	if seconds >= 28*86400 && seconds <= monthLayer {
		return monthLayer
	}
	return seconds
}

// relativeSeconds parses a non-negative countdown anchored by the caller.
func relativeSeconds(raw json.RawMessage) (time.Duration, bool) {
	seconds, err := number(raw)
	if err != nil || seconds < 0 || seconds >= float64(math.MaxInt64)/float64(time.Second) {
		return 0, false
	}
	return time.Duration(seconds * float64(time.Second)), true
}

func (s *Synchronizer) quotaGET(ctx context.Context, file authFile, endpoint string, headers map[string]string) ([]byte, string) {
	return s.upstream(ctx, apiCall{AuthIndex: file.Index, Method: "GET", URL: endpoint, Header: headers})
}

func jsonObject(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	raw = bytes.TrimSpace(raw)
	var value map[string]json.RawMessage
	if len(raw) == 0 || raw[0] != '{' || json.Unmarshal(raw, &value) != nil {
		return nil, false
	}
	return value, true
}

func nonNull(raw json.RawMessage) bool { return len(raw) > 0 && !isNull(raw) }
