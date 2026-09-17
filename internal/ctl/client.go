package ctl

import (
	"encoding/json"
	"fmt"
	"net"
	"time"
)

type Client struct {
	Socket  string
	Timeout time.Duration
}

func (c Client) Call(request Request, target any) error {
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 5 * time.Minute
	}
	connection, err := net.DialTimeout("unix", c.Socket, timeout)
	if err != nil {
		return err
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(timeout))
	if err := json.NewEncoder(connection).Encode(request); err != nil {
		return err
	}
	var raw struct {
		OK    bool            `json:"ok"`
		Data  json.RawMessage `json:"data"`
		Error string          `json:"error"`
	}
	if err := json.NewDecoder(connection).Decode(&raw); err != nil {
		return err
	}
	if !raw.OK {
		return fmt.Errorf("%s", raw.Error)
	}
	if target != nil && len(raw.Data) > 0 {
		return json.Unmarshal(raw.Data, target)
	}
	return nil
}
