#!/usr/bin/env python3
"""The MAX limits run (R38) through a maxapi 2.0 gateway.

Standard library only. See max-live-checks-2026-11.md, section 4. Run it only
from the owner's test account, in chats made for it, on the owner's word, and
only after the session has rested.

    GATEWAY=http://localhost:5555 TOKEN=... python3 max_limits_run.py \
        --chats -1,-2 --photos https://.../1.jpg,https://.../2.jpg --hours 12
"""

import argparse
import csv
import json
import os
import random
import sys
import time
import urllib.error
import urllib.request


def call(base, token, method, path, body=None, timeout=30):
    """One gateway call: (HTTP status, decoded body, seconds)."""
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(base + path, data=data, method=method)
    req.add_header("token", token)
    if data is not None:
        req.add_header("Content-Type", "application/json")
    started = time.monotonic()
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            status, raw = resp.status, resp.read()
    except urllib.error.HTTPError as err:
        status, raw = err.code, err.read()
    except (urllib.error.URLError, TimeoutError) as err:
        return 0, {"error": str(err)}, time.monotonic() - started
    try:
        payload = json.loads(raw or b"{}")
    except ValueError:
        payload = {"error": raw[:200].decode("utf-8", "replace")}
    return status, payload, time.monotonic() - started


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--chats", required=True, help="chat ids, comma-separated")
    ap.add_argument("--photos", required=True, help="photo URLs, comma-separated")
    ap.add_argument("--hours", type=float, default=1.0)
    ap.add_argument("--pause", default="3-5", help="random pause between sends, seconds, lo-hi")
    ap.add_argument("--max-photos", type=int, default=5)
    ap.add_argument("--out", default="max-run.csv")
    args = ap.parse_args()

    base = os.environ.get("GATEWAY", "http://localhost:5555").rstrip("/")
    token = os.environ["TOKEN"]
    chats = [int(c) for c in args.chats.split(",") if c.strip()]
    lo, hi = (float(x) for x in args.pause.split("-"))

    status, st, _ = call(base, token, "GET", "/session/status")
    if status != 200 or not st.get("loggedIn") or st.get("paused"):
        sys.exit(f"the account is not ready: {status} {st}")

    tokens = []
    for url in args.photos.split(","):
        status, body, secs = call(base, token, "POST", "/media/photo", {"url": url.strip()})
        if status != 200:
            sys.exit(f"upload of {url} failed: {status} {body}")
        tokens.append(body["attachment"])
        print(f"uploaded {url} in {secs:.1f}s")
        time.sleep(random.uniform(lo, hi))

    deadline = time.time() + args.hours * 3600
    counts, last_unknown, n = {}, None, 0
    with open(args.out, "w", newline="") as f:
        out = csv.writer(f)
        out.writerow(["time", "chat", "status", "code", "upstreamCode", "seconds", "photos", "messageId", "ref"])
        while time.time() < deadline:
            chat = chats[n % len(chats)]
            k = random.randint(1, min(args.max_photos, len(tokens)))
            ref = f"r38-{int(time.time())}-{n}"
            body = {"chatId": chat, "text": f"Тестовый пост {n}: платье, 1200 ₽, размеры 42–48", "notify": True,
                    "attachments": random.sample(tokens, k), "ref": ref}
            status, resp, secs = call(base, token, "POST", "/chat/send/text", body, timeout=15)
            code, upstream = resp.get("code", ""), resp.get("upstreamCode", "")
            out.writerow([time.strftime("%Y-%m-%dT%H:%M:%S"), chat, status, code, upstream, f"{secs:.2f}", k,
                          resp.get("messageId", ""), ref])
            f.flush()
            key = code or "ok"
            counts[key] = counts.get(key, 0) + 1
            n += 1
            if resp.get("paused"):
                print(f"stopped: the account is paused ({upstream}) after {n} sends")
                break
            if status in (0, 503) and code != "lookup_failed":
                print(f"stopped: no session ({resp}) after {n} sends")
                break
            if upstream and code != "send_failed":
                if upstream == last_unknown:
                    print(f"stopped: {upstream} twice in a row after {n} sends")
                    break
                last_unknown = upstream
            else:
                last_unknown = None
            time.sleep(random.uniform(lo, hi))
    print(json.dumps(counts, ensure_ascii=False))


if __name__ == "__main__":
    main()
