package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"
)

type Server struct {
	addr   string
	health *HealthHandler
	mux    *http.ServeMux
	srv    *http.Server
}

func New(addr string) *Server {
	s := &Server{addr: addr, health: NewHealthHandler(), mux: http.NewServeMux()}
	s.mux.Handle("/v1/health", s.health)
	s.srv = &http.Server{Addr: addr, Handler: s.mux}
	return s
}

func (s *Server) Run(ctx context.Context) error {
	errc := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", s.addr)
		if err := s.srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
		close(errc)
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	s.health.SetDraining(true)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return s.srv.Shutdown(shutdownCtx)
}
