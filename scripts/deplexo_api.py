#!/usr/bin/env python3
"""Deplexo REST API helper (the CLI does not run on Android).

The API key lives in ~/secrets/ai-sandbox/deplexo.key (0600) and is sent as a
bearer token. Nothing is printed except what the API returns.

    scripts/deplexo_api.py whoami
    scripts/deplexo_api.py apps
    scripts/deplexo_api.py deploy --repo URL --name NAME --env-file FILE
    scripts/deplexo_api.py poll APP_UUID [DEPLOYMENT_UUID]
    scripts/deplexo_api.py logs DEPLOYMENT_UUID
    scripts/deplexo_api.py env APP_UUID --env-file FILE
    scripts/deplexo_api.py app APP_UUID
"""
import json
import os
import sys
import time
import urllib.error
import urllib.request

BASE = "https://deplexo.com/user/api/v1"
KEY_FILE = os.path.expanduser("~/secrets/ai-sandbox/deplexo.key")


def key():
    try:
        with open(KEY_FILE) as fh:
            return fh.read().strip()
    except OSError:
        print("no API key: create one in the Deplexo dashboard (Settings > API Keys) "
              f"and store it in {KEY_FILE}", file=sys.stderr)
        sys.exit(3)


def request(method, path, payload=None):
    req = urllib.request.Request(BASE + path, method=method)
    req.add_header("Authorization", "Bearer " + key())
    data = None
    if payload is not None:
        data = json.dumps(payload).encode()
        req.add_header("Content-Type", "application/json")
        req.data = data
    try:
        with urllib.request.urlopen(req, timeout=90) as resp:
            body = resp.read().decode()
            try:
                return resp.status, json.loads(body or "{}")
            except json.JSONDecodeError:
                return resp.status, body
    except urllib.error.HTTPError as exc:
        body = exc.read().decode()
        try:
            return exc.code, json.loads(body or "{}")
        except json.JSONDecodeError:
            return exc.code, body
    except Exception as exc:  # noqa: BLE001
        return 0, {"error": str(exc)}


def read_env_file(path):
    values = {}
    with open(path) as fh:
        for line in fh:
            line = line.strip()
            if not line or line.startswith("#") or "=" not in line:
                continue
            key, _, value = line.partition("=")
            values[key.strip()] = value.strip()
    return values


def arg_value(flag, default=None):
    if flag in sys.argv:
        i = sys.argv.index(flag)
        if i + 1 < len(sys.argv):
            return sys.argv[i + 1]
    return default


def main():
    if len(sys.argv) < 2:
        print(__doc__)
        return 1
    cmd = sys.argv[1]

    if cmd == "whoami":
        status, body = request("GET", "/me")
        print(status, json.dumps(body, indent=2) if isinstance(body, dict) else body)
        return 0 if status < 300 else 1

    if cmd == "apps":
        status, body = request("GET", "/apps")
        print(status, json.dumps(body, indent=2) if isinstance(body, dict) else body)
        return 0 if status < 300 else 1

    if cmd == "app":
        app_id = sys.argv[2]
        status, body = request("GET", "/apps/" + app_id)
        print(status, json.dumps(body, indent=2) if isinstance(body, dict) else body)
        return 0 if status < 300 else 1

    if cmd == "deploy":
        repo = arg_value("--repo")
        name = arg_value("--name", "ai-sandbox-control")
        env_file = arg_value("--env-file")
        env = read_env_file(env_file) if env_file else {}
        payload = {
            "repoUrl": repo,
            "name": name,
            "appType": "web",
            "framework": "dockerfile",
            "dockerfilePath": "Dockerfile",
            "mountPath": "/data",
        }
        if env:
            payload["env"] = env
        status, body = request("POST", "/deploy", payload)
        print(status, json.dumps(body, indent=2) if isinstance(body, dict) else body)
        return 0 if status < 300 else 1

    if cmd == "env":
        app_id = sys.argv[2]
        env_file = arg_value("--env-file")
        values = read_env_file(env_file)
        status, body = request("PUT", "/apps/" + app_id + "/env", {"vars": values})
        print(status, json.dumps(body, indent=2) if isinstance(body, dict) else body)
        return 0 if status < 300 else 1

    if cmd == "logs":
        deployment = sys.argv[2]
        status, body = request("GET", "/deployments/" + deployment + "/logs")
        if isinstance(body, dict):
            for key in ("status", "errorMessage"):
                if body.get(key):
                    print(key, ":", body[key])
            logs = body.get("buildLogs") or body.get("logs") or ""
            print(logs[-8000:] if isinstance(logs, str) else json.dumps(body, indent=2))
        else:
            print(status, body)
        return 0 if status < 300 else 1

    if cmd == "poll":
        app_id = sys.argv[2]
        deployment = sys.argv[3] if len(sys.argv) > 3 else None
        deadline = time.time() + 900
        seen = None
        while time.time() < deadline:
            if deployment:
                status, body = request("GET", "/deployments/" + deployment + "/logs")
                if isinstance(body, dict):
                    state = body.get("status")
                    if state != seen:
                        seen = state
                        print("deployment:", state)
                    if state in ("success", "failed"):
                        if state == "failed":
                            print("error:", body.get("errorMessage"))
                            logs = body.get("buildLogs") or body.get("logs") or ""
                            print(logs[-6000:])
                        break
            status, body = request("GET", "/apps/" + app_id)
            if isinstance(body, dict):
                app = body.get("app") or {}
                print("app:", app.get("status"), app.get("subdomain"))
                if app.get("status") == "running":
                    break
            time.sleep(10)
        status, body = request("GET", "/apps/" + app_id)
        print(json.dumps(body, indent=2) if isinstance(body, dict) else body)
        return 0

    print(__doc__)
    return 1


if __name__ == "__main__":
    sys.exit(main())
