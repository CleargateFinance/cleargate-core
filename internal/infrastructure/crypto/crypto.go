// Package crypto provides the three primitives the whitepaper commits to:
// mandate signing, decision-record signing, and audit-log hash chaining.
// No exotic cryptography.
package crypto

// Signer signs a canonical payload. Implementations may be local (dev) or
// delegated to an external KMS (production).
type Signer interface{}

// TODO(scaffold): canonical JSON serialisation, Ed25519 signer, hash chain.
