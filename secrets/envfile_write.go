// envfile_write.go: writing a private env file that reads back the same
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package secrets

import (
	"crypto/subtle"
	"maps"
	"os"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/agilira/go-errors"
)

// WriteEnvFile writes values to a new private env file (mode 600) at
// path, one double-quoted NAME="value" line each, sorted, and reads it
// back with [EnvFile]: a value that does not come back the same removes
// the file and is an error. An existing path is never overwritten, nor
// followed if it is a link. For a host that reads its keys from a file it
// is given (a container): the keys are in clear, protected by the file's
// mode alone. Errors name a secret, never its value.
func WriteEnvFile(path string, values map[string]string) error {
	names := slices.Sorted(maps.Keys(values))
	var b strings.Builder
	for _, name := range names {
		if err := ValidateName(name); err != nil {
			return err
		}
		line := name + "=" + quoteEnv(values[name])
		if err := envLine(name, values[name], line); err != nil {
			return err
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	if err := create(path, b.String()); err != nil {
		return err
	}
	if err := readsBack(path, values); err != nil {
		_ = os.Remove(path) // #nosec G104 -- the error that matters is readsBack's
		return err
	}
	return nil
}

// envLine: the value is one an env file holds, on a line it reads
// (audit 23/11: refused after the write, without the key's name).
func envLine(name, value, line string) error {
	switch {
	case !utf8.ValidString(value) || strings.ContainsRune(value, 0):
		return errors.New(ErrValue, name+" is not text an env file can hold (a NUL or bytes that are not UTF-8): keep it in a store")
	case len(line) >= maxEnvLine:
		return errors.New(ErrValue, name+" is too long for a line of an env file: keep it in a store")
	}
	return nil
}

// create makes path, only if it does not exist, private, with text.
func create(path, text string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- the host's own choice of file
	if err != nil {
		return errors.Wrap(err, ErrSource, "the env file cannot be made (it must not exist yet)")
	}
	if _, err := f.WriteString(text); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return errors.Wrap(err, ErrSource, "the env file cannot be written")
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return errors.Wrap(err, ErrSource, "the env file cannot be written")
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path) // a file not known whole is not left with keys in clear
		return errors.Wrap(err, ErrSource, "the env file cannot be written")
	}
	return nil
}

// readsBack: every value of the file at path is the one written.
func readsBack(path string, values map[string]string) error {
	res, err := EnvFile(path)
	if err != nil {
		return err
	}
	back := res.(Map)
	for name, value := range values {
		got, ok := back[name]
		if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(value)) != 1 {
			return errors.New(ErrCopy, name+" did not read back from the env file as it was written")
		}
	}
	if len(back) != len(values) {
		return errors.New(ErrCopy, "the env file reads back with other names than were written")
	}
	return nil
}

// quoteEnv is value in double quotes, as ParseEnv reads them: backslash,
// quote and the line breaks escaped.
func quoteEnv(value string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`, "\t", `\t`)
	return `"` + r.Replace(value) + `"`
}
