//go:build windows

package main

import (
	"fmt"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const memoryAlertTimeout = 15 * time.Second

func showMemoryLimitAlert(sample memorySample, limitMB int) {
	title, titleErr := windows.UTF16PtrFromString("CodexWorkResumer 已自動停止")
	message, messageErr := windows.UTF16PtrFromString(fmt.Sprintf(
		"背景程序私有記憶體已達到 %d MB，超過設定上限 %d MB。\n\n已自動停止自動重試服務，Codex 和 Task 資料不會刪除。請關閉其他異常程序後，從啟動管理器重新啟動服務。",
		memoryBytesToMB(sample.PrivateBytes), limitMB,
	))
	if titleErr != nil || messageErr != nil {
		return
	}
	user32 := windows.NewLazySystemDLL("user32.dll")
	// MessageBoxTimeoutW is available on supported Windows versions and keeps a
	// safety alert from holding the worker alive forever when unattended.
	timeoutProc := user32.NewProc("MessageBoxTimeoutW")
	result, _, _ := timeoutProc.Call(
		0,
		uintptr(unsafe.Pointer(message)),
		uintptr(unsafe.Pointer(title)),
		0x30|0x10000,
		0,
		uintptr(memoryAlertTimeout.Milliseconds()),
	)
	if result != 0 {
		return
	}
	// Older or restricted hosts may not export MessageBoxTimeoutW. Fall back to
	// a normal box in a bounded goroutine; process shutdown still wins.
	done := make(chan struct{})
	go func() {
		box := user32.NewProc("MessageBoxW")
		box.Call(0, uintptr(unsafe.Pointer(message)), uintptr(unsafe.Pointer(title)), 0x30|0x10000)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(memoryAlertTimeout):
	}
}
