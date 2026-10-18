// Package role names the roles argvio itself defines. A tenant membership's
// role is free-form text (see `argvio tenant member add --role`); these are
// the ones with meaning to argvio, which an admin identity's role
// (auth.admin.role) must be drawn from. It is a package of its own so that
// config can validate against it without importing tenant.
package role

// Today the metrics API is read-only and treats these alike; Admin is what
// future write endpoints will require.
const (
	Viewer = "viewer" // read access to a tenant's metrics
	Admin  = "admin"  // everything a viewer may, plus future write access
)

// Known reports whether r is a role argvio defines.
func Known(r string) bool { return r == Viewer || r == Admin }
