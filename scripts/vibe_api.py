#!/usr/bin/env python3
"""Read-only Vibe Hosting API helper.

Usage:
  vibe_api.py get /account
  vibe_api.py get /servers
  vibe_api.py get /servers/srv_xxx/usage
"""
import json
import os
import sys
import urllib.error
import urllib.request

BASE = "https://thevibehosting.com"
SECRETS = os.path.expanduser("~/secrets/ai-sandbox-vibe")


def api_key():
    for name in sorted(os.listdir(SECRETS)):
        if name.endswith(".json"):
            with open(os.path.join(SECRETS, name)) as fh:
                data = json.load(fh)
            if data.get("api_key"):
                return data["api_key"]
    return ""


def main():
    if len(sys.argv) < 3:
        print(__doc__)
        return 1
    method, path = sys.argv[1], sys.argv[2]
    req = urllib.request.Request(BASE + path, method=method.upper())
    token = api_key()
    if token:
        req.add_header("authorization", "Bearer " + token)
    try:
        with urllib.request.urlopen(req, timeout=60) as resp:
            body = resp.read().decode()
            status = resp.status
    except urllib.error.HTTPError as exc:
        body = exc.read().decode()
        status = exc.code
    except Exception as exc:  # noqa: BLE001
        print("error:", exc)
        return 1
    try:
        print(json.dumps(json.loads(body), indent=2))
    except Exception:  # noqa: BLE001
        print(body[:2000])
    return 0 if status < 400 else 1


if __name__ == "__main__":
    sys.exit(main())
