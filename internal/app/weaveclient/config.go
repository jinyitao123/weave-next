package weaveclient

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	BaseURLEnv = "WEAVE_API_URL"
	APIKeyEnv  = "WEAVE_API_KEY"
)

type Config struct {
	BaseURL      string
	APIKey       string
	PollInterval time.Duration
	WaitTimeout  time.Duration
}

func ConfigFromEnv() (Config, error) {
	config := Config{
		BaseURL: strings.TrimSpace(os.Getenv(BaseURLEnv)),
		APIKey:  strings.TrimSpace(os.Getenv(APIKeyEnv)),
	}
	if config.BaseURL == "" {
		config.BaseURL = "http://127.0.0.1:8080"
	}
	if err := config.Validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}

func (c Config) Validate() error {
	parsed, err := url.Parse(strings.TrimSpace(c.BaseURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("invalid %s", BaseURLEnv)
	}
	if !strings.HasPrefix(strings.TrimSpace(c.APIKey), "wv_sk_") {
		return fmt.Errorf("%s must contain a wv_sk_ API key", APIKeyEnv)
	}
	return nil
}
