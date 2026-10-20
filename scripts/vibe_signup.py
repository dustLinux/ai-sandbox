#!/usr/bin/env python3
"""Vibe Hosting API helper: sends the phone, then finishes the signup.

The phone number is read from a 0600 file and never printed back: every
occurrence in the response is masked before it reaches stdout.
"""
import json
import os
import sys
import urllib.error
import urllib.request

BASE = "https://thevibehosting.com"
PHONE_FILE = os.path.expanduser("~/tmp/vibe/phone")


def mask(text, phone):
    if not phone:
        return text
    digits = phone.lstrip("+")
    out = text.replace(phone, phone[:2] + "***" + phone[-2:])
    if len(digits) > 4:
        out = out.replace(digits, digits[:2] + "***" + digits[-2:])
    return out


def request(path, payload, token=None):
    body = json.dumps(payload).encode()
    req = urllib.request.Request(BASE + path, data=body, method="POST")
    req.add_header("content-type", "application/json")
    if token:
        req.add_header("authorization", "Bearer " + token)
    try:
        with urllib.request.urlopen(req, timeout=60) as resp:
            return resp.status, resp.read().decode()
    except urllib.error.HTTPError as exc:
        return exc.code, exc.read().decode()
    except Exception as exc:  # noqa: BLE001
        return 0, json.dumps({"error": str(exc)})


def main():
    step = sys.argv[1] if len(sys.argv) > 1 else "send"
    phone = open(PHONE_FILE).read().strip()
    subs = sys.argv[2:] or ["ai-sandbox-control", "sandbox-control", "ai-sandbox"]
    if step == "send":
        status, text = request("/create", {"phone": phone})
    else:
        code = sys.argv[1]
        status, text = request(
            "/create",
            {"phone": phone, "code": code, "subdomains": subs, "never_sleep": True},
        )
    print("HTTP", status)
    print(mask(text, phone))


if __name__ == "__main__":
    main()
