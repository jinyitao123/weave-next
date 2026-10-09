package api

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"
)

type feishuEvent struct {
	Type      string `json:"type"`
	Token     string `json:"token"`
	Challenge string `json:"challenge"`
	Header    struct {
		EventType string `json:"event_type"`
		Token     string `json:"token"`
		AppID     string `json:"app_id"`
		TenantKey string `json:"tenant_key"`
	} `json:"header"`
	Event struct {
		Sender struct {
			SenderID struct {
				OpenID string `json:"open_id"`
			} `json:"sender_id"`
			SenderType string `json:"sender_type"`
			TenantKey  string `json:"tenant_key"`
		} `json:"sender"`
		Message struct {
			MessageID   string `json:"message_id"`
			ChatID      string `json:"chat_id"`
			ChatType    string `json:"chat_type"`
			MessageType string `json:"message_type"`
			Content     string `json:"content"`
		} `json:"message"`
	} `json:"event"`
}

type feishuCommand struct {
	BindingCodeHash string                     `json:"binding_code_hash,omitempty"`
	Text            string                     `json:"text"`
	ChatID          string                     `json:"chat_id"`
	Registration    *dispatchInputRegistration `json:"registration,omitempty"`
	Human           *completeHumanTaskRequest  `json:"human,omitempty"`
	HumanRunID      string                     `json:"human_run_id,omitempty"`
}

func constantEqual(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

func (f *feishuClient) decodeEvent(r *http.Request) (feishuEvent, error) {
	var event feishuEvent
	raw, err := io.ReadAll(io.LimitReader(r.Body, (256<<10)+1))
	if err != nil || len(raw) > 256<<10 {
		return event, errors.New("invalid event body")
	}
	signatureBody := raw
	var envelope struct {
		Encrypt string `json:"encrypt"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return event, errors.New("invalid event envelope")
	}
	if envelope.Encrypt != "" {
		ciphertext, err := base64.StdEncoding.DecodeString(envelope.Encrypt)
		if err != nil || len(ciphertext) < 32 || (len(ciphertext)-16)%aes.BlockSize != 0 {
			return event, errors.New("invalid event encryption")
		}
		key := sha256.Sum256([]byte(f.encryptKey))
		block, _ := aes.NewCipher(key[:])
		plain := make([]byte, len(ciphertext)-16)
		cipher.NewCBCDecrypter(block, ciphertext[:16]).CryptBlocks(plain, ciphertext[16:])
		padding := int(plain[len(plain)-1])
		if padding < 1 || padding > 16 || padding > len(plain) {
			return event, errors.New("invalid event padding")
		}
		if !bytes.Equal(plain[len(plain)-padding:], bytes.Repeat([]byte{byte(padding)}, padding)) {
			return event, errors.New("invalid event padding")
		}
		raw = plain[:len(plain)-padding]
	}
	if json.Unmarshal(raw, &event) != nil {
		return event, errors.New("invalid event JSON")
	}
	token := event.Header.Token
	if event.Type == "url_verification" {
		token = event.Token
	}
	if !constantEqual(token, f.verificationToken) {
		return event, errors.New("invalid event token")
	}
	// Feishu's URL verification handshake has no signature headers; it only
	// proves the encrypted challenge token and never creates a work receipt.
	if event.Type == "url_verification" {
		return event, nil
	}
	timestamp, nonce, signature := r.Header.Get("X-Lark-Request-Timestamp"), r.Header.Get("X-Lark-Request-Nonce"), r.Header.Get("X-Lark-Signature")
	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil || nonce == "" || len(nonce) > 256 || seconds < time.Now().Unix()-300 || seconds > time.Now().Unix()+300 {
		return event, errors.New("invalid event time")
	}
	hash := sha256.Sum256(append([]byte(timestamp+nonce+f.encryptKey), signatureBody...))
	if !constantEqual(signature, hex.EncodeToString(hash[:])) {
		return event, errors.New("invalid event signature")
	}
	return event, nil
}

func (s *Server) handleFeishuEvent(c echo.Context) error {
	if !s.Feishu.configured() || s.GetPool() == nil {
		return c.NoContent(http.StatusServiceUnavailable)
	}
	event, err := s.Feishu.decodeEvent(c.Request())
	if err != nil {
		return c.NoContent(http.StatusUnauthorized)
	}
	if event.Type == "url_verification" {
		if len(event.Challenge) > 4096 {
			return c.NoContent(400)
		}
		return c.JSON(200, map[string]string{"challenge": event.Challenge})
	}
	if event.Header.AppID != s.Feishu.appID || event.Header.TenantKey != s.Feishu.tenantKey || event.Event.Sender.TenantKey != s.Feishu.tenantKey {
		return c.NoContent(http.StatusForbidden)
	}
	m := event.Event.Message
	sender := event.Event.Sender
	if event.Header.EventType != "im.message.receive_v1" || sender.SenderType != "user" || m.ChatType != "p2p" || m.MessageType != "text" {
		return c.JSON(200, map[string]bool{"accepted": false})
	}
	var content struct {
		Text string `json:"text"`
	}
	if json.Unmarshal([]byte(m.Content), &content) != nil || strings.TrimSpace(content.Text) == "" || len(content.Text) > 32000 || sender.SenderID.OpenID == "" || len(sender.SenderID.OpenID) > 256 || m.MessageID == "" || len(m.MessageID) > 256 || m.ChatID == "" {
		return c.NoContent(400)
	}
	commandData := feishuCommand{Text: content.Text, ChatID: m.ChatID}
	if strings.HasPrefix(strings.TrimSpace(content.Text), "绑定 ") {
		commandData.BindingCodeHash = dispatchInputDigest([]byte(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(content.Text), "绑定 "))))
		commandData.Text = "绑定"
	}
	command, _ := json.Marshal(commandData)
	hash := dispatchInputDigest([]byte(m.Content + "\x1f" + sender.SenderID.OpenID + "\x1f" + m.ChatID))
	tag, err := s.GetPool().Exec(c.Request().Context(), `INSERT INTO weave_feishu_messages(app_id,message_id,open_id,content_hash,command,workspace_id,user_id,response)
 SELECT $1,$2,$3,$4,$5::jsonb,link.workspace_id,link.user_id,
 CASE WHEN link.user_id IS NULL AND $6 NOT LIKE '绑定 %' THEN '请先在桌面登录，并使用绑定码发送：绑定 <码>。' END
 FROM (SELECT 1) AS seed LEFT JOIN weave_feishu_links AS link ON link.app_id=$1 AND link.open_id=$3 AND link.expires_at>statement_timestamp()
 ON CONFLICT(app_id,message_id) DO UPDATE SET message_id=EXCLUDED.message_id
 WHERE weave_feishu_messages.content_hash=EXCLUDED.content_hash`, s.Feishu.appID, m.MessageID, sender.SenderID.OpenID, hash, string(command), strings.TrimSpace(content.Text))
	if err != nil {
		return c.NoContent(503)
	}
	if tag.RowsAffected() != 1 {
		return c.NoContent(409)
	}
	return c.JSON(200, map[string]bool{"accepted": true})
}

func (s *Server) handleFeishuLinkCode(c echo.Context) error {
	if !s.Feishu.configured() || s.GetPool() == nil {
		return c.NoContent(503)
	}
	if source, _ := c.Get(identitySourceContextKey).(string); source != "forge" {
		return c.NoContent(403)
	}
	if err := s.requireCurrentWorkspaceMember(c); err != nil {
		return err
	}
	// Bindings never outlive the already verified employee session.
	claims, err := parseFeishuLinkClaims(c, s.Config.JWTSecret)
	if err != nil {
		return c.NoContent(401)
	}
	expires := time.Now().Add(externalSessionTTL)
	if claims.ExpiresAt.Time.Before(expires) {
		expires = claims.ExpiresAt.Time
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return c.NoContent(503)
	}
	code := hex.EncodeToString(raw)
	sets, _ := json.Marshal(claims.PermissionSets)
	_, err = s.GetPool().Exec(c.Request().Context(), `INSERT INTO weave_feishu_links(app_id,workspace_id,user_id,code_hash,code_expires_at,permission_sets,expires_at)
 VALUES($1,$2,$3,$4,statement_timestamp()+interval '10 minutes',$5::jsonb,$6)
 ON CONFLICT(app_id,workspace_id,user_id) DO UPDATE SET code_hash=EXCLUDED.code_hash,code_expires_at=EXCLUDED.code_expires_at,
 permission_sets=EXCLUDED.permission_sets,expires_at=EXCLUDED.expires_at`, s.Feishu.appID, getTenant(c), getUserID(c), dispatchInputDigest([]byte(code)), string(sets), expires)
	if err != nil {
		return c.NoContent(503)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(200, map[string]any{"code": code, "code_expires_in_seconds": 600, "binding_expires_at": expires})
}

func (s *Server) handleFeishuUnlink(c echo.Context) error {
	if s.Feishu == nil || s.GetPool() == nil {
		return c.NoContent(503)
	}
	if source, _ := c.Get(identitySourceContextKey).(string); source != "forge" {
		return c.NoContent(403)
	}
	_, err := s.GetPool().Exec(c.Request().Context(), `DELETE FROM weave_feishu_links WHERE app_id=$1 AND workspace_id=$2 AND user_id=$3`, s.Feishu.appID, getTenant(c), getUserID(c))
	if err != nil {
		return c.NoContent(503)
	}
	return c.NoContent(204)
}

func feishuMessageKey(app, message string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("weave-feishu\x1f"+app+"\x1f"+message)).String()
}

func parseFeishuLinkClaims(c echo.Context, secret string) (*Claims, error) {
	claims := &Claims{}
	raw := strings.TrimPrefix(c.Request().Header.Get("Authorization"), "Bearer ")
	token, err := jwt.ParseWithClaims(raw, claims, func(*jwt.Token) (any, error) { return []byte(secret), nil }, jwt.WithValidMethods([]string{"HS256"}))
	if err != nil || !token.Valid || claims.ExpiresAt == nil || claims.UserID != getUserID(c) || claims.TenantID != getTenant(c) || claims.IdentitySource != "forge" {
		return nil, errors.New("invalid employee session")
	}
	return claims, nil
}

func (s *Server) handleFeishuBinding(c echo.Context) error {
	c.Response().Header().Set("Cache-Control", "no-store")
	if source, _ := c.Get(identitySourceContextKey).(string); source != "forge" {
		return c.NoContent(403)
	}
	if !s.Feishu.configured() || s.GetPool() == nil {
		return c.JSON(200, map[string]bool{"available": false, "bound": false})
	}
	var expires *time.Time
	err := s.GetPool().QueryRow(c.Request().Context(), `SELECT expires_at FROM weave_feishu_links WHERE app_id=$1 AND workspace_id=$2 AND user_id=$3 AND open_id IS NOT NULL AND expires_at>statement_timestamp()`, s.Feishu.appID, getTenant(c), getUserID(c)).Scan(&expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return c.JSON(200, map[string]bool{"available": true, "bound": false})
	}
	if err != nil {
		return c.NoContent(503)
	}
	return c.JSON(200, map[string]any{"available": true, "bound": true, "expires_at": expires})
}
