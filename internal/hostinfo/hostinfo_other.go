//go:build !linux && !windows

package hostinfo

import (
	"context"
	"os/exec"
	goruntime "runtime"
	"time"
)

func prepare(*exec.Cmd) {}

func collectStatic(_ context.Context, info *Info) { info.OSName = goruntime.GOOS }

func collectDynamic(*Info) {}

func takeSnapshot() snapshot { return snapshot{at: time.Now()} }

func procDetails(int) (string, string) { return "", "" }

func lookupUser(key string) string { return key }
