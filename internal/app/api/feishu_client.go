package api

// Adapted from jinyitao123/nexus, orchestration/internal/notify/feishu.go,
// commit 100890744f1189d149f4000919539fb057dd4c75. Transport only; Weave owns work.
import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// workspace is empty for the deployment app read from the environment, which
// serves only workspaces that have not saved an app of their own.
type feishuClient struct {
	appID, appSecret, base, verificationToken, encryptKey, tenantKey string
	workspace                                                        string
	client                                                           *http.Client
	mu                                                               sync.Mutex
	token                                                            string
	tokenExp                                                         time.Time
}

// feishuAPIBase is replaced only by tests that stand in for the provider.
var feishuAPIBase = "https://open.feishu.cn"

func newFeishuClient(workspace, appID, appSecret, verificationToken, encryptKey, tenantKey string) *feishuClient {
	return &feishuClient{appID: appID, appSecret: appSecret, verificationToken: verificationToken, encryptKey: encryptKey, tenantKey: tenantKey, workspace: workspace,
		base: feishuAPIBase, client: &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func feishuFromEnv() *feishuClient {
	f := newFeishuClient("", strings.TrimSpace(os.Getenv("WEAVE_FEISHU_APP_ID")), os.Getenv("WEAVE_FEISHU_APP_SECRET"),
		os.Getenv("WEAVE_FEISHU_VERIFICATION_TOKEN"), os.Getenv("WEAVE_FEISHU_ENCRYPT_KEY"), strings.TrimSpace(os.Getenv("WEAVE_FEISHU_TENANT_KEY")))
	if f.appID == "" && f.appSecret == "" {
		return nil
	}
	return f
}

func (f *feishuClient) configured() bool {
	return f != nil && f.appID != "" && f.appSecret != "" && f.verificationToken != "" && f.encryptKey != "" && f.tenantKey != ""
}

type feishuResponse struct {
	Code   *int   `json:"code"`
	Token  string `json:"tenant_access_token"`
	Expire int    `json:"expire"`
	Data   struct {
		MessageID string `json:"message_id"`
	} `json:"data"`
}

// Do not persist raw provider messages or URL errors: these may contain secrets.
func (f *feishuClient) post(ctx context.Context, path, token string, payload any) (feishuResponse, error) {
	var out feishuResponse
	data, err := json.Marshal(payload)
	if err != nil {
		return out, errors.New("feishu request encoding failed")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.base+path, bytes.NewReader(data))
	if err != nil {
		return out, errors.New("feishu endpoint invalid")
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return out, errors.New("feishu transport unavailable")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, (64<<10)+1))
	if err != nil || len(body) > 64<<10 {
		return out, errors.New("feishu response unavailable")
	}
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("feishu HTTP status %d", resp.StatusCode)
	}
	if json.Unmarshal(body, &out) != nil || out.Code == nil {
		return out, errors.New("feishu response invalid")
	}
	if *out.Code != 0 {
		return out, fmt.Errorf("feishu API code %d", *out.Code)
	}
	return out, nil
}

func (f *feishuClient) tenantToken(ctx context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.token != "" && time.Now().Before(f.tokenExp) {
		return f.token, nil
	}
	out, err := f.post(ctx, "/open-apis/auth/v3/tenant_access_token/internal", "", map[string]string{"app_id": f.appID, "app_secret": f.appSecret})
	if err != nil {
		return "", err
	}
	if out.Token == "" || out.Expire <= 0 || out.Expire > 7200 {
		return "", errors.New("feishu token response invalid")
	}
	f.token = out.Token
	f.tokenExp = time.Now().Add(time.Duration(out.Expire-min(60, out.Expire/2)) * time.Second)
	return f.token, nil
}

func (f *feishuClient) send(ctx context.Context, openID, key, text string) (string, error) {
	if f == nil || openID == "" || key == "" || len(key) > 50 || len(text) > 30000 {
		return "", errors.New("feishu message invalid")
	}
	tok, err := f.tenantToken(ctx)
	if err != nil {
		return "", err
	}
	content, _ := json.Marshal(map[string]string{"text": text})
	out, err := f.post(ctx, "/open-apis/im/v1/messages?receive_id_type="+url.QueryEscape("open_id"), tok, map[string]string{"receive_id": openID, "msg_type": "text", "content": string(content), "uuid": key})
	if err != nil {
		// Invalidate only the token this request used. Retry belongs to the event worker.
		f.mu.Lock()
		if f.token == tok {
			f.token = ""
		}
		f.mu.Unlock()
		return "", err
	}
	if strings.TrimSpace(out.Data.MessageID) == "" {
		return "", errors.New("feishu message receipt missing")
	}
	return out.Data.MessageID, nil
}
