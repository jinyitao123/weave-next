// Package execspec defines the inputs and frozen authority shared by platform
// execution and runtime adapters. It does not materialize files or access stores.
package execspec

// Attachment identifies an already-authorized local input for an execution
// adapter. Remote requests transport its bytes through runtimeprotocol instead
// of exposing this local source path on the wire.
type Attachment struct {
	Filename string
	Path     string
}
