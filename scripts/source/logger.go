package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

const (
	maxLogBytes = 5 * 1024 * 1024
	maxLogFiles = 3 // active log plus two bounded backups (about 15 MB total)
)

type safeLogger struct {
	mu     sync.Mutex
	file   *os.File
	logger *log.Logger
	path   string
	size   int64

	debugSettingsPath string
	debugCheckedAt    time.Time
	debugUntil        time.Time
}

type safeLogWriter struct{ owner *safeLogger }

func (w safeLogWriter) Write(value []byte) (int, error) {
	return w.owner.writeLocked(value)
}

func newSafeLogger(path string) (*safeLogger, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	// Remove any backup beyond the supported retention window left by an older
	// build before opening the active log.
	_ = os.Remove(path + "." + strconv.Itoa(maxLogFiles))
	if info, err := os.Stat(path); err == nil && info.Size() > maxLogBytes {
		rotateLogFiles(path)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	result := &safeLogger{
		file: file,
		path: path,
		size: info.Size(),
	}
	result.logger = log.New(io.Writer(safeLogWriter{owner: result}), "", log.Ldate|log.Ltime|log.LUTC)
	return result, nil
}

func rotateLogFiles(path string) {
	_ = os.Remove(path + "." + strconv.Itoa(maxLogFiles))
	for index := maxLogFiles - 2; index >= 1; index-- {
		source := path + "." + strconv.Itoa(index)
		destination := path + "." + strconv.Itoa(index+1)
		_ = os.Remove(destination)
		_ = os.Rename(source, destination)
	}
	_ = os.Remove(path + ".1")
	_ = os.Rename(path, path+".1")
}

func (l *safeLogger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.file.Close()
}

func (l *safeLogger) Printf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.logger.Printf(format, args...)
}

func (l *safeLogger) ConfigureDebug(settingsPath string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.debugSettingsPath = settingsPath
	l.debugCheckedAt = time.Time{}
	l.debugUntil = time.Time{}
}

func (l *safeLogger) Debugf(format string, args ...any) {
	now := time.Now().UTC()
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.debugEnabledLocked(now) {
		return
	}
	l.logger.Printf("debug=true "+format, args...)
}

func (l *safeLogger) debugEnabledLocked(now time.Time) bool {
	if l.debugSettingsPath == "" {
		return false
	}
	if l.debugCheckedAt.IsZero() || now.Sub(l.debugCheckedAt) >= 5*time.Second || now.Before(l.debugCheckedAt) {
		settings, _ := loadDiagnosticSettings(l.debugSettingsPath)
		l.debugUntil = settings.DebugUntil.UTC()
		l.debugCheckedAt = now
	}
	return l.debugUntil.After(now)
}

func (l *safeLogger) writeLocked(value []byte) (int, error) {
	if int64(len(value)) > maxLogBytes {
		value = append(append([]byte(nil), value[:maxLogBytes-1]...), '\n')
	}
	if l.size > 0 && l.size+int64(len(value)) > maxLogBytes {
		if err := l.rotateLocked(); err != nil {
			return 0, err
		}
	}
	written, err := l.file.Write(value)
	l.size += int64(written)
	return written, err
}

func (l *safeLogger) rotateLocked() error {
	if err := l.file.Close(); err != nil {
		return err
	}
	rotateLogFiles(l.path)
	file, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	l.file = file
	l.size = 0
	return nil
}

func shortThreadID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return fmt.Sprintf("%s...", id[:8])
}
