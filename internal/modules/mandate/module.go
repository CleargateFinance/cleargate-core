package mandate

// Module composes this feature's service and exposes route registration —
// the single entry point the rest of the application is allowed to touch.
// TODO(scaffold): New(deps) *Module; RegisterRoutes(rg).
type Module struct {
	svc *Service
}

// Service returns the module's service, for other modules to consume through
// their own ports.go interface.
func (m *Module) Service() *Service { return m.svc }
