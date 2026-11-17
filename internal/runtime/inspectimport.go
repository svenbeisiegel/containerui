package runtime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Importing a container from "docker inspect" output: the output, pasted
// by the user, is checked to be the inspect document of one container and
// turned into create options for a new standalone container. Unlike
// recreating, the result is not meant to be identical: compose's labels are
// dropped and a compose container is named after its service.

// ImportSource is a container read from pasted inspect output.
type ImportSource struct {
	RecreateSource
	// Service is the compose service the container belonged to, if any.
	Service string
	// Notes point out what to check before creating the new container.
	Notes []string
}

// ParseContainerInspect checks that raw is the inspect output of exactly
// one container (docker, podman) and returns its document. docker prints
// an array; a single object is accepted too.
func ParseContainerInspect(raw []byte) (json.RawMessage, error) {
	raw = bytes.TrimSpace(bytes.TrimPrefix(bytes.TrimSpace(raw), []byte("\xef\xbb\xbf")))
	if len(raw) == 0 {
		return nil, fmt.Errorf("%w: paste the output of docker inspect <container>", ErrInvalid)
	}
	if !json.Valid(raw) {
		return nil, fmt.Errorf("%w: the inspect output is not valid JSON", ErrInvalid)
	}
	doc := json.RawMessage(raw)
	if raw[0] == '[' {
		var docs []json.RawMessage
		if err := json.Unmarshal(raw, &docs); err != nil {
			return nil, fmt.Errorf("%w: the inspect output is not a list of objects", ErrInvalid)
		}
		switch len(docs) {
		case 0:
			return nil, fmt.Errorf("%w: the inspect output is empty", ErrInvalid)
		case 1:
			doc = docs[0]
		default:
			return nil, fmt.Errorf("%w: the inspect output contains %d objects; inspect one container", ErrInvalid, len(docs))
		}
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(doc, &keys); err != nil || keys == nil {
		return nil, fmt.Errorf("%w: the inspect output is not a JSON object", ErrInvalid)
	}
	has := func(k string) bool {
		v, ok := keys[k]
		return ok && string(v) != "null"
	}
	switch {
	case has("HostConfig") && has("Config") && has("State"):
		// A container.
	case has("RepoTags") || has("RootFS"):
		return nil, fmt.Errorf("%w: this is the inspect output of an image; inspect a container instead", ErrInvalid)
	case has("Mountpoint"):
		return nil, fmt.Errorf("%w: this is the inspect output of a volume; inspect a container instead", ErrInvalid)
	case has("IPAM"):
		return nil, fmt.Errorf("%w: this is the inspect output of a network; inspect a container instead", ErrInvalid)
	default:
		return nil, fmt.Errorf("%w: this is not the inspect output of a container (Config, HostConfig or State is missing)", ErrInvalid)
	}
	var c ContainerInspect
	if err := json.Unmarshal(doc, &c); err != nil {
		return nil, fmt.Errorf("%w: unexpected container inspect output: %v", ErrInvalid, err)
	}
	if c.ID == "" {
		return nil, fmt.Errorf("%w: the container inspect output has no Id", ErrInvalid)
	}
	if c.Config.Image == "" {
		return nil, fmt.Errorf("%w: the container inspect output names no image (Config.Image)", ErrInvalid)
	}
	return doc, nil
}

// ImportFromInspect turns a container's inspect document into options for a
// new standalone container. image is the inspect output of its image, nil
// if the image is not available locally.
func ImportFromInspect(container, image json.RawMessage) (ImportSource, error) {
	src, err := OptsFromInspect(container, image)
	if err != nil {
		return ImportSource{}, err
	}
	var c ContainerInspect
	if err := json.Unmarshal(container, &c); err != nil {
		return ImportSource{}, fmt.Errorf("decode container inspect: %w", err)
	}
	imp := ImportSource{RecreateSource: src, Service: c.Config.Labels[LabelService]}
	o := &imp.Opts
	o.Start, o.Pull = true, PullMissing
	if imp.Service != "" && containerNameRe.MatchString(imp.Service) {
		o.Name = imp.Service
	}
	if !containerNameRe.MatchString(o.Name) {
		o.Name = ""
	}
	note := func(format string, args ...any) { imp.Notes = append(imp.Notes, fmt.Sprintf(format, args...)) }

	if p := imp.Project; p != "" {
		note("The container belonged to compose project %s; its compose labels are not carried over.", p)
		prefix := p + "_"
		for _, n := range o.Networks {
			if strings.HasPrefix(n.Name, prefix) {
				note("Network %s belongs to compose project %s and is removed by docker compose down; choose another network if the project goes away.", n.Name, p)
			}
		}
		for _, v := range o.Volumes {
			if strings.HasPrefix(v.Source, prefix) {
				note("Volume %s belongs to compose project %s and is removed by docker compose down -v.", v.Source, p)
			}
		}
	}
	if src.Name != "" {
		note("If container %s still exists, stop or remove it first, or change the name and published ports: they must be free.", src.Name)
	}
	if len(image) == 0 {
		note("Image %s is not available locally, so the environment, labels and command the container inherited from it are filled in too.", o.Image)
	}
	return imp, nil
}
