//go:build !windows

package main

import (
	"context"
	"errors"
)

func codexAppServerSidecarSupported() bool { return false }

func dialCodexAppServerSidecar(context.Context) (appServerCaller, error) {
	return nil, errors.New("Codex app-server sidecar is supported on Windows only")
}
