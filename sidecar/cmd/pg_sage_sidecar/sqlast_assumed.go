//go:build !sage_e2e_assume_sqlast

package main

// sqlASTAssumed is false in every shipped build: the gate trusts only the
// parse-tree layer the build really carries (cgo builds).
const sqlASTAssumed = false
