package ctl

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"deploy-boss/internal/reqlog"
	"deploy-boss/internal/super"
)

type Server struct {
	path     string
	listener net.Listener
	manager  *super.Manager
	logs     *reqlog.Manager
	login    func() (string, error)
}

// Listen serves the control socket. login mints a console login link and is nil when the
// management console is not enabled.
func Listen(path string, manager *super.Manager, logs *reqlog.Manager, login func() (string, error)) (*Server, error) {
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
	server := &Server{path: path, listener: listener, manager: manager, logs: logs, login: login}
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

func (s *Server) dispatch(request Request) Response {
	var data any
	var err error
	switch request.Method {
	case "ls":
		snapshots := s.manager.Snapshots()
		for i := range snapshots {
			s.addRates(&snapshots[i])
		}
		data = snapshots
	case "status":
		var snapshot super.Snapshot
		snapshot, err = s.manager.Snapshot(request.App)
		if err == nil {
			s.addRates(&snapshot)
		}
		data = snapshot
	case "start":
		err = s.manager.Start(request.App)
	case "stop":
		err = s.manager.Stop(request.App)
	case "restart":
		err = s.manager.Restart(request.App)
	case "maintenance":
		err = s.manager.SetMaintenance(request.App, request.On)
	case "rescan":
		var invalid []error
		invalid, err = s.manager.Rescan()
		messages := make([]string, len(invalid))
		for i, scanErr := range invalid {
			messages[i] = scanErr.Error()
		}
		data = map[string]any{"invalid": messages, "restart_required": s.manager.RestartRequired()}
	case "logs":
		data, err = s.manager.Logs(request.App, request.Process, request.Lines)
	case "ports":
		data = s.manager.Ports()
	case "login":
		if s.login == nil {
			err = errors.New("management console is not enabled: set management.host in the host config")
			break
		}
		var link string
		link, err = s.login()
		data = map[string]string{"url": link}
	default:
		err = fmt.Errorf("unknown method %q", request.Method)
	}
	if err != nil {
		return Response{Error: err.Error()}
	}
	return Response{OK: true, Data: data}
}

func (s *Server) addRates(snapshot *super.Snapshot) {
	if s.logs == nil {
		return
	}
	rates, err := s.logs.Rates(snapshot.Name)
	if err != nil {
		return
	}
	snapshot.RequestRates = super.RequestRates{LastMinute: rates.LastMinute, LastHour: rates.LastHour, LastDay: rates.LastDay}
}
