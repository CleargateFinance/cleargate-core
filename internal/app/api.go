package app

// BuildAPI constructs the HTTP application: modules, middleware chains and
// route groups.
//
// Route grouping is a security boundary, not decoration. Two caller classes
// exist and must not overlap:
//
//	agentAPI   - API-key credential held by the SDK. May REQUEST payments.
//	consoleAPI - human session. May CREATE and SIGN mandates.
//
// Mandate-mutating routes are registered only under consoleAPI, so an agent
// credential cannot reach them even if a handler forgets to check. This is the
// structural expression of whitepaper §6.1: model output is a request, never
// an instruction, and an agent must not be able to widen its own authority.
//
// TODO(scaffold): BuildAPI(cfg) (*gin.Engine, func(), error).
