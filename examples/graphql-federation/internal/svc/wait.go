package svc

import (
	"fmt"
	"time"

	"github.com/infobloxopen/devedge-sdk/server"
)

// waitForBind blocks until the server's gRPC listener has a real address, or a
// fail-loud boot gate error arrives on serveErr, or 3s elapses.
func waitForBind(s *server.Server, serveErr <-chan error) (string, error) {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-serveErr:
			if err != nil {
				return "", fmt.Errorf("serve failed before bind: %w", err)
			}
		default:
		}
		if addr := s.GRPCAddr(); addr != "" && addr != ":0" {
			return addr, nil
		}
		time.Sleep(5 * time.Millisecond)
	}
	return "", fmt.Errorf("server did not bind within 3s")
}
