---
name: build-plan-task
description: Task — building, continuing or reviewing "Task N.N" of dev-docs/mvp-implementation-plan.md.
---

# Build a plan task

1. Check it is not built: `grep -n "### Task N.N" dev-docs/mvp-implementation-plan.md`, then read the ticks there and `git log --oneline -20`.
2. Read that task and its "Why this and not that" boxes. `grep -n "Task N.N"` finds the Revision notes that changed it. Read the whitepaper sections it cites. If it touches an open decision listed in the Revision notes, stop and ask the owner.
3. Branch `feature/<slug>` from main. Write the failing test, then the code. Commit in several focused Conventional Commits.
4. A task with independent pieces, each with its own tests, can go to sub-agents, one per piece. Keep the piece that joins the others, usually the transaction, for yourself. Brief each agent with the task text, the files it owns, the done-criteria, and "report the files changed, the tests run with their results, and every deviation from the task".
5. Verify each report yourself: read the diff against the done-criteria, run the package's tests, and send back any criterion not shown to hold.
6. Finish by replacing the task's code sketches with a **Built** line naming the files and tests, ticking its steps, and adding a dated Revision note for each way the build differs from the plan.

Done when every step of the task is ticked, `make check` is green, and each Revision note matches the code. Push and open the PR once the user agrees.
