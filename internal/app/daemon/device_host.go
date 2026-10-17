package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jinyitao123/weave/internal/kernel/runtimeprotocol"
)

// A device runtime is the person's own machine (weave-workbench decision
// 009). It claims control sessions, takes their operations one at a time and
// answers each; cua-driver sees and drives the screen, the node runs programs
// and touches files inside the roots its owner allows.

var deviceLevels = []string{"observe", "act", "exec"}

var deviceOperationLevel = map[string]string{
	"screenshot": "observe", "ui_tree": "observe", "windows": "observe",
	"act":  "act",
	"exec": "exec", "file_read": "exec", "file_write": "exec",
}

// deviceConfig is the owner's local limit, in ~/.weave/device.json. Weave can
// narrow it but never widen it. Missing, it allows every level under home.
type deviceConfig struct {
	Levels    []string `json:"levels"`
	FileRoots []string `json:"file_roots"`
}

type deviceHost struct {
	driver string
	config deviceConfig
}

func newDeviceHost(driver string) *deviceHost {
	home, _ := os.UserHomeDir()
	config := deviceConfig{Levels: append([]string(nil), deviceLevels...), FileRoots: []string{home}}
	if raw, err := os.ReadFile(filepath.Join(home, ".weave", "device.json")); err == nil {
		var local deviceConfig
		if json.Unmarshal(raw, &local) == nil {
			if local.Levels != nil {
				config.Levels = slices.DeleteFunc(local.Levels, func(level string) bool { return !slices.Contains(deviceLevels, level) })
			}
			if local.FileRoots != nil {
				config.FileRoots = local.FileRoots
			}
		}
	}
	return &deviceHost{driver: driver, config: config}
}

func (h *deviceHost) allows(kind string) bool {
	return slices.Contains(h.config.Levels, deviceOperationLevel[kind])
}

// deviceFacts is reported in hello; nil for a team runtime.
func (d *service) deviceFacts(ctx context.Context) *runtimeprotocol.DeviceFacts {
	if d.device == nil {
		return nil
	}
	return d.device.facts(ctx)
}

func (h *deviceHost) facts(ctx context.Context) *runtimeprotocol.DeviceFacts {
	facts := &runtimeprotocol.DeviceFacts{Platform: runtime.GOOS, OSVersion: osVersion(ctx), DriverPath: h.driver,
		Levels: append([]string(nil), h.config.Levels...), FileRoots: append([]string(nil), h.config.FileRoots...)}
	if h.driver == "" {
		facts.UnavailableReason = "未配置 cua-driver"
		return facts
	}
	if info, err := os.Stat(h.driver); err != nil || info.IsDir() {
		facts.UnavailableReason = "找不到 cua-driver，请在这台机器上重新运行注册命令"
		return facts
	}
	versionCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(versionCtx, h.driver, "--version").Output(); err == nil {
		facts.DriverVersion = strings.TrimSpace(string(out))
	}
	facts.Available = true
	return facts
}

func osVersion(ctx context.Context) string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	versionCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(versionCtx, "sw_vers", "-productVersion").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// runDevice heartbeats and serves one control session at a time.
func (d *service) runDevice(ctx context.Context) error {
	done := make(chan struct{})
	go func() { defer close(done); d.heartbeatLoop(ctx) }()
	backoff := newBackoff(d.minBackoff, d.maxBackoff)
	for ctx.Err() == nil {
		session, err := d.client.claimDevice(ctx, d.claimWaitSeconds)
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("device session claim failed; retrying", "error", err)
			}
			waitFor(ctx, backoff.next())
			continue
		}
		backoff.reset()
		if session != nil {
			d.active.Add(1)
			d.serveDeviceSession(ctx, session)
			d.active.Add(-1)
		}
	}
	<-done
	return nil
}

func (d *service) serveDeviceSession(ctx context.Context, session *runtimeprotocol.DeviceSessionClaim) {
	ctx = withTaskProof(ctx, session.Subject, session.ClaimEpoch)
	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	claim := &runtimeprotocol.ExecutionClaim{TaskID: session.TaskID, ClaimEpoch: session.ClaimEpoch, Subject: session.Subject,
		LeaseIssuedAt: time.Now(), LeaseExpiresAt: session.LeaseExpiresAt}
	leaseLost := &atomic.Bool{}
	renewDone := make(chan struct{})
	go func() { defer close(renewDone); d.renewLoop(sessionCtx, cancel, claim, leaseLost) }()
	slog.Info("device session started", "session", session.TaskID, "reason", session.Reason, "opened_by", session.OpenedBy)

	var driver *cuaClient
	defer func() { driver.close() }()
	reason := "ended_on_device"
	for sessionCtx.Err() == nil {
		next, err := d.client.deviceNext(sessionCtx, session.TaskID, session.ClaimEpoch, 20)
		if err != nil {
			if errors.Is(err, errLeaseLost) || sessionCtx.Err() != nil {
				reason = "device_disconnected"
				break
			}
			slog.Warn("device operation poll failed; retrying", "session", session.TaskID, "error", err)
			waitFor(sessionCtx, time.Second)
			continue
		}
		if next.Ended {
			reason = "closed"
			break
		}
		if next.Operation == nil {
			continue
		}
		op := next.Operation
		var result runtimeprotocol.DeviceOperationResult
		if !d.device.allows(op.Kind) {
			result = deviceFailed("这台设备的本机配置不允许这类操作")
		} else {
			if driver == nil && deviceOperationLevel[op.Kind] != "exec" {
				if driver, err = startCuaClient(sessionCtx, d.device.driver); err != nil {
					driver = nil
					result = deviceFailed("cua-driver 无法启动：" + err.Error())
				}
			}
			if result.Status == "" {
				result = d.device.perform(sessionCtx, driver, op)
			}
		}
		result.Versioned, result.ClaimEpoch = runtimeprotocol.NewVersioned(), session.ClaimEpoch
		if err := d.reportUntilAccepted(sessionCtx, func(reportCtx context.Context) error {
			return d.client.deviceResult(reportCtx, session.TaskID, op.OpID, result)
		}); err != nil {
			reason = "device_disconnected"
			break
		}
	}
	cancel()
	<-renewDone
	if !leaseLost.Load() {
		endCtx, endCancel := context.WithTimeout(withTaskProof(context.Background(), session.Subject, session.ClaimEpoch), 15*time.Second)
		if err := d.client.deviceEnd(endCtx, session.TaskID, session.ClaimEpoch, reason); err != nil {
			slog.Warn("device session end not recorded", "session", session.TaskID, "error", err)
		}
		endCancel()
	}
	slog.Info("device session ended", "session", session.TaskID, "reason", reason)
}

func deviceFailed(message string) runtimeprotocol.DeviceOperationResult {
	return runtimeprotocol.DeviceOperationResult{Status: "failed", Error: message}
}

func deviceSucceeded(result any, audit map[string]any) runtimeprotocol.DeviceOperationResult {
	raw, err := json.Marshal(result)
	if err != nil {
		return deviceFailed("结果无法编码")
	}
	answer := runtimeprotocol.DeviceOperationResult{Status: "succeeded", Result: raw}
	if len(audit) > 0 {
		answer.Audit, _ = json.Marshal(audit)
	}
	return answer
}

// --- Device requests to the server. ---

func (c *runtimeClient) claimDevice(ctx context.Context, waitSeconds int) (*runtimeprotocol.DeviceSessionClaim, error) {
	response, err := c.do(ctx, http.MethodPost, "/v1/runtime/claim", runtimeprotocol.ClaimRequest{Versioned: runtimeprotocol.NewVersioned(), WaitSeconds: waitSeconds})
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	if err := expectStatus(response, http.StatusOK); err != nil {
		return nil, err
	}
	var envelope runtimeprotocol.ClaimResponse
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		return nil, fmt.Errorf("daemon: decode device claim: %w", err)
	}
	if err := envelope.Versioned.Validate(); err != nil {
		return nil, err
	}
	if envelope.DeviceSession == nil || envelope.DeviceSession.TaskID == "" || envelope.DeviceSession.ClaimEpoch < 1 {
		return nil, errors.New("daemon: device claim omitted its session")
	}
	return envelope.DeviceSession, nil
}

func (c *runtimeClient) deviceNext(ctx context.Context, taskID string, epoch int64, waitSeconds int) (*runtimeprotocol.DeviceNextResponse, error) {
	path := "/v1/runtime/tasks/" + url.PathEscape(taskID) + "/device/next"
	response, err := c.do(ctx, http.MethodPost, path, runtimeprotocol.DeviceNextRequest{Versioned: runtimeprotocol.NewVersioned(), ClaimEpoch: epoch, WaitSeconds: waitSeconds})
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict:
		return nil, errLeaseLost
	}
	if err := expectStatus(response, http.StatusOK); err != nil {
		return nil, err
	}
	var next runtimeprotocol.DeviceNextResponse
	if err := json.NewDecoder(response.Body).Decode(&next); err != nil {
		return nil, fmt.Errorf("daemon: decode device operation: %w", err)
	}
	return &next, next.Versioned.Validate()
}

func (c *runtimeClient) deviceResult(ctx context.Context, taskID, opID string, result runtimeprotocol.DeviceOperationResult) error {
	return c.postTaskNoContent(ctx, taskID, "device/ops/"+url.PathEscape(opID), result)
}

func (c *runtimeClient) deviceEnd(ctx context.Context, taskID string, epoch int64, reason string) error {
	return c.postTaskNoContent(ctx, taskID, "device/end", runtimeprotocol.DeviceSessionEnd{Versioned: runtimeprotocol.NewVersioned(), ClaimEpoch: epoch, Reason: reason})
}
