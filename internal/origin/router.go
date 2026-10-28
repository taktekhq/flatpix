// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Taktek and Nizar Mahmoud

package origin

// Router maps a cloud name from the URL to an origin. A name that is not
// listed uses the default origin, if there is one.
type Router struct {
	byCloud map[string]Origin
	def     Origin
}

// NewRouter builds a router. def may be nil, then only listed clouds exist.
func NewRouter(def Origin, byCloud map[string]Origin) *Router {
	return &Router{byCloud: byCloud, def: def}
}

// For returns the origin for a cloud name.
func (r *Router) For(cloud string) (Origin, bool) {
	if o, ok := r.byCloud[cloud]; ok {
		return o, true
	}
	return r.def, r.def != nil
}
