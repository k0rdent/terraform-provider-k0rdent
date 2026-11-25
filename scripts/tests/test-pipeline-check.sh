#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../.."
work=$(mktemp -d "${TMPDIR:-/tmp}/pipeline-test.XXXXXX")
trap 'rm -f "$work/terraform"; rmdir "$work"' EXIT
cat > "$work/terraform" <<'EOF'
#!/usr/bin/env bash
printf 'Terraform v1.17.2\n'
EOF
chmod +x "$work/terraform"

refuses() {
  local want=$1 out
  shift
  if out=$(env "$@" ./scripts/pipeline-check.sh 2>&1); then
    printf 'Expected refusal: %s\n' "$want" >&2
    exit 1
  fi
  grep -F "$want" <<< "$out"
}
refuses 'Use HashiCorp Terraform 1.15.5' PIPELINE_MODE=fake TF_BIN="$work/terraform"
refuses 'Set PIPELINE_CONFIRM' PIPELINE_MODE=live K0RDENT_ENDPOINT=http://127.0.0.1:1 PIPELINE_CONFIRM=
refuses 'Set an HTTPS or loopback K0RDENT_ENDPOINT' PIPELINE_MODE=live K0RDENT_ENDPOINT=http://example.com

PYTHONPATH=scripts/lib python3 -B - "$work" <<'PY'
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
import signal
import subprocess
import sys
import threading

from pipeline_check import Recorder, json_body, module_config, request


class API(BaseHTTPRequestHandler):
    def do_POST(self):
        assert self.headers["Authorization"] == "Bearer fixture"
        self.rfile.read(int(self.headers["Content-Length"]))
        self.send_response(201)
        self.end_headers()
        self.wfile.write(b'{"uid":"original"}')

    def log_message(self, *_):
        pass


with ThreadingHTTPServer(("127.0.0.1", 0), API) as api:
    thread = threading.Thread(target=api.serve_forever, daemon=True)
    thread.start()
    log = Path(sys.argv[1]) / "api.log"
    child = subprocess.Popen(["sleep", "20"])
    try:
        with Recorder(f"http://127.0.0.1:{api.server_port}", log) as rec:
            path = "/v1/clusters"
            headers = {"Authorization": "Bearer fixture"}
            rec.kill_on_create = (child, path, "mine")
            assert request(rec.endpoint + path, "POST", json_body({"id": "other"}), headers)[0] == 201
            assert child.poll() is None, "the recorder killed the runner for an unrelated cluster"
            status, _, body = request(rec.endpoint + path, "POST", json_body({"id": "mine"}), headers)
            assert (status, body) == (201, b'{"uid":"original"}')
            assert child.wait(timeout=2) == -signal.SIGKILL, "the answered create didn't kill the runner"
            assert rec.cluster_writes(path) == [("POST", path, 201), ("POST", path, 201)]
            assert "fixture" not in log.read_text() and "original" not in log.read_text()
    finally:
        if child.poll() is None:
            child.kill()
            child.wait()
        api.shutdown()
        thread.join()
        log.unlink(missing_ok=True)

plain, keyed = module_config(plain=True), module_config()
assert 'backend "http" {}' in plain and 'backend "http" {}' in keyed
assert 'condition     = var.adopt_only || self.state == "active"' in plain
assert 'condition     = var.adopt_only || self.state == "active"' in keyed and 'ephemeral = true' in keyed
assert 'for_each' not in plain and 'import {' not in plain
assert 'for_each          = local.adoptees' in keyed and 'import {' in keyed
PY
printf 'Pipeline script checks passed.\n'
