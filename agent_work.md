# Agent Session Trigger Playbook (`agent_work.md`)

> Copy and paste one of the prompts below into a fresh agent session at any time to pick up Trimtab development cleanly from the current checkpoint without re-explaining project history, architecture, or safety invariants.

---

## Prompt 1 — Resume Next Planned Sub-Phase *(Default Copy-Paste Prompt)*

```text
You are resuming engineering work on Trimtab (`trimtab.io`), an open-source, vendor-neutral adaptive telemetry control plane written in Go.

1. Read these files first (in parallel) to orient yourself completely:
   - AGENT.md
   - docs/CHECKPOINT.md
   - docs/PLAN.md
   - docs/DESIGN.md
   - docs/BACKENDS_AND_AUTH.md

2. Identify the first unchecked `[ ]` sub-phase in the Master Phase Tracker inside `docs/PLAN.md` (currently starting from P2.4–P2.7, then Phase 3).

3. Verify the existing green baseline before making changes:
   go test -count=1 -race ./... && make verify

4. Review the target sub-phase specification in `docs/PLAN.md`:
   - If there is any genuine ambiguity or design choice not settled in `docs/PLAN.md`, ask concise clarifying questions before editing code.
   - Otherwise, implement the entire target sub-phase cleanly in idiomatic Go with minimal third-party dependencies.
   - Strictly preserve every Non-Negotiable Safety Invariant in `AGENT.md` and keep all code/docs vendor-neutral.
   - Never create separate per-phase plan files (`PHASE2_PLAN.md`, `PHASE3_PLAN.md`, etc.) — refine specifications in-place inside `docs/PLAN.md` only.
   - Add thorough, deterministic offline tests (`httptest.NewServer` / fixtures), golden tests (if synthesizer output changes), and Safety Test Matrix regression coverage (`pkg/engine/safety_matrix_test.go`).

5. Verify everything end-to-end after your final edit:
   go test -count=1 -race ./... && make verify && make demo && make demo-otel && make demo-promrw

6. Update handoff docs before finishing:
   - Mark the completed sub-phase `[x]` in `docs/PLAN.md`.
   - Update `docs/CHECKPOINT.md` (current status, completed milestones, safety matrix status, implemented package map, and next build target).
   - Update `AGENT.md` / `README.md` if CLI flags, config schema, or targets changed.

7. Reply concisely with: (a) what was implemented, (b) exact verification commands run and their observed results, and (c) the next unchecked sub-phase ready for the following session.
```

---

## Prompt 2 — Execute a Specific Sub-Phase (`<SUB_PHASE_ID>`)

Replace `<SUB_PHASE_ID>` (for example `P2.4`, `P2.5`, `P2.6`, `P2.7`) before pasting:

```text
Read AGENT.md, docs/CHECKPOINT.md, docs/PLAN.md, docs/DESIGN.md, and docs/BACKENDS_AND_AUTH.md in parallel.

Verify the current baseline with `go test -count=1 -race ./... && make verify`.

Then implement sub-phase `<SUB_PHASE_ID>` strictly following its specification in `docs/PLAN.md`, all Non-Negotiable Safety Invariants in `AGENT.md`, and the connectivity/auth rules in `docs/BACKENDS_AND_AUTH.md` (keeping everything vendor-neutral and offline-testable with `httptest` / fixtures). If `<SUB_PHASE_ID>` depends on an earlier unchecked sub-phase in `docs/PLAN.md`, tell me first before starting.

After implementation:
- Run `go test -count=1 -race ./... && make verify && make demo && make demo-otel && make demo-promrw`.
- Mark `<SUB_PHASE_ID>` `[x]` in `docs/PLAN.md` and update `docs/CHECKPOINT.md` (plus `AGENT.md` / `README.md` if CLI/config changed).
- Report only directly observed file paths, symbols, and command results, plus the next sub-phase.
```

---

## Prompt 3 — Plan-Only Spec Refinement (`/plan`)

Use when you want the agent to inspect the codebase and tighten a sub-phase in `docs/PLAN.md` **without writing Go code yet**:

```text
/plan Read AGENT.md, docs/CHECKPOINT.md, docs/PLAN.md, docs/DESIGN.md, and docs/BACKENDS_AND_AUTH.md, plus the relevant Go packages for `<SUB_PHASE_ID>`.

Do NOT modify Go code yet. Review `<SUB_PHASE_ID>` in `docs/PLAN.md` against the current codebase and safety invariants, point out any gaps, edge cases, or pushback, and propose exact updates to `docs/PLAN.md` (keeping all plans inside `docs/PLAN.md` and vendor-neutral). Wait for my approval before modifying files.
```

---

## Prompt 4 — Safety & Release Audit Pass

Use before cutting a milestone or after completing multiple sub-phases:

```text
Read AGENT.md, docs/CHECKPOINT.md, docs/PLAN.md, and docs/BACKENDS_AND_AUTH.md.

Run a thorough read-only audit of the codebase against:
1. Every Non-Negotiable Safety Invariant in `AGENT.md` and `pkg/engine/safety_matrix_test.go`.
2. Backend read-only, fail-closed auth, secret-redaction, and self-query exclusion rules in `docs/BACKENDS_AND_AUTH.md`.
3. `go test -count=1 -race ./...`, `make verify`, `golangci-lint run`, and all `make demo*` targets.

Report any safety, concurrency, pagination, rotation, or doc-drift gaps with exact file and line citations before changing any code.
```

---

## Mandatory Session Handoff Checklist (For Agents)

Before concluding any implementation session, confirm:
- [ ] `go test -count=1 -race ./...` passed after the final edit.
- [ ] `make verify` and applicable `make demo*` targets passed after the final edit (with no untracked/dirty artifacts left behind).
- [ ] Completed sub-phase marked `[x]` in [docs/PLAN.md](docs/PLAN.md); no extra per-phase plan files created.
- [ ] [docs/CHECKPOINT.md](docs/CHECKPOINT.md) reflects exact status, package inventory, safety matrix counts, and next build target.
- [ ] [AGENT.md](AGENT.md) and [README.md](README.md) remain accurate if CLI commands, config keys, or synthesizers changed.
