package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jinyitao123/loom/pgstore"
	"github.com/jinyitao123/weave/internal/app/apikeys"
	appbootstrap "github.com/jinyitao123/weave/internal/app/bootstrap"
	"github.com/jinyitao123/weave/internal/app/users"
	"github.com/jinyitao123/weave/internal/base/db"
)

func runBootstrapCommand(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("weave bootstrap", flag.ContinueOnError)
	flags.SetOutput(stderr)
	workspaceID := flags.String("workspace", "default", "workspace ID")
	username := flags.String("username", bootstrapUsernameDefault(), "administrator username")
	password := flags.String("password", strings.TrimSpace(os.Getenv("WEAVE_ADMIN_PASS")), "administrator password")
	resetPassword := flags.Bool("reset-password", false, "explicitly reset the existing administrator password")
	apiURL := flags.String("api-url", bootstrapAPIURLDefault(), "Weave API URL for MCP registration")
	command := flags.String("command", bootstrapCommandDefault(), "absolute weave executable path")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		writeBootstrapError(stderr, "invalid_arguments", "")
		return 2
	}
	databaseURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if databaseURL == "" {
		writeBootstrapError(stderr, "database_url_required", "")
		return 2
	}
	store, err := pgstore.New(databaseURL)
	if err != nil {
		writeBootstrapError(stderr, "database_unavailable", err.Error())
		return 1
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.Migrate(ctx); err != nil {
		writeBootstrapError(stderr, "database_migration_failed", err.Error())
		return 1
	}
	if err := db.Migrate(ctx, store.Pool()); err != nil {
		writeBootstrapError(stderr, "database_migration_failed", err.Error())
		return 1
	}
	result, err := (appbootstrap.Service{
		Users: users.NewStore(store.Pool()), Keys: apikeys.NewStore(store.Pool()),
	}).Ensure(ctx, appbootstrap.Options{
		WorkspaceID: *workspaceID, Username: *username, Password: *password,
		ResetPassword: *resetPassword, APIURL: *apiURL, Command: *command,
	})
	if err != nil {
		writeBootstrapError(stderr, "bootstrap_failed", err.Error())
		return 1
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(result); err != nil {
		writeBootstrapError(stderr, "output_failed", "")
		return 1
	}
	return 0
}

func bootstrapUsernameDefault() string {
	if value := strings.TrimSpace(os.Getenv("WEAVE_ADMIN_USER")); value != "" {
		return value
	}
	return "admin"
}

func bootstrapAPIURLDefault() string {
	if value := strings.TrimSpace(os.Getenv("WEAVE_API_URL")); value != "" {
		return value
	}
	port := strings.TrimSpace(os.Getenv("PORT"))
	if port == "" {
		port = "8080"
	}
	return "http://127.0.0.1:" + port
}

func bootstrapCommandDefault() string {
	executable, err := os.Executable()
	if err != nil {
		return "weave"
	}
	absolute, err := filepath.Abs(executable)
	if err != nil {
		return executable
	}
	return absolute
}

func writeBootstrapError(writer io.Writer, code, detail string) {
	payload := map[string]string{"error": code}
	if strings.TrimSpace(detail) != "" {
		payload["detail"] = detail
	}
	_ = json.NewEncoder(writer).Encode(payload)
}
