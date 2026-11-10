import base64
from concurrent.futures import ThreadPoolExecutor
import shutil
import ssl
import sys
import http.client
import json
import os
from pathlib import Path
import re
import subprocess
import time
import sqlite3
import tempfile
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlencode

class Echo(BaseHTTPRequestHandler):
    def log_message(self, format, *args):
        pass

    def do_HEAD(self):
        self.send_response(204)
        self.end_headers()

    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", "0")))
        reply = json.dumps([self.command, self.path, self.headers.get("Authorization"), body.decode(), self.headers.get("X-Gateway-Key")]).encode()
        self.send_response(201)
        self.send_header("Content-Length", str(len(reply)))
        self.end_headers()
        self.wfile.write(reply)

def request(port, method, path, body=None, headers=None, location=False, tls=None):
    conn = (http.client.HTTPSConnection("127.0.0.1", port, timeout=10, context=tls) if tls
            else http.client.HTTPConnection("127.0.0.1", port, timeout=10))
    try:
        conn.request(method, path, body, headers or {})
        response = conn.getresponse()
        payload = response.read()
        return response.status, response.getheader("Location") if location else payload
    finally:
        conn.close()

def start(db, *args):
    server = subprocess.Popen(
        [binary, "-db", str(db), "-listen", "127.0.0.1:0", "-admin-listen", "127.0.0.1:0", *args],
        env=dict(os.environ, GATEWAY_ADMIN_PASSWORD=password), stderr=subprocess.PIPE, text=True,
    )
    line = server.stderr.readline()
    ports = re.findall(r"127\.0\.0\.1:(\d+)", line)
    if len(ports) != 2:
        server.kill()
        server.wait()
        raise RuntimeError(line)
    return server, *map(int, ports)

binary = str(Path(sys.argv[1]).resolve())
password = "local-fixture-password"
authorization = "Basic " + base64.b64encode(f"admin:{password}".encode()).decode()
upstream = ThreadingHTTPServer(("127.0.0.1", 0), Echo)
threading.Thread(target=upstream.serve_forever, daemon=True).start()

with tempfile.TemporaryDirectory() as directory:
    db = Path(directory) / "catalog.db"
    with sqlite3.connect(db) as connection:
        connection.execute("CREATE TABLE api_imports (slug TEXT PRIMARY KEY NOT NULL, upstream TEXT NOT NULL, routes TEXT NOT NULL, published INTEGER NOT NULL)")
        for slug, published in (("legacy-live", 1), ("legacy-draft", 0)):
            connection.execute("INSERT INTO api_imports VALUES (?,?,?,?)", (slug, f"http://127.0.0.1:{upstream.server_port}/api", json.dumps([{"Method":"POST","Path":"/legacy"}]), published))
    server, gateway, admin = start(db)
    try:
        assert request(admin, "GET", "/")[0] == 401
        assert request(gateway, "GET", "/")[0] == 404
        assert request(gateway, "POST", "/gateway/legacy-live/legacy", "migrated")[0] == 201
        assert request(gateway, "POST", "/gateway/legacy-draft/legacy", "draft")[0] == 404
        assert request(gateway, "GET", "/healthz")[0] == 200
        form = urlencode({"slug": "billing", "upstream": f"http://127.0.0.1:{upstream.server_port}/api"})
        headers = {"Authorization": authorization, "Content-Type": "application/x-www-form-urlencoded"}
        assert request(admin, "POST", "/apis", form, headers | {"Origin": "https://example.com"})[0] == 403
        assert request(admin, "POST", "/apis", form, headers)[0] == 303
        assert request(admin, "POST", "/apis", form, headers)[0] == 409
        assert b"/gateway/billing/" in request(admin, "GET", "/", headers=headers)[1]
        status, body = request(gateway, "POST", "/gateway/billing/a%2Fb?x=1&x=2", "sample", {"Authorization": "Bearer demo"})
        assert status == 201 and json.loads(body) == ["POST", "/api/a%2Fb?x=1&x=2", "Bearer demo", "sample", None], (status, body)
        boundary = "gateway-fixture"
        document = "openapi: 3.1.0\npaths:\n  /invoices/{id}:\n    post:\n      responses: {}\n"
        sections = []
        for name, value in {"slug": "invoices", "upstream": f"http://127.0.0.1:{upstream.server_port}/api", "document": document}.items():
            filename = '; filename="openapi.yaml"' if name == "document" else ""
            sections.append(f'--{boundary}\r\nContent-Disposition: form-data; name="{name}"{filename}\r\n\r\n{value}\r\n')
        multipart = "".join(sections) + f"--{boundary}--\r\n"
        upload_headers = headers | {"Content-Type": f"multipart/form-data; boundary={boundary}"}
        assert request(admin, "POST", "/imports", multipart, upload_headers)[0] == 303
        assert request(admin, "POST", "/imports", multipart, upload_headers)[0] == 409
        status, body = request(admin, "GET", "/imports/invoices", headers=headers)
        assert status == 200 and b"/gateway/invoices/invoices/{id}" in body, (status, body)
        assert request(gateway, "POST", "/gateway/invoices/invoices/42", "draft")[0] == 404
        assert b"HEAD returned HTTP 204" in request(admin, "POST", "/imports/invoices/test", "revision=1", headers)[1]
        assert request(admin, "POST", "/imports/invoices/publish", "revision=1", headers)[0] == 303
        update = multipart.replace(f"--{boundary}--\r\n", f'--{boundary}\r\nContent-Disposition: form-data; name="revision"\r\n\r\n1\r\n--{boundary}--\r\n').replace("/invoices/{id}", "/revised/{id}")
        assert request(admin, "POST", "/imports", update, upload_headers)[0] == 303
        assert request(admin, "POST", "/imports/invoices/publish", "revision=1", headers)[0] == 404
        assert request(gateway, "POST", "/gateway/invoices/revised/42", "draft")[0] == 404
        assert request(admin, "POST", "/imports/invoices/publish", "revision=2", headers)[0] == 303
        assert request(gateway, "POST", "/gateway/invoices/revised/42", "revised")[0] == 201
        assert request(gateway, "POST", "/gateway/invoices/invoices/42", "old route")[0] == 404
        assert b"Activate v1" in request(admin, "GET", "/revisions/invoices", headers=headers)[1]
        assert request(admin, "POST", "/revisions/invoices/rollback", "revision=1", headers)[0] == 303
        assert request(gateway, "POST", "/gateway/invoices/revised/42", "rolled back")[0] == 404
        status, body = request(gateway, "POST", "/gateway/invoices/invoices/42?ref=1", "published")
        assert status == 201 and json.loads(body)[1] == "/api/invoices/42?ref=1"
        for path in ("/gateway/invoices/unlisted", "/gateway/invoices/invoices/%2e%2e", "/gateway/invoices/invoices/a%2Fb", "/gateway/invoices/invoices/..;x"):
            assert request(gateway, "POST", path, "blocked")[0] == 404, path
        assert request(gateway, "GET", "/gateway/invoices/invoices/42")[0] == 404
        for _ in range(30):
            status, body = request(admin, "GET", "/requests?api=invoices&status=201", headers=headers)
            if status == 200 and b"/invoices/{id}" in body:
                break
            time.sleep(.1)
        else:
            raise AssertionError((status, body))
        assert b"ref=1" not in body and b"/invoices/42" not in body and b"Bearer demo" not in body
        conn = http.client.HTTPConnection("127.0.0.1", gateway, timeout=5)
        conn.request("POST", "/gateway/invoices/invoices/999?secret=private", "private body")
        response = conn.getresponse()
        request_id = response.getheader("X-Request-ID")
        assert request_id and response.status == 201
        response.read()
        conn.close()
        def post(path, **fields):
            return request(admin, "POST", path, urlencode(fields), headers)
        assert post("/access/teams", team="billing")[0] == 303
        status, page = post("/access/keys", team="billing", app="worker", api="billing")
        assert status == 200, (status, page)
        old_key = re.search(rb"gw_[A-Za-z0-9_-]{43}", page).group().decode()
        key_id = re.search(rb"/access/keys/([A-Z2-7]+)/rotate", page).group(1).decode()
        assert old_key.encode() not in request(admin, "GET", "/access", headers=headers)[1]
        assert post("/access/protect", api="billing")[0] == 303
        assert request(gateway, "POST", "/gateway/billing/", "anonymous")[0] == 401
        assert request(gateway, "POST", "/gateway/invoices/invoices/42", "wrong scope", {"X-Gateway-Key": old_key})[0] == 401
        status, result = request(gateway, "POST", "/gateway/billing/", "old", {"X-Gateway-Key": old_key})
        assert status == 201 and json.loads(result)[4] is None
        status, page = post(f"/access/keys/{key_id}/rotate", hours="24")
        assert status == 200, (status, page)
        new_key = re.search(rb"gw_[A-Za-z0-9_-]{43}", page).group().decode()
        assert old_key != new_key
        for key in (old_key, new_key):
            assert request(gateway, "POST", "/gateway/billing/", "overlap", {"X-Gateway-Key": key})[0] == 201
        def rule_ids():
            return re.findall(rb"/policies/([A-Z2-7]+)/remove", request(admin, "GET", "/policies", headers=headers)[1])
        assert post("/policies", scope="app", target="billing/worker", rate="1", burst="1", mode="enforce")[0] == 303
        rule = rule_ids()[-1].decode()
        assert request(gateway, "POST", "/gateway/billing/", "one", {"X-Gateway-Key": old_key})[0] == 201
        assert request(gateway, "POST", "/gateway/billing/", "same app", {"X-Gateway-Key": new_key})[0] == 429
        assert post(f"/policies/{rule}/remove")[0] == 303
        assert post("/policies", scope="cidr", target="127.0.0.1/32", mode="observe")[0] == 303
        rule = rule_ids()[-1].decode()
        assert request(gateway, "POST", "/gateway/billing/", "observed", {"X-Gateway-Key": new_key})[0] == 201
        assert post(f"/policies/{rule}/remove")[0] == 303
        assert post("/policies", scope="cidr", target="127.0.0.1", mode="enforce")[0] == 303
        rule = rule_ids()[-1].decode()
        assert request(gateway, "POST", "/gateway/billing/", "blocked", {"X-Gateway-Key": new_key})[0] == 403
        assert post(f"/policies/{rule}/remove")[0] == 303
        for _ in range(30):
            status, exported = request(admin, "GET", "/requests.csv?api=billing&status=429", headers=headers)
            if status == 200 and b"Rate limited" in exported:
                break
            time.sleep(.1)
        else:
            raise AssertionError((status, exported))
        assert old_key.encode() not in exported and new_key.encode() not in exported
        assert b"Upstream response" not in exported
        status, dashboard = request(admin, "GET", "/dashboard", headers=headers)
        assert status == 200 and b"Denied by gateway" in dashboard and b"/requests?api=billing" in dashboard
        assert post(f"/access/keys/{key_id}/revoke")[0] == 303
        assert request(gateway, "POST", "/gateway/billing/", "revoked", {"X-Gateway-Key": old_key})[0] == 401
        assert request(gateway, "POST", "/gateway/billing/", "replacement", {"X-Gateway-Key": new_key})[0] == 201
        parts = []
        for slug, spec in (("bulk-one", document), ("billing", document), ("bulk-invalid", "invalid"), ("bulk-two", document)):
            for name, value in {"slug": slug, "upstream": f"http://127.0.0.1:{upstream.server_port}/api", "document": spec}.items():
                filename = '; filename="openapi.yaml"' if name == "document" else ""
                parts.append(f'--{boundary}\r\nContent-Disposition: form-data; name="{name}"{filename}\r\n\r\n{value}\r\n')
        status, import_path = request(admin, "POST", "/jobs/import", "".join(parts)+f"--{boundary}--\r\n", upload_headers, location=True)
        assert status == 303 and import_path.startswith("/jobs/")
        status, body = request(admin, "GET", import_path, headers=headers)
        assert status == 200 and b"Run valid items" in body and b"/invoices/{id}" in body
        assert request(admin, "GET", "/imports/bulk-one", headers=headers)[0] == 404
        assert post(import_path + "/start")[0] == 303
        def wait_job(path, marker):
            for _ in range(80):
                status, body = request(admin, "GET", path, headers=headers)
                if status == 200 and marker in body:
                    return body
                time.sleep(.1)
            raise AssertionError((status, body))
        assert b"2 of 4 succeeded" in wait_job(import_path, b"Completed with errors")
        assert request(admin, "GET", "/imports/bulk-two", headers=headers)[0] == 200
        assert request(gateway, "POST", "/gateway/bulk-one/invoices/42", "still draft")[0] == 404
        assert b"Gateway slug already exists." in request(admin, "GET", import_path + "/errors", headers=headers)[1]
        assert post(import_path + "/retry")[0] == 303
        assert b"2 of 4 succeeded" in wait_job(import_path, b"Completed with errors")
        status, policy_path = request(admin, "POST", "/jobs/policies", urlencode({"rules": json.dumps([{"Scope":"cidr","Target":"203.0.113.0/24","Observe":True}])}), headers, location=True)
        assert status == 303 and post(policy_path + "/start")[0] == 303
        assert b"1 of 1 succeeded" in wait_job(policy_path, b"Complete")
        for number in range(505):
            assert request(gateway, "POST", f"/gateway/invoices/invoices/{number}", "export")[0] == 201
        for _ in range(80):
            with sqlite3.connect(str(db) + ".telemetry") as connection:
                count = connection.execute("SELECT count(*) FROM request_events WHERE api='invoices' AND status=201").fetchone()[0]
            if count >= 508:
                break
            time.sleep(.1)
        else:
            raise AssertionError(count)
        status, export_path = request(admin, "POST", "/jobs/export", "api=invoices&status=201", headers, location=True)
        assert status == 303
        wait_job(export_path, b"Download CSV")
        status, exported = request(admin, "GET", export_path + "/download", headers=headers)
        assert status == 200 and len(exported.splitlines()) == count + 1, (status, len(exported.splitlines()), count)
    finally:
        server.terminate()
        server.wait(timeout=5)
    with sqlite3.connect(str(db) + ".jobs") as connection:
        connection.execute("UPDATE job_items SET state='running' WHERE job_id=? AND position=0", (import_path.rsplit("/",1)[1],))
        connection.execute("UPDATE job_items SET state='running' WHERE job_id=? AND position=0", (policy_path.rsplit("/",1)[1],))
    server, gateway, admin = start(db)
    try:
        assert b"2 of 4 succeeded" in wait_job(import_path, b"Completed with errors")
        assert b"1 of 1 succeeded" in wait_job(policy_path, b"Complete")
        assert request(admin, "GET", export_path + "/download", headers=headers)[1] == exported
        with sqlite3.connect(db) as connection:
            assert connection.execute("SELECT count(*) FROM audit_events WHERE action='OpenAPI draft saved' AND target='bulk-one'").fetchone()[0] == 1
        with sqlite3.connect(str(db)+".access") as connection:
            assert connection.execute("SELECT count(*) FROM gateway_rules WHERE target='203.0.113.0/24'").fetchone()[0] == 1
        status, page = request(admin, "GET", "/audit", headers=headers)
        assert status == 200
        for entry in (b"API revision published", b"API revision reactivated", b"invoices v1", b"invoices v2", b"API key rotated", b"API key revoked", b"Traffic rule added", b"Traffic rule removed", b"<td>admin</td>"):
            assert entry in page, entry
        assert old_key.encode() not in page and new_key.encode() not in page
        with sqlite3.connect(db) as connection:
            assert connection.execute("SELECT count(*) FROM audit_events WHERE action='API revision published'").fetchone()[0] == 2
        status, body = request(admin, "GET", f"/requests?id={request_id}", headers=headers)
        assert status == 200 and request_id.encode() in body and b"/invoices/{id}" in body
        with sqlite3.connect(str(db) + ".telemetry") as connection:
            assert connection.execute("SELECT route, outcome, status FROM request_events WHERE request_id = ?", (request_id,)).fetchone() == ("/invoices/{id}", "Upstream response", 201)
        assert b"/gateway/billing/" in request(admin, "GET", "/", headers=headers)[1]
        assert request(gateway, "POST", "/gateway/billing/", "again", {"X-Gateway-Key": new_key})[0] == 201
        assert request(gateway, "POST", "/gateway/billing/", "again", {"X-Gateway-Key": old_key})[0] == 401
        assert request(gateway, "POST", "/gateway/invoices/invoices/99", "again")[0] == 201
        assert post("/access/teams", team="delegated")[0] == 303
        assert post("/members/grants", team="delegated", api="invoices", mode="allow")[0] == 303
        def portal_user(user, role):
            status, page = post("/members/users", username=user)
            assert status == 200
            secret = re.search(rb'aria-label="New portal password" readonly value="([^"]+)"', page).group(1)
            assert post("/members/roles", username=user, team="delegated", role=role)[0] == 303
            return headers | {"Authorization": "Basic " + base64.b64encode(user.encode() + b":" + secret).decode()}
        delegated = portal_user("casey", "team-admin")
        viewer = portal_user("drew", "viewer")
        for auth in (delegated, viewer):
            assert b"/gateway/invoices/" in request(admin, "GET", "/", headers=auth)[1]
            assert b"/gateway/billing/" not in request(admin, "GET", "/", headers=auth)[1]
            assert b"/requests?api=invoices" in request(admin, "GET", "/dashboard", headers=auth)[1]
            assert b"/requests?api=billing" not in request(admin, "GET", "/dashboard", headers=auth)[1]
            assert len(request(admin, "GET", "/requests.csv?api=billing", headers=auth)[1].splitlines()) == 1
            assert request(admin, "GET", "/audit", headers=auth)[0] == 403
        operator = portal_user("river", "operator")
        assert request(admin, "POST", "/imports", update, upload_headers | {"Authorization": viewer["Authorization"]})[0] == 403
        assert request(admin, "POST", "/imports", update, upload_headers | {"Authorization": operator["Authorization"]})[0] == 303
        assert request(admin, "GET", "/imports/invoices", headers=operator)[0] == 200
        assert request(admin, "POST", "/imports/invoices/publish", "revision=3", operator)[0] == 403
        assert request(gateway, "POST", "/gateway/invoices/revised/42", "still draft")[0] == 404
        fields = urlencode({"team": "delegated", "app": "portal-client", "api": "invoices"})
        assert request(admin, "POST", "/access/keys", fields, viewer)[0] == 403
        status, page = request(admin, "POST", "/access/keys", fields, delegated)
        assert status == 200, (status, page)
        delegated_key = re.search(rb'gw_[A-Za-z0-9_-]+', page).group().decode()
        assert request(admin, "POST", f"/access/keys/{key_id}/rotate", "hours=1", delegated)[0] == 403
        assert request(admin, "POST", f"/access/keys/{key_id}/revoke", "", delegated)[0] == 403
        assert post("/access/protect", api="invoices")[0] == 303
        assert request(gateway, "POST", "/gateway/invoices/invoices/99", "scoped", {"X-Gateway-Key": delegated_key})[0] == 201
        delegated_id = re.search(rb"/access/keys/([A-Z2-7]+)/rotate", page).group(1).decode()
        status, page = request(admin, "POST", f"/access/keys/{delegated_id}/rotate", "hours=1", delegated)
        assert status == 200
        replacement = re.search(rb"gw_[A-Za-z0-9_-]{43}", page).group().decode()
        assert request(gateway, "POST", "/gateway/invoices/invoices/99", "rotated", {"X-Gateway-Key": replacement})[0] == 201
        assert request(admin, "POST", f"/access/keys/{delegated_id}/revoke", "", delegated)[0] == 303
        assert request(gateway, "POST", "/gateway/invoices/invoices/99", "revoked", {"X-Gateway-Key": delegated_key})[0] == 401
        delegated_key = replacement
        assert post("/members/grants", team="delegated", api="invoices", mode="revoke")[0] == 303
        assert request(gateway, "POST", "/gateway/invoices/invoices/99", "denied", {"X-Gateway-Key": delegated_key})[0] == 401
        assert b"/gateway/invoices/" not in request(admin, "GET", "/", headers=delegated)[1]
        assert b"<td>casey</td>" in request(admin, "GET", "/audit", headers=headers)[1]
    finally:
        server.terminate()
        server.wait(timeout=5)

    server, gateway, admin = start(db, "-trusted-proxies", "127.0.0.1/32,192.0.2.4/32")
    try:
        assert post("/policies", scope="cidr", target="203.0.113.7", mode="enforce")[0] == 303
        base = {"X-Gateway-Key": new_key}
        for forwarded, expected in (("", 400), ("bad, 192.0.2.4", 400), ("198.51.100.2, 203.0.113.7, 192.0.2.4", 403), ("203.0.113.7, 198.51.100.2, 192.0.2.4", 201)):
            h = base | ({"X-Forwarded-For": forwarded} if forwarded else {})
            assert request(gateway, "POST", "/gateway/billing/", "proxy", h)[0] == expected, forwarded
    finally:
        server.terminate()
        server.wait(timeout=5)

    restored = Path(directory) / "restored.db"
    for suffix in ("", ".telemetry", ".access", ".jobs"):
        shutil.copy2(str(db) + suffix, str(restored) + suffix)
    shutil.copytree(str(db) + ".jobs.files", str(restored) + ".jobs.files")
    cert, key = Path(directory) / "cert.pem", Path(directory) / "key.pem"
    subprocess.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1",
                    "-subj", "/CN=127.0.0.1", "-addext", "subjectAltName=IP:127.0.0.1",
                    "-out", str(cert), "-keyout", str(key)], check=True, capture_output=True)
    tls = ssl.create_default_context(cafile=str(cert))
    server, gateway, admin = start(restored, "-tls-cert", str(cert), "-tls-key", str(key))
    try:
        assert request(gateway, "GET", "/healthz", tls=tls)[0] == 200
        assert request(admin, "GET", export_path + "/download", headers=headers, tls=tls)[1] == exported
        status, page = request(admin, "GET", "/dashboard?window=7d", headers=headers, tls=tls)
        assert status == 200 and b"p50 latency" in page and b"p99 latency" in page
        assert request(admin, "GET", "/dashboard?window=bad", headers=headers, tls=tls)[0] == 400
        assert request(admin, "GET", "/", headers=delegated, tls=tls)[0] == 200
        start_time = time.monotonic()
        def send(n):
            return request(gateway, "POST", "/gateway/billing/", str(n), {"X-Gateway-Key": new_key}, tls=tls)[0]
        with ThreadPoolExecutor(max_workers=12) as pool:
            assert list(pool.map(send, range(480))) == [201] * 480
        print(f"TLS load smoke: 480 requests over {time.monotonic()-start_time:.2f}s at concurrency 12")
    finally:
        server.terminate()
        assert server.wait(timeout=10) == 0
    with sqlite3.connect(str(restored) + ".telemetry") as connection:
        actual = connection.execute("SELECT count(*) FROM request_events WHERE api='billing' AND status=201").fetchone()[0]
    with sqlite3.connect(str(db) + ".telemetry") as connection:
        baseline = connection.execute("SELECT count(*) FROM request_events WHERE api='billing' AND status=201").fetchone()[0]
    assert actual == baseline + 480, (actual, baseline)

upstream.shutdown()
upstream.server_close()
print("Catalog, delegated keys, trusted proxies, bulk jobs, CSV, restart replay, HTTPS, backup restore and telemetry drain passed.")
