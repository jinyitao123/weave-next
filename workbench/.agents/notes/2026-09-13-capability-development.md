# Conversation-first capability authoring in Workbench

Reusable capability creation follows the established team interaction model. The agent lists published capabilities first, reuses one when it fits, and otherwise sends the business request to Weave to generate and save a draft. The conversation presents purpose, responsibilities, flow, inputs and outputs for review. It does not expose a JSON editor, schemas, role identifiers or execution internals.

Draft creation requires a caller-supplied idempotency key. Retrying an unresolved request reuses that key and therefore the same draft identity. Publication is a separate tool call that requires the user to have confirmed the exact proposal and publication. A published revision is immutable; later changes create a new draft revision.

Capabilities are organizational assets inside one workspace. Database reads and writes include the workspace identity, while create and publish require capability management access. The current desktop Host represents one configured developer identity. A Host shared by multiple signed-in people would need delegated per-user credentials before individual attribution could be claimed.

The Application access settings section remains separate. It manages stable app IDs, scoped keys and exact-version grants. Application credentials use a separate token prefix and cannot enter ordinary platform routes or capability-authoring routes. The Host keeps its own key private, and an issued application key is displayed only until hidden or the selected app changes. Browser acceptance covers creation, exact-version grant, rotation, revocation and result readback.
