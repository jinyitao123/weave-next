// Package bootstrap creates the minimum headless administration identity.
package bootstrap

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jinyitao123/weave/internal/app/apikeys"
	"github.com/jinyitao123/weave/internal/app/users"
)

const defaultKeyName = "weave-bootstrap"

type UserStore interface {
	GetByUsername(context.Context, string, string) (*users.User, error)
	Create(context.Context, string, string, string, string, string) (*users.User, error)
	EnsureAdmin(context.Context, string, string) (*users.User, error)
	UpdatePassword(context.Context, string, string, string) error
}

type APIKeyStore interface {
	Create(context.Context, string, string, string, string, []string, *time.Time) (*apikeys.APIKey, string, error)
	List(context.Context, string) ([]apikeys.APIKey, error)
	Delete(context.Context, string, string) error
}

type Options struct {
	WorkspaceID   string
	Username      string
	Password      string
	ResetPassword bool
	APIURL        string
}

type Result struct {
	WorkspaceID       string   `json:"workspace_id"`
	AdminUserID       string   `json:"admin_user_id"`
	AdminUsername     string   `json:"admin_username"`
	AdminCreated      bool     `json:"admin_created"`
	PasswordReset     bool     `json:"password_reset"`
	GeneratedPassword string   `json:"generated_password,omitempty"`
	APIKeyID          string   `json:"api_key_id"`
	APIKeyCreated     bool     `json:"api_key_created"`
	APIKey            string   `json:"api_key,omitempty"`
	APIKeyScopes      []string `json:"api_key_scopes"`
	APIURL            string   `json:"api_url"`
}

type Service struct {
	Users UserStore
	Keys  APIKeyStore
}

func (s Service) Ensure(ctx context.Context, options Options) (Result, error) {
	options.WorkspaceID = strings.TrimSpace(options.WorkspaceID)
	options.Username = strings.TrimSpace(options.Username)
	options.APIURL = strings.TrimRight(strings.TrimSpace(options.APIURL), "/")
	if s.Users == nil || s.Keys == nil || options.WorkspaceID == "" || options.Username == "" ||
		options.APIURL == "" {
		return Result{}, errors.New("bootstrap stores, workspace, username, and API URL are required")
	}
	result := Result{WorkspaceID: options.WorkspaceID, AdminUsername: options.Username, APIURL: options.APIURL}
	user, err := s.Users.GetByUsername(ctx, options.WorkspaceID, options.Username)
	if err != nil {
		password := options.Password
		if password == "" {
			password, err = generatePassword()
			if err != nil {
				return Result{}, err
			}
			result.GeneratedPassword = password
		}
		user, err = s.Users.Create(ctx, options.WorkspaceID, options.Username, password, options.Username, "admin")
		if err != nil {
			return Result{}, fmt.Errorf("create bootstrap admin: %w", err)
		}
		result.AdminCreated = true
	} else {
		user, err = s.Users.EnsureAdmin(ctx, options.WorkspaceID, user.ID)
		if err != nil {
			return Result{}, fmt.Errorf("ensure bootstrap admin: %w", err)
		}
		if options.ResetPassword {
			password := options.Password
			if password == "" {
				password, err = generatePassword()
				if err != nil {
					return Result{}, err
				}
				result.GeneratedPassword = password
			}
			if err := s.Users.UpdatePassword(ctx, options.WorkspaceID, user.ID, password); err != nil {
				return Result{}, fmt.Errorf("reset bootstrap admin password: %w", err)
			}
			result.PasswordReset = true
		}
	}
	result.AdminUserID = user.ID

	scopes := apikeys.BootstrapScopes()
	keys, err := s.Keys.List(ctx, options.WorkspaceID)
	if err != nil {
		return Result{}, fmt.Errorf("list bootstrap API keys: %w", err)
	}
	var retained *apikeys.APIKey
	for index := range keys {
		key := keys[index]
		if key.Name != defaultKeyName {
			continue
		}
		if retained == nil && key.OwnerUserID == user.ID && key.Role == "admin" && slices.Equal(key.Scopes, scopes) {
			copy := key
			retained = &copy
			continue
		}
		if err := s.Keys.Delete(ctx, options.WorkspaceID, key.ID); err != nil {
			return Result{}, fmt.Errorf("replace incompatible bootstrap API key: %w", err)
		}
	}
	if retained == nil {
		key, raw, err := s.Keys.Create(ctx, options.WorkspaceID, defaultKeyName, "admin", user.ID, scopes, nil)
		if err != nil {
			return Result{}, fmt.Errorf("create bootstrap API key: %w", err)
		}
		retained, result.APIKey, result.APIKeyCreated = key, raw, true
	}
	result.APIKeyID = retained.ID
	result.APIKeyScopes = scopes
	return result, nil
}

func generatePassword() (string, error) {
	random := make([]byte, 18)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generate bootstrap password: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(random), nil
}
