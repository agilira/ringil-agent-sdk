//go:build darwin

// backend_darwin.go: the macOS Keychain, through /usr/bin/security
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package keyring

import (
	"bytes"
	"encoding/base64"
	stderrors "errors"
	"os/exec"
	"strings"
)

const (
	securityPath = "/usr/bin/security"
	// prefix marks a value this package wrote: base64, so the Keychain
	// gives it back byte for byte (security prints other values in hex).
	prefix = "ringil-base64:"
	// maxLine bounds a command of security's interactive mode.
	maxLine = 4000
	// notFound is security's exit status for an absent item.
	notFound = 44
)

type keychain struct{}

func system() backend { return keychain{} }

// security runs one command, its output apart from its messages, which
// never mix into a value.
func security(stdin string, args ...string) (string, int, error) {
	cmd := exec.Command(securityPath, args...) // #nosec G204 -- fixed binary; arguments are validated names
	var out, msgs bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &msgs
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	err := cmd.Run()
	var exit *exec.ExitError
	if stderrors.As(err, &exit) {
		return "", exit.ExitCode(), err
	}
	return out.String(), 0, err
}

func (keychain) get(service, name string) (string, bool, error) {
	out, code, err := security("", "find-generic-password", "-s", service, "-a", name, "-w")
	switch {
	case code == notFound:
		return "", false, nil
	case err != nil:
		return "", false, err
	}
	value := strings.TrimSuffix(out, "\n")
	if !strings.HasPrefix(value, prefix) {
		return value, true, nil // written by another tool: as the Keychain gives it
	}
	data, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(value, prefix))
	if err != nil {
		return "", false, err
	}
	return string(data), true, nil
}

// set writes through security's interactive mode, so the value is never
// in a command line another process could read; then reads it back, as
// security may report a failed command with a success.
func (k keychain) set(service, name, value string) error {
	line := "add-generic-password -U -s " + service + " -a " + name + " -w " + prefix + base64.StdEncoding.EncodeToString([]byte(value)) + "\n"
	if len(line) > maxLine {
		return errTooBig // checked before security starts
	}
	if _, _, err := security(line, "-i"); err != nil {
		return err
	}
	got, found, err := k.get(service, name)
	if err != nil {
		return err
	}
	if !found || got != value {
		return errNoKeyring
	}
	return nil
}

func (keychain) del(service, name string) error {
	_, code, err := security("", "delete-generic-password", "-s", service, "-a", name)
	if code == notFound {
		return nil
	}
	return err
}
