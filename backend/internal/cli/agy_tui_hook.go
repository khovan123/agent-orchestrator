package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/activitydispatch"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// newAgyTUIHookCommand owns Antigravity's worker-only native stdout contract.
// Keeping this bridge separate from the generic `ao hooks` receiver lets the
// current upstream hook dispatcher keep serving reviewers and other harnesses
// unchanged while Agy workers can return event-specific JSON and relay Stop
// outcomes to their orchestrator.
func newAgyTUIHookCommand(ctx *commandContext) *cobra.Command {
	return &cobra.Command{
		Use:    "agy-tui-hook <event>",
		Short:  "Internal Agy TUI hook bridge",
		Hidden: true,
		Args:   usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			event := strings.TrimSpace(args[0])
			switch event {
			case "pre-invocation", "post-tool-use", "stop":
			default:
				return usageError{err: fmt.Errorf("unsupported Agy TUI hook event %q", event)}
			}
			// Review sessions already use the generic hook receiver and do not
			// participate in worker completion relay.
			if strings.TrimSpace(os.Getenv("AO_REVIEW_SESSION_ID")) != "" {
				return ctx.runHook(cmd.Context(), "agy", event)
			}
			return ctx.runAgyTUIHook(cmd.Context(), event)
		},
	}
}

func (c *commandContext) runAgyTUIHook(ctx context.Context, event string) error {
	sessionID := strings.TrimSpace(os.Getenv("AO_SESSION_ID"))
	if !sessionIDPattern.MatchString(sessionID) {
		// Antigravity still requires syntactically valid JSON even when this
		// command is invoked outside an AO-managed worker.
		return c.emitAgyTUIHookResponse(event, "", nil)
	}

	payload, err := io.ReadAll(c.deps.In)
	if err != nil {
		c.reportHookFailure("agy", event, sessionID, fmt.Errorf("read stdin: %w", err))
	}
	defer func() {
		if emitErr := c.emitAgyTUIHookResponse(event, sessionID, payload); emitErr != nil {
			c.reportHookFailure("agy", event, sessionID, emitErr)
		}
	}()

	state, hasActivity := activitydispatch.Derive("agy", event, payload)
	agentSessionID := hookAgentSessionID(payload)
	if !hasActivity && agentSessionID == "" {
		return nil
	}
	launchID := validLaunchID(os.Getenv("AO_RUNTIME_LAUNCH_ID"))
	if launchID == "" {
		launchID = validLaunchID(hookLaunchID(payload))
	}
	toolName, toolUseID := activityMeta(payload)
	conversation := agyHookConversationFacts(payload)
	req := setActivityAPIRequest{
		Event:                 event,
		ToolName:              toolName,
		ToolUseID:             toolUseID,
		AgentSessionID:        agentSessionID,
		LatestUserPrompt:      conversation.LatestUserPrompt,
		LatestAssistantUpdate: conversation.LatestAssistantUpdate,
		TranscriptPath:        conversation.TranscriptPath,
		LaunchID:              launchID,
	}
	if hasActivity {
		req.State = string(state)
	}
	path := "sessions/" + url.PathEscape(sessionID) + "/activity"
	if err := c.postJSON(ctx, path, req, nil); err != nil {
		c.reportHookFailure("agy", event, sessionID, err)
		return nil
	}
	if event == "stop" {
		if err := c.relayAgyStop(ctx, sessionID, conversation, parseAgyStopOutcome(payload)); err != nil {
			c.reportHookFailure("agy", event, sessionID, fmt.Errorf("relay execution result: %w", err))
		}
	}
	return nil
}

func agyHookConversationFacts(payload []byte) hookConversationSnapshot {
	snapshot := hookConversationFacts(payload)
	if snapshot.LatestAssistantUpdate != "" {
		return snapshot
	}
	var p struct {
		PromptResponse      string `json:"prompt_response"`
		PromptResponseCamel string `json:"promptResponse"`
	}
	_ = json.Unmarshal(payload, &p)
	snapshot.LatestAssistantUpdate = capHookText(firstHookValue(p.PromptResponse, p.PromptResponseCamel), maxHookInteractionLen)
	return snapshot
}

type agyPreInvocationHookOutput struct {
	InjectSteps []agyInjectedStep `json:"injectSteps,omitempty"`
}

type agyInjectedStep struct {
	EphemeralMessage string `json:"ephemeralMessage"`
}

type agyStopHookOutput struct {
	Decision string `json:"decision"`
}

func (c *commandContext) emitAgyTUIHookResponse(event, sessionID string, payload []byte) error {
	switch event {
	case "pre-invocation":
		response := agyPreInvocationHookOutput{}
		if sessionID != "" && agyInvocationNumber(payload) == 0 {
			dataDir := strings.TrimSpace(os.Getenv("AO_DATA_DIR"))
			if dataDir != "" {
				path := filepath.Join(dataDir, "prompts", sessionID, "system.md")
				data, err := os.ReadFile(path) //nolint:gosec // sessionID is bounded by sessionIDPattern.
				if err == nil && strings.TrimSpace(string(data)) != "" {
					response.InjectSteps = []agyInjectedStep{{EphemeralMessage: strings.TrimSpace(string(data))}}
				} else if err != nil && !os.IsNotExist(err) {
					return fmt.Errorf("read system prompt: %w", err)
				}
			}
		}
		return json.NewEncoder(c.deps.Out).Encode(response)
	case "post-tool-use":
		return json.NewEncoder(c.deps.Out).Encode(struct{}{})
	case "stop":
		return json.NewEncoder(c.deps.Out).Encode(agyStopHookOutput{Decision: "allow"})
	default:
		return nil
	}
}

func agyInvocationNumber(payload []byte) int {
	var p struct {
		InvocationNum      *int `json:"invocationNum"`
		InvocationNumSnake *int `json:"invocation_num"`
	}
	_ = json.Unmarshal(payload, &p)
	if p.InvocationNum != nil {
		return *p.InvocationNum
	}
	if p.InvocationNumSnake != nil {
		return *p.InvocationNumSnake
	}
	return -1
}

var _ = domain.HarnessAgy
