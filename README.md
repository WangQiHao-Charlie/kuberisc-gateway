KubeRISC Gateway (MVP)

Thin HTTP gateway that turns POST /v1/functions/:name/invoke into a single NodeAction execution and streams back the result when it completes.

MVP behavior

- Route: POST /v1/functions/:name/invoke?timeout=5s&sync=true
- Body: arbitrary text/JSON, stored as param `payload`
- Response:
  - 200 with stdout tail when NodeAction Succeeded
  - 504 when agent/driver timed out
  - 502 for driver errors or non-zero exit code
- Idempotency: if `X-Request-ID` is sent, the gateway reuses any finished NodeAction created earlier with the same ID. New executions use `executionID=X-Request-ID` to leverage driver-side caching.

Parameters mapping

- InstructionRef: `{ name: $INSTRUCTIONSET_NAME, instruction: $INSTRUCTION_NAME, runtime: $RUNTIME }`
- Params:
  - `module`: from query `module` or header `X-Module` (required for MVP)
  - `entrypoint`: from query `entrypoint` or header `X-Entrypoint` (optional)
  - `payload`: raw request body as string
  - `timeout`: same as query `timeout`, e.g. `5s`
 - Spec:
  - `timeoutSeconds`: derived from `timeout` (integer seconds)
  - `executionID`: `X-Request-ID` or `gw-<ts>-rand`
  - `nodeName`: chosen from a ready agent Pod

Node selection (MVP)

- Lists Pods by label selector `AGENT_POD_SELECTOR` (default `app=kuberisc-agent`), picks a Ready Pod and binds execution to its node via `spec.nodeName`.
- Adds label `risc.dev/node=<nodeName>` on the NodeAction for agents using server-side label filtering (`NA_NODE_LABEL`).

Build & run

- Binary listens on `:8080` by default. Key env vars:
  - `GATEWAY_NAMESPACE` — namespace to create NodeActions (default `default`)
  - `INSTRUCTIONSET_NAME` — default `wasmtime`
  - `INSTRUCTION_NAME` — default `run`
  - `RUNTIME` — `exec` (with execTemplate) or `grpc`
  - `AGENT_POD_SELECTOR` — default `app=kuberisc-agent`
  - `WATCH_GRACE` — extra watch grace (default `500ms`)

Kubernetes deploy

Apply RBAC, Deployment and Service from `gateway/deploy/`.

Container image (GHCR)

- 已添加 GitHub Actions 工作流 `.github/workflows/gateway-image.yml`。
- Push 到 `main/master` 或提交 PR 会构建镜像；非 PR 会推送到 GHCR。
- 镜像名：`ghcr.io/<OWNER>/kuberisc-gateway`，标签包含：分支名、tag、`sha` 以及默认分支的 `latest`。
- 首次使用需确保仓库 `Settings → Packages → Read and write permissions` 已开启，或使用默认 `GITHUB_TOKEN`。

Example invocation

curl -X POST \
  -H 'X-Request-ID: abc-123' \
  --data-binary @- \
  'http://kuberisc-gateway.default.svc.cluster.local/v1/functions/hello/invoke?module=/opt/wasm/hello.wasm&timeout=5s&sync=true' <<'EOF'
{"name":"world"}
EOF

Notes

- For exec runtime with Wasmtime, ensure your Runtime Driver allowlist includes `/usr/bin/wasmtime`, and the module path exists in the Driver container.
- If you already expose a richer InstructionSet (e.g. `runtime: grpc` + `invoke`), set `INSTRUCTIONSET_NAME/INSTRUCTION_NAME/RUNTIME` accordingly and adjust params consumed by your driver.
- This MVP intentionally avoids introducing an Invocation CRD; it targets a single NodeAction for simplicity and stability during the hackathon.
