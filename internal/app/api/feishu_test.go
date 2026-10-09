package api

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testFeishuClient(t *testing.T) (*feishuClient, *atomic.Int32, *[]string) {
	t.Helper()
	tokens := &atomic.Int32{}
	sent := &[]string{}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "tenant_access_token") {
			tokens.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "tenant_access_token": "test-token", "expire": 7200})
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("missing message token")
		}
		var payload map[string]string
		if json.NewDecoder(r.Body).Decode(&payload) != nil {
			t.Error("message payload invalid")
		}
		if payload["receive_id"] == "" || payload["uuid"] == "" || r.URL.Query().Get("receive_id_type") != "open_id" {
			t.Error("missing private recipient/deduplication")
		}
		*sent = append(*sent, payload["content"])
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]string{"message_id": "reply-" + payload["uuid"]}})
	}))
	t.Cleanup(provider.Close)
	return &feishuClient{appID: "app", appSecret: "test-secret", verificationToken: "verify", encryptKey: "encrypt", tenantKey: "tenant", base: provider.URL, client: provider.Client()}, tokens, sent
}

func signedFeishuRequest(f *feishuClient, body any, encrypt bool) *http.Request {
	raw, _ := json.Marshal(body)
	if encrypt {
		key := sha256.Sum256([]byte(f.encryptKey))
		block, _ := aes.NewCipher(key[:])
		iv := bytes.Repeat([]byte{1}, 16)
		padding := 16 - len(raw)%16
		plain := append(raw, bytes.Repeat([]byte{byte(padding)}, padding)...)
		encrypted := make([]byte, len(plain))
		cipher.NewCBCEncrypter(block, iv).CryptBlocks(encrypted, plain)
		raw, _ = json.Marshal(map[string]string{"encrypt": base64.StdEncoding.EncodeToString(append(iv, encrypted...))})
	}
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	nonce := "test-nonce"
	sum := sha256.Sum256(append([]byte(timestamp+nonce+f.encryptKey), raw...))
	request := httptest.NewRequest(http.MethodPost, "/v1/integrations/feishu/events", bytes.NewReader(raw))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Lark-Request-Timestamp", timestamp)
	request.Header.Set("X-Lark-Request-Nonce", nonce)
	request.Header.Set("X-Lark-Signature", hex.EncodeToString(sum[:]))
	return request
}

func testFeishuMessage(f *feishuClient, id, openID, text string) map[string]any {
	content, _ := json.Marshal(map[string]string{"text": text})
	return map[string]any{"schema": "2.0", "header": map[string]string{"event_type": "im.message.receive_v1", "token": f.verificationToken, "app_id": f.appID, "tenant_key": f.tenantKey},
		"event": map[string]any{"sender": map[string]any{"sender_id": map[string]string{"open_id": openID}, "sender_type": "user", "tenant_key": f.tenantKey},
			"message": map[string]string{"message_id": id, "chat_id": "chat-" + openID, "chat_type": "p2p", "message_type": "text", "content": string(content)}}}
}

func TestFeishuTokenCacheAndHonestReceipts(t *testing.T) {
	f, tokens, _ := testFeishuClient(t)
	for i := 0; i < 2; i++ {
		if _, err := f.send(t.Context(), "open-user", "same-key", "测试消息"); err != nil {
			t.Fatal(err)
		}
	}
	if tokens.Load() != 1 {
		t.Fatal("token was not reused")
	}
	for _, test := range []struct {
		name   string
		status int
		body   string
	}{
		{"HTTP failure", 500, `{"code":0}`}, {"invalid JSON", 200, `{`}, {"missing code", 200, `{}`}, {"API failure", 200, `{"code":999,"msg":"test-secret"}`}, {"missing receipt", 200, `{"code":0}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			f.base = server.URL
			f.client = server.Client()
			f.token = "test-token"
			f.tokenExp = time.Now().Add(time.Hour)
			_, err := f.send(t.Context(), "user", "key", "text")
			if err == nil || strings.Contains(err.Error(), "test-secret") {
				t.Fatalf("false success or provider secret leaked: %v", err)
			}
		})
	}
}

func TestFeishuSignedEncryptedCallbacks(t *testing.T) {
	f, _, _ := testFeishuClient(t)
	body := testFeishuMessage(f, "message", "user", "团队")
	for _, encrypted := range []bool{false, true} {
		event, err := f.decodeEvent(signedFeishuRequest(f, body, encrypted))
		if err != nil || event.Event.Message.MessageID != "message" {
			t.Fatalf("decode encrypted=%v err=%v", encrypted, err)
		}
	}
	req := signedFeishuRequest(f, body, true)
	req.Header.Set("X-Lark-Signature", "wrong")
	if _, err := f.decodeEvent(req); err == nil {
		t.Fatal("forged signature accepted")
	}
	req = signedFeishuRequest(f, body, true)
	req.Header.Set("X-Lark-Request-Timestamp", "1")
	if _, err := f.decodeEvent(req); err == nil {
		t.Fatal("old event accepted")
	}
	body["header"].(map[string]string)["token"] = "wrong"
	if _, err := f.decodeEvent(signedFeishuRequest(f, body, true)); err == nil {
		t.Fatal("wrong verification token accepted")
	}
}

func TestFeishuUnsignedEncryptedURLVerification(t *testing.T) {
	f, _, _ := testFeishuClient(t)
	req := signedFeishuRequest(f, map[string]string{"type": "url_verification", "token": f.verificationToken, "challenge": "proof"}, true)
	req.Header.Del("X-Lark-Signature")
	req.Header.Del("X-Lark-Request-Timestamp")
	req.Header.Del("X-Lark-Request-Nonce")
	event, err := f.decodeEvent(req)
	if err != nil || event.Challenge != "proof" {
		t.Fatalf("official unsigned challenge rejected: %v", err)
	}
	req = signedFeishuRequest(f, map[string]string{"type": "url_verification", "token": "wrong", "challenge": "proof"}, true)
	req.Header.Del("X-Lark-Signature")
	if _, err := f.decodeEvent(req); err == nil {
		t.Fatal("untrusted challenge accepted")
	}
}
