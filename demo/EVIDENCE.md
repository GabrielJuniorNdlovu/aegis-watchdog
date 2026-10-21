# Run the evidence demonstration

The demo captures synthetic logs from one isolated BusyBox container. Its action
reservation is simulated in a private database. It does not use your operational
`data/aegis.db`, restart the container, or collect another container's logs.

From the repository root, with Go, CGO, a C compiler, and Docker Compose installed:

```sh
bash scripts/smoke-evidence.sh
```

Expect a complete capture, redacted credential values, unchanged action and budget
facts, and read-only action inspection with an invalid Docker host. The script
checks that the container restart count stays unchanged. Its exit trap removes
only its Compose project and temporary data, including on failure or Ctrl-C.
The shared BusyBox image remains cached.

## Keep the bundle for inspection

Use these commands for an interactive demonstration. Keep this shell open so its
private project name and directory remain available for cleanup.

```sh
mkdir -p data
incident_demo_dir=$(mktemp -d "$PWD/data/evidence-demo.XXXXXXXX")
incident_demo_project="aegis-$(basename "$incident_demo_dir" | tr '[:upper:].' '[:lower:]-')"
incident_demo_compose=(docker compose -f demo/compose.yaml -f demo/evidence.compose.yaml -p "$incident_demo_project")
export DOCKER_HOST="$(docker context inspect --format '{{.Endpoints.docker.Host}}')"
unset DOCKER_CONTEXT
"${incident_demo_compose[@]}" up -d --wait --wait-timeout 60 healthy
incident_demo_id=$("${incident_demo_compose[@]}" ps -q healthy)
mkdir -m 700 "$incident_demo_dir/state"
AEGIS_EVIDENCE_DEMO_CONTAINER="$incident_demo_id" AEGIS_EVIDENCE_DEMO_DIR="$incident_demo_dir/state" \
  go test -count=1 -v ./internal/evidence -run '^TestLiveEvidenceDemo$'
CGO_ENABLED=1 go build -o bin/aegis ./cmd/aegis
DOCKER_HOST=invalid-Docker-host ./bin/aegis actions --id 1 --db "$incident_demo_dir/state/simulated-actions.db"
```

The CLI reports status, capture times, file location, size, and limitations without
printing logs. To view only the synthetic redacted logs in this demo, use Python:

```sh
python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["Logs"], end="")' \
  "$incident_demo_dir/state/evidence/capture-1.json"
```

Clean up the project and the private directory created above:

```sh
"${incident_demo_compose[@]}" down --timeout 5
rm -rf -- "${incident_demo_dir:?}"
```

The opt-in test refuses a nonempty demo directory. To run it again, create a new
private directory. Production capture permits one journal record per action and
never silently overwrites a bundle or retries a failed capture.
