//go:build windows

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// stdioAppServerClient owns a short-lived Codex app-server process. It is
// used only for supported read APIs and never changes Codex Desktop routing.
type stdioAppServerClient struct {
	command   *exec.Cmd
	stdin     io.WriteCloser
	messages  chan []byte
	done      chan struct{}
	nextID    atomic.Int64
	writeMu   sync.Mutex
	closeOnce sync.Once
	waitMu    sync.Mutex
	waitErr   error
}

func codexAppServerSidecarSupported() bool { return true }

func dialCodexAppServerSidecar(ctx context.Context) (appServerCaller, error) {
	executable, err := findCodexExecutable()
	if err != nil {
		return nil, err
	}
	command := exec.Command(executable, "--disable", "plugins", "app-server", "--stdio")
	command.Stderr = io.Discard
	command.Env = os.Environ()
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNoWindow}
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, err
	}
	client := &stdioAppServerClient{
		command:  command,
		stdin:    stdin,
		messages: make(chan []byte, 16),
		done:     make(chan struct{}),
	}
	client.nextID.Store(1)
	if err := command.Start(); err != nil {
		_ = stdin.Close()
		return nil, err
	}
	go client.readLoop(stdout)
	go func() {
		err := command.Wait()
		client.waitMu.Lock()
		client.waitErr = err
		client.waitMu.Unlock()
		close(client.done)
	}()

	initialize := map[string]any{
		"clientInfo": map[string]any{
			"name":    "codex_work_resumer",
			"title":   "CodexWorkResumer",
			"version": appVersion,
		},
		"capabilities": map[string]any{"experimentalApi": true},
	}
	if err := client.Call(ctx, "initialize", initialize, nil); err != nil {
		_ = client.Close()
		return nil, err
	}
	if err := client.notify(ctx, "initialized", map[string]any{}); err != nil {
		_ = client.Close()
		return nil, err
	}
	return client, nil
}

func (c *stdioAppServerClient) readLoop(reader io.Reader) {
	defer close(c.messages)
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
	for scanner.Scan() {
		message := append([]byte(nil), scanner.Bytes()...)
		select {
		case c.messages <- message:
		case <-c.done:
			return
		}
	}
}

func (c *stdioAppServerClient) Call(ctx context.Context, method string, params any, destination any) error {
	if c == nil || c.stdin == nil {
		return errors.New("app-server connection is unavailable")
	}
	id := c.nextID.Add(1)
	if err := c.writeJSON(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return &appServerCallError{Method: method, Err: err}
	}
	for {
		select {
		case <-ctx.Done():
			return &appServerCallError{Method: method, Err: ctx.Err()}
		case <-c.done:
			c.waitMu.Lock()
			err := c.waitErr
			c.waitMu.Unlock()
			if err == nil {
				err = errors.New("app-server exited")
			}
			return &appServerCallError{Method: method, Err: err}
		case data, ok := <-c.messages:
			if !ok {
				return &appServerCallError{Method: method, Err: errors.New("app-server output closed")}
			}
			var message appServerWireMessage
			if json.Unmarshal(data, &message) != nil {
				continue
			}
			if message.Method != "" && len(message.ID) > 0 && string(message.ID) != "null" {
				_ = c.writeJSON(map[string]any{
					"jsonrpc": "2.0",
					"id":      json.RawMessage(message.ID),
					"error":   map[string]any{"code": -32601, "message": "Interactive request unavailable in quota monitor"},
				})
				continue
			}
			var responseID int64
			if len(message.ID) == 0 || json.Unmarshal(message.ID, &responseID) != nil || responseID != id {
				continue
			}
			if message.Error != nil {
				return &appServerRequestError{Method: method, Code: message.Error.Code, Message: message.Error.Message}
			}
			if destination == nil || len(message.Result) == 0 || string(message.Result) == "null" {
				return nil
			}
			if err := json.Unmarshal(message.Result, destination); err != nil {
				return &appServerCallError{Method: method, Err: fmt.Errorf("decode app-server result: %w", err)}
			}
			return nil
		}
	}
}

func (c *stdioAppServerClient) notify(ctx context.Context, method string, params any) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	return c.writeJSON(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (c *stdioAppServerClient) writeJSON(value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = c.stdin.Write(data)
	return err
}

func (c *stdioAppServerClient) Close() error {
	if c == nil {
		return nil
	}
	c.closeOnce.Do(func() {
		if c.stdin != nil {
			_ = c.stdin.Close()
		}
		select {
		case <-c.done:
		case <-time.After(250 * time.Millisecond):
			if c.command != nil && c.command.Process != nil {
				_ = c.command.Process.Kill()
			}
			select {
			case <-c.done:
			case <-time.After(2 * time.Second):
			}
		}
	})
	return nil
}
