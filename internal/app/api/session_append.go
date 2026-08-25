package api

import (
	"context"
	"encoding/json"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/stdlib"
)

// sessionNamespace matches stdlib.SaveSession's Put namespace, so appends land
// in the same rows the kernel session helpers read.
const sessionNamespace = "session"

// appendSessionMessages atomically appends messages to a loom session's stored
// history. With a PGStore it uses an advisory-locked read-modify-write so two
// turns racing on the same session key — a live sync chat and an async dispatch
// reflow landing its report — cannot clobber each other (the kernel's whole-blob
// Put is last-writer-wins). Without PGExt (MemStore in tests/daemon) it falls
// back to load→append→save, adequate for that single-process setting.
func (s *Server) appendSessionMessages(ctx context.Context, sessionKey string, add ...contract.Message) error {
	if len(add) == 0 {
		return nil
	}
	if s.StoreExt != nil {
		return s.StoreExt.MutateValue(ctx, sessionNamespace, sessionKey, func(current []byte, _ bool) ([]byte, error) {
			var msgs []contract.Message
			if len(current) > 0 {
				if err := json.Unmarshal(current, &msgs); err != nil {
					return nil, err
				}
			}
			msgs = append(msgs, add...)
			return json.Marshal(msgs)
		})
	}
	msgs, _ := stdlib.LoadSession(s.Store, sessionKey)
	msgs = append(msgs, add...)
	return stdlib.SaveSession(s.Store, sessionKey, msgs)
}
