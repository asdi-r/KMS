#!/usr/bin/env python3
"""Reference client for AV Central <-> Netrasky KMS integration.

Implements the exact patterns the integration guide requires:
  - stable device_id (never random per install)
  - activation_token stored per device and overwritten on re-activation
  - error matrix: seat_limit_reached is not retried; expired/revoked surface
    a definitive state; 429/5xx use exponential backoff (fail-open)
  - state kept in a small JSON file standing in for the central's database

Usage:
  export KMS_URL=https://kms.116.212.74.168.sslip.io
  export KMS_LICENSE_KEY=XXXXX-XXXXX-XXXXX-XXXXX-XXXXX
  python3 av_central_client.py activate   <device_id> [hostname]
  python3 av_central_client.py validate   <device_id>
  python3 av_central_client.py deactivate <device_id>
  python3 av_central_client.py status                  # key-level check
Only the Python standard library is used.
"""

import json
import os
import sys
import time
import urllib.error
import urllib.request

BASE_URL = os.environ.get("KMS_URL", "https://kms.116.212.74.168.sslip.io")
LICENSE_KEY = os.environ.get("KMS_LICENSE_KEY", "")
STATE_FILE = os.environ.get("KMS_STATE_FILE", "kms_state.json")  # per-device tokens

RETRYABLE = {429, 500, 502, 503, 504}
BACKOFF_S = [1, 5, 30]  # then give up and rely on the next scheduled run


# ---------- tiny state store (stands in for the central's database) ----------

def load_state():
    try:
        with open(STATE_FILE, encoding="utf-8") as f:
            return json.load(f)
    except FileNotFoundError:
        return {"devices": {}}


def save_state(state):
    tmp = STATE_FILE + ".tmp"
    with open(tmp, "w", encoding="utf-8") as f:
        json.dump(state, f, indent=2)
    os.replace(tmp, STATE_FILE)


# ---------- HTTP with backoff on transient failures ----------

def post(path, payload):
    """POST JSON; returns (http_status, body_dict). Retries only transient errors."""
    body = json.dumps(payload).encode()
    last_err = None
    for attempt, delay in enumerate([0] + BACKOFF_S):
        if delay:
            time.sleep(delay)
        req = urllib.request.Request(
            BASE_URL + path, data=body,
            headers={"Content-Type": "application/json"}, method="POST")
        try:
            with urllib.request.urlopen(req, timeout=15) as resp:
                return resp.status, json.load(resp)
        except urllib.error.HTTPError as e:
            data = {}
            try:
                data = json.load(e)
            except Exception:
                pass
            if e.code in RETRYABLE and attempt < len(BACKOFF_S):
                last_err = f"HTTP {e.code}"
                continue
            return e.code, data
        except (urllib.error.URLError, TimeoutError) as e:
            last_err = str(e)
            if attempt < len(BACKOFF_S):
                continue
            # Fail-open: network is down, not the licence. Caller keeps last
            # known state and tries again on the next scheduled run.
            return 0, {"error": f"network failure after retries: {last_err}"}
    return 0, {"error": last_err or "unreachable"}


# ---------- operations ----------

def activate(device_id, hostname=""):
    status, r = post("/activate", {"key": LICENSE_KEY, "device_id": device_id,
                                   "hostname": hostname})
    if status == 200 and r.get("activated"):
        state = load_state()
        # Token rotates on every successful activate: always overwrite.
        state["devices"][device_id] = {
            "activation_token": r["activation_token"],
            "activated_at": r.get("activated_at"),
            "hostname": hostname,
        }
        save_state(state)
        print(f"OK seat {r['used_seats']}/{r['seats']} "
              f"(sisa {r['remaining_seats']}), token disimpan")
        return 0
    if status == 409 and r.get("reason") == "seat_limit_reached":
        # Definitive: do NOT retry. Operator must free a seat or add quota.
        print(f"KUOTA PENUH {r['used_seats']}/{r['seats']} — lepas seat mesin "
              "pensiun atau ajukan tambah kuota")
        return 2
    if status == 403:
        print(f"DITOLAK: key {r.get('reason')} — hubungi vendor")
        return 3
    print(f"GAGAL ({status or 'network'}): {r.get('error') or r.get('reason')}")
    return 1


def validate(device_id=None):
    payload = {"key": LICENSE_KEY}
    if device_id:
        payload["device_id"] = device_id
    status, r = post("/validate", payload)
    if status == 0:
        print(f"JARINGAN GAGAL — pertahankan status terakhir ({r['error']})")
        return 1
    if r.get("valid"):
        print(f"VALID seat {r['used_seats']}/{r['seats']}, "
              f"berlaku s/d {r.get('expires_at', '?')[:10]}")
        return 0
    reason = r.get("reason", "unknown")
    actions = {
        "expired": "kontrak habis — masa tenggang lokal + minta perpanjangan",
        "revoked": "key dicabut — hentikan penggunaan, hubungi vendor",
        "reissued": "key diganti — ambil key baru; aktivasi TIDAK perlu diulang",
        "device_not_activated": "panggil activate untuk device ini",
        "not_found": "key salah — periksa konfigurasi",
    }
    print(f"TIDAK VALID: {reason} — {actions.get(reason, 'lihat panduan')}")
    return 2


def deactivate(device_id):
    state = load_state()
    dev = state["devices"].get(device_id)
    if not dev:
        print("token tidak ditemukan di state lokal — lepas via portal admin/customer")
        return 1
    status, r = post("/deactivate", {"key": LICENSE_KEY, "device_id": device_id,
                                     "activation_token": dev["activation_token"]})
    if status == 200 and r.get("deactivated"):
        del state["devices"][device_id]
        save_state(state)
        print(f"SEAT DILEPAS — sisa {r['remaining_seats']}/{r['seats']}")
        return 0
    print(f"GAGAL ({status}): {r.get('reason') or r.get('error')}")
    return 1


def main():
    if not LICENSE_KEY:
        sys.exit("set KMS_LICENSE_KEY dulu")
    cmd = sys.argv[1] if len(sys.argv) > 1 else ""
    if cmd == "activate" and len(sys.argv) >= 3:
        sys.exit(activate(sys.argv[2], sys.argv[3] if len(sys.argv) > 3 else ""))
    if cmd == "validate" and len(sys.argv) >= 3:
        sys.exit(validate(sys.argv[2]))
    if cmd == "deactivate" and len(sys.argv) >= 3:
        sys.exit(deactivate(sys.argv[2]))
    if cmd == "status":
        sys.exit(validate(None))
    sys.exit(__doc__)


if __name__ == "__main__":
    main()
