# UAT Suite Plan

This document describes a plan for a user acceptance testing (UAT) command for the Random Motivation API. The UAT suite should treat the service as a black box: it should use only the public HTTP API and process-level controls, never import application packages, query SQLite directly, or call handlers in-process.

The proposed command location is:

```text
cmd/
  uat/
    main.go
```

## Goals

- Verify externally observable service behavior.
- Support deterministic local runs against an isolated temporary database.
- Support smoke-style runs against an already-running local or deployed service.
- Produce concise PASS/FAIL output suitable for CI logs.
- Exit non-zero when any behavioral check fails.

## Proposed command modes

### Existing service mode

Run the UAT suite against a service that is already running:

```bash
go run ./cmd/uat --base-url http://localhost:8080
```

This mode should avoid checks that require a fresh database or controlled render service unless explicitly configured.

### Self-managed local service mode

Start the service as a subprocess, configure it with isolated test resources, run the full UAT suite, and shut it down:

```bash
go run ./cmd/uat \
  --start-command "go run ." \
  --base-url http://localhost:8080 \
  --timeout 30s
```

In this mode, the UAT command should:

1. Create a temporary directory.
2. Set `DB_PATH` to a database inside that directory.
3. Start the service subprocess.
4. Wait until the service is ready over HTTP.
5. Run the suite.
6. Stop the service subprocess.
7. Delete temporary files.

## Proposed CLI flags

```text
--base-url string
    Base URL of the motivation service.
    Default: http://localhost:8080

--start-command string
    Optional command used to start the service under test.

--timeout duration
    Overall timeout for the UAT run.
    Default: 30s

--verbose
    Print every request and response assertion.

--skip-destructive
    Skip checks that assume an empty or isolated database.

--render-url string
    Optional explicit render service URL.
    Mainly useful when testing an already-running service.
```

Recommended exit codes:

- `0`: all checks passed
- `1`: one or more behavioral checks failed
- `2`: invalid CLI usage or configuration

## Behaviors to verify

### Service availability and landing page

`GET /` should return a successful response describing the API.

Expected result:

- Status: `200 OK`
- Body includes:
  - `Welcome to the Random Motivation API`
  - `GET /motivation`
  - `POST /motivation`
  - `GET /motivations.png`

### Empty motivation collection

When no motivations exist, `GET /motivation` should report that none exist.

Expected result:

- Status: `404 Not Found`
- Body: `No motivations found`

This check should run only in isolated self-managed mode.

### Empty motivation submissions are rejected

`POST /motivation` with an empty body should be rejected.

Expected result:

- Status: `400 Bad Request`
- Body: `Motivation cannot be empty`

### Whitespace-only motivation submissions are rejected

`POST /motivation` with only spaces or newlines should be rejected after trimming.

Expected result:

- Status: `400 Bad Request`
- Body: `Motivation cannot be empty`

### Valid motivation submissions are accepted

`POST /motivation` with non-empty text should add a motivation.

Expected result:

- Status: `201 Created`
- Body: `Motivation added successfully`

### Submitted motivation text is trimmed

The service should trim leading and trailing whitespace before storing and serving submitted text.

Expected result:

- `POST /motivation` with `   Stay focused.   ` returns `201 Created`.
- A later `GET /motivation` returns `Stay focused.` without surrounding whitespace.

This check is deterministic only in isolated self-managed mode.

### Submitted motivations are retrievable

A motivation added through `POST /motivation` should become retrievable through `GET /motivation`.

Expected result:

- POST returns `201 Created`.
- GET returns `200 OK`.
- In isolated mode, the body equals the submitted motivation text.
- In existing-service mode, the body should eventually include the submitted unique text within a bounded number of GET attempts.

### Multiple submitted motivations are retrievable

Multiple motivations added through the public API should all be available through repeated `GET /motivation` calls.

Expected result in isolated mode:

- Submit three unique motivations.
- Call `GET /motivation` up to six times.
- Every response is `200 OK`.
- Every response body is one of the submitted motivations.
- All submitted motivations are observed at least once.

### Motivation retrieval remains available after repeated reads

After motivations have been added, repeated reads should continue returning valid motivations instead of failing.

Expected result:

- More GET requests than the number of submitted motivations all return `200 OK`.
- No `404 Not Found` occurs after motivations have been added.
- Every response body is a known motivation in isolated mode.

### PNG rendering succeeds

`GET /motivations.png` should return a PNG image for a motivation.

Expected result with a controlled fake render service:

- Status: `200 OK`
- `Content-Type` starts with `image/png`
- Response bytes equal the fake render service's PNG fixture bytes.
- The fake render service receives the motivation text in the `text` query parameter.

This check should use a fake render service in self-managed mode by setting `RENDER_SERVICE_URL` for the service subprocess.

### PNG rendering with no motivations

When no motivations exist, `GET /motivations.png` should report that none exist.

Expected result:

- Status: `404 Not Found`
- Body: `No motivations found`

This check should run only in isolated self-managed mode before adding motivations.

### PNG rendering fails when render service is unavailable

If the configured render service cannot be reached, `GET /motivations.png` should return an internal error.

Expected result:

- Status: `500 Internal Server Error`
- Body: `Error rendering motivation image`

### PNG rendering fails when render service returns non-OK

If the render service returns a non-`200` response, `GET /motivations.png` should return an internal error.

Expected result:

- Status: `500 Internal Server Error`
- Body: `Error rendering motivation image`

### Unsupported methods are rejected

Unsupported HTTP methods on known endpoints should be rejected.

Expected examples:

- `PUT /motivation` returns `405 Method Not Allowed`.
- `DELETE /motivation` returns `405 Method Not Allowed`.
- `POST /motivations.png` returns `405 Method Not Allowed`.

### Unknown routes are not found

Unknown paths should return not found.

Expected result:

- `GET /uat-route-that-should-not-exist-<run-id>` returns `404 Not Found`.

## Test steps by behavior

### Landing page

1. Send `GET /`.
2. Assert status `200`.
3. Read the response body.
4. Assert the body includes the welcome text and endpoint descriptions.

### Empty motivation collection

1. Start the service with a fresh temporary `DB_PATH`.
2. Send `GET /motivation`.
3. Assert status `404`.
4. Assert the body contains `No motivations found`.

### Empty POST rejected

1. Send `POST /motivation` with an empty body.
2. Assert status `400`.
3. Assert the body contains `Motivation cannot be empty`.

### Whitespace-only POST rejected

1. Send `POST /motivation` with a body containing only spaces and newlines.
2. Assert status `400`.
3. Assert the body contains `Motivation cannot be empty`.

### Valid POST accepted

1. Generate a unique motivation, such as `UAT valid motivation <run-id>`.
2. Send `POST /motivation` with that body.
3. Assert status `201`.
4. Assert the body contains `Motivation added successfully`.

### Submitted motivation can be retrieved

1. Generate a unique motivation, such as `UAT retrievable motivation <run-id>`.
2. Send `POST /motivation` with that body.
3. Assert status `201`.
4. Send `GET /motivation`.
5. Assert status `200`.
6. In isolated mode, assert the response body equals the submitted text.
7. In existing-service mode, retry GET a bounded number of times and pass if the submitted text appears.

### Submitted motivation is trimmed

1. Send `POST /motivation` with a value like `   UAT trimmed motivation <run-id>   `.
2. Assert status `201`.
3. Send `GET /motivation`.
4. Assert status `200`.
5. Assert the body equals `UAT trimmed motivation <run-id>`.
6. Assert the body does not include the original surrounding whitespace.

### Multiple motivations are returned

1. Submit three unique motivations:
   - `UAT quote A <run-id>`
   - `UAT quote B <run-id>`
   - `UAT quote C <run-id>`
2. Call `GET /motivation` up to six times.
3. Assert every response is `200`.
4. Assert every body is one of the submitted values.
5. Assert all three values are observed at least once.

### Repeated GET remains available

1. Reuse the submitted motivations from the multiple-motivation check.
2. Continue calling `GET /motivation` several more times.
3. Assert every response is `200`.
4. Assert no response is `404`.
5. Assert each body is a known submitted motivation in isolated mode.

### PNG render success

1. Start a fake render HTTP server inside the UAT command.
2. Configure the motivation service with `RENDER_SERVICE_URL=<fake-render-url>`.
3. The fake render server should handle `GET /render?text=<value>`.
4. The fake render server should record the received `text` query parameter.
5. The fake render server should return:
   - Status `200`
   - Header `Content-Type: image/png`
   - Fixed PNG fixture bytes
6. Submit a known motivation through `POST /motivation`.
7. Send `GET /motivations.png`.
8. Assert status `200`.
9. Assert response `Content-Type` starts with `image/png`.
10. Assert response bytes equal the fake PNG bytes.
11. Assert the fake render server received the expected motivation text.

### PNG with no motivations

1. Start the service with a fresh temporary `DB_PATH`.
2. Send `GET /motivations.png` before adding any motivations.
3. Assert status `404`.
4. Assert the body contains `No motivations found`.

### PNG render service unreachable

1. Start the service with `RENDER_SERVICE_URL` pointing at an unused local port.
2. Submit a valid motivation through `POST /motivation`.
3. Send `GET /motivations.png`.
4. Assert status `500`.
5. Assert the body contains `Error rendering motivation image`.

### PNG render service returns non-OK

1. Start a fake render service that returns `503 Service Unavailable`.
2. Start the motivation service with `RENDER_SERVICE_URL` pointing at the fake render service.
3. Submit a valid motivation through `POST /motivation`.
4. Send `GET /motivations.png`.
5. Assert status `500`.
6. Assert the body contains `Error rendering motivation image`.

### Unsupported methods

1. Send `PUT /motivation` and assert status `405`.
2. Send `DELETE /motivation` and assert status `405`.
3. Send `POST /motivations.png` and assert status `405`.

### Unknown route

1. Generate a unique path, such as `/uat-route-that-should-not-exist-<run-id>`.
2. Send `GET` to that path.
3. Assert status `404`.

## Suggested internal runner design

The UAT command can use a small internal runner instead of the Go `testing` package directly.

```go
type Check struct {
    Name string
    Run  func(ctx context.Context, client *http.Client, env Env) error
}

type Env struct {
    BaseURL string
    RunID   string
}
```

Each check should:

- Make requests only through HTTP.
- Assert status codes and response bodies.
- Return clear errors.
- Include the request method and path in failure messages.

Example failure output:

```text
FAIL post valid motivation: POST /motivation returned status 500, want 201; body="..."
```

Example success output:

```text
PASS landing page
PASS empty GET /motivation
PASS reject empty POST /motivation
PASS accept valid POST /motivation
PASS retrieve submitted motivation
PASS render PNG success
```

Example summary output:

```text
UAT passed: 12 checks
```

or:

```text
UAT failed: 2 of 12 checks failed
```

## How to run the UAT suite

### Against a running local service

Start the service in one terminal:

```bash
DB_PATH="$(mktemp -u /tmp/random-motivation-uat-XXXXXX.db)" go run .
```

Run the UAT command in another terminal:

```bash
go run ./cmd/uat --base-url http://localhost:8080
```

This mode is simple, but it depends on the manually started service and whatever database it uses.

### Recommended isolated local run

After `/cmd/uat` supports self-managed mode, run:

```bash
go run ./cmd/uat \
  --start-command "go run ." \
  --base-url http://localhost:8080 \
  --timeout 30s
```

This should run the full deterministic suite against a temporary database and controlled render service.

### Against a deployed service

Run a non-destructive subset:

```bash
go run ./cmd/uat \
  --base-url https://your-service.example.com \
  --skip-destructive
```

Recommended deployed checks:

- Landing page
- Empty POST rejected
- Whitespace-only POST rejected
- Valid POST accepted, if safe for that environment
- Submitted motivation eventually retrievable
- Unknown route returns `404`
- Unsupported methods return `405`

## Implementation guardrails

- Do not import `github.com/codegoalie/random-motivation/db`.
- Do not query or mutate SQLite directly.
- Do not call handlers in-process.
- Use only HTTP requests to public service endpoints.
- Use unique run IDs in submitted text to avoid collisions.
- Keep assertions focused on external behavior, not internal queue implementation.
- Prefer isolated self-managed mode for deterministic full-suite runs.
- Print concise PASS/FAIL output suitable for CI logs.
- Return meaningful process exit codes so the command can be used in CI/CD.
