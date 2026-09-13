# Capability development in Workbench

The existing ui-weave settings slot now exposes capability JSON authoring, saved draft selection, independent draft debugging, immutable publication, invocation, polling and cancellation. The Workbench Host forwards a fixed allowlist of operations through its configured credential, with independent manage/invoke/read/cancel scopes enforced by Weave.

Uncertain invocation submissions retain their original request identity and input for retry. Editing the input does not silently mint a second invocation while the first submission is unresolved. Runtime errors are separate from result availability. The JSON editor exposes the developer-owned document; it does not expose provider credentials.

The current server executes local Loom worker steps and dependency-based parallel joins. Debug runs freeze the unsaved document and use an independent invocation with run_kind=debug, no published revision, and an immutable database snapshot. CLI engines, human waits, loops and external tools remain unavailable.

Validation uses the Host proxy tests, component interactions, assembled workbench profile, full Workbench build and existing Weave UI suites. Live model and browser acceptance evidence must be recorded separately.

At widths up to 600px the existing Settings shell moves its section navigation above the content. This prevents the fixed desktop navigation width from squeezing the capability editor into a narrow column. Desktop geometry remains unchanged.
