// Package app is the composition root: the only place in the codebase that
// knows which concrete implementation satisfies which interface.
//
// It lives here rather than in main.go because cmd/api and cmd/worker need the
// same modules wired differently. Building them in main.go would duplicate the
// wiring; building them here also makes the whole application constructible
// inside an end-to-end test.
package app

// TODO(scaffold): buildModules(cfg) shared by API and worker.
