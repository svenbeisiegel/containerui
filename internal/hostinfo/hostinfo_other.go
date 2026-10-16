//go:build !linux && !windows

package hostinfo

import (
	"context"
	"os/exec"
	goruntime "runtime"
)

func prepare(*exec.Cmd) {}

func collectStatic(_ context.Context, info *Info) { info.OSName = goruntime.GOOS }

func collectDynamic(*Info) {}
