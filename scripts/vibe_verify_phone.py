#!/usr/bin/env python3
"""Verify a phone on an existing Vibe account (POST /account/verify-phone).

The number is read from a 0600 file and masked in the output.
"""
import json
import os
import sys
import urllib.error
import urllib.request

BASE = "https://thevibehosting.com"
SECRETS = os.path.expanduser("~/secrets/ai-sandbox-vibe")
PHONE_FILE = os.path.expanduser("~/tmp/vibe/phone")


def api_key():
    for name in sorted(os.listdir(SECRETS)):
        if name.endswith(".json"):
            with open(os.path.join(SECRETS, name)) as fh:
                data = json.load(fh)
            if data.get("api_key"):
                return data["api_key"]
    return ""


def mask(text, phone):
    if not phone:
        return text
    digits = phone.lstrip("+")
    out = text.replace(phone, phone[:2] + "***" + phone[-2:])
    if len(digits) > 4:
        out = out.replace(digits, digits[:2] + "***" + digits[-2:])
    return out


def post(payload, token):
    req = urllib.request.Request(
        BASE + "/account/verify-phone", data=json.dumps(payload).encode(), method="POST"
    )
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
    if step == "send":
        status, text = post({"phone": phone, "consent": True}, api_key())
    else:
        code = sys.argv[1]
        status, text = post({"phone": phone, "code": code}, api_key())
    print("HTTP", status)
    print(mask(text, phone))


if __name__ == "__main__":
    main()
