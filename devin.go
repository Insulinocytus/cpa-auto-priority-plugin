package priority

import (
	"context"
	"encoding/json"
	"time"
)

// The official UI's Connect-RPC call; the host substitutes the selected auth's
// token for $TOKEN$ inside the body, the same as for header placeholders.
const devinStatusBody = `{"metadata":{"ideName":"chisel","ideVersion":"3000.10.21","apiKey":"$TOKEN$","locale":"en","os":"darwin","extensionVersion":"3000.10.21","clientName":"chisel"}}`

func (s *Synchronizer) devin(ctx context.Context, file authFile) ([]time.Time, string) {
	payload, status := s.upstream(ctx, file.Index, "POST", "https://server.codeium.com/exa.seat_management_pb.SeatManagementService/GetUserStatus", map[string]string{"Content-Type": "application/json", "Connect-Protocol-Version": "1"}, devinStatusBody)
	if status != "ok" {
		return nil, status
	}
	// Only the natural quota resets are decoded; planStart/planEnd describe the
	// subscription, and the body's apiKey/email stay unread.
	var response struct {
		UserStatus *struct {
			PlanStatus *struct {
				Daily  json.RawMessage `json:"dailyQuotaResetAtUnix"`
				Weekly json.RawMessage `json:"weeklyQuotaResetAtUnix"`
			} `json:"planStatus"`
		} `json:"userStatus"`
	}
	if json.Unmarshal(payload, &response) != nil {
		return nil, "quota_response_malformed"
	}
	if response.UserStatus == nil || response.UserStatus.PlanStatus == nil {
		return nil, "no_reset_time"
	}
	periods := map[int64]time.Time{}
	for duration, raw := range map[int64]json.RawMessage{7 * 86400: response.UserStatus.PlanStatus.Weekly, 86400: response.UserStatus.PlanStatus.Daily} {
		reset, known, err := optionalUnixSeconds(raw)
		if err != nil {
			return nil, "quota_response_malformed"
		}
		if known {
			periods[duration] = reset
		}
	}
	return ordered(periods)
}
