package runtime

import "context"

// Compose labels set by docker compose and podman-compose on the containers,
// networks and volumes of a project.
const (
	LabelProject     = "com.docker.compose.project"
	LabelService     = "com.docker.compose.service"
	LabelNumber      = "com.docker.compose.container-number"
	LabelOneOff      = "com.docker.compose.oneoff"
	LabelWorkingDir  = "com.docker.compose.project.working_dir"
	LabelConfigFiles = "com.docker.compose.project.config_files"
	LabelDependsOn   = "com.docker.compose.depends_on"
	LabelNetwork     = "com.docker.compose.network"
	LabelVolume      = "com.docker.compose.volume"
	LabelPrefix      = "com.docker.compose."
)

// ComposeTarget addresses a compose project. Without File only commands
// that work from the labels alone succeed (Compose v2: start, stop,
// restart, down).
type ComposeTarget struct {
	Project string
	File    string // absolute path of the compose file; its folder is the project directory
}

// Composer is implemented by drivers that can run a native compose tool.
// Capabilities.Compose reports whether one was found.
type Composer interface {
	// ComposeValidate checks t.File with the compose tool ("config").
	ComposeValidate(ctx context.Context, t ComposeTarget) error
	// ComposeUp creates or updates the project; only changed services are
	// recreated and services no longer in the file are removed.
	ComposeUp(ctx context.Context, t ComposeTarget, progress func(line string)) error
	// ComposeAction runs start, stop, restart or pull.
	ComposeAction(ctx context.Context, t ComposeTarget, action string, progress func(line string)) error
	// ComposeDown removes the project's containers and networks, with
	// volumes also its named volumes.
	ComposeDown(ctx context.Context, t ComposeTarget, volumes bool, progress func(line string)) error
}

// ComposeActions are the actions ComposeAction accepts.
var ComposeActions = []string{"start", "stop", "restart", "pull"}
