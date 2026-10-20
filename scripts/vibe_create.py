#!/usr/bin/env python3
"""Create a Vibe Hosting account/server and store the credentials safely.

Prints a masked summary; the api_key and the SSH private key only ever land in
0600 files under ~/secrets/ai-sandbox-vibe/.
"""
import json
import os
import re
import stat
import sys
import urllib.error
import urllib.request

BASE = "https://thevibehosting.com"
SECRETS = os.path.expanduser("~/secrets/ai-sandbox-vibe")


def post(path, payload, token=None):
    req = urllib.request.Request(
        BASE + path, data=json.dumps(payload).encode(), method="POST"
    )
    req.add_header("content-type", "application/json")
    if token:
        req.add_header("authorization", "Bearer " + token)
    try:
        with urllib.request.urlopen(req, timeout=90) as resp:
            return resp.status, json.loads(resp.read().decode() or "{}")
    except urllib.error.HTTPError as exc:
        try:
            body = json.loads(exc.read().decode() or "{}")
        except Exception:  # noqa: BLE001
            body = {"raw": exc.read().decode()[:400]}
        return exc.code, body
    except Exception as exc:  # noqa: BLE001
        return 0, {"error": str(exc)}


def write_secret(path, data):
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "w") as fh:
        fh.write(data)
    os.chmod(path, 0o600)


def main():
    subs = sys.argv[1:] or ["ai-sandbox-control", "sandbox-control", "ai-sandbox"]
    never_sleep = os.environ.get("NEVER_SLEEP") == "1"
    status, body = post("/create", {"subdomains": subs, "never_sleep": never_sleep})
    print("HTTP", status)
    if status != 200:
        print(json.dumps(body)[:800])
        return 1
    os.makedirs(SECRETS, mode=0o700, exist_ok=True)
    os.chmod(SECRETS, 0o700)
    slug = body.get("subdomain", "server").split(".")[0]
    key = body.get("ssh_private_key", "")
    if key:
        write_secret(os.path.join(SECRETS, slug + ".key"), key)
    meta = {
        "id": body.get("id"),
        "subdomain": body.get("subdomain"),
        "ssh": body.get("ssh"),
        "api_key": body.get("api_key"),
        "subdomain_note": body.get("subdomain_note"),
        "account_note": body.get("account_note"),
        "never_sleep": never_sleep,
    }
    write_secret(
        os.path.join(SECRETS, slug + ".json"),
        json.dumps(meta, indent=2) + "\n",
    )
    print(json.dumps({k: v for k, v in meta.items() if k != "api_key"}, indent=2))
    api = body.get("api_key") or ""
    print("api_key:", (api[:10] + "…") if api else "none")
    print("saved:", SECRETS)
    return 0


if __name__ == "__main__":
    sys.exit(main())
