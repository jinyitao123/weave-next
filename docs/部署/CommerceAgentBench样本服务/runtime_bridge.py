#!/usr/bin/env python3
"""Keep upstream task URLs unchanged while using the public HTTP fixture."""
import argparse
import hmac
import http.client
import json
import os
import subprocess
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlsplit


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path != "/health":
            self.send_error(404)
            return
        data = json.dumps({"ok": True, "run_id": self.server.run_id,
                           "transport": "public_http"}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_POST(self):
        if self.path != "/mcp" or self.headers.get("Transfer-Encoding"):
            self.send_error(404)
            return
        supplied = self.headers.get("X-Weave-Bridge-Token", "")
        if not hmac.compare_digest(supplied, self.server.bridge_token):
            self.send_error(401)
            return
        try:
            length = int(self.headers.get("Content-Length", "0"))
        except ValueError:
            self.send_error(400)
            return
        if not 0 <= length <= 8 * 1024 * 1024:
            self.send_error(413)
            return
        self.connection.settimeout(30)
        target = self.server.target
        connection = http.client.HTTPConnection(target.hostname, target.port, timeout=65)
        try:
            headers = {"Authorization": "Bearer " + self.server.token,
                       "Content-Type": "application/json", "Accept": "application/json"}
            connection.request("POST", target.path, self.rfile.read(length), headers)
            response = connection.getresponse()
            data = response.read(8 * 1024 * 1024 + 1)
            if len(data) > 8 * 1024 * 1024:
                self.send_error(502)
                return
            self.send_response(response.status)
            self.send_header("Content-Type", response.getheader("Content-Type", "application/json"))
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)
        except (OSError, http.client.HTTPException):
            self.send_error(502, "Public fixture unavailable")
        finally:
            connection.close()


def required_setting(parser, name):
    value = os.environ.get(name, "").strip()
    if not value:
        parser.error(f"required environment setting is missing: {name}")
    return value


def target_for_run(parser, base_url, run_id):
    try:
        base = urlsplit(base_url)
        _ = base.port
    except ValueError:
        parser.error("WEAVE_BRIDGE_TARGET_RUNS_URL is invalid")
    if (
        base.scheme != "http"
        or not base.hostname
        or base.username
        or base.password
        or base.query
        or base.fragment
        or not base.path.rstrip("/").endswith("/runs")
    ):
        parser.error("WEAVE_BRIDGE_TARGET_RUNS_URL must be an http /runs URL without credentials, query, or fragment")
    path = f"{base.path.rstrip('/')}/{run_id}/mcp"
    return base._replace(path=path)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description="Local authenticated bridge for a configured Weave MCP run.")
    parser.add_argument("--run-id", required=True)
    parser.add_argument("--port", type=int, default=3071)
    args = parser.parse_args()
    if not args.run_id or any(c not in "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_.-" for c in args.run_id):
        parser.error("invalid run ID")

    target_base = required_setting(parser, "WEAVE_BRIDGE_TARGET_RUNS_URL")
    keychain_account = required_setting(parser, "WEAVE_BRIDGE_KEYCHAIN_ACCOUNT")
    api_token_service = required_setting(parser, "WEAVE_BRIDGE_API_TOKEN_SERVICE")
    bridge_token_service = required_setting(parser, "WEAVE_BRIDGE_TOKEN_SERVICE")
    target = target_for_run(parser, target_base, args.run_id)

    token = subprocess.run(
        ["/usr/bin/security", "find-generic-password", "-a", keychain_account, "-s", api_token_service, "-w"],
        capture_output=True,
        text=True,
        check=True,
    ).stdout.strip()
    bridge_token = subprocess.run(
        ["/usr/bin/security", "find-generic-password", "-a", keychain_account, "-s", bridge_token_service, "-w"],
        capture_output=True,
        text=True,
        check=True,
    ).stdout.strip()
    if not token or not bridge_token:
        raise SystemExit("required bridge credential is empty")
    server = ThreadingHTTPServer(("127.0.0.1", args.port), Handler)
    server.token = token
    server.bridge_token = bridge_token
    server.run_id = args.run_id
    server.target = target
    server.serve_forever()
