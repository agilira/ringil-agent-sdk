// encfile.go: secrets kept in one encrypted file
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Package encfile is a [secrets.Store] in one file encrypted with a
// passphrase: for a machine with neither a keyring nor a vault (a server,
// a container), or a set of keys to carry. The passphrase comes from the
// host (the keyring, an environment variable, a prompt) and never reaches
// the disk.
//
// The file is AES-256-GCM over a JSON object of the secrets, with a key
// derived from the passphrase by PBKDF2-HMAC-SHA256 (600 000 iterations
// by default, the OWASP figure): a stolen file costs a guesser that much
// per try. Its header (format, iterations, salt, nonce) is authenticated
// with it, so a change anywhere is refused as a wrong passphrase. Every
// write has a fresh nonce and replaces the file atomically; reads and
// writes from several processes take a lock, and writes start from the
// file as it is, so none undoes another's.
//
// The file's directory is the user's own: whoever can write there can
// put back an older copy of the file, which reads as valid (keys removed
// since come back). Keep it where only the user writes, as the check of
// a private directory asks.
package encfile

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	stderrors "errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"github.com/agilira/go-errors"

	"github.com/agilira/ringil-agent-sdk/internal/atomicfile"
	"github.com/agilira/ringil-agent-sdk/internal/lockfile"
	"github.com/agilira/ringil-agent-sdk/secrets"
)

// Error codes.
const (
	// ErrFile: the file could not be read or written.
	ErrFile errors.ErrorCode = "SECRET_FILE"
	// ErrDecrypt: the passphrase is wrong, or the file was changed.
	ErrDecrypt errors.ErrorCode = "SECRET_FILE_DECRYPT"
)

// DefaultIterations is PBKDF2's work per guess of the passphrase.
const DefaultIterations = 600_000

// The format: magic, version, iterations, salt, nonce, then the
// ciphertext of the JSON object and GCM's tag. The header is GCM's
// additional data.
const (
	magic         = "RGSF"
	version       = byte(1)
	saltLen       = 16
	nonceLen      = 12
	headerLen     = 4 + 1 + 4 + saltLen + nonceLen
	keyLen        = 32
	minIterations = 10_000
	maxIterations = 10_000_000 // a file cannot make every read cost minutes
)

// maxFile bounds the file (a variable for the tests, which lower it).
var maxFile = 16 << 20

// Config is where the file is and how to open it.
type Config struct {
	// Path is the file; its directory must exist and be private to the
	// user. The file is made by the first Set, sealed with the passphrase
	// given then: check it with the user before, as there is nothing yet
	// to check it against.
	Path string
	// Passphrase returns the passphrase: asked when the file is opened
	// and when a key for another salt is needed (reads in this process
	// go on meanwhile).
	Passphrase func(ctx context.Context) (string, error)
	// Iterations of PBKDF2 for new files (and files rewritten with
	// fewer); default DefaultIterations, at least 10 000.
	Iterations int
}

// File is the encrypted file's secrets.
type File struct {
	cfg  Config
	root *os.Root
	name string

	mu      sync.Mutex
	key     []byte // for keyFor's salt and iterations
	salt    []byte
	iters   int
	existed bool // a file was read or written: its absence is now an error
}

// New opens the store, checking the passphrase against the file if there
// is one. Close releases the directory.
func New(ctx context.Context, cfg Config) (*File, error) {
	if cfg.Iterations == 0 {
		cfg.Iterations = DefaultIterations
	}
	if cfg.Iterations < minIterations || cfg.Iterations > maxIterations {
		return nil, errors.New(secrets.ErrSource, "encfile: iterations are between 10 000 and 10 000 000")
	}
	if cfg.Passphrase == nil || cfg.Path == "" {
		return nil, errors.New(secrets.ErrSource, "encfile: a Path and a Passphrase are required")
	}
	root, name, err := openDir(cfg.Path)
	if err != nil {
		return nil, err
	}
	f := &File{cfg: cfg, root: root, name: name}
	if _, err := f.read(ctx); err != nil {
		_ = root.Close() // #nosec G104 -- the error that matters is err
		return nil, err
	}
	return f, nil
}

// openDir opens the file's directory: a real one (not a link), private
// to the user, checked once open so it cannot be swapped in between.
func openDir(path string) (*os.Root, string, error) {
	if path == "" || os.IsPathSeparator(path[len(path)-1]) {
		return nil, "", errors.New(secrets.ErrSource, "encfile: Path names a file")
	}
	clean := filepath.Clean(path)
	dir, name := filepath.Dir(clean), filepath.Base(clean)
	bad := errors.New(secrets.ErrSource, "encfile: the file's directory must exist, be a real directory, and be private to you")
	if info, err := os.Lstat(dir); err != nil || !info.IsDir() {
		return nil, "", bad
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, "", bad
	}
	if info, err := root.Stat("."); err != nil || lockfile.CheckPrivate(info) != nil {
		_ = root.Close() // #nosec G104 -- bad is the error
		return nil, "", bad
	}
	return root, name, nil
}

// Close releases the directory.
func (f *File) Close() error { return f.root.Close() }

// Lookup implements [secrets.Resolver]: the file as it is now, written by
// this process or another.
func (f *File) Lookup(ctx context.Context, name string) (string, bool, error) {
	if err := secrets.ValidateName(name); err != nil {
		return "", false, err
	}
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	bag, err := f.read(ctx)
	if err != nil {
		return "", false, err
	}
	value, ok := bag[name]
	return value, ok, nil
}

// read loads the file under the lock: on Windows a file open for reading
// cannot be renamed over, so a reader must not overlap a write (audit
// 21/11), and everywhere a read sees one write whole.
func (f *File) read(ctx context.Context) (map[string]string, error) {
	var bag map[string]string
	err := f.locked(ctx, func() (err error) {
		bag, _, err = f.load(ctx)
		return err
	})
	return bag, err
}

// locked runs fn holding the file's lock.
func (f *File) locked(ctx context.Context, fn func() error) error {
	lock, err := lockfile.Open(f.root, "."+f.name+".lock")
	if err != nil {
		return errors.New(ErrFile, "encfile: cannot open the lock (is the store closed?)")
	}
	defer func() { _ = lock.Close() }() // #nosec G104 -- closing releases the lock
	if err := lockfile.Lock(ctx, lock); err != nil {
		return err
	}
	return fn()
}

// Set implements [secrets.Store].
func (f *File) Set(ctx context.Context, name, value string) error {
	if err := secrets.ValidateName(name); err != nil {
		return err
	}
	if err := secrets.ValidateValue(name, value); err != nil {
		return err
	}
	return f.change(ctx, func(bag map[string]string) bool {
		bag[name] = value
		return true
	})
}

// Delete implements [secrets.Store]: an absent name is no error, and
// leaves the file as it is.
func (f *File) Delete(ctx context.Context, name string) error {
	if err := secrets.ValidateName(name); err != nil {
		return err
	}
	return f.change(ctx, func(bag map[string]string) bool {
		if _, ok := bag[name]; !ok {
			return false
		}
		delete(bag, name)
		return true
	})
}

// change applies edit to the file under its lock, from the file as it is:
// a write of another process meanwhile is kept.
func (f *File) change(ctx context.Context, edit func(map[string]string) bool) error {
	return f.locked(ctx, func() error {
		bag, h, err := f.load(ctx)
		if err != nil {
			return err
		}
		if !edit(bag) {
			return nil
		}
		return f.write(ctx, bag, h)
	})
}

// header is a file's, as read.
type header struct {
	iters int
	salt  []byte
}

// load reads and decrypts the file; a missing file is an empty store
// (h.salt nil).
func (f *File) load(ctx context.Context) (map[string]string, header, error) {
	data, err := f.raw()
	if err != nil || data == nil {
		return map[string]string{}, header{}, err
	}
	h, nonce, body, err := parse(data)
	if err != nil {
		return nil, header{}, err
	}
	key, err := f.keyFor(ctx, h)
	if err != nil {
		return nil, header{}, err
	}
	plain, err := open(key, nonce, body, data[:headerLen])
	if err != nil {
		return nil, header{}, errors.New(ErrDecrypt, "encfile: wrong passphrase, or "+f.cfg.Path+" was changed")
	}
	var bag map[string]string
	if err := json.Unmarshal(plain, &bag); err != nil || bag == nil {
		return nil, header{}, errors.New(ErrDecrypt, "encfile: the decrypted contents are not a set of secrets")
	}
	f.sawFile()
	return bag, h, nil
}

func (f *File) hadFile() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.existed
}

func (f *File) sawFile() {
	f.mu.Lock()
	f.existed = true
	f.mu.Unlock()
}

// raw is the file's bytes; nil when there is no file yet.
func (f *File) raw() ([]byte, error) {
	if info, err := f.root.Lstat(f.name); err == nil && !info.Mode().IsRegular() {
		return nil, errors.New(ErrFile, "encfile: "+f.cfg.Path+" is not a regular file (a link is not followed)")
	}
	data, err := atomicfile.Read(f.root, f.name, int64(maxFile))
	switch {
	case stderrors.Is(err, fs.ErrNotExist) && f.hadFile():
		// It was there: gone is not "no secrets", or a chain would fall
		// through to another source (audit 21/11).
		return nil, errors.New(ErrFile, "encfile: "+f.cfg.Path+" is gone")
	case stderrors.Is(err, fs.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, errors.New(ErrFile, "encfile: cannot read "+f.cfg.Path+" (a regular file of at most 16 MiB)")
	}
	return data, nil
}

func parse(data []byte) (header, []byte, []byte, error) {
	if len(data) < headerLen+16 || string(data[:4]) != magic {
		return header{}, nil, nil, errors.New(ErrFile, "encfile: not a file of encrypted secrets")
	}
	if data[4] != version {
		return header{}, nil, nil, errors.New(ErrFile, "encfile: a format version this package does not read")
	}
	iters := int(binary.BigEndian.Uint32(data[5:9]))
	if iters < minIterations || iters > maxIterations {
		return header{}, nil, nil, errors.New(ErrFile, "encfile: the file asks for an unacceptable number of iterations")
	}
	salt := data[9 : 9+saltLen]
	nonce := data[9+saltLen : headerLen]
	return header{iters: iters, salt: salt}, nonce, data[headerLen:], nil
}

// keyFor is the key for h, derived once per salt and iterations; the
// passphrase is asked outside f.mu, so a slow prompt holds up no reader.
func (f *File) keyFor(ctx context.Context, h header) ([]byte, error) {
	f.mu.Lock()
	if f.key != nil && f.iters == h.iters && string(f.salt) == string(h.salt) {
		key := f.key
		f.mu.Unlock()
		return key, nil
	}
	f.mu.Unlock()
	pass, err := f.cfg.Passphrase(ctx)
	if err != nil || pass == "" {
		return nil, errors.New(ErrDecrypt, "encfile: no passphrase")
	}
	key, err := pbkdf2.Key(sha256.New, pass, h.salt, h.iters, keyLen)
	if err != nil {
		return nil, errors.New(ErrDecrypt, "encfile: cannot derive the key")
	}
	f.mu.Lock()
	f.key, f.salt, f.iters = key, append([]byte(nil), h.salt...), h.iters
	f.mu.Unlock()
	return key, nil
}

// write encrypts bag with a fresh nonce: under the file's salt, or a new
// one for a new file or one with fewer iterations than configured.
func (f *File) write(ctx context.Context, bag map[string]string, h header) error {
	if h.salt == nil || h.iters < f.cfg.Iterations {
		h = header{iters: f.cfg.Iterations, salt: make([]byte, saltLen)}
		_, _ = rand.Read(h.salt) // never fails (crypto/rand)
	}
	key, err := f.keyFor(ctx, h)
	if err != nil {
		return err
	}
	plain, err := encode(bag)
	if err != nil {
		return err
	}
	if headerLen+len(plain)+16 > maxFile {
		// Past the bound, no call could read the file again (audit 21/11).
		return errors.New(secrets.ErrValue, "encfile: the secrets would make the file larger than 16 MiB")
	}
	head := make([]byte, headerLen)
	copy(head, magic)
	head[4] = version
	binary.BigEndian.PutUint32(head[5:9], uint32(h.iters)) // #nosec G115 -- bounded by maxIterations
	copy(head[9:], h.salt)
	_, _ = rand.Read(head[9+saltLen:]) // the nonce; never fails (crypto/rand)
	sealed, err := seal(key, head[9+saltLen:], plain, head)
	if err != nil {
		return err
	}
	if err := atomicfile.Write(f.root, f.name, append(head, sealed...)); err != nil {
		return errors.New(ErrFile, "encfile: cannot write "+f.cfg.Path)
	}
	f.sawFile()
	return nil
}

// encode is bag as JSON, without HTML escapes, which would grow a value
// of '<' sixfold.
func encode(bag map[string]string) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(bag); err != nil {
		return nil, errors.New(ErrFile, "encfile: cannot encode the secrets")
	}
	return b.Bytes(), nil
}

func gcm(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func seal(key, nonce, plain, aad []byte) ([]byte, error) {
	g, err := gcm(key)
	if err != nil {
		return nil, errors.New(ErrFile, "encfile: cannot encrypt")
	}
	return g.Seal(nil, nonce, plain, aad), nil
}

func open(key, nonce, body, aad []byte) ([]byte, error) {
	g, err := gcm(key)
	if err != nil {
		return nil, err
	}
	return g.Open(nil, nonce, body, aad)
}
