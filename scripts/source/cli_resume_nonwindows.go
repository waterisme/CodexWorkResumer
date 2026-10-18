//go:build !windows

package main

import "errors"

func dispatchCLIResumeFallback(RetryJob, ResumeSettings, string, *safeLogger) (DispatchResult, error) {
	return DispatchResult{}, errors.New("CLI Resume fallback is only available on Windows")
}
