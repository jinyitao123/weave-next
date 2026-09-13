# Capability operations in Workbench

Workbench keeps reusable capability authoring in conversation. The capability operations settings section shows durable runs and workspace limits without exposing the capability definition or engine configuration.

A published invocation records the authenticated Weave user, step checkpoints, transitions, and consumed step allowance. Human review parks the invocation and resumes from the saved checkpoint. An expired worker claim returns to the queue from that checkpoint; completed model and tool steps are not repeated.

A capability may use local Loom inference or an eligible remote CLI runtime. The published runtime requirement selects the execution route, while the published definition continues to own roles, steps, input validation, output validation, branching, bounded loops, and the final business result.
