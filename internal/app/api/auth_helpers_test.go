package api

import "time"

// signJWT and signJWTFor mint test tokens; production tokens come from the
// external identity exchange and admin API keys.
func (s *Server) signJWT(tenant, userID string, roles []string) (string, error) {
	return s.signJWTFor(tenant, userID, roles, "", 24*time.Hour)
}

func (s *Server) signJWTFor(tenant, userID string, roles []string, identitySource string, ttl time.Duration) (string, error) {
	return s.signJWTWithPermissionSets(tenant, userID, roles, identitySource, nil, ttl)
}
