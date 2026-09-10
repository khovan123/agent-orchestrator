// Package httpd builds and runs the daemon's HTTP surface: middleware, health
// probes, daemon control, REST APIs, and terminal WebSocket routing.
package httpd

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/daemonmeta"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	agentswitchobs "github.com/aoagents/agent-orchestrator/backend/internal/observe/agentswitch"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/telemetrymeta"
	"github.com/aoagents/agent-orchestrator/backend/internal/terminal"
)

type ControlDeps struct {
	RequestShutdown   func()
	AgentSwitchPolicy AgentSwitchPolicyControl
}

type AgentSwitchPolicyControl interface {
	PrepareDisable(context.Context) (ports.AgentSwitchFailurePolicyAcknowledgement, error)
	ApplyPolicy(context.Context, string, bool) (ports.AgentSwitchFailurePolicyAcknowledgement, error)
}

func NewRouterWithControl(cfg config.Config, log *slog.Logger, termMgr *terminal.Manager, deps APIDeps, control ControlDeps) chi.Router {
	log = loggerOrDefault(log)
	deps = normalizeAPIDeps(deps, log)
	r := chi.NewRouter()
	api := NewAPI(cfg, deps)

	r.Use(middleware.RequestID)
	r.Use(requestLogger(log, deps.Telemetry))
	r.Use(recoverTelemetry(log, deps.Telemetry))
	r.Use(codexAccountOriginMiddleware(cfg.AllowedOrigins))
	r.Use(corsMiddleware(cfg.AllowedOrigins))
	r.Use(previewOriginMiddleware(api.sessions))

	r.NotFound(notFoundJSON)
	r.MethodNotAllowed(methodNotAllowedJSON)

	mountHealth(r, cfg)
	mountTerminalMux(r, termMgr, log)
	mountControl(r, control)
	mountAgentSwitchPolicyControl(r, control.AgentSwitchPolicy)
	mountTelemetry(r, cfg, deps.Telemetry)
	mountMobile(r, deps.Mobile)
	mountMobileDevices(r, &controllers.MobileDevicesController{Registry: deps.DeviceRoster, Presence: deps.DeviceLive})
	mountAgyChatHooks(r, api.conversations)
	api.Register(r)

	return r
}

type applyAgentSwitchPolicyRequest struct {
	ConsentGeneration string `json:"consentGeneration"`
	EventsEnabled     bool   `json:"eventsEnabled"`
}

func mountAgentSwitchPolicyControl(r chi.Router, policy AgentSwitchPolicyControl) {
	if policy == nil {
		return
	}
	writeAck := func(w http.ResponseWriter, acknowledgement ports.AgentSwitchFailurePolicyAcknowledgement) {
		envelope.WriteJSON(w, http.StatusOK, map[string]any{
			"status": "applied", "consentGeneration": acknowledgement.Authorization.ConsentGeneration,
			"eventsEnabled": acknowledgement.Authorization.Enabled, "gateDrained": acknowledgement.GateDrained,
			"purgeConfirmed": acknowledgement.PurgeConfirmed,
		})
	}
	r.Post("/internal/agent-switch-observability/prepare-disable", func(w http.ResponseWriter, req *http.Request) {
		if !localControlRequest(req) {
			envelope.WriteJSON(w, http.StatusForbidden, map[string]any{"status": "forbidden"})
			return
		}
		acknowledgement, err := policy.PrepareDisable(req.Context())
		if err != nil {
			envelope.WriteJSON(w, http.StatusInternalServerError, map[string]any{"status": "failed"})
			return
		}
		writeAck(w, acknowledgement)
	})
	r.Post("/internal/agent-switch-observability/apply-policy", func(w http.ResponseWriter, req *http.Request) {
		if !localControlRequest(req) {
			envelope.WriteJSON(w, http.StatusForbidden, map[string]any{"status": "forbidden"})
			return
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, req.Body, 4096))
		decoder.DisallowUnknownFields()
		var body applyAgentSwitchPolicyRequest
		if err := decoder.Decode(&body); err != nil || body.ConsentGeneration == "" {
			envelope.WriteJSON(w, http.StatusBadRequest, map[string]any{"status": "invalid_request"})
			return
		}
		acknowledgement, err := policy.ApplyPolicy(req.Context(), body.ConsentGeneration, body.EventsEnabled)
		if err != nil {
			status := http.StatusInternalServerError
			result := "failed"
			if errors.Is(err, agentswitchobs.ErrPolicyHintMismatch) {
				status = http.StatusConflict
				result = "authority_mismatch"
			} else if errors.Is(err, agentswitchobs.ErrPolicyCleanupPending) {
				status = http.StatusConflict
				result = "cleanup_pending"
			}
			envelope.WriteJSON(w, status, map[string]any{"status": result})
			return
		}
		writeAck(w, acknowledgement)
	})
}

func previewOriginMiddleware(sessions *controllers.SessionsController) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if sessions != nil && sessions.PreviewOrigin(w, r) {
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func mountHealth(r chi.Router, cfg config.Config) {
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		envelope.WriteJSON(w, http.StatusOK, daemonProbePayload("ok", cfg))
	})
	r.Get("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		envelope.WriteJSON(w, http.StatusOK, daemonProbePayload("ready", cfg))
	})
}

func mountControl(r chi.Router, deps ControlDeps) {
	if deps.RequestShutdown == nil {
		return
	}
	r.Post("/shutdown", func(w http.ResponseWriter, req *http.Request) {
		if !localControlRequest(req) {
			envelope.WriteJSON(w, http.StatusForbidden, map[string]any{
				"status": "forbidden", "service": daemonmeta.ServiceName,
			})
			return
		}
		envelope.WriteJSON(w, http.StatusAccepted, map[string]any{
			"status": "shutting_down", "service": daemonmeta.ServiceName, "pid": os.Getpid(),
		})
		deps.RequestShutdown()
	})
}

func mountMobile(r chi.Router, c *controllers.MobileController) {
	if c == nil { return }
	r.Get("/api/v1/mobile/status", c.Status)
	r.Post("/api/v1/mobile/enable", c.Enable)
	r.Post("/api/v1/mobile/remote-access", c.StartRemoteAccess)
	r.Post("/api/v1/mobile/disable", c.Disable)
	r.Post("/api/v1/mobile/regenerate", c.Regenerate)
	r.Post("/api/v1/mobile/secure-pairing", c.SecurePairing)
}

func mountMobileDevices(r chi.Router, c *controllers.MobileDevicesController) {
	if c == nil { return }
	r.Get("/api/v1/mobile/devices", c.List)
	r.Patch("/api/v1/mobile/devices/{installId}", c.Mute)
	r.Delete("/api/v1/mobile/devices/{installId}", c.Remove)
}

type cliInvokedRequest struct {
	Command     string `json:"command"`
	CommandPath string `json:"commandPath"`
	ActorType   string `json:"actorType"`
}

type cliUsageErrorRequest struct {
	Command     string `json:"command"`
	CommandPath string `json:"commandPath"`
	Error       string `json:"error"`
}

func mountTelemetry(r chi.Router, cfg config.Config, sink ports.EventSink) {
	if sink == nil { return }
	cliTelemetry := newCLITelemetryReservoir(cfg.DataDir)
	r.Post("/internal/telemetry/cli-invoked", func(w http.ResponseWriter, req *http.Request) {
		if !localControlRequest(req) {
			envelope.WriteJSON(w, http.StatusForbidden, map[string]any{"status": "forbidden", "service": daemonmeta.ServiceName})
			return
		}
		var body cliInvokedRequest
		dec := json.NewDecoder(req.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil {
			envelope.WriteAPIError(w, req, http.StatusBadRequest, "bad_request", "INVALID_JSON", "request body must be valid JSON", nil)
			return
		}
		if body.CommandPath == "" {
			envelope.WriteAPIError(w, req, http.StatusBadRequest, "bad_request", "COMMAND_PATH_REQUIRED", "commandPath is required", nil)
			return
		}
		commandPath := telemetrymeta.NormalizeCommandPath(body.CommandPath)
		actorType := telemetrymeta.CLIActorType(body.ActorType, commandPath)
		if actorType == "system" || telemetrymeta.IsRoutineInternalCLICommand(commandPath) {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if now := time.Now(); cliTelemetry.reserveInvoked(now, actorType, commandPath) {
			sink.Emit(req.Context(), ports.TelemetryEvent{
				Name: "ao.cli.invoked", Source: "cli", OccurredAt: now.UTC(), Level: ports.TelemetryLevelInfo,
				RequestID: middleware.GetReqID(req.Context()), Payload: map[string]any{
					"command": body.Command, "command_path": commandPath, "actor_type": actorType,
				},
			})
		}
		if actorType == "user" {
			if now := time.Now(); cliTelemetry.reserveActive(now) {
				sink.Emit(req.Context(), ports.TelemetryEvent{
					Name: "ao.app.active", Source: "cli", OccurredAt: now.UTC(), Level: ports.TelemetryLevelInfo,
					RequestID: middleware.GetReqID(req.Context()), Payload: map[string]any{
						"channel": "cli", "command": body.Command, "command_path": commandPath, "actor_type": actorType,
					},
				})
			}
		}
		w.WriteHeader(http.StatusAccepted)
	})
	r.Post("/internal/telemetry/cli-usage-error", func(w http.ResponseWriter, req *http.Request) {
		if !localControlRequest(req) {
			envelope.WriteJSON(w, http.StatusForbidden, map[string]any{"status": "forbidden", "service": daemonmeta.ServiceName})
			return
		}
		var body cliUsageErrorRequest
		dec := json.NewDecoder(req.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil {
			envelope.WriteAPIError(w, req, http.StatusBadRequest, "bad_request", "INVALID_JSON", "request body must be valid JSON", nil)
			return
		}
		if body.CommandPath == "" {
			envelope.WriteAPIError(w, req, http.StatusBadRequest, "bad_request", "COMMAND_PATH_REQUIRED", "commandPath is required", nil)
			return
		}
		sink.Emit(req.Context(), ports.TelemetryEvent{
			Name: "ao.cli.usage_errors", Source: "cli", OccurredAt: time.Now().UTC(), Level: ports.TelemetryLevelWarn,
			RequestID: middleware.GetReqID(req.Context()), Payload: map[string]any{
				"component": "cli", "operation": "command_parse", "command": body.Command,
				"command_path": body.CommandPath, "error_kind": "usage",
				"fingerprint": telemetrymeta.Fingerprint("cli", "command_parse", body.CommandPath, "usage"),
			},
		})
		w.WriteHeader(http.StatusAccepted)
	})
}

func localControlRequest(r *http.Request) bool {
	if r.Header.Get("Origin") != "" { return false }
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil { host = h }
	switch host {
	case "127.0.0.1", "::1", "localhost":
		return true
	}
	if ip := net.ParseIP(host); ip != nil { return ip.IsLoopback() }
	return false
}

func daemonProbePayload(status string, cfg config.Config) map[string]any {
	payload := map[string]any{"status": status, "service": daemonmeta.ServiceName, "pid": os.Getpid()}
	if exe, err := os.Executable(); err == nil && exe != "" { payload["executablePath"] = exe }
	if cwd, err := os.Getwd(); err == nil && cwd != "" { payload["workingDirectory"] = cwd }
	if cfg.StartupWorkingDirectory != "" { payload["startupWorkingDirectory"] = cfg.StartupWorkingDirectory }
	if appImage := os.Getenv("AO_APPIMAGE"); appImage != "" { payload["appImagePath"] = appImage }
	return payload
}
