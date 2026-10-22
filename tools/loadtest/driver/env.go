package main

import (
	"bufio"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
)

// pgTarget is the PostgreSQL server the load test uses. Credentials come from
// an env file and are never printed.
type pgTarget struct {
	host, port, user, password string
}

// forbiddenDatabases are never connected to: "weave" belongs to the running
// Weave instance on :8080.
var forbiddenDatabases = map[string]bool{"weave": true}

func readEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	return out, sc.Err()
}

func loadPGTarget(envFile string) (pgTarget, error) {
	env := map[string]string{}
	if envFile != "" {
		var err error
		if env, err = readEnvFile(envFile); err != nil {
			return pgTarget{}, fmt.Errorf("read env file: %w", err)
		}
	}
	get := func(k, def string) string {
		if v := strings.TrimSpace(os.Getenv("LOADTEST_" + k)); v != "" {
			return v
		}
		if v := env[k]; v != "" {
			return v
		}
		return def
	}
	t := pgTarget{host: get("PGHOST", "127.0.0.1"), port: get("PGPORT", "5432"), user: get("PGUSER", ""), password: get("PGPASSWORD", "")}
	if t.user == "" {
		// Fall back to DATABASE_URL credentials, ignoring its database name.
		if raw := env["DATABASE_URL"]; raw != "" {
			if u, err := url.Parse(raw); err == nil && u.User != nil {
				t.user = u.User.Username()
				t.password, _ = u.User.Password()
				if h := u.Hostname(); h != "" {
					t.host = h
				}
				if p := u.Port(); p != "" {
					t.port = p
				}
			}
		}
	}
	if t.user == "" {
		return pgTarget{}, errors.New("no PostgreSQL user: set PGUSER in the env file or LOADTEST_PGUSER")
	}
	return t, nil
}

// url returns a connection URL for database db. It refuses forbidden
// databases so no code path can reach the production Weave database.
func (t pgTarget) url(db string, params map[string]string) (string, error) {
	if forbiddenDatabases[strings.ToLower(db)] {
		return "", fmt.Errorf("refusing to use database %q: it belongs to the running Weave", db)
	}
	u := url.URL{Scheme: "postgres", Host: t.host + ":" + t.port, Path: "/" + db}
	if t.password != "" {
		u.User = url.UserPassword(t.user, t.password)
	} else {
		u.User = url.User(t.user)
	}
	q := url.Values{}
	q.Set("sslmode", "disable")
	for k, v := range params {
		if v != "" {
			q.Set(k, v)
		}
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// redactURL hides the password in a connection URL for logs.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	if _, ok := u.User.Password(); ok {
		u.User = url.UserPassword(u.User.Username(), "***")
	}
	return u.String()
}
