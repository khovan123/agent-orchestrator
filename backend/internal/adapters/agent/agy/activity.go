package agy

import (
	"encoding/json"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// DeriveActivityState maps an Agy hook event onto an AO activity state. The
// bool is false when the event carries no activity signal.
//
// event is the AO hook sub-command name installed in the managed AGY hook:
// "pre-invocation", "post-tool-use", or "stop".
func DeriveActivityState(event string, payload []byte) (domain.ActivityState, bool) {
	switch event {
	case "pre-invocation", "post-tool-use":
		return domain.ActivityActive, true
	case "stop":
		if fullyIdle, known := agyStopFullyIdle(payload); known && !fullyIdle {
			return domain.ActivityActive, true
		}
		return domain.ActivityIdle, true
	default:
		return "", false
	}
}

func agyStopFullyIdle(payload []byte) (bool, bool) {
	var p struct {
		FullyIdle      *bool `json:"fullyIdle"`
		FullyIdleSnake *bool `json:"fully_idle"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return false, false
	}
	if p.FullyIdle != nil {
		return *p.FullyIdle, true
	}
	if p.FullyIdleSnake != nil {
		return *p.FullyIdleSnake, true
	}
	return false, false
}
