package registry

import (
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// What the daemon ships, stated once.
//
// Registration is the whole capability gate — a harness with no driver here cannot
// run chat mode — so the shipped set is a release decision, not an implementation
// detail. Codex uses its native app-server; Claude, Cursor, OpenCode, Droid,
// Kimi, Kimchi, Pi, and OMP use ACP; Agy uses its native stream-json interface.
func TestShippedChatDrivers(t *testing.T) {
	r := Build(nil)

	for _, harness := range []domain.AgentHarness{
		domain.HarnessCodex,
		domain.HarnessClaudeCode,
		domain.HarnessOpenCode,
		domain.HarnessDroid,
		domain.HarnessKimi,
		domain.HarnessKimchi,
		domain.HarnessPi,
		domain.HarnessCursor,
		domain.HarnessOMP,
		domain.HarnessAgy,
	} {
		if !r.SupportsChat(harness) {
			t.Errorf("%s has no chat driver", harness)
		}
		if _, err := r.Driver(harness); err != nil {
			t.Errorf("resolving the %s driver: %v", harness, err)
		}
	}

	for _, harness := range []domain.AgentHarness{
		domain.HarnessAider,
		"definitely-not-an-agent",
	} {
		if _, err := r.Driver(harness); err == nil {
			t.Errorf("%s resolved a chat driver", harness)
		} else if !errors.Is(err, ports.ErrChatUnsupported) {
			t.Errorf("%s refused with %v, want ErrChatUnsupported so callers can branch", harness, err)
		}
	}
}
