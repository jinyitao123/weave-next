package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v4"

	"github.com/jinyitao123/weave/internal/app/users"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/config"
	"github.com/jinyitao123/weave/internal/kernel/runtimeprotocol"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
)

type deviceAdmissionReply struct {
	status int
	body   map[string]any
	err    error
}

func sendDeviceAdmission(host *httptest.Server, token, path string, body any) deviceAdmissionReply {
	raw, err := json.Marshal(body)
	if err != nil {
		return deviceAdmissionReply{err: err}
	}
	req, err := http.NewRequest(http.MethodPost, host.URL+path, bytes.NewReader(raw))
	if err != nil {
		return deviceAdmissionReply{err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set(runtimeprotocol.HeaderVersion, runtimeprotocol.ProtocolVersion)
	resp, err := host.Client().Do(req)
	if err != nil {
		return deviceAdmissionReply{err: err}
	}
	defer resp.Body.Close()
	reply := deviceAdmissionReply{status: resp.StatusCode}
	err = json.NewDecoder(resp.Body).Decode(&reply.body)
	if err != nil && err != io.EOF {
		reply.err = err
	}
	return reply
}

func deviceAdmissionFixture(t *testing.T) (*Server, *httptest.Server, string) {
	t.Helper()
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO weave_workspaces(id,slug,name) VALUES('ws','ws','ws');
		INSERT INTO weave_users(id,tenant_id,username,password,role) VALUES('alice','ws','alice','unused','member');
	`); err != nil {
		t.Fatal(err)
	}
	s := &Server{Echo: echo.New(), Config: &config.Config{JWTSecret: "device-admission-test"},
		UserStore: users.NewStore(pool), Pool: pool, Store: teamDispatchPoolStore{pool: pool},
		Runtimes: runtimes.NewStore(pool), Tasks: taskqueue.New(pool, nil, time.Minute)}
	s.registerRoutes()
	host := httptest.NewServer(s.Echo)
	t.Cleanup(host.Close)
	token, err := s.signJWTWithPermissionSets("ws", "alice", []string{"member"}, "forge", nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return s, host, token
}

func readyAdmissionDevice(t *testing.T, s *Server, host *httptest.Server, name string) string {
	t.Helper()
	enrollment, err := s.Runtimes.IssueDeviceEnrollment(t.Context(), "ws", "alice")
	if err != nil {
		t.Fatal(err)
	}
	device, err := s.Runtimes.EnrollDevice(t.Context(), enrollment.Code, name, "")
	if err != nil {
		t.Fatal(err)
	}
	reply := sendDeviceAdmission(host, device.Token, "/v1/runtime/hello", runtimeprotocol.HostHelloRequest{
		Versioned: runtimeprotocol.NewVersioned(), Engines: []string{}, TotalSlots: 1,
		Device: &runtimeprotocol.DeviceFacts{Platform: "darwin", Available: true, Levels: []string{"observe"}},
	})
	if reply.err != nil || reply.status != http.StatusOK {
		t.Fatalf("ready device: %+v", reply)
	}
	return device.RuntimeID
}

func openAdmissionDevice(host *httptest.Server, token, device, request, reason string) deviceAdmissionReply {
	return sendDeviceAdmission(host, token, "/v1/devices/"+device+"/sessions", map[string]string{"request_id": request, "reason": reason})
}

func TestDeviceSessionRequestBindsItsDeviceRealPG(t *testing.T) {
	s, host, token := deviceAdmissionFixture(t)
	a, b := readyAdmissionDevice(t, s, host, "Mac A"), readyAdmissionDevice(t, s, host, "Mac B")
	request := uuid.NewString()
	first := openAdmissionDevice(host, token, a, request, "查看桌面")
	if first.err != nil || first.status != http.StatusOK || first.body["device_id"] != a {
		t.Fatalf("first session: %+v", first)
	}
	if retry := openAdmissionDevice(host, token, a, request, " 查看桌面 "); retry.err != nil || retry.status != http.StatusOK || retry.body["session_id"] != first.body["session_id"] {
		t.Fatalf("same request: %+v", retry)
	}
	if changed := openAdmissionDevice(host, token, b, request, "查看桌面"); changed.err != nil || changed.status != http.StatusConflict || changed.body["code"] != "request_conflict" {
		t.Fatalf("same request on another device must conflict: %+v", changed)
	}
}

func TestDeviceExistingSessionRemembersNewRequestRealPG(t *testing.T) {
	s, host, token := deviceAdmissionFixture(t)
	a, b := readyAdmissionDevice(t, s, host, "Mac A"), readyAdmissionDevice(t, s, host, "Mac B")
	first := openAdmissionDevice(host, token, a, uuid.NewString(), "查看桌面")
	request := uuid.NewString()
	again := openAdmissionDevice(host, token, a, request, "检查窗口")
	if first.err != nil || again.err != nil || again.status != http.StatusOK || again.body["session_id"] != first.body["session_id"] {
		t.Fatalf("reuse active session: first=%+v again=%+v", first, again)
	}
	for _, input := range []struct{ device, reason string }{{b, "检查窗口"}, {a, "不同原因"}} {
		changed := openAdmissionDevice(host, token, input.device, request, input.reason)
		if changed.err != nil || changed.status != http.StatusConflict || changed.body["code"] != "request_conflict" {
			t.Fatalf("active-session request lost its content binding: %+v", changed)
		}
	}
	if err := s.Tasks.Cancel(context.Background(), "ws", first.body["session_id"].(string)); err != nil {
		t.Fatal(err)
	}
	if retry := openAdmissionDevice(host, token, a, request, "检查窗口"); retry.err != nil || retry.status != http.StatusOK || retry.body["session_id"] != first.body["session_id"] || retry.body["state"] != "ended" {
		t.Fatalf("retry after the active session ended must not create a new session: %+v", retry)
	}
}

func TestDeviceSessionConcurrentAdmissionRealPG(t *testing.T) {
	for _, scope := range []struct{ sameDevice, sameRequest bool }{{true, false}, {true, true}, {false, true}} {
		sameDevice := scope.sameDevice
		t.Run(fmt.Sprintf("same_device_%t_same_request_%t", sameDevice, scope.sameRequest), func(t *testing.T) {
			s, host, token := deviceAdmissionFixture(t)
			a, b := readyAdmissionDevice(t, s, host, "Mac A"), ""
			if sameDevice {
				b = a
			} else {
				b = readyAdmissionDevice(t, s, host, "Mac B")
			}
			// Keep an insert in flight while the other normal HTTP request arrives.
			// This exposes check-then-enqueue races without changing the API handler.
			if _, err := s.GetPool().Exec(t.Context(), `
				CREATE FUNCTION delay_device_session_insert() RETURNS trigger LANGUAGE plpgsql AS $$
				BEGIN IF NEW.kind='device_session' THEN PERFORM pg_sleep(0.2); END IF; RETURN NEW; END $$;
				CREATE TRIGGER delay_device_session_insert BEFORE INSERT ON weave_task_queue
				FOR EACH ROW EXECUTE FUNCTION delay_device_session_insert();
			`); err != nil {
				t.Fatal(err)
			}
			requests := []string{uuid.NewString(), uuid.NewString()}
			if scope.sameRequest {
				requests[1] = requests[0]
			}
			start := make(chan struct{})
			replies := make(chan deviceAdmissionReply, 2)
			for i, device := range []string{a, b} {
				go func() { <-start; replies <- openAdmissionDevice(host, token, device, requests[i], "查看桌面") }()
			}
			close(start)
			one, two := <-replies, <-replies
			if one.err != nil || two.err != nil {
				t.Fatalf("open sessions: %+v %+v", one, two)
			}
			if sameDevice {
				if one.status != http.StatusOK || two.status != http.StatusOK || one.body["session_id"] != two.body["session_id"] {
					t.Fatalf("concurrent opens must share the device's one session: %+v %+v", one, two)
				}
			} else if !((one.status == http.StatusOK && two.status == http.StatusConflict && two.body["code"] == "request_conflict") || (two.status == http.StatusOK && one.status == http.StatusConflict && one.body["code"] == "request_conflict")) {
				t.Fatalf("one request cannot concurrently open two devices: %+v %+v", one, two)
			}
			var count int
			if err := s.GetPool().QueryRow(t.Context(), `SELECT count(*) FROM weave_task_queue WHERE kind='device_session'`).Scan(&count); err != nil || count != 1 {
				t.Fatalf("durable sessions=%d, err=%v", count, err)
			}
		})
	}
}

func TestDeviceSessionAdmissionWithOneConnectionRealPG(t *testing.T) {
	s, host, token := deviceAdmissionFixture(t)
	device := readyAdmissionDevice(t, s, host, "Mac A")
	cfg := s.GetPool().Config()
	cfg.MaxConns = 1
	limited, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(limited.Close)
	s.Pool, s.Store = limited, teamDispatchPoolStore{pool: limited}
	s.UserStore, s.Runtimes = users.NewStore(limited), runtimes.NewStore(limited)
	s.Tasks = taskqueue.New(limited, nil, time.Minute)
	host.Client().Timeout = 5 * time.Second
	if reply := openAdmissionDevice(host, token, device, uuid.NewString(), "查看桌面"); reply.err != nil || reply.status != http.StatusOK {
		t.Fatalf("session admission must not need another connection while its transaction holds the only one: %+v", reply)
	}
}

func TestDeviceSessionRequestLimitKeepsRecordedRetriesRealPG(t *testing.T) {
	s, host, token := deviceAdmissionFixture(t)
	device := readyAdmissionDevice(t, s, host, "Mac A")
	original := uuid.NewString()
	reason := strings.Repeat("界", deviceSessionReasonLimit)
	first := openAdmissionDevice(host, token, device, original, reason)
	if first.err != nil || first.status != http.StatusOK {
		t.Fatalf("maximum-length reason: %+v", first)
	}
	if tooLong := openAdmissionDevice(host, token, device, uuid.NewString(), reason+"界"); tooLong.err != nil || tooLong.status != http.StatusBadRequest {
		t.Fatalf("reason over the limit: %+v", tooLong)
	}
	last := ""
	for range deviceSessionRequestLimit - 1 {
		last = uuid.NewString()
		if next := openAdmissionDevice(host, token, device, last, "检查窗口"); next.err != nil || next.status != http.StatusOK || next.body["session_id"] != first.body["session_id"] {
			t.Fatalf("request within limit: %+v", next)
		}
	}
	if overflow := openAdmissionDevice(host, token, device, uuid.NewString(), "检查窗口"); overflow.err != nil || overflow.status != http.StatusConflict || overflow.body["code"] != "session_request_limit" {
		t.Fatalf("a new request over the limit must be explicitly refused: %+v", overflow)
	}
	for _, request := range []struct{ id, reason string }{{original, reason}, {last, "检查窗口"}} {
		if retry := openAdmissionDevice(host, token, device, request.id, request.reason); retry.err != nil || retry.status != http.StatusOK || retry.body["session_id"] != first.body["session_id"] {
			t.Fatalf("recorded request must remain replayable at the limit: %+v", retry)
		}
	}
}

// A device session is a queued task. The task store only reads tasks of the
// authenticated subject, so another member cannot cancel it.
func TestJobCancelOnlyBySubmitterRealPG(t *testing.T) {
	s, host, alice := deviceAdmissionFixture(t)
	if _, err := s.Pool.Exec(t.Context(), `INSERT INTO weave_users(id,tenant_id,username,password,role) VALUES('bob','ws','bob','unused','member')`); err != nil {
		t.Fatal(err)
	}
	bob, err := s.signJWTWithPermissionSets("ws", "bob", []string{"member"}, "forge", nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	opened := openAdmissionDevice(host, alice, readyAdmissionDevice(t, s, host, "Mac A"), uuid.NewString(), "查看桌面")
	if opened.err != nil || opened.status != http.StatusOK {
		t.Fatalf("open session: %+v", opened)
	}
	path := "/v1/jobs/" + opened.body["session_id"].(string) + "/cancel"
	if other := sendDeviceAdmission(host, bob, path, nil); other.status == http.StatusOK {
		t.Fatalf("another member cancelled the task: %+v", other)
	}
	if own := sendDeviceAdmission(host, alice, path, nil); own.status != http.StatusOK {
		t.Fatalf("the submitter could not cancel: %+v", own)
	}
}
