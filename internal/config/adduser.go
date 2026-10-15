package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// AddUser appends a user with a bcrypt password hash to the users list of
// the config file at path. It edits the YAML tree, so comments and the
// order of the other settings are kept.
func AddUser(path, username, passwordHash string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	if doc.Kind == 0 { // empty file
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode}}}
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return errors.New("config: top level is not a mapping")
	}
	users := mappingValue(root, "users")
	if users == nil {
		users = &yaml.Node{Kind: yaml.SequenceNode}
		root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "users"}, users)
	}
	if users.Kind == yaml.ScalarNode && users.Tag == "!!null" {
		users.Kind, users.Tag, users.Value = yaml.SequenceNode, "", ""
	}
	if users.Kind != yaml.SequenceNode {
		return errors.New("config: users is not a list")
	}
	for _, u := range users.Content {
		if n := mappingValue(u, "username"); n != nil && n.Value == username {
			return fmt.Errorf("config: user %q already exists", username)
		}
	}
	var user yaml.Node
	if err := user.Encode(User{Username: username, PasswordHash: passwordHash}); err != nil {
		return err
	}
	users.Style = 0 // "users: []" becomes a block list
	users.Content = append(users.Content, &user)

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	if _, err := Parse(buf.Bytes()); err != nil {
		return fmt.Errorf("config would become invalid: %w", err)
	}
	return writeFileAtomic(path, buf.Bytes())
}

// mappingValue returns the value node for key in mapping node m, or nil.
func mappingValue(m *yaml.Node, key string) *yaml.Node {
	if m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
