package watcher

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// FileState comes from the parsers subpackage via the type alias in
// scanner.go (FileState = parsers.FileState), so checkpoint serialization
// stays JSON-compatible with the previous on-disk format.

// Checkpoint is a thread-safe map of file paths to scan progress, persisted
// atomically to a JSON file. The watcher loads it at startup and writes it
// after every successful batch upload — so a crash or restart re-reads at
// most one batch worth of records (which the server dedups anyway).
type Checkpoint struct {
	mu    sync.Mutex
	path  string
	Files map[string]FileState `json:"files"`
}

func LoadCheckpoint(path string) (*Checkpoint, error) {
	c := &Checkpoint{path: path, Files: make(map[string]FileState)}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(data, c); err != nil {
		return nil, err
	}
	if c.Files == nil {
		c.Files = make(map[string]FileState)
	}
	return c, nil
}

func (c *Checkpoint) Get(path string) (FileState, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.Files[path]
	return s, ok
}

func (c *Checkpoint) Set(path string, s FileState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Files[path] = s
}

// Save writes the checkpoint atomically (write tmp + rename).
func (c *Checkpoint) Save() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return err
	}
	tmp := c.path + ".tmp"
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, c.path)
}
