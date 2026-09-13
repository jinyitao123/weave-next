// Package registry owns the complete agent definition and its pure validation
// contracts. It performs no catalog reads, transactions, or file materialization.
// Exact-version transactional readers belong to freezer; mutable catalog state
// belongs to the product layer. Runtime decoding uses this same definition so
// execution fields are never duplicated in a reduced Host-specific record.
package registry
