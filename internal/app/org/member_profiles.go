package org

import (
	"context"
	"errors"
)

type MemberProfile struct {
	Username    string
	DisplayName string
}

// MemberProfiles is a workspace-scoped, read-only product directory. Missing
// accounts must be omitted; the organization store projects them as deleted.
type MemberProfiles func(context.Context, string, []string) (map[string]MemberProfile, error)

var ErrMemberProfilesUnavailable = errors.New("member profile directory is unavailable")

type StoreOption func(*Store)

func WithMemberProfiles(read MemberProfiles) StoreOption {
	return func(s *Store) { s.memberProfiles = read }
}
