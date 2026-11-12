"""Exercise the official backend on a disposable ARM64 Android emulator over adb.

Usage: python3 verify_android.py /path/to/adb emulator-SERIAL /path/to/adviser-android-arm64
The emulator discards its data when the disposable session ends. No phone accounts are used.
"""
import json
import secrets
import subprocess
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path


def main(adb, serial, binary):
    cli = [adb, "-s", serial]

    def run(*args):
        return subprocess.check_output([*cli, *args], text=True).strip()

    assert run("shell", "getprop", "ro.boot.qemu") == "1", "Use a disposable emulator"
    assert run("shell", "getprop", "ro.product.cpu.abi") == "arm64-v8a"
    base = "/data/local/tmp/adviser-" + secrets.token_hex(6)
    run("shell", "mkdir", "-m", "700", base)
    run("push", str(Path(binary).resolve()), base + "/adviser")
    run("shell", "chmod", "700", base + "/adviser")
    command = base + "/adviser"
    db = base + "/memory.sqlite"
    owner = run("shell", command, "issue", "-db", db)
    guest = run("shell", command, "issue", "-db", db)
    port = run("forward", "tcp:0", "tcp:17341")
    transport = urllib.request.build_opener(urllib.request.ProxyHandler({}))

    def call(token, method, route, payload=None, want=200):
        data = json.dumps(payload).encode() if payload is not None else None
        req = urllib.request.Request("http://127.0.0.1:" + port + route, data=data, method=method,
            headers={"Authorization": "Bearer " + token, "Content-Type": "application/json"})
        try:
            response = transport.open(req, timeout=3)
        except urllib.error.HTTPError as exc:
            response = exc
        with response:
            assert response.status == want, (method, route, response.status)
            return json.load(response) if response.status == 200 else None

    def start(zone):
        script = f"echo $$ > {base}/pid && TZ={zone} exec {command} serve -db {db} -listen 127.0.0.1:17341"
        process = subprocess.Popen([*cli, "shell", "-T", script], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        for _ in range(60):
            if process.poll() is not None:
                raise RuntimeError("Android backend exited during startup")
            try:
                call(owner, "GET", "/v1/model")
                return process
            except OSError:
                time.sleep(0.1)
        raise TimeoutError("Could not reach Android backend")

    def stop(process):
        pid = run("shell", "cat", base + "/pid")
        assert pid.isdigit()
        run("shell", "kill", "-TERM", pid)
        process.wait(timeout=8)

    process = None
    try:
        process = start("Pacific/Honolulu")
        assert call(guest, "GET", "/v1/portrait") == []
        source = call(owner, "POST", "/v1/sources", {"kind":"self", "title":"近况", "text":"我每周练习钢琴，打算去考级。"})
        row, = call(owner, "GET", "/v1/portrait")
        assert row["quote"] in source["text"]
        call(guest, "GET", "/v1/sources/" + source["id"], want=404)
        wording = "我练钢琴是为了陪家人合奏。"
        call(owner, "PATCH", "/v1/memories/" + row["id"], {"text":wording,"version":row["version"]})
        first_day = run("shell", "TZ=Pacific/Honolulu", "date", "+%F")
        stop(process)
        process = None
        process = start("Pacific/Kiritimati")
        second_day = run("shell", "TZ=Pacific/Kiritimati", "date", "+%F")
        result = call(owner, "GET", "/v1/portrait")
        assert len(result) == 1 and result[0]["text"] == wording and result[0]["confirmed"]
        assert call(guest, "GET", "/v1/portrait") == []
        assert second_day > first_day
        print("Android", run("shell", "getprop", "ro.build.version.release"), "ARM64",
              "two separate users; source checked; signed-off correction survives process restart;",
              "local dates by timezone", first_day, second_day)
    finally:
        if process is not None and process.poll() is None:
            stop(process)
        run("forward", "--remove", "tcp:" + port)


if __name__ == "__main__":
    main(*sys.argv[1:])
