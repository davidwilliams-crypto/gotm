---
name: graph-run
description: Run a multi-part coding task as a graph - write pass/fail checks first, split by blast radius, fan out parallel graph-worker subagents, gate on evidence, and stop for one human approval. Use for /graph-run or "run this as a graph".
---

# graph-run

Run a coding task so the user approves one thing at the end instead of every step. Two layers:

- **Loop (inside a unit):** produce -> check -> correct -> repeat until green.
- **Graph (between units):** split -> fan out -> merge -> gate -> send back.

Use this for coding work with more than one separable part (ports, refactors, multi-file features, audits with fixes). For a single small change, skip the graph and run one loop with a real check.

Invoking this skill is the user's request to use subagents. Use the `graph-worker` subagent for each unit if it is installed; otherwise use a general-purpose subagent with the brief template below. If no subagent tool is available, run the units one at a time yourself but keep every contract, gate and return rule.

## Operating rules

- Do not ask the user to confirm intermediate steps. Ask only at: (a) no failable check exists, (b) a unit fails three corrections and the cause is unclear, (c) the final approval.
- Never report a check as green without the command and its actual output/exit code.
- Nothing in Lane C (below) is ever executed by this skill.
- Work on a new branch (`graph/<short-task-name>`), never directly on the default branch. Do not merge to the default branch, push or deploy before approval.

## Step 1 - Write the checks first

Before planning any work, find or create conditions a program can evaluate. Discover the repo's real commands (test, lint, type-check, build) by reading its config, and run them once to record the baseline. What already fails is not yours to be blamed for, and not yours to hide.

Valid checks:
- test suite / specific tests exit 0
- lint, type-check, build exit 0
- `git diff --name-only` is a subset of the unit's allowed files
- a script's output equals an expected value

Not checks: "looks good", the model is confident, no errors were raised.

If the area being changed has no failable check, the first unit of the graph is writing one (tests that fail before the change and pass after). If that is not possible, stop and tell the user what cannot be verified.

## Step 2 - Split (the splitter)

1. Read `.claude/graph-constraints.md` in the repo if it exists. Every rule in it is binding on how you cut and brief the work.
2. Cut by **blast radius**, not by folder: group by what breaks if the change is wrong (shared utility vs. isolated module vs. schema vs. callers). Two units must not own the same file.
3. For each unit record:

```
UNIT     short id
JOB      one bounded job
INPUT    what it reads
OUTPUT   what it produces
FILES    exact files it may create/modify
CHECK    command(s) that must exit 0
LANE     A | B | C
NEEDS    unit ids whose output it actually reads (or none)
```

4. Edge test for every NEEDS entry: name the concrete thing that crosses (a function signature, a generated file, a schema). If you cannot name it, there is no edge; the units run in parallel.

### Lanes (sort by cost of being wrong, not by confidence)

- **Lane A - reversible and contained:** tests, docs, copy, an isolated function with coverage. Passes on green checks.
- **Lane B - reversible but wide:** shared utilities, public interfaces, additive schema changes, anything many callers touch. Passes on green checks plus a full-suite run plus a clean trajectory (see gate).
- **Lane C - hard to reverse:** data migrations, deletion of data or of files outside the plan, anything writing to production, secrets/credentials, money, force-push, deploy, major dependency upgrades in production paths. **This lane does not open.** Prepare the change as a script or patch, do not run it, and list it for the human.

## Step 3 - Fan out (workers)

One worker per unit, each in its own context. Launch all units with no unmet NEEDS in the same message so they run in parallel. Use an isolated git worktree per worker when the tool supports it; if not, parallel workers must have disjoint FILES. Workers do not see each other's reports.

Worker brief template:

```
You own one unit of a larger change. Do only this.
UNIT: ...
JOB: ...
INPUT: ...
FILES YOU MAY TOUCH: ... (touch nothing else; if something else looks wrong, report it, do not fix it)
CONSTRAINTS: (relevant lines from graph-constraints.md)
CHECK: run `...` ; it must exit 0
LOOP: make the change, run the check, fix, repeat. Maximum 3 correction attempts.
REPORT: files changed, the check command, its final output and exit code, attempts used, anything out of scope you noticed.
```

## Step 4 - Code nodes (no judgement allowed)

Do these with shell commands, not reasoning: merging worktrees/branches, running checks, comparing changed files to the allowed list, deduplicating findings, before/after export or API-surface diffs. If a step can be described without the words judge, decide, assess or summarize, it is a command.

## Step 5 - Gate

Re-run each unit's check yourself; do not trust the worker's claim. Read evidence in this order:

1. Deterministic results: check exit codes, full suite after integration.
2. Scope: changed files are a subset of FILES.
3. Trajectory of this run: attempts used, any scope violation, any check that was edited or weakened to pass (a modified, skipped or deleted test is a red verdict unless the unit's job was that test).
4. History: prior entries for this area in graph-constraints.md.
5. The worker's own assessment - last, and never sufficient alone.

## Step 6 - Returns (correction edge)

Return **the unit, not the batch**. Units that passed are frozen and not regenerated.

```
UNIT      id
VERDICT   red
REASON    which check failed
EVIDENCE  exact output, file:line
SCOPE     fix this only; touch no other files or units
```

Cap: three corrections per unit. On the third failure, stop correcting: the fault is in the plan. Re-split that part once if the cause is clear from the evidence; otherwise hold the unit and report it at approval.

## Step 7 - Integrate and present one approval

Merge passed units onto the working branch, run the full check set, then present a single approval request:

- what will merge: units, files, one line each
- evidence: commands and results (baseline vs. now)
- held: failed units with their last return packet
- Lane C items prepared but not executed
- out-of-scope issues workers noticed
- proposed new constraints (Step 8)

The user approves the merge and chooses what ships. Do nothing irreversible before that answer.

## Step 8 - Learning edge

After approval, turn results into rules for the next run and append them to `.claude/graph-constraints.md` (create it if missing; include it in the approved change). Carry a constraint, not the output:

```
- [date] RULE: adapters preserve keyword args exactly
  FROM: utils slice, failed test_x on attempt 1 (positional reorder)
```

Write a rule for every confirmed failure cause and every rollback. Do not write rules from guesses; if the cause was not confirmed by a check, say so and skip it.
