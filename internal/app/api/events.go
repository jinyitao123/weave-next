package api

import (
	"time"

	"github.com/labstack/echo/v4"
)

const eventKeepaliveInterval = 25 * time.Second

func (s *Server) handleEvents(c echo.Context) error {
	sse, err := NewSSEWriter(c)
	if err != nil {
		return err
	}

	events, unsubscribe := s.Hub.Subscribe(getTenant(c), getUserID(c))
	defer unsubscribe()

	keepalive := time.NewTicker(eventKeepaliveInterval)
	defer keepalive.Stop()

	for {
		select {
		case event, open := <-events:
			if !open {
				return nil
			}
			if err := sse.SendEvent(event.Type, event); err != nil {
				return nil
			}
		case <-keepalive.C:
			if err := sse.SendComment("keepalive"); err != nil {
				return nil
			}
		case <-c.Request().Context().Done():
			return nil
		}
	}
}
