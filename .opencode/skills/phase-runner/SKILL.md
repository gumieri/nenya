---
name: phase-runner
description: Use ONLY when the user says "run phases", "execute the plan", "run all phases", "start phase review", or "run through all phases". Executes nenya deep code review phases 010-022 non-stop with code-reviewer loop, full MCP toolchain, until clean.
---

# Phase Runner

Executes the Nenya Deep Code Review phases (010–022) with zero tolerance for skipped fixes. Runs non-stop with zero pauses — every MCP service is used at every step. Phases chain immediately. The only reason execution stops is all 13 phases are done.

---

## MCP Tool Usage (MANDATORY AT EVERY STEP)

All MCP services MUST be used during each phase — they are not optional:

| MCP Service | When | What For |
|-------------|------|----------|
| **git-mcp-server** | Before edit, after edit, commit | `git_status`, `git_diff`, `git_blame` for context; `git_log --oneline -10` before branching; `git_add` + `git_commit` for each phase; `git_branch(create)` for new branches |
| **treesitter** | During code changes | `treesitter_get_symbols` on files about to be modified; `treesitter_find_similar_code` when writing new patterns; `treesitter_analyze_complexity` after refactoring AND after reviewer fixes; `treesitter_get_dependencies` before extracting functions |
| **claude-context** | Before each phase, after code review | `claude-context_search_code(path="/home/rafael/Projects/git.0ur.uk/nenya" query="<target functions>")` to understand what each function does before modifying; search again after changes for reviewer-level analysis |
| **mempalace** | Checkpoint, code-review, resume | `mempalace_add_drawer(wing="nenya" room="phase-progress")` for phase checkpoints; `mempalace_kg_add(subject="nenya" predicate="phase_completed" object="phase-NNN")` for graph tracking; `mempalace_search(wing="nenya" room="phase-progress")` on resume |

---

## Execution Order

Execute phases in this exact order: **010 → 011 → 012 → 013 → 014 → 015 → 016 → 017 → 018 → 019 → 020 → 021 → 022**

Do NOT skip, reorder, or merge phases. Each phase gets its own git branch and commit.

---

## Outer Loop (Per Phase)

```
1. Read the plan file: ../nenya.plans/NNN-name.md
2. Search claude-context for code areas the plan touches (semantic understanding)
3. Use treesitter_get_symbols on files listed in the plan (structural understanding)
4. Run git checkout -b phase-NNN-name from main
5. Execute ALL steps in the plan file — every single one, no shortcuts
6. After each significant edit, verify with treesitter_analyze_complexity
7. Run all verification commands from the plan's Verification section
8. If verification fails → fix → re-verify → repeat until clean
```

**Plan files:**

| Phase | File |
|-------|------|
| 010 | `../nenya.plans/010-fix-17-18-19-streaming-cache-adapter.md` |
| 011 | `../nenya.plans/011-testutil-cleanup.md` |
| 012 | `../nenya.plans/012-security-fixes.md` |
| 013 | `../nenya.plans/013-concurrency-fixes.md` |
| 014 | `../nenya.plans/014-chat-split.md` |
| 015 | `../nenya.plans/015-duplication-fixes.md` |
| 016 | `../nenya.plans/016-parameter-collapse.md` |
| 017 | `../nenya.plans/017-metrics-refactor.md` |
| 018 | `../nenya.plans/018-context-logging.md` |
| 019 | `../nenya.plans/019-retry-split.md` |
| 020 | `../nenya.plans/020-adapter-tests.md` |
| 021 | `../nenya.plans/021-interceptor-tests.md` |
| 022 | `../nenya.plans/022-performance-benchmarks.md` |

---

## Inner Loop (Code Review — MANDATORY, NO SHORTCUTS)

After the phase verification passes, you MUST enter the inner loop:

```
ITERATIONS = 0
COUNT = 0

LOOP:
  ITERATIONS = ITERATIONS + 1

  1. Launch the code-reviewer subagent on ALL files modified/created in this phase
     (task subagent_type=code-reviewer prompt="Review ALL changes in these files: <file list>")
     Scope: only files that were changed by this phase — do NOT request a full repo review.

  2. Collect EVERY finding the reviewer produces — critical, high, medium, low, cosmetic, style, suggestion — ALL OF THEM

  3. Use claude-context search to cross-reference reviewer findings:
     claude-context_search_code(path="/home/rafael/Projects/git.0ur.uk/nenya" query="<finding topic>")

  4. If findings == 0 AND claude-context finds no additional issues:
       COUNT = COUNT + 1
       if COUNT >= 3:
         BREAK (3 consecutive clean reviews = done)
       else:
         GOTO LOOP (verify again)

  5. If findings > 0 OR claude-context found issues:
       COUNT = 0 (reset counter)
       For EACH finding, in priority order (critical → high → medium → low → cosmetic):
         - Fix it. ALWAYS fix it. Do NOT evaluate whether it matters.
         - Do NOT skip any finding, no matter how minor.
         - If the finding says "consider X", implement X.
         - If the finding says "this would be better as Y", change to Y.
         - The ONLY acceptable reason to skip is if the reviewer is factually wrong.
           In that case, add a code comment explaining WHY it's wrong, then mark it resolved.
       Before each fix: use treesitter_find_similar_code to verify pattern consistency.
       After each fix:  use git-mcp-server git_diff to verify the change is correct.
                       use treesitter_analyze_complexity(file_path="<changed file>")
                         to verify no cyclomatic complexity or nesting regressions.
       Run: golangci-lint run
       Run: go build ./...
       Run: go test ./... -count=1
       If any fail → fix → re-run all three
       GOTO LOOP (re-review from scratch)

On BREAK: set this phase's iteration count = ITERATIONS
         (recorded in MemPalace checkpoint below)
```

**CRITICAL RULES (violation = incomplete phase):**
- The inner loop has NO maximum iteration cap. It may run 20+ times.
- "Cosmetic" and "low severity" are NOT reasons to skip. You fix them.
- "This is a suggestion" is NOT a reason to skip. You do it.
- After fixing, you must re-run the reviewer — do NOT assume the fix was correct.
- You must achieve 3 consecutive clean reviews before breaking the loop.
- Use claude-context and treesitter to augment every review cycle.
- Review only modified files — full repo review wastes context.

---

## Between Phases (Non-Stop Chaining — NO PAUSES)

After the inner loop breaks, IMMEDIATELY proceed — no pauses, no asking for permission. Order is IMPORTANT: commit FIRST to persist code changes, then use MCP services for recording:

```
1. Run: golangci-lint run && go build ./... && go test ./... -count=1

2. COMMIT FIRST (code changes persisted):
   git-mcp-server git_add(all=true)
   git-mcp-server git_commit(message=plan template)

3. Then tag:
   git-mcp-server git_tag(tagName="phase-NNN", message="Phase NNN: <name>")
   git-mcp-server git_branch(mode="delete", branchName="phase-NNN-name", force=true)

4. Then MemPalace (record what was committed):
   mempalace_add_drawer:
     Wing: "nenya"
     Room: "phase-progress"
     Content (verbatim):
       Phase: NNN
       Name: <name-from-plan>
       Commit: <hash>
       Tag: phase-NNN
       Reviewer iterations: <count>
       All files changed: <list>
       Verification: PASS
   mempalace_kg_add:
     Subject: "nenya"
     Predicate: "phase_completed"
     Object: "phase-NNN"
     Valid from: <today>

5. Then re-index claude-context for the next phase:
   claude-context_clear_index(path="/home/rafael/Projects/git.0ur.uk/nenya")
   claude-context_index_codebase(path="/home/rafael/Projects/git.0ur.uk/nenya")

6. CHAIN TO NEXT PHASE — do NOT stop. Read the next plan file and start the outer loop.
```

## Resumption Protocol (After Session Break)

If the session terminates mid-execution (the ONLY reason to resume, since there are no pauses):

```
1. Search MemPalace: mempalace_search wing=nenya room=phase-progress
2. Read the most recent entry to find the last COMPLETED phase
3. Run: git branch to find incomplete branches
4. Run: git status, git log --oneline -5 on main
5. Run: git-mcp-server git_log(maxCount=10) to verify where you left off
6. Resume from the FIRST incomplete phase
7. If the branch exists but is incomplete, checkout and continue
8. If no branch exists, create a new one from main and start fresh
9. Re-index claude-context: claude-context_index_codebase(path="/home/rafael/Projects/git.0ur.uk/nenya", force=true)
```

## Phase Dependencies (DO NOT VIOLATE)

```
010 (bugfixes) → 011 (cleanup) → 012 (security) → 013 (concurrency) →
014 (chat split) → 015 (duplication) → 016 (params) → 017 (metrics) →
018 (context/logging) → 019 (retry split) → 020 (adapter tests) →
021 (interceptor tests) → 022 (benchmarks)
```

All phases are executed SEQUENTIALLY. No parallel execution. No skipping.

---

## Completion Protocol (After Phase 022)

After phase 022 passes the inner loop, do NOT stop. Run the final closure:

```
1. Run: golangci-lint run && go build ./... && go test -race -count=1 ./...
2. Save final summary to MemPalace (wing="nenya", room="phase-progress"):
   Subject: Nenya Deep Code Review — COMPLETE
   Phases executed: 010-022 (13 phases)
   Branches created: <list>
   Tags created: <list>
   Final build: PASS
   Final lint: PASS
   Final tests (race): PASS
3. mempalace_kg_add:
   Subject: "nenya"
   Predicate: "deep_code_review_completed"
   Object: "phases_010_to_022"
   Valid from: <today>
4. Report to user with a summary table of all phases, files changed, and total effort.
```

---

## What to Do When Stuck

- If a verification command fails and you cannot fix it: **search MemPalace for similar issues first** (mempalace_search wing=nenya room=gotchas). If no match, try claude-context_search_code for patterns in the codebase. If still stuck, STOP and report the exact error.
- If the code-reviewer agent fails to launch: try again with a different wording. If persistent, use the general agent with prompt "Review this code as a senior Go engineer..." plus the diff.
- If claude-context indexing fails: log the failure and continue without it (treesitter + grep are the fallback).
- If a phase depends on code changed in a previous incomplete phase: **this should not happen** since phases chain immediately and each phase completes fully. If it does, STOP.
- If you discover a bug not covered by the plan: fix it as part of the current phase, document it in the commit message with `fix: <description>`, and save the discovery to MemPalace (wing="nenya", room="gotchas").
