# UAT Implementation Plan

## Foundation tasks (must come first)

**T1. Scaffold `cmd/uat/main.go` skeleton**
- Create `cmd/uat/` directory and empty `main.go` with package, basic CLI flag parsing (`--base-url`, `--start-command`, `--timeout`, `--verbose`, `--skip-destructive`, `--render-url`), and exit-code handling (0/1/2).
- No checks yet; just wiring + help output.
- Dependencies: none.

**T2. Internal runner + HTTP client helpers**
- Define `Check`, `Env` types per UAT spec.
- Implement a small runner that executes a slice of `Check`s, prints `PASS`/`FAIL`, and produces the summary line.
- Add HTTP request/assert helpers (status, body-contains, body-equals, content-type prefix) with method+path in failure messages.
- Generate a per-run `RunID` (e.g., timestamp + random).
- Dependencies: T1.

## Mode infrastructure (parallel after T2)

**T3. Existing-service mode wiring**
- When `--start-command` is empty, just probe `--base-url` for readiness (poll `GET /` until 200 or timeout), then run the selected suite.
- Dependencies: T2.

**T4. Self-managed service supervisor**
- Create temp dir, set `DB_PATH` env var, launch subprocess from `--start-command`, wait for HTTP readiness, stop subprocess (SIGTERM then kill), clean up temp dir.
- Provide hooks to inject additional env vars (e.g., `RENDER_SERVICE_URL`) per check group.
- Dependencies: T2.

**T5. Fake render service helper**
- An `httptest.Server`-style helper inside the UAT command that serves `GET /render?text=...`, records received `text`, and returns configurable status + body (PNG fixture bytes for success case, 503 for failure case).
- Include a small embedded PNG fixture (e.g., 1x1 PNG byte slice).
- Dependencies: T2.

**T6. Suite selection / tagging**
- Tag each check as `destructive` (needs isolation), `non-destructive`, or `render-required`.
- Build the appropriate check list based on mode + `--skip-destructive`.
- Dependencies: T2 (consumed by all check tasks).

## Check implementations (all parallel after T2 + T6)

These touch independent functions and can be parallelized across agents:

**T7. Landing page check** (`GET /`)
**T8. Empty POST rejected check**
**T9. Whitespace-only POST rejected check**
**T10. Valid POST accepted check**
**T11. Unsupported methods checks** (`PUT`/`DELETE /motivation`, `POST /motivations.png`)
**T12. Unknown route check**

These need the isolated mode helpers (depend on T4 and/or T5):

**T13. Empty motivation collection check** — depends on T4
**T14. Submitted-motivation-retrievable check** — isolated-equal vs existing-eventual variants; depends on T4 for isolated path
**T15. Trimmed submission check** — depends on T4
**T16. Multiple motivations retrievable check** — depends on T4
**T17. Repeated GET availability check** — depends on T4 (reuses T16 setup)
**T18. PNG render success check** — depends on T4 + T5
**T19. PNG with no motivations check** — depends on T4
**T20. PNG render service unreachable check** — depends on T4
**T21. PNG render service non-OK check** — depends on T4 + T5

## Integration + polish (sequential, last)

**T22. Wire all checks into the runner per mode**
- Existing-service suite: T7, T8, T9, T10 (optional), T11, T12, T14 (eventual variant).
- Isolated self-managed suite: all checks.
- Dependencies: T3, T4, T5, T6, T7–T21.

**T23. Documentation & CI integration**
- Add a `## UAT` section to `README.md` with usage examples.
- Update `CLAUDE.md` project structure section to mention `cmd/uat`.
- Optional: add a `make uat` / shell snippet for CI.
- Dependencies: T22.

**T24. End-to-end verification**
- Run UAT in both modes locally, fix any flakiness (readiness polling, port selection for fake render), confirm exit codes.
- Dependencies: T22.

## Dependency graph

```diagram
            ╭────╮
            │ T1 │
            ╰─┬──╯
              ▼
            ╭────╮
            │ T2 │
            ╰─┬──╯
   ╭──────────┼──────────┬─────────┬──────────╮
   ▼          ▼          ▼         ▼          ▼
 ╭────╮     ╭────╮     ╭────╮   ╭────╮   ╭──────────╮
 │ T3 │     │ T4 │     │ T5 │   │ T6 │   │ T7–T12   │ (parallel,
 ╰─┬──╯     ╰─┬──╯     ╰─┬──╯   ╰─┬──╯   ╰────┬─────╯  no infra)
   │          │          │        │           │
   │   ╭──────┴──────────┴─────╮  │           │
   │   ▼                       ▼  │           │
   │ ╭────────────────────╮ ╭──────────────╮  │
   │ │ T13–T17, T19, T20  │ │ T18, T21     │  │
   │ ╰─────────┬──────────╯ ╰──────┬───────╯  │
   │           │                   │          │
   ╰───────────┴────────┬──────────┴──────────╯
                        ▼
                     ╭─────╮
                     │ T22 │
                     ╰──┬──╯
                ╭───────┴───────╮
                ▼               ▼
              ╭─────╮         ╭─────╮
              │ T23 │         │ T24 │
              ╰─────╯         ╰─────╯
```

## Parallelization summary

- **Wave 1 (serial):** T1 → T2.
- **Wave 2 (fully parallel):** T3, T4, T5, T6, T7, T8, T9, T10, T11, T12.
- **Wave 3 (parallel, after T4 ± T5):** T13, T14, T15, T16, T17, T18, T19, T20, T21.
- **Wave 4 (serial):** T22 → (T23 ∥ T24).

This gives ~24 small, mostly-isolated beads where the bulk of work (Wave 2 + Wave 3 = 15 tasks) can be fanned out concurrently.
