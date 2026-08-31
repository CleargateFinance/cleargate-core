package account

// Composition and route registration for this module. The single entry point
// the rest of the application is allowed to touch.
// TODO(scaffold): New(deps) *Module; (*Module).Service(); RegisterRoutes(rg).

type Module struct {
	svc *Service
	h   *handler
}

func (m *Module) Service() *Service { return m.svc }
