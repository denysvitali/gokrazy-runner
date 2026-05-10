package webui

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
)

type PasswordManager struct {
	mu        sync.Mutex
	path      string
	active    string
	isDefault bool
}

// NewPasswordManager loads the active password from path, falling back to
// fallbackPath (read-only seed, e.g. /etc/gokr-pw.txt baked into the gokrazy
// rootfs) and finally to defaultPassword if neither file exists.
//
// IsDefault is true only when both files were missing and the literal default
// is in use; a password loaded from fallbackPath counts as set.
func NewPasswordManager(path, fallbackPath, defaultPassword string) (*PasswordManager, error) {
	if defaultPassword == "" {
		return nil, errors.New("defaultPassword is empty")
	}
	p := &PasswordManager{path: path}

	if data, err := os.ReadFile(path); err == nil {
		p.active = strings.TrimRight(string(data), " \t\r\n")
		p.isDefault = false
		return p, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	if fallbackPath != "" {
		if data, err := os.ReadFile(fallbackPath); err == nil {
			p.active = strings.TrimRight(string(data), " \t\r\n")
			p.isDefault = false
			return p, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("read %s: %w", fallbackPath, err)
		}
	}

	p.active = defaultPassword
	p.isDefault = true
	return p, nil
}

func (p *PasswordManager) Verify(pw string) bool {
	p.mu.Lock()
	active := p.active
	p.mu.Unlock()
	a := []byte(active)
	b := []byte(pw)
	if len(a) != len(b) {
		subtle.ConstantTimeCompare(a, a)
		return false
	}
	return subtle.ConstantTimeCompare(a, b) == 1
}

func (p *PasswordManager) Set(pw string) error {
	if strings.TrimSpace(pw) == "" {
		return errors.New("password is empty")
	}
	if len(pw) < 8 {
		return errors.New("password must be at least 8 characters")
	}
	if len(pw) > 256 {
		return errors.New("password must be at most 256 characters")
	}
	tmp := p.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(pw); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, p.path); err != nil {
		os.Remove(tmp)
		return err
	}
	p.mu.Lock()
	p.active = pw
	p.isDefault = false
	p.mu.Unlock()
	return nil
}

func (p *PasswordManager) IsDefault() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.isDefault
}
