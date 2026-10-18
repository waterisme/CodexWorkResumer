//go:build windows

package main

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestLiveCodexAppServerSidecarReadsRateLimits(t *testing.T) {
	if os.Getenv("CODEX_AUTO_RETRY_LIVE_APP_SERVER_TEST") != "1" {
		t.Skip("set CODEX_AUTO_RETRY_LIVE_APP_SERVER_TEST=1 for a local signed-in Codex check")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client, err := dialCodexAppServerSidecar(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var result appServerRateLimitsResult
	if err := client.Call(ctx, "account/rateLimits/read", map[string]any{}, &result); err != nil {
		t.Fatal(err)
	}
	snapshot := usageSnapshotFromRateLimits(result)
	if snapshot.FiveHour == nil && snapshot.Weekly == nil {
		t.Fatal("signed-in app-server returned no supported quota windows")
	}
}
