package ctl

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"app-boss/internal/ops"
)

type Server struct {
	path     string
	listener net.Listener
	service  *ops.Service
	login    func() (string, error)
}

// Listen serves the control socket. login mints a console login link and is nil when the
// management console is not enabled.
func Listen(path string, service *ops.Service, login func() (string, error)) (*Server, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	if _, err := os.Stat(path); err == nil {
		connection, dialErr := net.Dial("unix", path)
		if dialErr == nil {
			_ = connection.Close()
			return nil, fmt.Errorf("control socket already active: %s", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o660); err != nil {
		_ = listener.Close()
		return nil, err
	}
	server := &Server{path: path, listener: listener, service: service, login: login}
	go server.serve()
	return server, nil
}

func (s *Server) Close() error {
	err := s.listener.Close()
	removeErr := os.Remove(s.path)
	if err != nil && !errors.Is(err, net.ErrClosed) {
		return err
	}
	if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		return removeErr
	}
	return nil
}

func (s *Server) serve() {
	for {
		connection, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.handle(connection)
	}
}

func (s *Server) handle(connection net.Conn) {
	defer connection.Close()
	decoder := json.NewDecoder(bufio.NewReader(connection))
	encoder := json.NewEncoder(connection)
	for {
		var request Request
		if err := decoder.Decode(&request); err != nil {
			return
		}
		response := s.dispatch(request)
		if err := encoder.Encode(response); err != nil {
			return
		}
	}
}

const loginMethod = "login"

// dispatch runs one control request. Everything but login is the shared ops action; login mints
// a console link and so stays with the server.
func (s *Server) dispatch(request Request) Response {
	if request.Method == loginMethod {
		return s.loginResponse()
	}
	// The control socket has no user, so audited actions are attributed to the CLI.
	if request.Actor == "" {
		request.Actor = "cli"
	}
	data, err := s.service.Do(request)
	if err != nil {
		return Response{Error: err.Error()}
	}
	return Response{OK: true, Data: data}
}

func (s *Server) loginResponse() Response {
	if s.login == nil {
		return Response{Error: "management console is not enabled: set management.host in the host config"}
	}
	link, err := s.login()
	if err != nil {
		return Response{Error: err.Error()}
	}
	return Response{OK: true, Data: map[string]string{"url": link}}
}
