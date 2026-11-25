"""Run saved Terraform plans and record only safe API metadata."""

import argparse
import base64
import json
import os
from pathlib import Path
import re
import shlex
import signal
import subprocess
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


CLUSTER = "k0rdent_kubernetes_cluster.test"
TAG = "terraform-provider-k0rdent/create-token"
POSTCONDITION = "The cluster isn't active."
CASES = {
    1: "Retry while creating",
    2: "Retry after creation settles",
    3: "Recover after runner loss",
    4: "Delete with a refused version",
    5: "Leave a reused name alone",
    6: "Adopt an already managed cluster before destroy",
    7: "Skip destroy when no cluster exists",
    8: "Refuse a missing or wrong token",
    9: "Move the singleton to the keyed address",
    10: "Delete while creation is still running",
}


class Failure(Exception):
    pass


def require(condition, message):
    if not condition:
        raise Failure(message)


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


OPENER = urllib.request.build_opener(NoRedirect())


def request(url, method, body=None, headers=None):
    req = urllib.request.Request(url, data=body, method=method, headers=headers or {})
    try:
        response = OPENER.open(req, timeout=45)
    except urllib.error.HTTPError as err:
        response = err
    with response:
        return response.code, response.headers, response.read()


def json_body(body):
    return json.dumps(body).encode()


class Recorder:
    def __init__(self, upstream, path):
        self.upstream = upstream
        self.path = path
        self.rows = []
        self.kill_on_create = None
        self.lock = threading.Lock()
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), self.handler())
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)

    def __enter__(self):
        self.thread.start()
        return self

    def __exit__(self, *_):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join()

    @property
    def endpoint(self):
        return f"http://127.0.0.1:{self.server.server_port}"

    def handler(self):
        recorder = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *_):
                pass

            def proxy(self):
                # Reject unrelated local clients before reading their headers or body.
                if self.headers.get("Host") != f"127.0.0.1:{self.server.server_port}" or any(
                    self.headers.get(h) for h in ("Origin", "Sec-Fetch-Mode", "Sec-Fetch-Site")
                ):
                    self.send_error(403)
                    return
                body = self.rfile.read(int(self.headers["Content-Length"])) if "Content-Length" in self.headers else None
                headers = {h: self.headers[h] for h in ("Authorization", "Content-Type", "User-Agent") if h in self.headers}
                path = urllib.parse.urlsplit(self.path).path
                if not self.path.startswith("/v1/"):
                    self.send_error(404)
                    return
                start = time.monotonic()
                try:
                    status, upstream_headers, data = request(recorder.upstream + self.path, self.command, body, headers)
                except (OSError, ValueError):
                    self.send_error(502)
                    return
                with recorder.lock:
                    recorder.rows.append((self.command, path, status))
                    with recorder.path.open("a") as log:
                        print(self.command, path, status, f"{time.monotonic() - start:.3f}s", file=log)
                    if recorder.kill_on_create and self.command == "POST" and 200 <= status < 300:
                        runner, collection, name = recorder.kill_on_create
                        if path == collection and json.loads(body or b"{}").get("id") == name and runner.poll() is None:
                            # The platform answered, but the provider hasn't received that answer yet.
                            runner.kill()
                            recorder.kill_on_create = None
                try:
                    self.send_response(status)
                    for header in ("Content-Type", "Content-Encoding", "Retry-After"):
                        if header in upstream_headers:
                            self.send_header(header, upstream_headers[header])
                    self.end_headers()
                    self.wfile.write(data)
                except (BrokenPipeError, ConnectionResetError):
                    pass

            do_GET = do_POST = do_PATCH = do_PUT = do_DELETE = proxy

        return Handler

    def cluster_writes(self, collection, start=0):
        return [r for r in self.rows[start:] if r[0] in ("POST", "PATCH", "PUT", "DELETE")
                and (r[1] == collection or r[1].startswith(collection + "/"))]


def guide_blocks():
    guide = Path("docs/guides/import.md").read_text().split("## Recover a cluster after a lost runner\n", 1)[1]
    blocks = re.findall(r"```hcl\n(.*?)\n```", guide.split("\n## ", 1)[0], re.S)
    module, move = blocks
    require(move.startswith("moved {\n"), "The guide is missing the moved block.")
    return module, move


def module_config(plain=False):
    module, _ = guide_blocks()
    module = module.replace('terraform {', 'terraform {\n  backend "http" {}', 1)
    module = module.replace('  cluster_type      = "/v1/regions/us-texas-1/compute/cluster-types/gpu-training"',
                            '  cluster_type      = var.cluster_type')
    module = module.replace('  display_name      = "acceptance"', '  display_name      = var.display_name')
    module = re.sub(r'  node_pool = \{\n.*?\n  \}', '  node_pool = var.node_pool', module, count=1, flags=re.S)
    module = module.replace('    create = "10m"', '    create = var.create_timeout')
    module = module.replace('  timeouts {', '  on_create_timeout = "keep"\n  timeouts {')
    if plain:
        begin = module.index('data "k0rdent_kubernetes_clusters"')
        end = module.index('resource "k0rdent_kubernetes_cluster"', begin)
        module = module[:begin] + module[end:]
        module = module.replace('  for_each          = local.adoptees\n', '')
        for old, new in (("each.key", "var.cluster_id"), ("each.value.region", "var.region"),
                         ("each.value.project", "var.project")):
            module = module.replace(old, new)
    return module + '''

variable "cluster_type"   { type = string }
variable "display_name"   { type = string }
variable "node_pool"      { type = map(object({ node_count = number })) }
variable "create_timeout" { type = string }
'''


class Landscape:
    def __init__(self, mode):
        self.mode = mode
        self.region = os.environ["K0RDENT_REGION"]
        self.project = os.environ["K0RDENT_PROJECT"]
        self.upstream = os.environ["K0RDENT_ENDPOINT"].rstrip("/")
        url = urllib.parse.urlsplit(self.upstream)
        require(url.username is None and not url.query and not url.fragment and not url.path,
                "Use a base endpoint without a path, credentials, query, or fragment.")
        region_path = "/v1/regions/" + urllib.parse.quote(self.region, safe="")
        self.collection = region_path + f"/projects/{urllib.parse.quote(self.project, safe='')}/compute/clusters"
        self.cluster_type = region_path + "/compute/cluster-types/" + urllib.parse.quote(os.environ["REAL_API_CLUSTER_TYPE"], safe="")
        self.deadline = float(os.environ.get("PIPELINE_WAIT_SECONDS", "900"))
        self.prefix = os.environ.get("PIPELINE_PREFIX", "pipeline")
        require(re.fullmatch(r"[a-z][a-z0-9-]{0,35}", self.prefix), "Use a lowercase cluster id prefix of up to 36 characters.")
        self.run_id = uuid.uuid4().hex[:12]
        self.token = None
        self.token_until = 0

    def credentials(self, endpoint):
        if not self.token or time.monotonic() >= self.token_until:
            if os.environ.get("K0RDENT_CLIENT_ID"):
                body = {"grant_type": "client_credentials", "client_id": os.environ["K0RDENT_CLIENT_ID"],
                        "client_secret": os.environ["K0RDENT_CLIENT_SECRET"]}
            else:
                body = {"grant_type": "api_key", "api_key": os.environ["K0RDENT_API_KEY"]}
            status, _, data = request(endpoint + "/v1/regions/global/auth/token", "POST", json_body(body),
                                      {"Content-Type": "application/json"})
            require(status == 200, f"Token mint answered HTTP {status}.")
            result = json.loads(data)
            self.token = result["access_token"]
            self.token_until = time.monotonic() + max(0, int(result.get("expires_in", 120)) - 30)
        return {"Authorization": "Bearer " + self.token, "Content-Type": "application/json"}


class Run:
    def __init__(self, landscape, root, tf, number, log):
        self.land = landscape
        self.root = root
        self.tf = tf
        self.number = number
        self.log = log
        self.owned = set()
        self.suffix = ""
        self.proc = None
        self.step = 0
        self.fixture_creates = 0
        self.rec = Recorder(self.land.upstream, root / "api.log")

    @property
    def name(self):
        return f"{self.land.prefix}-{self.land.run_id}-{self.number}{self.suffix}"

    @property
    def path(self):
        return self.land.collection + "/" + self.name

    def record(self, message):
        print(message, file=self.log, flush=True)

    def configure(self, plain=False):
        self.case_dir = self.root / self.name
        self.case_dir.mkdir()
        self.config = self.case_dir / "main.tf"
        self.config.write_text(module_config(plain))
        self.token = uuid.uuid4().hex
        self.cleanup_ready = False
        self.backend = (self.root.parent / "backend.url").read_text().strip() + "/" + self.name
        self.env = {k: v for k, v in os.environ.items() if not k.startswith(("TF_VAR_", "TF_CLI_ARGS", "TF_HTTP_", "TF_LOG"))
                    and k not in ("TF_DATA_DIR", "TF_WORKSPACE", "TF_PLUGIN_CACHE_DIR", "TF_REATTACH_PROVIDERS")}
        self.env.update(K0RDENT_ENDPOINT=self.rec.endpoint, TF_HTTP_USERNAME="terraform",
                        TF_HTTP_PASSWORD=os.environ["TF_HTTP_PASSWORD"], TF_HTTP_ADDRESS=self.backend,
                        TF_HTTP_LOCK_ADDRESS=self.backend, TF_HTTP_UNLOCK_ADDRESS=self.backend,
                        TF_VAR_cluster_id=self.name, TF_VAR_region=self.land.region, TF_VAR_project=self.land.project,
                        TF_VAR_cluster_type=self.land.cluster_type, TF_VAR_display_name=self.name,
                        TF_VAR_create_token=self.token, TF_VAR_adopt_only="false",
                        TF_VAR_create_timeout=os.environ.get("PIPELINE_CREATE_TIMEOUT", "10m"),
                        K0RDENT_POLL_INTERVAL=os.environ.get("PIPELINE_POLL_INTERVAL", "100ms" if self.land.mode == "fake" else "500ms"))
        catalog = self.api("GET", self.land.cluster_type, expected=(200,))
        pools = {p["id"]: {"node_count": p.get("nodeCountMin") or 0} for p in catalog["nodePools"]}
        if self.land.mode == "fake":
            pools["gpu-workers"]["node_count"] = 2
        elif os.environ.get("PIPELINE_NODE_POOL"):
            pools = json.loads(os.environ["PIPELINE_NODE_POOL"])
        self.env["TF_VAR_node_pool"] = json.dumps(pools)
        self.require_gone()
        self.cleanup_ready = True
        self.cli("init", "-input=false")

    def cli(self, *args, want=0, kill=False):
        command = [self.tf, *args, "-no-color"]
        self.record("- Command: `" + shlex.join(command) + "`")
        self.step += 1
        path = self.case_dir / f"{self.step:02d}-{args[0]}.log"
        start = time.monotonic()
        with path.open("w") as out, self.rec.lock:
            self.proc = subprocess.Popen(command, cwd=self.case_dir, env=self.env, stdout=out,
                                         stderr=subprocess.STDOUT, start_new_session=True)
            if kill:
                self.rec.kill_on_create = (self.proc, self.land.collection, self.name)
        try:
            code = self.proc.wait(timeout=self.land.deadline + 60)
        except subprocess.TimeoutExpired:
            self.record(f"  - Timed out after {time.monotonic() - start:.3f} s.")
            raise
        finally:
            with self.rec.lock:
                recorder_killed_runner = self.rec.kill_on_create is None
                self.rec.kill_on_create = None
            # Terraform's provider processes belong to this command's own group.
            if self.proc.poll() is None:
                try:
                    os.killpg(self.proc.pid, signal.SIGKILL)
                except (ProcessLookupError, PermissionError):
                    self.proc.kill()
                self.proc.wait()
        display_code = 128 - code if code < 0 else code
        self.record(f"  - Exit: {display_code}. Time: {time.monotonic() - start:.3f} s.")
        if kill:
            require(recorder_killed_runner, "The recorder didn't see an accepted create before the process ended.")
            self.record("  - Recorder sent SIGKILL after the platform answered the cluster POST.")
        text = path.read_text()
        for line in text.splitlines():
            if re.match(r"(?:Plan:|No changes\.|Apply complete!|Changes to Outputs:|Error:|Warning:|\s*# (?:.* has moved to|\(moved from ))", line):
                self.record("  - `" + line.strip().replace("`", "'") + "`")
        expected = want if isinstance(want, tuple) else (want,)
        shown = "/".join(str(128 - value if value < 0 else value) for value in expected)
        require(code in expected, f"{args[0]} returned {display_code}, expected {shown}. See {path.name}.")
        return text, code

    def plan(self, label, *args, want=0):
        out, _ = self.cli("plan", "-input=false", "-out=" + label + ".tfplan", *args, want=want)
        if want != 1:
            plan = self.case_dir / (label + ".tfplan")
            # The CLI's JSON view includes the materialized plan, including variables.
            rendered, _ = self.cli("show", "-json", plan.name)
            require(self.token not in rendered, "The create token reached a saved plan.")
            return out, json.loads(rendered)
        return out, None

    def apply(self, label, **kwargs):
        return self.cli("apply", "-input=false", label + ".tfplan", **kwargs)

    def state(self):
        out, _ = self.cli("state", "pull")
        raw = out.strip()
        require(self.token not in raw, "The create token reached Terraform state.")
        return [(r, i) for r in (json.loads(raw).get("resources", []) if raw else [])
                if r["mode"] == "managed" for i in r.get("instances", [])]

    def check_state(self, uid, status="active", keyed=True):
        rows = self.state()
        require(len(rows) == 1, f"Expected one managed state instance, got {len(rows)}.")
        resource, row = rows[0]
        expect_key = self.name if keyed else None
        require(resource["type"] + "." + resource["name"] == CLUSTER and row.get("index_key") == expect_key,
                "Terraform recorded the wrong cluster address.")
        require(row.get("status") != "tainted" and row["attributes"]["uid"] == uid and row["attributes"]["state"] == status,
                f"State didn't hold the untainted {status} cluster with UID {uid}.")
        self.record(f"- State: UID `{uid}`, `{status}`, untainted.")

    def api(self, method, path, payload=None, expected=(200,)):
        headers = self.land.credentials(self.rec.endpoint)
        start = time.monotonic()
        status, _, data = request(self.rec.endpoint + path, method, None if payload is None else json_body(payload), headers)
        self.record(f"- API `{method} {path}`: HTTP {status}. Time: {time.monotonic() - start:.3f} s.")
        if method == "POST" and path == self.land.collection:
            self.fixture_creates += 1
        require(status in expected, f"{method} {path} answered HTTP {status}, expected {expected}.")
        return json.loads(data) if data and 200 <= status < 300 else None

    def read(self):
        row = self.api("GET", self.path, expected=(200, 404))
        if row is not None and row.get("state") == "deleted":
            return None
        return row

    def require_gone(self):
        require(self.read() is None, "The API still has a cluster under this id.")
        self.record("- Platform UID: absent.")

    def wait(self, state):
        start = time.monotonic()
        while time.monotonic() - start < self.land.deadline:
            row = self.read()
            if (row or {}).get("state") == state:
                self.record(f"- Platform UID: `{(row or {}).get('uid', 'absent')}`, {state or 'gone'}. Wait: {time.monotonic() - start:.3f} s.")
                return row
            require(not row or row.get("state") != "failed" or state is None, "The platform reports a failed cluster.")
            time.sleep(0.1 if self.land.mode == "fake" else 1)
        raise Failure(f"The platform didn't reach {state or 'gone'} before the wait expired.")

    def observe(self):
        row = self.read()
        require(row and row.get("uid"), "The platform didn't return a cluster UID.")
        self.record(f"- Platform UID: `{row['uid']}`, `{row['state']}`.")
        return row

    def create(self, keyed=True):
        self.plan("create")
        self.apply("create")
        uid = self.observe()["uid"]
        self.owned.add(uid)
        self.check_state(uid, keyed=keyed)
        return uid

    def seed(self, token):
        payload = {"id": self.name, "displayName": self.env["TF_VAR_display_name"],
                   "clusterType": self.env["TF_VAR_cluster_type"], "nodePools": [
                       {"id": name, "nodeCount": p["node_count"]}
                       for name, p in json.loads(self.env["TF_VAR_node_pool"]).items()]}
        if token is not None:
            payload["tags"] = {TAG: token}
        row = self.api("POST", self.land.collection, payload, (200, 201, 202)) or {}
        if row.get("uid"):
            self.owned.add(row["uid"])
        row = self.wait("active")
        self.owned.add(row["uid"])
        return row["uid"]

    def writes(self, start=0):
        return self.rec.cluster_writes(self.land.collection, start)

    def no_write(self, start):
        require(not self.writes(start), f"Terraform sent cluster writes: {self.writes(start)}.")
        self.record("- Recorder: no platform writes during plan and apply.")

    def create_count(self):
        return sum(m == "POST" and p == self.land.collection for m, p, _ in self.writes())

    def creates(self, want):
        count = self.create_count()
        require(count == want, f"Recorder saw {count} create requests, expected {want}.")

    def adopt(self):
        self.env["TF_VAR_adopt_only"] = "true"
        start = len(self.rec.rows)
        out, plan = self.plan("adopt")
        self.apply("adopt")
        self.no_write(start)
        return out, plan

    def destroy(self, expected=1):
        if not self.state():
            require(expected == 0, "The delete flow lost the managed cluster before destroy.")
            self.record("- Destroy skipped because Terraform state had no managed instances.")
            return
        require(expected != 0, "The delete flow would destroy a cluster it doesn't own.")
        _, plan = self.plan("destroy", "-destroy")
        require(actions(plan) == [["delete"]], "The destroy plan didn't delete exactly one cluster.")
        self.apply("destroy")
        require(not self.state(), "Destroy left a managed instance in Terraform state.")
        self.require_gone()

    def cleanup(self):
        row = self.read()
        if row is None:
            return
        require(row["uid"] in self.owned or (row.get("tags") or {}).get(TAG) == self.token,
                f"Cleanup refused cluster {self.name} with unrecognized UID {row['uid']}.")
        self.record(f"- Cleanup outside Terraform for fixture UID `{row['uid']}`.")
        if row["state"] == "creating":
            execution = (row.get("operation") or {}).get("executionId")
            if execution:
                self.api("POST", self.path + "/cancel", {"executionId": execution}, (200, 202, 204, 409))
        start = time.monotonic()
        while time.monotonic() - start < self.land.deadline:
            current = self.read()
            if current is None:
                return
            require(current["uid"] == row["uid"], "Cleanup stopped because the platform UID changed.")
            if current["state"] != "deleting":
                self.api("DELETE", self.path, expected=(200, 202, 204, 404, 409))
            time.sleep(0.1 if self.land.mode == "fake" else 1)
        raise Failure("Cleanup didn't remove the fixture cluster.")

    def reclaim_backend_lock(self):
        auth = base64.b64encode(("terraform:" + self.env["TF_HTTP_PASSWORD"]).encode()).decode()
        headers = {"Authorization": "Basic " + auth, "Content-Type": "application/json"}
        start = time.monotonic()
        status, _, body = request(self.backend, "LOCK", json_body({"ID": str(uuid.uuid4())}), headers)
        self.record(f"- Backend `LOCK` probe for the dead runner: HTTP {status}. Time: {time.monotonic() - start:.3f} s.")
        require(status == 423 and json.loads(body).get("ID"), "The dead runner didn't leave a held HTTP lock.")
        start = time.monotonic()
        status, _, _ = request(self.backend, "UNLOCK", body, headers)
        self.record(f"- Backend `UNLOCK` after confirming runner death: HTTP {status}. Time: {time.monotonic() - start:.3f} s.")
        require(status == 200, "The backend couldn't reclaim the dead runner's lock.")

    def verify_backend(self):
        lines = (self.root.parent / "backend.log").read_text().splitlines()
        methods = [line.split()[-2] for line in lines if line.endswith(" /" + self.name)]
        require("LOCK" in methods and "UNLOCK" in methods and "GET" in methods, "Terraform didn't use locked HTTP state.")
        self.record(f"- HTTP backend for `{self.name}`: " + ", ".join(f"{method} {methods.count(method)}" for method in ("GET", "POST", "LOCK", "UNLOCK")) + ".")


def changes(plan):
    return [r for r in plan.get("resource_changes", []) if r.get("mode") == "managed"]


def actions(plan):
    return [r["change"]["actions"] for r in changes(plan) if r["change"]["actions"] != ["no-op"]]


def timed_out_create(run, fake_display="fake-creates-slowly"):
    run.configure()
    run.env["TF_VAR_create_timeout"] = os.environ.get("PIPELINE_SHORT_TIMEOUT", "1s")
    if run.land.mode == "fake":
        run.env["TF_VAR_display_name"] = fake_display
    run.plan("timeout")
    out, _ = run.apply("timeout", want=1)
    require_wait_failure(out)
    row = run.observe()
    uid = row["uid"]
    run.owned.add(uid)
    run.check_state(uid, "creating")
    run.creates(1)
    return row


def require_wait_failure(out):
    require(POSTCONDITION in out and "is still being created" in out,
            "The apply didn't report that creation was still running and fail the active postcondition.")


def retry_until_active(run, uid):
    deadline = time.monotonic() + run.land.deadline
    attempt = 1
    while time.monotonic() < deadline:
        label = f"retry-{attempt}"
        out, plan = run.plan(label)
        if attempt == 1 or actions(plan):
            require(actions(plan) == [["update"]] and changes(plan)[0]["change"]["after_unknown"].get("state") is True,
                    "The retry didn't plan an in-place wait with unknown state.")
        else:
            require("No changes." in out, "The settled retry didn't plan cleanly.")
        out, code = run.apply(label, want=(0, 1))
        run.creates(1)
        if code == 0:
            return
        require_wait_failure(out)
        run.check_state(uid, "creating")
        require(run.observe()["uid"] == uid, "The timed-out retry changed the platform UID.")
        attempt += 1
    raise Failure("The create didn't settle within the retry window.")


def timeout_retry(run, still_running):
    row = timed_out_create(run)
    uid = row["uid"]
    if still_running:
        require(row["state"] == "creating", "The cluster already settled before the retry.")
        retry_until_active(run, uid)
    else:
        run.wait("active")
        out, plan = run.plan("retry")
        require(not actions(plan) and "No changes." in out, "The settled cluster didn't yield a no-change retry.")
        run.apply("retry")
    run.check_state(uid)
    require(run.observe()["uid"] == uid, "The retry changed the platform UID.")
    out, plan = run.plan("empty", "-detailed-exitcode")
    require(not actions(plan) and "No changes." in out, "The plan after recovery wasn't empty.")
    run.creates(1)
    run.adopt()
    run.destroy()


def lost_runner(run):
    run.configure()
    run.plan("killed")
    run.apply("killed", kill=True, want=-signal.SIGKILL)
    uid = run.observe()["uid"]
    run.owned.add(uid)
    require(not run.state(), "Terraform recorded the cluster before the kill.")
    run.reclaim_backend_lock()
    out, _ = run.plan("recover")
    require("apply adopts it" in " ".join(out.replace("│", " ").split()),
            "The recovery plan didn't describe token adoption.")
    run.apply("recover")
    after_recovery = len(run.rec.rows)
    run.check_state(uid)
    require(run.observe()["uid"] == uid, "The recovery changed the platform UID.")
    query = urllib.parse.urlencode({"id": run.name})
    clusters = run.api("GET", run.land.collection + "?" + query)["clusters"]
    matches = [row for row in clusters if row["id"] == run.name and row.get("state") != "deleted"]
    require([row["uid"] for row in matches] == [uid], "The platform didn't list exactly one cluster with the original UID.")
    statuses = [status for method, path, status in run.writes() if method == "POST" and path == run.land.collection]
    require(sum(200 <= status < 300 for status in statuses) == 1
            and all(200 <= status < 300 or status == 409 for status in statuses),
            f"Expected one accepted create and only conflicts after it, got {statuses}.")
    run.record(f"- Create response codes: {statuses}. The platform listed one cluster with the original UID.")
    run.adopt()
    run.destroy()
    require(not any(method == "POST" and path == run.land.collection for method, path, _ in run.writes(after_recovery)),
            "Terraform sent a create after recovery had adopted the cluster.")


def refused_version(run):
    run.configure()
    versions = run.api("GET", run.land.cluster_type + "/versions?status=available")["versions"]
    versions = sorted((v for v in versions if v["status"] == "available"), key=lambda v: v["version"])
    require(len(versions) >= 2, "Choose a cluster type with at least two available versions.")
    pin = run.land.cluster_type + "/versions/"
    current = pin + versions[-1]["versionId"]
    earlier = pin + versions[0]["versionId"]
    run.env["TF_VAR_cluster_type"] = current
    uid = run.create()
    require(run.observe()["clusterType"].endswith(current), "The platform didn't bind the newer version.")
    run.record(f"- Bound version: `{current}`. Configured version for teardown: `{earlier}`.")
    run.env["TF_VAR_cluster_type"] = earlier
    run.env["TF_VAR_adopt_only"] = "true"
    start = len(run.rec.rows)
    out, plan = run.plan("adopt")
    require("keeps the platform value for `cluster_type`" in " ".join(out.replace("│", " ").split()),
            "The adopt plan didn't warn that it kept the platform's cluster_type.")
    managed = changes(plan)
    require(len(managed) == 1 and managed[0]["change"]["actions"] == ["update"]
            and managed[0]["change"]["after"]["cluster_type"].endswith(current),
            "The adopt plan didn't retain the bound version in place.")
    run.apply("adopt")
    run.no_write(start)
    run.check_state(uid)
    require(run.observe()["clusterType"].endswith(current), "The adopt apply changed the bound version.")
    run.destroy()
    run.creates(1)


def reused_name(run):
    run.configure()
    original = run.create()
    run.api("DELETE", run.path, expected=(200, 202, 204))
    run.wait(None)
    replacement = run.seed(uuid.uuid4().hex)
    require(original != replacement, "The replacement didn't get a new UID.")
    run.env["TF_VAR_adopt_only"] = "true"
    start = len(run.rec.rows)
    out, _ = run.plan("adopt", want=1)
    require("different provenance" in out, "The delete flow didn't refuse the new cluster's provenance.")
    require(original in out and replacement in out, "The refresh didn't distinguish the two platform UIDs.")
    rows = run.state()
    require(not rows or len(rows) == 1 and rows[0][1]["attributes"]["uid"] == original,
            "The replacement UID reached Terraform state.")
    require(run.observe()["uid"] == replacement, "The delete flow didn't leave the replacement alone.")
    run.no_write(start)
    run.creates(2)
    run.record("- Destroy wasn't run because the adopt plan refused the replacement.")


def normal_delete(run):
    run.configure()
    uid = run.create()
    out, plan = run.adopt()
    require(actions(plan) == [["update"]] and "0 to add, 1 to change, 0 to destroy" in out
            and "adopt_only" in out, "The adopt plan didn't show only an in-place change to the managed cluster.")
    run.check_state(uid)
    run.destroy()
    run.creates(1)


def absent_delete(run):
    run.configure()
    _, plan = run.adopt()
    require(not changes(plan), "The absent cluster appeared in the adopt plan.")
    run.destroy(expected=0)
    run.require_gone()
    run.creates(0)


def wrong_provenance(run):
    for suffix, wrong in (("-missing", False), ("-wrong", True)):
        run.suffix = suffix
        run.configure()
        try:
            uid = run.seed(uuid.uuid4().hex if wrong else None)
            run.env["TF_VAR_adopt_only"] = "true"
            start = len(run.rec.rows)
            out, _ = run.plan("adopt", want=1)
            text = " ".join(out.replace("│", " ").split())
            why = "its create token differs" if wrong else "it carries no create token"
            require(why in text and "pass the original `create_token`" in text and "choose another `id`" in text,
                    "The refusal didn't name the provenance problem and the way out.")
            run.record(f"- Refusal: {why}. Pass the original `create_token` or choose another `id`.")
            require(not run.state(), "A refused plan wrote the cluster to state.")
            require(run.observe()["uid"] == uid, "The refused cluster changed UID.")
            run.no_write(start)
            run.verify_backend()
        finally:
            if run.cleanup_ready:
                run.cleanup()
    run.creates(2)


def migrate_address(run):
    run.configure(plain=True)
    uid = run.create(keyed=False)
    _, move = guide_blocks()
    run.config.write_text(module_config() + "\n" + move.replace('"ml-training"', json.dumps(run.name)) + "\n")
    run.env["TF_VAR_adopt_only"] = "true"
    start = len(run.rec.rows)
    out, plan = run.plan("adopt")
    addr = f'{CLUSTER}["{run.name}"]'
    managed = changes(plan)
    require(len(managed) == 1 and managed[0].get("previous_address") == CLUSTER
            and managed[0]["address"] == addr and managed[0]["change"]["actions"] in (["no-op"], ["update"])
            and (f"(moved from {CLUSTER})" in out or "has moved to" in out),
            "The plan didn't show a move without create, delete, or replacement.")
    run.apply("adopt")
    run.no_write(start)
    run.check_state(uid)
    require(run.observe()["uid"] == uid, "The address move changed the platform UID.")
    run.creates(1)
    run.destroy()


def delete_while_creating(run):
    row = timed_out_create(run, fake_display="fake-never-settles")
    require(row["state"] == "creating", "The cluster settled before the delete flow started.")
    start = len(run.rec.rows)
    run.adopt()
    run.check_state(row["uid"], "creating")
    require(run.observe()["state"] == "creating", "The cluster settled before the destroy plan.")
    require(not any(path == run.path + "/runtime/events" for _, path, _ in run.rec.rows[start:]),
            "The adopt apply polled the create operation.")
    run.destroy()
    run.creates(1)


SCENARIOS = (lambda r: timeout_retry(r, True), lambda r: timeout_retry(r, False), lost_runner,
             refused_version, reused_name, normal_delete, absent_delete, wrong_provenance, migrate_address,
             delete_while_creating)


def run_case(land, work, tf, number, log):
    root = work / f"case-{number}"
    root.mkdir()
    run = Run(land, root, tf, number, log)
    run.record(f"\n## {number}. {CASES[number]}\n")
    with run.rec:
        try:
            SCENARIOS[number - 1](run)
            if number != 8:
                run.verify_backend()
        except Exception as error:
            if getattr(run, "cleanup_ready", False) and number != 8:
                try:
                    run.cleanup()
                except Exception as cleanup_error:
                    raise Failure(f"{error} Cleanup also failed: {cleanup_error}") from error
            raise
        finally:
            count = run.create_count()
            run.record(f"- Cluster create requests: {count} total, {run.fixture_creates} from fixture setup.")
        if getattr(run, "cleanup_ready", False) and number != 8:
            run.cleanup()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("mode", choices=("fake", "live"))
    parser.add_argument("work", type=Path)
    parser.add_argument("terraform")
    choices = f"1 to {max(CASES)}"
    parser.add_argument("cases", type=int, metavar="CASE", nargs="*", help=f"case numbers {choices}, default all")
    args = parser.parse_args()
    if set(args.cases) - CASES.keys() or len(args.cases) != len(set(args.cases)):
        parser.error(f"choose distinct case numbers from {choices}")
    land = Landscape(args.mode)
    failed = False
    report = args.work / "report.md"
    print(f"Evidence: {report}", flush=True)
    with report.open("w") as log:
        print(f"# Pipeline evidence\n\nHashiCorp Terraform 1.15.5. Target: {args.mode}.", file=log, flush=True)
        for number in args.cases or CASES:
            try:
                run_case(land, args.work, args.terraform, number, log)
            except (Failure, OSError, subprocess.TimeoutExpired, ValueError, KeyError) as error:
                failed = True
                print(f"- Result: FAIL. {error}", file=log, flush=True)
                print(f"Case {number}: FAIL. {error}", flush=True)
                if args.mode == "live":
                    break
            else:
                print("- Result: PASS.", file=log, flush=True)
                print(f"Case {number}: PASS", flush=True)
    return int(failed)


if __name__ == "__main__":
    raise SystemExit(main())
