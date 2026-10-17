package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jinyitao123/weave/internal/kernel/runtimes"
	"github.com/labstack/echo/v4"
)

// Devices are the person's own machines, enrolled with one command each and
// controlled only from that person's account session (weave-workbench
// decision 009; contract 设备控制契约).

// personalAccountSession reports a caller signed in with their own account:
// not an API key, and not a session opened with the deployment login key or a
// console API-key sign-in.
func personalAccountSession(c echo.Context) bool {
	if c.Get(authSourceContextKey) != authSourceJWT || getUserID(c) == "" {
		return false
	}
	switch source, _ := c.Get(identitySourceContextKey).(string); source {
	case "", loginKeyIdentitySource, adminAPIKeyIdentitySource:
		return false
	}
	return true
}

// deviceEnrollmentsPerMinute bounds code guessing from one address.
const deviceEnrollmentsPerMinute = 10

func deviceEnrollCommands(origin, code string) map[string]string {
	return map[string]string{
		"macos":   fmt.Sprintf(`curl -fsSL "%[1]s/install.sh" | sh -s -- --server "%[1]s" --device-code %[2]s`, origin, code),
		"windows": fmt.Sprintf(`& ([scriptblock]::Create((irm "%[1]s/install.ps1"))) -Server "%[1]s" -DeviceCode "%[2]s"`, origin, code),
	}
}

func (s *Server) handleCreateDeviceEnrollment(c echo.Context) error {
	if s.Runtimes == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "temporarily_unavailable"})
	}
	if !personalAccountSession(c) {
		return c.JSON(http.StatusForbidden, map[string]string{"code": "account_session_required", "error": "添加设备需要用本人账号登录，密钥不能办理"})
	}
	enrollment, err := s.Runtimes.IssueDeviceEnrollment(c.Request().Context(), getTenant(c), getUserID(c))
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "注册码签发失败"})
	}
	return c.JSON(http.StatusCreated, map[string]any{
		"code": enrollment.Code, "expires_at": enrollment.ExpiresAt,
		"commands": deviceEnrollCommands(requestOrigin(c.Request()), enrollment.Code),
	})
}

// handleDeviceEnrollmentExchange trades a code for a runtime token. It takes no
// credentials: the code is the credential, so failures never say whose it was.
func (s *Server) handleDeviceEnrollmentExchange(c echo.Context) error {
	if s.Runtimes == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "temporarily_unavailable"})
	}
	if wait := s.deviceEnrollLimiter.take("ip:"+c.RealIP(), deviceEnrollmentsPerMinute, time.Now()); wait > 0 {
		return c.JSON(http.StatusTooManyRequests, map[string]string{"code": "rate_limited", "error": "尝试过于频繁，请一分钟后再试"})
	}
	var request struct {
		Code      string `json:"code"`
		Hostname  string `json:"hostname"`
		Platform  string `json:"platform"`
		Arch      string `json:"arch"`
		RuntimeID string `json:"runtime_id"`
	}
	if err := c.Bind(&request); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"code": "invalid_request", "error": "请求无效"})
	}
	if !supportedDevicePlatform(request.Platform, request.Arch) {
		return c.JSON(http.StatusBadRequest, map[string]string{"code": "unsupported_platform", "error": "设备只支持 macOS 与 Windows"})
	}
	device, err := s.Runtimes.EnrollDevice(c.Request().Context(), request.Code, request.Hostname, strings.TrimSpace(request.RuntimeID))
	switch {
	case errors.Is(err, runtimes.ErrEnrollmentCodeInvalid):
		return c.JSON(http.StatusNotFound, map[string]string{"code": "enrollment_code_invalid", "error": "注册码不对，请检查后重试"})
	case errors.Is(err, runtimes.ErrEnrollmentCodeExpired):
		return c.JSON(http.StatusGone, map[string]string{"code": "enrollment_code_expired", "error": "注册码已过期，请重新获取"})
	case errors.Is(err, runtimes.ErrEnrollmentCodeUsed):
		return c.JSON(http.StatusConflict, map[string]string{"code": "enrollment_code_used", "error": "注册码已经用过，请重新获取"})
	case errors.Is(err, runtimes.ErrRuntimeBindingMismatch):
		return c.JSON(http.StatusConflict, map[string]string{"code": "runtime_binding_mismatch", "error": "这台机器已绑定到其他账号"})
	case err != nil:
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "设备注册失败"})
	}
	return c.JSON(http.StatusOK, map[string]string{"runtime_id": device.RuntimeID, "token": device.Token, "name": device.Name})
}

func supportedDevicePlatform(osName, arch string) bool {
	switch osName + "/" + arch {
	case "darwin/amd64", "darwin/arm64", "windows/amd64", "windows/arm64":
		return true
	}
	return false
}
