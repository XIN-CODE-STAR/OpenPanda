# Conversation planning and persistent goals

Web, interactive CLI and TUI share one conversation service. Open the proposal card above the Web composer to read the Markdown, request changes, or start implementation in the side pane. You can read and confirm the proposal after leaving Plan. Entering Plan again archives the previous proposal. Click **Goal** in the chat toolbar to create a goal, check progress, edit, pause, or resume.

Enter `/plan` in the terminal to toggle persistent planning. On the Web, select **Plan** and send a description to enter persistent planning. Enter `/plan <description>` to enter planning and submit the description in one step. Planning survives subsequent messages, session switches and restarts. Tools have read-only access during planning. Exiting planning restores the original permissions.

A complete proposal receives a version. Enter `/plan show` to review the current proposal or `/plan confirm <version>` to approve that version on the Web or in the terminal. Use `/plan -- <description>` for a description starting with a reserved word such as `confirm`. In the Web side pane, you can also request changes to generate a new version or click **Start implementation**. You can confirm the current version once, after any previously confirmed plan in the session has finished or stopped. The Web composer displays the implementation phase or pending question and provides **Stop** for that round. Implementation stays in the current session. A goal paused for planning resumes after successful implementation and task settlement.

`/goal <full objective>` creates and starts a persistent goal. `/goal` opens the editor or displays its state. Web controls and terminal commands use the same service.

| Command | Behavior |
| --- | --- |
| `/goal edit <full objective>` | Replace the objective and revoke the old execution generation |
| `/goal pause` | Pause continuation and the current model request while retaining dispatched tasks and pending questions |
| `/goal stop` | Pause continuation and interrupt the current round and descendants |
| `/goal resume` | Resume and reset the consecutive blocker counter. A pending question still needs an answer |
| `/goal clear` | Remove the current goal, revoke continuation and retain history |
| `/goal --tokens 100000 <objective>` | Create a goal with an explicit token budget |
| `/goal --tokens 200000 resume` | Explicitly raise the budget and resume |
| `/goal -- edit the backlog` | Create the literal objective `edit the backlog` |

`pause`, `stop`, `resume` and `clear` act as controls when entered alone. A phrase such as `/goal stop the bleeding in CI` creates a goal. In the Web console, clearing a saved goal requires confirmation.

Goals have no additional token budget by default. Request-local entry usage and associated task usage are reconciled after each round. Reaching the budget prevents another round; an in-flight request can exceed the limit. Models cannot change the objective, increase budgets or resume paused goals.

An active goal continues after its round and task tree settle. Questions, approvals, user input and other active session work prevent continuation. Closing the browser does not stop the service. Entering Plan pauses continuation; exiting Plan alone does not resume the goal. Explicitly continuing a goal during Plan also exits planning, without implementing an unconfirmed proposal.

A successful task or answer does not complete the goal. The model must submit a full-objective audit through `update_goal`; the controller also checks task and recovery settlement. Editing a completed goal invalidates its old audit and pauses it until the user explicitly resumes. The same concrete blocker must recur in three consecutive rounds before the goal becomes blocked. Explicit resume restarts that counter. Usage limits, budgets, nonretryable errors and repeated empty responses stop unproductive continuation.

SQLite persists state and usage. Leases and execution generations fence concurrent processes. Restart recovery observes surviving tasks before dispatching more work; interrupted implementation continues from stored history after reconciliation. Session deletion, goal replacement and clearing revoke eligibility before cancellation, preventing stale callbacks from dispatching again.

Legacy browser goals require explicit restoration before becoming background work. Forked sessions start in normal mode without inherited goal eligibility. The separate `panda plan run` pipeline remains independent.
