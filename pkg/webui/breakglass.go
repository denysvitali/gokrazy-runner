package webui

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func ReadAuthorizedKeys(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	return string(b), nil
}

func WriteAuthorizedKeys(path, content string) error {
	if strings.ContainsRune(content, 0) {
		return fmt.Errorf("authorized_keys contains NUL byte")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Chmod(path, 0600)
}
