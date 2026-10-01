package frozen

// MaxDelegatedFiles is the most Forge files one handoff may freeze. Registration
// rejects a larger set before Weave accepts the work, and the runtime decodes
// the same limit, so an accepted handoff can never fail later on file count.
const MaxDelegatedFiles = 10

// MaxDelegatedResources is the frozen resource list a task may carry: the input
// entry, one bound Forge record, and up to MaxDelegatedFiles files.
const MaxDelegatedResources = MaxDelegatedFiles + 2
